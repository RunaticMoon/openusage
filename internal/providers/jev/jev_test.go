package jev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func writeRows(t *testing.T, dir, file string, rows ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(strings.Join(rows, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}
func fetch(t *testing.T, root string) core.UsageSnapshot {
	t.Helper()
	acct := core.AccountConfig{ID: "m1-jev-personal", Provider: ID}
	acct.SetPath("usage_dir", root)
	snap, err := New().Fetch(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

const native = `{"version":1,"id":"native-1","timestamp":"2026-10-20T23:00:00Z","model":"jev-1.13.0","inputTokens":1000000,"outputTokens":20,"prompt":"PRIVATE_PROMPT","apiKey":"PRIVATE_SECRET"}`

func TestSnapshotDailyUsageRatesPrivacyAndPathIdentity(t *testing.T) {
	root := t.TempDir()
	writeRows(t, root, "2026-10-20.jsonl", native, native)
	writeRows(t, root, "2026-10-21.jsonl", `{"version":1,"id":"native-2","timestamp":"2026-10-21T01:00:00Z","model":"jev-future","inputTokens":7,"outputTokens":3,"requests":2}`)
	writeRows(t, root, "credentials.json", "PRIVATE_SECRET")
	snap := fetch(t, root)
	if snap.Status != core.StatusOK || len(snap.ModelUsage) != 2 {
		t.Fatalf("snapshot: status=%s models=%d", snap.Status, len(snap.ModelUsage))
	}
	if *snap.Metrics["requests"].Used != 3 || *snap.Metrics["total_tokens"].Used != 1000030 {
		t.Fatal("duplicate record counted or totals changed")
	}
	if metric := snap.Metrics["window_cost"]; metric.Used == nil || *metric.Used != 0.042 || snap.Attributes["cost_partial"] != "true" {
		t.Fatal("known subtotal or partial coverage marker lost")
	}
	known, unknown := snap.ModelUsage[0], snap.ModelUsage[1]
	if known.Dimensions["date"] != "2026-10-20" || known.CostUSD == nil || *known.CostUSD != 0.042 || unknown.CostUSD != nil {
		t.Fatalf("model costs: known=%+v unknown=%+v", known, unknown)
	}
	if points := snap.DailySeries["analytics_cost"]; len(points) != 1 || points[0].Date != "2026-10-20" || points[0].Value != 0.042 {
		t.Fatalf("unknown cost emitted: %+v", points)
	}
	if len(snap.DailySeries["analytics_tokens"]) != 2 {
		t.Fatal("daily tokens lost")
	}
	digest := sha256.Sum256([]byte("jev-records\x00" + root))
	if snap.Attributes["history_path_identity"] != "jev-records:sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatal("history identity mismatch")
	}
	encoded, _ := json.Marshal(snap)
	for _, private := range []string{"PRIVATE_PROMPT", "PRIVATE_SECRET", root, "native-1"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("private input exported: %q", private)
		}
	}
}

func TestMixedAndUnknownDailyCostCoverage(t *testing.T) {
	root := t.TempDir()
	writeRows(t, root, "2026-10-20.jsonl", native,
		`{"version":1,"id":"unknown-same-day","timestamp":"2026-10-20T23:01:00Z","model":"jev-future","inputTokens":7,"outputTokens":3}`)
	writeRows(t, root, "2026-10-21.jsonl",
		`{"version":1,"id":"unknown-only","timestamp":"2026-10-21T00:00:00Z","model":"jev-future","inputTokens":11,"outputTokens":3}`)
	snap := fetch(t, root)
	if snap.Attributes["cost_basis"] != "provider-reference-estimate" || snap.Attributes["cost_partial"] != "true" {
		t.Fatal("reference price provenance or partial coverage missing")
	}
	if metric := snap.Metrics["window_cost"]; metric.Used == nil || *metric.Used != 0.042 {
		t.Fatal("known subtotal lost when unknown model shares a day")
	}
	if points := snap.DailySeries["analytics_cost"]; len(points) != 1 || points[0].Date != "2026-10-20" || points[0].Value != 0.042 {
		t.Fatalf("daily cost fabricated or mixed-day subtotal lost: %+v", points)
	}
	if points := snap.DailySeries["analytics_cost_partial"]; len(points) != 2 || points[0].Date != "2026-10-20" || points[1].Date != "2026-10-21" || points[0].Value != 1 || points[1].Value != 1 {
		t.Fatalf("incomplete day coverage lost: %+v", points)
	}
	for _, row := range snap.ModelUsage {
		if row.RawModelID == "jev-future" && (row.CostUSD != nil || row.Dimensions["cost_partial"] != "true") {
			t.Fatal("unknown model cost became zero or lost its missing-price marker")
		}
		if row.RawModelID == "jev-1.13.0" && row.Dimensions["cost_partial"] != "" {
			t.Fatal("known model incorrectly marked incomplete")
		}
	}
}

func TestUnknownOnlyAndKnownZeroCostRemainDistinct(t *testing.T) {
	for _, model := range []string{"jev-future", "jev-1.13.0"} {
		t.Run(model, func(t *testing.T) {
			root := t.TempDir()
			writeRows(t, root, "2026-10-20.jsonl", `{"version":1,"id":"zero-input","timestamp":"2026-10-20T00:00:00Z","model":"`+model+`","inputTokens":0,"outputTokens":30}`)
			snap := fetch(t, root)
			if model == "jev-future" {
				if snap.Metrics["window_cost"].Used != nil || snap.ModelUsage[0].CostUSD != nil || len(snap.DailySeries["analytics_cost"]) != 0 || snap.Attributes["cost_partial"] != "true" {
					t.Fatal("unknown-only usage received a fabricated zero cost")
				}
			} else if snap.Metrics["window_cost"].Used == nil || *snap.Metrics["window_cost"].Used != 0 || snap.ModelUsage[0].CostUSD == nil || *snap.ModelUsage[0].CostUSD != 0 || len(snap.DailySeries["analytics_cost"]) != 1 || snap.Attributes["cost_partial"] != "" {
				t.Fatal("known zero cost did not remain a complete observation")
			}
		})
	}
}

func TestCSVReplacesPriorNativeSnapshotAcrossFiles(t *testing.T) {
	root := t.TempDir()
	writeRows(t, root, "2026-10-20.jsonl", native)
	first := fetch(t, root)
	if *first.Metrics["window_cost"].Used != 0.042 {
		t.Fatal("initial cost absent")
	}
	writeRows(t, root, "2026-10-21.jsonl", `{"version":1,"id":"csv-1","timestamp":"2026-10-20T23:00:00Z","model":"jev-1.13.0","inputTokens":500000,"outputTokens":10,"requests":4,"source":"typesafe-console-csv","supersedes":["native-1"]}`)
	second := fetch(t, root)
	if *second.Metrics["requests"].Used != 4 || *second.Metrics["window_cost"].Used != 0.021 || len(second.ModelUsage) != 1 {
		t.Fatal("CSV replacement added to obsolete native usage")
	}
	third := fetch(t, root)
	a, _ := json.Marshal(second.ModelUsage)
	b, _ := json.Marshal(third.ModelUsage)
	if string(a) != string(b) {
		t.Fatal("poll replay changed usage")
	}
}

func TestRejectMalformedConflictingAndUnsafeMetadata(t *testing.T) {
	for name, rows := range map[string][]string{
		"invalid-json":         {native, `{"prompt":"PRIVATE_SECRET"`},
		"missing-tokens":       {`{"version":1,"id":"a","timestamp":"2026-10-20T00:00:00Z","model":"jev-1.13.0"}`},
		"negative":             {strings.Replace(native, "1000000", "-1", 1)},
		"overflow":             {strings.Replace(native, "1000000", "9007199254740991", 1)},
		"conflicting-id":       {native, strings.Replace(native, "1000000", "1", 1)},
		"null-requests":        {strings.TrimSuffix(native, "}") + `,"requests":null}`},
		"untrusted-supersedes": {strings.TrimSuffix(native, "}") + `,"supersedes":["other"]}`},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeRows(t, root, "2026-10-20.jsonl", rows...)
			acct := core.AccountConfig{ID: "test", Provider: ID}
			acct.SetPath("usage_dir", root)
			_, err := New().Fetch(context.Background(), acct)
			if err == nil || strings.Contains(err.Error(), "PRIVATE_SECRET") || strings.Contains(err.Error(), root) {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		secret := filepath.Join(t.TempDir(), "credentials.json")
		if err := os.WriteFile(secret, []byte(native), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(root, "2026-10-20.jsonl")); err != nil {
			t.Fatal(err)
		}
		if _, err := readLedger(context.Background(), root); err == nil {
			t.Fatal("symlink was followed")
		}
	})
}

func TestDefaultPathMissingAndCancellation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PASEO_HOME", "")
	if DefaultUsageDir() != filepath.Join(root, ".paseo", "usage-limits", "jev") {
		t.Fatal("unexpected default")
	}
	t.Setenv("PASEO_HOME", filepath.Join(root, "custom"))
	if DefaultUsageDir() != filepath.Join(root, "custom", "usage-limits", "jev") {
		t.Fatal("PASEO_HOME ignored")
	}
	if _, err := New().Fetch(context.Background(), core.AccountConfig{ID: "test"}); err == nil {
		t.Fatal("missing ledger presented as zero usage")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writeRows(t, root, "2026-10-20.jsonl", native)
	if _, err := readLedger(ctx, root); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}
