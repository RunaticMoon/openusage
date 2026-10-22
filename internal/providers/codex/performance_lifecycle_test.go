package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/performance"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

func TestCodexLifecycleParserRequiresReviewedExplicitScope(t *testing.T) {
	at := time.Date(2026, 10, 21, 0, 0, 0, 123000000, time.UTC)
	for _, tc := range []struct {
		name, version, kind, turn, stamp string
		native                           bool
		want                             int
	}{
		{"complete", "0.163.0", "task_complete", "turn", at.Format(time.RFC3339Nano), true, 1},
		{"alias", "0.163.0", "turn_complete", "turn", at.Format(time.RFC3339Nano), true, 1},
		{"aborted", "0.163.0", "turn_aborted", "turn", at.Format(time.RFC3339Nano), true, 1},
		{"start", "0.163.0", "task_started", "turn", at.Format(time.RFC3339Nano), true, 0},
		{"unknown_version", "0.162.0", "task_complete", "turn", at.Format(time.RFC3339Nano), true, 0},
		{"no_turn_fallback", "0.163.0", "task_complete", "", at.Format(time.RFC3339Nano), true, 0},
		{"no_thread_fallback", "0.163.0", "task_complete", "turn", at.Format(time.RFC3339Nano), false, 0},
		{"bad_clock", "0.163.0", "task_complete", "turn", "not-time", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &telemetryParserState{cliVersion: tc.version, sessionID: "thread", currentTurnID: "fallback"}
			if tc.native {
				s.nativeThreadID = "thread"
			}
			collectCodexTurnTerminal(sessionLine{Timestamp: tc.stamp, EventPayload: &eventPayload{Type: tc.kind, TurnID: tc.turn}}, s)
			if len(s.nativeTerminals) != tc.want {
				t.Fatal("unverified terminal scope accepted or valid closure lost")
			}
			if tc.want == 1 && (!s.nativeTerminals[0].At.Equal(at) || s.nativeTerminals[0].ThreadID != "thread") {
				t.Fatal("terminal timestamp/scope changed")
			}
		})
	}
}

func TestCodexLifecycleUsesNativeChildThreadNotRootSession(t *testing.T) {
	for _, thread := range []string{"child-thread", ""} {
		state := &telemetryParserState{}
		applyTelemetrySessionMeta(state, &sessionMetaPayload{ID: thread, SessionID: "root-thread", CLIVersion: "0.163.0"})
		if state.sessionID != "root-thread" {
			t.Fatal("lifecycle correction changed existing accounting session association")
		}
		collectCodexTurnTerminal(sessionLine{Timestamp: nativeTestEpoch.Format(time.RFC3339Nano), EventPayload: &eventPayload{Type: "task_complete", TurnID: "child-turn"}}, state)
		if thread == "" {
			if len(state.nativeTerminals) != 0 {
				t.Fatal("root session alias supplied an unverified thread identity")
			}
		} else if len(state.nativeTerminals) != 1 || state.nativeTerminals[0].ThreadID != "child-thread" {
			t.Fatal("child terminal was assigned to its root thread")
		}
	}
}

func TestCodexLifecycleGlobalMergeBoundsAndDeterministicEviction(t *testing.T) {
	all := make([]nativeTurnTerminal, codexTimingPendingLimit*2+17)
	for i := range all {
		all[i] = nativeTurnTerminal{ThreadID: "thread", TurnID: fmt.Sprintf("turn-%05d", i), At: nativeTestEpoch.Add(time.Duration(i) * time.Second)}
	}
	// An oversized batch is internally chunked. Retention follows event time,
	// not the number/order of files or repeated records at file boundaries.
	want := mergeCodexTurnTerminals(nil, all)
	if len(want) != codexTimingPendingLimit || cap(want) > codexTimingPendingLimit ||
		want[0] != all[len(all)-1] || want[len(want)-1] != all[len(all)-codexTimingPendingLimit] {
		t.Fatal("global terminal retention bound or newest-event selection failed")
	}
	var forward, reverse []nativeTurnTerminal
	const batchSize = 997
	for start := 0; start < len(all); start += batchSize {
		end := min(start+batchSize, len(all))
		forward = mergeCodexTurnTerminals(forward, all[start:end])
		forward = mergeCodexTurnTerminals(forward, all[start:end])
		if len(forward) > codexTimingPendingLimit || cap(forward) > codexTimingPendingLimit {
			t.Fatal("per-file global merge exceeded its bound")
		}
	}
	for end := len(all); end > 0; {
		start := max(0, end-batchSize)
		reverse = mergeCodexTurnTerminals(reverse, all[start:end])
		end = start
	}
	if !slices.Equal(forward, want) || !slices.Equal(reverse, want) {
		t.Fatal("file ordering or repeated records changed retained lifecycle evidence")
	}
	// Equal instants in different locations deduplicate after UTC conversion.
	duplicate := all[len(all)-1]
	duplicate.At = duplicate.At.In(time.FixedZone("fixture", 3600))
	if got := mergeCodexTurnTerminals(want, []nativeTurnTerminal{duplicate, {ThreadID: "thread", TurnID: "", At: nativeTestEpoch}}); !slices.Equal(got, want) {
		t.Fatal("duplicate or invalid metadata displaced useful evidence")
	}
	// The enrichment cache uses the same bounded merge before matching; this
	// arbitrary directory has no implicit native DB path and causes no scan.
	p := New()
	dir := t.TempDir()
	if got := p.enrichNativeTiming(context.Background(), shared.TelemetryCollectOptions{}, dir, "personal", nil, all, all); len(got) != 0 {
		t.Fatal("lifecycle metadata alone fabricated usage")
	}
	cache := p.nativeTiming[codexTimingScope(dir, "personal", "")]
	if cache == nil || !slices.Equal(cache.terminals, want) || cap(cache.terminals) > codexTimingPendingLimit {
		t.Fatal("account cache did not preserve the global retention bound")
	}
}

