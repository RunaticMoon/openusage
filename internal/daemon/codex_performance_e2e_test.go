package daemon_test

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
	"github.com/janekbaraniewski/openusage/internal/providers/codex"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

// Native usage arrives first; the independently identified successful
// completion arrives later without any JSONL change. All inputs are synthetic,
// and the collector, SQLite stores, exporter and Hub run in isolation.
func TestCodexNativePerformanceLateCompletionToHub(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	end, start := now.Add(-20*time.Second), now.Add(-60*time.Second)
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"thread","model":"gpt-test","model_provider":"openai","cli_version":"0.163.0"}}
{"timestamp":%q,"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":3000,"reasoning_output_tokens":1000,"total_tokens":3100},"last_token_usage":{"input_tokens":100,"output_tokens":3000,"reasoning_output_tokens":1000,"total_tokens":3100}}}}
{"timestamp":%q,"type":"token_usage_record","payload":{"response_id":"response","thread_id":"thread","turn_id":"turn","usage":{"output_tokens":3000,"reasoning_output_tokens":1000},"private":"PRIVATE_CODEX_TIMING_SENTINEL"}}
`, start.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(sessions, "source.jsonl"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	logsPath := filepath.Join(root, "logs_2.sqlite")
	logs, err := sql.Open("sqlite3", logsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	if _, err := logs.Exec(`CREATE TABLE logs(id INTEGER PRIMARY KEY,ts INTEGER,ts_nanos INTEGER,target TEXT,feedback_log_body TEXT,process_uuid TEXT)`); err != nil {
		t.Fatal(err)
	}
	appendLog := func(at time.Time, body string) {
		t.Helper()
		if _, err := logs.Exec(`INSERT INTO logs(ts,ts_nanos,target,feedback_log_body,process_uuid) VALUES(?,?,?,?,?)`, at.Unix(), at.Nanosecond(), "feedback_tags", body, "synthetic-process"); err != nil {
			t.Fatal(err)
		}
	}
	appendLog(start.Add(-time.Second), "retained boundary sentinel")
	appendLog(start, `try_run_sampling_request{thread_id="thread" turn_id="turn"}: model="gpt-test" approval_policy="never" sandbox_policy="workspace" tags_json="{}"`)
	const account = "synthetic-codex-personal"
	opts := shared.TelemetryCollectOptions{Paths: map[string]string{"sessions_dir": sessions, "codex_logs_db": logsPath, "baseline_existing": "false"}}
	plain, err := telemetry.NewSourceCollector(codex.New(), opts, account).Collect(ctx)
	if err != nil {
		t.Fatal("disabled native collection failed")
	}
	baseline, baselinePath := performanceE2EStore(t, false)
	performanceE2EIngest(t, baseline, plain)
	opts.PerformanceEnabled = true
	collector := telemetry.NewSourceCollector(codex.New(), opts, account)
	initial, err := collector.Collect(ctx)
	if err != nil {
		t.Fatal("initial native collection failed")
	}
	store, path := performanceE2EStore(t, true)
	performanceE2EIngest(t, store, initial)
	view := func(database string, enabled bool) core.UsageSnapshot {
		t.Helper()
		got, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, database, map[string]core.UsageSnapshot{account: core.NewUsageSnapshot("codex", account)}, telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll, PerformanceEnabled: enabled, Now: now})
		if err != nil {
			t.Fatal("isolated read model failed")
		}
		return got[account]
	}
	before := view(path, true)
	if before.Performance == nil || before.Performance.Summary.Coverage.Candidates != 1 || before.Performance.Summary.Coverage.Measured != 0 {
		t.Fatal("usage without completion was measured")
	}
	appendLog(end, `last_model_response_id="response"`)
	late, err := collector.Collect(ctx)
	if err != nil || len(late) != 1 || !late[0].PerformanceOnly {
		t.Fatal("unchanged JSONL did not receive metadata-only completion enrichment")
	}
	for range 3 {
		performanceE2EIngest(t, store, late)
	}
	if err := store.RecordPerformanceScan(ctx, "codex", account, now); err != nil {
		t.Fatal(err)
	}
	measured := view(path, true)
	without := view(baselinePath, false)
	summary := measured.Performance.Summary
	if summary.Coverage.Candidates != 1 || summary.Coverage.Measured != 1 || summary.Coverage.Inferred != 1 || summary.Coverage.Direct != 0 || summary.Stats.OutputTokens != 3000 || summary.Stats.DurationMS != 40000 || summary.Stats.WeightedResponseTokS == nil || *summary.Stats.WeightedResponseTokS != 75 {
		t.Fatalf("numeric native timing mismatch: %+v", summary)
	}
	if !reflect.DeepEqual(without.Metrics, measured.Metrics) || !reflect.DeepEqual(performanceE2EAccounting(t, baseline), performanceE2EAccounting(t, store)) {
		t.Fatal("completion enrichment changed canonical accounting or metrics")
	}
	warm, err := collector.Collect(ctx)
	if err != nil || len(warm) != 0 {
		t.Fatal("warm replay emitted duplicate source events")
	}
	endpoint, token := performanceE2EHub(t)
	remote := performanceE2EPush(t, endpoint, token, map[string]core.UsageSnapshot{account: measured}, 1)["synthetic-performance:codex:"+account]
	if remote.Performance == nil || !performanceE2EJSONEqual(summary, remote.Performance.Summary) || remote.Performance.Summary.Coverage.Direct != 0 {
		t.Fatal("Hub lost inferred provenance or numeric timing")
	}
	encoded, err := json.Marshal(remote)
	if err != nil || strings.Contains(string(encoded), "PRIVATE_CODEX_TIMING_SENTINEL") || strings.Contains(string(encoded), root) || strings.Contains(string(encoded), "last_model_response_id=") {
		t.Fatal("raw source text or private path reached Hub")
	}
}

// Explicit personal-source opt-in. Source JSONL/SQLite are only read; every
// write and HTTP request uses temporary local stores and an isolated Hub.
func TestPerformanceRealCodexReadOnlyToHub(t *testing.T) {
	root, logs := os.Getenv("OPENUSAGE_PERFORMANCE_REAL_CODEX_ROOT"), os.Getenv("OPENUSAGE_PERFORMANCE_REAL_CODEX_LOGS_DB")
	if root == "" || logs == "" {
		t.Skip("requires explicit personal Codex sessions root and native logs DB")
	}
	ctx := context.Background()
	const account = "real-codex-validation"
	opts := shared.TelemetryCollectOptions{PerformanceEnabled: true, Paths: map[string]string{"sessions_dir": root, "codex_logs_db": logs, "baseline_existing": "false"}}
	collector := telemetry.NewSourceCollector(codex.New(), opts, account)
	started := time.Now()
	requests, err := collector.Collect(ctx)
	coldElapsed := time.Since(started)
	if err != nil || len(requests) == 0 {
		t.Fatal("read-only native Codex collection failed or empty")
	}
	store, path := performanceE2EStore(t, true)
	baseline, baselinePath := performanceE2EStore(t, false)
	// Use the same observed accounting batch for both stores: live source files
	// may grow between separate scans. Metadata never manufactures usage rows.
	ingestBatch := func(batch []telemetry.IngestRequest) {
		t.Helper()
		plain := make([]telemetry.IngestRequest, 0, len(batch))
		for _, r := range batch {
			if !r.PerformanceOnly {
				r.Performance = nil
				plain = append(plain, r)
			}
		}
		performanceE2EIngest(t, baseline, plain)
		for range 2 {
			performanceE2EIngest(t, store, batch)
		}
	}
	ingestBatch(requests)
	var warmElapsed, catchupElapsed time.Duration
	warmEvents, sourceEvents, advances := 0, len(requests), 0
	// The native reader intentionally bounds each pass. Always take one warm
	// sample, then advance only while no measurable retained evidence exists.
	// Live accounting additions are ingested into both isolated stores.
	for attempt := 0; attempt < 8; attempt++ {
		started = time.Now()
		warm, err := collector.Collect(ctx)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatal("warm read-only source collection failed")
		}
		if attempt == 0 {
			warmElapsed, warmEvents = elapsed, len(warm)
		}
		advances++
		catchupElapsed += elapsed
		sourceEvents += len(warm)
		ingestBatch(warm)
		current, err := telemetry.QueryPerformance(ctx, path, telemetry.PerformanceQuery{Source: "codex", Account: account, Now: time.Now().UTC()})
		if err != nil {
			t.Fatal("isolated catch-up read model failed")
		}
		if current.Summary.Coverage.Measured > 0 {
			break
		}
	}
	if !reflect.DeepEqual(performanceE2EAccounting(t, baseline), performanceE2EAccounting(t, store)) {
		t.Fatal("real-source performance changed canonical accounting")
	}
	now := time.Now().UTC()
	if err := store.RecordPerformanceScan(ctx, "codex", account, now); err != nil {
		t.Fatal("isolated freshness metadata failed")
	}
	base := map[string]core.UsageSnapshot{account: core.NewUsageSnapshot("codex", account)}
	snaps, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, path, base, telemetry.ReadModelOptions{PerformanceEnabled: true, TimeWindow: core.TimeWindowAll, Now: now})
	if err != nil {
		t.Fatal("isolated native read model failed")
	}
	plainSnaps, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, baselinePath, base, telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll, Now: now})
	if err != nil || !reflect.DeepEqual(plainSnaps[account].Metrics, snaps[account].Metrics) {
		t.Fatal("real-source performance changed canonical metrics")
	}
	p := snaps[account].Performance
	if p == nil || p.Summary.Coverage.Inferred == 0 || p.Summary.Coverage.Direct != 0 || p.Summary.Stats.WeightedResponseTokS == nil {
		t.Fatal("no conservative native Codex timing reached snapshot")
	}
	endpoint, token := performanceE2EHub(t)
	remote := performanceE2EPush(t, endpoint, token, snaps, 1)["synthetic-performance:codex:"+account]
	if remote.Performance == nil || !performanceE2EJSONEqual(p.Summary, remote.Performance.Summary) {
		t.Fatal("native timing changed during Exporter/Hub transport")
	}
	var leaks int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM performance_requests WHERE record_json LIKE '%"feedback_log_body":%' OR record_json LIKE '%"content":%' OR record_json LIKE '%"prompt":%' OR record_json LIKE '%"authorization":%'`).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatal("native metadata privacy check failed")
	}
	c, s := p.Summary.Coverage, p.Summary.Stats
	t.Logf("numeric validation only: source_events=%d candidates=%d measured=%d direct=%d inferred=%d excluded=%d output_tokens=%d duration_ms=%.3f weighted_response_tok_s=%.6f warm_source_events=%d cold_ms=%.3f warm_ms=%.3f catchup_passes=%d catchup_collect_ms=%.3f accounting_equal=true metrics_equal=true replay_pass=true hub_snapshots=1", sourceEvents, c.Candidates, c.Measured, c.Direct, c.Inferred, c.Excluded, s.OutputTokens, s.DurationMS, *s.WeightedResponseTokS, warmEvents, float64(coldElapsed.Microseconds())/1000, float64(warmElapsed.Microseconds())/1000, advances, float64(catchupElapsed.Microseconds())/1000)
}
