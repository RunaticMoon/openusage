package tui

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/janekbaraniewski/openusage/internal/core"
)

// These are output-only response rates. No gauge, quota percentage or decode
// speed is derived from them. Untimed observations remain visible in coverage.
func performanceRate(value *float64) string {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f", *value)
}

func performanceText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

// DataAsOf is the last successful source scan, not the last request timestamp.
// This presentation threshold is independent of the Hub's storage TTL.
func performanceFreshness(p *core.PerformanceSnapshot, now time.Time) string {
	if p.DataAsOf.IsZero() {
		return "unknown"
	}
	if p.DataAsOf.After(now) {
		return "clock ahead"
	}
	thresholdSeconds := float64(120)
	if p.ExportIntervalMS > 0 && p.ExportIntervalMS <= int64((24*time.Hour)/time.Millisecond) {
		thresholdSeconds = math.Max(thresholdSeconds, float64(p.ExportIntervalMS)/500)
	}
	if p.Stale || now.Sub(p.DataAsOf).Seconds() >= thresholdSeconds {
		return "stale"
	}
	return "checked"
}

func performanceStamp(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Local().Format("2006-01-02 15:04:05 MST")
}

func performanceSourceAge(p *core.PerformanceSnapshot, now time.Time) string {
	if p.DataAsOf.IsZero() || p.DataAsOf.After(now) {
		return "—"
	}
	seconds := int64(now.Sub(p.DataAsOf).Seconds())
	if seconds < 10 {
		return "<10s"
	}
	if seconds < 120 {
		return fmt.Sprintf("~%ds", seconds/10*10)
	}
	return fmt.Sprintf("~%dm", seconds/60)
}

func performanceCoverage(c core.PerformanceCoverage) string {
	return fmt.Sprintf("Coverage %d/%d; inferred %d", c.Measured, c.Candidates, c.Inferred)
}

func buildTilePerformanceLines(p *core.PerformanceSnapshot, now time.Time, width int) []string {
	if p == nil {
		return nil
	}
	label := "All response"
	if p.Summary.Coverage.Inferred > 0 {
		label = "All incl inferred"
	}
	lines := []string{
		fmt.Sprintf("%s %s tok/s", label, performanceRate(p.Summary.Stats.WeightedResponseTokS)),
		fmt.Sprintf("Direct %s · Inferred %s", performanceRate(p.Summary.Direct.WeightedResponseTokS), performanceRate(p.Summary.Inferred.WeightedResponseTokS)),
		performanceCoverage(p.Summary.Coverage) + " · " + performanceFreshness(p, now),
	}
	// Performance has its own explicit period; it must not silently inherit the
	// dashboard's quota/usage window pill.
	if !p.Period.Start.IsZero() && !p.Period.End.IsZero() {
		lines = append(lines, "Perf period "+p.Period.Start.Local().Format("01-02 15:04")+" → "+p.Period.End.Local().Format("01-02 15:04"))
	}
	for i := range lines {
		lines[i] = truncateToWidth(lines[i], width)
	}
	return lines
}

func buildDetailPerformanceLines(p *core.PerformanceSnapshot, now time.Time, width int) []string {
	if p == nil {
		return nil
	}
	lines := []string{"Output-only response tok/s; not decode speed"}
	stats := func(label string, value core.PerformanceStats) {
		lines = append(lines,
			fmt.Sprintf("%s weighted %s tok/s", label, performanceRate(value.WeightedResponseTokS)),
			fmt.Sprintf("  Calls %d", value.Count),
			fmt.Sprintf("  P50 %s · P90 %s (est.)", performanceRate(value.P50TokS), performanceRate(value.P90TokS)),
			fmt.Sprintf("  Min %s · max %s tok/s", performanceRate(value.MinTokS), performanceRate(value.MaxTokS)))
	}
	stats("All measured", p.Summary.Stats)
	if p.Summary.Coverage.Inferred > 0 {
		lines = append(lines, fmt.Sprintf("All includes inferred: %d calls", p.Summary.Coverage.Inferred))
	}
	stats("Direct", p.Summary.Direct)
	stats("Inferred", p.Summary.Inferred)
	c := p.Summary.Coverage
	lines = append(lines, performanceCoverage(c),
		fmt.Sprintf("Excluded %d · provisional %d", c.Excluded, c.Provisional),
		fmt.Sprintf("Failed %d · unknown %d", c.Failed, c.Unknown))
	keys := make([]string, 0, len(c.Reasons))
	for key := range c.Reasons {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("  %s: %d", performanceText(key), c.Reasons[key]))
	}
	keys = keys[:0]
	for key := range c.OutputBases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("Output basis %s: %d", performanceText(key), c.OutputBases[key]))
	}
	lines = append(lines,
		"Period "+performanceStamp(p.Period.Start),
		"    to "+performanceStamp(p.Period.End),
		"Source checked "+performanceStamp(p.DataAsOf),
		"Source update age "+performanceSourceAge(p, now),
		"Snapshot generated "+performanceStamp(p.GeneratedAt),
		"Source status: "+performanceFreshness(p, now))
	interval := "—"
	if p.ExportIntervalMS > 0 {
		interval = fmt.Sprintf("%.1fs", float64(p.ExportIntervalMS)/1000)
	}
	lines = append(lines, "Export interval "+interval)
	if p.Window != nil {
		window := p.Window.Latest60Seconds
		rate := window.WallTokS
		fresh := performanceFreshness(p, now)
		if p.Window.Stale || window.Stale || fresh != "checked" {
			rate = nil
		}
		coverage := "partial coverage"
		if window.CompleteCoverage {
			coverage = "complete coverage"
		}
		lines = append(lines,
			"Window: time-allocation estimate",
			"Wall 60s output "+performanceRate(rate)+" tok/s (includes idle)",
			fmt.Sprintf("  %s: %.1fs", coverage, window.CoveredSeconds),
			fmt.Sprintf("  Missing start %d", p.Window.MissingStart))
	}
	groups := append([]core.PerformanceGroup(nil), p.Groups...)
	priority := func(dimension string) int {
		switch dimension {
		case "source":
			return 0
		case "billing_provider":
			return 1
		case "model":
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return priority(groups[i].Dimension) < priority(groups[j].Dimension) })
	shown := 0
	for _, group := range groups {
		if shown == 8 {
			break
		}
		lines = append(lines, fmt.Sprintf("%s %s", performanceText(group.Dimension), performanceText(group.Key)),
			fmt.Sprintf("  Direct %s · inferred %s tok/s", performanceRate(group.Summary.Direct.WeightedResponseTokS), performanceRate(group.Summary.Inferred.WeightedResponseTokS)))
		shown++
	}
	if len(p.Groups) > shown || p.GroupsTruncated {
		lines = append(lines, "More groups in performance report")
	}
	for i := range lines {
		lines[i] = truncateToWidth(lines[i], width)
	}
	return lines
}

// Performance may refresh while the provider quota timestamp remains unchanged.
// Include only rendered metadata/aggregates, never the request list. Freshness
// has finitely many states, so an idle snapshot does not grow the tile cache on
// every clock tick.
func performanceRenderKey(p *core.PerformanceSnapshot, now time.Time) string {
	if p == nil {
		return ""
	}
	visible := *p
	visible.Requests = nil
	data, err := json.Marshal(visible)
	if err != nil {
		return "invalid:" + performanceFreshness(p, now)
	}
	return fmt.Sprintf("%x:%s", sha256.Sum256(data), performanceFreshness(p, now))
}
