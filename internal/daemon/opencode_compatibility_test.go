//go:build fork_integration

package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/config"
	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/exporter"
	"github.com/janekbaraniewski/openusage/internal/hub"
	"github.com/janekbaraniewski/openusage/internal/providers/opencode"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

// This crosses the public source/parser/store/read-model/exporter/Hub boundaries
// rather than constructing canonical requests with already-matching identities.
// Every database, HTTP listener and payload is synthetic and temporary.
func TestOpenCodeV2HookSQLiteReadModelHubCompatibility(t *testing.T) {
	for _, rawProvider := range []string{"opencode", "anthropic", "openai", "custom-vendor", "missing-hook-model"} {
		for _, hookFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/hook_first_%t", rawProvider, hookFirst), func(t *testing.T) {
				ctx := context.Background()
				hookProvider, dbProvider, hookModel := rawProvider, rawProvider, "synthetic-model"
				if rawProvider == "missing-hook-model" {
					hookProvider, dbProvider, hookModel = "", "anthropic", ""
				}
				sourcePath := filepath.Join(t.TempDir(), "source.db")
				db, err := sql.Open("sqlite3", sourcePath)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				for _, stmt := range []string{
					`CREATE TABLE session_v2(id TEXT PRIMARY KEY, directory TEXT)`,
					`CREATE TABLE session_message(id TEXT PRIMARY KEY, session_id TEXT, type TEXT, seq INTEGER, time_created INTEGER, time_updated INTEGER, data TEXT)`,
					`INSERT INTO session_v2 VALUES ('synthetic-session', '/synthetic/project')`,
				} {
					if _, err := db.Exec(stmt); err != nil {
						t.Fatal(err)
					}
				}
				now := time.Now().UTC()
				options := shared.TelemetryCollectOptions{
					Paths:     map[string]string{"db_path": sourcePath, "account_id": "synthetic-account"},
					PathLists: map[string][]string{"events_dirs": {t.TempDir()}},
				}
				collector := telemetry.NewSourceCollector(opencode.New(), options, "synthetic-account")
				storePath := filepath.Join(t.TempDir(), "telemetry.db")
				store, err := telemetry.OpenStore(storePath)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				hook, err := telemetry.ParseSourceHookPayload(opencode.New(), []byte(fmt.Sprintf(`{
				"event":{"type":"session.step.ended","created":%d,
				"location":{"directory":"/synthetic/project"},
				"data":{"sessionID":"synthetic-session","assistantMessageID":"synthetic-message",
				"model":{"providerID":%q,"id":%q},"agent":"opencode",
				"tokens":{"input":99,"output":0},"cost":0.99,
				"prompt":"SYNTHETIC_PRIVATE_BODY","native":"SYNTHETIC_PRIVATE_BODY"}}}`, now.UnixMilli(), hookProvider, hookModel)), options, "synthetic-account")
				if err != nil || len(hook) != 1 {
					t.Fatalf("hook count=%d err=%v", len(hook), err)
				}
				if hook[0].EventType != telemetry.EventTypeMessageUsage {
					t.Fatal("v2 hook was not parsed as message usage")
				}
				ingest := func(req telemetry.IngestRequest) telemetry.IngestResult {
					t.Helper()
					result, err := store.Ingest(ctx, req)
					if err != nil {
						t.Fatal(err)
					}
					return result
				}
				url, token := compatibilityHub(t)
				if hookFirst {
					ingest(hook[0])
					// Prime the cache before SQLite supplies authoritative usage
					// and possibly previously missing model/vendor attribution.
					primed, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, storePath,
						map[string]core.UsageSnapshot{"synthetic-account": core.NewUsageSnapshot("opencode", "synthetic-account")},
						telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll})
					if err != nil {
						t.Fatal(err)
					}
					if requests := primed["synthetic-account"].Metrics["window_requests"].Used; requests == nil || *requests != 1 {
						t.Fatal("initial hook did not prime one request")
					}
				}
				var originalID string
				var final telemetry.IngestRequest
				for revision, input := range []int{0, 30, 20} {
					data := fmt.Sprintf(`{"model":{"id":"synthetic-model","providerID":%q},"agent":"opencode",
					"tokens":{"input":%d,"output":0},"cost":%g,
					"time":{"created":%d,"completed":%d},"finish":"stop",
					"content":[{"type":"text","text":"SYNTHETIC_PRIVATE_BODY"}]}`, dbProvider, input, float64(input)/100, now.UnixMilli(), now.UnixMilli())
					_, err := db.Exec(`INSERT INTO session_message VALUES ('synthetic-message','synthetic-session','assistant',1,?,?,?)
					ON CONFLICT(id) DO UPDATE SET data=excluded.data,time_updated=excluded.time_updated`, now.UnixMilli(), now.UnixMilli()+int64(revision), data)
					if err != nil {
						t.Fatal(err)
					}
					requests, err := collector.Collect(ctx)
					if err != nil || len(requests) != 1 {
						t.Fatalf("revision %d: requests=%d err=%v", revision, len(requests), err)
					}
					final = requests[0]
					if final.SourceSchemaVersion != "opencode_sqlite_v2" || final.SourceChannel != telemetry.SourceChannelSQLite {
						t.Fatal("collector lost v2 SQLite provenance")
					}
					if telemetry.BuildDedupKey(final) != telemetry.BuildDedupKey(hook[0]) {
						t.Fatal("native hook and SQLite identities differ")
					}
					result := ingest(final)
					if revision == 0 {
						originalID = result.EventID
					} else if result.EventID != originalID {
						t.Fatal("revision replaced stable event ID")
					}
					// A stale hook must not roll back the authoritative SQLite value in
					// either ingestion order, even when it advertises a larger usage.
					if result := ingest(hook[0]); !result.Deduped {
						t.Fatal("late hook inserted duplicate")
					}
					var count, total int
					if err := store.DB().QueryRow(`SELECT COUNT(*),SUM(input_tokens) FROM usage_events WHERE event_type='message_usage'`).Scan(&count, &total); err != nil {
						t.Fatal(err)
					}
					if count != 1 || total != input {
						t.Fatalf("revision %d: canonical count=%d input=%d want 1/%d", revision, count, total, input)
					}
					snaps, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, storePath,
						map[string]core.UsageSnapshot{"synthetic-account": core.NewUsageSnapshot("opencode", "synthetic-account")},
						telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll})
					if err != nil {
						t.Fatal(err)
					}
					assertCompatibilityUsage(t, snaps["synthetic-account"], float64(input))
					pushCompatibilitySnapshot(t, url, token, snaps, float64(input))
				}
				// A fresh source instance represents restart/replay, not a new message.
				restarted := telemetry.NewSourceCollector(opencode.New(), options, "synthetic-account")
				requests, err := restarted.Collect(ctx)
				if err != nil || len(requests) != 1 {
					t.Fatalf("restart requests=%d err=%v", len(requests), err)
				}
				if result := ingest(requests[0]); !result.Deduped || result.EventID != originalID {
					t.Fatal("restart did not preserve stable identity")
				}
				if result := ingest(final); !result.Deduped {
					t.Fatal("identical replay inserted duplicate")
				}
			})
		}
	}
}

