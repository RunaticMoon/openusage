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

func codexV2StoreFixture(at time.Time) IngestRequest {
	r := performanceFixture("native-response", 600, 20000, at)
	r.SourceSystem, r.ProviderID = "codex", "openai"
	r.Performance.SourceSystem, r.Performance.Namespace = "codex", "codex:response"
	r.Performance.AdapterVersion = "codex_native_sqlite_v1"
	r.Performance.TimingBasis = core.TimingInferredLogBoundary
	r.Performance.OutputBasis = "codex.token_usage_record.usage.output_tokens"
	r.Performance.Evidence = []string{"native_response_id", "native_completed_response_id"}
	return r
}

func reviewedCodexV2(r IngestRequest, outcome core.PerformanceOutcome, missing bool) IngestRequest {
	v := r.Performance.DeepClone()
	r.Performance, r.PerformanceOnly = &v, true
	v.AdapterVersion, v.Outcome = "codex_native_sqlite_v2", outcome
	v.Evidence = []string{"codex_outcome_v2:" + string(outcome), "native_response_id"}
	if missing {
		v.StartedAt, v.EndedAt, v.DurationMS = nil, nil, nil
		v.TimingBasis, v.TimingStatus = core.TimingUnknown, core.TimingMissing
		v.ExclusionReason = "native_outcome_unverified"
	}
	return r
}

