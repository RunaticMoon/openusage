package exporter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/config"
	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestHourlyExporterOwnsCloneAndPreservesKnownZeroAndCoverage(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	exporter, err := New(config.ExportConfig{Target: server.URL, MachineName: "hourly-host"})
	if err != nil {
		t.Fatal(err)
	}
	snap := core.NewUsageSnapshot("claude_code", "personal")
	snap.HourlySeries = map[string][]core.TimePoint{
		"analytics_tokens":              {{Date: "2026-10-23T10:00:00Z", Value: 20}},
		"analytics_work_tokens":         {{Date: "2026-10-23T10:00:00Z", Value: 0}},
		"analytics_token_split_partial": {{Date: "2026-10-23T10:00:00Z", Value: 1}},
	}
	want := snap.DeepClone()
	exporter.Ingest(map[string]core.UsageSnapshot{"personal": snap})
	snap.HourlySeries["analytics_tokens"][0].Value = 999
	delete(snap.HourlySeries, "analytics_token_split_partial")
	exporter.tick(context.Background())
	var envelope core.RemoteEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Machine != "hourly-host" || len(envelope.Snapshots) != 1 {
		t.Fatalf("bad export: %+v", envelope)
	}
	if !reflect.DeepEqual(envelope.Snapshots[0].HourlySeries, want.HourlySeries) {
		t.Fatalf("hourly export aliased or changed: %+v", envelope.Snapshots[0].HourlySeries)
	}
}
