package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestPerformanceHubHTTPPreservesFieldsAndSeparatesHosts(t *testing.T) {
	srv, store := newTestServer(t)
	now := time.Now().UTC()
	for _, host := range []string{"host-a", "host-b"} {
		snap := makeSnap("codex", "personal")
		out := int64(100)
		snap.Performance = &core.PerformanceSnapshot{
			DefinitionVersion: core.PerformanceDefinitionVersion, HostID: host, DataAsOf: now, GeneratedAt: now, ExportIntervalMS: 60000,
			Summary:  core.PerformanceSummary{Coverage: core.PerformanceCoverage{Candidates: 2, Measured: 1, Direct: 1, Excluded: 1, Reasons: map[string]int64{"missing_timing": 1}}, Direct: core.PerformanceStats{Count: 1, WeightedResponseTokS: core.Float64Ptr(5), Histogram: core.PerformanceHistogram{Buckets: map[int]int64{2: 1}}}},
			Requests: []core.PerformanceRequest{{ID: "same-id", HostID: host, AccountID: "personal", OutputTokens: &out, Evidence: []string{"direct_duration"}}},
			Window:   &core.PerformanceWindow{Latest60Seconds: core.PerformanceRateWindow{WallTokS: core.Float64Ptr(0)}},
		}
		body, err := json.Marshal(core.RemoteEnvelope{Machine: host, SentAt: now, Snapshots: []core.UsageSnapshot{snap}})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		srv.handlePush(w, httptest.NewRequest(http.MethodPost, "/v1/push", bytes.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("push status %d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	srv.handleSnapshots(w, httptest.NewRequest(http.MethodGet, "/v1/snapshots", nil))
	var received map[string]core.UsageSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &received); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 {
		t.Fatalf("hosts merged: got %d snapshots", len(received))
	}
	for _, host := range []string{"host-a", "host-b"} {
		p := received[host+":codex:personal"].Performance
		if p == nil || p.HostID != host || p.Requests[0].HostID != host || p.Requests[0].AccountID != "personal" || p.Summary.Coverage.Candidates != 2 || *p.Summary.Direct.WeightedResponseTokS != 5 {
			t.Fatalf("host %s lost its performance contract", host)
		}
		if p.Summary.Inferred.WeightedResponseTokS != nil || p.Requests[0].DurationMS != nil || p.Window.Latest60Seconds.WallTokS == nil || *p.Window.Latest60Seconds.WallTokS != 0 {
			t.Fatal("null timing and valid zero were conflated")
		}
	}
	// Store reads must not expose mutable references into its latest snapshot.
	view := store.Snapshots()["host-a:codex:personal"].Performance
	view.Summary.Coverage.Reasons["missing_timing"] = 9
	view.Summary.Direct.Histogram.Buckets[2] = 9
	*view.Summary.Direct.WeightedResponseTokS = 9
	*view.Requests[0].OutputTokens = 9
	view.Requests[0].Evidence[0] = "changed"
	*view.Window.Latest60Seconds.WallTokS = 9
	stored := store.Snapshots()["host-a:codex:personal"].Performance
	if stored.Summary.Coverage.Reasons["missing_timing"] != 1 || stored.Summary.Direct.Histogram.Buckets[2] != 1 || *stored.Summary.Direct.WeightedResponseTokS != 5 || *stored.Requests[0].OutputTokens != 100 || stored.Requests[0].Evidence[0] != "direct_duration" || *stored.Window.Latest60Seconds.WallTokS != 0 {
		t.Fatal("returned performance object aliases Hub storage")
	}
}

func TestPerformanceOptionalJSONLegacyCompatibility(t *testing.T) {
	// This mirrors the old typed Hub behavior: unknown fields are accepted but
	// discarded during decoding. Actual old/new binaries are tested separately;
	// an original Hub must not be advertised as forwarding performance fields.
	type legacySnapshot struct {
		ProviderID string                 `json:"provider_id"`
		AccountID  string                 `json:"account_id"`
		Status     core.Status            `json:"status"`
		Metrics    map[string]core.Metric `json:"metrics"`
	}
	snap := makeSnap("codex", "personal")
	legacy, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var newer core.UsageSnapshot
	if err := json.Unmarshal(legacy, &newer); err != nil || newer.Performance != nil {
		t.Fatalf("legacy snapshot rejected or timing invented: %v", err)
	}
	if strings.Contains(string(legacy), `"performance"`) {
		t.Fatal("optional performance field was serialized without observations")
	}
	snap.Performance = &core.PerformanceSnapshot{DefinitionVersion: core.PerformanceDefinitionVersion}
	updated, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var old legacySnapshot
	if err := json.Unmarshal(updated, &old); err != nil {
		t.Fatalf("new snapshot rejected by legacy schema: %v", err)
	}
	if old.ProviderID != snap.ProviderID || old.AccountID != snap.AccountID || old.Status != snap.Status || len(old.Metrics) != len(snap.Metrics) {
		t.Fatal("legacy usage fields changed")
	}
	roundTrip, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(roundTrip), `"performance"`) {
		t.Fatal("legacy struct unexpectedly preserved unknown fields")
	}
}
