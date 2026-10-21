package codex

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

const codexTimingPendingLimit = 8192

type codexTimingObservation struct {
	event            shared.TelemetryEvent
	last             *core.PerformanceRequest
	sent             string
	conflictingScope bool
}

type codexTimingCache struct {
	reader    nativeTimingReader
	usage     map[string]*codexTimingObservation
	terminals []nativeTurnTerminal
}

// The native DB is resolved inside the selected profile. Arbitrary transcript
// directories require an explicit DB path; they never fall back to ~/.codex.
func codexLogsPath(opts shared.TelemetryCollectOptions, sessionsDir string) string {
	if explicit := opts.Path("codex_logs_db", ""); explicit != "" {
		return filepath.Clean(shared.ExpandHome(explicit))
	}
	dir := filepath.Clean(sessionsDir)
	if base := filepath.Base(dir); base == "sessions" || base == "archived_sessions" {
		return filepath.Join(filepath.Dir(dir), "logs_2.sqlite")
	}
	return ""
}

func codexTimingScope(sessionsDir, accountID, logsPath string) string {
	key, _ := json.Marshal([]string{filepath.Clean(sessionsDir), accountID, logsPath})
	return string(key)
}

// Called under telemetryCacheMu and only when performance is enabled. The
// existing incremental JSONL pass supplies usage; this adds one bounded native
// metadata DB scan, with no second collector, timer, hook or network call.
func (p *Provider) enrichNativeTiming(ctx context.Context, opts shared.TelemetryCollectOptions, sessionsDir, accountID string, events []shared.TelemetryEvent, terminalBatches ...[]nativeTurnTerminal) []shared.TelemetryEvent {
	path := codexLogsPath(opts, sessionsDir)
	key := codexTimingScope(sessionsDir, accountID, path)
	if p.nativeTiming == nil {
		p.nativeTiming = make(map[string]*codexTimingCache)
	}
	cache := p.nativeTiming[key]
	if cache == nil {
		cache = &codexTimingCache{usage: make(map[string]*codexTimingObservation)}
		p.nativeTiming[key] = cache
	}
	for _, batch := range terminalBatches {
		cache.terminals = mergeCodexTurnTerminals(cache.terminals, batch)
	}
	out := make([]shared.TelemetryEvent, 0, len(events))
	for _, event := range events {
		if !event.PerformanceOnly || event.Performance == nil {
			out = append(out, event)
			continue
		}
		id := event.Performance.ID
		previous := cache.usage[id]
		if previous == nil {
			cache.usage[id] = &codexTimingObservation{event: event}
		} else {
			if event.Performance.SessionID != previous.event.Performance.SessionID || event.TurnID != previous.event.TurnID {
				previous.conflictingScope = true
			}
			if event.Performance.Revision > previous.event.Performance.Revision {
				previous.event = event
			}
		}
	}
	// Keep recent identities for timing that arrives after an unchanged JSONL
	// scan. Older observations still reach the store, but are not retained in
	// this bounded in-memory enrichment cache.
	ids := make([]string, 0, len(cache.usage))
	for id := range cache.usage {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := cache.usage[ids[i]].event, cache.usage[ids[j]].event
		if a.OccurredAt.Equal(b.OccurredAt) {
			return ids[i] < ids[j]
		}
		return a.OccurredAt.After(b.OccurredAt)
	})
	if len(ids) > codexTimingPendingLimit {
		for _, id := range ids[codexTimingPendingLimit:] {
			observation := cache.usage[id]
			if observation.sent == "" {
				out = append(out, observation.event)
			}
			delete(cache.usage, id)
		}
		ids = ids[:codexTimingPendingLimit]
	}
	usages := make([]nativeTimingUsage, 0, len(ids))
	for _, id := range ids {
		event := cache.usage[id].event
		r := event.Performance
		version := ""
		if slices.Contains(r.Evidence, "codex_cli_version:0.163.0") && slices.Contains(r.Evidence, "native_thread_id") {
			version = "0.163.0"
		}
		usages = append(usages, nativeTimingUsage{ResponseID: id, ThreadID: r.SessionID, TurnID: event.TurnID, CLIVersion: version})
	}
	var matches map[string]nativeTimingMatch
	diagnostic := "native_logs_path_unconfigured"
	if path != "" {
		stats, err := cache.reader.Scan(ctx, path)
		diagnostic = stats.Reason
		if err == nil {
			matches = cache.reader.MatchBatch(usages, cache.terminals)
		}
	}
	slices.Reverse(ids)
	for _, id := range ids {
		observation := cache.usage[id]
		event := observation.event
		r := event.Performance.DeepClone()
		// Revalidate the v1 adapter's unjustified success classification even
		// when its old native diagnostic rows have since been pruned. This is
		// a semantic correction, not a claim that missing logs prove failure.
		if slices.Contains(r.Evidence, "codex_cli_version:0.163.0") && slices.Contains(r.Evidence, "native_thread_id") {
			r.AdapterVersion = "codex_native_sqlite_v2"
			r.Outcome = core.OutcomeUnknown
			r.Evidence = append(r.Evidence, "codex_outcome_v2:unknown")
		}
		match, found := matches[id]
		if observation.conflictingScope {
			match.Contradicted, match.Reason, found = true, "native_response_scope_conflict", true
		}
		if found {
			applyCodexTimingMatch(&r, match)
		} else if diagnostic != "" {
			r.ExclusionReason = "codex_" + diagnostic
		}
		// Rotation, temporary locking or incomplete metadata cannot revoke
		// previously observed evidence. Positive contradictions can, and stay
		// excluded at the same usage revision even if old rows are later pruned.
		if old := observation.last; old != nil {
			if r.Outcome == core.OutcomeUnknown && old.Final {
				r.Outcome = old.Outcome
			}
			ambiguous := r.TimingStatus == core.TimingAmbiguous || r.TimingStatus == core.TimingInvalid
			sticky := old.Revision == r.Revision && old.TimingBasis == core.TimingInferredLogBoundary &&
				old.ExclusionReason != "ambiguous_unmapped_completion" &&
				old.ExclusionReason != "ambiguous_parallel_sampling" &&
				(old.TimingStatus == core.TimingAmbiguous || old.TimingStatus == core.TimingInvalid)
			if sticky || (!ambiguous && r.DurationMS == nil && old.DurationMS != nil) {
				r.StartedAt, r.EndedAt, r.DurationMS = old.DeepClone().StartedAt, old.DeepClone().EndedAt, old.DeepClone().DurationMS
				r.TimingBasis, r.TimingStatus, r.Scope = old.TimingBasis, old.TimingStatus, old.Scope
				r.ExclusionReason, r.Evidence, r.AdapterVersion = old.ExclusionReason, slices.Clone(old.Evidence), old.AdapterVersion
			}
		}
		event.Performance = &r
		encoded, _ := json.Marshal(event)
		if fingerprint := string(encoded); fingerprint != observation.sent {
			out = append(out, event)
			observation.sent = fingerprint
			copy := r.DeepClone()
			observation.last = &copy
		}
	}
	return out
}

