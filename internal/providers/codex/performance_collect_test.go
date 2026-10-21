package codex

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/performance"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

func codexCollectorFixture(t *testing.T, dir, thread, response string, at time.Time, output int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout.jsonl")
	data := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"cli_version":"0.163.0","model":"model","model_provider":"openai"}}
{"timestamp":%q,"type":"token_usage_record","payload":{"response_id":%q,"thread_id":%q,"turn_id":"turn","usage":{"output_tokens":%d,"reasoning_output_tokens":1000},"turn_token_usage":{"output_tokens":90000},"thread_token_usage":{"output_tokens":900000},"prompt":"PRIVATE_CODEX_SENTINEL"}}
`, at.Add(-time.Minute).Format(time.RFC3339Nano), thread, at.Format(time.RFC3339Nano), response, thread, output)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexCollectorDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE logs(id INTEGER PRIMARY KEY,ts INTEGER,ts_nanos INTEGER,target TEXT,feedback_log_body TEXT,process_uuid TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func codexCollectorLog(t *testing.T, db *sql.DB, at time.Time, target, body string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO logs(ts,ts_nanos,target,feedback_log_body,process_uuid) VALUES(?,?,?,?,?)`, at.Unix(), at.Nanosecond(), target, body, "process"); err != nil {
		t.Fatal(err)
	}
}

func TestCodexNativeTimingLateDBReplayCorrectionAndRestart(t *testing.T) {
	root := t.TempDir()
	dir, dbPath := filepath.Join(root, "sessions"), filepath.Join(root, "logs_2.sqlite")
	start := time.Date(2026, 10, 21, 0, 0, 0, 123000000, time.UTC)
	end := start.Add(40 * time.Second)
	path := codexCollectorFixture(t, dir, "thread", "response", end, 3000)
	opts := shared.TelemetryCollectOptions{PerformanceEnabled: true, Paths: map[string]string{"sessions_dir": dir, "account_id": "personal", "baseline_existing": "false"}}
	p := New()
	events, err := p.Collect(context.Background(), opts)
	if err != nil || len(events) != 1 || events[0].Performance.DurationMS != nil {
		t.Fatal("missing DB must retain unknown request usage")
	}
	db := codexCollectorDB(t, dbPath)
	codexCollectorLog(t, db, start.Add(-time.Second), "codex", "unrelated")
	codexCollectorLog(t, db, start, "feedback_tags", `try_run_sampling_request{thread_id="thread" turn_id="turn"}: approval_policy="never" sandbox_policy="workspace" tags_json="{}"`)
	codexCollectorLog(t, db, end, "feedback_tags", `last_model_response_id="response"`)
	events, err = p.Collect(context.Background(), opts)
	if err != nil || len(events) != 1 {
		t.Fatalf("late timing: count=%d err=%v", len(events), err)
	}
	r := performance.NormalizeRequest(*events[0].Performance)
	if r.ResponseTokS == nil || *r.ResponseTokS != 75 || r.Outcome != core.OutcomeSuccess || r.TimingBasis != core.TimingInferredLogBoundary || r.Revision != end.UnixMilli() {
		t.Fatalf("late native response evidence: %+v", r)
	}
	for range 10 {
		events, err = p.Collect(context.Background(), opts)
		if err != nil || len(events) != 0 {
			t.Fatalf("stable replay emitted metadata: count=%d err=%v", len(events), err)
		}
	}
	// An authoritative later usage observation corrects output downwards. It
	// retains the request's original native clock and never adds reasoning.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(f, `{"timestamp":%q,"type":"token_usage_record","payload":{"response_id":"response","thread_id":"thread","turn_id":"turn","usage":{"output_tokens":2000,"reasoning_output_tokens":9999}}}`+"\n", end.Add(time.Second).Format(time.RFC3339Nano))
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	events, err = p.Collect(context.Background(), opts)
	if err != nil || len(events) != 1 {
		t.Fatal("correction missing")
	}
	r = performance.NormalizeRequest(*events[0].Performance)
	if r.ResponseTokS == nil || *r.ResponseTokS != 50 || *r.OutputTokens != 2000 {
		t.Fatalf("corrected speed %+v", r)
	}
	restarted, err := New().Collect(context.Background(), opts)
	if err != nil || len(restarted) != 1 || !reflect.DeepEqual(events[0].Performance, restarted[0].Performance) {
		t.Fatal("restart changed identity, timing or correction")
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "PRIVATE_CODEX_SENTINEL") || strings.Contains(string(encoded), root) {
		t.Fatal("source content/path leaked into transport metadata")
	}
	// A newly observed earlier sampling attempt makes the interval ambiguous.
	codexCollectorLog(t, db, start.Add(time.Second), "feedback_tags", `try_run_sampling_request{thread_id="thread" turn_id="turn"}: approval_policy="never" sandbox_policy="workspace" tags_json="{}"`)
	events, err = p.Collect(context.Background(), opts)
	if err != nil || len(events) != 1 {
		t.Fatal("late contradiction not emitted")
	}
	r = performance.NormalizeRequest(*events[0].Performance)
	if r.ResponseTokS != nil || r.TimingStatus != core.TimingAmbiguous {
		t.Fatalf("ambiguous retry retained rate: %+v", r)
	}
}

