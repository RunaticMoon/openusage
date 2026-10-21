package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/janekbaraniewski/openusage/internal/config"
	"github.com/janekbaraniewski/openusage/internal/report"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

// This command is a read-only view of the existing local SQLite store. It
// cannot enable collection, create a migration or invoke a provider/model.
func newPerformanceCommand() *cobra.Command {
	var db, since, until string
	var asJSON, windows bool
	var q telemetry.PerformanceQuery
	cmd := &cobra.Command{Use: "performance", Short: "Query opt-in log-based output response speed (read-only)", RunE: func(cmd *cobra.Command, _ []string) error {
		now := time.Now().UTC()
		q.Now = now
		q.Since = now.Add(-24 * time.Hour)
		q.Until = now
		var err error
		if since != "" {
			q.Since, err = parsePerformanceBound(since)
			if err != nil {
				return fmt.Errorf("--since: %w", err)
			}
		}
		if until != "" {
			q.Until, err = parsePerformanceBound(until)
			if err != nil {
				return fmt.Errorf("--until: %w", err)
			}
		}
		if q.BufferMinutes != 30 && q.BufferMinutes != 60 {
			return fmt.Errorf("--buffer-minutes must be 30 or 60")
		}
		if q.RequestLimit < 1 || q.RequestLimit > 1000 {
			return fmt.Errorf("--request-limit must be between 1 and 1000")
		}
		if strings.TrimSpace(db) == "" {
			db, err = telemetry.DefaultDBPath()
			if err != nil {
				return err
			}
		}
		q.IncludeWindow = windows
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		result, err := telemetry.QueryPerformance(ctx, db, q)
		if err != nil {
			return err
		}
		if asJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(result)
		}
		return report.WritePerformanceTable(cmd.OutOrStdout(), result)
	}}
	f := cmd.Flags()
	f.StringVar(&db, "db-path", "", "existing local telemetry SQLite path (read-only)")
	f.BoolVar(&asJSON, "json", false, "include request details, mergeable histograms and UTC buckets as JSON")
	f.StringVar(&q.Source, "source", "", "execution tool (claude_code, codex, opencode)")
	f.StringVar(&q.Account, "account", "", "exact configured account/profile ID")
	f.StringVar(&q.Model, "model", "", "exact logged model ID")
	f.StringVar(&q.Session, "session", "", "exact source session ID")
	f.StringVar(&q.BillingProvider, "billing-provider", "", "model billing provider; distinct from execution tool")
	f.StringVar(&since, "since", "", "inclusive RFC3339 or local YYYY-MM-DD bound (default last 24h)")
	f.StringVar(&until, "until", "", "exclusive RFC3339 or local YYYY-MM-DD bound (default now)")
	f.IntVar(&q.RequestLimit, "request-limit", 100, "returned request detail limit, 1–1000; does not limit statistics")
	f.IntVar(&q.BufferMinutes, "buffer-minutes", 30, "bounded 10-second bucket history: 30 or 60 minutes")
	f.BoolVar(&windows, "windows", true, "include time-allocation estimates for recent 60s and per-session 120s")
	return cmd
}
func parsePerformanceBound(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	return time.ParseInLocation("2006-01-02", s, time.Local)
}

// Existing reports append the same local performance projection only when
// opted in. They do not rescan source logs to construct a second timing stream.
func attachReportPerformance(rep *report.Report, opts report.Options) {
	cfg, err := config.Load()
	if err != nil || !cfg.Telemetry.Performance.Enabled {
		return
	}
	note := func(s string) {
		if rep.Note != "" {
			rep.Note += "; "
		}
		rep.Note += s
	}
	if opts.Project != "" {
		note("performance unavailable for project filter: request project attribution is not stored")
		return
	}
	path, err := telemetry.DefaultDBPath()
	if err != nil {
		note("performance database unavailable")
		return
	}
	since := opts.Since
	days := cfg.Data.RetentionDays
	if days <= 0 {
		days = 30
	}
	retained := opts.Now.AddDate(0, 0, -days)
	if since.IsZero() || since.Before(retained) {
		since = retained
	}
	until := opts.Until
	if until.IsZero() {
		until = opts.Now
	} else {
		until = until.Add(time.Nanosecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := telemetry.QueryPerformance(ctx, path, telemetry.PerformanceQuery{Source: opts.Provider, Since: since, Until: until, Now: opts.Now, BufferMinutes: cfg.Telemetry.Performance.WindowMinutes(), ExportIntervalMS: int64(cfg.Export.IntervalSeconds) * 1000, IncludeWindow: true})
	if err != nil {
		note("performance unavailable in local metadata store")
		return
	}
	rep.Performance = p
	note("performance uses retained local request metadata; its explicit period/coverage may differ from older token history")
}