func applyCodexTimingMatch(r *core.PerformanceRequest, match nativeTimingMatch) {
	if !match.Completed && !match.Contradicted {
		if match.Reason != "" {
			r.ExclusionReason = match.Reason
		}
		return
	}
	r.AdapterVersion = "codex_native_sqlite_v2"
	versionKnown := slices.Contains(r.Evidence, "codex_cli_version:0.163.0")
	r.Evidence = []string{"native_response_id", "per_response_usage", "codex_outcome_v2:unknown"}
	r.Outcome = core.OutcomeUnknown
	if versionKnown {
		r.Evidence = append(r.Evidence, "codex_cli_version:0.163.0")
	}
	if match.Completed {
		// Internal Completed also represents response.incomplete(interrupted).
		// The exact response tag establishes a logged endpoint, not success.
		r.Evidence = append(r.Evidence, "native_completed_response_id", "native_completed_outcome_unavailable")
	}
	if match.Contradicted || strings.HasPrefix(match.Reason, "ambiguous_") {
		r.TimingBasis = core.TimingInferredLogBoundary
		r.TimingStatus = core.TimingAmbiguous
		if strings.Contains(match.Reason, "duration") || strings.Contains(match.Reason, "clock") {
			r.TimingStatus = core.TimingInvalid
		}
		r.ExclusionReason = match.Reason
		return
	}
	if match.StartedAt.IsZero() || match.CompletedAt.IsZero() {
		r.TimingStatus = core.TimingMissing
		r.ExclusionReason = match.Reason
		return
	}
	start, end := match.StartedAt.UTC(), match.CompletedAt.UTC()
	duration := float64(end.Sub(start)) / 1e6
	r.StartedAt, r.EndedAt, r.DurationMS = &start, &end, &duration
	r.TimingBasis, r.TimingStatus = core.TimingInferredLogBoundary, core.TimingValid
	r.ExclusionReason = ""
	r.Evidence = append(r.Evidence, "unique_process_thread_turn_sampling_start", "sampling_elapsed_includes_local_preparation", "inferred_not_transport_clock")
}
