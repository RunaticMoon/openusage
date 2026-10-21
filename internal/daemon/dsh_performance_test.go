package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/dsh"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

// Exercise opt-in native parsing through account-scoped storage, a process
// restart and the authenticated Exporter/Hub path. All inputs are synthetic.
func TestDSHPerformanceAccountingReplayRestartAndHub(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Minute).UnixMilli()
	root := t.TempDir()
	session := filepath.Join(root, "synthetic-session")
	if err := os.Mkdir(session, 0700); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	fmt.Fprintf(&log, `{"type":"session","version":4,"id":"synthetic-session","createdAt":%d,"isSeeded":false,"delegationDepth":0}`+"\n", start)
	seq := 0
	appendRow := func(kind string, at int64, data any) {
		t.Helper()
		row, err := json.Marshal(map[string]any{"type": kind, "seq": seq, "time": at, "data": data})
		if err != nil {
			t.Fatal(err)
		}
		seq++
		log.Write(row)
		log.WriteByte('\n')
	}
	for i := range 2 {
		at := start + int64(i)*10000
		appendRow("step/start", at, map[string]any{"turn": 1, "step": i})
		if i == 0 {
			appendRow("request/header", at, map[string]any{"reason": "initial", "private": "PRIVATE_DSH_TIMING_BODY"})
		}
		output, reasoning := 150, 50
		if i == 1 {
			output, reasoning = 30, 10
		}
		appendRow("assistant/message", at+2100, map[string]any{
			"turn": 1, "step": i,
			"message": map[string]any{"source": map[string]any{"provider": "hive", "model": "synthetic-model"}},
			"usage":   map[string]any{"inputTokens": 10, "outputTokens": output, "reasoningTokens": reasoning, "cacheReadTokens": 20},
			"stream": []any{
				map[string]any{"type": "text-chunks", "time0": at + 1000, "dt": []int{50}, "texts": []string{"PRIVATE_DSH_TIMING_BODY", "PRIVATE_DSH_TIMING_BODY"}},
				map[string]any{"type": "chunk", "time": at + 2000, "chunk": map[string]any{"type": "finish", "reason": map[string]any{"kind": "stop"}}},
			},
		})
		appendRow("step/end", at+2200, map[string]any{"turn": 1, "step": i})
	}
	if err := os.WriteFile(filepath.Join(session, "session.v4.jsonl"), []byte(log.String()), 0600); err != nil {
		t.Fatal(err)
	}
	off, offPath := performanceE2EStore(t, false)
	onPath := filepath.Join(t.TempDir(), "telemetry.db")
	on, err := telemetry.OpenStore(onPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = on.Close() }()
	accounts := []string{"synthetic-personal-one", "synthetic-personal-two"}
	base := map[string]core.UsageSnapshot{}
	for _, account := range accounts {
		base[account] = core.NewUsageSnapshot("dsh", account)
		requests, err := telemetry.NewSourceCollector(dsh.New(), shared.TelemetryCollectOptions{
			Paths: map[string]string{"sessions_dir": root, "account_id": account},
		}, account).Collect(ctx)
		if err != nil || len(requests) != 2 {
			t.Fatalf("disabled source requests=%d err=%v", len(requests), err)
		}
		for _, request := range requests {
			if request.Performance != nil {
				t.Fatal("disabled source acquired performance metadata")
			}
		}
		performanceE2EIngest(t, off, requests)
		performanceE2EIngest(t, on, requests)
	}
	if !reflect.DeepEqual(performanceE2EAccounting(t, off), performanceE2EAccounting(t, on)) {
		t.Fatal("existing disabled ledger differs before performance backfill")
	}
	var performanceTables int
	if err := on.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'performance_requests'`).Scan(&performanceTables); err != nil || performanceTables != 0 {
		t.Fatalf("disabled store performance tables=%d want 0 err=%v", performanceTables, err)
	}
	read := func(path string, enabled bool) map[string]core.UsageSnapshot {
		t.Helper()
		snaps, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, path, base, telemetry.ReadModelOptions{
			TimeWindow: core.TimeWindowAll, PerformanceEnabled: enabled, Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return snaps
	}
	plain := read(offPath, false)
	var measured map[string]core.UsageSnapshot
	for replay := range 3 {
		if replay == 2 {
			if err := on.Close(); err != nil {
				t.Fatal(err)
			}
			on, err = telemetry.OpenStore(onPath)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := on.EnablePerformance(ctx); err != nil {
			t.Fatal(err)
		}
		for _, account := range accounts {
			requests, err := telemetry.NewSourceCollector(dsh.New(), shared.TelemetryCollectOptions{
				PerformanceEnabled: true, Paths: map[string]string{"sessions_dir": root, "account_id": account},
			}, account).Collect(ctx)
			if err != nil || len(requests) != 2 {
				t.Fatalf("enabled source requests=%d err=%v", len(requests), err)
			}
			performanceE2EIngest(t, on, requests)
			if err := on.RecordPerformanceScan(ctx, "dsh", account, now); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(performanceE2EAccounting(t, off), performanceE2EAccounting(t, on)) {
			t.Fatal("performance/replay/restart changed canonical accounting")
		}
		var rows int
		if err := on.DB().QueryRow(`SELECT COUNT(*) FROM performance_requests`).Scan(&rows); err != nil || rows != 4 {
			t.Fatalf("performance rows=%d want 4 err=%v", rows, err)
		}
		measured = read(onPath, true)
		for _, account := range accounts {
			without, with := plain[account], measured[account]
			if without.Performance != nil || with.Performance == nil {
				t.Fatal("opt-in projection contract changed")
			}
			if !reflect.DeepEqual(without.Metrics, with.Metrics) || !reflect.DeepEqual(without.DailySeries, with.DailySeries) || !reflect.DeepEqual(without.ModelUsage, with.ModelUsage) {
				t.Fatal("performance changed usage, Work/Cached or model totals")
			}
			s := with.Performance.Summary
			if s.Coverage.Candidates != 2 || s.Coverage.Measured != 1 || s.Coverage.Inferred != 1 || s.Coverage.Direct != 0 || s.Coverage.Reasons["request_header_missing"] != 1 {
				t.Fatalf("unexpected coverage: %+v", s.Coverage)
			}
			if s.Stats.OutputTokens != 150 || s.Stats.DurationMS != 2000 || s.Stats.WeightedResponseTokS == nil || *s.Stats.WeightedResponseTokS != 75 {
				t.Fatalf("unexpected numeric response speed: %+v", s.Stats)
			}
			foundVendor := false
			for _, group := range with.Performance.Groups {
				if group.Dimension == "billing_provider" && group.Key == "hive" {
					foundVendor = group.Summary.Coverage.Measured == 1 && group.Summary.Stats.OutputTokens == 150
				}
			}
			if !foundVendor {
				t.Fatal("DSH timing lost its reported billing provider")
			}
		}
	}
	endpoint, token := performanceE2EHub(t)
	received := performanceE2EPush(t, endpoint, token, measured, len(accounts))
	for _, account := range accounts {
		remote := received["synthetic-performance:dsh:"+account]
		if remote.Performance == nil || !performanceE2EJSONEqual(remote.Performance.Summary, measured[account].Performance.Summary) {
			t.Fatal("Exporter/Hub changed DSH account performance")
		}
	}
	payload, err := json.Marshal(received)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "PRIVATE_DSH_TIMING_BODY") || strings.Contains(string(payload), root) {
		t.Fatal("private source content or path reached the Hub")
	}
}

func TestDSHPerformanceRevisionCannotReuseAnEarlierInterval(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Minute).UnixMilli()
	root := t.TempDir()
	session := filepath.Join(root, "synthetic-session")
	if err := os.Mkdir(session, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(session, "session.v4.jsonl")
	settlement := func(seq, output int, elapsed int64) string {
		return fmt.Sprintf(`{"type":"assistant/message","seq":%d,"time":%d,"data":{"turn":1,"step":1,"message":{"source":{"provider":"hive","model":"synthetic-model"}},"usage":{"inputTokens":10,"outputTokens":%d},"stream":[{"type":"chunk","time":%d,"chunk":{"type":"finish","reason":{"kind":"stop"}}}]}}`+"\n", seq, start+elapsed+100, output, start+elapsed)
	}
	text := fmt.Sprintf(`{"type":"session","version":4,"id":"synthetic-session","createdAt":%d,"isSeeded":false,"delegationDepth":0}
{"type":"step/start","seq":0,"time":%d,"data":{"turn":1,"step":1}}
{"type":"request/header","seq":1,"time":%d,"data":{"reason":"initial"}}
`, start, start, start) + settlement(2, 150, 2000)
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	const account = "synthetic-personal"
	collect := func() []telemetry.IngestRequest {
		t.Helper()
		requests, err := telemetry.NewSourceCollector(dsh.New(), shared.TelemetryCollectOptions{
			PerformanceEnabled: true, Paths: map[string]string{"sessions_dir": root, "account_id": account},
		}, account).Collect(ctx)
		if err != nil || len(requests) != 1 {
			t.Fatalf("source settlement requests=%d err=%v", len(requests), err)
		}
		return requests
	}
	warmPath := filepath.Join(t.TempDir(), "telemetry.db")
	warm, err := telemetry.OpenStore(warmPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = warm.Close() }()
	if err := warm.EnablePerformance(ctx); err != nil {
		t.Fatal(err)
	}
	read := func(path string) core.UsageSnapshot {
		t.Helper()
		snaps, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, path,
			map[string]core.UsageSnapshot{account: core.NewUsageSnapshot("dsh", account)},
			telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll, PerformanceEnabled: true, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		return snaps[account]
	}
	initial := collect()
	performanceE2EIngest(t, warm, initial)
	first := read(warmPath)
	if first.Performance == nil || first.Performance.Summary.Coverage.Measured != 1 {
		t.Fatal("initial native request was not measured")
	}
	// Same accounting slot, changed settled output and endpoint, no fresh
	// request header. Prior timing cannot be reused with the new counters.
	if err := os.WriteFile(file, []byte(text+settlement(3, 300, 4000)), 0600); err != nil {
		t.Fatal(err)
	}
	updated := collect()
	if telemetry.BuildDedupKey(initial[0]) != telemetry.BuildDedupKey(updated[0]) {
		t.Fatal("settlement correction changed canonical accounting identity")
	}
	performanceE2EIngest(t, warm, updated)
	cold, coldPath := performanceE2EStore(t, true)
	performanceE2EIngest(t, cold, updated)
	for round := range 2 {
		if round == 1 {
			if err := warm.Close(); err != nil {
				t.Fatal(err)
			}
			warm, err = telemetry.OpenStore(warmPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := warm.EnablePerformance(ctx); err != nil {
				t.Fatal(err)
			}
			performanceE2EIngest(t, warm, initial) // Delayed old evidence cannot resurrect the rate.
			performanceE2EIngest(t, warm, updated)
		}
		got, fresh := read(warmPath), read(coldPath)
		if got.Performance == nil || fresh.Performance == nil || !performanceE2EJSONEqual(got.Performance.Summary, fresh.Performance.Summary) {
			t.Fatal("warm/reopened and cold collectors disagree after a settlement correction")
		}
		summary := got.Performance.Summary
		if summary.Coverage.Candidates != 1 || summary.Coverage.Measured != 0 || summary.Stats.WeightedResponseTokS != nil {
			t.Fatal("later output was divided by an earlier request interval")
		}
		accounting := performanceE2EAccounting(t, warm)
		if !reflect.DeepEqual(accounting, performanceE2EAccounting(t, cold)) || accounting[1] != 300 {
			t.Fatal("timing revocation lost the corrected accounting output")
		}
	}
}
