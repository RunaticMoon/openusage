package performance

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

const MaxRecords = 100000

// Period selects requests by their original source event RecordedAt (falling
// back only to a reported EndedAt). ObservedAt is never historical event time.
// A request limit bounds exported details, not statistics. WindowMinutes is
// intentionally limited to 30 or 60; the independent live window uses Now.
type BuildOptions struct {
	Period                      core.PerformancePeriod
	FilterID, HostID, QueryPath string
	Now, DataAsOf               time.Time
	CoverageStart               *time.Time
	BufferMinutes               int
	ExportIntervalMS            int64
	RequestLimit, GroupLimit    int
	IncludeWindow               bool
}

func newSummary() core.PerformanceSummary {
	return core.PerformanceSummary{Coverage: core.PerformanceCoverage{Reasons: map[string]int64{}, OutputBases: map[string]int64{}}, Stats: newStats(), Direct: newStats(), Inferred: newStats()}
}
func addSummary(s *core.PerformanceSummary, r core.PerformanceRequest) error {
	if r.ExclusionReason == "missing_identity" {
		s.Coverage.Reasons["missing_identity"]++
		return nil
	}
	if r.Scope != core.ScopeModelRequest {
		s.Coverage.Reasons["not_model_request"]++
		return nil
	}
	if !r.Final || r.TimingStatus == core.TimingProvisional {
		s.Coverage.Provisional++
		s.Coverage.Reasons["provisional"]++
		return nil
	}
	c := &s.Coverage
	c.Candidates++
	c.OutputBases[r.OutputBasis]++
	if r.Outcome == core.OutcomeFailed {
		c.Failed++
	}
	if r.Outcome != core.OutcomeSuccess && r.Outcome != core.OutcomeFailed {
		c.Unknown++
	}
	if r.ResponseTokS == nil {
		c.Excluded++
		reason := r.ExclusionReason
		if reason == "" {
			reason = "unmeasured"
		}
		c.Reasons[reason]++
		return nil
	}
	c.Measured++
	if err := addStats(&s.Stats, r); err != nil {
		return err
	}
	if r.TimingBasis == core.TimingInferredLogBoundary {
		c.Inferred++
		return addStats(&s.Inferred, r)
	}
	c.Direct++
	return addStats(&s.Direct, r)
}
func finishSummary(s *core.PerformanceSummary) {
	finalizeStats(&s.Stats)
	finalizeStats(&s.Direct)
	finalizeStats(&s.Inferred)
}
func recordTime(r core.PerformanceRequest) time.Time {
	if !r.RecordedAt.IsZero() {
		return r.RecordedAt
	}
	if r.EndedAt != nil {
		return *r.EndedAt
	}
	return time.Time{}
}
func within(t time.Time, p core.PerformancePeriod) bool {
	return !t.IsZero() && !t.Before(p.Start) && t.Before(p.End)
}
func groupKey(parts ...string) string { v, _ := json.Marshal(parts); return string(v) }
func stale(opts BuildOptions) bool {
	if opts.DataAsOf.IsZero() || opts.Now.IsZero() {
		return true
	}
	limit := 120 * time.Second
	if opts.ExportIntervalMS > 0 && opts.ExportIntervalMS <= int64((24*time.Hour)/time.Millisecond) {
		if v := time.Duration(opts.ExportIntervalMS) * 2 * time.Millisecond; v > limit {
			limit = v
		}
	}
	return opts.Now.Sub(opts.DataAsOf) >= limit || opts.DataAsOf.After(opts.Now)
}
func normalizedRecords(records []core.PerformanceRequest) ([]core.PerformanceRequest, error) {
	if len(records) > MaxRecords {
		return nil, fmt.Errorf("performance: record limit exceeded")
	}
	unique := map[string]core.PerformanceRequest{}
	for _, raw := range records {
		r := NormalizeRequest(raw)
		key := RequestKey(r)
		if old, ok := unique[key]; ok {
			r = PreferRequest(old, r)
		}
		unique[key] = r
	}
	out := make([]core.PerformanceRequest, 0, len(unique))
	for _, r := range unique {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := recordTime(out[i]), recordTime(out[j])
		if !a.Equal(b) {
			return a.After(b)
		}
		return RequestKey(out[i]) < RequestKey(out[j])
	})
	return out, nil
}

