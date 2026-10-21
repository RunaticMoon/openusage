package performance

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

var testNow = time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }
func request(id string, output int64, duration time.Duration) core.PerformanceRequest {
	start, end := testNow.Add(-duration), testNow
	return core.PerformanceRequest{ID: id, Namespace: "installation-A", SourceSystem: "tool", HostID: "host-a", AccountID: "account-A", BillingProviderID: "provider-A", SessionID: "session-A", ModelID: "model-A", OutputTokens: &output, OutputBasis: "provider_output_tokens", StartedAt: &start, EndedAt: &end, TimingBasis: core.TimingProviderRequestTimestamps, Scope: core.ScopeModelRequest, Final: true, Outcome: core.OutcomeSuccess, RecordedAt: end, ObservedAt: end, AdapterVersion: "fixture-v1"}
}
func options() BuildOptions {
	return BuildOptions{Period: core.PerformancePeriod{Start: testNow.Add(-24 * time.Hour), End: testNow.Add(time.Second)}, Now: testNow, DataAsOf: testNow, CoverageStart: ptr(testNow.Add(-time.Hour)), HostID: "host-a", FilterID: "all", BufferMinutes: 30, ExportIntervalMS: 30000}
}
func near(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-8 {
		t.Fatalf("got %v, want %v", got, want)
	}
}
func TestNormalizeResponseSpeed(t *testing.T) {
	cases := []struct {
		name   string
		r      core.PerformanceRequest
		status core.PerformanceTimingStatus
		rate   *float64
	}{
		{"75", request("75", 150, 2*time.Second), core.TimingValid, ptr(75.)},
		{"zero", request("zero", 0, time.Second), core.TimingValid, ptr(0.)},
		{"short direct", request("short", 5, time.Millisecond), core.TimingValid, ptr(5000.)},
		{"long direct", request("long", 7200, 2*time.Hour), core.TimingValid, ptr(1.)},
	}
	missing := request("missing", 5, time.Second)
	missing.OutputTokens = nil
	cases = append(cases, struct {
		name   string
		r      core.PerformanceRequest
		status core.PerformanceTimingStatus
		rate   *float64
	}{"missing", missing, core.TimingMissing, nil})
	invalid := request("inverted", 5, time.Second)
	invalid.StartedAt = ptr(testNow.Add(time.Second))
	cases = append(cases, struct {
		name   string
		r      core.PerformanceRequest
		status core.PerformanceTimingStatus
		rate   *float64
	}{"inverted", invalid, core.TimingInvalid, nil})
	nan := request("nan", 5, time.Second)
	nan.DurationMS = ptr(math.NaN())
	cases = append(cases, struct {
		name   string
		r      core.PerformanceRequest
		status core.PerformanceTimingStatus
		rate   *float64
	}{"nan", nan, core.TimingInvalid, nil})
	inferred := request("inferred", 5, 50*time.Millisecond)
	inferred.TimingBasis = core.TimingInferredLogBoundary
	cases = append(cases, struct {
		name   string
		r      core.PerformanceRequest
		status core.PerformanceTimingStatus
		rate   *float64
	}{"short inferred", inferred, core.TimingInvalid, nil})
	unknown := request("unknown", 5, time.Second)
	unknown.TimingBasis = core.TimingUnknown
	unknown.TimingStatus = core.TimingUnsupported
	unknown.ExclusionReason = "per_request_start_missing"
	cases = append(cases, struct {
		name   string
		r      core.PerformanceRequest
		status core.PerformanceTimingStatus
		rate   *float64
	}{"unsupported", unknown, core.TimingUnsupported, nil})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeRequest(tc.r)
			if got.TimingStatus != tc.status {
				t.Fatalf("status %s: %s", got.TimingStatus, got.ExclusionReason)
			}
			if tc.rate != nil {
				near(t, got.ResponseTokS, *tc.rate)
			} else if got.ResponseTokS != nil {
				t.Fatal("unexpected numeric rate")
			}
			again := NormalizeRequest(got)
			if !reflect.DeepEqual(got, again) {
				t.Fatalf("normalization must be idempotent: %s -> %s", got.ExclusionReason, again.ExclusionReason)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if tc.rate == nil && !strings.Contains(string(encoded), `"response_tok_s":null`) {
				t.Fatal("missing must serialize as null")
			}
		})
	}
	if NormalizeRequest(unknown).ExclusionReason != "per_request_start_missing" {
		t.Fatal("lost adapter reason")
	}
}
func TestWeightedCoverageAndOutputOnly(t *testing.T) {
	a, b := request("a", 100, time.Second), request("b", 100, 2*time.Second)
	b.TimingBasis = core.TimingInferredLogBoundary
	failed := request("failed", 500, time.Second)
	failed.Outcome = core.OutcomeFailed
	unknown := request("unknown", 500, time.Second)
	unknown.Outcome = core.OutcomeUnknown
	provisional := request("partial", 500, time.Second)
	provisional.Final = false
	missing := request("missing", 0, time.Second)
	missing.OutputTokens = nil
	s, err := Build([]core.PerformanceRequest{a, a, b, failed, unknown, provisional, missing}, options())
	if err != nil {
		t.Fatal(err)
	}
	near(t, s.Summary.Stats.WeightedResponseTokS, 200000./3000.)
	c := s.Summary.Coverage
	if c.Candidates != 5 || c.Measured != 2 || c.Direct != 1 || c.Inferred != 1 || c.Excluded != 3 || c.Provisional != 1 || c.Failed != 1 || c.Unknown != 1 {
		t.Fatalf("coverage %+v", c)
	}
	if s.Summary.Stats.OutputTokens != 200 {
		t.Fatal("wrong numerator")
	}
	zero := request("zero", 0, time.Second)
	zero.Evidence = []string{"reasoning_present"}
	s, err = Build([]core.PerformanceRequest{zero}, options())
	if err != nil {
		t.Fatal(err)
	}
	near(t, s.Summary.Stats.WeightedResponseTokS, 0)
}
func TestRevisionIndependentTimingAndIsolation(t *testing.T) {
	initial := request("id", 100, time.Second)
	initial.Revision = 1
	revision := initial.DeepClone()
	revision.OutputTokens = ptr(int64(50))
	revision.Revision = 2
	revision.TimingBasis = core.TimingInferredLogBoundary
	revision.StartedAt = ptr(testNow.Add(-2 * time.Second))
	got := PreferRequest(initial, revision)
	near(t, got.ResponseTokS, 50)
	if got.TimingBasis != core.TimingProviderRequestTimestamps || got.Revision != 2 {
		t.Fatal("lost revision or direct timing")
	}
	replay := PreferRequest(got, initial)
	near(t, replay.ResponseTokS, 50)
	another := initial.DeepClone()
	another.AccountID = "account-B"
	another.Namespace = "installation-B"
	s, err := Build([]core.PerformanceRequest{initial, revision, another}, options())
	if err != nil {
		t.Fatal(err)
	}
	if s.Summary.Stats.Count != 2 || s.Summary.Stats.OutputTokens != 150 {
		t.Fatal("identity dedup failed")
	}
	edited := initial.DeepClone()
	edited.ModelID = "changed"
	edited.SessionID = "changed"
	if RequestKey(initial) != RequestKey(edited) {
		t.Fatal("mutable identity")
	}
}
func TestHistogramMergeAndPercentiles(t *testing.T) {
	samples := []core.PerformanceRequest{request("one", 10, time.Second), request("two", 100, time.Second), request("zero", 0, time.Second)}
	whole, err := Build(samples, options())
	if err != nil {
		t.Fatal(err)
	}
	left, err := Build(samples[:1], options())
	if err != nil {
		t.Fatal(err)
	}
	right, err := Build(samples[1:], options())
	if err != nil {
		t.Fatal(err)
	}
	merged, err := MergeStats(left.Summary.Stats, right.Summary.Stats)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(merged, whole.Summary.Stats) {
		t.Fatalf("hist merge differs %+v / %+v", merged, whole.Summary.Stats)
	}
	for _, item := range []struct {
		got  *float64
		want float64
	}{{merged.P50TokS, 10}, {merged.P90TokS, 100}} {
		if item.got == nil || math.Abs(*item.got/item.want-1) > merged.Histogram.RelativeErrorBound {
			t.Fatal("percentile outside documented error")
		}
	}
	h := NewHistogram()
	if HistogramQuantile(h, .5) != nil {
		t.Fatal("empty histogram is not null")
	}
	addHistogram(&h, 0)
	near(t, HistogramQuantile(h, .9), 0)
	addHistogram(&h, math.Exp2(40))
	if HistogramQuantile(h, .9) != nil {
		t.Fatal("overflow must not clamp")
	}
	broken := merged.DeepClone()
	broken.Histogram.ZeroCount = MaxExactInteger
	if _, err := MergeStats(broken); err == nil {
		t.Fatal("invalid histogram accepted")
	}
}
func TestPeriodDoesNotUsePollTimeAndExplicitMerge(t *testing.T) {
	old := request("old", 20, time.Second)
	old.RecordedAt = testNow.Add(-48 * time.Hour)
	old.ObservedAt = testNow
	s, err := Build([]core.PerformanceRequest{old}, options())
	if err != nil {
		t.Fatal(err)
	}
	if s.Summary.Coverage.Candidates != 0 {
		t.Fatal("poll time used as event time")
	}
	a, _ := Build([]core.PerformanceRequest{request("a", 100, time.Second)}, options())
	r := request("b", 100, 2*time.Second)
	r.HostID = "host-b"
	opts := options()
	opts.HostID = "host-b"
	b, _ := Build([]core.PerformanceRequest{r}, opts)
	merged, err := Merge(testNow, a, b)
	if err != nil {
		t.Fatal(err)
	}
	near(t, merged.Summary.Stats.WeightedResponseTokS, 200000./3000.)
	if _, err := Merge(testNow, a, a); err == nil {
		t.Fatal("duplicate machine accepted")
	}
	b.FilterID = "other"
	if _, err := Merge(testNow, a, b); err == nil {
		t.Fatal("filter mismatch accepted")
	}
	b.FilterID = a.FilterID
	b.Stale = true
	merged, err = Merge(testNow, a, b)
	if err != nil {
		t.Fatal(err)
	}
	near(t, merged.Summary.Stats.WeightedResponseTokS, 100)
	if len(merged.ExcludedHosts) != 1 {
		t.Fatal("stale host not identified")
	}
}

