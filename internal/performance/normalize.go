// Package performance computes output-only response speed from already
// observed request records. It performs no I/O, source scans or model calls.
package performance

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

const MaxExactInteger int64 = 1<<53 - 1

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// RequestKey deliberately excludes mutable model/session/timing/token fields.
// The caller supplies a namespace that separates installations or account roots.
func RequestKey(r core.PerformanceRequest) string {
	encoded, _ := json.Marshal([]string{r.Namespace, r.SourceSystem, r.AccountID, r.ID})
	return string(encoded)
}

func elapsedMS(start, end time.Time) float64 {
	// Unlike time.Sub, this does not silently saturate at ~290 years.
	return float64(end.Unix()-start.Unix())*1000 + float64(end.Nanosecond()-start.Nanosecond())/1e6
}

// NormalizeRequest computes response_tok_s only from the provided output
// counter and request elapsed time. Inferred intervals alone are restricted to
// [100ms,15min]; verified direct intervals have no arbitrary speed/time cutoff.
func NormalizeRequest(input core.PerformanceRequest) core.PerformanceRequest {
	r := input.DeepClone()
	r.ResponseTokS = nil
	r.ExclusionReason = ""
	priorReason := sanitizeEvidence([]string{input.ExclusionReason})
	if r.Outcome == "" {
		r.Outcome = core.OutcomeUnknown
	}
	// The retired Codex adapter treated internal Completed as API success,
	// although Codex also emits it for response.incomplete(interrupted). Correct
	// old stored observations on read, including archived requests that are no
	// longer in the configured scan. Keep their identity/output/clock metadata;
	// no DB rewrite, archive scan, or accounting migration is needed.
	if r.SourceSystem == "codex" && r.Namespace == "codex:response" &&
		r.AdapterVersion == "codex_native_sqlite_v1" && r.Outcome == core.OutcomeSuccess {
		r.Outcome = core.OutcomeUnknown
	}
	if r.Scope == "" {
		r.Scope = core.ScopeUnknown
	}
	if r.TimingBasis == "" {
		r.TimingBasis = core.TimingUnknown
	}
	if r.OutputBasis == "" {
		r.OutputBasis = "unknown"
	}
	// Only compact adapter-defined codes may cross the snapshot boundary.
	r.Evidence = sanitizeEvidence(r.Evidence)
	exclude := func(status core.PerformanceTimingStatus, reason string) core.PerformanceRequest {
		r.TimingStatus, r.ExclusionReason = status, reason
		return r
	}
	if r.DurationMS != nil && !finite(*r.DurationMS) {
		r.DurationMS = nil
		return exclude(core.TimingInvalid, "nonfinite_duration")
	}
	if r.StartedAt != nil && r.StartedAt.IsZero() {
		r.StartedAt = nil
	}
	if r.EndedAt != nil && r.EndedAt.IsZero() {
		r.EndedAt = nil
	}
	if r.ID == "" || r.Namespace == "" || r.SourceSystem == "" {
		return exclude(core.TimingUnsupported, "missing_identity")
	}
	if r.Revision < 0 || r.Revision > MaxExactInteger {
		return exclude(core.TimingInvalid, "invalid_revision")
	}
	if r.Scope != core.ScopeModelRequest {
		return exclude(core.TimingUnsupported, "not_model_request")
	}
	// A partial observation's end is not the final request end. If a merged
	// or external record claims finality while retaining provisional timing,
	// keep the final candidate but require independent final timing evidence.
	if r.Final && (r.TimingStatus == core.TimingProvisional || (len(priorReason) > 0 && priorReason[0] == "final_request_timing_unconfirmed" && r.EndedAt == nil && r.DurationMS == nil)) {
		r.EndedAt = nil
		r.DurationMS = nil
		return exclude(core.TimingMissing, "final_request_timing_unconfirmed")
	}
	switch r.TimingStatus {
	case core.TimingMissing:
		// A source can identify a completed request while its independently
		// collected clock is still missing. Preserve the sanitized diagnostic
		// so late metadata, retention gaps and unsupported clocks differ.
		if r.DurationMS == nil && (r.StartedAt == nil || r.EndedAt == nil) && len(priorReason) > 0 {
			return exclude(core.TimingMissing, priorReason[0])
		}
	case core.TimingInvalid, core.TimingAmbiguous, core.TimingUnsupported:
		if len(priorReason) > 0 {
			return exclude(r.TimingStatus, priorReason[0])
		}
		return exclude(r.TimingStatus, "timing_"+string(r.TimingStatus))
	}
	if r.StartedAt != nil && r.EndedAt != nil && !r.EndedAt.After(*r.StartedAt) {
		return exclude(core.TimingInvalid, "nonpositive_interval")
	}
	switch r.TimingBasis {
	case core.TimingProviderRequestTimestamps, core.TimingInferredLogBoundary:
		if r.StartedAt == nil || r.EndedAt == nil {
			return exclude(core.TimingMissing, "missing_request_boundary")
		}
		duration := elapsedMS(*r.StartedAt, *r.EndedAt)
		if r.DurationMS != nil && math.Abs(*r.DurationMS-duration) > math.Max(1e-6, duration*1e-9) {
			return exclude(core.TimingAmbiguous, "conflicting_duration")
		}
		r.DurationMS = &duration
	case core.TimingProviderReportedDuration:
		if r.DurationMS == nil {
			return exclude(core.TimingMissing, "missing_duration")
		}
	default:
		return exclude(core.TimingUnsupported, "unknown_timing_basis")
	}
	if r.DurationMS == nil || !finite(*r.DurationMS) || *r.DurationMS <= 0 {
		return exclude(core.TimingInvalid, "nonpositive_duration")
	}
	if r.TimingBasis == core.TimingInferredLogBoundary && (*r.DurationMS < 100 || *r.DurationMS > 15*60*1000) {
		return exclude(core.TimingInvalid, "inferred_interval_out_of_range")
	}
	if r.OutputTokens == nil {
		return exclude(core.TimingMissing, "missing_output")
	}
	if *r.OutputTokens < 0 || *r.OutputTokens > MaxExactInteger {
		return exclude(core.TimingInvalid, "invalid_output")
	}
	if !r.Final || r.TimingStatus == core.TimingProvisional {
		return exclude(core.TimingProvisional, "provisional")
	}
	if r.Outcome != core.OutcomeSuccess {
		r.TimingStatus = core.TimingValid
		if r.Outcome == core.OutcomeFailed {
			r.ExclusionReason = "failed_request"
		} else {
			r.ExclusionReason = "unknown_outcome"
		}
		return r
	}
	rate := float64(*r.OutputTokens) * 1000 / *r.DurationMS
	if !finite(rate) {
		return exclude(core.TimingInvalid, "nonfinite_rate")
	}
	r.TimingStatus, r.ResponseTokS = core.TimingValid, &rate
	return r
}

