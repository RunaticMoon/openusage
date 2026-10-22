package jev_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/jev"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

func TestNativeJevSnapshotSurvivesCanonicalReadModelAndHubJSON(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	data := `{"version":1,"id":"known","timestamp":"2026-10-20T23:00:00Z","model":"jev-1.13.0","inputTokens":1000000,"outputTokens":5}` + "\n" + `{"version":1,"id":"unknown-mixed","timestamp":"2026-10-20T23:01:00Z","model":"jev-future","inputTokens":7,"outputTokens":2}` + "\n" + `{"version":1,"id":"unknown","timestamp":"2026-10-21T00:00:00Z","model":"jev-future","inputTokens":11,"outputTokens":2}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-10-20.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	acct := core.AccountConfig{ID: "m1-jev-personal", Provider: "jev"}
	acct.SetPath("usage_dir", dir)
	snap, err := jev.New().Fetch(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := telemetry.OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := map[string]core.UsageSnapshot{acct.ID: snap}
	if err := telemetry.NewQuotaSnapshotIngestor(store).Ingest(ctx, base); err != nil {
		t.Fatal(err)
	}
	// Another tool's Jev events must not replace this host-local ledger merely
	// because the upstream provider ID happens to be the same.
	if _, err := store.Ingest(ctx, telemetry.IngestRequest{
		SourceSystem: "opencode", SourceChannel: telemetry.SourceChannelSQLite,
		ProviderID: "jev", AccountID: "different-tool-account", AgentName: "opencode",
		OccurredAt: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
		EventType:  telemetry.EventTypeMessageUsage, MessageID: "unrelated-usage", ModelRaw: "jev-1.13.0",
		TokenUsage: core.TokenUsage{InputTokens: core.Int64Ptr(999), OutputTokens: core.Int64Ptr(3), CostUSD: core.Float64Ptr(9), Requests: core.Int64Ptr(1)},
	}); err != nil {
		t.Fatal(err)
	}
	view, err := telemetry.ApplyCanonicalTelemetryViewWithOptions(ctx, db, base, telemetry.ReadModelOptions{Now: snap.Timestamp, Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), TimeWindow: core.TimeWindow30d})
	if err != nil {
		t.Fatal(err)
	}
	got := view[acct.ID]
	if !reflect.DeepEqual(got.ModelUsage, snap.ModelUsage) || !reflect.DeepEqual(got.DailySeries, snap.DailySeries) {
		t.Fatal("native model/day accounting was replaced by poller events")
	}
	if metric := got.Metrics["window_cost"]; metric.Used == nil || *metric.Used != 0.042 || got.Attributes["cost_partial"] != "true" {
		t.Fatal("partial subtotal or coverage changed")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wire core.UsageSnapshot
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ModelUsage[1].CostUSD != nil || wire.ModelUsage[1].Dimensions["cost_partial"] != "true" || len(wire.DailySeries["analytics_cost"]) != 1 || len(wire.DailySeries["analytics_cost_partial"]) != 2 || wire.Attributes["history_path_identity"] != snap.Attributes["history_path_identity"] || wire.Attributes["cost_basis"] != "provider-reference-estimate" || wire.Attributes["cost_partial"] != "true" {
		t.Fatal("Hub serialization changed unknown cost or source identity")
	}
}
