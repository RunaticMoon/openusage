package core

import "testing"

func TestNormalizeJevLedgerPreservesNativeZeroDaysOnly(t *testing.T) {
	for _, tc := range []struct {
		provider, marker string
		keep             bool
	}{
		{"jev", "paseo-ledger-v1", true}, {"jev", "", false}, {"codex", "paseo-ledger-v1", false},
	} {
		s := NewUsageSnapshot(tc.provider, "fixture")
		s.SetAttribute("jev_usage_source", tc.marker)
		s.DailySeries = map[string][]TimePoint{"analytics_cost": {{Date: "2020-01-01", Value: 0}}}
		s = NormalizeUsageSnapshotWithConfig(s, DefaultModelNormalizationConfig())
		if got := len(s.DailySeries["analytics_cost"]) == 1; got != tc.keep {
			t.Fatalf("native zero preservation for %s/%s = %t", tc.provider, tc.marker, got)
		}
	}
}
