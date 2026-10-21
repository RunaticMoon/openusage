package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/integrations"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

// Executes the shipped TypeScript template, transports synthetic events through
// the actual daemon HTTP hook handler, then checks persisted normalized usage.
// Node >=22.18 is required for native TypeScript type stripping (CI pins 24).
func TestOpenCodePluginRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket runtime fixture")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node >=22.18 required for plugin runtime regression")
	}
	for _, mode := range []string{"live", "legacy", "rejected", "offline", "ingest-failed", "malformed-ack"} {
		t.Run(mode, func(t *testing.T) {
			// Keep socket paths below macOS's 104-byte sockaddr_un limit.
			root, err := os.MkdirTemp("", "ou-plugin-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
			socket := filepath.Join(root, "daemon.sock")
			store, err := telemetry.OpenStore(filepath.Join(root, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			svc := &Service{store: store, logThrottle: core.NewLogThrottle(20, time.Minute)}
			var mu sync.Mutex
			var bodies [][]byte
			if mode != "offline" {
				listener, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					mu.Lock()
					bodies = append(bodies, body)
					mu.Unlock()
					r.Body = io.NopCloser(bytes.NewReader(body))
					if mode == "rejected" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if mode == "ingest-failed" {
						_, _ = io.WriteString(w, `{"processed":1,"ingested":0,"failed":1}`)
						return
					}
					if mode == "malformed-ack" {
						_, _ = io.WriteString(w, "not-json")
						return
					}
					svc.handleHook(w, r)
				})
				server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
				go func() { _ = server.Serve(listener) }()
				defer server.Close()
			}
			def, _ := integrations.DefinitionByID(integrations.OpenCodeID)
			export := `export default {id:"openusage-telemetry",setup}`
			if mode == "legacy" {
				export = "export default OpenUsageTelemetry"
			}
			content := strings.ReplaceAll(def.Template, "__OPENUSAGE_OPENCODE_EXPORT__", export)
			plugin := filepath.Join(root, "plugin.mts")
			if err := os.WriteFile(plugin, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, node, "testdata/opencode-plugin-runtime.mjs", plugin, mode)
			cmd.Env = append(os.Environ(), "OPENUSAGE_SOCKET="+socket, "OPENUSAGE_HOOK_SPOOL="+filepath.Join(root, "hook-spool"), "OPENUSAGE_TELEMETRY_ACCOUNT_ID=fixture-opencode", "OPENUSAGE_TELEMETRY_ENABLED=true")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("runtime: %v\n%s", err, out)
			}
			mu.Lock()
			for _, body := range bodies {
				if bytes.Contains(body, []byte("SYNTHETIC_CONTENT_MUST_NOT_LEAVE_HOST")) {
					t.Error("content escaped plugin allowlist")
				}
			}
			mu.Unlock()
			var count, total int64
			if err := store.DB().QueryRow(`SELECT count(*),COALESCE(sum(total_tokens),0) FROM usage_events`).Scan(&count, &total); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "live":
				if count != 6 || total != 471 {
					t.Fatalf("persisted count=%d tokens=%d; want 6/471 (duplicate/cumulative excluded)", count, total)
				}
				var provider, model, workspace, status, account string
				var input, output, reasoning, cacheRead, cacheWrite int64
				var cost float64
				if err := store.DB().QueryRow(`SELECT provider_id,model_raw,workspace_id,status,account_id,input_tokens,output_tokens,reasoning_tokens,cache_read_tokens,cache_write_tokens,cost_usd FROM usage_events WHERE message_id='msg_test'`).Scan(&provider, &model, &workspace, &status, &account, &input, &output, &reasoning, &cacheRead, &cacheWrite, &cost); err != nil {
					t.Fatal(err)
				}
				if provider != "anthropic" || model != "test-model" || workspace != "project" || status != "ok" || account != "fixture-opencode" || input != 100 || output != 30 || reasoning != 5 || cacheRead != 20 || cacheWrite != 2 || cost != 0.12 {
					t.Fatalf("normalization mismatch provider/model/workspace/status/account=%q/%q/%q/%q/%q tokens=%d/%d/%d/%d/%d cost=%v", provider, model, workspace, status, account, input, output, reasoning, cacheRead, cacheWrite, cost)
				}
				if err := store.DB().QueryRow(`SELECT status FROM usage_events WHERE message_id='msg_failed_usage'`).Scan(&status); err != nil || status != "error" {
					t.Fatalf("failed usage status=%q err=%v", status, err)
				}
				var knownInput, unknownOutput, unknownReasoning, unknownCache sql.NullInt64
				var unknownCost sql.NullFloat64
				if err := store.DB().QueryRow(`SELECT input_tokens,output_tokens,reasoning_tokens,cache_read_tokens,cost_usd FROM usage_events WHERE message_id='msg_unknown'`).Scan(&knownInput, &unknownOutput, &unknownReasoning, &unknownCache, &unknownCost); err != nil {
					t.Fatal(err)
				}
				if !knownInput.Valid || knownInput.Int64 != 0 || unknownOutput.Valid || unknownReasoning.Valid || unknownCache.Valid || unknownCost.Valid {
					t.Fatal("unknown fields were converted to explicit zero")
				}
				if err := store.DB().QueryRow(`SELECT input_tokens,output_tokens,cache_read_tokens,cost_usd FROM usage_events WHERE message_id='msg_zero'`).Scan(&knownInput, &unknownOutput, &unknownCache, &unknownCost); err != nil {
					t.Fatal(err)
				}
				if knownInput.Valid || !unknownOutput.Valid || unknownOutput.Int64 != 0 || !unknownCache.Valid || unknownCache.Int64 != 0 || !unknownCost.Valid || unknownCost.Float64 != 0 {
					t.Fatal("explicit zero fields lost")
				}
			case "legacy":
				if count != 1 || total != 13 {
					t.Fatalf("legacy count/tokens=%d/%d", count, total)
				}
			default:
				if count != 0 {
					t.Fatalf("failed transport ingested %d", count)
				}
				files, err := filepath.Glob(filepath.Join(root, "hook-spool", "*.json"))
				if err != nil || len(files) != 1 {
					t.Fatalf("spool files=%v err=%v", files, err)
				}
				info, err := os.Stat(files[0])
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Fatalf("spool mode=%v", info.Mode())
				}
				// Feed the spooled record back through the native parser/store,
				// twice, to prove failure recovery does not double-count.
				raw, err := os.ReadFile(files[0])
				if err != nil {
					t.Fatal(err)
				}
				var record struct {
					Source    string          `json:"source"`
					AccountID string          `json:"account_id"`
					Payload   json.RawMessage `json:"payload"`
				}
				if err := json.Unmarshal(raw, &record); err != nil {
					t.Fatal(err)
				}
				parsed, err := ParseHookRequests(record.Source, record.AccountID, record.Payload)
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					tally, _ := svc.ingestBatch(context.Background(), parsed.Requests)
					if tally.failed != 0 {
						t.Fatal("replay ingest failed")
					}
				}
				if err := store.DB().QueryRow(`SELECT count(*),COALESCE(sum(total_tokens),0) FROM usage_events`).Scan(&count, &total); err != nil {
					t.Fatal(err)
				}
				if count != 1 || total != 157 {
					t.Fatalf("replay count/tokens=%d/%d", count, total)
				}
			}
		})
	}
}
