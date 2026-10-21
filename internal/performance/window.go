package performance

import (
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

const bucketWidth = 10 * time.Second

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func timestampNS(t time.Time) *big.Int {
	v := big.NewInt(t.Unix())
	v.Mul(v, big.NewInt(1e9))
	return v.Add(v, big.NewInt(int64(t.Nanosecond())))
}

// cumulative allocation avoids independent rounding loss: adjacent bucket
// differences telescope to exactly OutputTokens over [StartedAt,EndedAt).
// Big integers avoid overflow even for long verified direct intervals.
func allocated(r core.PerformanceRequest, start, end time.Time) int64 {
	if r.StartedAt == nil || r.EndedAt == nil || r.OutputTokens == nil || !end.After(start) {
		return 0
	}
	a, b := maxTime(start, *r.StartedAt), minTime(end, *r.EndedAt)
	if !b.After(a) {
		return 0
	}
	origin := timestampNS(*r.StartedAt)
	duration := new(big.Int).Sub(timestampNS(*r.EndedAt), origin)
	if duration.Sign() <= 0 {
		return 0
	}
	cumulative := func(t time.Time) *big.Int {
		elapsed := new(big.Int).Sub(timestampNS(t), origin)
		elapsed.Mul(elapsed, big.NewInt(*r.OutputTokens))
		return elapsed.Quo(elapsed, duration)
	}
	return new(big.Int).Sub(cumulative(b), cumulative(a)).Int64()
}
func checkedAdd(target *int64, value int64) error {
	if value < 0 || *target > MaxExactInteger-value {
		return fmt.Errorf("performance: window numeric limit exceeded")
	}
	*target += value
	return nil
}
func rateWindow(period core.PerformancePeriod, opts BuildOptions) core.PerformanceRateWindow {
	out := core.PerformanceRateWindow{Period: period, Stale: stale(opts)}
	if opts.CoverageStart != nil && !opts.DataAsOf.IsZero() {
		start := maxTime(period.Start, *opts.CoverageStart)
		end := minTime(period.End, opts.DataAsOf)
		if end.After(start) {
			out.CoveredSeconds = elapsedMS(start, end) / 1000
		}
		out.CompleteCoverage = !opts.CoverageStart.After(period.Start) && !opts.DataAsOf.Before(period.End) && !out.Stale
	}
	return out
}
func finishRate(r *core.PerformanceRateWindow, observed bool) {
	if (observed || r.CompleteCoverage) && r.Period.End.After(r.Period.Start) {
		value := float64(r.OutputTokens) * 1000 / elapsedMS(r.Period.Start, r.Period.End)
		r.WallTokS = &value
	}
}
func windowEligible(r core.PerformanceRequest, now time.Time) (bool, string) {
	if r.Scope != core.ScopeModelRequest {
		return false, "not_model_request"
	}
	if r.StartedAt == nil {
		return false, "missing_request_start"
	}
	if r.EndedAt == nil {
		return false, "missing_request_end"
	}
	if !r.EndedAt.After(*r.StartedAt) {
		return false, "nonpositive_interval"
	}
	if r.EndedAt.After(now.Add(10 * time.Second)) {
		return false, "future_request_boundary"
	}
	if r.OutputTokens == nil {
		return false, "missing_output"
	}
	if *r.OutputTokens < 0 || *r.OutputTokens > MaxExactInteger {
		return false, "invalid_output"
	}
	if r.TimingStatus == core.TimingInvalid || r.TimingStatus == core.TimingAmbiguous || r.TimingStatus == core.TimingUnsupported || r.TimingStatus == core.TimingMissing {
		return false, r.ExclusionReason
	}
	if r.Final && r.Outcome != core.OutcomeSuccess {
		return false, "unsuccessful_request"
	}
	return true, ""
}

// Records with valid boundaries overlap by interval. Unknown boundaries use
// only their source event timestamp to mark coverage gaps; scan time is never
// substituted and old unsupported records do not pollute the live window.
func touches(r core.PerformanceRequest, p core.PerformancePeriod) bool {
	if r.TimingStatus != core.TimingInvalid && r.TimingStatus != core.TimingAmbiguous && r.TimingStatus != core.TimingUnsupported && r.TimingStatus != core.TimingMissing && r.StartedAt != nil && r.EndedAt != nil && r.EndedAt.After(*r.StartedAt) {
		return r.EndedAt.After(p.Start) && r.StartedAt.Before(p.End)
	}
	return within(recordTime(r), p)
}

func buildWindow(records []core.PerformanceRequest, opts BuildOptions) (*core.PerformanceWindow, error) {
	minutes := opts.BufferMinutes
	if minutes == 0 {
		minutes = 30
	}
	if minutes != 30 && minutes != 60 {
		return nil, fmt.Errorf("performance: buffer must be 30 or 60 minutes")
	}
	end := opts.Now.UTC().Truncate(bucketWidth)
	if end.Before(opts.Now) {
		end = end.Add(bucketWidth)
	}
	start := end.Add(-time.Duration(minutes) * time.Minute)
	out := &core.PerformanceWindow{Period: core.PerformancePeriod{Start: start, End: end}, BucketWidthMS: 10000, ExcludedReasons: map[string]int64{}, Stale: stale(opts)}
	if opts.CoverageStart != nil {
		v := *opts.CoverageStart
		out.CoverageStart = &v
	}
	out.Latest60Seconds = rateWindow(core.PerformancePeriod{Start: opts.Now.Add(-60 * time.Second), End: opts.Now}, opts)
	count := minutes * 6
	out.Buckets = make([]core.PerformanceBucket, count)
	for i := range out.Buckets {
		out.Buckets[i].Start = start.Add(time.Duration(i) * bucketWidth)
		out.Buckets[i].End = out.Buckets[i].Start.Add(bucketWidth)
	}
	sessions := map[string]*core.PerformanceSessionRate{}
	sessionObserved := map[string]bool{}
	sessionPartial := map[string]bool{}
	sessionPeriod := core.PerformancePeriod{Start: opts.Now.Add(-120 * time.Second), End: opts.Now}
	activePeriod := core.PerformancePeriod{Start: start, End: opts.Now.Add(time.Nanosecond)}
	observed := false
	observable := false
	sessionObservable := map[string]bool{}
	var outputSum int64
	for _, r := range records {
		if !touches(r, activePeriod) {
			continue
		}
		valid, reason := windowEligible(r, opts.Now)
		if !valid {
			if reason == "missing_request_start" {
				out.MissingStart++
			}
			if reason == "" {
				reason = "unmeasured"
			}
			out.ExcludedReasons[reason]++
			if touches(r, core.PerformancePeriod{Start: out.Latest60Seconds.Period.Start, End: opts.Now.Add(time.Nanosecond)}) {
				out.Latest60Seconds.CompleteCoverage = false
			}
			if touches(r, core.PerformancePeriod{Start: sessionPeriod.Start, End: opts.Now.Add(time.Nanosecond)}) {
				sessionPartial[groupKey(r.HostID, r.Namespace, r.SourceSystem, r.AccountID, r.SessionID)] = true
			}
			continue
		}
		if !r.EndedAt.After(start) || !r.StartedAt.Before(opts.Now) {
			continue
		}
		if err := checkedAdd(&outputSum, *r.OutputTokens); err != nil {
			return nil, err
		}
		provisional := !r.Final || r.TimingStatus == core.TimingProvisional
		if !provisional {
			observable = true
		}
		firstIndex := int(maxTime(*r.StartedAt, start).Sub(start) / bucketWidth)
		lastTime := minTime(*r.EndedAt, opts.Now)
		lastIndex := int(lastTime.Sub(start) / bucketWidth)
		if lastIndex >= len(out.Buckets) {
			lastIndex = len(out.Buckets) - 1
		}
		for i := firstIndex; i <= lastIndex; i++ {
			bucket := &out.Buckets[i]
			n := allocated(r, bucket.Start, minTime(bucket.End, opts.Now))
			if n == 0 {
				continue
			}
			if provisional {
				if err := checkedAdd(&bucket.ProvisionalOutputTokens, n); err != nil {
					return nil, err
				}
				continue
			}
			if err := checkedAdd(&bucket.OutputTokens, n); err != nil {
				return nil, err
			}
			if r.TimingBasis == core.TimingInferredLogBoundary {
				bucket.InferredOutputTokens += n
			} else {
				bucket.DirectOutputTokens += n
			}
		}
		if r.EndedAt.After(out.Latest60Seconds.Period.Start) && r.StartedAt.Before(out.Latest60Seconds.Period.End) {
			n := allocated(r, out.Latest60Seconds.Period.Start, out.Latest60Seconds.Period.End)
			if provisional {
				out.Latest60Seconds.CompleteCoverage = false
				if err := checkedAdd(&out.Latest60Seconds.ProvisionalOutputTokens, n); err != nil {
					return nil, err
				}
			} else {
				observed = true
				if err := checkedAdd(&out.Latest60Seconds.OutputTokens, n); err != nil {
					return nil, err
				}
			}
		}
		// Empty session remains unknown rather than merging unrelated requests.
		if r.SessionID == "" {
			continue
		}
		key := groupKey(r.HostID, r.Namespace, r.SourceSystem, r.AccountID, r.SessionID)
		if !provisional {
			sessionObservable[key] = true
		}
		session, ok := sessions[key]
		if !ok {
			if len(sessions) >= 1000 {
				out.ExcludedReasons["session_group_limit"]++
				continue
			}
			session = &core.PerformanceSessionRate{Namespace: r.Namespace, HostID: r.HostID, Source: r.SourceSystem, AccountID: r.AccountID, SessionID: r.SessionID, Rate: rateWindow(core.PerformancePeriod{Start: opts.Now.Add(-120 * time.Second), End: opts.Now}, opts)}
			sessions[key] = session
		}
		if r.EndedAt.After(session.Rate.Period.Start) && r.StartedAt.Before(session.Rate.Period.End) {
			n := allocated(r, session.Rate.Period.Start, session.Rate.Period.End)
			if provisional {
				session.Rate.CompleteCoverage = false
				if err := checkedAdd(&session.Rate.ProvisionalOutputTokens, n); err != nil {
					return nil, err
				}
			} else {
				sessionObserved[key] = true
				if err := checkedAdd(&session.Rate.OutputTokens, n); err != nil {
					return nil, err
				}
			}
		}
	}
	if !observable {
		out.Latest60Seconds.CompleteCoverage = false
	}
	finishRate(&out.Latest60Seconds, observed)
	keys := make([]string, 0, len(sessions))
	for key := range sessions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		s := sessions[key]
		if sessionPartial[key] || !sessionObservable[key] {
			s.Rate.CompleteCoverage = false
		}
		finishRate(&s.Rate, sessionObserved[key])
		out.Session120Seconds = append(out.Session120Seconds, *s)
	}
	return out, nil
}

