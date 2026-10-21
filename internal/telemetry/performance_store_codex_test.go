package telemetry

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestPerformanceLateFinalOutcomeAndCorrectionPreserveAccounting(t *testing.T) {
	s, path := performanceTestStore(t, true)
	at := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	r := performanceFixture("native-response", 3000, 40000, at)
	r.Performance.Outcome = core.OutcomeUnknown
	r.Performance.TimingBasis = core.TimingInferredLogBoundary
	r.Performance.Evidence = []string{"native_request_boundary"}
	ingestPerformance(t, s, r)
	p := readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Candidates != 1 || p.Summary.Coverage.Measured != 0 {
		t.Fatal("unknown completion was measured")
	}
	var accountingBefore string
	if err := s.db.QueryRow(`SELECT json_array(input_tokens,output_tokens,cache_read_tokens,reasoning_tokens,total_tokens,requests,cost_usd) FROM usage_events`).Scan(&accountingBefore); err != nil {
		t.Fatal(err)
	}
	late := r
	v := r.Performance.DeepClone()
	late.Performance, late.PerformanceOnly = &v, true
	v.Outcome = core.OutcomeSuccess
	v.AdapterVersion = "native-completion-v2"
	v.Evidence = []string{"native_response_completed"}
	if !ingestPerformance(t, s, late).Updated {
		t.Fatal("same-revision final completion did not enrich outcome")
	}
	p = readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Measured != 1 || p.Summary.Stats.WeightedResponseTokS == nil || *p.Summary.Stats.WeightedResponseTokS != 75 {
		t.Fatal("late outcome did not invalidate cached summary")
	}
	var stored core.PerformanceRequest
	var encoded string
	if err := s.db.QueryRow(`SELECT record_json FROM performance_requests`).Scan(&encoded); err != nil || json.Unmarshal([]byte(encoded), &stored) != nil {
		t.Fatal("stored metadata unavailable")
	}
	if stored.AdapterVersion != "native-completion-v2" || !slices.Contains(stored.Evidence, "native_response_completed") || !slices.Contains(stored.Evidence, "native_request_boundary") {
		t.Fatal("late completion provenance lost")
	}
	for range 3 {
		if ingestPerformance(t, s, late).Updated {
			t.Fatal("replayed completion rewrote metadata")
		}
	}
	// A later native counter correction can survive expiry of the separate
	// timing log. Neither missing timing nor unknown outcome retracts proof.
	corrected := r
	correction := r.Performance.DeepClone()
	corrected.Performance, corrected.PerformanceOnly = &correction, true
	output := int64(1000)
	correction.OutputTokens, correction.Revision = &output, 2
	correction.StartedAt, correction.EndedAt, correction.DurationMS = nil, nil, nil
	correction.TimingBasis, correction.TimingStatus = core.TimingUnknown, core.TimingMissing
	correction.Evidence = nil
	ingestPerformance(t, s, corrected)
	p = readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Candidates != 1 || p.Summary.Coverage.Measured != 1 || p.Summary.Stats.OutputTokens != 1000 || *p.Summary.Stats.WeightedResponseTokS != 25 {
		t.Fatal("downward correction lost independent final proof or duplicated request")
	}
	var accountingAfter string
	var count int
	if err := s.db.QueryRow(`SELECT json_array(input_tokens,output_tokens,cache_read_tokens,reasoning_tokens,total_tokens,requests,cost_usd) FROM usage_events`).Scan(&accountingAfter); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&count); err != nil || count != 1 || accountingAfter != accountingBefore {
		t.Fatal("performance enrichment changed canonical accounting")
	}
	if ingestPerformance(t, s, corrected).Updated {
		t.Fatal("correction replay rewrote metadata")
	}
}

func TestPerformanceOutcomeEnrichmentRequiresIndependentFinalEvidence(t *testing.T) {
	at := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	for _, outcome := range []core.PerformanceOutcome{core.OutcomeSuccess, core.OutcomeFailed} {
		for _, kind := range []string{"same-final", "older-final", "newer-partial"} {
			t.Run(string(outcome)+"/"+kind, func(t *testing.T) {
				old := performanceFixture("response", 10, 1000, at).Performance.DeepClone()
				old.Outcome, old.Revision = core.OutcomeUnknown, 2
				in := old.DeepClone()
				in.Outcome = outcome
				switch kind {
				case "older-final":
					in.Revision = 1
				case "newer-partial":
					in.Revision, in.Final = 3, false
				}
				got := mergePerformanceObservation(old, in)
				want := core.OutcomeUnknown
				if kind == "same-final" {
					want = outcome
				}
				if got.Outcome != want || !got.Final {
					t.Fatal("older or partial observation certified a final outcome")
				}
			})
		}
	}
}