func TestCodexNativeTimingProfileResolutionAndFeatureOff(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "unrelated-home"))
	cases := []struct{ dir, explicit, want string }{
		{filepath.Join(root, "personal", "sessions"), "", filepath.Join(root, "personal", "logs_2.sqlite")},
		{filepath.Join(root, "personal", "archived_sessions"), "", filepath.Join(root, "personal", "logs_2.sqlite")},
		{filepath.Join(root, "company", "custom"), "", ""},
		{filepath.Join(root, "company", "custom"), filepath.Join(root, "company", "native.db"), filepath.Join(root, "company", "native.db")},
	}
	for _, tc := range cases {
		if got := codexLogsPath(shared.TelemetryCollectOptions{Paths: map[string]string{"codex_logs_db": tc.explicit}}, tc.dir); got != tc.want {
			t.Fatalf("profile path %q != %q", got, tc.want)
		}
	}
	dir := filepath.Join(root, "personal", "sessions")
	codexCollectorFixture(t, dir, "thread", "response", time.Now().UTC(), 3000)
	p := New()
	opts := shared.TelemetryCollectOptions{Paths: map[string]string{"sessions_dir": dir, "account_id": "personal", "codex_logs_db": filepath.Join(root, "must-not-create.sqlite")}}
	events, err := p.Collect(context.Background(), opts)
	if err != nil || len(events) != 0 || p.nativeTiming != nil {
		t.Fatal("disabled feature touched timing state")
	}
	opts.PerformanceEnabled = true
	events, err = p.Collect(context.Background(), opts)
	if err != nil || len(events) != 1 {
		t.Fatal("off cache prevented opt-in")
	}
	if _, err = os.Stat(opts.Paths["codex_logs_db"]); !os.IsNotExist(err) {
		t.Fatal("reader created a source DB")
	}
	// A separately selected account gets its own cache, even at the same path;
	// no evidence or account attribution is inherited from the first scope.
	opts.Paths["account_id"] = "another-profile"
	events, err = p.Collect(context.Background(), opts)
	if err != nil || len(events) != 1 || events[0].AccountID != "another-profile" {
		t.Fatal("profile/account cache collision")
	}
}