func assertCompatibilityUsage(t *testing.T, snap core.UsageSnapshot, input float64) {
	t.Helper()
	metric := snap.Metrics["model_synthetic_model_input_tokens"]
	if metric.Used == nil || *metric.Used != input {
		t.Fatalf("snapshot input=%v want %.0f", metric.Used, input)
	}
	metric = snap.Metrics["window_requests"]
	if metric.Used == nil || *metric.Used != 1 {
		t.Fatalf("snapshot requests=%v want 1", metric.Used)
	}
	if input > 0 {
		metric = snap.Metrics["window_cost"]
		if metric.Used == nil || *metric.Used != input/100 {
			t.Fatalf("snapshot cost=%v want %g", metric.Used, input/100)
		}
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "SYNTHETIC_PRIVATE_BODY") {
		t.Fatal("private source content reached snapshot")
	}
}

func compatibilityHub(t *testing.T) (string, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	const token = "synthetic-compatibility-token"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.NewServerWithAuth(addr, hub.NewStore(time.Minute), token).ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("hub shutdown: %v", err)
			}
		case <-time.After(6 * time.Second):
			t.Error("hub did not shut down")
		}
	})
	url := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(url + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated Hub did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	response, err := http.Get(url + "/v1/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("Hub accepted unauthenticated snapshot read")
	}
	return url, token
}

func pushCompatibilitySnapshot(t *testing.T, url, token string, snaps map[string]core.UsageSnapshot, input float64) {
	t.Helper()
	e, err := exporter.New(config.ExportConfig{Target: url, MachineName: "synthetic-machine", AuthToken: token, IntervalSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.Ingest(snaps)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Start(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/snapshots", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("Hub GET status=%d", response.StatusCode)
		}
		var received map[string]core.UsageSnapshot
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatal(err)
		}
		if snap, ok := received["synthetic-machine:opencode:synthetic-account"]; ok {
			if value := snap.Metrics["model_synthetic_model_input_tokens"].Used; value != nil && *value == input {
				if len(received) != 1 {
					t.Fatal("Hub retained duplicate snapshots")
				}
				assertCompatibilityUsage(t, snap, input)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Hub did not receive revision with input=%.0f", input)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
