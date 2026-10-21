package performance

import (
	"fmt"
	"math"

	"github.com/janekbaraniewski/openusage/internal/core"
)

const HistogramVersion = "log2_16_v1"
const histogramBuckets = 1024

// The fixed range is [2^-32,2^32), with 16 buckets per doubling.
// Geometric midpoints have <=2.19% relative value error in-range. Quantile
// ranks use nearest-rank (ceil(q*n)); zero is exact. Out-of-range quantiles
// are null, while sample counts and exact min/max remain available.
func NewHistogram() core.PerformanceHistogram {
	ratio := math.Exp2(1.0 / 16)
	return core.PerformanceHistogram{
		Version: HistogramVersion, Base: math.Exp2(-32), Ratio: ratio,
		BucketCount: histogramBuckets, RelativeErrorBound: math.Sqrt(ratio) - 1,
		Buckets: make(map[int]int64),
	}
}

func validHistogram(h core.PerformanceHistogram) bool {
	prototype := NewHistogram()
	if h.Version != prototype.Version || h.Base != prototype.Base || h.Ratio != prototype.Ratio || h.BucketCount != histogramBuckets || h.RelativeErrorBound != prototype.RelativeErrorBound {
		return false
	}
	if h.ZeroCount < 0 || h.UnderflowCount < 0 || h.OverflowCount < 0 || h.ZeroCount > MaxExactInteger || h.UnderflowCount > MaxExactInteger || h.OverflowCount > MaxExactInteger {
		return false
	}
	for index, count := range h.Buckets {
		if index < 0 || index >= histogramBuckets || count < 0 || count > MaxExactInteger {
			return false
		}
	}
	return true
}

func histogramCount(h core.PerformanceHistogram) (int64, bool) {
	if !validHistogram(h) {
		return 0, false
	}
	var n int64
	for _, v := range []int64{h.ZeroCount, h.UnderflowCount, h.OverflowCount} {
		if n > MaxExactInteger-v {
			return 0, false
		}
		n += v
	}
	for _, v := range h.Buckets {
		if n > MaxExactInteger-v {
			return 0, false
		}
		n += v
	}
	return n, true
}

func addHistogram(h *core.PerformanceHistogram, value float64) {
	if value == 0 {
		h.ZeroCount++
		return
	}
	index := int(math.Floor((math.Log2(value) + 32) * 16))
	if index < 0 {
		h.UnderflowCount++
		return
	}
	if index >= histogramBuckets {
		h.OverflowCount++
		return
	}
	h.Buckets[index]++
}

func HistogramQuantile(h core.PerformanceHistogram, q float64) *float64 {
	if !validHistogram(h) || !finite(q) || q < 0 || q > 1 {
		return nil
	}
	n, valid := histogramCount(h)
	if !valid {
		return nil
	}
	if n == 0 {
		return nil
	}
	rank := int64(math.Ceil(q * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank <= h.ZeroCount {
		value := 0.0
		return &value
	}
	rank -= h.ZeroCount
	if rank <= h.UnderflowCount {
		return nil
	}
	rank -= h.UnderflowCount
	for i := 0; i < histogramBuckets; i++ {
		if rank <= h.Buckets[i] {
			value := math.Exp2(-32 + (float64(i)+0.5)/16)
			return &value
		}
		rank -= h.Buckets[i]
	}
	return nil
}

func newStats() core.PerformanceStats { return core.PerformanceStats{Histogram: NewHistogram()} }

func addStats(s *core.PerformanceStats, r core.PerformanceRequest) error {
	if r.ResponseTokS == nil || r.OutputTokens == nil || r.DurationMS == nil {
		return fmt.Errorf("performance: invalid statistics sample")
	}
	if s.Count >= MaxExactInteger || s.OutputTokens > MaxExactInteger-*r.OutputTokens || !finite(s.DurationMS+*r.DurationMS) {
		return fmt.Errorf("performance: statistics numeric limit exceeded")
	}
	s.Count++
	s.OutputTokens += *r.OutputTokens
	s.DurationMS += *r.DurationMS
	rate := *r.ResponseTokS
	if s.MinTokS == nil || rate < *s.MinTokS {
		s.MinTokS = core.Float64Ptr(rate)
	}
	if s.MaxTokS == nil || rate > *s.MaxTokS {
		s.MaxTokS = core.Float64Ptr(rate)
	}
	addHistogram(&s.Histogram, rate)
	return nil
}

func finalizeStats(s *core.PerformanceStats) {
	s.WeightedResponseTokS, s.P50TokS, s.P90TokS = nil, nil, nil
	if s.Count > 0 && s.DurationMS > 0 {
		rate := float64(s.OutputTokens) * 1000 / s.DurationMS
		if finite(rate) {
			s.WeightedResponseTokS = &rate
		}
		s.P50TokS, s.P90TokS = HistogramQuantile(s.Histogram, .5), HistogramQuantile(s.Histogram, .9)
	}
}

func MergeStats(stats ...core.PerformanceStats) (core.PerformanceStats, error) {
	out := newStats()
	for _, s := range stats {
		histogramN, histogramValid := histogramCount(s.Histogram)
		if !histogramValid || histogramN != s.Count || (s.Count == 0 && (s.OutputTokens != 0 || s.DurationMS != 0)) || (s.Count > 0 && s.DurationMS <= 0) || (s.MinTokS != nil && (!finite(*s.MinTokS) || *s.MinTokS < 0)) || (s.MaxTokS != nil && (!finite(*s.MaxTokS) || *s.MaxTokS < 0)) {
			return out, fmt.Errorf("performance: inconsistent statistics")
		}
		if !validHistogram(s.Histogram) || s.Count < 0 || s.OutputTokens < 0 || !finite(s.DurationMS) || s.DurationMS < 0 || out.Count > MaxExactInteger-s.Count || out.OutputTokens > MaxExactInteger-s.OutputTokens || !finite(out.DurationMS+s.DurationMS) {
			return out, fmt.Errorf("performance: incompatible or invalid statistics")
		}
		out.Count += s.Count
		out.OutputTokens += s.OutputTokens
		out.DurationMS += s.DurationMS
		out.Histogram.ZeroCount += s.Histogram.ZeroCount
		out.Histogram.UnderflowCount += s.Histogram.UnderflowCount
		out.Histogram.OverflowCount += s.Histogram.OverflowCount
		for index, count := range s.Histogram.Buckets {
			out.Histogram.Buckets[index] += count
		}
		if s.MinTokS != nil && (out.MinTokS == nil || *s.MinTokS < *out.MinTokS) {
			out.MinTokS = core.Float64Ptr(*s.MinTokS)
		}
		if s.MaxTokS != nil && (out.MaxTokS == nil || *s.MaxTokS > *out.MaxTokS) {
			out.MaxTokS = core.Float64Ptr(*s.MaxTokS)
		}
	}
	finalizeStats(&out)
	return out, nil
}
