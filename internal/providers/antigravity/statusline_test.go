package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

func TestStatusLineObservationsNeverBecomeRequests(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(sampleStatusLineJSON), &payload); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	opts := shared.TelemetryCollectOptions{Paths: map[string]string{
		"status_file": path, "transcripts_dir": t.TempDir(),
	}}
	p := New()
	for _, state := range []string{"idle", "thinking", "working", "tool_use", "working", "working"} {
		payload["agent_state"] = state
		assertStatusLineNotRequest(t, p, payload, path, opts)
	}
	// An identical count can represent multiple requests, while a revision
	// can represent one unfinished request. Neither proves request identity.
	cw := payload["context_window"].(map[string]any)
	current := cw["current_usage"].(map[string]any)
	for _, tokens := range []int{40, 41, 40} {
		current["output_tokens"] = tokens
		cw["total_output_tokens"] = tokens + 300
		assertStatusLineNotRequest(t, p, payload, path, opts)
	}
	payload["conversation_id"] = "another-conversation"
	payload["session_id"] = "another-session"
	assertStatusLineNotRequest(t, p, payload, path, opts)
}

func assertStatusLineNotRequest(t *testing.T, p *Provider, payload map[string]any, path string, opts shared.TelemetryCollectOptions) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureStatusLine(raw, path); err != nil {
		t.Fatal(err)
	}
	if events, err := p.ParseHookPayload(raw, opts); err != nil || len(events) != 0 {
		t.Fatalf("hook observations created history: count=%d, error=%v", len(events), err)
	}
	if events, err := p.Collect(context.Background(), opts); err != nil || len(events) != 0 {
		t.Fatalf("status file created history: count=%d, error=%v", len(events), err)
	}
	snap, err := p.Fetch(context.Background(), core.AccountConfig{ID: "profile", ProviderPaths: opts.Paths})
	if err != nil {
		t.Fatal(err)
	}
	want := payload["context_window"].(map[string]any)["current_usage"].(map[string]any)["output_tokens"]
	var number float64
	switch n := want.(type) {
	case int:
		number = float64(n)
	case float64:
		number = n
	}
	if got := metricUsed(t, snap, "current_output_tokens"); got != number {
		t.Fatalf("latest output = %v, want %v", got, number)
	}
}

func TestStatusLineCapturePrivacyAndProfileIsolation(t *testing.T) {
	root := t.TempDir()
	paths := []string{filepath.Join(root, "one", "state.json"), filepath.Join(root, "two", "state.json")}
	private := []byte(`{"email":"private@example.invalid","token":"secret-marker","prompt":"prompt-marker","transcript_path":"/private/transcript-marker","cwd":"/private/root/project","workspace":{"project_dir":"/private/root/project","secret":"nested-marker"},"conversation_id":"conversation-one","context_window":{"current_usage":{"input_tokens":7,"unknown":"hidden-marker"}}}`)
	if _, err := CaptureStatusLine(private, paths[0]); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureStatusLine([]byte(`{"conversation_id":"conversation-two","context_window":{"current_usage":{"input_tokens":19}}}`), paths[1]); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(paths[0])
	if err != nil || !bytes.Equal(first, again) {
		t.Fatalf("second profile changed first profile: %v", err)
	}
	for i, path := range paths {
		acct := core.AccountConfig{ID: []string{"profile-one", "profile-two"}[i], ProviderPaths: map[string]string{"status_file": path}}
		snap, err := New().Fetch(context.Background(), acct)
		if err != nil {
			t.Fatal(err)
		}
		if snap.AccountID != acct.ID || metricUsed(t, snap, "current_input_tokens") != []float64{7, 19}[i] {
			t.Fatalf("profile state crossed: account=%s", snap.AccountID)
		}
		encoded, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range []string{"private@example.invalid", "secret-marker", "prompt-marker", "transcript-marker", "nested-marker", "hidden-marker", "/private/root", path} {
			if bytes.Contains(encoded, []byte(marker)) || bytes.Contains(first, []byte(marker)) {
				t.Fatalf("private marker retained: %q", marker)
			}
		}
	}
	// Previously captured unsanitized files also must not export private fields.
	if err := os.WriteFile(paths[0], private, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := New().Fetch(context.Background(), core.AccountConfig{ProviderPaths: map[string]string{"status_file": paths[0]}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(snap)
	for _, marker := range []string{"private@example.invalid", "secret-marker", "prompt-marker", "transcript-marker", "/private/root"} {
		if bytes.Contains(encoded, []byte(marker)) {
			t.Fatalf("legacy file exposed private marker: %q", marker)
		}
	}
}

func TestStatusLineFreshnessUsesCaptureTimeOrLegacyMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	acct := core.AccountConfig{ProviderPaths: map[string]string{"status_file": path}}
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, withTimestamp := range []bool{false, true} {
		payload := map[string]any{"quota": map[string]any{"pro": map[string]any{"remaining_fraction": 0.1, "reset_in_seconds": 3600}}}
		mtime := old
		if withTimestamp {
			payload["received_at"] = old.Format(time.RFC3339)
			mtime = time.Now()
		}
		raw, _ := json.Marshal(payload)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			snap, err := New().Fetch(context.Background(), acct)
			if err != nil || !snap.Timestamp.Equal(old) {
				t.Fatalf("old observation became fresh: %v, %v", snap.Timestamp, err)
			}
			if _, ok := snap.Metrics["quota"]; ok {
				t.Fatal("poll renewed a relative quota reset")
			}
		}
	}
	before := time.Now().UTC()
	if _, err := CaptureStatusLine([]byte(`{"received_at":"2099-01-01T00:00:00Z"}`), path); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	snap, err := New().Fetch(context.Background(), acct)
	if err != nil || snap.Timestamp.Before(before) || snap.Timestamp.After(after) {
		t.Fatalf("capture timestamp not recorded: %v, %v", snap.Timestamp, err)
	}
}

func TestInvalidStatusLineDoesNotReplaceSavedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if _, err := CaptureStatusLine([]byte(sampleStatusLineJSON), path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for _, invalid := range []string{"null", "", "{", "[]", `{"context_window":{"current_usage":{"input_tokens":"wrong"}}}`} {
		if _, err := CaptureStatusLine([]byte(invalid), path); err == nil {
			t.Fatalf("accepted malformed payload %q", invalid)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid payload changed saved state")
		}
	}
}
