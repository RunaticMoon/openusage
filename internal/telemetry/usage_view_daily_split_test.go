package telemetry

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestDailyTokenSplitPreservesCoverageAccountAndIncrementalWinners(t *testing.T) {
	globalUsageViewCache.reset()
	_, db, store := openUsageViewRawTestStore(t)
	ctx := context.Background()
	filter := usageFilter{ProviderIDs: []string{"codex"}, AccountID: "account-one"}
	namespace := usageViewCacheNamespace("daily-token-split")
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	event := func(day int, turn string, tokens core.TokenUsage) IngestRequest {
		return IngestRequest{SourceSystem: "codex", SourceChannel: SourceChannelJSONL,
			OccurredAt: base.AddDate(0, 0, day), ProviderID: "codex", AccountID: "account-one",
			AgentName: "codex", EventType: EventTypeMessageUsage, SessionID: "daily-split-session",
			TurnID: turn, ModelRaw: "gpt-5", TokenUsage: tokens}
	}
	first := event(0, "first", core.TokenUsage{InputTokens: int64Ptr(10), OutputTokens: int64Ptr(20),
		ReasoningTokens: int64Ptr(5), CacheReadTokens: int64Ptr(60), CacheWriteTokens: int64Ptr(20), TotalTokens: int64Ptr(115)})
	missing := core.TokenUsage{InputTokens: int64Ptr(10), OutputTokens: int64Ptr(5), TotalTokens: int64Ptr(15)}
	for _, row := range []IngestRequest{
		first,
		event(1, "zero", core.TokenUsage{TotalTokens: int64Ptr(0), CacheReadTokens: int64Ptr(0)}),
		event(2, "unknown", missing),
		event(3, "mixed-known", core.TokenUsage{TotalTokens: int64Ptr(80), CacheReadTokens: int64Ptr(30)}),
		event(3, "mixed-unknown", missing),
		event(4, "invalid", core.TokenUsage{TotalTokens: int64Ptr(10), CacheReadTokens: int64Ptr(30)}),
	} {
		mustIngestUsageEvent(t, store, row, "ingest daily split fixture")
	}
	sibling := first
	sibling.AccountID = "account-two"
	sibling.TurnID = "sibling"
	mustIngestUsageEvent(t, store, sibling, "ingest sibling account")
	failed := first
	failed.TurnID = "failed"
	failed.Status = EventStatusError
	mustIngestUsageEvent(t, store, failed, "ingest failed response")

	check := func(wantFirstWork, wantFirstCached float64) {
		t.Helper()
		agg, err := loadUsageViewForFilter(ctx, db, namespace, filter)
		if err != nil {
			t.Fatalf("load incremental daily view: %v", err)
		}
		daily, err := queryDailyTotals(ctx, db, filter)
		if err != nil {
			t.Fatalf("load SQL daily view: %v", err)
		}
		if !reflect.DeepEqual(agg.Daily, daily) {
			t.Fatalf("daily incremental/SQL mismatch: incremental=%+v SQL=%+v", agg.Daily, daily)
		}
		if len(daily) != 5 || daily[0].WorkTokens != wantFirstWork || daily[0].CachedTokens != wantFirstCached || daily[0].TokenSplitSamples != 1 {
			t.Fatalf("unexpected daily token split: %+v", daily)
		}
		if daily[1].TokenSplitSamples != 1 || daily[1].WorkTokens != 0 || daily[1].CachedTokens != 0 {
			t.Fatalf("measured zero was lost: %+v", daily[1])
		}
		if daily[2].TokenSplitSamples != 0 || daily[2].TokenSplitMissing != 1 || daily[4].TokenSplitSamples != 0 || daily[4].TokenSplitMissing != 1 {
			t.Fatalf("unknown/invalid split became zero: %+v", daily)
		}
		if daily[3].WorkTokens != 50 || daily[3].CachedTokens != 30 || daily[3].TokenSplitSamples != 1 || daily[3].TokenSplitMissing != 1 {
			t.Fatalf("mixed coverage was lost: %+v", daily[3])
		}
		snap := core.NewUsageSnapshot("codex", "account-one")
		// Old read-model entries must not survive a fresh projection.
		snap.DailySeries = map[string][]core.TimePoint{
			"analytics_work_tokens":         {{Date: "1999-01-01", Value: 999}},
			"analytics_cached_tokens":       {{Date: "1999-01-01", Value: 999}},
			"analytics_token_split_partial": {{Date: "1999-01-01", Value: 1}},
		}
		applyUsageViewToSnapshot(&snap, agg, core.TimeWindow30d)
		for key, want := range map[string][]core.TimePoint{
			"analytics_work_tokens":         {{Date: "2026-10-20", Value: wantFirstWork}, {Date: "2026-10-21", Value: 0}, {Date: "2026-10-23", Value: 50}},
			"analytics_cached_tokens":       {{Date: "2026-10-20", Value: wantFirstCached}, {Date: "2026-10-21", Value: 0}, {Date: "2026-10-23", Value: 30}},
			"analytics_token_split_partial": {{Date: "2026-10-23", Value: 1}},
		} {
			if !reflect.DeepEqual(snap.DailySeries[key], want) {
				t.Fatalf("%s = %+v, want %+v", key, snap.DailySeries[key], want)
			}
		}
	}
	check(35, 80)
	replacement := first
	replacement.SourceChannel = SourceChannelHook
	replacement.MessageID = "replacement-hook"
	replacement.TokenUsage = core.TokenUsage{InputTokens: int64Ptr(50), OutputTokens: int64Ptr(20), ReasoningTokens: int64Ptr(5),
		CacheReadTokens: int64Ptr(40), CacheWriteTokens: int64Ptr(10), TotalTokens: int64Ptr(125)}
	replaced, err := store.Ingest(ctx, replacement)
	if err != nil {
		t.Fatalf("ingest replacement: %v", err)
	}
	check(75, 50)
	if _, err := db.Exec(`DELETE FROM usage_events WHERE event_id = ?`, replaced.EventID); err != nil {
		t.Fatalf("delete replacement: %v", err)
	}
	check(35, 80)
}
