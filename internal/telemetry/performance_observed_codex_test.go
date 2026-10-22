package telemetry

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestObservedCodexStoredIntervalReadReplayAndRevocation(t *testing.T) {
	s, path := performanceTestStore(t, true)
	ctx := context.Background()
	at := time.Date(2026, 10, 21, 12, 0, 0, 123456789, time.UTC)
	r := reviewedCodexV2(codexV2StoreFixture(at), core.OutcomeUnknown, false)
	r.PerformanceOnly = false
	ingestPerformance(t, s, r)
	accounting := func() string {
		t.Helper()
		var value string
		if err := s.db.QueryRow(`SELECT json_array(event_id,input_tokens,output_tokens,reasoning_tokens,cache_read_tokens,cache_write_tokens,total_tokens,cost_usd,requests,raw_event_id) FROM usage_events`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := accounting()
	// An existing v2-adapter row written under the old throughput definition
	// carries a valid interval, unknown outcome and a null derived speed.
	var raw string
	if err := s.db.QueryRow(`SELECT record_json FROM performance_requests`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var old core.PerformanceRequest
	if err := json.Unmarshal([]byte(raw), &old); err != nil {
		t.Fatal(err)
	}
	old.ResponseTokS, old.ExclusionReason = nil, "unknown_outcome"
	encoded, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE performance_requests SET record_json=?`, string(encoded)); err != nil {
		t.Fatal(err)
	}
	check := func(measured bool) {
		t.Helper()
		p, err := QueryPerformance(ctx, path, PerformanceQuery{Source: "codex", Account: "personal", Since: at.Add(-time.Hour), Now: at.Add(time.Second), IncludeWindow: true})
		if err != nil || p.DefinitionVersion != "log_response_throughput_v2" || p.Summary.Coverage.Candidates != 1 {
			t.Fatal("stored observation missing from v2 projection")
		}
		if measured {
			if p.Summary.Coverage.Measured != 1 || p.Summary.Coverage.Unknown != 1 || p.Summary.Coverage.Inferred != 1 || p.Summary.Stats.WeightedResponseTokS == nil || *p.Summary.Stats.WeightedResponseTokS != 30 || len(p.Requests) != 1 || p.Requests[0].Outcome != core.OutcomeUnknown {
				t.Fatal("stored unknown interval was not measured without outcome promotion")
			}
		} else if p.Summary.Coverage.Measured != 0 || p.Summary.Coverage.Failed != 1 || p.Summary.Stats.WeightedResponseTokS != nil {
			t.Fatal("explicit failure did not revoke observed speed")
		}
		if p.Window == nil || p.Window.Latest60Seconds.WallTokS != nil || p.Window.Latest60Seconds.OutputTokens != 0 {
			t.Fatal("unknown/failed observation entered successful live window")
		}
	}
	check(true)
	var unchanged string
	if err := s.db.QueryRow(`SELECT record_json FROM performance_requests`).Scan(&unchanged); err != nil || unchanged != string(encoded) {
		t.Fatal("read-time throughput projection rewrote stored metadata")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.EnablePerformance(ctx); err != nil {
		t.Fatal(err)
	}
	check(true)
	// The first normal ingestion may remove the obsolete exclusion reason.
	// Subsequent identical source observations must not rewrite metadata.
	r.PerformanceOnly = true
	ingestPerformance(t, s, r)
	for range 3 {
		if ingestPerformance(t, s, r).Updated {
			t.Fatal("observed replay changed stable metadata")
		}
	}
	check(true)
	failed := reviewedCodexV2(r, core.OutcomeFailed, false)
	if !ingestPerformance(t, s, failed).Updated {
		t.Fatal("explicit failed evidence was ignored")
	}
	check(false)
	ingestPerformance(t, s, r)
	check(false)
	var canonical, rawCount, perf, schema int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM usage_events`:                             &canonical,
		`SELECT COUNT(*) FROM usage_raw_events`:                         &rawCount,
		`SELECT COUNT(*) FROM performance_requests`:                     &perf,
		`SELECT value FROM performance_meta WHERE key='schema_version'`: &schema,
	} {
		if err := s.db.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if accounting() != before || canonical != 1 || rawCount != 1 || perf != 1 || schema != 1 {
		t.Fatal("normalization/replay changed accounting, identity or schema")
	}
}
