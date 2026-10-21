package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/performance"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

const codexPerformanceFixture = `{"timestamp":"2026-10-21T00:00:00Z","type":"session_meta","payload":{"id":"session","model":"model","model_provider":"openai"}}
{"timestamp":"2026-10-21T00:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"timestamp":"2026-10-21T00:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"output_tokens":150,"reasoning_output_tokens":100,"total_tokens":160},"last_token_usage":{"input_tokens":10,"output_tokens":150,"reasoning_output_tokens":100,"total_tokens":160}}}}
{"timestamp":"2026-10-21T00:00:02Z","type":"token_usage_record","payload":{"response_id":"response","session_id":"session","turn_id":"turn","usage":{"output_tokens":150,"reasoning_output_tokens":100},"turn_token_usage":{"output_tokens":1500},"thread_token_usage":{"output_tokens":15000},"unrelated":"PRIVATE_PERFORMANCE_SENTINEL"}}
{"timestamp":"2026-10-21T00:00:03Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn","duration_ms":3000,"time_to_first_token_ms":100,"last_agent_message":"PRIVATE_PERFORMANCE_SENTINEL"}}
`

func TestCodexPerformanceNativeIdentityWithoutInventedTimingOrAccounting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(codexPerformanceFixture), 0600); err != nil {
		t.Fatal(err)
	}
	off, err := ParseTelemetrySessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	on, err := ParseTelemetrySessionFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	var accounting []shared.TelemetryEvent
	var requests []core.PerformanceRequest
	for _, e := range on {
		if e.PerformanceOnly {
			if e.Performance == nil {
				t.Fatal("missing metadata")
			}
			requests = append(requests, performance.NormalizeRequest(*e.Performance))
		} else {
			accounting = append(accounting, e)
		}
	}
	if !reflect.DeepEqual(off, accounting) {
		t.Fatal("opt-in changed established token_count accounting")
	}
	if len(requests) != 1 {
		t.Fatalf("requests=%d", len(requests))
	}
	r := requests[0]
	if r.ID != "response" || r.OutputTokens == nil || *r.OutputTokens != 150 || r.ResponseTokS != nil || r.StartedAt != nil || r.DurationMS != nil || r.Outcome != core.OutcomeUnknown || !r.Final {
		t.Fatalf("invented request success/timing or used cumulative/reasoning tokens: %+v", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "PRIVATE_PERFORMANCE_SENTINEL") || strings.Contains(string(b), path) {
		t.Fatal("private source content/path leaked")
	}
}

func TestCodexPerformanceFlagCacheRetryAndCorrectionIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(codexPerformanceFixture), 0600); err != nil {
		t.Fatal(err)
	}
	p := New()
	opts := shared.TelemetryCollectOptions{Paths: map[string]string{"sessions_dir": dir, "account_id": "personal"}}
	if _, err := p.Collect(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	opts.PerformanceEnabled = true
	e, err := p.Collect(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, v := range e {
		if v.PerformanceOnly {
			count++
		}
	}
	if count != 1 {
		t.Fatal("off cache suppressed metadata")
	}
	for i := 0; i < 10; i++ {
		e, err = p.Collect(context.Background(), opts)
		if err != nil || len(e) != 0 {
			t.Fatal("cached replay")
		}
	}
	appendLine := `{"timestamp":"2026-10-21T00:00:04Z","type":"token_usage_record","payload":{"response_id":"response","session_id":"session","usage":{"output_tokens":100}}}` + "\n" +
		`{"timestamp":"2026-10-21T00:00:05Z","type":"token_usage_record","payload":{"response_id":"retry","session_id":"session","usage":{"output_tokens":0}}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(appendLine)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	e, err = p.Collect(context.Background(), opts)
	if err != nil || len(e) != 2 {
		t.Fatalf("incremental events=%d err=%v", len(e), err)
	}
	if !e[0].PerformanceOnly || !e[1].PerformanceOnly || e[0].Performance.ID != "response" || *e[0].Performance.OutputTokens != 100 || e[1].Performance.ID != "retry" || *e[1].Performance.OutputTokens != 0 {
		t.Fatal("correction/retry identity conflated")
	}
}
