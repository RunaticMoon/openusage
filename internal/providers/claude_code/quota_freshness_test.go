package claude_code

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/config"
	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/exporter"
	"github.com/janekbaraniewski/openusage/internal/hub"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

func TestHasChangedPollsRemoteQuotaWithUnchangedLocalFiles(t *testing.T) {
	for _, source := range []string{"oauth", "desktop", "previous-success", "none"} {
		t.Run(source, func(t *testing.T) {
			home := setTempHome(t)
			p := New()
			path := ""
			switch source {
			case "oauth":
				path = filepath.Join(home, ".claude", ".credentials.json")
			case "desktop":
				path = filepath.Join(home, "Library", "Application Support", "Claude", "Cookies")
			case "previous-success":
				p.setCachedUsage(&usageResponse{}, time.Now().Add(-time.Hour))
			}
			if path != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("synthetic credential presence only"), 0o600); err != nil {
					t.Fatal(err)
				}
				past := time.Now().Add(-time.Hour)
				if err := os.Chtimes(path, past, past); err != nil {
					t.Fatal(err)
				}
			}
			got, err := p.HasChanged(core.AccountConfig{}, time.Now())
			want := source == "oauth" || source == "previous-success" || source == "desktop" && runtime.GOOS == "darwin"
			if err != nil || got != want {
				t.Fatalf("HasChanged=%t, %v; want %t", got, err, want)
			}
		})
	}
}

func TestCachedQuotaAuthFailurePreservesObservationThroughReadModelAndExporter(t *testing.T) {
	writeCredentials(t, `{"claudeAiOauth":{"accessToken":"synthetic-token"}}`)
	root, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	resetAt := observedAt.Add(time.Hour) // already passed when the later retry fails
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"five_hour": map[string]any{"utilization": 37, "resets_at": resetAt.Format(time.RFC3339)}})
	}))
	defer api.Close()
	oldURL := oauthUsageURL
	oauthUsageURL = api.URL
	t.Cleanup(func() { oauthUsageURL = oldURL })
	p := New()
	// Avoid the platform cookie/Keychain lookup; the synthetic OAuth source is
	// the only credential this isolated regression is allowed to resolve.
	p.setLastUsageAuthSource("oauth")
	first := core.NewUsageSnapshot("claude_code", "synthetic-account")
	first.Timestamp = observedAt
	if err := p.readUsageAPI(context.Background(), "synthetic-org", &first); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".claude", ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	fallback := core.NewUsageSnapshot("claude_code", "synthetic-account")
	fallback.Metrics["local_counter"] = core.Metric{Used: core.Float64Ptr(99), Unit: "tokens"}
	if err := p.readUsageAPI(context.Background(), "synthetic-org", &fallback); err != nil {
		t.Fatal(err)
	}
	if !fallback.Timestamp.Equal(observedAt) || fallback.Raw["usage_api_cached"] != "true" || *fallback.Metrics["usage_five_hour"].Used != 37 {
		t.Fatal("cached quota became current or invented a reset after authentication failure")
	}
	if *fallback.Metrics["local_counter"].Used != 99 {
		t.Fatal("preserving the quota timestamp discarded local telemetry")
	}
	dbPath := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := telemetry.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ingest := telemetry.NewQuotaSnapshotIngestor(store)
	for _, snap := range []core.UsageSnapshot{first, fallback, fallback} {
		if err := ingest.Ingest(context.Background(), map[string]core.UsageSnapshot{snap.AccountID: snap}); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE event_type='limit_snapshot'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("cached retry duplicated quota events: rows=%d err=%v", rows, err)
	}
	base := core.NewUsageSnapshot("claude_code", "synthetic-account")
	snaps, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(context.Background(), dbPath,
		map[string]core.UsageSnapshot{base.AccountID: base}, telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll})
	if err != nil {
		t.Fatal(err)
	}
	if !snaps[base.AccountID].Timestamp.Equal(observedAt) {
		t.Fatal("read model refreshed cached quota age")
	}
	latest := hub.NewStore(time.Minute)
	received := make(chan struct{}, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope core.RemoteEnvelope
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		latest.Ingest(envelope)
		received <- struct{}{}
	}))
	defer sink.Close()
	exp, err := exporter.New(config.ExportConfig{Target: sink.URL, MachineName: "synthetic-machine", AuthToken: "synthetic-hub-token"})
	if err != nil {
		t.Fatal(err)
	}
	exp.Ingest(snaps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); exp.Start(ctx) }()
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("isolated exporter did not deliver")
	}
	cancel()
	<-done
	got := latest.Snapshots()["synthetic-machine:claude_code:synthetic-account"]
	if !got.Timestamp.Equal(observedAt) || *got.Metrics["usage_five_hour"].Used != 37 {
		t.Fatal("exporter/Hub changed the original quota observation")
	}
}