func mergeRate(a, b core.PerformanceRateWindow) (core.PerformanceRateWindow, error) {
	if !a.Period.Start.Equal(b.Period.Start) || !a.Period.End.Equal(b.Period.End) {
		return a, fmt.Errorf("performance: incompatible wall windows")
	}
	if err := checkedAdd(&a.OutputTokens, b.OutputTokens); err != nil {
		return a, err
	}
	if err := checkedAdd(&a.ProvisionalOutputTokens, b.ProvisionalOutputTokens); err != nil {
		return a, err
	}
	a.CompleteCoverage = a.CompleteCoverage && b.CompleteCoverage
	a.Stale = a.Stale || b.Stale
	if b.CoveredSeconds < a.CoveredSeconds {
		a.CoveredSeconds = b.CoveredSeconds
	}
	known := a.WallTokS != nil && b.WallTokS != nil
	a.WallTokS = nil
	finishRate(&a, known)
	return a, nil
}
func mergeWindow(a, b *core.PerformanceWindow) (*core.PerformanceWindow, error) {
	if a == nil {
		snapshot := core.PerformanceSnapshot{Window: b}
		clone := snapshot.DeepClone()
		return clone.Window, nil
	}
	if b == nil {
		return nil, fmt.Errorf("performance: missing wall window")
	}
	if a.BucketWidthMS != b.BucketWidthMS || !a.Period.Start.Equal(b.Period.Start) || !a.Period.End.Equal(b.Period.End) || len(a.Buckets) != len(b.Buckets) {
		return nil, fmt.Errorf("performance: incompatible bucket windows")
	}
	for i := range a.Buckets {
		left, right := &a.Buckets[i], b.Buckets[i]
		if !left.Start.Equal(right.Start) || !left.End.Equal(right.End) {
			return nil, fmt.Errorf("performance: misaligned buckets")
		}
		for _, pair := range [][2]*int64{{&left.OutputTokens, &right.OutputTokens}, {&left.DirectOutputTokens, &right.DirectOutputTokens}, {&left.InferredOutputTokens, &right.InferredOutputTokens}, {&left.ProvisionalOutputTokens, &right.ProvisionalOutputTokens}} {
			if err := checkedAdd(pair[0], *pair[1]); err != nil {
				return nil, err
			}
		}
	}
	var err error
	a.Latest60Seconds, err = mergeRate(a.Latest60Seconds, b.Latest60Seconds)
	if err != nil {
		return nil, err
	}
	if len(a.Session120Seconds)+len(b.Session120Seconds) > 1000 {
		return nil, fmt.Errorf("performance: merged session limit exceeded")
	}
	for _, session := range b.Session120Seconds {
		if session.Rate.WallTokS != nil {
			v := *session.Rate.WallTokS
			session.Rate.WallTokS = &v
		}
		a.Session120Seconds = append(a.Session120Seconds, session)
	}
	if err := checkedAdd(&a.MissingStart, b.MissingStart); err != nil {
		return nil, err
	}
	for key, value := range b.ExcludedReasons {
		n := a.ExcludedReasons[key]
		if err := checkedAdd(&n, value); err != nil {
			return nil, err
		}
		a.ExcludedReasons[key] = n
	}
	if a.CoverageStart == nil || b.CoverageStart == nil {
		a.CoverageStart = nil
	} else if b.CoverageStart.After(*a.CoverageStart) {
		v := *b.CoverageStart
		a.CoverageStart = &v
	}
	a.Stale = a.Stale || b.Stale
	return a, nil
}

