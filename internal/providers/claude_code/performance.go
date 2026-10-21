package claude_code

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

type claudeRequestStart struct {
	at        time.Time
	session   string
	ambiguous bool
}

// This state belongs to the existing per-file incremental collector, not a
// second scanner. The observed Claude 2.1.304 format joins api-request.id to
// assistant.requestRef exactly. User/tool/turn times are never substitutes.
type claudePerformanceState struct{ starts map[string]claudeRequestStart }

func newClaudePerformanceState() *claudePerformanceState {
	return &claudePerformanceState{starts: make(map[string]claudeRequestStart)}
}

func (s *claudePerformanceState) clone() *claudePerformanceState {
	out := newClaudePerformanceState()
	if s != nil {
		for k, v := range s.starts {
			out.starts[k] = v
		}
	}
	return out
}

func (s *claudePerformanceState) observe(line []byte, path string) *core.PerformanceRequest {
	var e struct {
		Type       string `json:"type"`
		ID         string `json:"id"`
		SessionID  string `json:"sessionId"`
		RequestID  string `json:"requestId"`
		RequestRef string `json:"requestRef"`
		Timestamp  string `json:"timestamp"`
		APIError   bool   `json:"isApiErrorMessage"`
		Message    *struct {
			ID         string  `json:"id"`
			Model      string  `json:"model"`
			StopReason *string `json:"stop_reason"`
			Usage      *struct {
				Output *int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil {
		return nil
	}
	at, ok := parseJSONLTimestamp(e.Timestamp)
	if !ok {
		return nil
	}
	if e.Type == "api-request" {
		if e.ID == "" {
			return nil
		}
		start := claudeRequestStart{at: at, session: e.SessionID}
		if old, exists := s.starts[e.ID]; exists {
			start.ambiguous = old.ambiguous || !old.at.Equal(at) || old.session != e.SessionID
		} else if len(s.starts) >= 10000 {
			// Bounded evidence cache: missing timing is preferable to guessing.
			return nil
		}
		s.starts[e.ID] = start
		return nil
	}
	if e.Type != "assistant" || e.Message == nil || e.Message.Usage == nil {
		return nil
	}
	id := core.FirstNonEmpty(e.RequestID, e.Message.ID)
	if strings.TrimSpace(id) == "" {
		return nil
	}
	r := &core.PerformanceRequest{
		ID: id, Namespace: "claude_code:model_request", SourceSystem: "claude_code",
		SessionID: e.SessionID, MessageID: e.Message.ID, ModelID: e.Message.Model,
		// The log's model/tool label does not establish whether billing goes
		// through Anthropic, Bedrock, Vertex or another configured route.
		BillingProviderID: "unknown", SourcePath: path,
		AdapterVersion: "claude_api_request_ref_v1", OutputTokens: e.Message.Usage.Output,
		OutputBasis: "claude.message.usage.output_tokens", Scope: core.ScopeModelRequest,
		TimingBasis: core.TimingUnknown, TimingStatus: core.TimingMissing,
		Outcome: core.OutcomeUnknown, ObservedAt: at, Revision: at.UnixMilli(),
		Evidence: []string{"native_request_or_message_id"},
	}
	if start, found := s.starts[e.RequestRef]; found && e.RequestRef != "" {
		r.TimingBasis = core.TimingInferredLogBoundary
		if start.ambiguous || (start.session != "" && e.SessionID != "" && start.session != e.SessionID) {
			r.TimingStatus, r.ExclusionReason = core.TimingAmbiguous, "conflicting_request_start"
		} else {
			a := start.at.UTC()
			end := at.UTC()
			r.StartedAt, r.EndedAt = &a, &end
			r.TimingStatus = core.TimingValid
			r.Evidence = append(r.Evidence, "exact_api_request_id_request_ref_join", "logged_response_boundary")
		}
	}
	if e.Message.StopReason != nil && strings.TrimSpace(*e.Message.StopReason) != "" {
		r.Final = true
		switch *e.Message.StopReason {
		case "end_turn", "tool_use", "max_tokens", "stop_sequence", "pause_turn", "refusal":
			r.Outcome = core.OutcomeSuccess
		}
	}
	if e.APIError {
		r.Final, r.Outcome = true, core.OutcomeFailed
	}
	if !r.Final {
		r.TimingStatus = core.TimingProvisional
	}
	return r
}

// Account usage still runs through the original record decoder and MAX merge.
// A last line lacking a newline can contribute the same existing usage, but
// cannot finalize a performance request and is retried when the file grows.
func parsePerformanceConversationFile(path string, offset int64, state *claudePerformanceState, withLine bool) ([]shared.TelemetryEvent, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, offset, err
	}
	if state == nil {
		state = newClaudePerformanceState()
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 10*1024*1024)
	consumed := int64(0)
	complete := true
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, token, err := bufio.ScanLines(data, atEOF)
		if token != nil {
			complete = advance > 0 && data[advance-1] == '\n'
			if complete {
				consumed += int64(advance)
			}
		}
		return advance, token, err
	})
	var records []conversationRecord
	line := 0
	for scanner.Scan() {
		line++
		var perf *core.PerformanceRequest
		if complete {
			perf = state.observe(scanner.Bytes(), path)
		}
		if rec, ok := conversationRecordFromLine(scanner.Bytes(), line, path, ""); ok {
			rec.performance = perf
			records = append(records, rec)
		}
	}
	// The existing accounting decoder retains the successfully decoded prefix
	// when ScanLines reaches its size limit. Preserve that behavior with the
	// option enabled; never discard previously decoded usage for timing work.
	return telemetryEventsFromRecords(path, mergeStreamingDuplicates(records), withLine), offset + consumed, nil
}
