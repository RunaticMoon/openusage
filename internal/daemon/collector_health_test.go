package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/config"
	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
	"github.com/mattn/go-sqlite3"
)

type healthClock struct{ at time.Time }

func (c *healthClock) Now() time.Time { return c.at }

type healthCollector struct {
	requests []telemetry.IngestRequest
	err      error
}

func (c *healthCollector) Name() string { return "fixture" }
func (c *healthCollector) Collect(context.Context) ([]telemetry.IngestRequest, error) {
	return c.requests, c.err
}

type codedCollectionError string

func (e codedCollectionError) Error() string {
	return "private path, token and payload must not escape"
}
func (e codedCollectionError) CollectionErrorCode() string { return string(e) }

func TestCollectorHealthTransitionsAndPrivacy(t *testing.T) {
	clock := &healthClock{time.Date(2026, 10, 21, 12, 0, 0, 0, time.FixedZone("KST", 9*3600))}
	svc := &Service{clock: clock}
	eventTime := clock.at.Add(-24 * time.Hour)
	collector := &healthCollector{requests: []telemetry.IngestRequest{
		{EventType: telemetry.EventTypeMessageUsage, OccurredAt: eventTime},
		{EventType: telemetry.EventTypeRawEnvelope, OccurredAt: clock.at},
		{EventType: telemetry.EventTypeLimitSnapshot, OccurredAt: clock.at},
	}}
	if _, err := svc.collectWithHealth(context.Background(), collector); err != nil {
		t.Fatal(err)
	}
	key := collectorHealthKey("source", "fixture", "")
	first := svc.collectorHealthSnapshot()[key]
	if first.Status != "ok" || first.CollectedCount != 3 || !first.LastEventAt.Equal(eventTime) || !first.LastSuccessAt.Equal(clock.at) || first.LastAttemptAt.Location() != time.UTC {
		t.Fatalf("initial health=%+v", first)
	}
	clock.at = clock.at.Add(time.Hour)
	collector.requests = nil
	_, _ = svc.collectWithHealth(context.Background(), collector)
	empty := svc.collectorHealthSnapshot()[key]
	if empty.Status != "no_new_events" || empty.CollectedCount != 0 || !empty.LastEventAt.Equal(eventTime) || !empty.LastSuccessAt.After(first.LastSuccessAt) {
		t.Fatalf("empty read=%+v", empty)
	}
	clock.at = clock.at.Add(time.Hour)
	collector.err = fmt.Errorf("private payload: %w", codedCollectionError("unsupported_schema"))
	_, _ = svc.collectWithHealth(context.Background(), collector)
	failed := svc.collectorHealthSnapshot()[key]
	if failed.Status != "unsupported_schema" || !failed.LastSuccessAt.Equal(empty.LastSuccessAt) || !failed.LastEventAt.Equal(eventTime) || !failed.LastAttemptAt.After(empty.LastAttemptAt) {
		t.Fatalf("failed read=%+v", failed)
	}
	w := httptest.NewRecorder()
	svc.handleHealth(w, httptest.NewRequest("GET", "/healthz", nil))
	if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "payload") || strings.Contains(w.Body.String(), "token") {
		t.Fatalf("sensitive error leaked: %s", w.Body.String())
	}
	var health HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health.Status != "ok" || health.Collectors[key].Status != "unsupported_schema" {
		t.Fatal("liveness changed with source error")
	}
	clock.at = clock.at.Add(time.Hour)
	collector.err = nil
	collector.requests = []telemetry.IngestRequest{{EventType: telemetry.EventTypeToolUsage, OccurredAt: clock.at.Add(-time.Minute)}}
	_, _ = svc.collectWithHealth(context.Background(), collector)
	recovered := svc.collectorHealthSnapshot()[key]
	if recovered.Status != "ok" || !recovered.LastSuccessAt.After(empty.LastSuccessAt) || !recovered.LastEventAt.After(eventTime) {
		t.Fatalf("recovery=%+v", recovered)
	}
}

func TestCollectorErrorClassification(t *testing.T) {
	syntaxErr := json.Unmarshal([]byte(`{broken`), new(any))
	var typeErr error = json.Unmarshal([]byte(`{"count":"bad"}`), &struct {
		Count int `json:"count"`
	}{})
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", os.ErrNotExist), "missing"},
		{fmt.Errorf("wrapped: %w", os.ErrPermission), "permission"},
		{fmt.Errorf("wrapped: %w", sqlite3.Error{Code: sqlite3.ErrBusy}), "database_busy"},
		{sqlite3.Error{Code: sqlite3.ErrLocked}, "database_busy"},
		{syntaxErr, "parse_error"}, {typeErr, "parse_error"},
		{fmt.Errorf("wrapped: %w", codedCollectionError("unsupported_schema")), "unsupported_schema"},
		{codedCollectionError("parse_error"), "parse_error"},
		{codedCollectionError("SECRET_MUST_NOT_ESCAPE"), "error"},
		{errors.New("SECRET_MUST_NOT_ESCAPE"), "error"},
	} {
		if got := collectorErrorCode(tc.err); got != tc.want {
			t.Fatalf("code=%q want=%q", got, tc.want)
		}
	}
}

