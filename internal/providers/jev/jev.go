// Package jev reads Paseo's accounting-only Jev ledger. It never invokes a
// model, opens credentials, or exports prompts and permission requests.
package jev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/providerbase"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

const ID = "jev"

type Provider struct{ providerbase.Base }

func New() *Provider {
	widget := providerbase.DefaultDashboard(providerbase.WithColorRole(core.DashboardColorRoleLavender))
	widget.CompactRows = []core.DashboardCompactRow{
		{Label: "Usage", Keys: []string{"requests", "total_tokens"}, MaxSegments: 2},
		{Label: "Estimate", Keys: []string{"window_cost"}, MaxSegments: 1},
	}
	return &Provider{Base: providerbase.New(core.ProviderSpec{
		ID:        ID,
		Info:      core.ProviderInfo{Name: "Jev", Capabilities: []string{"local_stats", "token_usage", "by_model"}, DocURL: "https://docs.typesafe.ai/models"},
		Auth:      core.ProviderAuthSpec{Type: core.ProviderAuthTypeLocal, DefaultAccountID: ID},
		Setup:     core.ProviderSetupSpec{Quickstart: []string{"Read Paseo's existing Jev usage metadata without credentials or model requests.", "Set provider_paths.usage_dir to the host's usage-limits/jev directory."}},
		Dashboard: widget,
	})}
}

