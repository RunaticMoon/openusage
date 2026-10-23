package telemetry

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func jevAccountingSnapshot() core.UsageSnapshot {
	s := core.NewUsageSnapshot("jev", "local-jev")
	s.Timestamp = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s.Status = core.StatusOK
	s.SetAttribute("jev_usage_source", jevLedgerSource)
	s.Metrics["window_cost"] = core.Metric{Used: core.Float64Ptr(0), Unit: "USD", Window: "all-time"}
	s.ModelUsage = []core.ModelUsageRecord{{RawModelID: "jev-1.13.0", RawSource: jevLedgerSource, Window: "1d",
		Dimensions: map[string]string{"date": "2026-10-19"}, InputTokens: core.Float64Ptr(0),
		OutputTokens: core.Float64Ptr(2), TotalTokens: core.Float64Ptr(2), Requests: core.Float64Ptr(1), CostUSD: core.Float64Ptr(0)}}
	s.DailySeries = map[string][]core.TimePoint{"analytics_tokens": {{Date: "2026-10-19", Value: 2}}, "analytics_cost": {{Date: "2026-10-19", Value: 0}}}
	s.HourlySeries = map[string][]core.TimePoint{"analytics_tokens": {{Date: "2026-10-19T23:00:00Z", Value: 2}}, "analytics_cost": {{Date: "2026-10-19T23:00:00Z", Value: 0}}}
	return s
}