func TestMergeExpiresCachedHostsAtQueryTime(t *testing.T) {
	a, err := Build([]core.PerformanceRequest{request("a", 100, time.Second)}, options())
	if err != nil {
		t.Fatal(err)
	}
	merged, err := Merge(testNow.Add(2*time.Minute), a)
	if err != nil {
		t.Fatal(err)
	}
	if !merged.Stale || merged.Summary.Stats.Count != 0 || len(merged.ExcludedHosts) != 1 {
		t.Fatal("aged cached snapshot included in live merge")
	}
}

func TestDelayedProvisionalCannotRegressFinal(t *testing.T) {
	final := request("final", 100, time.Second)
	final.Revision = 1
	delayed := final.DeepClone()
	delayed.Final = false
	delayed.Revision = 9
	delayed.OutputTokens = ptr(int64(900))
	delayed.SessionID = "copied-session"
	got := PreferRequest(final, delayed)
	if !got.Final || *got.OutputTokens != 100 || got.SessionID != final.SessionID {
		t.Fatal("delayed partial regressed final")
	}
	missingOutput := request("id", 0, time.Second)
	missingOutput.OutputTokens = nil
	inferred := request("id", 50, 2*time.Second)
	inferred.TimingBasis = core.TimingInferredLogBoundary
	inferred.Revision = 1
	got = PreferRequest(missingOutput, inferred)
	near(t, got.ResponseTokS, 50)
	if got.TimingBasis != core.TimingProviderRequestTimestamps {
		t.Fatal("missing output lost valid direct timing")
	}
}

