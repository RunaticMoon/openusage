package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

func TestOpenCodeV2UpdatedCountsAndSpoolRefresh(t *testing.T) {
	store, err := telemetry.OpenStore(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &Service{store: store, pipeline: telemetry.NewPipeline(store, telemetry.NewSpool(t.TempDir()))}
	req := telemetry.IngestRequest{SourceSystem: "opencode", SourceChannel: telemetry.SourceChannelSQLite, SourceSchemaVersion: "opencode_sqlite_v2", ProviderID: "opencode", AccountID: "test-personal", AgentName: "opencode", EventType: telemetry.EventTypeMessageUsage, SessionID: "session", MessageID: "message", TokenUsage: core.TokenUsage{InputTokens: core.Int64Ptr(0)}, Payload: map[string]any{"db": map[string]any{"time_updated": 1}}}
	final := req
	final.InputTokens = core.Int64Ptr(20)
	final.Payload = map[string]any{"db": map[string]any{"time_updated": 2}}
	tally, retries := s.ingestBatch(context.Background(), []telemetry.IngestRequest{req, final, final})
	if len(retries) != 0 || tally.ingested != 1 || tally.updated != 1 || tally.deduped != 2 {
		t.Fatalf("direct counts: %+v retries=%d", tally, len(retries))
	}
	correction := final
	correction.InputTokens = core.Int64Ptr(10)
	correction.Payload = map[string]any{"db": map[string]any{"time_updated": 3}}
	if _, err := s.pipeline.EnqueueRequests([]telemetry.IngestRequest{correction, correction}); err != nil {
		t.Fatal(err)
	}
	s.flushSpoolBacklog(context.Background(), 100)
	version := s.dataVersion.Load()
	if version == 0 || !s.dataIngested.Load() {
		t.Fatal("deduplicated correction did not invalidate read model")
	}
	if _, err := s.pipeline.EnqueueRequests([]telemetry.IngestRequest{final, correction}); err != nil {
		t.Fatal(err)
	}
	s.flushSpoolBacklog(context.Background(), 100)
	if s.dataVersion.Load() != version {
		t.Fatal("stale/repeated snapshots invalidated cache without changes")
	}
}
