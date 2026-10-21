//go:build fork_integration

package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/config"
	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/exporter"
	"github.com/janekbaraniewski/openusage/internal/providers/dsh"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

// Exercise the actual parser, store correction, account projection and exporter
// against a temporary authenticated upstream Hub. No production data is used.
func TestDSHRevisionsAndAccountIsolationReachLatestHubSnapshot(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	sessionDir := filepath.Join(sourceDir, "synthetic-session")
	if err := os.Mkdir(sessionDir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sessionDir, "session.v4.jsonl")
	now := time.Now().UTC()
	header := fmt.Sprintf("{\"type\":\"session\",\"version\":4,\"id\":\"synthetic-session\",\"createdAt\":%d,\"cwd\":\"/SYNTHETIC_PRIVATE_PATH\",\"isSeeded\":false,\"delegationDepth\":0}\n", now.UnixMilli())
	writeRevision := func(seq, input int) {
		t.Helper()
		row := fmt.Sprintf(`{"type":"assistant/message","seq":%d,"time":%d,"data":{"turn":0,"step":0,"message":{"id":"synthetic-message","role":"assistant","content":[{"type":"text","text":"SYNTHETIC_PRIVATE_BODY"}],"source":{"kind":"llm","provider":"synthetic-vendor","model":"synthetic-model"}},"usage":{"inputTokens":%d,"outputTokens":2,"cacheReadTokens":3,"totalTokens":%d}}}`, seq, now.UnixMilli(), input, input+5)
		flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
		f, err := os.OpenFile(file, flags, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if seq == 0 {
			_, err = f.WriteString(header)
		}
		if err == nil {
			_, err = f.WriteString(row + "\n")
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("fixture append: %v / %v", err, closeErr)
		}
	}
	storePath := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := telemetry.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	url, token := compatibilityHub(t)
	accounts := []string{"synthetic-personal-one", "synthetic-personal-two"}
	for revision, input := range []int{40, 12} {
		writeRevision(revision, input)
		snaps := map[string]core.UsageSnapshot{}
		for _, account := range accounts {
			// A new source each time also exercises process restart/replay.
			collector := telemetry.NewSourceCollector(dsh.New(), shared.TelemetryCollectOptions{Paths: map[string]string{
				"sessions_dir": sourceDir, "account_id": account,
			}}, account)
			requests, err := collector.Collect(ctx)
			if err != nil || len(requests) != 1 {
				t.Fatalf("source requests=%d err=%v", len(requests), err)
			}
			for replay := 0; replay < 2; replay++ {
				if _, err := store.Ingest(ctx, requests[0]); err != nil {
					t.Fatal(err)
				}
			}
			snaps[account] = core.NewUsageSnapshot("dsh", account)
		}
		var count, total int
		if err := store.DB().QueryRow(`SELECT COUNT(*),SUM(total_tokens) FROM usage_events WHERE event_type='message_usage'`).Scan(&count, &total); err != nil {
			t.Fatal(err)
		}
		if count != 2 || total != 2*(input+5) {
			t.Fatalf("canonical count=%d total=%d", count, total)
		}
		snaps, err = telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, storePath, snaps, telemetry.ReadModelOptions{TimeWindow: core.TimeWindowAll})
		if err != nil {
			t.Fatal(err)
		}
		for _, snap := range snaps {
			assertDSHCompatibilitySnapshot(t, snap, float64(input))
		}
		pushDSHCompatibilitySnapshots(t, url, token, snaps, float64(input))
	}
}

func assertDSHCompatibilitySnapshot(t *testing.T, snap core.UsageSnapshot, input float64) {
	t.Helper()
	for key, want := range map[string]float64{"window_requests": 1, "model_synthetic_model_input_tokens": input, "model_synthetic_model_cache_read_tokens": 3, "provider_synthetic_vendor_input_tokens": input} {
		if got := snap.Metrics[key].Used; got == nil || *got != want {
			t.Fatalf("%s=%v want %g", key, got, want)
		}
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SYNTHETIC_PRIVATE") {
		t.Fatal("source content or private path reached snapshot")
	}
}

func pushDSHCompatibilitySnapshots(t *testing.T, url, token string, snaps map[string]core.UsageSnapshot, input float64) {
	t.Helper()
	e, err := exporter.New(config.ExportConfig{Target: url, MachineName: "synthetic-dsh-machine", AuthToken: token, IntervalSeconds: 1})
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
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/snapshots", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("Hub GET status=%d err=%v", response.StatusCode, err)
		}
		var received map[string]core.UsageSnapshot
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatal(err)
		}
		ready := len(received) == len(snaps)
		for account := range snaps {
			snap, ok := received["synthetic-dsh-machine:dsh:"+account]
			got := snap.Metrics["model_synthetic_model_input_tokens"].Used
			if !ok || got == nil || *got != input {
				ready = false
			}
		}
		if ready {
			for _, snap := range received {
				assertDSHCompatibilitySnapshot(t, snap, input)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Hub did not receive exactly two latest account snapshots")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
