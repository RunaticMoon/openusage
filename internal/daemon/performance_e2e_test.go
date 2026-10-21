package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
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
	"github.com/janekbaraniewski/openusage/internal/hub"
	"github.com/janekbaraniewski/openusage/internal/providers/claude_code"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

// This exercises the actual native-file parser, accounting mapper, local
// metadata store, read model, Exporter JSON and original Hub HTTP implementation.
// No production configuration, listener, database or provider request is used.
func TestPerformanceNativeSourceAccountingExporterHub(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-10 * time.Second)
	root := t.TempDir()
	var lines strings.Builder
	for i, duration := range []time.Duration{time.Second, 2 * time.Second} {
		fmt.Fprintf(&lines, `{"type":"api-request","id":"start-%d","sessionId":"session","timestamp":%q,"params":{"prompt":"PRIVATE_PERFORMANCE_SOURCE_BODY"}}`+"\n", i, start.Format(time.RFC3339Nano))
		fmt.Fprintf(&lines, `{"type":"assistant","sessionId":"session","requestId":"request-%d","requestRef":"start-%d","timestamp":%q,"message":{"id":"message-%d","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":700,"output_tokens":100,"cache_read_input_tokens":300,"cache_creation_input_tokens":200},"content":[{"type":"text","text":"PRIVATE_PERFORMANCE_SOURCE_BODY"}]}}`+"\n", i, i, start.Add(duration).Format(time.RFC3339Nano), i)
	}
	if err := os.WriteFile(filepath.Join(root, "source.jsonl"), []byte(lines.String()), 0600); err != nil {
		t.Fatal(err)
	}
	account := "synthetic-personal"
	options := shared.TelemetryCollectOptions{Paths: map[string]string{"projects_dir": root, "alt_projects_dir": t.TempDir(), "baseline_existing": "false"}}
	off, offPath := performanceE2EStore(t, false)
	on, onPath := performanceE2EStore(t, true)
	// Existing hooks carry accounting but no trustworthy request clock. They
	// must remain usable, then receive timing from the same native request.
	for i := range 2 {
		raw := []byte(fmt.Sprintf(`{"hook_event_name":"Stop","timestamp":%q,"session_id":"session","request_id":"request-%d","message_id":"message-%d","model":"claude-sonnet-4","usage":{"input_tokens":700,"output_tokens":100,"cache_read_input_tokens":300,"cache_creation_input_tokens":200}}`, start.Add(time.Duration(i+1)*time.Second).Format(time.RFC3339Nano), i, i))
		hooks, err := telemetry.ParseSourceHookPayload(claude_code.New(), raw, options, account)
		if err != nil || len(hooks) != 1 || hooks[0].Performance != nil {
			t.Fatal("legacy hook contract changed")
		}
		for _, s := range []*telemetry.Store{off, on} {
			performanceE2EIngest(t, s, hooks)
		}
	}
	enabled := options
	enabled.PerformanceEnabled = true
	plainRequests, err := telemetry.NewSourceCollector(claude_code.New(), options, account).Collect(ctx)
	if err != nil {
		t.Fatal("synthetic source collection failed")
	}
	measuredRequests, err := telemetry.NewSourceCollector(claude_code.New(), enabled, account).Collect(ctx)
	if err != nil || len(measuredRequests) != 2 {
		t.Fatal("native timing collection failed")
	}
	for range 10 {
		performanceE2EIngest(t, off, plainRequests)
		performanceE2EIngest(t, on, measuredRequests)
	}
	if err := on.RecordPerformanceScan(ctx, "claude_code", account, now); err != nil {
		t.Fatal(err)
	}
	base := map[string]core.UsageSnapshot{account: core.NewUsageSnapshot("claude_code", account)}
	view := func(path string, enabled bool) core.UsageSnapshot {
		t.Helper()
		snaps, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, path, base, telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll, PerformanceEnabled: enabled, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		return snaps[account]
	}
	without, with := view(offPath, false), view(onPath, true)
	if without.Performance != nil || with.Performance == nil {
		t.Fatal("opt-in snapshot contract failed")
	}
	if !reflect.DeepEqual(without.Metrics, with.Metrics) {
		t.Fatal("performance changed canonical usage or cost metrics")
	}
	if !reflect.DeepEqual(performanceE2EAccounting(t, off), performanceE2EAccounting(t, on)) {
		t.Fatal("performance changed canonical ledger")
	}
	var count int
	if err := on.DB().QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("hook/log/replay accounting count=%d", count)
	}
	summary := with.Performance.Summary
	if summary.Coverage.Candidates != 2 || summary.Coverage.Measured != 2 || summary.Coverage.Direct != 0 || summary.Coverage.Inferred != 2 || summary.Stats.OutputTokens != 200 || summary.Stats.DurationMS != 3000 || summary.Stats.WeightedResponseTokS == nil || math.Abs(*summary.Stats.WeightedResponseTokS-200.0/3) > 1e-9 {
		t.Fatalf("unexpected numeric performance summary: %+v", summary)
	}
	if summary.Stats.P50TokS == nil || summary.Stats.P90TokS == nil {
		t.Fatal("distribution lost")
	}
	endpoint, token := performanceE2EHub(t)
	old := core.NewUsageSnapshot("legacy-provider", "legacy-account")
	received := performanceE2EPush(t, endpoint, token, map[string]core.UsageSnapshot{account: with, "legacy": old}, 2)
	remote := received["synthetic-performance:claude_code:"+account]
	if remote.Performance == nil || !performanceE2EJSONEqual(remote.Performance.Summary, summary) {
		t.Fatal("Exporter/Hub changed request-weighted summary")
	}
	if remote.Performance.HostID != "synthetic-performance" || with.Performance.HostID != "" {
		t.Fatal("Exporter transport identity mutated its source")
	}
	if received["synthetic-performance:legacy-provider:legacy-account"].Performance != nil {
		t.Fatal("old snapshot acquired fabricated performance")
	}
	payload, err := json.Marshal(received)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "PRIVATE_PERFORMANCE_SOURCE_BODY") || strings.Contains(string(payload), root) {
		t.Fatal("private source body/path reached Hub")
	}
	// A pre-feature consumer ignores the additive field without losing usage.
	var legacy map[string]struct {
		ProviderID string                 `json:"provider_id"`
		AccountID  string                 `json:"account_id"`
		Metrics    map[string]core.Metric `json:"metrics"`
	}
	if json.Unmarshal(payload, &legacy) != nil || !reflect.DeepEqual(legacy["synthetic-performance:claude_code:"+account].Metrics, with.Metrics) {
		t.Fatal("old JSON consumer lost existing metrics")
	}
}

