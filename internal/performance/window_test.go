package performance

import (
	"reflect"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestWindowParallelConservationAndZero(t *testing.T) {
	opts := options()
	opts.IncludeWindow = true
	a, b := request("a", 6000, time.Minute), request("b", 6000, time.Minute)
	s, err := Build([]core.PerformanceRequest{a, b}, opts)
	if err != nil {
		t.Fatal(err)
	}
	near(t, s.Summary.Stats.WeightedResponseTokS, 100)
	near(t, s.Window.Latest60Seconds.WallTokS, 200)
	if len(s.Window.Buckets) != 180 {
		t.Fatal("ring size")
	}
	var sum int64
	for _, bucket := range s.Window.Buckets {
		sum += bucket.OutputTokens
		if bucket.Start.Second()%10 != 0 || bucket.Start.Location() != time.UTC {
			t.Fatal("unaligned UTC bucket")
		}
	}
	if sum != 12000 {
		t.Fatalf("allocation lost %d", sum)
	}
	s, err = Build(nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.Window.Latest60Seconds.WallTokS != nil || s.Window.Latest60Seconds.CompleteCoverage {
		t.Fatal("empty scan cannot establish observed zero")
	}
	opts.CoverageStart = nil
	s, err = Build(nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.Window.Latest60Seconds.WallTokS != nil {
		t.Fatal("unknown coverage became measured zero")
	}
	zero := request("zero", 0, time.Minute)
	s, err = Build([]core.PerformanceRequest{zero}, opts)
	if err != nil {
		t.Fatal(err)
	}
	near(t, s.Window.Latest60Seconds.WallTokS, 0)
}
func TestWindowOverlapRoundingAndNoStart(t *testing.T) {
	opts := options()
	opts.IncludeWindow = true
	r := request("odd", 7, 25*time.Second)
	r.StartedAt = ptr(testNow.Add(-28 * time.Second))
	r.EndedAt = ptr(testNow.Add(-3 * time.Second))
	r.RecordedAt = *r.EndedAt
	s, err := Build([]core.PerformanceRequest{r}, opts)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, b := range s.Window.Buckets {
		sum += b.OutputTokens
	}
	if sum != 7 {
		t.Fatalf("rounding %d", sum)
	}
	clipped := request("clipped", 11, time.Hour)
	s, err = Build([]core.PerformanceRequest{clipped}, opts)
	if err != nil {
		t.Fatal(err)
	}
	sum = 0
	for _, b := range s.Window.Buckets {
		sum += b.OutputTokens
	}
	if sum != 6 {
		t.Fatalf("clipped interval redistributed %d", sum)
	}
	noStart := request("duration-only", 150, 2*time.Second)
	noStart.StartedAt = nil
	noStart.TimingBasis = core.TimingProviderReportedDuration
	noStart.DurationMS = ptr(2000.)
	s, err = Build([]core.PerformanceRequest{noStart}, opts)
	if err != nil {
		t.Fatal(err)
	}
	near(t, s.Summary.Stats.WeightedResponseTokS, 75)
	if s.Window.MissingStart != 1 || s.Window.Latest60Seconds.OutputTokens != 0 {
		t.Fatal("duration-only output spiked at end")
	}
}
func TestRingReplaceLateProvisionalEviction(t *testing.T) {
	ring, err := NewRing(30)
	if err != nil {
		t.Fatal(err)
	}
	if err := ring.Advance(testNow); err != nil {
		t.Fatal(err)
	}
	a := request("a", 6000, time.Minute)
	a.Final = false
	if err := ring.Upsert(a); err != nil {
		t.Fatal(err)
	}
	s, err := ring.Snapshot(options())
	if err != nil {
		t.Fatal(err)
	}
	if s.Window.Latest60Seconds.OutputTokens != 0 || s.Window.Latest60Seconds.ProvisionalOutputTokens != 6000 {
		t.Fatal("provisional mixed into final")
	}
	a.Final = true
	a.Revision = 1
	if err := ring.Upsert(a); err != nil {
		t.Fatal(err)
	}
	if err := ring.Upsert(a); err != nil {
		t.Fatal(err)
	}
	s, err = ring.Snapshot(options())
	if err != nil {
		t.Fatal(err)
	}
	near(t, s.Window.Latest60Seconds.WallTokS, 100)
	if s.Window.Latest60Seconds.ProvisionalOutputTokens != 0 {
		t.Fatal("old contribution retained")
	}
	a.OutputTokens = ptr(int64(3000))
	a.Revision = 2
	if err := ring.Upsert(a); err != nil {
		t.Fatal(err)
	}
	s, err = ring.Snapshot(options())
	if err != nil {
		t.Fatal(err)
	}
	near(t, s.Window.Latest60Seconds.WallTokS, 50)
	want, err := Build([]core.PerformanceRequest{a}, func() BuildOptions { o := options(); o.IncludeWindow = true; return o }())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Window, want.Window) {
		t.Fatal("incremental replacement differs from cold build")
	}
	if err := ring.Advance(testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(ring.records) != 0 {
		t.Fatal("ring did not evict")
	}
	if err := ring.Advance(testNow); err == nil {
		t.Fatal("backward clock accepted")
	}
}
func TestWindowStaleClockSkewAndBounds(t *testing.T) {
	opts := options()
	opts.IncludeWindow = true
	opts.BufferMinutes = 60
	opts.DataAsOf = testNow.Add(-3 * time.Minute)
	s, err := Build([]core.PerformanceRequest{request("a", 100, time.Second)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Stale || !s.Window.Stale || !s.Window.Latest60Seconds.Stale || len(s.Window.Buckets) != 360 {
		t.Fatal("staleness or bound lost")
	}
	opts.DataAsOf = testNow.Add(time.Minute)
	s, err = Build(nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Stale {
		t.Fatal("future heartbeat not flagged")
	}
	r := request("future", 100, time.Second)
	r.StartedAt = ptr(testNow.Add(time.Minute))
	r.EndedAt = ptr(testNow.Add(time.Minute + time.Second))
	r.RecordedAt = testNow
	opts.DataAsOf = testNow
	s, err = Build([]core.PerformanceRequest{r}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.Window.ExcludedReasons["future_request_boundary"] != 1 {
		t.Fatal("future interval accepted")
	}
	opts.BufferMinutes = 90
	if _, err := Build(nil, opts); err == nil {
		t.Fatal("unbounded ring accepted")
	}
}

func TestRecentUnknownCoverageIsNotIdleZero(t *testing.T) {
	opts := options()
	opts.IncludeWindow = true
	missing := request("unknown", 50, time.Second)
	missing.StartedAt = nil
	missing.TimingBasis = core.TimingUnknown
	s, err := Build([]core.PerformanceRequest{missing}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.Window.Latest60Seconds.WallTokS != nil || s.Window.Latest60Seconds.CompleteCoverage {
		t.Fatal("unobservable recent activity became idle zero")
	}
	measured := request("measured", 0, time.Second)
	s, err = Build([]core.PerformanceRequest{missing, measured}, opts)
	if err != nil {
		t.Fatal(err)
	}
	near(t, s.Window.Latest60Seconds.WallTokS, 0)
	if s.Window.Latest60Seconds.CompleteCoverage {
		t.Fatal("missing activity hidden by valid zero")
	}
	missing.RecordedAt = testNow.Add(-time.Hour)
	missing.EndedAt = ptr(missing.RecordedAt)
	s, err = Build([]core.PerformanceRequest{missing}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.Window.MissingStart != 0 {
		t.Fatal("historical missing row polluted recent coverage")
	}
	if s.Window.Latest60Seconds.WallTokS != nil {
		t.Fatal("unsupported history established idle zero")
	}
	idle := request("idle", 30, time.Second)
	idle.StartedAt = ptr(testNow.Add(-91 * time.Second))
	idle.EndedAt = ptr(testNow.Add(-90 * time.Second))
	idle.RecordedAt = *idle.EndedAt
	s, err = Build([]core.PerformanceRequest{idle}, opts)
	if err != nil {
		t.Fatal(err)
	}
	near(t, s.Window.Latest60Seconds.WallTokS, 0)

}
func TestWindowNumericLimitAndBoundedDetails(t *testing.T) {
	opts := options()
	opts.IncludeWindow = true
	opts.RequestLimit = 1
	opts.GroupLimit = 1
	a, b := request("a", MaxExactInteger, time.Second), request("b", 1, time.Second)
	if _, err := Build([]core.PerformanceRequest{a, b}, opts); err == nil {
		t.Fatal("aggregate overflow accepted")
	}
	a.OutputTokens = ptr(int64(1))
	s, err := Build([]core.PerformanceRequest{a, b}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !s.RequestsTruncated || len(s.Requests) != 1 || s.Summary.Stats.Count != 2 || !s.GroupsTruncated {
		t.Fatal("detail bounds changed total statistics")
	}
	other := a.DeepClone()
	other.HostID = "other"
	if _, err := Build([]core.PerformanceRequest{a, other}, opts); err == nil {
		t.Fatal("same ID across hosts silently deduplicated")
	}
}

func TestRingInitialBackfillBoundAndRemove(t *testing.T) {
	ring, _ := NewRing(30)
	if err := ring.Advance(testNow); err != nil {
		t.Fatal(err)
	}
	old := request("old", 100, time.Second)
	old.EndedAt = ptr(testNow.Add(-time.Hour))
	old.RecordedAt = *old.EndedAt
	old.StartedAt = ptr(old.EndedAt.Add(-time.Second))
	if err := ring.Upsert(old); err != nil {
		t.Fatal(err)
	}
	if len(ring.records) != 0 {
		t.Fatal("initial backfill retained expired record")
	}
	current := request("corrected", 100, time.Second)
	if err := ring.Upsert(current); err != nil {
		t.Fatal(err)
	}
	ring.Remove(current)
	if len(ring.records) != 0 {
		t.Fatal("remove retained selected-out record")
	}
	if err := ring.Upsert(current); err != nil {
		t.Fatal(err)
	}
	old.ID = current.ID
	old.Revision = 2
	if err := ring.Upsert(old); err != nil {
		t.Fatal(err)
	}
	if len(ring.records) != 0 {
		t.Fatal("correction left old active contribution")
	}
}

func TestRingExpiredReplayCannotDeleteNewerInterval(t *testing.T) {
	ring, _ := NewRing(30)
	ring.Advance(testNow)
	current := request("id", 100, time.Second)
	current.Revision = 2
	ring.Upsert(current)
	old := current.DeepClone()
	old.Revision = 1
	old.StartedAt = ptr(testNow.Add(-time.Hour))
	old.EndedAt = ptr(testNow.Add(-time.Hour + time.Second))
	old.RecordedAt = *old.EndedAt
	if err := ring.Upsert(old); err != nil {
		t.Fatal(err)
	}
	s, err := ring.Snapshot(options())
	if err != nil {
		t.Fatal(err)
	}
	near(t, s.Window.Latest60Seconds.WallTokS, 100./60)
}
