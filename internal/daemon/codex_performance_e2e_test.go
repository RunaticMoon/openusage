package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/config"
	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/exporter"
	"github.com/janekbaraniewski/openusage/internal/providers/codex"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

// Native usage arrives first; an exact-ID endpoint tag arrives later without
// any JSONL change. The tag also represents incomplete/interrupted responses:
// it supplies interval metadata, never successful outcome evidence. All inputs
// and the collector, SQLite stores, exporter and Hub are isolated.
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
	if err != nil || len(late) != 1 || !late[0].PerformanceOnly || late[0].Performance == nil {
		t.Fatal("unchanged JSONL did not receive metadata-only completion enrichment")
	}
	// Feed the previous adapter's incorrect success assertion. Current
	// normalization must suppress it immediately; the later v2 observation
	// then replaces its provenance without changing accounting or usage revision.
	legacy := late[0]
	legacyPerformance := legacy.Performance.DeepClone()
	legacyPerformance.AdapterVersion = "codex_native_sqlite_v1"
	legacyPerformance.Outcome = core.OutcomeSuccess
	legacyPerformance.Evidence = []string{"native_response_id", "per_response_usage", "native_completed_response_id"}
	legacy.Performance = &legacyPerformance
	// A separate old-state store avoids trying to downgrade the authoritative
	// v2 unknown record already checked above back to v1 through normal ingest.
	store, path = performanceE2EStore(t, true)
	performanceE2EIngest(t, store, plain)
	performanceE2EIngest(t, store, []telemetry.IngestRequest{legacy})
	legacySnapshot := view(path, true)
	if legacySnapshot.Performance == nil || legacySnapshot.Performance.Summary.Coverage.Measured != 0 || legacySnapshot.Performance.Summary.Coverage.Unknown != 1 || legacySnapshot.Performance.Summary.Stats.WeightedResponseTokS != nil {
		t.Fatal("current runtime trusted the retired adapter's unjustified success assertion")
	}
	endpoint, token := performanceE2EHub(t)
	codexPerformancePushLatest(t, endpoint, token, account, legacySnapshot)
	for range 3 {
		performanceE2EIngest(t, store, late)
	}
	if err := store.RecordPerformanceScan(ctx, "codex", account, now); err != nil {
		t.Fatal(err)
	}
	observed := view(path, true)
	without := view(baselinePath, false)
	summary := observed.Performance.Summary
	if summary.Coverage.Candidates != 1 || summary.Coverage.Measured != 1 || summary.Coverage.Unknown != 1 || summary.Coverage.Excluded != 0 || summary.Coverage.Inferred != 1 || summary.Coverage.Direct != 0 || len(summary.Coverage.Reasons) != 0 || summary.Stats.Count != 1 || summary.Stats.OutputTokens != 3000 || summary.Stats.DurationMS != 40000 || summary.Stats.WeightedResponseTokS == nil || *summary.Stats.WeightedResponseTokS != 75 || summary.Stats.P50TokS == nil || summary.Stats.P90TokS == nil {
		t.Fatalf("valid observed native interval was not measured: %+v", summary)
	}
	stored := codexPerformanceStoredRecords(t, store)
	if len(stored) != 1 || stored[0].OutputTokens == nil || *stored[0].OutputTokens != 3000 || stored[0].DurationMS == nil || *stored[0].DurationMS != 40000 || stored[0].StartedAt == nil || !stored[0].StartedAt.Equal(start) || stored[0].EndedAt == nil || !stored[0].EndedAt.Equal(end) || stored[0].TimingBasis != core.TimingInferredLogBoundary || stored[0].Outcome != core.OutcomeUnknown || stored[0].ResponseTokS != nil || stored[0].AdapterVersion != "codex_native_sqlite_v2" {
		t.Fatal("native interval/output metadata was lost or promoted to a successful response")
	}
	if !reflect.DeepEqual(without.Metrics, observed.Metrics) || !reflect.DeepEqual(performanceE2EAccounting(t, baseline), performanceE2EAccounting(t, store)) {
		t.Fatal("completion enrichment changed canonical accounting or metrics")
	}
	warm, err := collector.Collect(ctx)
	if err != nil || len(warm) != 0 {
		t.Fatal("warm replay emitted duplicate source events")
	}
	remote := codexPerformancePushLatest(t, endpoint, token, account, observed)
	if remote.Performance == nil || !performanceE2EJSONEqual(summary, remote.Performance.Summary) || remote.Performance.Summary.Coverage.Direct != 0 {
		t.Fatal("Hub did not retain observed speed and unknown outcome")
	}
	if len(remote.Performance.Requests) != 1 || remote.Performance.Requests[0].Outcome != core.OutcomeUnknown || remote.Performance.Requests[0].ResponseTokS == nil || *remote.Performance.Requests[0].ResponseTokS != 75 || remote.Performance.Requests[0].OutputTokens == nil || *remote.Performance.Requests[0].OutputTokens != 3000 || remote.Performance.Requests[0].DurationMS == nil || *remote.Performance.Requests[0].DurationMS != 40000 {
		t.Fatal("Hub lost observed speed or changed the unknown outcome")
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
	// sample, then advance only while no retained interval metadata exists.
	// Live accounting additions are ingested into both isolated stores.
	// Soft page deadlines can require more passes than the row-limit quotient
	// under race instrumentation or host load. This test-only bound permits
	// catch-up without relaxing the production scan budget or inference guards.
	catchupLimit := 8
	if os.Getenv("OPENUSAGE_PERFORMANCE_REAL_CODEX_REQUIRE_INTERVAL") == "1" {
		catchupLimit = 64
	}
	for attempt := 0; attempt < catchupLimit; attempt++ {
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
		if intervals, _, _ := codexPerformanceStoredIntervals(t, store); intervals > 0 {
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
	if p == nil || p.DefinitionVersion != core.PerformanceDefinitionVersion || p.Summary.Coverage.Candidates == 0 || p.Summary.Coverage.Measured != p.Summary.Coverage.Inferred || p.Summary.Coverage.Direct != 0 || p.Summary.Coverage.Unknown != p.Summary.Coverage.Candidates {
		t.Fatal("native observed coverage or unknown outcomes were lost")
	}
	intervals, intervalOutput, intervalDuration := codexPerformanceStoredIntervals(t, store)
	if p.Summary.Coverage.Measured != int64(intervals) || (intervals > 0 && (p.Summary.Stats.WeightedResponseTokS == nil || p.Summary.Stats.OutputTokens != intervalOutput || math.Abs(p.Summary.Stats.DurationMS-intervalDuration) > 1e-6)) {
		t.Fatal("valid stored intervals were not projected into observed speed")
	}
	// The configured active scope can legitimately have no retained native
	// boundary pair. Positive-interval evidence is an explicit opt-in assertion,
	// never a reason to widen the source scope to archived sessions.
	if os.Getenv("OPENUSAGE_PERFORMANCE_REAL_CODEX_REQUIRE_INTERVAL") == "1" && intervals == 0 {
		t.Fatal("explicitly required native interval metadata was not retained")
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
	c := p.Summary.Coverage
	t.Logf("numeric validation only: source_events=%d candidates=%d measured=%d direct=%d inferred=%d unknown=%d excluded=%d stored_intervals=%d interval_output_tokens=%d interval_duration_ms=%.3f weighted_response_tok_s=%s warm_source_events=%d cold_ms=%.3f warm_ms=%.3f catchup_passes=%d catchup_collect_ms=%.3f accounting_equal=true metrics_equal=true replay_pass=true hub_snapshots=1", sourceEvents, c.Candidates, c.Measured, c.Direct, c.Inferred, c.Unknown, c.Excluded, intervals, intervalOutput, intervalDuration, codexPerformanceRateJSON(p.Summary.Stats.WeightedResponseTokS), warmEvents, float64(coldElapsed.Microseconds())/1000, float64(warmElapsed.Microseconds())/1000, advances, float64(catchupElapsed.Microseconds())/1000)
}

func codexPerformanceStoredRecords(t *testing.T, store *telemetry.Store) []core.PerformanceRequest {
	t.Helper()
	rows, err := store.DB().Query(`SELECT record_json FROM performance_requests`)
	if err != nil {
		t.Fatal("isolated native metadata query failed")
	}
	defer rows.Close()
	var records []core.PerformanceRequest
	for rows.Next() {
		var encoded string
		var record core.PerformanceRequest
		if rows.Scan(&encoded) != nil || json.Unmarshal([]byte(encoded), &record) != nil {
			t.Fatal("isolated native metadata decoding failed")
		}
		records = append(records, record)
	}
	if rows.Err() != nil {
		t.Fatal("isolated native metadata iteration failed")
	}
	return records
}

func codexPerformanceStoredIntervals(t *testing.T, store *telemetry.Store) (count int, output int64, duration float64) {
	t.Helper()
	for _, r := range codexPerformanceStoredRecords(t, store) {
		if r.ResponseTokS != nil || r.Outcome == core.OutcomeSuccess {
			t.Fatal("native endpoint metadata manufactured successful response outcome")
		}
		if r.StartedAt != nil && r.EndedAt != nil && r.DurationMS != nil && *r.DurationMS > 0 && r.OutputTokens != nil && r.TimingStatus == core.TimingValid {
			count++
			output += *r.OutputTokens
			duration += *r.DurationMS
		}
	}
	return
}

// Unlike a count-only push probe, this waits for the same Hub key's updated
// summary and request metadata. A cached zero-rate summary with stale adapter
// provenance cannot satisfy the update assertion.
func codexPerformancePushLatest(t *testing.T, endpoint, token, account string, snapshot core.UsageSnapshot) core.UsageSnapshot {
	t.Helper()
	expected := snapshot.DeepClone()
	for i := range expected.Performance.Requests {
		if expected.Performance.Requests[i].HostID == "" {
			expected.Performance.Requests[i].HostID = "synthetic-performance"
		}
	}
	e, err := exporter.New(config.ExportConfig{Target: endpoint, MachineName: "synthetic-performance", AuthToken: token, IntervalSeconds: 1})
	if err != nil {
		t.Fatal("isolated exporter initialization failed")
	}
	e.Ingest(map[string]core.UsageSnapshot{account: snapshot})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Start(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/snapshots", nil)
		if err != nil {
			t.Fatal("isolated Hub request creation failed")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("isolated Hub request failed")
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024))
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatal("isolated Hub response failed")
		}
		var received map[string]core.UsageSnapshot
		if json.Unmarshal(body, &received) != nil {
			t.Fatal("isolated Hub response decoding failed")
		}
		remote := received["synthetic-performance:codex:"+account]
		if len(received) == 1 && remote.Performance != nil && performanceE2EJSONEqual(expected.Performance.Summary, remote.Performance.Summary) && performanceE2EJSONEqual(expected.Performance.Requests, remote.Performance.Requests) {
			return remote
		}
		if time.Now().After(deadline) {
			t.Fatal("Hub did not replace the previous performance summary")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func codexPerformanceRateJSON(rate *float64) string {
	encoded, _ := json.Marshal(rate)
	return string(encoded)
}