// Ring retains at most MaxRecords observations and exactly 180/360 UTC slots.
// Replacement rebuilds the bounded bucket projection, so corrected/late records
// remove their previous contribution instead of incrementing it twice. The map
// is retained only for the active window; historical statistics use Build.
type Ring struct {
	mu      sync.Mutex
	minutes int
	now     time.Time
	records map[string]core.PerformanceRequest
	buckets []core.PerformanceBucket
}

func NewRing(bufferMinutes int) (*Ring, error) {
	if bufferMinutes == 0 {
		bufferMinutes = 30
	}
	if bufferMinutes != 30 && bufferMinutes != 60 {
		return nil, fmt.Errorf("performance: buffer must be 30 or 60 minutes")
	}
	return &Ring{minutes: bufferMinutes, records: map[string]core.PerformanceRequest{}, buckets: make([]core.PerformanceBucket, bufferMinutes*6)}, nil
}
func (r *Ring) Upsert(record core.PerformanceRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := RequestKey(record)
	old, exists := r.records[key]
	if exists {
		record = PreferRequest(old, record)
	}
	if !r.now.IsZero() {
		anchor := recordTime(record)
		if record.EndedAt != nil {
			anchor = *record.EndedAt
		}
		if anchor.IsZero() || anchor.Before(r.now.Add(-time.Duration(r.minutes)*time.Minute-bucketWidth)) {
			delete(r.records, key)
			return nil
		}
	}
	if !exists && len(r.records) >= MaxRecords {
		return fmt.Errorf("performance: ring record limit exceeded")
	}
	r.records[key] = NormalizeRequest(record)
	return nil
}