func sanitizeEvidence(codes []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, code := range codes {
		if len(code) == 0 || len(code) > 64 {
			continue
		}
		valid := true
		for _, c := range code {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:-", c)) {
				valid = false
				break
			}
		}
		if valid && !seen[code] && len(out) < 16 {
			out = append(out, code)
			seen[code] = true
		}
	}
	return out
}

func timingQuality(r core.PerformanceRequest) int {
	if r.Scope != core.ScopeModelRequest || r.TimingStatus == core.TimingInvalid || r.TimingStatus == core.TimingAmbiguous || r.TimingStatus == core.TimingUnsupported {
		return 0
	}
	if r.DurationMS == nil || !finite(*r.DurationMS) || *r.DurationMS <= 0 {
		return 0
	}
	switch r.TimingBasis {
	case core.TimingProviderRequestTimestamps:
		if r.StartedAt != nil && r.EndedAt != nil && r.EndedAt.After(*r.StartedAt) {
			return 30
		}
	case core.TimingProviderReportedDuration:
		return 20
	case core.TimingInferredLogBoundary:
		if r.StartedAt != nil && r.EndedAt != nil && r.EndedAt.After(*r.StartedAt) && *r.DurationMS >= 100 && *r.DurationMS <= 15*60*1000 {
			return 10
		}
	}
	return 0
}

// PreferRequest merges output revisions independently from timing authority.
// The latest native revision may correct a counter without replacing verified
// direct timing with a weaker inferred or missing observation of that request.
func PreferRequest(current, incoming core.PerformanceRequest) core.PerformanceRequest {
	a, b := NormalizeRequest(current), NormalizeRequest(incoming)
	newer := (!a.Final || b.Final) && (b.Revision > a.Revision || (!a.Final && b.Final) || (b.Revision == a.Revision && a.Final == b.Final && b.ObservedAt.After(a.ObservedAt)))
	result := a.DeepClone()
	if newer {
		result = b.DeepClone()
	}
	if a.SourcePath != "" {
		result.SourcePath = a.SourcePath
	}
	if a.SessionID != "" {
		result.SessionID = a.SessionID
	}
	if result.OutputTokens == nil {
		if a.OutputTokens != nil && (!result.Final || a.Final) {
			v := *a.OutputTokens
			result.OutputTokens = &v
			result.OutputBasis = a.OutputBasis
		}
		if b.OutputTokens != nil && (!result.Final || b.Final) {
			v := *b.OutputTokens
			result.OutputTokens = &v
			result.OutputBasis = b.OutputBasis
		}
	}
	timing := a
	if result.Final && !a.Final {
		timing = b
	} else if (!result.Final || b.Final) && (timingQuality(b) > timingQuality(a) || (timingQuality(b) == timingQuality(a) && newer)) {
		timing = b
	}
	result.StartedAt, result.EndedAt, result.DurationMS = timing.StartedAt, timing.EndedAt, timing.DurationMS
	result.TimingBasis, result.TimingStatus = timing.TimingBasis, timing.TimingStatus
	result.Scope = timing.Scope
	result.ExclusionReason, result.Evidence = timing.ExclusionReason, timing.Evidence
	return NormalizeRequest(result)
}
