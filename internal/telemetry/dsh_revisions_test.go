package telemetry

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func dshRevisionRequest(input, revision int64) IngestRequest {
	req := usageEventRequest()
	req.SourceSystem, req.SourceChannel, req.SourceSchemaVersion = "dsh", SourceChannelJSONL, "dsh_jsonl_v4"
	req.ProviderID, req.AgentName, req.AccountID = "synthetic-vendor", "dsh", "personal-test"
	req.InputTokens, req.TotalTokens = int64Ptr(input), int64Ptr(input+40)
	if revision == 0 {
		req.ReasoningTokens = int64Ptr(4)
	} else {
		req.ReasoningTokens = nil
	}
	req.Payload = map[string]any{"dsh": map[string]any{"revision": revision}, "event_type": "assistant/message", "outcome": "reported"}
	return req
}

func TestDSHRevisionsCachedViewPruneRestartAndNoWriteReplay(t *testing.T) {
	globalUsageViewCache.reset()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	filter := usageFilter{ProviderIDs: []string{"synthetic-vendor"}}
	namespace := usageViewCacheNamespace(path)
	var eventID, rawID string
	var seq int64
	for revision, input := range []int64{0, 30, 20} {
		result, err := store.Ingest(ctx, dshRevisionRequest(input, int64(revision)))
		if err != nil || result.Updated != (revision > 0) || result.Deduped != (revision > 0) {
			t.Fatalf("revision=%d result=%+v err=%v", revision, result, err)
		}
		if revision == 0 {
			eventID, rawID = result.EventID, result.RawEventID
		} else if result.EventID != eventID || result.RawEventID != rawID {
			t.Fatal("stable identity changed")
		}
		var next int64
		if err := store.db.QueryRow(`SELECT MAX(seq) FROM usage_event_changes`).Scan(&next); err != nil || next <= seq {
			t.Fatalf("change cursor failed: next=%d old=%d err=%v", next, seq, err)
		}
		seq = next
		view, err := loadUsageViewForFilter(ctx, store.db, namespace, filter)
		if err != nil || view.EventCount != 1 || view.Activity.InputTokens != float64(input) {
			t.Fatalf("view revision=%d events=%d tokens=%v err=%v", revision, view.EventCount, view.Activity.InputTokens, err)
		}
	}
	var reasoningCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE reasoning_tokens IS NOT NULL`).Scan(&reasoningCount); err != nil || reasoningCount != 0 {
		t.Fatal("missing optional reasoning retained a previous sample")
	}
	var originalHash string
	if err := store.db.QueryRow(`SELECT source_payload_hash FROM usage_raw_events`).Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if n, err := store.PruneRawEventPayloads(ctx, 1, 100); err != nil || n != 1 {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	var payload, hash string
	if err := store.db.QueryRow(`SELECT source_payload,source_payload_hash FROM usage_raw_events`).Scan(&payload, &hash); err != nil || payload != `{"dsh":{"revision":2}}` || hash != originalHash {
		t.Fatalf("retained marker=%q hash preserved=%v err=%v", payload, hash == originalHash, err)
	}
	before := totalChanges(t, store.db)
	if n, err := store.PruneRawEventPayloads(ctx, 1, 100); err != nil || n != 0 || totalChanges(t, store.db) != before {
		t.Fatal("already compact marker was rewritten")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	globalUsageViewCache.reset()
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []IngestRequest{dshRevisionRequest(0, 0), dshRevisionRequest(30, 1), dshRevisionRequest(20, 2)} {
		before := totalChanges(t, store.db)
		result, err := store.Ingest(ctx, req)
		if err != nil || !result.Deduped || result.Updated || totalChanges(t, store.db) != before {
			t.Fatalf("stale/replay changed source: %+v err=%v", result, err)
		}
	}
	view, err := loadUsageViewForFilter(ctx, store.db, namespace, filter)
	if err != nil || view.Activity.InputTokens != 20 {
		t.Fatalf("restart regressed tokens: %v err=%v", view.Activity.InputTokens, err)
	}
	// A metadata-only settlement still advances the durable cursor, but an
	// identical replay does not cause a write or restore pruned payloads.
	result, err := store.Ingest(ctx, dshRevisionRequest(20, 3))
	if err != nil || !result.Updated {
		t.Fatalf("revision-only update=%+v err=%v", result, err)
	}
	var next int64
	if err := store.db.QueryRow(`SELECT MAX(seq) FROM usage_event_changes`).Scan(&next); err != nil || next <= seq {
		t.Fatal("metadata revision was hidden")
	}
}

func TestDSHAuthorityDoesNotExtendToOtherSchemas(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first := dshRevisionRequest(10, 1)
	first.SourceSchemaVersion = "unrecognized"
	if _, err := store.Ingest(ctx, first); err != nil {
		t.Fatal(err)
	}
	next := first
	next.InputTokens = int64Ptr(99)
	result, err := store.Ingest(ctx, next)
	if err != nil || result.Updated {
		t.Fatal("unknown schema acquired revision authority")
	}
	var input int64
	if err := store.db.QueryRow(`SELECT input_tokens FROM usage_events`).Scan(&input); err != nil || input != 10 {
		t.Fatalf("legacy policy changed: %d %v", input, err)
	}
}