func TestCodexLifecycleLatePartialTerminalRecoveryAndRestart(t *testing.T) {
	for _, kind := range []string{"task_complete", "turn_aborted"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "sessions")
			start := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
			end := start.Add(40 * time.Second)
			path := codexCollectorFixture(t, dir, "thread", "response", end, 3000)
			db := codexCollectorDB(t, filepath.Join(root, "logs_2.sqlite"))
			codexCollectorLog(t, db, start.Add(-2*time.Hour), "codex", "retention boundary")
			codexCollectorLog(t, db, start.Add(-time.Hour), "feedback_tags", `try_run_sampling_request{thread_id="thread" turn_id="old-turn"}: approval_policy="never" sandbox_policy="workspace" tags_json="{}"`)
			codexCollectorLog(t, db, start, "feedback_tags", `try_run_sampling_request{thread_id="thread" turn_id="turn"}: approval_policy="never" sandbox_policy="workspace" tags_json="{}"`)
			codexCollectorLog(t, db, end, "feedback_tags", `last_model_response_id="response"`)
			opts := shared.TelemetryCollectOptions{PerformanceEnabled: true, Paths: map[string]string{"sessions_dir": dir, "account_id": "personal", "baseline_existing": "false"}}
			p := New()
			events, err := p.Collect(context.Background(), opts)
			if err != nil || len(events) != 1 || events[0].Performance.ExclusionReason != "ambiguous_parallel_sampling" {
				t.Fatal("unclosed prior turn was not conservative")
			}
			appendText := func(text string) {
				t.Helper()
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString(text)
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatal("append fixture failed")
				}
			}
			terminal := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":%q,"turn_id":"old-turn","reason":"interrupted","duration_ms":999999999,"last_agent_message":"PRIVATE_LIFECYCLE_SENTINEL"}}`+"\n", start.Add(-30*time.Minute).Format(time.RFC3339Nano), kind)
			split := len(terminal) / 2
			appendText(terminal[:split])
			if got, err := p.Collect(context.Background(), opts); err != nil || len(got) != 0 {
				t.Fatal("partial terminal affected inference")
			}
			appendText(terminal[split:])
			events, err = p.Collect(context.Background(), opts)
			if err != nil || len(events) != 1 {
				t.Fatal("late closure did not enrich unchanged usage")
			}
			r := performance.NormalizeRequest(*events[0].Performance)
			if r.DurationMS == nil || *r.DurationMS != 40000 || r.TimingStatus != core.TimingValid || r.ResponseTokS == nil || *r.ResponseTokS != 75 || r.Outcome != core.OutcomeUnknown || !slices.Contains(r.Evidence, "codex_outcome_v2:unknown") {
				t.Fatal("closure invented request duration or success, or failed to recover")
			}
			for range 10 {
				if got, err := p.Collect(context.Background(), opts); err != nil || len(got) != 0 {
					t.Fatal("replay changed evidence")
				}
			}
			restarted, err := New().Collect(context.Background(), opts)
			if err != nil || len(restarted) != 1 || restarted[0].Performance.DurationMS == nil || *restarted[0].Performance.DurationMS != 40000 {
				t.Fatal("restart lost closure")
			}
			// A second profile has identical native IDs but no terminal records.
			other := filepath.Join(root, "company", "sessions")
			codexCollectorFixture(t, other, "thread", "response", end, 3000)
			opts.Paths = map[string]string{"sessions_dir": other, "codex_logs_db": filepath.Join(root, "logs_2.sqlite"), "account_id": "company", "baseline_existing": "false"}
			got, err := p.Collect(context.Background(), opts)
			if err != nil || len(got) != 1 || got[0].Performance.ExclusionReason != "ambiguous_parallel_sampling" {
				t.Fatal("closure leaked between configured profiles")
			}
		})
	}
}