func TestPerformanceCodexV2RevalidationReplacesLedgerAndRingOnly(t *testing.T) {
	for _, outcome := range []core.PerformanceOutcome{core.OutcomeUnknown, core.OutcomeFailed} {
		t.Run(string(outcome), func(t *testing.T) {
			s, path := performanceTestStore(t, true)
			ctx := context.Background()
			now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
			r := codexV2StoreFixture(now.Add(-10 * time.Second))
			if outcome == core.OutcomeFailed {
				r = reviewedCodexV2(r, core.OutcomeSuccess, false)
				r.PerformanceOnly = false
			}
			ingestPerformance(t, s, r)
			for _, at := range []time.Time{now.Add(-2 * time.Minute), now} {
				if err := s.RecordPerformanceScan(ctx, "codex", "personal", at); err != nil {
					t.Fatal(err)
				}
			}
			accounting := func() string {
				t.Helper()
				var value string
				if err := s.db.QueryRow(`SELECT json_array(event_id,input_tokens,output_tokens,reasoning_tokens,cache_read_tokens,cache_write_tokens,total_tokens,cost_usd,requests,raw_event_id) FROM usage_events`).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := accounting()
			q := PerformanceQuery{Source: "codex", Account: "personal", Since: now.Add(-time.Hour), Now: now, IncludeWindow: true}
			check := func(measured bool) {
				t.Helper()
				p, err := QueryPerformance(ctx, path, q)
				if err != nil || p.Window == nil || p.Summary.Coverage.Candidates != 1 {
					t.Fatal("request projection unavailable or identity changed")
				}
				var bucketTokens int64
				for _, bucket := range p.Window.Buckets {
					bucketTokens += bucket.OutputTokens
				}
				if measured {
					if p.Summary.Coverage.Measured != 1 || *p.Summary.Stats.WeightedResponseTokS != 30 || p.Window.Latest60Seconds.OutputTokens != 600 || bucketTokens != 600 {
						t.Fatal("initial inference unavailable")
					}
				} else if p.Summary.Coverage.Measured != 0 || p.Summary.Stats.WeightedResponseTokS != nil || p.Window.Latest60Seconds.WallTokS != nil || bucketTokens != 0 {
					t.Fatal("old success survived authoritative revalidation in summary or ring")
				}
			}
			// Current normalization already distrusts legacy v1 success. Use
			// explicit v2 success to exercise warm-ring removal on interruption.
			check(outcome == core.OutcomeFailed)
			correction := reviewedCodexV2(r, outcome, true)
			if !ingestPerformance(t, s, correction).Updated {
				t.Fatal("v1 outcome revalidation was ignored")
			}
			check(false)
			for range 3 {
				if ingestPerformance(t, s, correction).Updated {
					t.Fatal("v2 replay rewrote metadata")
				}
				old := r
				old.PerformanceOnly = true
				if ingestPerformance(t, s, old).Updated {
					t.Fatal("old success replay resurrected retired evidence")
				}
			}
			check(false)
			if accounting() != before {
				t.Fatal("metadata correction changed prior canonical accounting or identity")
			}
			var canonical, raw, perf, schema int
			for query, target := range map[string]*int{
				`SELECT COUNT(*) FROM usage_events`:                             &canonical,
				`SELECT COUNT(*) FROM usage_raw_events`:                         &raw,
				`SELECT COUNT(*) FROM performance_requests`:                     &perf,
				`SELECT value FROM performance_meta WHERE key='schema_version'`: &schema,
			} {
				if err := s.db.QueryRow(query).Scan(target); err != nil {
					t.Fatal(err)
				}
			}
			if canonical != 1 || raw != 1 || perf != 1 || schema != 1 {
				t.Fatal("correction duplicated history or changed SQLite compatibility")
			}
			reopened, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.EnablePerformance(ctx); err != nil {
				t.Fatal(err)
			}
			old := r
			old.PerformanceOnly = true
			if ingestPerformance(t, reopened, old).Updated {
				t.Fatal("restart lost the reviewed adapter authority")
			}
			check(false)
		})
	}
}

func TestPerformanceLegacyCodexV1ReadDowngradeDoesNotMutateStoredData(t *testing.T) {
	s, path := performanceTestStore(t, true)
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	r := codexV2StoreFixture(at)
	ingestPerformance(t, s, r)
	// A pre-upgrade database contains v1's original success classification.
	// Seed exactly that JSON in the isolated fixture, bypassing new ingestion.
	legacy, err := json.Marshal(r.Performance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE performance_requests SET record_json=?`, string(legacy)); err != nil {
		t.Fatal(err)
	}
	q := PerformanceQuery{Source: "codex", Account: "personal", Since: at.Add(-time.Hour), Now: at.Add(time.Second), IncludeWindow: true}
	for range 2 {
		p, err := QueryPerformance(context.Background(), path, q)
		if err != nil || p.Summary.Coverage.Candidates != 1 || p.Summary.Coverage.Measured != 0 || p.Summary.Coverage.Unknown != 1 || p.Summary.Stats.WeightedResponseTokS != nil {
			t.Fatal("unrecollected legacy success remained measurable")
		}
		if p.Window == nil || p.Window.Latest60Seconds.OutputTokens != 0 || p.Window.Latest60Seconds.WallTokS != nil {
			t.Fatal("legacy inference remained in the live ring")
		}
		var after string
		if err := s.db.QueryRow(`SELECT record_json FROM performance_requests`).Scan(&after); err != nil || after != string(legacy) {
			t.Fatal("read-only downgrade rewrote legacy database data")
		}
	}
	correction := reviewedCodexV2(r, core.OutcomeUnknown, true)
	if !ingestPerformance(t, s, correction).Updated {
		t.Fatal("recollected v2 metadata did not supersede stored v1")
	}
	var after string
	var stored core.PerformanceRequest
	if err := s.db.QueryRow(`SELECT record_json FROM performance_requests`).Scan(&after); err != nil || json.Unmarshal([]byte(after), &stored) != nil || stored.AdapterVersion != "codex_native_sqlite_v2" || stored.Outcome != core.OutcomeUnknown {
		t.Fatal("v2 replacement was not persisted compatibly")
	}
}

func TestPerformanceCodexV2RecoversV1AmbiguityWithFreshTiming(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	old := codexV2StoreFixture(at).Performance.DeepClone()
	old.TimingStatus, old.ExclusionReason = core.TimingAmbiguous, "ambiguous_parallel_sampling"
	for _, missing := range []bool{false, true} {
		incoming := reviewedCodexV2(codexV2StoreFixture(at), core.OutcomeSuccess, missing).Performance.DeepClone()
		got := mergePerformanceObservation(old, incoming)
		if got.AdapterVersion != "codex_native_sqlite_v2" || got.Outcome != core.OutcomeSuccess {
			t.Fatal("adapter authority did not replace v1 ambiguity")
		}
		if missing {
			if got.DurationMS != nil || got.TimingStatus != core.TimingMissing {
				t.Fatal("fresh outcome reused the unreviewed old interval")
			}
		} else if got.TimingStatus != core.TimingValid || got.DurationMS == nil || *got.DurationMS != 20000 {
			t.Fatal("fresh v2 timing could not recover old poisoning")
		}
	}
}

func TestPerformanceCodexV2OutcomeAuthorityAndIndependentOutputCorrection(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	v1 := codexV2StoreFixture(at)
	unknown := reviewedCodexV2(v1, core.OutcomeUnknown, true).Performance.DeepClone()
	success := reviewedCodexV2(v1, core.OutcomeSuccess, false).Performance.DeepClone()
	failed := reviewedCodexV2(v1, core.OutcomeFailed, false).Performance.DeepClone()
	got := mergePerformanceObservation(unknown, success)
	if got.Outcome != core.OutcomeSuccess || got.TimingStatus != core.TimingValid {
		t.Fatal("explicit late final proof did not enrich v2 unknown")
	}
	got = mergePerformanceObservation(got, unknown)
	if got.Outcome != core.OutcomeSuccess || got.DurationMS == nil {
		t.Fatal("incidental unknown erased established v2 proof")
	}
	got.Revision = 2
	failed.Evidence = append(failed.Evidence, "native_interrupted_response")
	got = mergePerformanceObservation(got, failed)
	if got.Outcome != core.OutcomeFailed || !slices.Contains(got.Evidence, "codex_outcome_v2:failed") || !slices.Contains(got.Evidence, "native_interrupted_response") || slices.Contains(got.Evidence, "codex_outcome_v2:success") || got.Revision != 2 {
		t.Fatal("late interruption did not revoke success and its certificate")
	}
	for _, replay := range []core.PerformanceRequest{success, unknown, v1.Performance.DeepClone()} {
		got = mergePerformanceObservation(got, replay)
		if got.Outcome != core.OutcomeFailed || got.AdapterVersion != "codex_native_sqlite_v2" {
			t.Fatal("stale outcome evidence resurrected failure")
		}
	}
	// Native accounting corrections have a separate revision. They can lower
	// the output without changing the independently reviewed terminal outcome.
	correction := v1.Performance.DeepClone()
	output := int64(25)
	correction.OutputTokens = &output
	for i, adapter := range []string{"codex_native_sqlite_v1", "codex_token_usage_record_v1"} {
		output = 25 - int64(i)*10
		correction.Revision = 100 + int64(i)
		correction.AdapterVersion = adapter
		got = mergePerformanceObservation(got, correction)
		if *got.OutputTokens != output || got.Revision != correction.Revision || got.Outcome != core.OutcomeFailed || got.AdapterVersion != "codex_native_sqlite_v2" {
			t.Fatal("counter revision changed outcome authority or was ignored")
		}
	}
	if got.SessionID != v1.Performance.SessionID || got.ID != v1.Performance.ID {
		t.Fatal("correction changed stable native identity")
	}
}

func TestPerformanceCodexV2CertificateMustMatchAndRemainScoped(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	base := reviewedCodexV2(codexV2StoreFixture(at), core.OutcomeUnknown, true).Performance.DeepClone()
	for _, kind := range []string{"missing", "mismatch", "conflict", "partial", "other-source", "other-namespace"} {
		t.Run(kind, func(t *testing.T) {
			in := reviewedCodexV2(codexV2StoreFixture(at), core.OutcomeSuccess, false).Performance.DeepClone()
			switch kind {
			case "missing":
				in.Evidence = nil
			case "mismatch":
				in.Evidence = []string{"codex_outcome_v2:unknown"}
			case "conflict":
				in.Evidence = append(in.Evidence, "codex_outcome_v2:failed")
			case "partial":
				in.Final = false
			case "other-source":
				in.SourceSystem = "claude_code"
			case "other-namespace":
				in.Namespace = "unrelated"
			}
			if codexReviewedOutcome(in) || mergePerformanceObservation(base, in).Outcome != core.OutcomeUnknown {
				t.Fatal("unreviewed or mismatched certificate proved a final outcome")
			}
		})
	}
}

func TestPerformanceCodexV2ReplayDoesNotReorderEvidence(t *testing.T) {
	for _, outcome := range []core.PerformanceOutcome{core.OutcomeUnknown, core.OutcomeSuccess, core.OutcomeFailed} {
		t.Run(string(outcome), func(t *testing.T) {
			s, _ := performanceTestStore(t, true)
			r := reviewedCodexV2(codexV2StoreFixture(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)), outcome, false)
			r.Performance.Evidence = []string{"native_response_id", "per_response_usage", "codex_outcome_v2:" + string(outcome)}
			ingestPerformance(t, s, r)
			for range 3 {
				if ingestPerformance(t, s, r).Updated {
					t.Fatal("identical v2 replay changed evidence ordering or metadata")
				}
			}
		})
	}
}

func TestPerformanceCodexV2ParallelClosureRecoversButPositiveConflictStays(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	valid := reviewedCodexV2(codexV2StoreFixture(at), core.OutcomeSuccess, false).Performance.DeepClone()
	for _, reason := range []string{"ambiguous_parallel_sampling", "ambiguous_unmapped_completion", "ambiguous_sampling_starts", "native_duplicate_completion"} {
		old := valid.DeepClone()
		old.TimingStatus, old.ExclusionReason = core.TimingAmbiguous, reason
		got := mergePerformanceObservation(old, valid)
		recoverable := reason == "ambiguous_parallel_sampling" || reason == "ambiguous_unmapped_completion"
		if (got.TimingStatus == core.TimingValid) != recoverable {
			t.Fatalf("wrong same-revision recovery policy for %s", reason)
		}
	}
}

func TestPerformanceCodexV2PreservesIndependentDirectTiming(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, basis := range []core.PerformanceTimingBasis{core.TimingProviderRequestTimestamps, core.TimingProviderReportedDuration} {
		old := codexV2StoreFixture(at).Performance.DeepClone()
		old.TimingBasis = basis
		incoming := reviewedCodexV2(codexV2StoreFixture(at), core.OutcomeUnknown, true).Performance.DeepClone()
		got := mergePerformanceObservation(old, incoming)
		if got.Outcome != core.OutcomeUnknown || got.TimingBasis != basis || !reflect.DeepEqual(got.DurationMS, old.DurationMS) || got.AdapterVersion != "codex_native_sqlite_v2" {
			t.Fatal("outcome revalidation erased direct timing or retained old success")
		}
		// Confirm old and new representations remain ordinary schema-v1 JSON.
		encoded, err := json.Marshal(got)
		var decoded core.PerformanceRequest
		if err != nil || json.Unmarshal(encoded, &decoded) != nil || !reflect.DeepEqual(got, decoded) {
			t.Fatal("v2 metadata is not compatible with the existing JSON representation")
		}
	}
}
