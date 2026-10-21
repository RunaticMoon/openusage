package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestPerformanceFreshnessRequiresEveryRepresentedSourceAccount(t *testing.T) {
	s, path := performanceTestStore(t, true)
	ctx := context.Background()
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	first := performanceFixture("personal", 100, 1000, at)
	first.PerformanceOnly = true
	ingestPerformance(t, s, first)
	second := performanceFixture("company", 100, 1000, at)
	second.PerformanceOnly = true
	second.AccountID = "company"
	second.Performance.AccountID = "company"
	ingestPerformance(t, s, second)
	if err := s.RecordPerformanceScan(ctx, "claude_code", "personal", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	query := PerformanceQuery{Source: "claude_code", Since: at.Add(-time.Hour), Now: at.Add(time.Second), IncludeWindow: true}
	p, err := QueryPerformance(ctx, path, query)
	if err != nil {
		t.Fatal(err)
	}
	if !p.DataAsOf.IsZero() || !p.Stale || p.Window.CoverageStart != nil {
		t.Fatal("one account heartbeat marked unscanned account fresh")
	}
	query.Account = "personal"
	p, err = QueryPerformance(ctx, path, query)
	if err != nil {
		t.Fatal(err)
	}
	if p.DataAsOf.IsZero() || p.Stale {
		t.Fatal("unselected unscanned account poisoned selected account")
	}
	query.Account = ""
	if err := s.RecordPerformanceScan(ctx, "claude_code", "company", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	p, err = QueryPerformance(ctx, path, query)
	if err != nil {
		t.Fatal(err)
	}
	if p.DataAsOf.IsZero() || p.Stale {
		t.Fatal("successful scan did not refresh cached selection")
	}
	third := performanceFixture("different-source", 100, 1000, at)
	third.SourceSystem = SourceSystem("codex")
	third.Performance.SourceSystem = "codex"
	third.PerformanceOnly = true
	ingestPerformance(t, s, third)
	query.Source = ""
	p, err = QueryPerformance(ctx, path, query)
	if err != nil {
		t.Fatal(err)
	}
	if !p.DataAsOf.IsZero() || !p.Stale {
		t.Fatal("another source inherited unrelated heartbeat")
	}
}

func TestPerformanceFreshScanWithoutObservableRequestsRemainsUnknown(t *testing.T) {
	s, path := performanceTestStore(t, true)
	ctx := context.Background()
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, when := range []time.Time{at.Add(-time.Hour), at} {
		if err := s.RecordPerformanceScan(ctx, "codex", "personal", when); err != nil {
			t.Fatal(err)
		}
	}
	query := PerformanceQuery{Source: "codex", Account: "personal", Since: at.Add(-time.Hour), Now: at, IncludeWindow: true}
	p, err := QueryPerformance(ctx, path, query)
	if err != nil {
		t.Fatal(err)
	}
	if p.Window.Latest60Seconds.WallTokS != nil || p.Window.Latest60Seconds.CompleteCoverage {
		t.Fatal("empty source scan fabricated idle zero")
	}
	unknown := performanceFixture("unknown", 100, 1000, at.Add(-time.Second))
	unknown.SourceSystem = SourceSystem("codex")
	unknown.Performance.SourceSystem = "codex"
	unknown.Performance.TimingBasis = core.TimingUnknown
	unknown.Performance.TimingStatus = core.TimingUnsupported
	unknown.Performance.StartedAt = nil
	unknown.Performance.DurationMS = nil
	unknown.Performance.ExclusionReason = "native_request_timing_unavailable"
	unknown.PerformanceOnly = true
	ingestPerformance(t, s, unknown)
	p, err = QueryPerformance(ctx, path, query)
	if err != nil {
		t.Fatal(err)
	}
	if p.Window.Latest60Seconds.WallTokS != nil || p.Summary.Stats.WeightedResponseTokS != nil || p.Summary.Coverage.Candidates != 1 {
		t.Fatal("unsupported source became measured zero")
	}
}