func TestCollectorHealthJSONCompatibility(t *testing.T) {
	var modern HealthResponse
	if err := json.Unmarshal([]byte(`{"status":"ok","api_version":"v1"}`), &modern); err != nil || modern.Collectors != nil {
		t.Fatalf("old response rejected: %v", err)
	}
	data, err := json.Marshal(HealthResponse{Status: "ok", APIVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "collectors") {
		t.Fatal("empty extension should be omitted")
	}
	svc := &Service{}
	at := svc.beginCollectorAttempt("source", "fixture", "account-a")
	svc.finishCollectorAttempt("source", "fixture", "account-a", at, "missing", 0, time.Time{}, false)
	w := httptest.NewRecorder()
	svc.handleHealth(w, httptest.NewRequest("GET", "/healthz", nil))
	var legacy struct {
		Status string `json:"status"`
		API    string `json:"api_version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &legacy); err != nil || legacy.Status != "ok" || legacy.API != "v1" {
		t.Fatalf("legacy decoder rejected extended health: %v", err)
	}
	if strings.Contains(w.Body.String(), "0001-") {
		t.Fatal("unknown timestamp was serialized as an actual date")
	}
	if err := json.Unmarshal([]byte(`{"status":"ok","collectors":{"source/x/":{"source":"x","kind":"source","status":"no_new_events","collected_count":0,"future_field":true}},"future":1}`), &modern); err != nil {
		t.Fatal(err)
	}
	if modern.Collectors["source/x/"].Status != "no_new_events" {
		t.Fatal("known extension was not decoded")
	}
}

func TestProviderHealthAuthRecoveryDoesNotInventUsageTime(t *testing.T) {
	clock := &healthClock{time.Now()}
	svc := &Service{clock: clock}
	key := collectorHealthKey("provider_poll", "claude_code", "personal")
	at := svc.beginCollectorAttempt("provider_poll", "claude_code", "personal")
	svc.recordProviderHealth("claude_code", "personal", at, core.UsageSnapshot{Status: core.StatusAuth, Timestamp: clock.at, Message: "private authentication detail"}, nil)
	auth := svc.collectorHealthSnapshot()[key]
	if auth.Status != "auth_required" || !auth.LastSuccessAt.IsZero() || !auth.LastEventAt.IsZero() {
		t.Fatalf("auth health=%+v", auth)
	}
	for _, status := range []core.Status{core.StatusOK, core.StatusNearLimit, core.StatusLimited} {
		clock.at = clock.at.Add(time.Minute)
		at = svc.beginCollectorAttempt("provider_poll", "claude_code", "personal")
		snap := core.UsageSnapshot{Status: status, Timestamp: clock.at}
		svc.recordProviderHealth("claude_code", "personal", at, snap, nil)
		got := svc.collectorHealthSnapshot()[key]
		if got.Status != "ok" || got.LastSuccessAt.IsZero() || !got.LastEventAt.IsZero() || snap.Status != status {
			t.Fatalf("quota status changed or observation became event time: %+v", got)
		}
	}
}

type healthProvider struct {
	core.UsageProvider
	calls int
}

func (p *healthProvider) Fetch(context.Context, core.AccountConfig) (core.UsageSnapshot, error) {
	p.calls++
	return core.UsageSnapshot{ProviderID: "codex", AccountID: "personal", Status: core.StatusOK, Timestamp: time.Now()}, nil
}
func (p *healthProvider) Spec() core.ProviderSpec                                { return core.ProviderSpec{} }
func (p *healthProvider) HasChanged(core.AccountConfig, time.Time) (bool, error) { return false, nil }

func TestProviderHealthCachedSkipsDoNotAdvanceAttempt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	cfg := config.DefaultConfig()
	cfg.AutoDetect = false
	cfg.Accounts = []core.AccountConfig{{ID: "personal", Provider: "codex", Auth: "local"}}
	cfg.AutoDetectedAccounts = nil
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	clock := &healthClock{time.Now()}
	provider := &healthProvider{}
	svc := &Service{clock: clock, quotaIngest: telemetry.NewQuotaSnapshotIngestor(nil), providerByID: map[string]core.UsageProvider{"codex": provider}, pollScheduler: newPollScheduler(time.Hour), pollState: make(map[string]*providerPollState), logThrottle: core.NewLogThrottle(20, time.Minute)}
	svc.pollProviders(context.Background())
	key := collectorHealthKey("provider_poll", "codex", "personal")
	first := svc.collectorHealthSnapshot()[key]
	if provider.calls != 1 || first.LastAttemptAt.IsZero() {
		t.Fatalf("initial provider calls=%d health=%+v", provider.calls, first)
	}
	clock.at = clock.at.Add(time.Hour)
	svc.pollProviders(context.Background())
	if provider.calls != 1 || !svc.collectorHealthSnapshot()[key].LastAttemptAt.Equal(first.LastAttemptAt) {
		t.Fatal("backoff cache advanced fetch attempt")
	}
	// Pass the scheduler gate, then exercise ChangeDetector's unchanged skip.
	svc.pollScheduler.mu.Lock()
	svc.pollScheduler.states["personal"].lastPollAt = time.Now().Add(-2 * time.Hour)
	svc.pollScheduler.mu.Unlock()
	svc.pollProviders(context.Background())
	if provider.calls != 1 || !svc.collectorHealthSnapshot()[key].LastAttemptAt.Equal(first.LastAttemptAt) {
		t.Fatal("unchanged source advanced fetch attempt")
	}
}

func TestCollectorHealthConcurrentReadsAndWrites(t *testing.T) {
	svc := &Service{}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				account := fmt.Sprintf("account-%d", i)
				at := svc.beginCollectorAttempt("source", "fixture", account)
				svc.finishCollectorAttempt("source", "fixture", account, at, "no_new_events", 0, time.Time{}, true)
				copy := svc.collectorHealthSnapshot()
				delete(copy, collectorHealthKey("source", "fixture", account))
				_, _ = json.Marshal(copy)
			}
		}()
	}
	wg.Wait()
	if len(svc.collectorHealthSnapshot()) != 8 {
		t.Fatal("snapshot caller mutated shared state")
	}
}
