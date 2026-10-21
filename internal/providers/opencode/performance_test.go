package opencode

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/performance"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

func TestV2PerformanceOutputOnlyAndAccountingInvariant(t *testing.T) {
	db, path := newV2Fixture(t)
	putV2Message(t, db, "message", "parent", "assistant", `{"model":{"id":"model","providerID":"billing"},"tokens":{"input":900,"output":150,"reasoning":100,"cache":{"read":99,"write":1}},"cost":1.25,"finish":"stop","time":{"created":1780000000000,"streamed":1780000001999,"completed":1780000002000},"content":[{"type":"text","text":"PRIVATE_PERFORMANCE_SENTINEL"}]}`)
	off, err := CollectTelemetryFromSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	on, err := CollectTelemetryFromSQLite(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(on) != 1 || on[0].Performance == nil {
		t.Fatal("missing request performance")
	}
	r := performance.NormalizeRequest(*on[0].Performance)
	if r.ResponseTokS == nil || *r.ResponseTokS != 75 || *r.OutputTokens != 150 || r.TimingBasis != core.TimingProviderRequestTimestamps {
		t.Fatalf("wrong speed: %+v", r)
	}
	if r.DurationMS == nil || *r.DurationMS != 2000 {
		t.Fatal("used stream time or database update time")
	}
	on[0].Performance = nil
	if !reflect.DeepEqual(off, on) {
		t.Fatal("opt-in changed token/cost/tool accounting")
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "PRIVATE_PERFORMANCE_SENTINEL") || strings.Contains(string(encoded), path) {
		t.Fatal("private source content/path leaked")
	}
}

func TestV2PerformanceZeroMissingFailedPartialAndCorrections(t *testing.T) {
	db, path := newV2Fixture(t)
	for _, c := range []struct{ id, body string }{
		{"zero", `{"tokens":{"output":0,"reasoning":100},"finish":"stop","time":{"created":1780000000000,"completed":1780000002000}}`},
		{"missing", `{"tokens":{"input":2},"finish":"stop","time":{"created":1780000000000,"completed":1780000002000}}`},
		{"failed", `{"error":{"private":"PRIVATE_PERFORMANCE_SENTINEL"},"finish":"error","time":{"created":1780000000000,"completed":1780000002000}}`},
		{"partial", `{"tokens":{"output":10},"time":{"created":1780000000000}}`},
		{"no_clock", `{"tokens":{"output":10},"finish":"stop"}`},
		{"inverted", `{"tokens":{"output":10},"finish":"stop","time":{"created":1780000002000,"completed":1780000000000}}`},
		{"bad_units", `{"tokens":{"output":10},"finish":"stop","time":{"created":"1780000000000","completed":1780000002000}}`},
		{"seconds_units", `{"tokens":{"output":10},"finish":"stop","time":{"created":1780000000,"completed":1780000002}}`},
		{"microseconds_units", `{"tokens":{"output":10},"finish":"stop","time":{"created":1780000000000000,"completed":1780000002000000}}`},
		{"nanoseconds_units", `{"tokens":{"output":10},"finish":"stop","time":{"created":1780000000000000000,"completed":1780000002000000000}}`},
	} {
		putV2Message(t, db, c.id, "parent", "assistant", c.body)
	}
	events, err := CollectTelemetryFromSQLite(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]core.PerformanceRequest{}
	for _, e := range events {
		if e.Performance == nil {
			t.Fatal("missing performance metadata")
		}
		got[e.MessageID] = performance.NormalizeRequest(*e.Performance)
		if e.MessageID == "failed" && !e.PerformanceOnly {
			t.Fatal("failed no-usage row became accounting event")
		}
	}
	if got["zero"].ResponseTokS == nil || *got["zero"].ResponseTokS != 0 {
		t.Fatal("explicit zero lost")
	}
	for _, id := range []string{"missing", "failed", "partial", "no_clock", "inverted", "bad_units", "seconds_units", "microseconds_units", "nanoseconds_units"} {
		if got[id].ResponseTokS != nil {
			t.Fatalf("%s unexpectedly measured", id)
		}
	}
	if got["failed"].Outcome != core.OutcomeFailed || got["partial"].Final {
		t.Fatal("outcome/finality confused")
	}
	before := got["zero"]
	putV2Message(t, db, "zero", "parent", "assistant", `{"tokens":{"output":4},"finish":"stop","time":{"created":1780000000000,"completed":1780000002000}}`)
	events, err = CollectTelemetryFromSQLite(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.MessageID == "zero" && (e.Performance.ID != before.ID || e.Performance.Revision <= before.Revision) {
			t.Fatal("correction changed identity or lost source revision")
		}
	}
}

func TestV2PerformanceDirectDurationHasNoInferenceClamp(t *testing.T) {
	db, path := newV2Fixture(t)
	putV2Message(t, db, "short", "parent", "assistant", `{"tokens":{"output":1},"finish":"stop","time":{"created":1780000000000,"completed":1780000000001}}`)
	putV2Message(t, db, "long", "parent", "assistant", `{"tokens":{"output":1200},"finish":"stop","time":{"created":1780000000000,"completed":1780001200000}}`)
	events, err := CollectTelemetryFromSQLite(context.Background(), path, true)
	if err != nil || len(events) != 2 {
		t.Fatal("direct clock fixture failed")
	}
	for _, ev := range events {
		r := performance.NormalizeRequest(*ev.Performance)
		if r.ResponseTokS == nil || r.TimingStatus != core.TimingValid {
			t.Fatal("direct interval inherited an inference duration cutoff")
		}
	}
}

func TestV2PerformanceCacheSeparatesOptIn(t *testing.T) {
	db, path := newV2Fixture(t)
	putV2Message(t, db, "message", "parent", "assistant", `{"tokens":{"output":150},"finish":"stop","time":{"created":1780000000000,"completed":1780000002000}}`)
	p := New()
	opts := shared.TelemetryCollectOptions{Paths: map[string]string{"db_path": path, "account_id": "personal"}, PathLists: map[string][]string{"events_dirs": {t.TempDir()}}}
	first, err := p.Collect(context.Background(), opts)
	if err != nil || len(first) != 1 || first[0].Performance != nil {
		t.Fatal("default should not collect performance")
	}
	opts.PerformanceEnabled = true
	second, err := p.Collect(context.Background(), opts)
	if err != nil || len(second) != 1 || second[0].Performance == nil {
		t.Fatal("old fingerprint hid opt-in metadata")
	}
	third, err := p.Collect(context.Background(), opts)
	if err != nil || len(third) != 0 {
		t.Fatal("unchanged database replayed")
	}
}
