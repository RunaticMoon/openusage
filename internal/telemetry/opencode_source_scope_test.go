package telemetry

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestOpenCodeAccountProjectionPreservesVendorAndSourceBoundaries(t *testing.T) {
	globalUsageViewCache.reset()
	path := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	serial := 0
	add := func(source, vendor, account string, input int64) {
		t.Helper()
		serial++
		_, err := store.Ingest(ctx, IngestRequest{
			SourceSystem: SourceSystem(source), SourceChannel: SourceChannelHook,
			OccurredAt: now, ProviderID: vendor, AccountID: account,
			AgentName: source, EventType: EventTypeMessageUsage,
			SessionID: "synthetic-session", MessageID: fmt.Sprintf("synthetic-%d", serial),
			ModelRaw: "synthetic-model", TokenUsage: core.TokenUsage{
				InputTokens: &input, TotalTokens: &input, Requests: int64Ptr(1),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	add("opencode", "anthropic", "personal", 11)
	add("opencode", "openai", "personal", 22)
	add("opencode", "custom-vendor", "personal", 33)
	add("opencode", "anthropic", "company", 777)
	add("claude_code", "anthropic", "personal", 888)
	add("claude_code", "anthropic", "claude-personal", 44)
	add("codex", "opencode", "personal", 999)

	project := func() map[string]core.UsageSnapshot {
		t.Helper()
		snaps := map[string]core.UsageSnapshot{
			"personal": core.NewUsageSnapshot("opencode", "personal"),
			"company":  core.NewUsageSnapshot("opencode", "company"),
			"empty":    core.NewUsageSnapshot("opencode", "empty"),
			"claude":   core.NewUsageSnapshot("claude_code", "claude-personal"),
		}
		personal := snaps["personal"]
		personal.Metrics["rolling_usage"] = core.Metric{Used: core.Float64Ptr(12), Unit: "percent"}
		snaps["personal"] = personal
		// Existing native Claude routing remains valid; no global link rewrite
		// is needed to expose the same model vendor in an OpenCode account.
		out, err := ApplyCanonicalTelemetryViewWithOptions(ctx, path, snaps, ReadModelOptions{
			ProviderLinks: map[string]string{"anthropic": "claude_code"}, TimeWindow: core.TimeWindowAll,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	assertInput := func(snap core.UsageSnapshot, want float64) {
		t.Helper()
		metric := snap.Metrics["model_synthetic_model_input_tokens"]
		if metric.Used == nil || *metric.Used != want {
			t.Fatalf("input=%v want %g", metric.Used, want)
		}
	}
	out := project()
	assertInput(out["personal"], 66)
	assertInput(out["company"], 777)
	if quota := out["personal"].Metrics["rolling_usage"].Used; quota == nil || *quota != 12 {
		t.Fatal("usage projection changed the root quota")
	}
	// Empty explicit accounts must never use source-wide fallback, even if
	// they are the only configured OpenCode account.
	if value := out["empty"].Metrics["window_requests"].Used; value != nil && *value != 0 {
		t.Fatal("empty account inherited another profile")
	}
	if out["personal"].Attributes["telemetry_scope_account_id"] != "personal" {
		t.Fatal("missing explicit account attribution")
	}
	for vendor, want := range map[string]float64{"anthropic": 11, "openai": 22, "custom_vendor": 33} {
		value := out["personal"].Metrics["provider_"+vendor+"_input_tokens"].Used
		if value == nil || *value != want {
			t.Fatalf("vendor %s attribution missing", vendor)
		}
	}
	// The existing Claude provider policy is unchanged, including its vendor
	// filter. This assertion guards against a global provider-link remap.
	assertInput(out["claude"], 44)
	add("opencode", "new-vendor", "personal", 7)
	out = project() // incremental cached path, including a previously unseen vendor
	assertInput(out["personal"], 73)
	assertInput(out["company"], 777)
	for _, namespace := range []usageViewCacheNamespace{"", usageViewCacheNamespace(path)} {
		agg, err := loadUsageViewForFilter(ctx, store.DB(), namespace, usageFilter{SourceSystem: "opencode", AccountID: "personal"})
		if err != nil {
			t.Fatal(err)
		}
		if agg.EventCount != 4 || agg.Activity.InputTokens != 73 {
			t.Fatalf("source aggregation events=%d input=%g", agg.EventCount, agg.Activity.InputTokens)
		}
	}
	empty, err := ApplyCanonicalTelemetryViewWithOptions(ctx, path,
		map[string]core.UsageSnapshot{"empty": core.NewUsageSnapshot("opencode", "empty")}, ReadModelOptions{TimeWindow: core.TimeWindowAll})
	if err != nil {
		t.Fatal(err)
	}
	if value := empty["empty"].Metrics["window_requests"].Used; value != nil && *value != 0 {
		t.Fatal("sole empty account inherited another profile")
	}
}

func TestSourceScopedUsageCacheKeyAndMatcher(t *testing.T) {
	filter := usageFilter{SourceSystem: "opencode", AccountID: "personal"}
	otherSource := filter
	otherSource.SourceSystem = "claude_code"
	providerFilter := usageFilter{ProviderIDs: []string{"opencode"}, AccountID: "personal"}
	if usageViewCacheKey("db", filter) == usageViewCacheKey("db", otherSource) || usageViewCacheKey("db", filter) == usageViewCacheKey("db", providerFilter) {
		t.Fatal("source scope collided with another cache key")
	}
	event := usageViewEvent{SourceSystem: "opencode", ProviderID: "new-vendor", AccountID: "personal", EventType: "message_usage"}
	if !usageViewEventMatchesFilter(event, filter) {
		t.Fatal("model vendor excluded from tool source")
	}
	event.SourceSystem = "claude_code"
	if usageViewEventMatchesFilter(event, filter) {
		t.Fatal("another source matched")
	}
	event.SourceSystem, event.AccountID = "opencode", "company"
	if usageViewEventMatchesFilter(event, filter) {
		t.Fatal("another account matched")
	}
}
