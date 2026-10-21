package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func performanceFixture(id string, output int64, duration float64, at time.Time) IngestRequest {
	start := at.Add(-time.Duration(duration * float64(time.Millisecond)))
	input, cached, reason, cost, requests := int64(9000), int64(7000), int64(2000), 0.75, int64(1)
	p := &core.PerformanceRequest{ID: id, Namespace: "fixture.request", SourceSystem: "claude_code", AccountID: "personal", BillingProviderID: "anthropic", SessionID: "session", ModelID: "model", AdapterVersion: "test-v1", OutputTokens: &output, OutputBasis: "output_includes_reasoning", StartedAt: &start, EndedAt: &at, DurationMS: &duration, TimingBasis: core.TimingProviderRequestTimestamps, TimingStatus: core.TimingValid, Scope: core.ScopeModelRequest, Final: true, Outcome: core.OutcomeSuccess, Revision: 1, RecordedAt: at}
	return IngestRequest{SourceSystem: "claude_code", SourceChannel: SourceChannelJSONL, OccurredAt: at, SessionID: "session", MessageID: id, ProviderID: "anthropic", AccountID: "personal", EventType: EventTypeMessageUsage, Status: EventStatusOK, TokenUsage: core.TokenUsage{InputTokens: &input, OutputTokens: &output, CacheReadTokens: &cached, ReasoningTokens: &reason, CostUSD: &cost, Requests: &requests}, Performance: p}
}
func performanceTestStore(t *testing.T, enabled bool) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telemetry.db")
	s, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	if enabled {
		if e := s.EnablePerformance(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	return s, path
}
func readPerformance(t *testing.T, path string, at time.Time, account string) *core.PerformanceSnapshot {
	t.Helper()
	p, e := QueryPerformance(context.Background(), path, PerformanceQuery{Source: "claude_code", Account: account, Since: at.Add(-time.Hour), Now: at.Add(time.Second), IncludeWindow: true})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func ingestPerformance(t *testing.T, s *Store, r IngestRequest) IngestResult {
	t.Helper()
	v, e := s.Ingest(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	return v
}

func TestPerformanceReplayCorrectionDelayedTimingAndAccountingInvariant(t *testing.T) {
	ctx := context.Background()
	on, path := performanceTestStore(t, true)
	off, _ := performanceTestStore(t, false)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r := performanceFixture("request", 3000, 40000, at)
	for i := 0; i < 10; i++ {
		v := ingestPerformance(t, on, r)
		ingestPerformance(t, off, r)
		if i > 0 && v.Updated {
			t.Fatal("replay invalidated performance")
		}
	}
	if err := on.RecordPerformanceScan(ctx, "claude_code", "personal", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	p := readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Candidates != 1 || p.Summary.Stats.OutputTokens != 3000 || *p.Summary.Stats.WeightedResponseTokS != 75 {
		t.Fatalf("bad replay summary: %+v", p.Summary)
	}
	// A lower authoritative final counter replaces the speed contribution, and
	// must not silently change existing Work/Cached/Total/Cost merge rules.
	clone := r.Performance.DeepClone()
	r.Performance = &clone
	value := int64(1000)
	r.OutputTokens = &value
	r.Performance.OutputTokens = &value
	r.Performance.Revision = 2
	ingestPerformance(t, on, r)
	ingestPerformance(t, off, r)
	p = readPerformance(t, path, at, "personal")
	if p.Summary.Stats.OutputTokens != 1000 || *p.Summary.Stats.WeightedResponseTokS != 25 {
		t.Fatal("downward correction not applied")
	}
	for _, s := range []*Store{on, off} {
		var rows int
		if e := s.db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&rows); e != nil || rows != 1 {
			t.Fatalf("accounting rows=%d err=%v", rows, e)
		}
	}
	var a, b []any
	for i, s := range []*Store{on, off} {
		var input, output, cache, reason, total, requests int64
		var cost float64
		if err := s.db.QueryRow(`SELECT input_tokens,output_tokens,cache_read_tokens,reasoning_tokens,total_tokens,requests,cost_usd FROM usage_events`).Scan(&input, &output, &cache, &reason, &total, &requests, &cost); err != nil {
			t.Fatal(err)
		}
		v := []any{input, output, cache, reason, total, requests, cost}
		if i == 0 {
			a = v
		} else {
			b = v
		}
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("opt-in changed accounting: %v vs %v", a, b)
	}
	// Timing can arrive through the metadata-only path after the usage ledger.
	late := performanceFixture("late", 2000, 20000, at)
	late.Performance.DurationMS = nil
	late.Performance.StartedAt = nil
	late.Performance.TimingStatus = core.TimingMissing
	ingestPerformance(t, on, late)
	p = readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Candidates != 2 || p.Summary.Coverage.Measured != 1 {
		t.Fatal("missing timing coverage")
	}
	late = performanceFixture("late", 2000, 20000, at)
	late.PerformanceOnly = true
	ingestPerformance(t, on, late)
	p = readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Measured != 2 || p.Summary.Stats.OutputTokens != 3000 || math.Abs(*p.Summary.Stats.WeightedResponseTokS-50) > 1e-9 {
		t.Fatalf("late timing %+v", p.Summary)
	}
	var count int
	on.db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&count)
	if count != 2 {
		t.Fatal("late timing inserted usage")
	}
}

func TestPerformanceIdentityIsolationReopenAndTimingAuthority(t *testing.T) {
	s, path := performanceTestStore(t, true)
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	r := performanceFixture("native", 1000, 10000, at)
	r.PerformanceOnly = true
	ingestPerformance(t, s, r)
	fork := r
	v := r.Performance.DeepClone()
	fork.Performance = &v
	fork.Performance.SessionID = "copied-fork"
	fork.SessionID = "copied-fork"
	fork.SourceChannel = SourceChannelHook
	ingestPerformance(t, s, fork)
	retry := r
	v = r.Performance.DeepClone()
	v.ID = "separate-retry"
	retry.Performance = &v
	ingestPerformance(t, s, retry)
	company := r
	v = r.Performance.DeepClone()
	v.AccountID = "company"
	company.AccountID = "company"
	company.Performance = &v
	ingestPerformance(t, s, company)
	p := readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Candidates != 2 {
		t.Fatal("copy/retry dedup incorrect")
	}
	if p := readPerformance(t, path, at, "company"); p.Summary.Coverage.Candidates != 1 {
		t.Fatal("company profile isolation lost")
	}
	// Newer lower-quality timing may update output but cannot replace direct time.
	weak := r
	v = r.Performance.DeepClone()
	out := int64(500)
	duration := float64(20000)
	start := at.Add(-20 * time.Second)
	v.OutputTokens = &out
	v.DurationMS = &duration
	v.StartedAt = &start
	v.TimingBasis = core.TimingInferredLogBoundary
	v.Revision = 3
	weak.Performance = &v
	ingestPerformance(t, s, weak)
	p = readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Direct != 2 || p.Summary.Stats.OutputTokens != 1500 || p.Summary.Stats.DurationMS != 20000 {
		t.Fatalf("timing authority lost %+v", p.Summary)
	}
	reopened, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if e := reopened.EnablePerformance(context.Background()); e != nil {
		t.Fatal(e)
	}
	ingestPerformance(t, reopened, weak)
	p2 := readPerformance(t, path, at, "personal")
	if p2.Summary.Stats.OutputTokens != 1500 || p2.Summary.Coverage.Candidates != 2 {
		t.Fatal("restart replay changed totals")
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&count)
	if count != 0 {
		t.Fatal("metadata-only ingestion affected accounting")
	}
}

func TestPerformanceOptInMigrationRollbackBackfillAndRetention(t *testing.T) {
	ctx := context.Background()
	s, path := performanceTestStore(t, false)
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	r := performanceFixture("backfill", 3000, 40000, at)
	ingestPerformance(t, s, r)
	var tables int
	s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'performance_%'`).Scan(&tables)
	if tables != 0 {
		t.Fatal("disabled created performance schema")
	}
	if _, err := QueryPerformance(ctx, path, PerformanceQuery{}); err != ErrPerformanceDisabled {
		t.Fatalf("disabled read: %v", err)
	}
	if err := s.EnablePerformance(ctx); err != nil {
		t.Fatal(err)
	}
	ingestPerformance(t, s, r)
	ingestPerformance(t, s, r)
	p := readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Candidates != 1 {
		t.Fatal("backfill not idempotent")
	}
	// Old binaries can still read/write accounting tables; disabling the feature
	// leaves additive metadata intact and avoids any destructive down migration.
	old, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer old.Close()
	r.PerformanceOnly = true
	ingestPerformance(t, old, r)
	if n, e := s.PrunePerformance(ctx, at.Add(time.Millisecond)); e != nil || n != 1 {
		t.Fatalf("retention: %d %v", n, e)
	}
	p = readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Candidates != 0 {
		t.Fatal("retention did not invalidate incremental cache")
	}
	// Unknown schemas and lock failures preserve existing data and can be retried.
	bad, _ := performanceTestStore(t, false)
	bad.db.Exec(`CREATE TABLE performance_meta(key TEXT PRIMARY KEY,value INTEGER NOT NULL)`)
	bad.db.Exec(`INSERT INTO performance_meta VALUES('schema_version',999)`)
	if err := bad.EnablePerformance(ctx); err == nil {
		t.Fatal("unsupported schema accepted")
	}
	var version int
	bad.db.QueryRow(`SELECT value FROM performance_meta WHERE key='schema_version'`).Scan(&version)
	if version != 999 || bad.performanceEnabled.Load() {
		t.Fatal("failed migration mutated version/enabled state")
	}
	var requests int
	bad.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='performance_requests'`).Scan(&requests)
	if requests != 0 {
		t.Fatal("failed migration not atomic")
	}
}

func TestPerformanceFailureMissingZeroAndSensitivePayloadExcluded(t *testing.T) {
	s, path := performanceTestStore(t, true)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"zero", "missing", "failure", "parent"} {
		r := performanceFixture(id, 0, 10000, at)
		r.PerformanceOnly = true
		r.Payload = map[string]any{"message": "private body", "api_key": "synthetic-secret"}
		switch i {
		case 1:
			r.Performance.OutputTokens = nil
		case 2:
			r.Performance.Outcome = core.OutcomeFailed
		case 3:
			r.Performance.Scope = core.ScopeSession
		}
		ingestPerformance(t, s, r)
	}
	p := readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Candidates != 3 || p.Summary.Coverage.Measured != 1 || p.Summary.Coverage.Failed != 1 || p.Summary.Stats.WeightedResponseTokS == nil || *p.Summary.Stats.WeightedResponseTokS != 0 {
		t.Fatalf("zero/failure/parent %+v", p.Summary)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM performance_requests WHERE record_json LIKE '%private body%' OR record_json LIKE '%synthetic-secret%'`).Scan(&count)
	if count != 0 {
		t.Fatal("sensitive payload persisted")
	}
	encoded, e := json.Marshal(p)
	if e != nil || len(encoded) == 0 {
		t.Fatal(e)
	}
}

func TestPerformanceMigrationBusyPreservesAndRetries(t *testing.T) {
	s, path := performanceTestStore(t, false)
	locker, e := sql.Open("sqlite3", path)
	if e != nil {
		t.Fatal(e)
	}
	defer locker.Close()
	if _, e := locker.Exec(`BEGIN IMMEDIATE`); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.EnablePerformance(ctx); err == nil {
		t.Fatal("expected lock error")
	}
	if _, err := locker.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if err := s.EnablePerformance(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPerformanceInvalidCorrectionKeepsVerifiedTiming(t *testing.T) {
	s, path := performanceTestStore(t, true)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := performanceFixture("native", 1000, 10000, at)
	r.PerformanceOnly = true
	ingestPerformance(t, s, r)
	bad := r.Performance.DeepClone()
	r.Performance = &bad
	output := int64(500)
	duration := float64(-1)
	bad.OutputTokens = &output
	bad.DurationMS = &duration
	bad.Revision = 2
	bad.TimingStatus = core.TimingInvalid
	ingestPerformance(t, s, r)
	p := readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Measured != 1 || p.Summary.Stats.DurationMS != 10000 || *p.Summary.Stats.WeightedResponseTokS != 50 {
		t.Fatalf("invalid correction erased good time: %+v", p.Summary)
	}
	// Missing output must not lower the authority of independently valid timing.
	r = performanceFixture("late-output", 0, 10000, at)
	r.PerformanceOnly = true
	r.Performance.OutputTokens = nil
	ingestPerformance(t, s, r)
	r = performanceFixture("late-output", 1000, 20000, at)
	r.PerformanceOnly = true
	r.Performance.TimingBasis = core.TimingInferredLogBoundary
	r.Performance.Revision = 2
	ingestPerformance(t, s, r)
	p = readPerformance(t, path, at, "personal")
	if p.Summary.Coverage.Direct != 2 || p.Summary.Stats.DurationMS != 20000 || p.Summary.Stats.OutputTokens != 1500 {
		t.Fatalf("missing output lost independent timing: %+v", p.Summary)
	}
}

func TestPerformanceReadModelLeavesTokenCostAndQuotaUnchanged(t *testing.T) {
	ctx := context.Background()
	s, path := performanceTestStore(t, true)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	ingestPerformance(t, s, performanceFixture("request", 3000, 40000, at))
	limit, used, remaining := 100.0, 40.0, 60.0
	base := map[string]core.UsageSnapshot{"personal": {ProviderID: "claude_code", AccountID: "personal", Timestamp: at, Status: core.StatusOK, Metrics: map[string]core.Metric{"quota": {Limit: &limit, Used: &used, Remaining: &remaining, Unit: "requests", Window: "day"}}}}
	if err := NewQuotaSnapshotIngestor(s).Ingest(ctx, base); err != nil {
		t.Fatal(err)
	}
	opts := ReadModelOptions{Since: at.Add(-time.Hour), Now: at.Add(time.Second), TimeWindow: core.TimeWindowAll}
	off, err := ApplyCanonicalTelemetryViewWithOptions(ctx, path, base, opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.PerformanceEnabled = true
	on, err := ApplyCanonicalTelemetryViewWithOptions(ctx, path, base, opts)
	if err != nil {
		t.Fatal(err)
	}
	a, b := off["personal"], on["personal"]
	if b.Performance == nil || b.Performance.Summary.Coverage.Measured != 1 {
		t.Fatal("missing enriched snapshot")
	}
	if !reflect.DeepEqual(a.Metrics, b.Metrics) || !reflect.DeepEqual(a.ModelUsage, b.ModelUsage) || !reflect.DeepEqual(a.DailySeries, b.DailySeries) || !reflect.DeepEqual(a.Resets, b.Resets) || a.Status != b.Status {
		t.Fatal("performance changed original accounting/quota projection")
	}
}

func TestPerformanceIncrementalCacheCorrectionAndBackwardClock(t *testing.T) {
	ctx := context.Background()
	s, path := performanceTestStore(t, true)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := performanceFixture("request", 6000, 60000, at)
	r.PerformanceOnly = true
	ingestPerformance(t, s, r)
	s.RecordPerformanceScan(ctx, "claude_code", "personal", at.Add(-5*time.Minute))
	s.RecordPerformanceScan(ctx, "claude_code", "personal", at)
	q := PerformanceQuery{Source: "claude_code", Account: "personal", Since: at.Add(-time.Hour), Now: at, IncludeWindow: true}
	first, err := QueryPerformance(ctx, path, q)
	if err != nil {
		t.Fatal(err)
	}
	if first.Window.Latest60Seconds.OutputTokens != 6000 {
		t.Fatal("missing original interval")
	}
	q.Now = at.Add(30 * time.Second)
	if _, err := QueryPerformance(ctx, path, q); err != nil {
		t.Fatal(err)
	}
	q.Now = at
	restored, err := QueryPerformance(ctx, path, q)
	if err != nil || restored.Window.Latest60Seconds.OutputTokens != 6000 {
		t.Fatalf("clock reversal lost cache: %v", err)
	}
	// Correct native interval into a period older than the cache's lower bound.
	older := at.Add(-2 * time.Hour)
	r = performanceFixture("request", 3000, 40000, older)
	r.PerformanceOnly = true
	r.Performance.Revision = 2
	ingestPerformance(t, s, r)
	removed, err := QueryPerformance(ctx, path, q)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Summary.Coverage.Candidates != 0 || removed.Window.Latest60Seconds.OutputTokens != 0 {
		t.Fatal("corrected historical request left ghost ring contribution")
	}
}

func TestPerformanceRollingPeriodUsesIncrementalCache(t *testing.T) {
	ctx := context.Background()
	s, path := performanceTestStore(t, true)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := performanceFixture("native", 1000, 10000, at)
	r.PerformanceOnly = true
	ingestPerformance(t, s, r)
	q := PerformanceQuery{Source: "claude_code", Account: "personal", Since: at.Add(-7 * 24 * time.Hour), Now: at.Add(time.Second), IncludeWindow: true}
	first, err := QueryPerformance(ctx, path, q)
	if err != nil {
		t.Fatal(err)
	}
	if first.Summary.Coverage.Candidates != 1 {
		t.Fatal("initial load")
	}
	// An unchanged sequence must not re-read historical JSON on every moving
	// period. The intentionally corrupted temp row detects an accidental cold
	// reload; real writes always advance change_seq and are covered separately.
	if _, err := s.db.Exec(`UPDATE performance_requests SET record_json='invalid-test-json'`); err != nil {
		t.Fatal(err)
	}
	q.Since = q.Since.Add(time.Second)
	q.Now = q.Now.Add(time.Second)
	next, err := QueryPerformance(ctx, path, q)
	if err != nil {
		t.Fatalf("rolling cutoff caused full reload: %v", err)
	}
	if next.Summary.Coverage.Candidates != 1 || !next.Period.Start.Equal(q.Since) {
		t.Fatal("rolling filter changed")
	}
}
