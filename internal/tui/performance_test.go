package tui

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/janekbaraniewski/openusage/internal/core"
)

func performanceFixture(now time.Time) *core.PerformanceSnapshot {
	return &core.PerformanceSnapshot{
		DefinitionVersion: core.PerformanceDefinitionVersion,
		DataAsOf:          now.Add(-time.Second), GeneratedAt: now, ExportIntervalMS: 60000,
		Period: core.PerformancePeriod{Start: now.Add(-time.Hour), End: now},
		Summary: core.PerformanceSummary{
			Coverage: core.PerformanceCoverage{Candidates: 5, Measured: 3, Direct: 2, Inferred: 1, Excluded: 2, Unknown: 1, Reasons: map[string]int64{"missing_timing": 2}},
			Stats:    core.PerformanceStats{Count: 3, WeightedResponseTokS: core.Float64Ptr(20), P50TokS: core.Float64Ptr(15), P90TokS: core.Float64Ptr(30)},
			Direct:   core.PerformanceStats{Count: 2, WeightedResponseTokS: core.Float64Ptr(10)},
			Inferred: core.PerformanceStats{Count: 1, WeightedResponseTokS: core.Float64Ptr(40)},
		},
	}
}

func TestPerformanceRatesDistinguishUnknownZeroAndInvalid(t *testing.T) {
	for _, tc := range []struct {
		value *float64
		want  string
	}{
		{nil, "—"}, {core.Float64Ptr(0), "0.0"}, {core.Float64Ptr(-1), "—"},
		{core.Float64Ptr(math.NaN()), "—"}, {core.Float64Ptr(math.Inf(1)), "—"},
		{core.Float64Ptr(12.34), "12.3"},
	} {
		if got := performanceRate(tc.value); got != tc.want {
			t.Fatalf("rate = %q, want %q", got, tc.want)
		}
	}
}

func TestPerformanceDetailKeepsTimingClassesCoverageAndQuotaSeparate(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	p := performanceFixture(now)
	snap := core.NewUsageSnapshot("codex", "personal")
	snap.Status, snap.Performance = core.StatusOK, p
	out := RenderDetailContent(snap, now, 120, .3, .1, 0, core.TimeWindowAll, true)
	for _, want := range []string{"Performance", "not decode speed", "includes inferred", "Direct weighted 10.0", "Inferred weighted 40.0", "P50 15.0", "P90 30.0", "Coverage 3/5", "missing_timing: 2", "Source checked", "Export interval 60.0s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in performance-only detail: %s", want, out)
		}
	}
	for _, forbidden := range []string{"remaining", "quota", "%"} {
		if strings.Contains(strings.ToLower(strings.Join(buildDetailPerformanceLines(p, now, 120), "\n")), forbidden) {
			t.Fatalf("performance rendered as quota: %q", forbidden)
		}
	}
	if (Model{}).tileShouldRenderLoading(snap) {
		t.Fatal("performance-only snapshot is not a loading placeholder")
	}
}

func TestPerformanceUnknownTimingStaysVisibleAndFitsNarrowWidths(t *testing.T) {
	now := time.Now()
	p := performanceFixture(now)
	p.Summary = core.PerformanceSummary{Coverage: core.PerformanceCoverage{Candidates: 4, Excluded: 4, Unknown: 4}}
	p.DataAsOf = time.Time{}
	for _, width := range []int{30, 45, 80} {
		lines := append(buildDetailPerformanceLines(p, now, width), buildTilePerformanceLines(p, now, width)...)
		for _, line := range lines {
			if lipgloss.Width(line) > width {
				t.Fatalf("width %d exceeded: %q", width, line)
			}
		}
		out := strings.Join(lines, "\n")
		if !strings.Contains(out, "—") || !strings.Contains(out, "Coverage 0/4") || !strings.Contains(out, "unknown") {
			t.Fatalf("unsupported coverage not explicit at width %d: %s", width, out)
		}
	}
}

