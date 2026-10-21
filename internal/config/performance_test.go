package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPerformanceOptInSurvivesNormalizationAndDefaultsOff(t *testing.T) {
	if DefaultConfig().Telemetry.Performance.Enabled {
		t.Fatal("performance enabled by default")
	}
	for _, mins := range []int{0, 30, 60, 123} {
		t.Run(string(rune('a'+mins)), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "settings.json")
			data, _ := json.Marshal(map[string]any{"telemetry": map[string]any{"performance": map[string]any{"enabled": true, "buffer_minutes": mins}}})
			if err := os.WriteFile(p, data, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadFrom(p)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Telemetry.Performance.Enabled {
				t.Fatal("normalization dropped opt-in")
			}
			want := 30
			if mins == 60 {
				want = 60
			}
			if cfg.Telemetry.Performance.WindowMinutes() != want {
				t.Fatal("window bound lost")
			}
			encoded, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var round Config
			if err := json.Unmarshal(encoded, &round); err != nil {
				t.Fatal(err)
			}
			if round.Telemetry.Performance != cfg.Telemetry.Performance {
				t.Fatal("config round trip lost performance")
			}
		})
	}
}
