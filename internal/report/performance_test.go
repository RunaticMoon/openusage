package report

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/performance"
)

func TestPerformanceReportPreservesTokenCostAndJSONContract(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	out := int64(3000)
	duration := float64(40000)
	start := at.Add(-40 * time.Second)
	r := core.PerformanceRequest{ID: "request", Namespace: "test", SourceSystem: "claude_code", AccountID: "personal", OutputTokens: &out, OutputBasis: "output", StartedAt: &start, EndedAt: &at, DurationMS: &duration, RecordedAt: at, TimingBasis: core.TimingInferredLogBoundary, TimingStatus: core.TimingValid, Scope: core.ScopeModelRequest, Final: true, Outcome: core.OutcomeSuccess}
	p, err := performance.Build([]core.PerformanceRequest{r}, performance.BuildOptions{Period: core.PerformancePeriod{Start: at.Add(-time.Hour), End: at.Add(time.Second)}, Now: at.Add(time.Second), DataAsOf: at, IncludeWindow: true})
	if err != nil {
		t.Fatal(err)
	}
	rep := Build([]Event{{Time: at, Provider: "claude_code", Model: "model", Session: "session", Input: 100, Output: 3000, CacheRead: 200, Reasoning: 30, Cost: 1.25}}, Options{Kind: KindSession, Now: at.Add(time.Second)})
	before := rep.Totals
	rep.Performance = p
	if !reflect.DeepEqual(before, rep.Totals) {
		t.Fatal("performance changed accounting")
	}
	var encoded bytes.Buffer
	if err := rep.WriteJSON(&encoded); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(encoded.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got["performance"]) == 0 {
		t.Fatal("JSON dropped performance")
	}
	var table bytes.Buffer
	if err := rep.WriteTable(&table); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"75.000 tok/s", "log inferred 1", "time-allocation estimate", "not decode speed"} {
		if !strings.Contains(table.String(), text) {
			t.Errorf("missing %q", text)
		}
	}
	rep.Performance = nil
	encoded.Reset()
	rep.WriteJSON(&encoded)
	if strings.Contains(encoded.String(), `"performance"`) {
		t.Fatal("old report JSON gained non-optional field")
	}
	if performanceNumber(nil) != "—" {
		t.Fatal("missing must be dash")
	}
	zero := float64(0)
	if performanceNumber(&zero) != "0.000" {
		t.Fatal("zero must be numeric")
	}
}
