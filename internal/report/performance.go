package report

import (
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func performanceNumber(v *float64) string {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return "—"
	}
	return fmt.Sprintf("%.3f", *v)
}
func safePerformanceLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// WritePerformanceTable appends a distinct output-only section. Existing token,
// cost and quota columns and their arithmetic remain untouched.
func WritePerformanceTable(w io.Writer, p *core.PerformanceSnapshot) error {
	if p == nil {
		_, err := fmt.Fprintln(w, "Response — · request timing unavailable")
		return err
	}
	c := p.Summary.Coverage
	s := p.Summary.Stats
	if _, err := fmt.Fprintf(w, "\nOutput response %s tok/s · P50 %s / P90 %s (histogram estimates)\nTiming %d/%d requests · direct %d · log inferred %d · failed %d · unknown %d · provisional %d\nValid output %d / duration %.3f ms\nDefinition %s · period %s → %s\nSource checked %s · snapshot %s · export every %.1fs · stale %t\n",
		performanceNumber(s.WeightedResponseTokS), performanceNumber(s.P50TokS), performanceNumber(s.P90TokS), c.Measured, c.Candidates, c.Direct, c.Inferred, c.Failed, c.Unknown, c.Provisional, s.OutputTokens, s.DurationMS, p.DefinitionVersion, p.Period.Start.Format("2006-01-02T15:04:05Z07:00"), p.Period.End.Format("2006-01-02T15:04:05Z07:00"), p.DataAsOf.Format("2006-01-02T15:04:05Z07:00"), p.GeneratedAt.Format("2006-01-02T15:04:05Z07:00"), float64(p.ExportIntervalMS)/1000, p.Stale); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DIMENSION\tVALUE\tRESPONSE tok/s\tP50\tP90\tTIMED\tINFERRED")
	for _, g := range p.Groups {
		if g.Dimension == "host" {
			continue
		}
		v := g.Summary
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d/%d\t%d\n", safePerformanceLabel(g.Dimension), safePerformanceLabel(g.Key), performanceNumber(v.Stats.WeightedResponseTokS), performanceNumber(v.Stats.P50TokS), performanceNumber(v.Stats.P90TokS), v.Coverage.Measured, v.Coverage.Candidates, v.Coverage.Inferred)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if p.GroupsTruncated {
		fmt.Fprintln(w, "Group details truncated; query with --session/--model/--account for an exact subset.")
	}
	if p.Window != nil {
		r := p.Window.Latest60Seconds
		rate := r.WallTokS
		if p.Window.Stale || r.Stale || p.Stale {
			rate = nil
		}
		fmt.Fprintf(w, "Recent 60s output %s tok/s · time-allocation estimate · coverage %.1f/60s · complete %t\n", performanceNumber(rate), r.CoveredSeconds, r.CompleteCoverage)
		for _, session := range p.Window.Session120Seconds {
			rate := session.Rate.WallTokS
			if p.Window.Stale || session.Rate.Stale || p.Stale {
				rate = nil
			}
			fmt.Fprintf(w, "Session %s recent 120s output %s tok/s · time-allocation estimate · complete %t\n", safePerformanceLabel(session.SessionID), performanceNumber(rate), session.Rate.CompleteCoverage)
		}
	}
	fmt.Fprintln(w, "Response elapsed time may include first-token waiting; this is not decode speed or TTFT.")
	return nil
}
