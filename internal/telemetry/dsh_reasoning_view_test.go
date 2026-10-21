package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func reasoningViewRequest(source, message string, revision, input, output, read, write, reasoning int64) IngestRequest {
	total := input + output + read + write
	if source != "dsh" {
		total += reasoning // Existing non-DSH accounting policy is unchanged.
	}
	return IngestRequest{
		SourceSystem: SourceSystem(source), SourceChannel: SourceChannelJSONL, SourceSchemaVersion: "dsh_jsonl_v4",
		OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ProviderID: "synthetic-vendor", AccountID: "synthetic-profile", AgentName: source,
		SessionID: "synthetic-session", MessageID: message, ModelRaw: "synthetic-model",
		EventType: EventTypeMessageUsage, Status: EventStatusOK,
		TokenUsage: core.TokenUsage{
			InputTokens: &input, OutputTokens: &output, CacheReadTokens: &read,
			CacheWriteTokens: &write, ReasoningTokens: &reasoning, TotalTokens: &total, Requests: int64Ptr(1),
		},
		Payload: map[string]any{"dsh": map[string]any{"revision": revision}},
	}
}

func TestDSHReasoningSubsetColdCachedAppendAndRevision(t *testing.T) {
	globalUsageViewCache.reset()
	path, db, store := openUsageViewRawTestStore(t)
	ctx := context.Background()
	filter := usageFilter{SourceSystem: "dsh", AccountID: "synthetic-profile"}
	namespace := usageViewCacheNamespace(path)
	var originalState *incrementalUsageState
	var originalID string
	stages := []struct {
		req                               IngestRequest
		requests, total, billable, reason float64
	}{
		{reasoningViewRequest("dsh", "one", 0, 10, 6, 3, 4, 2), 1, 23, 20, 2},
		{reasoningViewRequest("dsh", "two", 1, 5, 4, 2, 1, 1), 2, 35, 30, 3},
		{reasoningViewRequest("dsh", "one", 2, 7, 3, 3, 2, 1), 2, 27, 22, 2},
	}
	for i, stage := range stages {
		result, err := store.Ingest(ctx, stage.req)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			originalID = result.EventID
		} else if i == 2 && (!result.Updated || result.EventID != originalID) {
			t.Fatal("decreasing revision lost stable event identity")
		}
		for _, ns := range []usageViewCacheNamespace{"", namespace, namespace} {
			agg, err := loadUsageViewForFilter(ctx, db, ns, filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(agg.Models) != 1 {
				t.Fatalf("stage %d: model count=%d", i, len(agg.Models))
			}
			model := agg.Models[0]
			if model.Requests != stage.requests || model.TotalTokens != stage.total || model.BillableTokens != stage.billable || model.Reasoning != stage.reason {
				t.Fatalf("stage %d: total=%g billable=%g reasoning=%g requests=%g", i, model.TotalTokens, model.BillableTokens, model.Reasoning, model.Requests)
			}
			snap := core.NewUsageSnapshot("dsh", "synthetic-profile")
			applyUsageViewToSnapshot(&snap, agg, core.TimeWindowAll)
			if value := snap.Metrics["window_tokens"].Used; value == nil || *value != stage.billable {
				t.Fatalf("stage %d: projected window double counted reasoning", i)
			}
		}
		entry, ok := globalUsageViewCache.lookup(usageViewCacheKey(namespace, filter))
		if !ok || entry.state == nil {
			t.Fatal("missing incremental cache state")
		}
		if i == 0 {
			originalState = entry.state
		} else if entry.state != originalState || entry.state.appliedChanges == 0 {
			t.Fatal("append/revision did not exercise incremental refresh")
		}
	}
}

func TestDSHReasoningExceptionPreservesOtherSourcePolicy(t *testing.T) {
	globalUsageViewCache.reset()
	path, db, store := openUsageViewRawTestStore(t)
	ctx := context.Background()
	mustIngestUsageEvent(t, store, reasoningViewRequest("codex", "control", 0, 10, 6, 3, 4, 2), "ingest non-DSH control")
	filter := usageFilter{SourceSystem: "codex", AccountID: "synthetic-profile"}
	for _, ns := range []usageViewCacheNamespace{"", usageViewCacheNamespace(path)} {
		agg, err := loadUsageViewForFilter(ctx, db, ns, filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(agg.Models) != 1 || agg.Models[0].BillableTokens != 22 || agg.Models[0].Reasoning != 2 || agg.Models[0].TotalTokens != 25 {
			t.Fatal("DSH exception changed another source's accounting")
		}
	}
}
