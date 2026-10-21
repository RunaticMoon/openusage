package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func opencodeRevisionRequest(input, revision int64) IngestRequest {
	req := usageEventRequest()
	req.SourceSystem = "opencode"
	req.SourceChannel = SourceChannelSQLite
	req.SourceSchemaVersion = "opencode_sqlite_v2"
	req.ProviderID, req.AgentName, req.AccountID = "opencode", "opencode", "personal-test"
	req.InputTokens, req.TotalTokens = int64Ptr(input), int64Ptr(input+40)
	req.CostUSD = float64Ptr(float64(input) / 100)
	req.Payload = map[string]any{"db": map[string]any{"time_updated": revision}, "client": fmt.Sprintf("revision-%d", revision), "tokens": input}
	return req
}

func TestOpenCodeV2RevisionsReachCachedViewAndSurvivePruneRestart(t *testing.T) {
	globalUsageViewCache.reset()
	path := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	ctx := context.Background()
	filter := usageFilter{ProviderIDs: []string{"opencode"}}
	namespace := usageViewCacheNamespace(path)
	var eventID, rawID string
	var seq int64
	for i, input := range []int64{0, 30, 20} {
		result, err := store.Ingest(ctx, opencodeRevisionRequest(input, int64(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		if result.Updated != (i > 0) || result.Deduped != (i > 0) {
			t.Fatalf("revision %d result=%+v", i, result)
		}
		if i == 0 {
			eventID, rawID = result.EventID, result.RawEventID
		} else if result.EventID != eventID || result.RawEventID != rawID {
			t.Fatal("revision changed stable identity")
		}
		var next int64
		if err := store.db.QueryRow(`SELECT MAX(seq) FROM usage_event_changes`).Scan(&next); err != nil {
			t.Fatal(err)
		}
		if next <= seq {
			t.Fatalf("revision sequence did not advance: %d <= %d", next, seq)
		}
		seq = next
		view, err := loadUsageViewForFilter(ctx, store.db, namespace, filter)
		if err != nil {
			t.Fatal(err)
		}
		if view.EventCount != 1 || view.Activity.InputTokens != float64(input) {
			t.Fatalf("cached revision %d: events=%d tokens=%.0f", i, view.EventCount, view.Activity.InputTokens)
		}
	}
	final := opencodeRevisionRequest(20, 3)
	var originalHash string
	if err := store.db.QueryRow(`SELECT source_payload_hash FROM usage_raw_events`).Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if n, err := store.PruneRawEventPayloads(ctx, 1, 100); err != nil || n != 1 {
		t.Fatalf("prune: n=%d err=%v", n, err)
	}
	var compact, hash string
	if err := store.db.QueryRow(`SELECT source_payload,source_payload_hash FROM usage_raw_events`).Scan(&compact, &hash); err != nil {
		t.Fatal(err)
	}
	if compact != `{"db":{"time_updated":3}}` || hash != originalHash {
		t.Fatalf("revision/hash not preserved: %q hash changed=%v", compact, hash != originalHash)
	}
	before := totalChanges(t, store.db)
	if n, err := store.PruneRawEventPayloads(ctx, 1, 100); err != nil || n != 0 {
		t.Fatalf("repeated prune: n=%d err=%v", n, err)
	}
	if totalChanges(t, store.db) != before {
		t.Fatal("compact markers were rewritten")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []IngestRequest{opencodeRevisionRequest(0, 1), opencodeRevisionRequest(30, 2), final, final} {
		before := totalChanges(t, store.db)
		result, err := store.Ingest(ctx, req)
		if err != nil || result.Updated || !result.Deduped {
			t.Fatalf("stale/replay result=%+v err=%v", result, err)
		}
		if totalChanges(t, store.db) != before {
			t.Fatal("stale/replay wrote after restart")
		}
	}
	var value int64
	if err := store.db.QueryRow(`SELECT input_tokens FROM usage_events`).Scan(&value); err != nil || value != 20 {
		t.Fatalf("regressed final value=%d err=%v", value, err)
	}
	// A new revision with identical usage still updates provenance and advances
	// the durable cursor; nil values never erase previously known usage.
	next := opencodeRevisionRequest(20, 4)
	next.InputTokens = nil
	before = totalChanges(t, store.db)
	result, err := store.Ingest(ctx, next)
	if err != nil || !result.Updated {
		t.Fatalf("metadata revision result=%+v err=%v", result, err)
	}
	if totalChanges(t, store.db) == before {
		t.Fatal("metadata revision was ignored")
	}
	if err := store.db.QueryRow(`SELECT input_tokens FROM usage_events`).Scan(&value); err != nil || value != 20 {
		t.Fatalf("nil erased value=%d err=%v", value, err)
	}
	var nextSeq int64
	if err := store.db.QueryRow(`SELECT MAX(seq) FROM usage_event_changes`).Scan(&nextSeq); err != nil || nextSeq <= seq {
		t.Fatalf("metadata cursor=%d previous=%d err=%v", nextSeq, seq, err)
	}
	view, err := loadUsageViewForFilter(ctx, store.db, namespace, filter)
	if err != nil {
		t.Fatal(err)
	}
	var oldClient, newClient float64
	for _, point := range view.ClientDaily["revision_3"] {
		oldClient += point.Value
	}
	for _, point := range view.ClientDaily["revision_4"] {
		newClient += point.Value
	}
	if oldClient != 0 || newClient != 1 || view.Activity.InputTokens != 20 {
		t.Fatalf("metadata-only client update: old=%v new=%v tokens=%v", oldClient, newClient, view.Activity.InputTokens)
	}
}

func TestOpenCodeV2HookOrderingAndProvenance(t *testing.T) {
	for _, hookFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("hook_first_%v", hookFirst), func(t *testing.T) {
			store, err := OpenStore(filepath.Join(t.TempDir(), "telemetry.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			dbReq := opencodeRevisionRequest(20, 2)
			hook := opencodeRevisionRequest(5, 1)
			hook.SourceChannel = SourceChannelHook
			hook.SourceSchemaVersion = "opencode_hook_v1"
			first, second := dbReq, hook
			if hookFirst {
				first, second = hook, dbReq
			}
			if _, err := store.Ingest(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Ingest(context.Background(), second); err != nil {
				t.Fatal(err)
			}
			before := totalChanges(t, store.db)
			hook.InputTokens = int64Ptr(999)
			result, err := store.Ingest(context.Background(), hook)
			if err != nil || result.Updated || totalChanges(t, store.db) != before {
				t.Fatalf("late hook changed canonical event: %+v %v", result, err)
			}
			var value int64
			var channel, schema, payload, hash string
			if err := store.db.QueryRow(`SELECT e.input_tokens,r.source_channel,r.source_schema_version,r.source_payload,r.source_payload_hash FROM usage_events e JOIN usage_raw_events r USING(raw_event_id)`).Scan(&value, &channel, &schema, &payload, &hash); err != nil {
				t.Fatal(err)
			}
			if value != 20 || channel != "sqlite" || schema != "opencode_sqlite_v2" || openCodeRevision([]byte(payload)) != 2 || hash == "" {
				t.Fatalf("wrong authority: value=%d channel=%s schema=%s", value, channel, schema)
			}
		})
	}
}

func TestOpenCodeV2ScopeLeavesOtherMergesUnchanged(t *testing.T) {
	for _, scope := range []string{"other_provider", "v1", "tool"} {
		t.Run(scope, func(t *testing.T) {
			store, err := OpenStore(filepath.Join(t.TempDir(), "telemetry.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			req := opencodeRevisionRequest(1, 1)
			switch scope {
			case "other_provider":
				req.SourceSystem = "codex"
			case "v1":
				req.SourceSchemaVersion = "opencode_sqlite_v1"
			case "tool":
				req.EventType = EventTypeToolUsage
				req.ToolCallID = "tool-1"
			}
			if _, err := store.Ingest(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			req.InputTokens = int64Ptr(9)
			before := totalChanges(t, store.db)
			result, err := store.Ingest(context.Background(), req)
			if err != nil || result.Updated || totalChanges(t, store.db) != before {
				t.Fatalf("unrelated policy changed: %+v err=%v", result, err)
			}
		})
	}
}

func TestOpenCodeV2RevisionRollbackIsAtomic(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Ingest(context.Background(), opencodeRevisionRequest(10, 1)); err != nil {
		t.Fatal(err)
	}
	var beforeSeq int64
	if err := store.db.QueryRow(`SELECT MAX(seq) FROM usage_event_changes`).Scan(&beforeSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_revision BEFORE UPDATE ON usage_events BEGIN SELECT RAISE(ABORT, 'synthetic write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(context.Background(), opencodeRevisionRequest(20, 2)); err == nil {
		t.Fatal("expected synthetic failure")
	}
	var payload string
	var afterSeq, value int64
	if err := store.db.QueryRow(`SELECT input_tokens,source_payload FROM usage_events JOIN usage_raw_events USING(raw_event_id)`).Scan(&value, &payload); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT MAX(seq) FROM usage_event_changes`).Scan(&afterSeq); err != nil {
		t.Fatal(err)
	}
	if value != 10 || openCodeRevision([]byte(payload)) != 1 || beforeSeq != afterSeq {
		t.Fatal("failed revision left partial canonical/raw/cursor updates")
	}
}

func TestIngestResultUpdatedJSONCompatibility(t *testing.T) {
	old := `{"status":"accepted","deduped":true,"event_id":"e","raw_event_id":"r"}`
	var result IngestResult
	if err := json.Unmarshal([]byte(old), &result); err != nil || result.Updated {
		t.Fatalf("legacy decode: %+v %v", result, err)
	}
	data, err := json.Marshal(result)
	if err != nil || string(data) != old {
		t.Fatalf("legacy encoding changed: %s %v", data, err)
	}
	result.Updated = true
	data, err = json.Marshal(result)
	if err != nil || !strings.Contains(string(data), `"updated":true`) {
		t.Fatalf("updated flag missing: %s %v", data, err)
	}
	var legacy struct {
		Status     string `json:"status"`
		Deduped    bool   `json:"deduped"`
		EventID    string `json:"event_id"`
		RawEventID string `json:"raw_event_id"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil || legacy.EventID != "e" || !legacy.Deduped {
		t.Fatalf("legacy consumer: %+v %v", legacy, err)
	}
}

func TestPipelineCountsOpenCodeV2Updates(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pipeline := NewPipeline(store, NewSpool(t.TempDir()))
	final := opencodeRevisionRequest(20, 2)
	if _, err := pipeline.EnqueueRequests([]IngestRequest{opencodeRevisionRequest(0, 1), final, final, opencodeRevisionRequest(0, 1)}); err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Flush(context.Background(), 100)
	if err != nil || result.Ingested != 1 || result.Updated != 1 || result.Deduped != 3 || result.Processed != 4 {
		t.Fatalf("flush=%+v err=%v", result, err)
	}
}
