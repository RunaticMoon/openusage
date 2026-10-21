package dsh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

const maxSafeInteger int64 = 9007199254740991

type usage struct {
	Input     *int64 `json:"inputTokens"`
	Output    *int64 `json:"outputTokens"`
	Read      *int64 `json:"cacheReadTokens"`
	Write     *int64 `json:"cacheWriteTokens"`
	Reasoning *int64 `json:"reasoningTokens"`
	Total     *int64 `json:"totalTokens"`
}

func (u usage) tokens() (core.TokenUsage, error) {
	if u.Input == nil || u.Output == nil {
		return core.TokenUsage{}, errParse
	}
	for _, n := range []*int64{u.Input, u.Output, u.Read, u.Write, u.Reasoning, u.Total} {
		if n != nil && (*n < 0 || *n > maxSafeInteger) {
			return core.TokenUsage{}, errParse
		}
	}
	read, write := int64(0), int64(0)
	if u.Read != nil {
		read = *u.Read
	}
	if u.Write != nil {
		write = *u.Write
	}
	// DSH input is uncached; reasoning is an output detail, not a fifth
	// additive bucket. Always supply TotalTokens so generic normalization
	// cannot add reasoning again. Missing cache buckets mean zero in the native
	// projection; absent reasoning remains unknown rather than measured zero.
	total := *u.Input + *u.Output + read + write
	if total > maxSafeInteger || (u.Total != nil && *u.Total != total) {
		return core.TokenUsage{}, errParse
	}
	return core.TokenUsage{InputTokens: core.Int64Ptr(*u.Input), OutputTokens: core.Int64Ptr(*u.Output), CacheReadTokens: core.Int64Ptr(read), CacheWriteTokens: core.Int64Ptr(write), ReasoningTokens: u.Reasoning, TotalTokens: core.Int64Ptr(total), Requests: core.Int64Ptr(1)}, nil
}

