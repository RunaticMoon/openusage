package telemetry

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/pricing"
)

type codexPriceLookup func(context.Context, string, int) (*pricing.Price, error)

var codexCostPriceLookup codexPriceLookup = func(ctx context.Context, model string, contextLen int) (*pricing.Price, error) {
	return pricing.DefaultResolver().Lookup(ctx, model, contextLen)
}

type codexCostAmount struct {
	USD                       float64
	Known, Missing, Estimated int
}

func (a *codexCostAmount) add(cost *float64, estimated bool) {
	if cost == nil {
		a.Missing++
		return
	}
	a.USD += *cost
	a.Known++
	if estimated {
		a.Estimated++
	}
}

type codexCostView struct {
	Total, Today codexCostAmount
	Days         map[string]*codexCostAmount
	Models       map[string]*codexCostAmount
}

func isCodexCostFilter(filter usageFilter) bool {
	if filter.SourceSystem != "" {
		return strings.EqualFold(strings.TrimSpace(filter.SourceSystem), "codex")
	}
	providers := normalizeProviderIDs(filter.ProviderIDs)
	return len(providers) == 1 && providers[0] == "codex"
}

// queryCodexCosts prices the same logical winners and account/window as the
// native usage view. Original NULL/zero values remain distinguishable here.
func queryCodexCosts(ctx context.Context, db *sql.DB, filter usageFilter, lookup codexPriceLookup) (*codexCostView, error) {
	cte, args := dedupedUsageCTE(filter)
	query := cte + fmt.Sprintf(`SELECT date(occurred_at),
		COALESCE(NULLIF(TRIM(COALESCE(model_canonical, model_raw)), ''), 'unknown'),
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, total_tokens, cost_usd, source_system,
		CASE WHEN %s THEN 1 ELSE 0 END
		FROM deduped_usage WHERE event_type = 'message_usage' AND status != 'error'`, filter.todayExpr("occurred_at"))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("codex cost view: %w", err)
	}
	defer rows.Close()
	view := &codexCostView{Days: make(map[string]*codexCostAmount), Models: make(map[string]*codexCostAmount)}
	priceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for rows.Next() {
		var day, model, source string
		var input, output, cached, writes, total, observed sql.NullFloat64
		var today bool
		if err := rows.Scan(&day, &model, &input, &output, &cached, &writes, &total, &observed, &source, &today); err != nil {
			return nil, fmt.Errorf("scan codex cost view: %w", err)
		}
		var cost *float64
		estimated := false
		if observed.Valid && validCostNumber(observed.Float64) {
			cost = core.Float64Ptr(observed.Float64)
		} else if !observed.Valid && strings.EqualFold(strings.TrimSpace(source), "codex") {
			cost = estimateCodexViewCost(priceCtx, lookup, model, input, output, cached, writes, total)
			estimated = cost != nil
		}
		if view.Days[day] == nil {
			view.Days[day] = &codexCostAmount{}
		}
		if view.Models[model] == nil {
			view.Models[model] = &codexCostAmount{}
		}
		view.Total.add(cost, estimated)
		view.Days[day].add(cost, estimated)
		view.Models[model].add(cost, estimated)
		if today {
			view.Today.add(cost, estimated)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate codex cost view: %w", err)
	}
	return view, nil
}

func validCostNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func estimateCodexViewCost(ctx context.Context, lookup codexPriceLookup, model string, input, output, cached, writes, total sql.NullFloat64) *float64 {
	model = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "openai/")
	if model == "" || model == "unknown" || model == "codex-auto-review" || lookup == nil {
		return nil
	}
	// Codex cached input is a subset of input, and reasoning is a subset of
	// output. Require complete compatible counters instead of guessing either.
	for _, n := range []sql.NullFloat64{input, output, cached} {
		if !n.Valid || !validCostNumber(n.Float64) || math.Trunc(n.Float64) != n.Float64 || n.Float64 > float64(math.MaxInt32) {
			return nil
		}
	}
	if cached.Float64 > input.Float64 || (writes.Valid && writes.Float64 != 0) ||
		(total.Valid && total.Float64 != input.Float64+output.Float64) {
		return nil
	}
	p, err := lookup(ctx, model, int(input.Float64))
	if err != nil || p == nil || strings.TrimPrefix(strings.ToLower(p.ModelID), "openai/") != model {
		return nil // A fuzzy match to a different model is not cost evidence.
	}
	pTier := pricing.ApplyTier(*p, int(input.Float64))
	for _, rate := range []float64{pTier.InputCostPerMillion, pTier.OutputCostPerMillion, pTier.CacheReadCostPerMillion} {
		if !validCostNumber(rate) {
			return nil
		}
	}
	if (input.Float64 > cached.Float64 && pTier.InputCostPerMillion == 0) ||
		(output.Float64 > 0 && pTier.OutputCostPerMillion == 0) || (cached.Float64 > 0 && pTier.CacheReadCostPerMillion == 0) {
		return nil // Optional unpublished rates are zero in the resolver schema.
	}
	amount := pricing.Estimate(&pTier, int(input.Float64), pricing.Usage{
		InputTokens: int(input.Float64 - cached.Float64), OutputTokens: int(output.Float64), CacheReadTokens: int(cached.Float64),
	})
	if !validCostNumber(amount) {
		return nil
	}
	return core.Float64Ptr(amount)
}

func applyCodexCostView(snap *core.UsageSnapshot, view *codexCostView, window core.TimeWindow) {
	if view == nil || !strings.EqualFold(snap.ProviderID, "codex") {
		return
	}
	// Remove stale aliases before exporting the matched-window subtotal. In
	// particular today's provider estimate must not become a 7/30-day total.
	for key, metric := range snap.Metrics {
		if metric.Unit == "USD" && (strings.HasPrefix(key, "model_") || key == "window_cost" || key == "today_api_cost" || key == "today_cost" || key == "total_cost_usd" || key == "all_time_api_cost" || key == "7d_api_cost" || key == "30d_api_cost") {
			delete(snap.Metrics, key)
		}
	}
	for _, key := range []string{"analytics_cost", "cost", "cost_usd", "analytics_cost_partial"} {
		delete(snap.DailySeries, key)
	}
	for _, key := range []string{"cost_basis", "cost_partial"} {
		delete(snap.Attributes, key)
	}
	if view.Total.Known > 0 {
		snap.Metrics["window_cost"] = core.Metric{Used: core.Float64Ptr(view.Total.USD), Unit: "USD", Window: string(window)}
	}
	if view.Today.Known > 0 {
		snap.Metrics["today_api_cost"] = core.Metric{Used: core.Float64Ptr(view.Today.USD), Unit: "USD", Window: "1d"}
	}
	if view.Total.Estimated > 0 {
		snap.SetAttribute("cost_basis", "api-price-estimate")
	}
	if view.Total.Missing > 0 {
		snap.SetAttribute("cost_partial", "true")
	}
	snap.SetDiagnostic("cost_known_samples", strconv.Itoa(view.Total.Known))
	snap.SetDiagnostic("cost_missing_samples", strconv.Itoa(view.Total.Missing))
	snap.SetDiagnostic("cost_estimated_samples", strconv.Itoa(view.Total.Estimated))
	for i := range snap.ModelUsage {
		row := &snap.ModelUsage[i]
		amount := view.Models[row.RawModelID]
		row.CostUSD = nil
		if amount == nil {
			continue
		}
		if amount.Known > 0 {
			row.CostUSD = core.Float64Ptr(amount.USD)
			snap.Metrics["model_"+sanitizeMetricID(row.RawModelID)+"_cost_usd"] = core.Metric{Used: row.CostUSD, Unit: "USD", Window: string(window)}
		}
		if amount.Missing > 0 {
			if row.Dimensions == nil {
				row.Dimensions = make(map[string]string)
			}
			row.Dimensions["cost_partial"] = "true"
		}
	}
	days := make([]string, 0, len(view.Days))
	for day := range view.Days {
		days = append(days, day)
	}
	sort.Strings(days)
	for _, day := range days {
		amount := view.Days[day]
		if amount.Known > 0 {
			snap.DailySeries["analytics_cost"] = append(snap.DailySeries["analytics_cost"], core.TimePoint{Date: day, Value: amount.USD})
		}
		if amount.Missing > 0 {
			snap.DailySeries["analytics_cost_partial"] = append(snap.DailySeries["analytics_cost_partial"], core.TimePoint{Date: day, Value: 1})
		}
	}
}