func TestPerformanceInferredRevocationIsStickyAndNeverReplacesDirect(t *testing.T) {
	for _, status := range []core.PerformanceTimingStatus{core.TimingAmbiguous, core.TimingInvalid} {
		t.Run(string(status), func(t *testing.T) {
			s, path := performanceTestStore(t, true)
			at := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
			r := performanceFixture("response", 100, 2000, at)
			r.PerformanceOnly = true
			r.Performance.TimingBasis = core.TimingInferredLogBoundary
			ingestPerformance(t, s, r)
			if readPerformance(t, path, at, "personal").Summary.Coverage.Measured != 1 {
				t.Fatal("initial inferred timing missing")
			}
			bad := r
			v := r.Performance.DeepClone()
			bad.Performance = &v
			v.TimingStatus, v.ExclusionReason = status, "native_request_boundary_conflict"
			v.Evidence = []string{"native_conflicting_completion"}
			v.AdapterVersion = "native-completion-v2"
			if !ingestPerformance(t, s, bad).Updated || readPerformance(t, path, at, "personal").Summary.Coverage.Measured != 0 {
				t.Fatal("positive contradiction did not revoke cached inference")
			}
			for range 3 {
				if ingestPerformance(t, s, r).Updated {
					t.Fatal("stale valid replay resurrected revoked inference")
				}
			}
			reopened, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.EnablePerformance(context.Background()); err != nil {
				t.Fatal(err)
			}
			if ingestPerformance(t, reopened, r).Updated {
				t.Fatal("restart lost sticky revocation")
			}
			newRevision := r
			newEvidence := r.Performance.DeepClone()
			newRevision.Performance = &newEvidence
			newEvidence.Revision++
			ingestPerformance(t, s, newRevision)
			if readPerformance(t, path, at, "personal").Summary.Coverage.Measured != 1 {
				t.Fatal("new final usage revision could not supply valid evidence")
			}
			direct := r.Performance.DeepClone()
			direct.TimingBasis = core.TimingProviderRequestTimestamps
			in := bad.Performance.DeepClone()
			in.Revision = 10
			got := mergePerformanceObservation(direct, in)
			if got.TimingBasis != direct.TimingBasis || got.TimingStatus != direct.TimingStatus || !reflect.DeepEqual(got.DurationMS, direct.DurationMS) {
				t.Fatal("inferred contradiction replaced direct evidence")
			}
			got = mergePerformanceObservation(*bad.Performance, direct)
			if got.TimingBasis != core.TimingProviderRequestTimestamps {
				t.Fatal("stronger direct evidence could not recover at same revision")
			}
		})
	}
}

func TestPerformanceUnmappedCompletionBarrierCanRecoverAfterIdentityArrives(t *testing.T) {
	s, path := performanceTestStore(t, true)
	at := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	r := performanceFixture("response", 100, 2000, at)
	r.PerformanceOnly = true
	r.Performance.TimingBasis = core.TimingInferredLogBoundary
	ingestPerformance(t, s, r)
	barrier := r
	v := r.Performance.DeepClone()
	barrier.Performance = &v
	v.TimingStatus, v.ExclusionReason = core.TimingAmbiguous, "ambiguous_unmapped_completion"
	ingestPerformance(t, s, barrier)
	if readPerformance(t, path, at, "personal").Summary.Coverage.Measured != 0 {
		t.Fatal("unmapped completed response did not suspend inference")
	}
	missing := r
	missingEvidence := r.Performance.DeepClone()
	missing.Performance = &missingEvidence
	missingEvidence.StartedAt, missingEvidence.EndedAt, missingEvidence.DurationMS = nil, nil, nil
	missingEvidence.TimingBasis, missingEvidence.TimingStatus = core.TimingUnknown, core.TimingMissing
	ingestPerformance(t, s, missing)
	if readPerformance(t, path, at, "personal").Summary.Coverage.Measured != 0 {
		t.Fatal("missing logs cleared unresolved completion barrier")
	}
	if !ingestPerformance(t, s, r).Updated || readPerformance(t, path, at, "personal").Summary.Coverage.Measured != 1 {
		t.Fatal("resolved response identity could not recover at the same usage revision")
	}
	conflict := barrier
	conflictingEvidence := barrier.Performance.DeepClone()
	conflict.Performance = &conflictingEvidence
	conflictingEvidence.ExclusionReason = "native_request_boundary_conflict"
	ingestPerformance(t, s, conflict)
	// A weaker temporary barrier must not replace a positive contradiction and
	// thereby allow a stale valid replay to bypass its terminal priority.
	if ingestPerformance(t, s, barrier).Updated || ingestPerformance(t, s, r).Updated {
		t.Fatal("temporary ambiguity weakened positive contradiction")
	}
	if readPerformance(t, path, at, "personal").Summary.Coverage.Measured != 0 {
		t.Fatal("positive contradiction recovered without newer evidence")
	}
}

