package core

import (
	"maps"
	"time"
)

// PerformanceDefinitionVersion identifies output-only response speed. It does
// not describe decode speed, first-token latency, reasoning speed or cost.
const PerformanceDefinitionVersion = "log_response_throughput_v1"

type PerformanceTimingBasis string

const (
	TimingProviderRequestTimestamps PerformanceTimingBasis = "provider_request_timestamps"
	TimingProviderReportedDuration  PerformanceTimingBasis = "provider_reported_duration"
	TimingInferredLogBoundary       PerformanceTimingBasis = "inferred_log_boundary"
	TimingUnknown                   PerformanceTimingBasis = "unknown"
)

type PerformanceScope string

const (
	ScopeModelRequest PerformanceScope = "model_request"
	ScopeTurn         PerformanceScope = "turn"
	ScopeSession      PerformanceScope = "session"
	ScopeUnknown      PerformanceScope = "unknown"
)

type PerformanceTimingStatus string

const (
	TimingValid       PerformanceTimingStatus = "valid"
	TimingMissing     PerformanceTimingStatus = "missing"
	TimingInvalid     PerformanceTimingStatus = "invalid"
	TimingAmbiguous   PerformanceTimingStatus = "ambiguous"
	TimingUnsupported PerformanceTimingStatus = "unsupported"
	TimingProvisional PerformanceTimingStatus = "provisional"
)

type PerformanceOutcome string

const (
	OutcomeSuccess PerformanceOutcome = "success"
	OutcomeFailed  PerformanceOutcome = "failed"
	OutcomeUnknown PerformanceOutcome = "unknown"
)

// PerformanceRequest is an observation about one model request. OutputTokens
// is the original output counter, never output+reasoning. Evidence contains
// adapter-defined reason codes only, never prompt/tool/log text. SourcePath is
// local provenance and is deliberately excluded from exported JSON.
type PerformanceRequest struct {
	ID                string                  `json:"id"`
	Namespace         string                  `json:"namespace"`
	SourceSystem      string                  `json:"source_system"`
	HostID            string                  `json:"host_id,omitempty"`
	AccountID         string                  `json:"account_id"`
	BillingProviderID string                  `json:"billing_provider_id,omitempty"`
	SessionID         string                  `json:"session_id,omitempty"`
	ModelID           string                  `json:"model_id,omitempty"`
	MessageID         string                  `json:"message_id,omitempty"`
	SourcePath        string                  `json:"-"`
	AdapterVersion    string                  `json:"adapter_version"`
	OutputTokens      *int64                  `json:"output_tokens"`
	OutputBasis       string                  `json:"output_basis"`
	StartedAt         *time.Time              `json:"started_at"`
	EndedAt           *time.Time              `json:"ended_at"`
	DurationMS        *float64                `json:"duration_ms"`
	TimingBasis       PerformanceTimingBasis  `json:"timing_basis"`
	Scope             PerformanceScope        `json:"scope"`
	TimingStatus      PerformanceTimingStatus `json:"timing_status"`
	Final             bool                    `json:"final"`
	Outcome           PerformanceOutcome      `json:"outcome"`
	Revision          int64                   `json:"revision"`
	RecordedAt        time.Time               `json:"recorded_at"`
	ObservedAt        time.Time               `json:"observed_at"`
	Evidence          []string                `json:"evidence,omitempty"`
	ResponseTokS      *float64                `json:"response_tok_s"`
	ExclusionReason   string                  `json:"exclusion_reason,omitempty"`
}

type PerformancePeriod struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Sparse fixed log buckets are mergeable; percentiles are estimated only
// inside the declared range. Zero is exact and out-of-range samples are kept
// in explicit counters instead of being silently clamped.
type PerformanceHistogram struct {
	Version            string        `json:"version"`
	Base               float64       `json:"base"`
	Ratio              float64       `json:"ratio"`
	BucketCount        int           `json:"bucket_count"`
	RelativeErrorBound float64       `json:"relative_error_bound"`
	Buckets            map[int]int64 `json:"buckets,omitempty"`
	ZeroCount          int64         `json:"zero_count"`
	UnderflowCount     int64         `json:"underflow_count"`
	OverflowCount      int64         `json:"overflow_count"`
}