func TestPerformanceFreshnessUsesScanHeartbeatAndExportInterval(t *testing.T) {
	now := time.Now()
	p := performanceFixture(now)
	p.ExportIntervalMS = 90000
	p.DataAsOf = now.Add(-179 * time.Second)
	if got := performanceFreshness(p, now); got != "checked" {
		t.Fatalf("before threshold: %s", got)
	}
	p.DataAsOf = now.Add(-180 * time.Second)
	if got := performanceFreshness(p, now); got != "stale" {
		t.Fatalf("at threshold: %s", got)
	}
	p.DataAsOf = now.Add(time.Second)
	if got := performanceFreshness(p, now); got != "clock ahead" {
		t.Fatalf("future heartbeat: %s", got)
	}
	p.DataAsOf = time.Time{}
	if got := performanceFreshness(p, now); got != "unknown" {
		t.Fatalf("absent heartbeat: %s", got)
	}
	// Invalid transport intervals fall back to the engine's two-minute limit;
	// they cannot overflow or keep a cached Hub snapshot fresh indefinitely.
	p.DataAsOf = now.Add(-time.Hour)
	p.ExportIntervalMS = math.MaxInt64
	if got := performanceFreshness(p, now); got != "stale" {
		t.Fatalf("interval arithmetic overflow: %s", got)
	}
}

func TestPerformanceWindowLabelsPartialCoverageAndSuppressesStaleRate(t *testing.T) {
	now := time.Now()
	p := performanceFixture(now)
	p.Window = &core.PerformanceWindow{Latest60Seconds: core.PerformanceRateWindow{WallTokS: core.Float64Ptr(12), CoveredSeconds: 60, CompleteCoverage: true}}
	for _, tc := range []struct {
		stale, complete bool
		want            string
	}{
		{false, true, "Wall 60s output 12.0"}, {true, true, "Wall 60s output —"}, {false, false, "Wall 60s output 12.0"},
	} {
		p.Window.Stale, p.Window.Latest60Seconds.CompleteCoverage = tc.stale, tc.complete
		if out := strings.Join(buildDetailPerformanceLines(p, now, 120), "\n"); !strings.Contains(out, tc.want) {
			t.Fatalf("missing %q: %s", tc.want, out)
		}
		if out := strings.Join(buildDetailPerformanceLines(p, now, 120), "\n"); !strings.Contains(out, "time-allocation estimate") {
			t.Fatal("window allocation was presented as observed streaming throughput")
		}
		if !tc.complete && !strings.Contains(strings.Join(buildDetailPerformanceLines(p, now, 120), "\n"), "partial coverage") {
			t.Fatal("partial coverage was not disclosed")
		}
	}
}

func TestPerformanceCacheRefreshesWithoutQuotaTimestampChange(t *testing.T) {
	now := time.Now()
	snap := core.NewUsageSnapshot("codex", "personal")
	snap.Status, snap.Performance = core.StatusOK, performanceFixture(now)
	m := Model{referenceTime: now, timeWindow: core.TimeWindowAll}
	before := m.cachedDetailContent("personal", snap, 120, 0)
	widget := dashboardWidget(snap.ProviderID)
	di := computeDisplayInfo(snap, widget, false)
	tileBefore := strings.Join(m.cachedTileBodyLines(snap, widget, di, 90, false), "\n")
	*snap.Performance.Summary.Direct.WeightedResponseTokS = 7
	after := m.cachedDetailContent("personal", snap, 120, 0)
	tileAfter := strings.Join(m.cachedTileBodyLines(snap, widget, di, 90, false), "\n")
	if before == after || !strings.Contains(after, "Direct weighted 7.0") || tileBefore == tileAfter || !strings.Contains(tileAfter, "Direct 7.0") {
		t.Fatal("performance changed without refreshing cached detail and tile")
	}
	m.referenceTime = now.Add(121 * time.Second)
	if out := m.cachedDetailContent("personal", snap, 120, 0); !strings.Contains(out, "Source status: stale") {
		t.Fatal("an idle cached snapshot never became stale")
	}
	key := performanceRenderKey(snap.Performance, m.referenceTime)
	if key != performanceRenderKey(snap.Performance, m.referenceTime.Add(time.Hour)) {
		t.Fatal("stale clock ticks keep creating tile cache entries")
	}
}
