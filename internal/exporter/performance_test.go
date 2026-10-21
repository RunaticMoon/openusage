package exporter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/config"
	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestPerformanceExportEnrichesPrivateCloneAndPreservesUnknowns(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	e, err := New(config.ExportConfig{Target: server.URL, MachineName: "synthetic-host", IntervalSeconds: 90})
	if err != nil {
		t.Fatal(err)
	}
	output := int64(120)
	now := time.Now().UTC()
	p := &core.PerformanceSnapshot{
		DefinitionVersion: core.PerformanceDefinitionVersion,
		DataAsOf:          now.Add(-time.Second), GeneratedAt: now,
		Summary: core.PerformanceSummary{
			Coverage: core.PerformanceCoverage{Candidates: 2, Measured: 1, Direct: 1, Excluded: 1, Reasons: map[string]int64{"missing_timing": 1}},
			Direct:   core.PerformanceStats{Count: 1, WeightedResponseTokS: core.Float64Ptr(12), Histogram: core.PerformanceHistogram{Buckets: map[int]int64{3: 1}}},
		},
		Groups:   []core.PerformanceGroup{{Dimension: "model", Key: "synthetic-model"}, {Dimension: "source", Key: "codex", HostID: "explicit-host"}},
		Requests: []core.PerformanceRequest{{ID: "stable-id", AccountID: "personal", OutputTokens: &output, SourcePath: "/PRIVATE/source.jsonl", Evidence: []string{"direct_duration"}}},
		Window:   &core.PerformanceWindow{Session120Seconds: []core.PerformanceSessionRate{{SessionID: "synthetic-session"}}},
	}
	snap := core.NewUsageSnapshot("codex", "personal")
	snap.Performance = p
	e.Ingest(map[string]core.UsageSnapshot{"personal": snap})
	if p.HostID != "" || p.ExportIntervalMS != 0 || p.Requests[0].HostID != "" || p.Groups[0].HostID != "" {
		t.Fatal("export enrichment mutated the original snapshot")
	}
	// The producer may update its read model after handoff. The exporter owns a
	// complete copy, including nested optional rates and histogram maps.
	p.Summary.Coverage.Reasons["missing_timing"] = 9
	p.Summary.Direct.Histogram.Buckets[3] = 9
	*p.Summary.Direct.WeightedResponseTokS = 99
	*p.Requests[0].OutputTokens = 999
	p.Requests[0].Evidence[0] = "changed"
	p.Window.Session120Seconds[0].SessionID = "changed"
	e.tick(context.Background())
	var env core.RemoteEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Snapshots) != 1 || env.Snapshots[0].Performance == nil {
		t.Fatal("performance snapshot was not exported")
	}
	got := env.Snapshots[0].Performance
	if got.HostID != "synthetic-host" || got.ExportIntervalMS != 90000 || got.Groups[0].HostID != "synthetic-host" || got.Groups[1].HostID != "explicit-host" || got.Requests[0].HostID != "synthetic-host" || got.Window.Session120Seconds[0].HostID != "synthetic-host" {
		t.Fatal("transport identity or interval missing, or explicit provenance overwritten")
	}
	if got.Requests[0].ID != "stable-id" || *got.Requests[0].OutputTokens != 120 || got.Requests[0].Evidence[0] != "direct_duration" || got.Window.Session120Seconds[0].SessionID != "synthetic-session" {
		t.Fatal("stable request identity/data changed after handoff")
	}
	if *got.Summary.Direct.WeightedResponseTokS != 12 || got.Summary.Direct.Histogram.Buckets[3] != 1 || got.Summary.Coverage.Reasons["missing_timing"] != 1 {
		t.Fatal("nested aggregate data aliased the producer")
	}
	if got.Summary.Inferred.WeightedResponseTokS != nil || got.Requests[0].DurationMS != nil || !got.DataAsOf.Equal(now.Add(-time.Second)) || !got.GeneratedAt.Equal(now) {
		t.Fatal("export invented timing or altered source freshness")
	}
	if strings.Contains(string(body), "PRIVATE") || strings.Contains(string(body), "source_path") {
		t.Fatal("local source path escaped into exported JSON")
	}
}

func TestPerformanceAbsentFromLegacyExport(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	e, err := New(config.ExportConfig{Target: server.URL, MachineName: "legacy-host"})
	if err != nil {
		t.Fatal(err)
	}
	e.Ingest(map[string]core.UsageSnapshot{"personal": core.NewUsageSnapshot("codex", "personal")})
	e.tick(context.Background())
	if strings.Contains(string(body), `"performance"`) {
		t.Fatal("an unsupported/old source acquired a fake performance section")
	}
}