func Build(records []core.PerformanceRequest, opts BuildOptions) (*core.PerformanceSnapshot, error) {
	if opts.Now.IsZero() || !opts.Period.End.After(opts.Period.Start) {
		return nil, fmt.Errorf("performance: explicit now and valid period required")
	}
	// Validate host isolation before deduplication: identical native IDs on
	// different machines must never collapse into one observation.
	rawHosts := map[string]bool{}
	for _, record := range records {
		host := record.HostID
		if host == "" {
			host = opts.HostID
		}
		rawHosts[host] = true
		if opts.HostID != "" && host != opts.HostID {
			return nil, fmt.Errorf("performance: configured host differs from record host")
		}
	}
	if len(rawHosts) > 1 {
		return nil, fmt.Errorf("performance: build requires one host")
	}
	rows, err := normalizedRecords(records)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].HostID == "" {
			rows[i].HostID = opts.HostID
		}
		if rows[i].EndedAt != nil && rows[i].EndedAt.After(opts.Now.Add(10*time.Second)) {
			rows[i].TimingStatus = core.TimingInvalid
			rows[i].ExclusionReason = "future_request_boundary"
			rows[i].ResponseTokS = nil
		}
	}
	if opts.RequestLimit <= 0 {
		opts.RequestLimit = 100
	}
	if opts.RequestLimit > 1000 {
		opts.RequestLimit = 1000
	}
	if opts.GroupLimit <= 0 {
		opts.GroupLimit = 128
	}
	if opts.GroupLimit > 1000 {
		opts.GroupLimit = 1000
	}
	out := &core.PerformanceSnapshot{DefinitionVersion: core.PerformanceDefinitionVersion, Period: opts.Period, FilterID: opts.FilterID, HostID: opts.HostID, DataAsOf: opts.DataAsOf, GeneratedAt: opts.Now, ExportIntervalMS: opts.ExportIntervalMS, Stale: stale(opts), Summary: newSummary(), QueryPath: opts.QueryPath}
	groups := map[string]*core.PerformanceGroup{}
	hosts := map[string]bool{}
	for _, r := range rows {
		if !within(recordTime(r), opts.Period) {
			continue
		}
		if r.HostID == "" {
			r.HostID = opts.HostID
		}
		hosts[r.HostID] = true
		if err := addSummary(&out.Summary, r); err != nil {
			return nil, err
		}
		if len(out.Requests) < opts.RequestLimit {
			out.Requests = append(out.Requests, r)
		} else {
			out.RequestsTruncated = true
		}
		for _, pair := range [][2]string{{"session", groupKey(r.Namespace, r.SourceSystem, r.AccountID, r.SessionID)}, {"model", r.ModelID}, {"account", groupKey(r.SourceSystem, r.AccountID)}, {"billing_provider", r.BillingProviderID}, {"source", r.SourceSystem}, {"host", r.HostID}} {
			// Unknown dimensions stay explicitly empty; they are never inferred.
			key := groupKey(r.HostID, pair[0], pair[1])
			g, ok := groups[key]
			if !ok {
				if len(groups) >= opts.GroupLimit {
					out.GroupsTruncated = true
					continue
				}
				g = &core.PerformanceGroup{Dimension: pair[0], Key: pair[1], HostID: r.HostID, Summary: newSummary()}
				groups[key] = g
			}
			if err := addSummary(&g.Summary, r); err != nil {
				return nil, err
			}
		}
	}
	if len(hosts) > 1 {
		return nil, fmt.Errorf("performance: build requires one host; use explicit compatible snapshot merge")
	}
	if out.HostID == "" {
		for host := range hosts {
			out.HostID = host
		}
	}
	finishSummary(&out.Summary)
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		g := groups[key]
		finishSummary(&g.Summary)
		out.Groups = append(out.Groups, *g)
	}
	if opts.IncludeWindow {
		out.Window, err = buildWindow(rows, opts)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func mergeSummary(a, b core.PerformanceSummary) (core.PerformanceSummary, error) {
	out := a.DeepClone()
	var err error
	out.Stats, err = MergeStats(a.Stats, b.Stats)
	if err != nil {
		return out, err
	}
	out.Direct, err = MergeStats(a.Direct, b.Direct)
	if err != nil {
		return out, err
	}
	out.Inferred, err = MergeStats(a.Inferred, b.Inferred)
	if err != nil {
		return out, err
	}
	ac, bc := &out.Coverage, b.Coverage
	pairs := [][2]*int64{{&ac.Candidates, &bc.Candidates}, {&ac.Measured, &bc.Measured}, {&ac.Direct, &bc.Direct}, {&ac.Inferred, &bc.Inferred}, {&ac.Excluded, &bc.Excluded}, {&ac.Provisional, &bc.Provisional}, {&ac.Failed, &bc.Failed}, {&ac.Unknown, &bc.Unknown}}
	for _, pair := range pairs {
		if *pair[1] < 0 || *pair[0] > MaxExactInteger-*pair[1] {
			return out, fmt.Errorf("performance: coverage limit exceeded")
		}
		*pair[0] += *pair[1]
	}
	for k, v := range bc.Reasons {
		if v < 0 || ac.Reasons[k] > MaxExactInteger-v {
			return out, fmt.Errorf("performance: reason limit exceeded")
		}
		ac.Reasons[k] += v
	}
	for k, v := range bc.OutputBases {
		if v < 0 || ac.OutputBases[k] > MaxExactInteger-v {
			return out, fmt.Errorf("performance: basis limit exceeded")
		}
		ac.OutputBases[k] += v
	}
	return out, nil
}

// Merge is an explicit multi-host operation. Compatibility is mandatory even
// for stale inputs. Stale hosts are listed and excluded. Repeated hosts are
// rejected to prevent double counting snapshots of the same machine. The
// explicit query clock also expires cached snapshots whose Stale flag was
// false when the collector originally generated them.
func Merge(now time.Time, snapshots ...*core.PerformanceSnapshot) (*core.PerformanceSnapshot, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("performance: explicit merge clock required")
	}
	if len(snapshots) == 0 {
		return nil, fmt.Errorf("performance: no snapshots")
	}
	if snapshots[0] == nil {
		return nil, fmt.Errorf("performance: nil snapshot")
	}
	first := snapshots[0]
	out := &core.PerformanceSnapshot{DefinitionVersion: first.DefinitionVersion, Period: first.Period, FilterID: first.FilterID, Summary: newSummary(), Stale: true, GeneratedAt: now}
	seen := map[string]bool{}
	var windowShape *bool
	for _, s := range snapshots {
		if s == nil || s.DefinitionVersion != core.PerformanceDefinitionVersion || s.DefinitionVersion != first.DefinitionVersion || s.FilterID != first.FilterID || !s.Period.Start.Equal(first.Period.Start) || !s.Period.End.Equal(first.Period.End) {
			return nil, fmt.Errorf("performance: incompatible definition, filter or period")
		}
		if s.HostID == "" || seen[s.HostID] {
			return nil, fmt.Errorf("performance: merge requires distinct nonempty hosts")
		}
		seen[s.HostID] = true
		if s.Stale || stale(BuildOptions{Now: now, DataAsOf: s.DataAsOf, ExportIntervalMS: s.ExportIntervalMS}) {
			out.ExcludedHosts = append(out.ExcludedHosts, s.HostID)
			continue
		}
		hasWindow := s.Window != nil
		if windowShape != nil && *windowShape != hasWindow {
			return nil, fmt.Errorf("performance: incompatible window availability")
		}
		windowShape = &hasWindow
		var err error
		out.Summary, err = mergeSummary(out.Summary, s.Summary)
		if err != nil {
			return nil, err
		}
		if out.DataAsOf.IsZero() || s.DataAsOf.Before(out.DataAsOf) {
			out.DataAsOf = s.DataAsOf
		}
		if s.GeneratedAt.After(out.GeneratedAt) {
			out.GeneratedAt = s.GeneratedAt
		}
		if s.ExportIntervalMS > out.ExportIntervalMS {
			out.ExportIntervalMS = s.ExportIntervalMS
		}
		out.Stale = false
		for _, g := range s.Groups {
			if len(out.Groups) < 1000 {
				cloned := g
				cloned.Summary = g.Summary.DeepClone()
				out.Groups = append(out.Groups, cloned)
			} else {
				out.GroupsTruncated = true
			}
		}
		out.GroupsTruncated = out.GroupsTruncated || s.GroupsTruncated
		// Request details remain query-local and are not concatenated across hosts.
		out.RequestsTruncated = out.RequestsTruncated || len(s.Requests) > 0 || s.RequestsTruncated
		if s.Window != nil {
			out.Window, err = mergeWindow(out.Window, s.Window)
			if err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(out.ExcludedHosts)
	return out, nil
}