func DefaultUsageDir() string {
	root := strings.TrimSpace(os.Getenv("PASEO_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		root = filepath.Join(home, ".paseo")
	}
	return filepath.Join(shared.ExpandHome(root), "usage-limits", "jev")
}

// Poll a complete metadata snapshot instead of appending accounting events to
// SQLite: a later CSV replacement can remove prior records without retaining
// stale costs in an incremental event store. The daemon exports this snapshot
// through its normal provider-poll/read-model/Hub path.
func (p *Provider) Fetch(ctx context.Context, acct core.AccountConfig) (core.UsageSnapshot, error) {
	return p.fetchAt(ctx, acct, time.Now())
}

func (p *Provider) fetchAt(ctx context.Context, acct core.AccountConfig, now time.Time) (core.UsageSnapshot, error) {
	snap := core.NewUsageSnapshot(ID, acct.ID)
	snap.Timestamp = now
	root, err := filepath.Abs(shared.ExpandHome(acct.Path("usage_dir", DefaultUsageDir())))
	if err != nil {
		return snap, fmt.Errorf("jev: invalid usage directory")
	}
	rows, err := readLedger(ctx, root)
	if err != nil {
		snap.Status, snap.Message = core.StatusError, "Unable to read complete Jev usage metadata"
		return snap, err
	}
	digest := sha256.Sum256([]byte("jev-records\x00" + root))
	snap.SetAttribute("history_path_identity", "jev-records:sha256:"+hex.EncodeToString(digest[:]))
	snap.SetAttribute("jev_usage_source", "paseo-ledger-v1")
	snap.SetAttribute("jev_usage_scope", "host-local")
	snap.SetAttribute("cost_basis", "provider-reference-estimate")
	snap.SetAttribute("cost_rate_verified", "2026-09-25")
	snap.SetAttribute("cost_rate_source", "https://docs.typesafe.ai/models")
	models, days, hours := map[string]*aggregate{}, map[string]*aggregate{}, map[string]*aggregate{}
	hourlySince := snap.Timestamp.UTC().Truncate(24*time.Hour).AddDate(0, 0, -29)
	total := newAggregate()
	var latest time.Time
	for _, row := range rows {
		if row.at.After(snap.Timestamp) {
			continue
		}
		day := row.at.UTC().Format("2006-01-02")
		hour := row.at.UTC().Truncate(time.Hour).Format(time.RFC3339)
		key := day + "\x00" + row.Model
		if models[key] == nil {
			models[key] = newAggregate()
		}
		if days[day] == nil {
			days[day] = newAggregate()
		}
		buckets := []*aggregate{models[key], days[day], total}
		if !row.at.Before(hourlySince) {
			if hours[hour] == nil {
				hours[hour] = newAggregate()
			}
			buckets = append(buckets, hours[hour])
		}
		for _, bucket := range buckets {
			if err := bucket.add(row); err != nil {
				return core.UsageSnapshot{}, err
			}
		}
		if row.at.After(latest) {
			latest = row.at
		}
	}
	for _, key := range sortedKeys(models) {
		parts := strings.SplitN(key, "\x00", 2)
		value := models[key]
		dimensions := map[string]string{"date": parts[0]}
		if value.unpriced > 0 {
			dimensions["cost_partial"] = "true"
		}
		snap.AppendModelUsage(core.ModelUsageRecord{
			RawModelID: parts[1], RawSource: "paseo-ledger-v1", Window: "1d",
			Dimensions:  dimensions,
			InputTokens: core.Float64Ptr(float64(value.input)), OutputTokens: core.Float64Ptr(float64(value.output)),
			TotalTokens: core.Float64Ptr(float64(value.input + value.output)), Requests: core.Float64Ptr(float64(value.requests)), CostUSD: value.cost,
		})
	}
	snap.DailySeries = aggregateSeries(days)
	snap.HourlySeries = aggregateSeries(hours)
	for key, n := range map[string]int64{"input_tokens": total.input, "output_tokens": total.output, "total_tokens": total.input + total.output, "requests": total.requests} {
		unit := "tokens"
		if key == "requests" {
			unit = "requests"
		}
		snap.Metrics[key] = core.Metric{Used: core.Float64Ptr(float64(n)), Unit: unit, Window: "all-time"}
	}
	if total.cost != nil {
		snap.Metrics["window_cost"] = core.Metric{Used: total.cost, Unit: "USD", Window: "all-time"}
	}
	if total.unpriced > 0 {
		snap.SetAttribute("cost_partial", "true")
		snap.SetDiagnostic("cost_coverage", "Some models have no verified reference rate; cost values include only known estimates")
	}
	if !latest.IsZero() {
		snap.SetAttribute("usage_last_event_at", latest.UTC().Format(time.RFC3339Nano))
	}
	snap.Status = core.StatusOK
	snap.Message = "Host-local Jev metadata; costs are reference estimates, not an account balance"
	return snap, nil
}

func aggregateSeries(buckets map[string]*aggregate) map[string][]core.TimePoint {
	series := make(map[string][]core.TimePoint)
	for _, date := range sortedKeys(buckets) {
		value := buckets[date]
		for key, n := range map[string]int64{"analytics_tokens": value.input + value.output, "analytics_requests": value.requests, "analytics_work_tokens": value.input + value.output} {
			series[key] = append(series[key], core.TimePoint{Date: date, Value: float64(n)})
		}
		if value.cost != nil {
			series["analytics_cost"] = append(series["analytics_cost"], core.TimePoint{Date: date, Value: *value.cost})
		}
		if value.unpriced > 0 {
			series["analytics_cost_partial"] = append(series["analytics_cost_partial"], core.TimePoint{Date: date, Value: 1})
		}
	}
	return series
}

type aggregate struct {
	input, output, requests int64
	cost                    *float64
	unpriced                int64
}

func newAggregate() *aggregate { return &aggregate{} }
func (a *aggregate) add(row ledgerEntry) error {
	if a.input > maxSafeInteger-*row.InputTokens || a.output > maxSafeInteger-*row.OutputTokens || a.requests > maxSafeInteger-row.requests() {
		return fmt.Errorf("jev: usage exceeds safe numeric range")
	}
	a.input += *row.InputTokens
	a.output += *row.OutputTokens
	a.requests += row.requests()
	if a.input > maxSafeInteger-a.output {
		return fmt.Errorf("jev: usage exceeds safe numeric range")
	}
	// Existing verified Paseo reference: jev-1.13.0 input $0.042/M, output $0.
	// Unknown versions remain unavailable. Preserve known subtotals and track
	// their incomplete coverage separately; this is never a measured invoice.
	if row.Model != "jev-1.13.0" {
		a.unpriced++
	} else {
		if a.cost == nil {
			a.cost = core.Float64Ptr(0)
		}
		*a.cost += float64(*row.InputTokens) * 0.042 / 1e6
	}
	return nil
}
func sortedKeys(values map[string]*aggregate) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