func TestCodexNativeTimingPendingAfterArchiveAndUnknownScopeRecovery(t *testing.T) {
	root := t.TempDir()
	dir, dbPath := filepath.Join(root, "sessions"), filepath.Join(root, "logs_2.sqlite")
	start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	end := start.Add(40 * time.Second)
	path := codexCollectorFixture(t, dir, "thread", "response", end, 3000)
	db := codexCollectorDB(t, dbPath)
	codexCollectorLog(t, db, start.Add(-time.Second), "codex", "retention boundary")
	codexCollectorLog(t, db, start, "feedback_tags", `try_run_sampling_request{thread_id="thread" turn_id="turn"}: approval_policy="never" sandbox_policy="workspace" tags_json="{}"`)
	p := New()
	opts := shared.TelemetryCollectOptions{PerformanceEnabled: true, Paths: map[string]string{"sessions_dir": dir, "baseline_existing": "false"}}
	if _, err := p.Collect(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	// The last rollout leaves the configured active directory before its
	// completion feedback reaches SQLite. Pending identities still enrich.
	if err := os.Rename(path, filepath.Join(root, "archived.jsonl")); err != nil {
		t.Fatal(err)
	}
	codexCollectorLog(t, db, end, "feedback_tags", `last_model_response_id="response"`)
	events, err := p.Collect(context.Background(), opts)
	if err != nil || len(events) != 1 || events[0].Performance.DurationMS == nil {
		t.Fatal("archived pending response lost late timing")
	}
	// A late unscoped completion temporarily makes this interval ambiguous.
	codexCollectorLog(t, db, start.Add(20*time.Second), "feedback_tags", `last_model_response_id="other-response"`)
	events, err = p.Collect(context.Background(), opts)
	if err != nil || len(events) != 1 || performance.NormalizeRequest(*events[0].Performance).TimingStatus != core.TimingAmbiguous {
		t.Fatal("unknown scope was silently ignored")
	}
	// Its own JSONL later proves it belongs to a separate thread. The original
	// usage revision is unchanged but the ambiguity is now resolved.
	codexCollectorFixture(t, dir, "other-thread", "other-response", start.Add(20*time.Second), 10)
	events, err = p.Collect(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	var recovered bool
	for _, e := range events {
		r := performance.NormalizeRequest(*e.Performance)
		if r.ID == "response" {
			recovered = r.ResponseTokS != nil && *r.ResponseTokS == 75
		}
		if r.ID == "other-response" && (r.ResponseTokS != nil || r.ExclusionReason != "missing_start") {
			t.Fatalf("missing start diagnostic lost: %+v", r)
		}
	}
	if !recovered {
		t.Fatal("resolved late usage scope did not recover native clock")
	}
}

func TestCodexNativeTimingContradictionDoesNotInventEvidence(t *testing.T) {
	r := core.PerformanceRequest{Evidence: []string{"native_response_id", "per_response_usage"}, Outcome: core.OutcomeUnknown}
	applyCodexTimingMatch(&r, nativeTimingMatch{Contradicted: true, Reason: "native_response_scope_conflict"})
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "native_completed_response_id") || strings.Contains(string(encoded), "codex_cli_version") || r.Outcome != core.OutcomeUnknown {
		t.Fatal("unobserved version/completion claimed")
	}
}

func TestCodexNativeTimingObservedZeroAndMissingOutputDiffer(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_%v", missing), func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "sessions")
			start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
			end := start.Add(40 * time.Second)
			path := codexCollectorFixture(t, dir, "thread", "response", end, 0)
			if missing {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(data), `"output_tokens":0,`, "", 1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			db := codexCollectorDB(t, filepath.Join(root, "logs_2.sqlite"))
			codexCollectorLog(t, db, start.Add(-time.Second), "codex", "retention boundary")
			codexCollectorLog(t, db, start, "feedback_tags", `try_run_sampling_request{thread_id="thread" turn_id="turn"}: approval_policy="never" sandbox_policy="workspace" tags_json="{}"`)
			codexCollectorLog(t, db, end, "feedback_tags", `last_model_response_id="response"`)
			events, err := New().Collect(context.Background(), shared.TelemetryCollectOptions{PerformanceEnabled: true, Paths: map[string]string{"sessions_dir": dir, "baseline_existing": "false"}})
			if err != nil || len(events) != 1 {
				t.Fatal("native source failed")
			}
			r := performance.NormalizeRequest(*events[0].Performance)
			if missing {
				if r.ResponseTokS != nil || r.OutputTokens != nil || r.ExclusionReason != "missing_output" {
					t.Fatal("missing output became observed zero")
				}
			} else if r.ResponseTokS == nil || *r.ResponseTokS != 0 || r.OutputTokens == nil || *r.OutputTokens != 0 {
				t.Fatal("observed zero lost or reasoning added")
			}
		})
	}
}
