package codex

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

// Codex 0.163 writes native response identity and per-response usage separately
// from its turn/thread totals. The observed record has no request clock or
// success marker. Official 0.163.0 emits the record after completed AND
// incomplete/cancelled responses. It settles request accounting but does not
// prove success. Task timers and item/tool timings cannot fill that gap.
func codexUsagePerformance(record sessionLine, state *telemetryParserState, path string) (shared.TelemetryEvent, bool) {
	var p struct {
		ResponseID string `json:"response_id"`
		SessionID  string `json:"session_id"`
		TurnID     string `json:"turn_id"`
		Usage      *struct {
			Output *int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(record.PerformanceUsage, &p) != nil || strings.TrimSpace(p.ResponseID) == "" || p.Usage == nil {
		return shared.TelemetryEvent{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, record.Timestamp)
	if err != nil {
		return shared.TelemetryEvent{}, false
	}
	at = at.UTC()
	r := &core.PerformanceRequest{
		ID: p.ResponseID, Namespace: "codex:response", SourceSystem: "codex",
		SessionID: core.FirstNonEmpty(p.SessionID, state.sessionID), ModelID: state.model,
		BillingProviderID: state.upstreamProviderID, SourcePath: path,
		AdapterVersion: "codex_token_usage_record_v1", OutputTokens: p.Usage.Output,
		OutputBasis: "codex.token_usage_record.usage.output_tokens", Scope: core.ScopeModelRequest,
		TimingBasis: core.TimingUnknown, TimingStatus: core.TimingUnsupported,
		Outcome: core.OutcomeUnknown, Final: true, Revision: at.UnixMilli(), ObservedAt: at,
		ExclusionReason: "native_request_timing_unavailable",
		Evidence:        []string{"native_response_id", "per_response_usage", "no_request_timing", "settled_usage_outcome_unavailable"},
	}
	return shared.TelemetryEvent{
		SchemaVersion: "codex_performance_v1", Channel: shared.TelemetryChannelJSONL,
		OccurredAt: at, SessionID: r.SessionID, TurnID: p.TurnID, MessageID: p.ResponseID,
		ProviderID: codexTelemetryProviderID, AgentName: "codex", ModelRaw: state.model,
		EventType: shared.TelemetryEventTypeMessageUsage, Status: shared.TelemetryStatusOK,
		PerformanceOnly: true, Performance: r,
	}, true
}
