package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/pricing"
)

func TestCodexCostViewPreservesDatesCoverageAndNativeCosts(t *testing.T) {
	globalUsageViewCache.reset()
	dbPath, db, store := openUsageViewRawTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	lookup := codexCostPriceLookup
	codexCostPriceLookup = func(_ context.Context, model string, contextLen int) (*pricing.Price, error) {
		if model != "gpt-6-astra" || contextLen != 100 {
			t.Fatalf("unexpected price lookup model=%q context=%d", model, contextLen)
		}
		return &pricing.Price{ModelID: model, InputCostPerMillion: 2, OutputCostPerMillion: 10, CacheReadCostPerMillion: 0.2}, nil
	}
	t.Cleanup(func() { codexCostPriceLookup = lookup })
	event := func(day int, turn, model string, cost *float64) IngestRequest {
		return IngestRequest{SourceSystem: "codex", SourceChannel: SourceChannelJSONL,
			OccurredAt: base.AddDate(0, 0, day), ProviderID: "codex", AccountID: "one", AgentName: "codex",
			EventType: EventTypeMessageUsage, SessionID: "cost-session", TurnID: turn, ModelRaw: model,
			TokenUsage: core.TokenUsage{InputTokens: int64Ptr(100), OutputTokens: int64Ptr(20),
				CacheReadTokens: int64Ptr(80), ReasoningTokens: int64Ptr(12), TotalTokens: int64Ptr(120), CostUSD: cost}}
	}
	for _, row := range []IngestRequest{
		event(0, "estimated", "gpt-6-astra", nil),
		event(0, "unknown", "codex-auto-review", nil),
		event(1, "native-zero", "codex-auto-review", float64Ptr(0)),
		event(2, "unpriced-day", "codex-auto-review", nil),
	} {
		mustIngestUsageEvent(t, store, row, "ingest cost fixture")
	}
	sibling := event(0, "sibling", "gpt-6-astra", float64Ptr(99))
	sibling.AccountID = "two"
	mustIngestUsageEvent(t, store, sibling, "ingest sibling cost")
	failed := event(0, "failed", "gpt-6-astra", float64Ptr(99))
	failed.Status = EventStatusError
	mustIngestUsageEvent(t, store, failed, "ingest failed cost")
	filter := usageFilter{ProviderIDs: []string{"codex"}, AccountID: "one", TodaySince: base.AddDate(0, 0, 1).Truncate(24 * time.Hour)}
	check := func(want float64, missing, estimated int) {
		t.Helper()
		for _, namespace := range []usageViewCacheNamespace{"", "codex-cost-test"} {
			agg, err := loadUsageViewForFilter(ctx, db, namespace, filter)
			if err != nil {
				t.Fatal(err)
			}
			snap := core.NewUsageSnapshot("codex", "one")
			snap.Metrics["window_cost"] = core.Metric{Used: float64Ptr(900), Unit: "USD", Window: "today"}
			applyUsageViewToSnapshot(&snap, agg, core.TimeWindow30d)
			if got := snap.Metrics["window_cost"]; got.Used == nil || math.Abs(*got.Used-want) > 1e-12 || got.Window != "30d" {
				t.Fatalf("window cost = %+v, want %v in 30d", got, want)
			}
			if got := snap.Metrics["today_api_cost"]; got.Used == nil || *got.Used != 0 || got.Window != "1d" {
				t.Fatalf("recorded today zero lost: %+v", got)
			}
			if snap.Diagnostics["cost_missing_samples"] != fmt.Sprint(missing) || snap.Diagnostics["cost_estimated_samples"] != fmt.Sprint(estimated) {
				t.Fatalf("coverage = %+v", snap.Diagnostics)
			}
			if snap.Attributes["cost_partial"] != "true" || (estimated > 0 && snap.Attributes["cost_basis"] != "api-price-estimate") {
				t.Fatalf("cost attributes = %+v", snap.Attributes)
			}
			points := snap.DailySeries["analytics_cost"]
			if len(points) != 2 || points[0].Date != "2026-10-20" || points[0].Value != want || points[1].Value != 0 {
				t.Fatalf("daily costs = %+v", points)
			}
			if len(snap.DailySeries["analytics_cost_partial"]) != 2 {
				t.Fatalf("missing daily coverage: %+v", snap.DailySeries)
			}
			for _, row := range snap.ModelUsage {
				if row.RawModelID == "codex-auto-review" && (row.CostUSD == nil || *row.CostUSD != 0 || row.Dimensions["cost_partial"] != "true") {
					t.Fatalf("known zero plus unknown model subtotal = %+v", row)
				}
			}
		}
	}
	// 20 uncached input + 80 cached + 20 output; reasoning is already output.
	check(0.000256, 2, 1)
	exported, err := ApplyCanonicalTelemetryViewWithOptions(ctx, dbPath, map[string]core.UsageSnapshot{
		"one": core.NewUsageSnapshot("codex", "one"),
	}, ReadModelOptions{TodaySince: filter.TodaySince, TimeWindow: core.TimeWindow30d})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(exported["one"])
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip core.UsageSnapshot
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Attributes["cost_basis"] != "api-price-estimate" || len(roundTrip.DailySeries["analytics_cost"]) != 2 || roundTrip.Diagnostics["cost_missing_samples"] != "2" {
		t.Fatalf("read model/JSON lost cost provenance: %+v", roundTrip)
	}
	var missingRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE provider_id = 'codex' AND cost_usd IS NULL`).Scan(&missingRows); err != nil || missingRows != 3 {
		t.Fatalf("native NULL costs were modified: count=%d err=%v", missingRows, err)
	}
	// A higher-priority native observation replaces the estimate exactly once.
	replacement := event(0, "estimated", "gpt-6-astra", float64Ptr(0.5))
	replacement.SourceChannel = SourceChannelHook
	mustIngestUsageEvent(t, store, replacement, "ingest actual replacement")
	check(0.5, 2, 0)
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE provider_id = 'codex' AND cost_usd IS NULL`).Scan(&missingRows); err != nil || missingRows != 2 {
		t.Fatalf("native NULL costs were modified: count=%d err=%v", missingRows, err)
	}
	// A narrower window must neither borrow the wider subtotal nor invent zero.
	filter.Since = base.AddDate(0, 0, 2).Truncate(24 * time.Hour)
	agg, err := loadUsageViewForFilter(ctx, db, "codex-cost-short", filter)
	if err != nil {
		t.Fatal(err)
	}
	snap := core.NewUsageSnapshot("codex", "one")
	snap.Metrics["window_cost"] = core.Metric{Used: float64Ptr(900), Unit: "USD", Window: "today"}
	applyUsageViewToSnapshot(&snap, agg, core.TimeWindow7d)
	if _, ok := snap.Metrics["window_cost"]; ok || len(snap.DailySeries["analytics_cost"]) != 0 || snap.ModelUsage[0].CostUSD != nil {
		t.Fatalf("unknown-only window became a cost: %+v", snap)
	}
}