// Explicit opt-in only: the caller supplies the previously verified personal
// Claude projects root. Source files are read directly and never copied or
// modified; all writes and HTTP traffic use temporary local resources.
func TestPerformanceRealClaudeReadOnlyToHub(t *testing.T) {
	root := os.Getenv("OPENUSAGE_PERFORMANCE_REAL_CLAUDE_ROOT")
	if root == "" {
		t.Skip("requires explicit personal Claude source root")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		t.Fatal("personal source root unavailable")
	}
	ctx := context.Background()
	const account = "real-personal-validation"
	options := shared.TelemetryCollectOptions{PerformanceEnabled: true, Paths: map[string]string{"projects_dir": root, "alt_projects_dir": t.TempDir(), "baseline_existing": "false"}}
	plainOptions := options
	plainOptions.PerformanceEnabled = false
	started := time.Now()
	plain, err := telemetry.NewSourceCollector(claude_code.New(), plainOptions, account).Collect(ctx)
	baselineElapsed := time.Since(started)
	if err != nil {
		t.Fatal("baseline read-only source collection failed")
	}
	started = time.Now()
	requests, err := telemetry.NewSourceCollector(claude_code.New(), options, account).Collect(ctx)
	enabledElapsed := time.Since(started)
	if err != nil {
		t.Fatal("read-only native source collection failed")
	}
	if len(requests) == 0 {
		t.Fatal("native source emitted no usage")
	}
	store, path := performanceE2EStore(t, true)
	baseline, baselinePath := performanceE2EStore(t, false)
	performanceE2EIngest(t, baseline, plain)
	for range 2 {
		performanceE2EIngest(t, store, requests)
	}
	if !reflect.DeepEqual(performanceE2EAccounting(t, baseline), performanceE2EAccounting(t, store)) {
		t.Fatal("real source opt-in changed canonical accounting")
	}
	now := time.Now().UTC()
	if err := store.RecordPerformanceScan(ctx, "claude_code", account, now); err != nil {
		t.Fatal("scan metadata failed")
	}
	snaps, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, path, map[string]core.UsageSnapshot{account: core.NewUsageSnapshot("claude_code", account)}, telemetry.ReadModelOptions{PerformanceEnabled: true, TimeWindow: core.TimeWindowAll, Now: now})
	if err != nil {
		t.Fatal("canonical read model failed")
	}
	plainSnaps, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, baselinePath, map[string]core.UsageSnapshot{account: core.NewUsageSnapshot("claude_code", account)}, telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll, Now: now})
	if err != nil || !reflect.DeepEqual(plainSnaps[account].Metrics, snaps[account].Metrics) {
		t.Fatal("real source opt-in changed canonical metrics")
	}
	p := snaps[account].Performance
	if p == nil || p.Summary.Coverage.Inferred == 0 || p.Summary.Stats.WeightedResponseTokS == nil {
		t.Fatal("real request timing did not reach snapshot")
	}
	endpoint, token := performanceE2EHub(t)
	remote := performanceE2EPush(t, endpoint, token, snaps, 1)["synthetic-performance:claude_code:"+account]
	if remote.Performance == nil || !performanceE2EJSONEqual(p.Summary, remote.Performance.Summary) {
		t.Fatal("real timing did not survive Exporter/Hub query")
	}
	var leaks int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM performance_requests WHERE record_json LIKE '%"content":%' OR record_json LIKE '%"prompt":%' OR record_json LIKE '%"params":%' OR record_json LIKE '%"authorization":%'`).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatal("metadata allowlist check failed")
	}
	c, s := p.Summary.Coverage, p.Summary.Stats
	t.Logf("numeric validation only: source_events=%d candidates=%d measured=%d direct=%d inferred=%d excluded=%d provisional=%d output_tokens=%d duration_ms=%.3f weighted_response_tok_s=%.6f hub_snapshots=1 replay_pass=true accounting_equal=true cold_off_ms=%.3f warm_on_ms=%.3f", len(requests), c.Candidates, c.Measured, c.Direct, c.Inferred, c.Excluded, c.Provisional, s.OutputTokens, s.DurationMS, *s.WeightedResponseTokS, float64(baselineElapsed.Microseconds())/1000, float64(enabledElapsed.Microseconds())/1000)
}

func performanceE2EJSONEqual(a, b any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(left) == string(right)
}

func performanceE2EStore(t *testing.T, enabled bool) (*telemetry.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := telemetry.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if enabled {
		if err := store.EnablePerformance(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return store, path
}
func performanceE2EIngest(t *testing.T, store *telemetry.Store, requests []telemetry.IngestRequest) {
	t.Helper()
	for _, r := range requests {
		if _, err := store.Ingest(context.Background(), r); err != nil {
			t.Fatal("source request ingestion failed")
		}
	}
}
func performanceE2EAccounting(t *testing.T, store *telemetry.Store) []float64 {
	t.Helper()
	out := make([]float64, 8)
	err := store.DB().QueryRow(`SELECT COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(cache_write_tokens),0),COALESCE(SUM(reasoning_tokens),0),COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COUNT(*) FROM usage_events`).Scan(&out[0], &out[1], &out[2], &out[3], &out[4], &out[5], &out[6], &out[7])
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func performanceE2EHub(t *testing.T) (string, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	const token = "synthetic-isolated-performance-token"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.NewServerWithAuth(addr, hub.NewStore(time.Minute), token).ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("isolated Hub shutdown failed")
			}
		case <-time.After(6 * time.Second):
			t.Error("isolated Hub shutdown timed out")
		}
	})
	endpoint := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(endpoint + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated Hub readiness failed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	response, err := http.Get(endpoint + "/v1/snapshots")
	if err != nil {
		t.Fatal("isolated Hub auth probe failed")
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("isolated Hub accepted missing authentication")
	}
	return endpoint, token
}
func performanceE2EPush(t *testing.T, endpoint, token string, snaps map[string]core.UsageSnapshot, want int) map[string]core.UsageSnapshot {
	t.Helper()
	e, err := exporter.New(config.ExportConfig{Target: endpoint, MachineName: "synthetic-performance", AuthToken: token, IntervalSeconds: 1})
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
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/snapshots", nil)
		if err != nil {
			t.Fatal("isolated query construction failed")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("isolated Hub query failed")
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024))
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatal("isolated Hub query response failed")
		}
		var received map[string]core.UsageSnapshot
		if json.Unmarshal(body, &received) != nil {
			t.Fatal("isolated Hub JSON decode failed")
		}
		if len(received) == want {
			return received
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated Hub did not receive expected snapshot count")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
