package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/jev"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

// Exercise the actual daemon request handler from blank templates after
// persistence. No RunServer, config/auth discovery, service, socket or exporter
// target is started. All ledger and SQLite inputs are synthetic temporary files.
func TestJevLedgerDaemonPersistenceAndRemoteEnvelope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	recentDates := strings.NewReplacer(
		"2020-01-01", today.AddDate(0, 0, -4).Format("2006-01-02"),
		"2020-01-02", today.AddDate(0, 0, -3).Format("2006-01-02"),
		"2020-01-03", today.AddDate(0, 0, -2).Format("2006-01-02"),
		"2020-01-04", today.AddDate(0, 0, -1).Format("2006-01-02"),
	)
	initialPath := filepath.Join(dir, "2020-01-01.jsonl")
	initial := `{"version":1,"id":"known","timestamp":"2020-01-01T01:00:00Z","model":"jev-1.13.0","inputTokens":1000000,"outputTokens":5,"prompt":"private-canary"}` + "\n" +
		`{"version":1,"id":"mixed","timestamp":"2020-01-01T02:00:00Z","model":"jev-future","inputTokens":7,"outputTokens":2}` + "\n" +
		`{"version":1,"id":"unknown","timestamp":"2020-01-02T01:00:00Z","model":"jev-future","inputTokens":11,"outputTokens":2}` + "\n" +
		`{"version":1,"id":"zero","timestamp":"2020-01-03T01:00:00Z","model":"jev-1.13.0","inputTokens":0,"outputTokens":2}` + "\n"
	initial = recentDates.Replace(initial)
	if err := os.WriteFile(initialPath, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	acct := core.AccountConfig{ID: "isolated-jev", Provider: "jev", Auth: "local"}
	acct.SetPath("usage_dir", dir)
	dbPath := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := telemetry.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	scheduler := newPollScheduler(time.Minute)
	at := time.Now().UTC().Add(-time.Minute)
	fetchAndPersist := func(wantChanged bool) core.UsageSnapshot {
		t.Helper()
		snap, err := jev.New().Fetch(ctx, acct)
		if err != nil {
			t.Fatal(err)
		}
		snap.Timestamp = at
		at = at.Add(time.Second)
		native := snap.DeepClone()
		// Provider polling normalizes before change detection and persistence.
		// Include that boundary: it must not drop known-zero ledger days.
		snap = core.NormalizeUsageSnapshotWithConfig(snap, core.DefaultModelNormalizationConfig())
		changed := scheduler.SnapshotChanged(acct.ID, snap)
		if changed != wantChanged {
			t.Fatalf("ledger change = %t, want %t", changed, wantChanged)
		}
		scheduler.RecordPoll(acct.ID, changed)
		observations := pollSnapshotsForIngest([]pollProviderResult{{accountID: acct.ID, snapshot: snap,
			shouldIngest: changed || canRefreshUnchangedObservation(snap)}})
		svc := &Service{store: store, quotaIngest: telemetry.NewQuotaSnapshotIngestor(store)}
		if err := svc.ingestQuotaSnapshots(ctx, observations); err != nil {
			t.Fatal(err)
		}
		return native
	}
	readRemote := func() core.UsageSnapshot {
		t.Helper()
		// Recreate the service/cache for each read: no fetched snapshot is held
		// in memory by the reader, as after a daemon restart.
		svc := &Service{cfg: Config{DBPath: dbPath}, rmCache: newReadModelCache(),
			logThrottle: core.NewLogThrottle(20, time.Minute), readModelDisabledAccounts: func() map[string]bool { return nil }}
		req := ReadModelRequest{Accounts: []ReadModelAccount{{AccountID: acct.ID, ProviderID: "jev"}}, TimeWindow: core.TimeWindowAll}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		svc.handleReadModel(response, httptest.NewRequest(http.MethodPost, "/v1/read-model", bytes.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("read model status %d", response.Code)
		}
		var result ReadModelResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Snapshots) != 1 {
			t.Fatal("read model returned unexpected accounts")
		}
		wire, err := json.Marshal(core.RemoteEnvelope{Machine: "isolated-fixture", SentAt: at, Snapshots: []core.UsageSnapshot{result.Snapshots[acct.ID]}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wire), "private-canary") || strings.Contains(string(wire), dir) {
			t.Fatal("private ledger content or path exported")
		}
		var envelope core.RemoteEnvelope
		if err := json.Unmarshal(wire, &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Snapshots[0]
	}
	assertAccounting := func(want, got core.UsageSnapshot) {
		t.Helper()
		if !reflect.DeepEqual(got.ModelUsage, want.ModelUsage) || !reflect.DeepEqual(got.DailySeries, want.DailySeries) || !reflect.DeepEqual(got.HourlySeries, want.HourlySeries) || got.Status != core.StatusOK || !got.Timestamp.Equal(want.Timestamp) {
			t.Fatal("daemon persistence/read-model/RemoteEnvelope lost accounting or observation timestamp")
		}
		for key, metric := range want.Metrics {
			if !reflect.DeepEqual(got.Metrics[key], metric) {
				t.Fatalf("accounting metric changed: %s", key)
			}
		}
		for _, key := range []string{"history_path_identity", "cost_partial", "jev_usage_source", "cost_basis"} {
			if got.Attributes[key] != want.Attributes[key] {
				t.Fatalf("source/coverage attribute changed: %s", key)
			}
		}
	}
	first := fetchAndPersist(true)
	if len(first.HourlySeries["analytics_tokens"]) != 4 {
		t.Fatal("daemon fixture must exercise four observed hourly buckets")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = telemetry.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	assertAccounting(first, readRemote())
	// Both changes retain every total and the known cost subtotal. Only the
	// date/model distribution changes, so the legacy metrics-only hash misses it.
	replacementPath := filepath.Join(dir, "2020-01-04.jsonl")
	replacement := `{"version":1,"id":"csv-known","timestamp":"2020-01-04T01:00:00Z","model":"jev-1.13.0","inputTokens":1000000,"outputTokens":5,"source":"typesafe-console-csv","supersedes":["known"]}` + "\n" +
		`{"version":1,"id":"csv-mixed","timestamp":"2020-01-04T02:00:00Z","model":"jev-future-v2","inputTokens":7,"outputTokens":2,"source":"typesafe-console-csv","supersedes":["mixed"]}` + "\n"
	replacement = recentDates.Replace(replacement)
	if err := os.WriteFile(replacementPath, []byte(replacement), 0600); err != nil {
		t.Fatal(err)
	}
	corrected := fetchAndPersist(true)
	if !reflect.DeepEqual(corrected.Metrics, first.Metrics) {
		t.Fatal("fixture did not retain equal totals")
	}
	assertAccounting(corrected, readRemote())
	fetchAndPersist(false) // Timestamp-only updates retain existing change/backoff behavior.
	for _, path := range []string{initialPath, replacementPath} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	empty := fetchAndPersist(true)
	got := readRemote()
	// omitempty makes an empty daily map nil after JSON; both mean no points.
	if got.Status != core.StatusOK || len(got.ModelUsage) != 0 || len(got.DailySeries) != 0 || len(got.HourlySeries) != 0 || !got.Timestamp.Equal(empty.Timestamp) || got.Metrics["window_cost"].Used != nil || *got.Metrics["requests"].Used != 0 {
		t.Fatal("deleted ledger data survived the latest snapshot")
	}
	var limitRows, usageRows int
	if err := store.DB().QueryRow(`SELECT count(*) FROM usage_events WHERE event_type='limit_snapshot'`).Scan(&limitRows); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM usage_events WHERE event_type!='limit_snapshot'`).Scan(&usageRows); err != nil {
		t.Fatal(err)
	}
	if limitRows != 3 || usageRows != 0 {
		t.Fatalf("unexpected event history: snapshots=%d usage=%d", limitRows, usageRows)
	}
}

func TestJevLedgerHashScope(t *testing.T) {
	for _, tc := range []struct {
		provider, marker string
		changed          bool
	}{
		{"jev", "paseo-ledger-v1", true}, {"jev", "", false}, {"codex", "paseo-ledger-v1", false},
	} {
		first := core.NewUsageSnapshot(tc.provider, "fixture")
		first.SetAttribute("jev_usage_source", tc.marker)
		second := first.DeepClone()
		second.DailySeries = make(map[string][]core.TimePoint)
		second.DailySeries["analytics_tokens"] = []core.TimePoint{{Date: "2020-01-01", Value: 1}}
		if got := hashSnapshotMetrics(first) != hashSnapshotMetrics(second); got != tc.changed {
			t.Fatalf("hash scope %s/%s changed=%t", tc.provider, tc.marker, got)
		}
		second = first.DeepClone()
		second.HourlySeries = map[string][]core.TimePoint{"analytics_tokens": {{Date: "2020-01-01T01:00:00Z", Value: 1}}}
		if got := hashSnapshotMetrics(first) != hashSnapshotMetrics(second); got != tc.changed {
			t.Fatalf("hourly hash scope %s/%s changed=%t", tc.provider, tc.marker, got)
		}
	}
}