func TestPerformanceLateCompletionWithoutStartUpdatesMissingDiagnostic(t *testing.T) {
	s, path := performanceTestStore(t, true)
	at := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	r := performanceFixture("response", 100, 2000, at)
	r.PerformanceOnly = true
	r.Performance.Outcome = core.OutcomeUnknown
	r.Performance.StartedAt, r.Performance.EndedAt, r.Performance.DurationMS = nil, nil, nil
	r.Performance.TimingBasis, r.Performance.TimingStatus = core.TimingUnknown, core.TimingUnsupported
	r.Performance.ExclusionReason = "missing_native_completion"
	ingestPerformance(t, s, r)
	late := r
	v := r.Performance.DeepClone()
	late.Performance = &v
	v.Outcome, v.TimingStatus, v.ExclusionReason = core.OutcomeSuccess, core.TimingMissing, "missing_start"
	v.AdapterVersion = "native-completion-v2"
	v.Evidence = []string{"native_response_completed"}
	ingestPerformance(t, s, late)
	var encoded string
	var stored core.PerformanceRequest
	if err := s.db.QueryRow(`SELECT record_json FROM performance_requests`).Scan(&encoded); err != nil || json.Unmarshal([]byte(encoded), &stored) != nil {
		t.Fatal("stored metadata unavailable")
	}
	if stored.Outcome != core.OutcomeSuccess || stored.TimingStatus != core.TimingMissing || stored.ExclusionReason != "missing_start" || stored.AdapterVersion != "native-completion-v2" || stored.DurationMS != nil {
		t.Fatal("late completion retained obsolete absence diagnostic or invented duration")
	}
	if readPerformance(t, path, at, "personal").Summary.Coverage.Measured != 0 || ingestPerformance(t, s, late).Updated {
		t.Fatal("missing start was measured or completion replay changed metadata")
	}
}

func TestPerformanceCommittedRevocationReplacesIncrementalWindowContribution(t *testing.T) {
	s, path := performanceTestStore(t, true)
	now := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	r := performanceFixture("response", 600, 20000, now.Add(-10*time.Second))
	r.PerformanceOnly = true
	r.Performance.TimingBasis = core.TimingInferredLogBoundary
	ctx := context.Background()
	for _, at := range []time.Time{now.Add(-2 * time.Minute), now} {
		if err := s.RecordPerformanceScan(ctx, "claude_code", "personal", at); err != nil {
			t.Fatal(err)
		}
	}
	q := PerformanceQuery{Source: "claude_code", Account: "personal", Since: now.Add(-time.Hour), Now: now, IncludeWindow: true}
	assertView := func(measured bool) {
		t.Helper()
		p, err := QueryPerformance(ctx, path, q)
		if err != nil || p.Window == nil {
			t.Fatal("incremental performance view unavailable")
		}
		if p.Summary.Coverage.Candidates != 1 {
			t.Fatal("evidence update changed request identity")
		}
		window := p.Window.Latest60Seconds
		var bucketTokens int64
		for _, b := range p.Window.Buckets {
			bucketTokens += b.OutputTokens
		}
		if measured {
			if p.Summary.Coverage.Measured != 1 || p.Summary.Stats.WeightedResponseTokS == nil || *p.Summary.Stats.WeightedResponseTokS != 30 || window.OutputTokens != 600 || window.WallTokS == nil || *window.WallTokS != 10 || bucketTokens != 600 {
				t.Fatal("summary and rolling window did not retain the same valid contribution")
			}
		} else if p.Summary.Coverage.Measured != 0 || p.Summary.Stats.WeightedResponseTokS != nil || window.OutputTokens != 0 || window.WallTokS != nil || bucketTokens != 0 {
			t.Fatal("authoritative revocation left stale output or a false zero rate in the rolling window")
		}
	}
	ingestPerformance(t, s, r)
	assertView(true)
	ambiguous := r
	barrier := r.Performance.DeepClone()
	ambiguous.Performance = &barrier
	barrier.TimingStatus, barrier.ExclusionReason = core.TimingAmbiguous, "ambiguous_unmapped_completion"
	ingestPerformance(t, s, ambiguous)
	assertView(false)
	// The same native usage revision becomes measurable again once the
	// independent missing identity is resolved. No second request is created.
	ingestPerformance(t, s, r)
	assertView(true)
	invalid := r
	conflict := r.Performance.DeepClone()
	invalid.Performance = &conflict
	conflict.TimingStatus, conflict.ExclusionReason = core.TimingInvalid, "native_clock_conflict"
	ingestPerformance(t, s, invalid)
	assertView(false)
	if ingestPerformance(t, s, r).Updated {
		t.Fatal("stale replay cleared positive contradiction")
	}
	assertView(false)
}