type route struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type accountingData struct {
	Turn        *int64 `json:"turn"`
	Step        *int64 `json:"step"`
	Inherited   bool   `json:"inherited"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Usage       *usage `json:"usage"`
	Interrupted bool   `json:"interrupted"`
	Message     struct {
		Source route `json:"source"`
	} `json:"message"`
	Stream []struct {
		Type  string `json:"type"`
		Chunk struct {
			Type   string `json:"type"`
			Usage  *usage `json:"usage"`
			Reason struct {
				Kind string `json:"kind"`
			} `json:"reason"`
		} `json:"chunk"`
	} `json:"stream"`
}
type action struct {
	typeName              string
	seq, time, turn, step int64
	route                 route
	usage                 *usage
	outcome               string
	performance           *core.PerformanceRequest
}

// parseLog follows the tagged dsh v0.2.1-rc.2 accounting projection. Only the
// allowlisted accounting fields survive decoding. Surface replacements and
// compaction never erase previously observed assistant usage.
func parseLog(ctx context.Context, data []byte, account string) (string, []shared.TelemetryEvent, error) {
	return parseLogWithPerformance(ctx, data, account, false)
}

func parseLogWithPerformance(ctx context.Context, data []byte, account string, enabled bool) (string, []shared.TelemetryEvent, error) {
	scanner := logScanner(data)
	if !scanner.Scan() {
		return "", nil, errParse
	}
	var header struct {
		Type            string `json:"type"`
		Version         *int64 `json:"version"`
		ID              string `json:"id"`
		CreatedAt       *int64 `json:"createdAt"`
		Seeded          *bool  `json:"isSeeded"`
		DelegationDepth *int64 `json:"delegationDepth"`
	}
	if json.Unmarshal(scanner.Bytes(), &header) != nil {
		return "", nil, errParse
	}
	if header.Type != "session" || header.Version == nil || *header.Version != 4 {
		return "", nil, errSchema
	}
	if strings.TrimSpace(header.ID) == "" || len(header.ID) > 1024 || header.Seeded == nil || !safeNumber(header.CreatedAt) || !safeNumber(header.DelegationDepth) {
		return "", nil, errParse
	}
	var actions []action
	var timing *performanceState
	if enabled {
		timing = &performanceState{}
	}
	cut, expected := int64(-1), int64(0)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		var row struct {
			Type string          `json:"type"`
			Seq  *int64          `json:"seq"`
			Time *int64          `json:"time"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil || row.Type == "" || !safeNumber(row.Seq) || *row.Seq != expected || !safeNumber(row.Time) {
			return "", nil, errParse
		}
		expected++
		if expected > 500000 {
			return "", nil, errParse
		}
		// Observe all lifecycle settlements, including ones without usage. They
		// still consume a request header and must not donate it to a later call.
		perf := timing.observe(row.Type, *row.Time, row.Data)
		switch row.Type {
		case "request/context", "session/end-seed", "llm/retry-started", "assistant/message", "assistant/attempt", "compaction/summary":
		default:
			continue
		}
		var fields accountingData
		if json.Unmarshal(row.Data, &fields) != nil {
			return "", nil, errParse
		}
		if row.Type == "session/end-seed" {
			if fields.Inherited {
				cut = *row.Seq
			}
			continue
		}
		a := action{typeName: row.Type, seq: *row.Seq, time: *row.Time, route: route{fields.Provider, fields.Model}, outcome: "reported", performance: perf}
		if row.Type == "request/context" {
			if !validRoute(a.route) {
				return "", nil, errParse
			}
			actions = append(actions, a)
			continue
		}
		if row.Type != "compaction/summary" {
			if !safeNumber(fields.Turn) || !safeNumber(fields.Step) {
				return "", nil, errParse
			}
			a.turn, a.step = *fields.Turn, *fields.Step
		}
		if row.Type == "llm/retry-started" {
			actions = append(actions, a)
			continue
		}
		if row.Type == "assistant/message" || row.Type == "compaction/summary" {
			a.usage = fields.Usage
		}
		if row.Type == "assistant/message" && validRoute(fields.Message.Source) {
			a.route = fields.Message.Source
		}
		for _, record := range fields.Stream {
			if record.Type != "chunk" {
				continue
			}
			if record.Chunk.Type == "finish" {
				switch record.Chunk.Reason.Kind {
				case "error", "aborted":
					a.outcome = record.Chunk.Reason.Kind
				}
			}
		}
		if a.usage == nil {
			for _, record := range fields.Stream {
				if record.Type == "chunk" && record.Chunk.Type == "usage" {
					a.usage = record.Chunk.Usage
				}
			}
		}
		if fields.Interrupted {
			a.outcome = "aborted"
		}
		if a.usage != nil {
			if row.Type == "compaction/summary" && !validRoute(a.route) {
				return "", nil, errParse
			}
			if _, err := a.usage.tokens(); err != nil {
				return "", nil, err
			}
			actions = append(actions, a)
		}
	}
	if scanner.Err() != nil {
		return "", nil, errParse
	}
	if *header.Seeded != (cut >= 0) {
		return "", nil, errParse
	}
	var out []shared.TelemetryEvent
	current := route{}
	last := -1
	var lastTurn, lastStep int64
	for _, a := range actions {
		if a.typeName == "request/context" {
			current = a.route
			continue
		}
		if a.seq <= cut {
			continue
		}
		if a.typeName == "llm/retry-started" {
			if last >= 0 && a.turn == lastTurn && a.step == lastStep {
				last = -1
			}
			continue
		}
		chosen := a.route
		if !validRoute(chosen) {
			chosen = current
		}
		if !validRoute(chosen) {
			return "", nil, errParse
		}
		tokens, _ := a.usage.tokens()
		idMaterial, _ := json.Marshal([]any{account, header.ID, a.seq})
		hash := sha256.Sum256(idMaterial)
		event := shared.TelemetryEvent{
			SchemaVersion: SchemaVersion, Channel: shared.TelemetryChannelJSONL,
			OccurredAt: time.UnixMilli(a.time).UTC(), AccountID: account, SessionID: header.ID,
			TurnID: fmt.Sprintf("%d:%d", a.turn, a.step), MessageID: hex.EncodeToString(hash[:]),
			ProviderID: chosen.Provider, AgentName: ID, EventType: shared.TelemetryEventTypeMessageUsage, ModelRaw: chosen.Model,
			TokenUsage: tokens, Status: shared.TelemetryStatusOK,
			Payload: map[string]any{"dsh": map[string]any{"revision": a.seq}, "event_type": a.typeName, "outcome": a.outcome},
		}
		if a.typeName == "compaction/summary" {
			// Separate reported summarizer usage, beyond dsh's assistant meter.
			out = append(out, event)
			continue
		}
		if last >= 0 && a.turn == lastTurn && a.step == lastStep {
			event.MessageID, event.OccurredAt = out[last].MessageID, out[last].OccurredAt
			bindPerformance(&event, a)
			if event.Performance != nil {
				// The accounting projection replaces this slot, but it does not
				// prove that both settlements describe the same request interval.
				// Explicitly revoke an old inference so warm and cold stores agree.
				event.Performance.StartedAt, event.Performance.EndedAt = nil, nil
				event.Performance.TimingBasis = core.TimingInferredLogBoundary
				event.Performance.TimingStatus = core.TimingAmbiguous
				event.Performance.ExclusionReason = "settlement_replacement_ambiguous"
				event.Performance.Evidence = []string{"native_usage_settlement", "accounting_slot_replaced"}
			}
			out[last] = event
		} else {
			bindPerformance(&event, a)
			out = append(out, event)
			last = len(out) - 1
		}
		lastTurn, lastStep = a.turn, a.step
	}
	return header.ID, out, nil
}

func safeNumber(value *int64) bool { return value != nil && *value >= 0 && *value <= maxSafeInteger }
func validRoute(value route) bool {
	return strings.TrimSpace(value.Provider) != "" && strings.TrimSpace(value.Model) != "" && len(value.Provider) <= 256 && len(value.Model) <= 512
}
