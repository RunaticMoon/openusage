package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

func TestPerformanceCommandReadsExistingStoreAndFiltersProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	s, err := telemetry.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.EnablePerformance(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute)
	start := at.Add(-40 * time.Second)
	tokens := int64(3000)
	duration := float64(40000)
	for _, account := range []string{"personal", "company"} {
		r := &core.PerformanceRequest{ID: "same-id", Namespace: "test", SourceSystem: "opencode", AccountID: account, ModelID: "model", OutputTokens: &tokens, OutputBasis: "output", StartedAt: &start, EndedAt: &at, DurationMS: &duration, TimingBasis: core.TimingProviderRequestTimestamps, TimingStatus: core.TimingValid, Scope: core.ScopeModelRequest, Final: true, Outcome: core.OutcomeSuccess}
		_, err := s.Ingest(ctx, telemetry.IngestRequest{SourceSystem: "opencode", AccountID: account, OccurredAt: at, PerformanceOnly: true, Performance: r})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newPerformanceCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--db-path", path, "--source", "opencode", "--account", "personal", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var p core.PerformanceSnapshot
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Summary.Coverage.Candidates != 1 || len(p.Requests) != 1 || p.Requests[0].AccountID != "personal" {
		t.Fatal("profile filter failed")
	}
	if *p.Summary.Stats.WeightedResponseTokS != 75 {
		t.Fatal("query speed wrong")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("query modified SQLite main file")
	}
	missing := filepath.Join(t.TempDir(), "absent.db")
	cmd = newPerformanceCommand()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--db-path", missing})
	if err := cmd.Execute(); err == nil {
		t.Fatal("missing store unexpectedly created")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("query created missing DB")
	}
}
func TestPerformanceCommandValidatesBounds(t *testing.T) {
	for _, args := range [][]string{{"--buffer-minutes", "1"}, {"--request-limit", "1001"}, {"--since", "bad"}} {
		cmd := newPerformanceCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if got, err := parsePerformanceBound("2026-10-21T12:34:56.123Z"); err != nil || got.Nanosecond() != 123000000 {
		t.Fatalf("lost ms %v %v", got, err)
	}
	if !strings.Contains(newPerformanceCommand().Short, "read-only") {
		t.Fatal("query purpose hidden")
	}
}
