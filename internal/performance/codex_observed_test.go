package performance

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func observedCodexRequest() core.PerformanceRequest {
	r := request("observed-response", 150, 2*time.Second)
	r.SourceSystem, r.Namespace = "codex", "codex:response"
	r.AdapterVersion = "codex_native_sqlite_v2"
	r.OutputBasis = "codex.token_usage_record.usage.output_tokens"
	r.Outcome, r.TimingBasis, r.TimingStatus = core.OutcomeUnknown, core.TimingInferredLogBoundary, core.TimingValid
	r.ExclusionReason = "unknown_outcome" // already-stored v1-definition metadata
	return r
}

func TestObservedCodexSpeedRetainsOutcomeAndValidation(t *testing.T) {
	r := observedCodexRequest()
	got := NormalizeRequest(r)
	near(t, got.ResponseTokS, 75)
	if got.Outcome != core.OutcomeUnknown || got.ExclusionReason != "" || got.TimingStatus != core.TimingValid || !reflect.DeepEqual(got, NormalizeRequest(got)) {
		t.Fatal("observed speed changed outcome, retained an exclusion, or was not idempotent")
	}
	for _, tc := range []struct {
		name string
		edit func(*core.PerformanceRequest)
	}{
		{"other source", func(r *core.PerformanceRequest) { r.SourceSystem = "dsh" }},
		{"other namespace", func(r *core.PerformanceRequest) { r.Namespace = "other" }},
		{"legacy adapter", func(r *core.PerformanceRequest) { r.AdapterVersion = "codex_native_sqlite_v1" }},
		{"legacy success", func(r *core.PerformanceRequest) {
			r.AdapterVersion = "codex_native_sqlite_v1"
			r.Outcome = core.OutcomeSuccess
		}},
		{"usage only adapter", func(r *core.PerformanceRequest) { r.AdapterVersion = "codex_token_usage_record_v1" }},
		{"other output", func(r *core.PerformanceRequest) { r.OutputBasis = "total_tokens" }},
		{"other timing", func(r *core.PerformanceRequest) { r.TimingBasis = core.TimingProviderRequestTimestamps }},
		{"failed", func(r *core.PerformanceRequest) { r.Outcome = core.OutcomeFailed }},
		{"other outcome", func(r *core.PerformanceRequest) { r.Outcome = "unrecognized" }},
		{"provisional", func(r *core.PerformanceRequest) { r.Final = false }},
		{"provisional timing", func(r *core.PerformanceRequest) { r.TimingStatus = core.TimingProvisional }},
		{"missing with clock", func(r *core.PerformanceRequest) { r.TimingStatus = core.TimingMissing }},
		{"unknown timing status", func(r *core.PerformanceRequest) { r.TimingStatus = "unrecognized" }},
		{"turn", func(r *core.PerformanceRequest) { r.Scope = core.ScopeTurn }},
		{"missing identity", func(r *core.PerformanceRequest) { r.ID = "" }},
		{"missing start", func(r *core.PerformanceRequest) { r.StartedAt = nil }},
		{"missing end", func(r *core.PerformanceRequest) { r.EndedAt = nil }},
		{"missing output", func(r *core.PerformanceRequest) { r.OutputTokens = nil }},
		{"negative output", func(r *core.PerformanceRequest) { r.OutputTokens = ptr(int64(-1)) }},
		{"oversized output", func(r *core.PerformanceRequest) { r.OutputTokens = ptr(MaxExactInteger + 1) }},
		{"invalid revision", func(r *core.PerformanceRequest) { r.Revision = -1 }},
		{"nonfinite duration", func(r *core.PerformanceRequest) { r.DurationMS = ptr(math.Inf(1)) }},
		{"conflicting duration", func(r *core.PerformanceRequest) { r.DurationMS = ptr(3000.) }},
		{"ambiguous", func(r *core.PerformanceRequest) { r.TimingStatus = core.TimingAmbiguous }},
		{"invalid", func(r *core.PerformanceRequest) { r.TimingStatus = core.TimingInvalid }},
		{"unsupported", func(r *core.PerformanceRequest) { r.TimingStatus = core.TimingUnsupported }},
		{"too short", func(r *core.PerformanceRequest) { r.StartedAt = ptr(r.EndedAt.Add(-99 * time.Millisecond)) }},
		{"too long", func(r *core.PerformanceRequest) { r.StartedAt = ptr(r.EndedAt.Add(-16 * time.Minute)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := observedCodexRequest()
			tc.edit(&r)
			got := NormalizeRequest(r)
			if got.ResponseTokS != nil || got.ExclusionReason == "" {
				t.Fatal("ineligible source or invalid evidence was measured")
			}
			if !reflect.DeepEqual(got, NormalizeRequest(got)) {
				t.Fatal("excluded normalization was not idempotent")
			}
		})
	}
	zero := observedCodexRequest()
	zero.OutputTokens = ptr(int64(0))
	near(t, NormalizeRequest(zero).ResponseTokS, 0)
}

func TestObservedCodexSummaryGroupsAndWindowRemainDistinct(t *testing.T) {
	r := observedCodexRequest()
	opts := options()
	opts.IncludeWindow = true
	p, err := Build([]core.PerformanceRequest{r}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if p.DefinitionVersion != "log_response_throughput_v2" || p.Summary.Coverage.Candidates != 1 || p.Summary.Coverage.Measured != 1 || p.Summary.Coverage.Unknown != 1 || p.Summary.Coverage.Inferred != 1 || p.Summary.Coverage.Excluded != 0 {
		t.Fatal("definition or overlapping measured/unknown coverage was lost")
	}
	near(t, p.Summary.Stats.WeightedResponseTokS, 75)
	if len(p.Groups) == 0 {
		t.Fatal("group projections missing")
	}
	for _, group := range p.Groups {
		near(t, group.Summary.Stats.WeightedResponseTokS, 75)
		if group.Summary.Coverage.Unknown != 1 {
			t.Fatal("group projection promoted unknown outcome")
		}
	}
	if len(p.Requests) != 1 || p.Requests[0].Outcome != core.OutcomeUnknown {
		t.Fatal("request outcome changed")
	}
	near(t, p.Requests[0].ResponseTokS, 75)
	if p.Window == nil || p.Window.Latest60Seconds.OutputTokens != 0 || p.Window.Latest60Seconds.WallTokS != nil {
		t.Fatal("observed summary speed broadened the successful live wall window")
	}
	for _, b := range p.Window.Buckets {
		if b.OutputTokens != 0 {
			t.Fatal("unknown outcome entered successful live buckets")
		}
	}
}
