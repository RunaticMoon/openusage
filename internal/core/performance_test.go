package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPerformanceOptionalJSONAndDeepClone(t *testing.T) {
	old := UsageSnapshot{ProviderID: "fixture"}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"performance"`) {
		t.Fatal("optional field altered old payload")
	}
	var decoded UsageSnapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Performance != nil {
		t.Fatal("old payload fabricated performance")
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	output := int64(0)
	duration := 1000.
	rate := 0.
	stats := PerformanceStats{WeightedResponseTokS: &rate, P50TokS: &rate, P90TokS: &rate, MinTokS: &rate, MaxTokS: &rate, Histogram: PerformanceHistogram{Buckets: map[int]int64{2: 1}}}
	summary := PerformanceSummary{Coverage: PerformanceCoverage{Reasons: map[string]int64{"missing": 1}, OutputBases: map[string]int64{"provider_output_tokens": 1}}, Stats: stats, Direct: stats, Inferred: stats}
	old.Performance = &PerformanceSnapshot{DefinitionVersion: PerformanceDefinitionVersion, Summary: summary, Groups: []PerformanceGroup{{Dimension: "source", Summary: summary}}, ExcludedHosts: []string{"old-machine"}, Requests: []PerformanceRequest{{ID: "r", OutputTokens: &output, StartedAt: &now, EndedAt: &now, DurationMS: &duration, ResponseTokS: &rate, Evidence: []string{"native_counter"}, SourcePath: "/private/do-not-export"}}, Window: &PerformanceWindow{Buckets: []PerformanceBucket{{OutputTokens: 1}}, CoverageStart: &now, ExcludedReasons: map[string]int64{"missing": 1}, Latest60Seconds: PerformanceRateWindow{WallTokS: &rate}, Session120Seconds: []PerformanceSessionRate{{Rate: PerformanceRateWindow{WallTokS: &rate}}}}}
	clone := old.DeepClone()
	p := clone.Performance
	*p.Requests[0].OutputTokens = 99
	*p.Requests[0].DurationMS = 7
	*p.Requests[0].StartedAt = now.Add(time.Hour)
	*p.Requests[0].ResponseTokS = 44
	p.Requests[0].Evidence[0] = "changed"
	p.Summary.Coverage.Reasons["missing"] = 55
	p.Summary.Coverage.OutputBases["provider_output_tokens"] = 55
	p.Summary.Stats.Histogram.Buckets[2] = 55
	*p.Summary.Stats.P50TokS = 55
	p.Groups[0].Summary.Direct.Histogram.Buckets[2] = 55
	p.ExcludedHosts[0] = "changed"
	p.Window.Buckets[0].OutputTokens = 55
	*p.Window.CoverageStart = now.Add(time.Hour)
	p.Window.ExcludedReasons["missing"] = 55
	*p.Window.Latest60Seconds.WallTokS = 55
	*p.Window.Session120Seconds[0].Rate.WallTokS = 55
	if *old.Performance.Requests[0].OutputTokens != 0 || *old.Performance.Requests[0].DurationMS != 1000 || !old.Performance.Requests[0].StartedAt.Equal(now) || *old.Performance.Requests[0].ResponseTokS != 0 || old.Performance.Requests[0].Evidence[0] != "native_counter" || old.Performance.Summary.Coverage.Reasons["missing"] != 1 || old.Performance.Summary.Coverage.OutputBases["provider_output_tokens"] != 1 || old.Performance.Summary.Stats.Histogram.Buckets[2] != 1 || *old.Performance.Summary.Stats.P50TokS != 0 || old.Performance.Groups[0].Summary.Direct.Histogram.Buckets[2] != 1 || old.Performance.ExcludedHosts[0] != "old-machine" || old.Performance.Window.Buckets[0].OutputTokens != 1 || !old.Performance.Window.CoverageStart.Equal(now) || old.Performance.Window.ExcludedReasons["missing"] != 1 || *old.Performance.Window.Latest60Seconds.WallTokS != 0 || *old.Performance.Window.Session120Seconds[0].Rate.WallTokS != 0 {
		t.Fatal("performance clone aliases source")
	}
	data, err = json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "do-not-export") || strings.Contains(string(data), "source_path") {
		t.Fatal("local path exported")
	}
	if !strings.Contains(string(data), `"output_tokens":0`) || !strings.Contains(string(data), `"response_tok_s":0`) {
		t.Fatal("measured zero lost")
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Performance == nil || *decoded.Performance.Requests[0].OutputTokens != 0 {
		t.Fatal("new payload lost")
	}
}
