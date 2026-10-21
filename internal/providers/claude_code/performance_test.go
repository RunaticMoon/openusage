package claude_code

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
	"github.com/janekbaraniewski/openusage/internal/performance"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

func claudeStart(ref, session, at string) string {
	return fmt.Sprintf(`{"type":"api-request","id":%q,"sessionId":%q,"timestamp":%q,"params":{"prompt":"PRIVATE_PERFORMANCE_SENTINEL"}}`+"\n", ref, session, at)
}
func claudeResponse(ref, request, message, at, output, stop string) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":"session","requestRef":%q,"requestId":%q,"timestamp":%q,"message":{"id":%q,"model":"model","stop_reason":%s,"usage":{"input_tokens":100,"output_tokens":%s,"reasoning_tokens":90},"content":[{"type":"text","text":"PRIVATE_PERFORMANCE_SENTINEL"}]}}`+"\n", ref, request, at, message, stop, output)
}
func writePerformanceLog(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeExactJoinFinalCorrectionsAndAccountingInvariant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := claudeStart("native-start", "session", "2026-10-21T00:00:00Z") +
		claudeResponse("native-start", "request", "message", "2026-10-21T00:00:01Z", "180", "null") +
		claudeResponse("native-start", "request", "message", "2026-10-21T00:00:02Z", "150", `"end_turn"`)
	writePerformanceLog(t, path, data)
	off, _ := ParseTelemetryConversationFile(path)
	on, _, err := parsePerformanceConversationFile(path, 0, newClaudePerformanceState(), true)
	if err != nil || len(on) != 1 || on[0].Performance == nil {
		t.Fatal("no performance request")
	}
	r := performance.NormalizeRequest(*on[0].Performance)
	if r.ResponseTokS == nil || *r.ResponseTokS != 75 || *r.OutputTokens != 150 || r.TimingBasis != core.TimingInferredLogBoundary || r.ID != "request" {
		t.Fatalf("bad request: %+v", r)
	}
	if r.BillingProviderID != "unknown" {
		t.Fatal("model vendor was guessed as billing provider")
	}
	if *on[0].OutputTokens != 180 {
		t.Fatal("performance correction changed existing accounting MAX")
	}
	on[0].Performance = nil
	if !reflect.DeepEqual(off, on) {
		t.Fatal("performance altered accounting event")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "PRIVATE_PERFORMANCE_SENTINEL") || strings.Contains(string(b), path) {
		t.Fatal("private provenance leaked")
	}
}