func snapshotPayload(t *testing.T, s core.UsageSnapshot) string {
	t.Helper()
	reqs := BuildLimitSnapshotRequests(map[string]core.UsageSnapshot{s.AccountID: s})
	data, err := json.Marshal(reqs[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestJevSnapshotPersistenceAllowlistAndScope(t *testing.T) {
	want := jevAccountingSnapshot()
	input := want.DeepClone()
	input.Raw = map[string]string{"private": "private-canary"}
	input.ModelUsage[0].Reason = "private-canary"
	input.ModelUsage[0].Canonical = "private-canary"
	input.ModelUsage[0].RawSource = "private-canary"
	input.ModelUsage[0].Dimensions["prompt"] = "private-canary"
	input.ModelUsage[0].Window = "private-canary"
	input.DailySeries["private-canary"] = []core.TimePoint{{Date: "bad", Value: 1}}
	input.HourlySeries["private-canary"] = []core.TimePoint{{Date: "bad", Value: 1}}
	payload := snapshotPayload(t, input)
	if strings.Contains(payload, "private-canary") {
		t.Fatal("non-accounting metadata persisted")
	}
	got, ok := decodeStoredLimitSnapshot("jev", want.AccountID, payload, want.Timestamp.Format(time.RFC3339))
	if !ok || !reflect.DeepEqual(got.ModelUsage, want.ModelUsage) || !reflect.DeepEqual(got.DailySeries, want.DailySeries) || !reflect.DeepEqual(got.HourlySeries, want.HourlySeries) {
		t.Fatal("accounting values or known zero changed")
	}
	for _, tc := range []struct{ provider, marker string }{{"codex", jevLedgerSource}, {"jev", ""}, {"jev", "other"}, {"Jev", jevLedgerSource}} {
		s := input.DeepClone()
		s.ProviderID, s.Attributes["jev_usage_source"] = tc.provider, tc.marker
		p := snapshotPayload(t, s)
		if strings.Contains(p, "jev_ledger_usage") {
			t.Fatalf("extension persisted outside exact scope: %q/%q", tc.provider, tc.marker)
		}
		root, ok := decodeStoredLimitSnapshot(tc.provider, s.AccountID, p, s.Timestamp.Format(time.RFC3339))
		if !ok || len(root.ModelUsage) != 0 || len(root.DailySeries) != 0 || root.Metrics["window_cost"].Used == nil {
			t.Fatal("legacy quota behavior changed")
		}
	}
	for _, ids := range [][2]string{{"jev", "another-account"}, {"codex", want.AccountID}} {
		if _, ok := decodeStoredLimitSnapshot(ids[0], ids[1], payload, "2026-10-20T00:00:00Z"); ok {
			t.Fatal("mismatched accounting identity restored")
		}
	}
	missingID := strings.Replace(payload, `"provider_id":"jev"`, `"provider_id":""`, 1)
	if _, ok := decodeStoredLimitSnapshot("jev", want.AccountID, missingID, "2026-10-20T00:00:00Z"); ok {
		t.Fatal("missing payload provider accepted")
	}
	missingAccount := strings.Replace(payload, `"account_id":"local-jev"`, `"account_id":""`, 1)
	if _, ok := decodeStoredLimitSnapshot("jev", want.AccountID, missingAccount, "2026-10-20T00:00:00Z"); ok {
		t.Fatal("missing payload account accepted")
	}
}

func TestJevSnapshotRejectsInvalidOrOversizedAccounting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*core.UsageSnapshot)
	}{
		{"nan", func(s *core.UsageSnapshot) { s.ModelUsage[0].CostUSD = core.Float64Ptr(math.NaN()) }},
		{"negative", func(s *core.UsageSnapshot) { s.DailySeries["analytics_tokens"][0].Value = -1 }},
		{"invalid date", func(s *core.UsageSnapshot) { s.ModelUsage[0].Dimensions["date"] = "2026-02-30" }},
		{"invalid hour", func(s *core.UsageSnapshot) { s.HourlySeries["analytics_tokens"][0].Date = "2026-10-19T23:01:00Z" }},
		{"non-UTC hour", func(s *core.UsageSnapshot) { s.HourlySeries["analytics_tokens"][0].Date = "2026-10-20T08:00:00+09:00" }},
		{"negative hourly value", func(s *core.UsageSnapshot) { s.HourlySeries["analytics_tokens"][0].Value = -1 }},
		{"hourly point limit", func(s *core.UsageSnapshot) {
			s.HourlySeries["analytics_tokens"] = make([]core.TimePoint, maxJevHourlyPoints+1)
		}},
		{"row limit", func(s *core.UsageSnapshot) { s.ModelUsage = make([]core.ModelUsageRecord, maxJevModelRows+1) }},
		{"point limit", func(s *core.UsageSnapshot) {
			s.DailySeries["analytics_tokens"] = make([]core.TimePoint, maxJevDailyPoints+1)
		}},
		{"byte limit", func(s *core.UsageSnapshot) {
			row := s.ModelUsage[0]
			row.RawModelID = strings.Repeat("m", 1024)
			s.ModelUsage = make([]core.ModelUsageRecord, 1024)
			for i := range s.ModelUsage {
				s.ModelUsage[i] = row
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := jevAccountingSnapshot()
			s.Metrics["provider_jev_cost_usd"] = core.Metric{Used: core.Float64Ptr(1)}
			tc.change(&s)
			payload := snapshotPayload(t, s)
			got, ok := decodeStoredLimitSnapshot("jev", s.AccountID, payload, s.Timestamp.Format(time.RFC3339))
			if !ok || got.Status != core.StatusError || got.Diagnostics["jev_usage_snapshot"] == "" || len(got.ModelUsage) != 0 || len(got.DailySeries) != 0 || len(got.HourlySeries) != 0 || len(got.Metrics) != 0 {
				t.Fatal("invalid accounting was represented as complete")
			}
			if s.Status != core.StatusOK || s.Metrics["window_cost"].Used == nil {
				t.Fatal("ingest mutated caller snapshot")
			}
		})
	}
	for _, data := range []string{"", `{}`, `{"version":2}`, strings.Repeat(" ", maxJevUsageBytes+1)} {
		s := jevAccountingSnapshot()
		restoreJevUsage(&s, json.RawMessage(data))
		if s.Status != core.StatusError || len(s.ModelUsage) != 0 || len(s.DailySeries) != 0 || len(s.HourlySeries) != 0 || s.Metrics["window_cost"].Used != nil {
			t.Fatal("invalid stored payload retained prior usage")
		}
	}
}

func TestJevLegacySnapshotHasNoInventedHourlyHistory(t *testing.T) {
	s := jevAccountingSnapshot()
	legacy, err := sanitizeJevUsage(s.ModelUsage, s.DailySeries, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hourly_series") {
		t.Fatal("legacy payload fixture unexpectedly has hourly data")
	}
	restoreJevUsage(&s, data)
	if s.Status != core.StatusOK || len(s.DailySeries["analytics_tokens"]) != 1 || len(s.HourlySeries) != 0 {
		t.Fatal("legacy accounting failed to load or retained an unrelated hourly template")
	}
}

func TestJevSnapshotLatestEmptyReplacementSurvivesRetention(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	initial := jevAccountingSnapshot()
	store.now = func() time.Time { return initial.Timestamp }
	ingest := NewQuotaSnapshotIngestor(store)
	if err := ingest.Ingest(ctx, map[string]core.UsageSnapshot{initial.AccountID: initial}); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return initial.Timestamp.Add(time.Hour) }
	if _, err := store.PruneRawEventPayloads(ctx, 0, 1000); err != nil {
		t.Fatal(err)
	}
	root, err := loadLatestLimitSnapshot(ctx, store.db, "jev", initial.AccountID)
	if err != nil || root == nil || !reflect.DeepEqual(root.ModelUsage, initial.ModelUsage) {
		t.Fatal("latest accounting snapshot lost to retention")
	}
	empty := initial.DeepClone()
	empty.Timestamp = empty.Timestamp.Add(time.Second)
	empty.ModelUsage, empty.DailySeries = nil, map[string][]core.TimePoint{}
	empty.HourlySeries = map[string][]core.TimePoint{}
	if err := ingest.Ingest(ctx, map[string]core.UsageSnapshot{empty.AccountID: empty}); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return initial.Timestamp.Add(2 * time.Hour) }
	if n, err := store.PruneRawEventPayloads(ctx, 0, 1000); err != nil || n != 1 {
		t.Fatalf("superseded payload retention: n=%d err=%v", n, err)
	}
	root, err = loadLatestLimitSnapshot(ctx, store.db, "jev", empty.AccountID)
	if err != nil || root == nil {
		t.Fatal("empty replacement unavailable")
	}
	got := mergeLimitSnapshotRoot(initial, *root)
	if got.Status != core.StatusOK || len(got.ModelUsage) != 0 || len(got.DailySeries) != 0 || len(got.HourlySeries) != 0 || !got.Timestamp.Equal(empty.Timestamp) {
		t.Fatal("older accounting survived complete replacement")
	}
	// The generic quota path must continue to preserve another provider's
	// live usage template while hydrating its limits.
	quota := core.NewUsageSnapshot("codex", "other")
	quota.Metrics["quota"] = core.Metric{Limit: core.Float64Ptr(100), Remaining: core.Float64Ptr(79)}
	base := initial.DeepClone()
	base.ProviderID = "codex"
	merged := mergeLimitSnapshotRoot(base, quota)
	if !reflect.DeepEqual(merged.ModelUsage, base.ModelUsage) || *merged.Metrics["quota"].Remaining != 79 {
		t.Fatal("non-Jev usage or quota changed")
	}
}
