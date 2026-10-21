package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/config"
	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

func TestPollSnapshotsForIngestExcludesUnchangedCachedResults(t *testing.T) {
	results := []pollProviderResult{
		{
			accountID:    "cached",
			snapshot:     core.UsageSnapshot{ProviderID: "codex", AccountID: "cached"},
			shouldIngest: false,
		},
		{
			accountID:    "changed",
			snapshot:     core.UsageSnapshot{ProviderID: "openai", AccountID: "changed"},
			shouldIngest: true,
		},
	}

	got := pollSnapshotsForIngest(results)
	if _, ok := got["cached"]; ok {
		t.Fatal("unchanged cached snapshot should not be re-ingested")
	}
	if _, ok := got["changed"]; !ok {
		t.Fatal("changed snapshot should be ingested")
	}
}

type observationProvider struct {
	core.UsageProvider
	snapshot core.UsageSnapshot
	calls    int
}

func (p *observationProvider) Spec() core.ProviderSpec { return core.ProviderSpec{} }
func (p *observationProvider) Fetch(context.Context, core.AccountConfig) (core.UsageSnapshot, error) {
	p.calls++
	return p.snapshot.DeepClone(), nil
}

func TestPollPersistsUnchangedFetchObservationWithoutDefeatingBackoff(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	cfg := config.DefaultConfig()
	cfg.AutoDetect = false
	cfg.Accounts = []core.AccountConfig{{ID: "personal", Provider: "codex", Auth: "local"}}
	cfg.AutoDetectedAccounts = nil
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "telemetry.db")
	store, err := telemetry.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	clock := &healthClock{time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	provider := &observationProvider{snapshot: core.UsageSnapshot{
		ProviderID: "codex", AccountID: "personal", Status: core.StatusOK, Timestamp: clock.at,
		Raw:     map[string]string{"rate_limit_source": "live"},
		Metrics: map[string]core.Metric{"rate_limit_primary": {Used: core.Float64Ptr(20), Limit: core.Float64Ptr(100), Unit: "%", Window: "5h"}},
	}}
	svc := &Service{clock: clock, store: store, quotaIngest: telemetry.NewQuotaSnapshotIngestor(store),
		providerByID: map[string]core.UsageProvider{"codex": provider}, pollScheduler: newPollScheduler(time.Minute),
		pollState: make(map[string]*providerPollState), logThrottle: core.NewLogThrottle(20, time.Minute)}
	countSnapshots := func() int {
		t.Helper()
		var count int
		if err := store.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE event_type='limit_snapshot'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	readSnapshot := func() core.UsageSnapshot {
		t.Helper()
		base := map[string]core.UsageSnapshot{"personal": {ProviderID: "codex", AccountID: "personal", Metrics: make(map[string]core.Metric)}}
		snaps, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(context.Background(), dbPath, base,
			telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll})
		if err != nil {
			t.Fatal(err)
		}
		return snaps["personal"]
	}
	allowPoll := func() {
		svc.pollScheduler.mu.Lock()
		svc.pollScheduler.states["personal"].lastPollAt = time.Now().Add(-time.Hour)
		svc.pollScheduler.mu.Unlock()
	}
	for i := 0; i < 4; i++ {
		if i > 0 {
			clock.at = clock.at.Add(time.Minute)
			provider.snapshot.Timestamp = clock.at
			allowPoll()
		}
		svc.pollProviders(context.Background())
		got := readSnapshot()
		if provider.calls != i+1 || countSnapshots() != i+1 || !got.Timestamp.Equal(clock.at) {
			t.Fatalf("unchanged actual observation lost: calls=%d rows=%d timestamp=%s want=%s", provider.calls, countSnapshots(), got.Timestamp, clock.at)
		}
	}
	state := svc.pollScheduler.states["personal"]
	if state.consecutiveNoChange != 3 || svc.pollScheduler.effectiveIntervalLocked(state) != 2*time.Minute {
		t.Fatal("persisting observations reset adaptive backoff")
	}
	version := svc.dataVersion.Load()
	svc.pollProviders(context.Background())
	if provider.calls != 4 || countSnapshots() != 4 || svc.dataVersion.Load() != version {
		t.Fatal("scheduler cache skip created another observation")
	}
	// A provider fallback carrying the original observation retains its event
	// identity, even though Fetch ran again. It must not add a quota event.
	allowPoll()
	svc.pollProviders(context.Background())
	if provider.calls != 5 || countSnapshots() != 4 || !readSnapshot().Timestamp.Equal(clock.at) {
		t.Fatal("same observation timestamp was duplicated or made fresh")
	}
	// An actual failed fetch is persisted without reviving the prior quota.
	clock.at = clock.at.Add(time.Minute)
	provider.snapshot.Timestamp = clock.at
	provider.snapshot.Status = core.StatusAuth
	provider.snapshot.Metrics = map[string]core.Metric{}
	provider.snapshot.Raw = map[string]string{}
	allowPoll()
	svc.pollProviders(context.Background())
	failed := readSnapshot()
	if failed.Status != core.StatusAuth || !failed.Timestamp.Equal(clock.at) || failed.Metrics["rate_limit_primary"].Used != nil {
		t.Fatalf("failed observation revived old quota: %+v", failed)
	}
}

func TestUnchangedObservationRequiresAuditedFreshProvenance(t *testing.T) {
	for _, tc := range []struct {
		provider string
		status   core.Status
		raw      map[string]string
		want     bool
	}{
		{"claude_code", core.StatusOK, map[string]string{"usage_api_ok": "true"}, true},
		{"claude_code", core.StatusOK, map[string]string{"usage_api_cached": "true"}, false},
		{"codex", core.StatusOK, map[string]string{"rate_limit_source": "live"}, true},
		{"codex", core.StatusOK, map[string]string{"rate_limit_source": "cli_rpc"}, true},
		{"codex", core.StatusOK, map[string]string{"rate_limit_source": "session"}, false},
		{"cursor", core.StatusOK, nil, false},
		{"copilot", core.StatusOK, nil, false},
		{"opencode", core.StatusOK, nil, true},
		{"dsh", core.StatusOK, nil, true},
		{"opencode", core.StatusAuth, nil, false},
		{"claude_code", core.StatusError, map[string]string{"usage_api_ok": "true"}, false},
	} {
		got := canRefreshUnchangedObservation(core.UsageSnapshot{ProviderID: tc.provider, Status: tc.status, Raw: tc.raw})
		if got != tc.want {
			t.Errorf("provider=%s status=%s raw=%v: got=%t want=%t", tc.provider, tc.status, tc.raw, got, tc.want)
		}
	}
}
