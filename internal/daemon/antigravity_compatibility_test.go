//go:build fork_integration

package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/hub"
	"github.com/janekbaraniewski/openusage/internal/providers/antigravity"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

func TestAntigravityNativeHistoryDoesNotCountStatusTransitions(t *testing.T) {
	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := telemetry.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accounts := []core.AccountConfig{}
	for _, account := range []string{"synthetic-profile-one", "synthetic-profile-two"} {
		root := t.TempDir()
		logs := filepath.Join(root, "11111111-1111-4111-8111-111111111111", ".system_generated", "logs")
		if err := os.MkdirAll(logs, 0700); err != nil {
			t.Fatal(err)
		}
		row := fmt.Sprintf(`{"type":"PLANNER_RESPONSE","status":"DONE","source":"MODEL","step_index":1,"created_at":%q,"input_tokens":13,"output_tokens":2,"cache_read_tokens":5,"content":"SYNTHETIC_PRIVATE_BODY"}`+"\n", time.Now().UTC().Format(time.RFC3339Nano))
		if err := os.WriteFile(filepath.Join(logs, "transcript.jsonl"), []byte(row), 0600); err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, core.AccountConfig{ID: account, Provider: "antigravity", Auth: "local", ProviderPaths: map[string]string{
			"transcripts_dir": root, "status_file": filepath.Join(t.TempDir(), "status.json"),
		}})
	}
	latest := hub.NewStore(time.Minute)
	for _, state := range []string{"working", "idle", "working"} {
		snaps := make(map[string]core.UsageSnapshot)
		for _, account := range accounts {
			// These latest observations deliberately repeat unrelated counters.
			// Only the identifiable, settled native transcript contributes history.
			payload := []byte(fmt.Sprintf(`{"session_id":"11111111-1111-4111-8111-111111111111","agent_state":%q,"model":{"id":"current-model"},"context_window":{"total_input_tokens":999,"total_output_tokens":111,"current_usage":{"input_tokens":999,"output_tokens":111}},"email":"SYNTHETIC_PRIVATE_EMAIL","transcript_path":"/SYNTHETIC_PRIVATE_PATH","unknown":"SYNTHETIC_PRIVATE_UNKNOWN"}`, state))
			if _, err := antigravity.CaptureStatusLine(payload, account.Path("status_file", "")); err != nil {
				t.Fatal(err)
			}
			opts := shared.TelemetryCollectOptions{Paths: account.PathMap()}
			opts.Paths["account_id"] = account.ID
			collector := telemetry.NewSourceCollector(antigravity.New(), opts, account.ID)
			requests, err := collector.Collect(ctx)
			if err != nil || len(requests) != 1 {
				t.Fatalf("native source requests=%d err=%v", len(requests), err)
			}
			if _, err := store.Ingest(ctx, requests[0]); err != nil {
				t.Fatal(err)
			}
			hook, err := antigravity.New().ParseHookPayload(payload, opts)
			if err != nil || len(hook) != 0 {
				t.Fatal("status transition manufactured usage history")
			}
			snaps[account.ID], err = antigravity.New().Fetch(ctx, account)
			if err != nil {
				t.Fatal(err)
			}
		}
		snaps, err = telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, storePath, snaps, telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll})
		if err != nil {
			t.Fatal(err)
		}
		var count, total int
		if err := store.DB().QueryRow(`SELECT COUNT(*),SUM(total_tokens) FROM usage_events WHERE event_type='message_usage'`).Scan(&count, &total); err != nil {
			t.Fatal(err)
		}
		if count != 2 || total != 40 {
			t.Fatalf("status transition/replay changed history count=%d total=%d", count, total)
		}
		batch := []core.UsageSnapshot{}
		for _, snap := range snaps {
			for key, want := range map[string]float64{"window_requests": 1, "window_tokens": 15, "window_cache_read_tokens": 5} {
				got := snap.Metrics[key].Used
				if got == nil || *got != want {
					t.Fatalf("%s=%v want %g", key, got, want)
				}
			}
			batch = append(batch, snap)
		}
		latest.Ingest(core.RemoteEnvelope{Machine: "synthetic-antigravity-machine", SentAt: time.Now(), Snapshots: batch})
		received := latest.Snapshots()
		if len(received) != 2 {
			t.Fatal("Hub retained status transitions as additional snapshots")
		}
		encoded, err := json.Marshal(received)
		if err != nil || strings.Contains(string(encoded), "SYNTHETIC_PRIVATE") {
			t.Fatal("private native content reached Hub projection")
		}
	}
}
