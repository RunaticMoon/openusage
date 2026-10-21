package telemetry

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestAntigravitySourceAccountIsolationIncludingSoleEmptyProfile(t *testing.T) {
	globalUsageViewCache.reset()
	path := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for i, row := range []struct {
		source, account string
		input           int64
	}{{"antigravity", "one", 11}, {"antigravity", "two", 22}, {"codex", "one", 999}} {
		_, err := store.Ingest(ctx, IngestRequest{
			SourceSystem: SourceSystem(row.source), SourceChannel: SourceChannelJSONL,
			OccurredAt: time.Now().UTC(), ProviderID: "antigravity", AccountID: row.account,
			AgentName: row.source, EventType: EventTypeMessageUsage,
			SessionID: "synthetic", MessageID: fmt.Sprintf("step-%d", i),
			TokenUsage: core.TokenUsage{InputTokens: &row.input, TotalTokens: &row.input, Requests: int64Ptr(1)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, accounts := range [][]string{{"one", "two", "empty"}, {"empty"}} {
		snaps := make(map[string]core.UsageSnapshot)
		for _, account := range accounts {
			snaps[account] = core.NewUsageSnapshot("antigravity", account)
		}
		out, err := ApplyCanonicalTelemetryViewWithOptions(ctx, path, snaps, ReadModelOptions{TimeWindow: core.TimeWindowAll})
		if err != nil {
			t.Fatal(err)
		}
		for _, account := range accounts {
			metric := out[account].Metrics["window_requests"]
			if account == "empty" {
				if metric.Used != nil && *metric.Used != 0 {
					t.Fatal("empty profile inherited another profile's usage")
				}
			} else if metric.Used == nil || *metric.Used != 1 {
				t.Fatalf("profile %s did not preserve source/account boundary", account)
			}
		}
	}
}
