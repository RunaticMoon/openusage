package telemetry

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestHourlyUsagePreservesUTCBucketsCoverageAndIncrementalWinners(t *testing.T) {
	globalUsageViewCache.reset()
	_, db, store := openUsageViewRawTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC)
	fixture := func(id string, at time.Time, total, cached int64, cost *float64) IngestRequest {
		return IngestRequest{SourceSystem: "codex", SourceChannel: SourceChannelJSONL,
			OccurredAt: at, ProviderID: "codex", AccountID: "personal", AgentName: "codex",
			EventType: EventTypeMessageUsage, SessionID: "hourly", TurnID: id, ModelRaw: "test-model",
			TokenUsage: core.TokenUsage{TotalTokens: int64Ptr(total), CacheReadTokens: int64Ptr(cached), CostUSD: cost}}
	}
	first := fixture("first", base.In(time.FixedZone("KST", 9*3600)), 100, 20, float64Ptr(0.2))
	missing := fixture("missing", base.Add(10*time.Minute), 50, 0, nil)
	missing.CacheReadTokens = nil
	zero := fixture("zero", base.Add(time.Hour), 0, 0, float64Ptr(0))
	unknown := fixture("unknown", base.Add(2*time.Hour), 15, 0, nil)
	for _, row := range []IngestRequest{first, missing, zero, unknown} {
		mustIngestUsageEvent(t, store, row, "hourly fixture")
	}
	sibling := fixture("sibling", base, 999, 0, float64Ptr(99))
	sibling.AccountID = "other"
	mustIngestUsageEvent(t, store, sibling, "sibling")
	failed := fixture("failed", base, 999, 0, float64Ptr(99))
	failed.Status = EventStatusError
	mustIngestUsageEvent(t, store, failed, "failed")
	foreign := fixture("foreign", base, 999, 0, float64Ptr(99))
	foreign.SourceSystem = "opencode"
	mustIngestUsageEvent(t, store, foreign, "other source")
	filter := usageFilter{SourceSystem: "codex", AccountID: "personal", Since: base.Add(-time.Hour), WindowKey: core.TimeWindow30d}
	namespace := usageViewCacheNamespace("hourly-winners")
	check := func(wantTokens, wantCost float64) *telemetryUsageAgg {
		t.Helper()
		agg, err := loadUsageViewForFilter(ctx, db, namespace, filter)
		if err != nil {
			t.Fatal(err)
		}
		sqlAgg, err := loadUsageViewForFilter(ctx, db, "", filter)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(agg.Hourly, sqlAgg.Hourly) || !reflect.DeepEqual(agg.Daily, sqlAgg.Daily) {
			t.Fatalf("incremental/SQL differ: hourly=%+v SQL=%+v daily=%+v SQL=%+v", agg.Hourly, sqlAgg.Hourly, agg.Daily, sqlAgg.Daily)
		}
		if len(agg.Hourly) != 3 || agg.Hourly[0].Day != "2026-10-20T23:00:00Z" || agg.Hourly[0].Tokens != wantTokens || agg.Hourly[0].CostUSD != wantCost {
			t.Fatalf("hour buckets = %+v", agg.Hourly)
		}
		byDay := map[string]float64{}
		for _, hour := range agg.Hourly {
			byDay[hour.Day[:10]] += hour.Tokens
		}
		for _, day := range agg.Daily {
			if byDay[day.Day] != day.Tokens {
				t.Fatalf("daily/hour sum mismatch: %+v", agg)
			}
		}
		snap := core.NewUsageSnapshot("codex", "personal")
		snap.HourlySeries = map[string][]core.TimePoint{"stale": {{Date: "1999-01-01T00:00:00Z", Value: 999}}}
		applyUsageViewToSnapshotAt(&snap, agg, core.TimeWindow30d, base.Add(3*time.Hour))
		if _, exists := snap.HourlySeries["stale"]; exists {
			t.Fatal("stale hourly data retained")
		}
		if got := snap.HourlySeries["analytics_cost"]; len(got) != 2 || got[0].Value != wantCost || got[1].Date != "2026-10-21T00:00:00Z" || got[1].Value != 0 {
			t.Fatalf("known zero/unknown cost conflated: %+v", got)
		}
		if got := snap.HourlySeries["analytics_cost_partial"]; len(got) != 2 || got[0].Date != "2026-10-20T23:00:00Z" || got[1].Date != "2026-10-21T01:00:00Z" {
			t.Fatalf("cost coverage=%+v", got)
		}
		if got := snap.HourlySeries["analytics_token_split_partial"]; len(got) != 1 || got[0].Date != "2026-10-20T23:00:00Z" {
			t.Fatalf("token coverage=%+v", got)
		}
		return agg
	}
	check(150, 0.2)
	check(150, 0.2) // Warm cache returns the same hourly observations.
	replacement := first
	replacement.SourceChannel = SourceChannelHook
	replacement.MessageID = "hook-first"
	replacement.TotalTokens, replacement.CostUSD = int64Ptr(200), float64Ptr(0.4)
	inserted, err := store.Ingest(ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	check(250, 0.4)
	if _, err := db.Exec(`DELETE FROM usage_events WHERE event_id = ?`, inserted.EventID); err != nil {
		t.Fatal(err)
	}
	check(150, 0.2)
	filter.Since = base.Add(30 * time.Minute)
	narrow, err := loadUsageViewForFilter(ctx, db, namespace, filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow.Hourly) != 2 || narrow.Hourly[0].Day != "2026-10-21T00:00:00Z" || narrow.Hourly[0].Tokens != 0 {
		t.Fatalf("moving window retained old hour or lost zero: %+v", narrow.Hourly)
	}
}