func TestCodexCostEstimateRequiresExactModelAndCompleteCompatibleUsage(t *testing.T) {
	n := func(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
	lookup := func(_ context.Context, model string, _ int) (*pricing.Price, error) {
		return &pricing.Price{ModelID: model, InputCostPerMillion: 2, OutputCostPerMillion: 10, CacheReadCostPerMillion: 0.2,
			Tiers: pricing.TierOverrides{Above200k: &pricing.TierRates{InputCostPerMillion: float64Ptr(4)}}}, nil
	}
	got := estimateCodexViewCost(context.Background(), lookup, "gpt-6-astra", n(300000), n(20), n(200000), n(0), n(300020))
	if got == nil || math.Abs(*got-0.4402) > 1e-10 {
		t.Fatalf("per-request context tier = %v, want 0.4402", got)
	}
	for _, tc := range []struct {
		name, model                  string
		input, output, cached, total sql.NullFloat64
	}{
		{"auto review", "codex-auto-review", n(100), n(20), n(80), n(120)},
		{"missing cache", "gpt-6-astra", n(100), n(20), sql.NullFloat64{}, n(120)},
		{"cache exceeds input", "gpt-6-astra", n(100), n(20), n(101), n(120)},
		{"incompatible total", "gpt-6-astra", n(100), n(20), n(80), n(200)},
		{"negative input", "gpt-6-astra", n(-1), n(20), n(0), n(19)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimateCodexViewCost(context.Background(), lookup, tc.model, tc.input, tc.output, tc.cached, n(0), tc.total); got != nil {
				t.Fatalf("invalid usage estimated: %v", *got)
			}
		})
	}
	wrongModel := func(context.Context, string, int) (*pricing.Price, error) {
		return &pricing.Price{ModelID: "different-model", InputCostPerMillion: 1, OutputCostPerMillion: 2, CacheReadCostPerMillion: 0.1}, nil
	}
	if got := estimateCodexViewCost(context.Background(), wrongModel, "gpt-6-astra", n(100), n(20), n(80), n(0), n(120)); got != nil {
		t.Fatal("fuzzy match to another model was accepted")
	}
}
