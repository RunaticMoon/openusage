package opencode

import (
	"math"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

// The native assistant-message clock is one model request in OpenCode v2.
// Database update times and the first streamed chunk are not request starts.
// Output excludes the separately reported reasoning bucket; explicit zero is
// known output, even when reasoning is positive.
func sqliteV2Performance(ev shared.TelemetryEvent, payload map[string]any, sourcePath string, revision int64) *core.PerformanceRequest {
	r := &core.PerformanceRequest{
		ID: ev.MessageID, Namespace: "opencode:assistant_message", SourceSystem: "opencode",
		SessionID: ev.SessionID, MessageID: ev.MessageID, ModelID: ev.ModelRaw,
		BillingProviderID: ev.ProviderID, SourcePath: sourcePath,
		AdapterVersion: "opencode_sqlite_v2_performance_v1", OutputTokens: ev.TokenUsage.OutputTokens,
		OutputBasis: "opencode.tokens.output_excludes_reasoning", Scope: core.ScopeModelRequest,
		TimingBasis: core.TimingProviderRequestTimestamps, TimingStatus: core.TimingMissing,
		Outcome: core.OutcomeUnknown, ObservedAt: ev.OccurredAt, Revision: revision,
		Evidence: []string{"native_assistant_message_id", "native_time_created_completed"},
	}
	clock, _ := payload["time"].(map[string]any)
	var invalid bool
	r.StartedAt, invalid = performanceMillis(clock, "created")
	var invalidEnd bool
	r.EndedAt, invalidEnd = performanceMillis(clock, "completed")
	if invalid || invalidEnd {
		r.TimingStatus = core.TimingInvalid
		r.ExclusionReason = "invalid_native_timestamp"
	} else if r.StartedAt != nil && r.EndedAt != nil {
		r.TimingStatus = core.TimingValid
	}
	finish, _ := payload["finish"].(string)
	r.Final = finish != "" || r.EndedAt != nil
	if payload["error"] != nil || finish == "error" || finish == "cancelled" || finish == "canceled" || finish == "aborted" {
		r.Outcome = core.OutcomeFailed
	} else {
		switch finish {
		case "stop", "length", "tool-calls", "content-filter":
			r.Outcome = core.OutcomeSuccess
		}
	}
	if !r.Final {
		r.TimingStatus = core.TimingProvisional
	}
	return r
}

func performanceMillis(clock map[string]any, key string) (*time.Time, bool) {
	raw, present := clock[key]
	if !present || raw == nil {
		return nil, false
	}
	n, ok := raw.(float64)
	// This native schema specifies Unix milliseconds. Refuse plausible unit
	// mistakes (seconds/microseconds/nanoseconds) rather than rescaling them.
	// The absolute source-era check is not a limit on request elapsed time.
	const earliestMillis = 946684800000 // 2000-01-01 UTC
	const latestMillis = 4102444800000  // 2100-01-01 UTC, exclusive
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n < earliestMillis || n >= latestMillis {
		return nil, true
	}
	t := time.UnixMilli(int64(n)).UTC()
	return &t, false
}