func BenchmarkBuild1000(b *testing.B) {
	rows := make([]core.PerformanceRequest, 1000)
	for i := range rows {
		r := request(strconv.Itoa(i), 100, 30*time.Second)
		end := testNow.Add(-time.Duration(i) * time.Second)
		start := end.Add(-30 * time.Second)
		r.StartedAt = &start
		r.EndedAt = &end
		r.RecordedAt = end
		r.ModelID = "model-" + strconv.Itoa(i%10)
		rows[i] = r
	}
	for _, window := range []bool{false, true} {
		name := "summary"
		if window {
			name = "summary_and_window"
		}
		b.Run(name, func(b *testing.B) {
			opts := options()
			opts.IncludeWindow = window
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Build(rows, opts); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestFinalMeasurementsNeverBorrowPartialCountersOrEnd(t *testing.T) {
	partial := request("id", 100, time.Second)
	partial.Final = false
	partial.Outcome = core.OutcomeUnknown
	partial.TimingBasis = core.TimingInferredLogBoundary
	final := request("id", 200, 2*time.Second)
	final.Revision = 1
	final.TimingBasis = core.TimingUnknown
	final.StartedAt = nil
	final.EndedAt = nil
	final.DurationMS = nil
	final.RecordedAt = testNow
	got := PreferRequest(partial, final)
	if !got.Final || got.ResponseTokS != nil || got.TimingStatus == core.TimingProvisional {
		t.Fatal("partial end promoted to final measurement")
	}
	s, err := Build([]core.PerformanceRequest{partial, final}, options())
	if err != nil {
		t.Fatal(err)
	}
	if s.Summary.Coverage.Candidates != 1 || s.Summary.Coverage.Excluded != 1 || s.Summary.Coverage.Provisional != 0 {
		t.Fatal("final candidate lost")
	}
	final = request("id", 200, 2*time.Second)
	final.Revision = 1
	final.OutputTokens = nil
	got = PreferRequest(partial, final)
	if got.OutputTokens != nil || got.ResponseTokS != nil {
		t.Fatal("partial output promoted to final output")
	}
	partial.Revision = 5
	partial.TimingBasis = core.TimingProviderRequestTimestamps
	got = PreferRequest(final, partial)
	if !got.Final || got.OutputTokens != nil || got.ResponseTokS != nil {
		t.Fatal("late partial enriched final measurement")
	}
	contradictory := request("contradictory", 100, time.Second)
	contradictory.TimingStatus = core.TimingProvisional
	got = NormalizeRequest(contradictory)
	if got.TimingStatus != core.TimingMissing || got.EndedAt != nil || got.DurationMS != nil || got.ResponseTokS != nil {
		t.Fatal("contradictory final/provisional record trusted")
	}
	if !reflect.DeepEqual(got, NormalizeRequest(got)) {
		t.Fatal("contradictory normalization not idempotent")
	}
	enriched := got.DeepClone()
	enriched.EndedAt = ptr(testNow)
	enriched.TimingStatus = core.TimingValid
	recovered := NormalizeRequest(enriched)
	near(t, recovered.ResponseTokS, 100)
	if recovered.ExclusionReason != "" {
		t.Fatal("derived missing reason blocked later final timing")
	}
}
