package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

const (
	jevLedgerSource    = "paseo-ledger-v1"
	maxJevUsageBytes   = 1 << 20
	maxJevModelRows    = 8192
	maxJevDailyPoints  = 32768
	maxJevHourlyPoints = 30 * 24 * 5
)

// Only this audited, complete accounting snapshot is persisted. In particular,
// arbitrary ModelUsage dimensions, normalization reasons and Raw are excluded.
type storedJevUsage struct {
	Version      int                         `json:"version"`
	ModelUsage   []core.ModelUsageRecord     `json:"model_usage"`
	DailySeries  map[string][]core.TimePoint `json:"daily_series"`
	HourlySeries map[string][]core.TimePoint `json:"hourly_series,omitempty"`
}

func isJevLedgerSnapshot(s core.UsageSnapshot) bool {
	return s.ProviderID == "jev" && s.Attributes["jev_usage_source"] == jevLedgerSource
}

func sanitizeJevUsage(models []core.ModelUsageRecord, daily, hourly map[string][]core.TimePoint) (storedJevUsage, error) {
	out := storedJevUsage{Version: 1, DailySeries: make(map[string][]core.TimePoint), HourlySeries: make(map[string][]core.TimePoint)}
	if len(models) > maxJevModelRows {
		return out, fmt.Errorf("model row limit exceeded")
	}
	for _, row := range models {
		if strings.TrimSpace(row.RawModelID) == "" || len(row.RawModelID) > 1024 || strings.ContainsAny(row.RawModelID, "\x00\r\n") || !validJevDay(row.Dimensions["date"]) {
			return out, fmt.Errorf("invalid model accounting metadata")
		}
		for _, n := range []*float64{row.InputTokens, row.OutputTokens, row.TotalTokens, row.Requests, row.CostUSD} {
			if n != nil && !validJevNumber(*n) {
				return out, fmt.Errorf("invalid model accounting value")
			}
		}
		dimensions := map[string]string{"date": row.Dimensions["date"]}
		if row.Dimensions["cost_partial"] == "true" {
			dimensions["cost_partial"] = "true"
		}
		out.ModelUsage = append(out.ModelUsage, core.ModelUsageRecord{
			RawModelID: row.RawModelID, RawSource: jevLedgerSource, Window: "1d", Dimensions: dimensions,
			InputTokens: row.InputTokens, OutputTokens: row.OutputTokens, TotalTokens: row.TotalTokens,
			Requests: row.Requests, CostUSD: row.CostUSD,
		})
	}
	points := 0
	for _, key := range []string{"analytics_tokens", "analytics_requests", "analytics_work_tokens", "analytics_cost", "analytics_cost_partial"} {
		series, ok := daily[key]
		if !ok {
			continue
		}
		points += len(series)
		if points > maxJevDailyPoints {
			return out, fmt.Errorf("daily point limit exceeded")
		}
		for _, point := range series {
			if !validJevDay(point.Date) || !validJevNumber(point.Value) {
				return out, fmt.Errorf("invalid daily accounting value")
			}
		}
		out.DailySeries[key] = append([]core.TimePoint(nil), series...)
	}
	points = 0
	for _, key := range []string{"analytics_tokens", "analytics_requests", "analytics_work_tokens", "analytics_cost", "analytics_cost_partial"} {
		series, ok := hourly[key]
		if !ok {
			continue
		}
		points += len(series)
		if points > maxJevHourlyPoints {
			return out, fmt.Errorf("hourly point limit exceeded")
		}
		for _, point := range series {
			if !validJevHour(point.Date) || !validJevNumber(point.Value) {
				return out, fmt.Errorf("invalid hourly accounting value")
			}
		}
		out.HourlySeries[key] = append([]core.TimePoint(nil), series...)
	}
	return out, nil
}

func validJevDay(day string) bool {
	_, err := time.Parse("2006-01-02", day)
	return err == nil && len(day) == 10
}

func validJevHour(hour string) bool {
	at, err := time.Parse(time.RFC3339, hour)
	return err == nil && at.UTC().Truncate(time.Hour).Format(time.RFC3339) == hour
}

func validJevNumber(n float64) bool {
	return n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func encodeJevUsage(s core.UsageSnapshot) (json.RawMessage, error) {
	usage, err := sanitizeJevUsage(s.ModelUsage, s.DailySeries, s.HourlySeries)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(usage)
	if err != nil || len(data) > maxJevUsageBytes {
		return nil, fmt.Errorf("accounting payload exceeds 1 MiB limit")
	}
	return data, nil
}

func restoreJevUsage(s *core.UsageSnapshot, data json.RawMessage) {
	if len(data) == 0 {
		rejectJevUsage(s, "Accounting detail is unavailable; a successful collector poll is required")
		return
	}
	if len(data) > maxJevUsageBytes {
		rejectJevUsage(s, "Accounting payload exceeds 1 MiB limit")
		return
	}
	var usage storedJevUsage
	if err := json.Unmarshal(data, &usage); err != nil || usage.Version != 1 {
		rejectJevUsage(s, "Invalid accounting payload")
		return
	}
	clean, err := sanitizeJevUsage(usage.ModelUsage, usage.DailySeries, usage.HourlySeries)
	if err != nil {
		rejectJevUsage(s, "Invalid accounting payload")
		return
	}
	s.ModelUsage, s.DailySeries, s.HourlySeries = clean.ModelUsage, clean.DailySeries, clean.HourlySeries
}

func rejectJevUsage(s *core.UsageSnapshot, reason string) {
	s.EnsureMaps()
	s.Status = core.StatusError
	s.Message = "Jev accounting snapshot is unavailable"
	s.SetDiagnostic("jev_usage_snapshot", reason)
	// Jev exposes accounting only. Include normalized aliases in the reset so
	// an incomplete detail payload cannot leave apparently complete totals.
	s.Metrics = make(map[string]core.Metric)
	s.SetAttribute("cost_partial", "true")
	s.ModelUsage = nil
	s.DailySeries = make(map[string][]core.TimePoint)
	s.HourlySeries = make(map[string][]core.TimePoint)
}
