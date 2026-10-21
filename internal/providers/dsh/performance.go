package dsh

import (
	"encoding/json"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

// In DSH 0.2.1-rc.2 buildRequest appends request/header immediately before
// dispatch, but only for initial/resumed/changed request envelopes or series.
// A header is therefore a one-use inferred boundary, never a reusable clock.
// step/start only scopes that boundary; it is not itself a request timer.
type performanceState struct {
	active    bool
	turn      int64
	step      int64
	stepTime  int64
	header    *int64
	ambiguous bool
}

type performanceFields struct {
	Turn        *int64        `json:"turn"`
	Step        *int64        `json:"step"`
	Reason      string        `json:"reason"`
	Interrupted bool          `json:"interrupted"`
	Stream      []timedRecord `json:"stream"`
}

// No text, tool arguments, header/configuration, error bodies or credentials
// are decoded. Raw numeric fields let bad timing stay unknown without changing
// the existing accounting parser's acceptance of a usage record.
type timedRecord struct {
	Type  string            `json:"type"`
	Time  json.RawMessage   `json:"time"`
	Time0 json.RawMessage   `json:"time0"`
	DT    []json.RawMessage `json:"dt"`
	Chunk struct {
		Type   string `json:"type"`
		Reason struct {
			Kind string `json:"kind"`
		} `json:"reason"`
	} `json:"chunk"`
}

func (s *performanceState) observe(kind string, at int64, data json.RawMessage) *core.PerformanceRequest {
	if s == nil {
		return nil
	}
	switch kind {
	case "turn/start", "turn/end", "step/end", "session/end-seed", "compaction/start", "compaction/end":
		*s = performanceState{}
		return nil
	case "step/start", "request/header", "llm/retry", "llm/retry-started", "assistant/message", "assistant/attempt":
	default:
		return nil
	}
	var fields performanceFields
	if json.Unmarshal(data, &fields) != nil {
		*s = performanceState{}
		return nil
	}
	if kind == "step/start" {
		*s = performanceState{}
		if safeNumber(fields.Turn) && safeNumber(fields.Step) && validTimingMS(at) {
			s.active, s.turn, s.step, s.stepTime = true, *fields.Turn, *fields.Step, at
		}
		return nil
	}
	if kind == "request/header" {
		if !s.active {
			return nil
		}
		if s.header != nil || !validTimingMS(at) || at < s.stepTime {
			s.ambiguous = true
		}
		switch fields.Reason {
		case "initial", "resume", "change", "series":
		default:
			s.ambiguous = true
		}
		s.header = &at
		return nil
	}
	if kind == "llm/retry" || kind == "llm/retry-started" {
		// Retrying must supply a fresh explicit header to become measurable.
		s.header, s.ambiguous = nil, false
		return nil
	}

	start, ambiguous := s.header, s.ambiguous
	s.header, s.ambiguous = nil, false
	r := &core.PerformanceRequest{
		Namespace: "dsh:model_request", SourceSystem: ID,
		AdapterVersion: "dsh_v4_request_header_finish_v1",
		OutputBasis:    "dsh.usage.outputTokens_includes_reasoning",
		Scope:          core.ScopeModelRequest, TimingBasis: core.TimingUnknown,
		TimingStatus: core.TimingMissing, Outcome: core.OutcomeUnknown,
		Final: true, Evidence: []string{"native_usage_settlement"},
		ExclusionReason: "request_header_missing",
	}
	end, finish, streamOK := streamFinish(fields.Stream, at)
	if fields.Interrupted || kind == "assistant/attempt" || finish == "error" || finish == "aborted" {
		r.Outcome = core.OutcomeFailed
	} else if streamOK {
		switch finish {
		case "stop", "tool-calls", "max-tokens":
			r.Outcome = core.OutcomeSuccess
		}
	}
	if start == nil {
		return r
	}
	r.TimingBasis = core.TimingInferredLogBoundary
	if ambiguous || !s.active || !safeNumber(fields.Turn) || !safeNumber(fields.Step) || *fields.Turn != s.turn || *fields.Step != s.step {
		r.TimingStatus, r.ExclusionReason = core.TimingAmbiguous, "request_header_scope_ambiguous"
		return r
	}
	if !streamOK || end == nil {
		r.TimingStatus, r.ExclusionReason = core.TimingInvalid, "stream_finish_timing_invalid"
		return r
	}
	for _, record := range fields.Stream {
		raw := record.Time
		if record.Type != "chunk" {
			raw = record.Time0
		}
		first, ok := timingNumber(raw)
		if !ok || first < *start {
			r.TimingStatus, r.ExclusionReason = core.TimingInvalid, "stream_precedes_request_header"
			return r
		}
	}
	if *end <= *start {
		r.TimingStatus, r.ExclusionReason = core.TimingInvalid, "request_duration_invalid"
		return r
	}
	a, b := time.UnixMilli(*start).UTC(), time.UnixMilli(*end).UTC()
	r.StartedAt, r.EndedAt = &a, &b
	r.TimingStatus, r.ExclusionReason = core.TimingValid, ""
	r.Evidence = append(r.Evidence, "single_request_header_in_step", "explicit_stream_finish")
	return r
}

func timingNumber(raw json.RawMessage) (int64, bool) {
	var n int64
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, n >= -maxSafeInteger && n <= maxSafeInteger
}

func validTimingMS(n int64) bool { return n >= 946684800000 && n < 4102444800000 }

// Stream time/time0 are epoch milliseconds; dt preserves successive chunk
// gaps, not a provider-reported request duration. Inspect their ordering only.
// A finish is explicit terminal evidence; DSH's default implicit stop is not.
func streamFinish(records []timedRecord, settlement int64) (*int64, string, bool) {
	var end *int64
	var finish string
	var previous int64
	if !validTimingMS(settlement) {
		return nil, "", false
	}
	for _, record := range records {
		// A later output or terminal contradicts the claimed terminal boundary.
		// Do not use a shortened interval from a nonterminal finish record.
		if end != nil {
			return nil, "", false
		}
		raw := record.Time
		switch record.Type {
		case "chunk":
		case "text-chunks", "reasoning-chunks", "tool-call-chunks":
			raw = record.Time0
		default:
			return nil, "", false
		}
		at, ok := timingNumber(raw)
		if !ok || !validTimingMS(at) || at < previous || at > settlement {
			return nil, "", false
		}
		if record.Type == "chunk" && record.Chunk.Type == "finish" {
			if end != nil {
				return nil, "", false
			}
			v := at
			end, finish = &v, record.Chunk.Reason.Kind
		}
		if record.Type != "chunk" {
			for _, rawGap := range record.DT {
				gap, ok := timingNumber(rawGap)
				if !ok || gap < 0 || gap > settlement-at {
					return nil, "", false
				}
				at += gap
			}
		}
		previous = at
	}
	return end, finish, end != nil
}

func bindPerformance(event *shared.TelemetryEvent, a action) {
	if a.performance == nil {
		return
	}
	r := a.performance
	r.ID, r.MessageID, r.AccountID = event.MessageID, event.MessageID, event.AccountID
	r.SessionID, r.ModelID, r.BillingProviderID = event.SessionID, event.ModelRaw, event.ProviderID
	r.OutputTokens = event.OutputTokens
	r.Revision = a.seq
	r.ObservedAt = time.UnixMilli(a.time).UTC()
	event.Performance = r
}
