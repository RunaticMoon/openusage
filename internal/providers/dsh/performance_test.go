package dsh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/performance"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

const timingBase int64 = 1800000000000

func timedRow(kind string, offset int64, data map[string]any) map[string]any {
	r := row(kind, data)
	r["time"] = timingBase + offset
	return r
}

func timedLog(t *testing.T, seeded bool, rows ...map[string]any) []byte {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	if err := enc.Encode(map[string]any{"type": "session", "version": 4, "id": "synthetic-timing", "createdAt": timingBase, "isSeeded": seeded, "delegationDepth": 0}); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		r["seq"] = i
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

func timingRows() []map[string]any {
	return []map[string]any{
		timedRow("step/start", 0, map[string]any{"turn": 1, "step": 1}),
		timedRow("request/header", 100, map[string]any{"reason": "initial", "header": map[string]any{"private": "PRIVATE_CONFIG"}}),
		timedRow("assistant/message", 2200, map[string]any{
			"turn": 1, "step": 1, "usage": map[string]any{"inputTokens": 10, "outputTokens": 150, "reasoningTokens": 50},
			"message": map[string]any{"source": map[string]any{"provider": "synthetic-vendor", "model": "synthetic-model"}, "content": "PRIVATE_TEXT"},
			"stream": []any{
				map[string]any{"type": "reasoning-chunks", "time0": timingBase + 1100, "dt": []any{50}, "texts": []any{"PRIVATE_REASONING", "PRIVATE_REASONING"}},
				map[string]any{"type": "text-chunks", "time0": timingBase + 1200, "dt": []any{50}, "texts": []any{"PRIVATE_TEXT", "PRIVATE_TEXT"}},
				map[string]any{"type": "chunk", "time": timingBase + 2100, "chunk": map[string]any{"type": "finish", "reason": map[string]any{"kind": "stop"}}},
			},
		}),
	}
}

func TestPerformanceRequestHeaderFinishAndAccountingInvariant(t *testing.T) {
	ctx := context.Background()
	data := timedLog(t, false, timingRows()...)
	_, off, err := parseLog(ctx, data, "synthetic-account")
	if err != nil {
		t.Fatal(err)
	}
	_, on, err := parseLogWithPerformance(ctx, data, "synthetic-account", true)
	if err != nil || len(on) != 1 || on[0].Performance == nil {
		t.Fatalf("performance missing: %v", err)
	}
	r := performance.NormalizeRequest(*on[0].Performance)
	if r.ResponseTokS == nil || *r.ResponseTokS != 75 || *r.OutputTokens != 150 || r.TimingBasis != core.TimingInferredLogBoundary || r.Outcome != core.OutcomeSuccess || *r.DurationMS != 2000 {
		t.Fatalf("unexpected throughput: %+v", r)
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "PRIVATE_") {
		t.Fatal("private native payload retained")
	}
	on[0].Performance = nil
	if !reflect.DeepEqual(off, on) {
		t.Fatal("enabling performance changed usage accounting or identity")
	}
}

func TestPerformanceMissingFailedAndInvalidEvidence(t *testing.T) {
	cases := []struct {
		name string
		edit func([]map[string]any) []map[string]any
	}{
		{"header absent", func(r []map[string]any) []map[string]any { return append(r[:1], r[2:]...) }},
		{"step absent", func(r []map[string]any) []map[string]any { return r[1:] }},
		{"scope mismatch", func(r []map[string]any) []map[string]any { r[2]["data"].(map[string]any)["step"] = 2; return r }},
		{"duplicate header", func(r []map[string]any) []map[string]any { return append(r[:2], r[1], r[2]) }},
		{"unknown header reason", func(r []map[string]any) []map[string]any {
			r[1]["data"].(map[string]any)["reason"] = "unknown"
			return r
		}},
		{"interrupted", func(r []map[string]any) []map[string]any {
			r[2]["data"].(map[string]any)["interrupted"] = true
			return r
		}},
		{"no finish", func(r []map[string]any) []map[string]any {
			d := r[2]["data"].(map[string]any)
			d["stream"] = d["stream"].([]any)[:2]
			return r
		}},
		{"duplicate finish", func(r []map[string]any) []map[string]any {
			d := r[2]["data"].(map[string]any)
			s := d["stream"].([]any)
			d["stream"] = append(s, s[2])
			return r
		}},
		{"output after finish", func(r []map[string]any) []map[string]any {
			d := r[2]["data"].(map[string]any)
			s := d["stream"].([]any)
			d["stream"] = append(s, map[string]any{"type": "text-chunks", "time0": timingBase + 2150, "dt": []any{}, "texts": []any{"PRIVATE_LATE_TEXT"}})
			return r
		}},
		{"wrong timestamp units", func(r []map[string]any) []map[string]any { r[1]["time"] = int64(1800000000); return r }},
		{"negative compressed gap", func(r []map[string]any) []map[string]any {
			s := r[2]["data"].(map[string]any)["stream"].([]any)
			s[0].(map[string]any)["dt"] = []any{-1}
			return r
		}},
		{"fractional stream time", func(r []map[string]any) []map[string]any {
			s := r[2]["data"].(map[string]any)["stream"].([]any)
			s[2].(map[string]any)["time"] = 1800000002100.5
			return r
		}},
		{"finish after settlement", func(r []map[string]any) []map[string]any { r[2]["time"] = timingBase + 2050; return r }},
		{"chunk before header", func(r []map[string]any) []map[string]any {
			s := r[2]["data"].(map[string]any)["stream"].([]any)
			s[0].(map[string]any)["time0"] = timingBase
			return r
		}},
		{"inference sleep guard", func(r []map[string]any) []map[string]any {
			r[1]["time"] = timingBase - 1000000
			r[0]["time"] = timingBase - 1000001
			return r
		}},
	}
	for _, finish := range []string{"error", "aborted", "unknown"} {
		finish := finish
		cases = append(cases, struct {
			name string
			edit func([]map[string]any) []map[string]any
		}{finish, func(r []map[string]any) []map[string]any {
			s := r[2]["data"].(map[string]any)["stream"].([]any)
			s[2].(map[string]any)["chunk"].(map[string]any)["reason"] = map[string]any{"kind": finish}
			return r
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := timedLog(t, false, tc.edit(timingRows())...)
			_, off, err := parseLog(context.Background(), data, "test")
			if err != nil {
				t.Fatal(err)
			}
			_, on, err := parseLogWithPerformance(context.Background(), data, "test", true)
			if err != nil || len(on) != 1 || on[0].Performance == nil {
				t.Fatalf("missing candidate: %v", err)
			}
			r := performance.NormalizeRequest(*on[0].Performance)
			if r.ResponseTokS != nil {
				t.Fatalf("invalid evidence measured: %+v", r)
			}
			on[0].Performance = nil
			if !reflect.DeepEqual(off, on) {
				t.Fatal("invalid timing changed accounting")
			}
		})
	}
}

func TestPerformanceNoHeaderReuseAfterSettlementRetryRestoreOrSeed(t *testing.T) {
	for _, boundary := range []string{"assistant/message", "assistant/attempt", "llm/retry-started", "session/end-seed", "step/end", "turn/end", "compaction/start"} {
		t.Run(boundary, func(t *testing.T) {
			r := timingRows()
			b := timedRow(boundary, 200, map[string]any{"turn": 1, "step": 1})
			r = append(r[:2], b, r[2])
			_, events, err := parseLogWithPerformance(context.Background(), timedLog(t, false, r...), "test", true)
			if err != nil || len(events) != 1 {
				t.Fatalf("events=%d err=%v", len(events), err)
			}
			p := performance.NormalizeRequest(*events[0].Performance)
			if p.ResponseTokS != nil {
				t.Fatal("consumed header reused")
			}
		})
	}
	rows := timingRows()
	rows = append(rows, timedRow("session/end-seed", 2300, map[string]any{"inherited": true}))
	live := timingRows()[2]
	live["time"] = timingBase + 2400
	rows = append(rows, live)
	_, events, err := parseLogWithPerformance(context.Background(), timedLog(t, true, rows...), "test", true)
	if err != nil || len(events) != 1 || events[0].Performance.StartedAt != nil {
		t.Fatal("seed timing leaked")
	}
}

func TestPerformanceExplicitZeroAndReplayBackfill(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.v4.jsonl")
	rows := timingRows()
	rows[2]["data"].(map[string]any)["usage"] = map[string]any{"inputTokens": 0, "outputTokens": 0}
	if err := os.WriteFile(path, timedLog(t, false, rows...), 0600); err != nil {
		t.Fatal(err)
	}
	opts := shared.TelemetryCollectOptions{Paths: map[string]string{"sessions_dir": root, "account_id": "test"}}
	off, err := New().Collect(context.Background(), opts)
	if err != nil || len(off) != 1 || off[0].Performance != nil {
		t.Fatalf("disabled collection: %v", err)
	}
	opts.PerformanceEnabled = true
	var previous []shared.TelemetryEvent
	for i := 0; i < 10; i++ {
		events, err := New().Collect(context.Background(), opts)
		if err != nil || len(events) != 1 {
			t.Fatalf("replay: %v", err)
		}
		p := performance.NormalizeRequest(*events[0].Performance)
		if p.ResponseTokS == nil || *p.ResponseTokS != 0 {
			t.Fatal("explicit zero not measured")
		}
		if i > 0 && !reflect.DeepEqual(previous, events) {
			t.Fatal("restart/replay changed metadata")
		}
		previous = events
	}
	previous[0].Performance = nil
	if !reflect.DeepEqual(off, previous) {
		t.Fatal("backfill changed old accounting")
	}
}

func TestPerformanceReplacementRevokesInferenceAndRetryHasDistinctIdentity(t *testing.T) {
	rows := timingRows()
	_, first, err := parseLogWithPerformance(context.Background(), timedLog(t, false, rows...), "test", true)
	if err != nil {
		t.Fatal(err)
	}
	changed := timingRows()[2]
	changed["time"] = timingBase + 4200
	changed["data"].(map[string]any)["usage"] = map[string]any{"inputTokens": 10, "outputTokens": 300}
	rows = append(rows, changed)
	_, events, err := parseLogWithPerformance(context.Background(), timedLog(t, false, rows...), "test", true)
	if err != nil || len(events) != 1 {
		t.Fatalf("replacement: %v", err)
	}
	p := events[0].Performance
	if p.ID != first[0].Performance.ID || *p.OutputTokens != 300 || p.Revision <= first[0].Performance.Revision || p.TimingStatus != core.TimingAmbiguous || p.StartedAt != nil {
		t.Fatal("replacement did not revoke the original inference")
	}
	rows = timingRows()
	rows = append(rows, timedRow("llm/retry-started", 2300, map[string]any{"turn": 1, "step": 1}), timedRow("request/header", 2400, map[string]any{"reason": "change"}))
	changed["data"].(map[string]any)["stream"] = []any{map[string]any{"type": "chunk", "time": timingBase + 4100, "chunk": map[string]any{"type": "finish", "reason": map[string]any{"kind": "tool-calls"}}}}
	rows = append(rows, changed)
	_, events, err = parseLogWithPerformance(context.Background(), timedLog(t, false, rows...), "test", true)
	if err != nil || len(events) != 2 || events[0].Performance.ID == events[1].Performance.ID {
		t.Fatal("retry identity collided")
	}
	if p := performance.NormalizeRequest(*events[1].Performance); p.ResponseTokS == nil {
		t.Fatal("fresh retry header not measured")
	}
}

// This explicit opt-in reads an already configured source root, never invokes
// DSH and never writes native files. Only aggregate numeric evidence is logged.
func TestPerformanceRealDSHReadOnly(t *testing.T) {
	root := os.Getenv("OPENUSAGE_PERFORMANCE_REAL_DSH_ROOT")
	if root == "" {
		t.Skip("requires an explicitly approved native DSH sessions root")
	}
	opts := shared.TelemetryCollectOptions{Paths: map[string]string{"sessions_dir": root, "account_id": "readonly-audit"}}
	off, err := New().Collect(context.Background(), opts)
	if err != nil {
		t.Fatal("native disabled collection failed")
	}
	opts.PerformanceEnabled = true
	on, err := New().Collect(context.Background(), opts)
	if err != nil {
		t.Fatal("native enabled collection failed")
	}
	if len(off) != len(on) {
		t.Fatal("native accounting event count changed")
	}
	type totals struct {
		Candidates int
		Measured   int
		Output     int64
		DurationMS float64
	}
	byProvider := map[string]*totals{}
	for i := range on {
		if p := on[i].Performance; p != nil {
			key := "other"
			switch p.BillingProviderID {
			case "deepseek", "openai", "anthropic":
				key = p.BillingProviderID
			}
			a := byProvider[key]
			if a == nil {
				a = &totals{}
				byProvider[key] = a
			}
			a.Candidates++
			n := performance.NormalizeRequest(*p)
			if n.ResponseTokS != nil {
				a.Measured++
				a.Output += *n.OutputTokens
				a.DurationMS += *n.DurationMS
			}
		}
		on[i].Performance = nil
		if !reflect.DeepEqual(off[i], on[i]) {
			t.Fatal("native accounting fields or event identity changed")
		}
	}
	data, err := json.Marshal(byProvider)
	if err != nil {
		t.Fatal("aggregate encoding failed")
	}
	t.Log(fmt.Sprintf("native accounting events=%d; performance aggregates=%s", len(on), data))
}