// Remove drops a previously indexed request when the caller's selection no
// longer includes it (for example after a timestamp correction).
func (r *Ring) Remove(record core.PerformanceRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.records, RequestKey(record))
}

func (r *Ring) Advance(now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.IsZero() {
		return fmt.Errorf("performance: explicit now required")
	}
	if !r.now.IsZero() && now.Before(r.now) {
		return fmt.Errorf("performance: ring clock moved backwards")
	}
	r.now = now
	cutoff := now.Add(-time.Duration(r.minutes)*time.Minute - bucketWidth)
	for key, record := range r.records {
		anchor := recordTime(record)
		if record.EndedAt != nil {
			anchor = *record.EndedAt
		}
		if anchor.IsZero() || anchor.Before(cutoff) {
			delete(r.records, key)
		}
	}
	return nil
}
func (r *Ring) Snapshot(opts BuildOptions) (*core.PerformanceSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if opts.Now.IsZero() {
		opts.Now = r.now
	}
	if opts.Now.IsZero() {
		return nil, fmt.Errorf("performance: ring must advance or receive explicit now")
	}
	opts.IncludeWindow = true
	opts.BufferMinutes = r.minutes
	records := make([]core.PerformanceRequest, 0, len(r.records))
	for _, record := range r.records {
		records = append(records, record)
	}
	snapshot, err := Build(records, opts)
	if err != nil {
		return nil, err
	}
	copy(r.buckets, snapshot.Window.Buckets)
	return snapshot, nil
}