type PerformanceStats struct {
	Count                int64                `json:"count"`
	OutputTokens         int64                `json:"output_tokens"`
	DurationMS           float64              `json:"duration_ms"`
	WeightedResponseTokS *float64             `json:"weighted_response_tok_s"`
	P50TokS              *float64             `json:"p50_tok_s"`
	P90TokS              *float64             `json:"p90_tok_s"`
	MinTokS              *float64             `json:"min_tok_s"`
	MaxTokS              *float64             `json:"max_tok_s"`
	Histogram            PerformanceHistogram `json:"histogram"`
}

// Candidates are unique final model requests, including failed/unknown
// outcomes. Provisional requests are outside Candidates. Measured is the
// successful valid subset; Direct+Inferred equals Measured.
type PerformanceCoverage struct {
	Candidates  int64            `json:"candidates"`
	Measured    int64            `json:"measured"`
	Direct      int64            `json:"direct"`
	Inferred    int64            `json:"inferred"`
	Excluded    int64            `json:"excluded"`
	Provisional int64            `json:"provisional"`
	Failed      int64            `json:"failed"`
	Unknown     int64            `json:"unknown"`
	Reasons     map[string]int64 `json:"reasons,omitempty"`
	OutputBases map[string]int64 `json:"output_bases,omitempty"`
}

type PerformanceSummary struct {
	Coverage PerformanceCoverage `json:"coverage"`
	Stats    PerformanceStats    `json:"stats"`
	Direct   PerformanceStats    `json:"direct"`
	Inferred PerformanceStats    `json:"inferred"`
}

// Group dimensions remain separate. No billing provider is inferred from a
// tool source, and no account/session/model value is used as a request ID.
type PerformanceGroup struct {
	Dimension string             `json:"dimension"`
	Key       string             `json:"key"`
	HostID    string             `json:"host_id,omitempty"`
	Summary   PerformanceSummary `json:"summary"`
}

type PerformanceBucket struct {
	Start                   time.Time `json:"start"`
	End                     time.Time `json:"end"`
	OutputTokens            int64     `json:"output_tokens"`
	DirectOutputTokens      int64     `json:"direct_output_tokens"`
	InferredOutputTokens    int64     `json:"inferred_output_tokens"`
	ProvisionalOutputTokens int64     `json:"provisional_output_tokens"`
}

// WallTokS is throughput over the entire wall-clock period, including idle
// time. It is not a request response speed or the average of bucket speeds.
type PerformanceRateWindow struct {
	Period                  PerformancePeriod `json:"period"`
	OutputTokens            int64             `json:"output_tokens"`
	ProvisionalOutputTokens int64             `json:"provisional_output_tokens"`
	WallTokS                *float64          `json:"wall_tok_s"`
	CoveredSeconds          float64           `json:"covered_seconds"`
	CompleteCoverage        bool              `json:"complete_coverage"`
	Stale                   bool              `json:"stale"`
}

type PerformanceSessionRate struct {
	Namespace string                `json:"namespace"`
	HostID    string                `json:"host_id,omitempty"`
	Source    string                `json:"source_system"`
	AccountID string                `json:"account_id"`
	SessionID string                `json:"session_id"`
	Rate      PerformanceRateWindow `json:"rate"`
}

type PerformanceWindow struct {
	Period            PerformancePeriod        `json:"period"`
	BucketWidthMS     int64                    `json:"bucket_width_ms"`
	Buckets           []PerformanceBucket      `json:"buckets"`
	Latest60Seconds   PerformanceRateWindow    `json:"latest_60_seconds"`
	Session120Seconds []PerformanceSessionRate `json:"session_120_seconds,omitempty"`
	MissingStart      int64                    `json:"missing_start"`
	ExcludedReasons   map[string]int64         `json:"excluded_reasons,omitempty"`
	CoverageStart     *time.Time               `json:"coverage_start"`
	Stale             bool                     `json:"stale"`
}