func TestClaudePerformanceSameTimestampFinalityAndUnorderedCorrection(t *testing.T) {
	for _, tc := range []struct {
		name, observations string
	}{
		{"partial_then_final", claudeResponse("a", "r", "m", "2026-10-21T00:00:02Z", "180", "null") + claudeResponse("a", "r", "m", "2026-10-21T00:00:02Z", "150", `"end_turn"`)},
		{"final_then_delayed_partial", claudeResponse("a", "r", "m", "2026-10-21T00:00:02Z", "150", `"end_turn"`) + claudeResponse("a", "r", "m", "2026-10-21T00:00:02Z", "180", "null")},
		{"equal_time_final_correction", claudeResponse("a", "r", "m", "2026-10-21T00:00:02Z", "150", `"end_turn"`) + claudeResponse("a", "r", "m", "2026-10-21T00:00:02Z", "100", `"end_turn"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			writePerformanceLog(t, path, claudeStart("a", "session", "2026-10-21T00:00:00Z")+tc.observations)
			events, _, err := parsePerformanceConversationFile(path, 0, newClaudePerformanceState(), true)
			if err != nil || len(events) != 1 || events[0].Performance == nil {
				t.Fatal("missing finalized observation")
			}
			r := performance.NormalizeRequest(*events[0].Performance)
			if !r.Final || r.OutputTokens == nil || *r.OutputTokens != 150 || r.ResponseTokS == nil || *r.ResponseTokS != 75 {
				t.Fatal("same-time partial regressed final or unordered final replaced it")
			}
		})
	}
}

func TestClaudePerformanceAmbiguityMissingZeroAndGuardrails(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, ref, output, stop, end string
		want                                 *float64
		status                               core.PerformanceTimingStatus
	}{
		{"zero", claudeStart("a", "session", "2026-10-21T00:00:00Z"), "a", "0", `"end_turn"`, "2026-10-21T00:00:02Z", core.Float64Ptr(0), core.TimingValid},
		{"missing_output", claudeStart("a", "session", "2026-10-21T00:00:00Z"), "a", "null", `"end_turn"`, "2026-10-21T00:00:02Z", nil, core.TimingMissing},
		{"no_join", `{"type":"user","timestamp":"2026-10-21T00:00:00Z"}` + "\n", "a", "150", `"end_turn"`, "2026-10-21T00:00:02Z", nil, core.TimingUnsupported},
		{"parallel_exact", claudeStart("a", "session", "2026-10-21T00:00:00Z") + claudeStart("b", "session", "2026-10-21T00:00:01Z"), "a", "150", `"tool_use"`, "2026-10-21T00:00:02Z", core.Float64Ptr(75), core.TimingValid},
		{"conflict", claudeStart("a", "session", "2026-10-21T00:00:00Z") + claudeStart("a", "session", "2026-10-21T00:00:01Z"), "a", "150", `"end_turn"`, "2026-10-21T00:00:02Z", nil, core.TimingAmbiguous},
		{"wrong_session", claudeStart("a", "other", "2026-10-21T00:00:00Z"), "a", "150", `"end_turn"`, "2026-10-21T00:00:02Z", nil, core.TimingAmbiguous},
		{"partial", claudeStart("a", "session", "2026-10-21T00:00:00Z"), "a", "150", "null", "2026-10-21T00:00:02Z", nil, core.TimingProvisional},
		{"sleep", claudeStart("a", "session", "2026-10-21T00:00:00Z"), "a", "150", `"end_turn"`, "2026-10-21T01:00:00Z", nil, core.TimingInvalid},
		{"too_short", claudeStart("a", "session", "2026-10-21T00:00:00Z"), "a", "150", `"end_turn"`, "2026-10-21T00:00:00.050Z", nil, core.TimingInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "session.jsonl")
			writePerformanceLog(t, p, tc.prefix+claudeResponse(tc.ref, "r", "m", tc.end, tc.output, tc.stop))
			e, _, err := parsePerformanceConversationFile(p, 0, newClaudePerformanceState(), true)
			if err != nil || len(e) != 1 || e[0].Performance == nil {
				t.Fatal("missing observation")
			}
			r := performance.NormalizeRequest(*e[0].Performance)
			if r.TimingStatus != tc.status || !reflect.DeepEqual(r.ResponseTokS, tc.want) {
				t.Fatalf("got status=%s speed=%v", r.TimingStatus, r.ResponseTokS)
			}
		})
	}
}

func TestClaudePerformanceFailureAndAccountingPrefixPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := claudeStart("a", "session", "2026-10-21T00:00:00Z") +
		strings.Replace(claudeResponse("a", "r", "m", "2026-10-21T00:00:02Z", "150", `"end_turn"`), `"type":"assistant"`, `"type":"assistant","isApiErrorMessage":true`, 1)
	// Preserve the old parser's successfully decoded prefix on oversized input.
	data += strings.Repeat("x", 10*1024*1024+1) + "\n"
	writePerformanceLog(t, path, data)
	off, _ := ParseTelemetryConversationFile(path)
	on, _, err := parsePerformanceConversationFile(path, 0, newClaudePerformanceState(), true)
	if err != nil || len(on) != 1 || on[0].Performance == nil {
		t.Fatal("valid prefix was discarded")
	}
	r := performance.NormalizeRequest(*on[0].Performance)
	if !r.Final || r.Outcome != core.OutcomeFailed || r.ResponseTokS != nil {
		t.Fatal("failed response counted as a measured success")
	}
	on[0].Performance = nil
	if !reflect.DeepEqual(off, on) {
		t.Fatal("performance option changed valid prefix accounting")
	}
}

func TestClaudePerformanceIncrementalPartialRotationAndFlagCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	start := claudeStart("a", "session", "2026-10-21T00:00:00Z")
	writePerformanceLog(t, path, start)
	opts := shared.TelemetryCollectOptions{PerformanceEnabled: true, Paths: map[string]string{"projects_dir": dir, "alt_projects_dir": t.TempDir()}}
	p := New()
	if e, err := p.Collect(context.Background(), opts); err != nil || len(e) != 0 {
		t.Fatal("start created usage")
	}
	response := claudeResponse("a", "r", "m", "2026-10-21T00:00:02Z", "150", `"end_turn"`)
	writePerformanceLog(t, path, start+strings.TrimSuffix(response, "\n"))
	e, err := p.Collect(context.Background(), opts)
	if err != nil || len(e) != 1 || e[0].Performance != nil {
		t.Fatal("unterminated line finalized timing")
	}
	writePerformanceLog(t, path, start+response)
	e, err = p.Collect(context.Background(), opts)
	if err != nil || len(e) != 1 || e[0].Performance == nil {
		t.Fatal("completed append missed exact start")
	}
	for i := 0; i < 10; i++ {
		e, err = p.Collect(context.Background(), opts)
		if err != nil || len(e) != 0 {
			t.Fatal("unchanged file replayed")
		}
	}
	// Replacement inode and same byte count cannot inherit the previous join.
	old, _ := os.Stat(path)
	replacement := filepath.Join(dir, "replacement.tmp")
	writePerformanceLog(t, replacement, strings.ReplaceAll(start+response, `"r"`, `"q"`))
	if err := os.Chtimes(replacement, old.ModTime(), old.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	e, err = p.Collect(context.Background(), opts)
	if err != nil || len(e) != 1 || e[0].Performance.ID != "q" {
		t.Fatal("rotation hidden by size cache")
	}
	// Truncation must drop retained starts; no old request boundary is reused.
	writePerformanceLog(t, path, response)
	_ = os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second))
	e, err = p.Collect(context.Background(), opts)
	if err != nil || len(e) != 1 || e[0].Performance.StartedAt != nil {
		t.Fatal("truncated file reused old start")
	}
	plain := New()
	opts.PerformanceEnabled = false
	if _, err = plain.Collect(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	opts.PerformanceEnabled = true
	e, err = plain.Collect(context.Background(), opts)
	if err != nil || len(e) != 1 || e[0].Performance == nil {
		t.Fatal("off cache suppressed opt-in")
	}
}