func TestHourlyUsageFollowsAuthoritativeSourceRevisions(t *testing.T) {
	for _, tc := range []struct {
		source  string
		request func(int64, int64) IngestRequest
	}{
		{"opencode", opencodeRevisionRequest}, {"dsh", dshRevisionRequest},
	} {
		t.Run(tc.source, func(t *testing.T) {
			globalUsageViewCache.reset()
			_, db, store := openUsageViewRawTestStore(t)
			filter := usageFilter{SourceSystem: tc.source, AccountID: "personal-test"}
			for i, input := range []int64{0, 30, 20} {
				req := tc.request(input, int64(i+1))
				mustIngestUsageEvent(t, store, req, "hourly revision")
				for _, namespace := range []usageViewCacheNamespace{"hourly-revisions", ""} {
					agg, err := loadUsageViewForFilter(context.Background(), db, namespace, filter)
					if err != nil {
						t.Fatal(err)
					}
					if len(agg.Hourly) != 1 || agg.Hourly[0].Tokens != float64(input+40) || agg.Hourly[0].Requests != 1 {
						t.Fatalf("revision %d inflated/staled hourly: %+v", i, agg.Hourly)
					}
				}
			}
		})
	}
}

func TestUsageEventHourDoesNotInventTimestamp(t *testing.T) {
	for input, want := range map[string]string{
		"2026-10-21T08:30:12.123456789+09:00": "2026-10-20T23:00:00Z",
		"2026-10-20T23:59:59Z":                "2026-10-20T23:00:00Z",
		"2026-10-20":                          "", "": "", "invalid": "",
	} {
		if got := usageEventHour(input); got != want {
			t.Fatalf("%q => %q, want %q", input, got, want)
		}
	}
}

func TestHourlyProjectionIsBoundedWithoutChangingDailyOrInventingCosts(t *testing.T) {
	now := time.Date(2026, 10, 24, 23, 59, 0, 0, time.UTC)
	first := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	agg := newTelemetryUsageAgg()
	agg.Daily = []telemetryDayPoint{{Day: "2020-01-01", Tokens: 1234}}
	for at := first.Add(-time.Hour); !at.After(now.Add(time.Hour)); at = at.Add(time.Hour) {
		agg.Hourly = append(agg.Hourly, telemetryDayPoint{Day: at.Format(time.RFC3339), Tokens: 1, Requests: 1, TokenSplitMissing: 1, CostMissing: 1})
	}
	snap := core.NewUsageSnapshot("claude_code", "personal")
	applyUsageViewToSnapshotAt(&snap, agg, core.TimeWindowAll, now)
	if got := snap.HourlySeries["analytics_tokens"]; len(got) != 720 || got[0].Date != "2026-09-25T00:00:00Z" || got[719].Date != "2026-10-24T23:00:00Z" {
		t.Fatalf("hourly boundary/count = %d %+v", len(got), got)
	}
	if len(snap.HourlySeries["analytics_cost"]) != 0 || len(snap.HourlySeries["analytics_cost_partial"]) != 720 || len(snap.HourlySeries["analytics_work_tokens"]) != 0 || len(snap.HourlySeries["analytics_token_split_partial"]) != 720 {
		t.Fatal("unknown hourly observations became measured zero or lost coverage")
	}
	if got := snap.DailySeries["analytics_tokens"]; len(got) != 1 || got[0].Date != "2020-01-01" || got[0].Value != 1234 {
		t.Fatalf("hourly cap changed legacy daily view: %+v", got)
	}
	applyUsageViewToSnapshotAt(&snap, newTelemetryUsageAgg(), core.TimeWindowAll, now)
	if len(snap.HourlySeries) != 0 {
		t.Fatal("empty window retained hourly observations")
	}
}