type PerformanceSnapshot struct {
	DefinitionVersion string               `json:"definition_version"`
	Period            PerformancePeriod    `json:"period"`
	FilterID          string               `json:"filter_id"`
	HostID            string               `json:"host_id,omitempty"`
	DataAsOf          time.Time            `json:"data_as_of"`
	GeneratedAt       time.Time            `json:"generated_at"`
	ExportIntervalMS  int64                `json:"export_interval_ms"`
	Stale             bool                 `json:"stale"`
	ExcludedHosts     []string             `json:"excluded_hosts,omitempty"`
	Summary           PerformanceSummary   `json:"summary"`
	Groups            []PerformanceGroup   `json:"groups,omitempty"`
	Requests          []PerformanceRequest `json:"requests,omitempty"`
	RequestsTruncated bool                 `json:"requests_truncated"`
	GroupsTruncated   bool                 `json:"groups_truncated"`
	QueryPath         string               `json:"query_path,omitempty"`
	Window            *PerformanceWindow   `json:"window,omitempty"`
}

func (r PerformanceRequest) DeepClone() PerformanceRequest {
	copy := r
	if r.OutputTokens != nil {
		v := *r.OutputTokens
		copy.OutputTokens = &v
	}
	copy.DurationMS = cloneFloat64Ptr(r.DurationMS)
	copy.ResponseTokS = cloneFloat64Ptr(r.ResponseTokS)
	copy.StartedAt = clonePerformanceTime(r.StartedAt)
	copy.EndedAt = clonePerformanceTime(r.EndedAt)
	copy.Evidence = append([]string(nil), r.Evidence...)
	return copy
}

func clonePerformanceTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (s PerformanceStats) DeepClone() PerformanceStats {
	copy := s
	copy.WeightedResponseTokS = cloneFloat64Ptr(s.WeightedResponseTokS)
	copy.P50TokS = cloneFloat64Ptr(s.P50TokS)
	copy.P90TokS = cloneFloat64Ptr(s.P90TokS)
	copy.MinTokS = cloneFloat64Ptr(s.MinTokS)
	copy.MaxTokS = cloneFloat64Ptr(s.MaxTokS)
	copy.Histogram.Buckets = maps.Clone(s.Histogram.Buckets)
	return copy
}

func (s PerformanceSummary) DeepClone() PerformanceSummary {
	copy := s
	copy.Coverage.Reasons = maps.Clone(s.Coverage.Reasons)
	copy.Coverage.OutputBases = maps.Clone(s.Coverage.OutputBases)
	copy.Stats, copy.Direct, copy.Inferred = s.Stats.DeepClone(), s.Direct.DeepClone(), s.Inferred.DeepClone()
	return copy
}

func (s PerformanceSnapshot) DeepClone() PerformanceSnapshot {
	copy := s
	copy.Summary = s.Summary.DeepClone()
	copy.ExcludedHosts = append([]string(nil), s.ExcludedHosts...)
	if s.Groups != nil {
		copy.Groups = make([]PerformanceGroup, len(s.Groups))
		for i, group := range s.Groups {
			copy.Groups[i] = group
			copy.Groups[i].Summary = group.Summary.DeepClone()
		}
	}
	if s.Requests != nil {
		copy.Requests = make([]PerformanceRequest, len(s.Requests))
		for i, request := range s.Requests {
			copy.Requests[i] = request.DeepClone()
		}
	}
	if s.Window != nil {
		window := *s.Window
		window.Buckets = append([]PerformanceBucket(nil), s.Window.Buckets...)
		window.ExcludedReasons = maps.Clone(s.Window.ExcludedReasons)
		window.CoverageStart = clonePerformanceTime(s.Window.CoverageStart)
		window.Latest60Seconds.WallTokS = cloneFloat64Ptr(s.Window.Latest60Seconds.WallTokS)
		window.Session120Seconds = append([]PerformanceSessionRate(nil), s.Window.Session120Seconds...)
		for i := range window.Session120Seconds {
			window.Session120Seconds[i].Rate.WallTokS = cloneFloat64Ptr(window.Session120Seconds[i].Rate.WallTokS)
		}
		copy.Window = &window
	}
	return copy
}
