package telemetry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/performance"
)

// EnablePerformance is an additive, transactional opt-in migration. The normal
// store initialization deliberately does not create or read these tables.
func (s *Store) EnablePerformance(ctx context.Context) error {
	if s.performanceEnabled.Load() {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS performance_meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO performance_meta(key,value) VALUES ('schema_version',1),('sequence',0),('epoch',0),('instance',random())`,
		`CREATE TABLE IF NOT EXISTS performance_requests (
   request_key TEXT PRIMARY KEY, source_system TEXT NOT NULL, account_id TEXT NOT NULL,
   occurred_at_ms INTEGER NOT NULL, record_json TEXT NOT NULL, source_path TEXT NOT NULL,
   change_seq INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_performance_scope ON performance_requests(source_system,account_id,occurred_at_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_performance_changes ON performance_requests(change_seq)`,
		`CREATE TABLE IF NOT EXISTS performance_sources (
   source_system TEXT NOT NULL, account_id TEXT NOT NULL, first_scan_ms INTEGER NOT NULL,
   last_scan_ms INTEGER NOT NULL, PRIMARY KEY(source_system,account_id))`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("performance migration (existing DB preserved): %w", err)
		}
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT value FROM performance_meta WHERE key='schema_version'`).Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("unsupported performance schema %d (existing DB preserved)", version)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.performanceEnabled.Store(true)
	return nil
}

func performanceKey(r core.PerformanceRequest) string {
	// JSON array framing prevents ambiguous separator concatenation. Native IDs
	// are case-sensitive. Session/path/host are not identities: copied transcripts
	// with globally namespaced native IDs are the same request on this host.
	data, _ := json.Marshal([]string{r.Namespace, r.SourceSystem, r.AccountID, r.ID})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Store) ingestPerformanceOnly(ctx context.Context, req IngestRequest) (IngestResult, error) {
	if !s.performanceEnabled.Load() || req.Performance == nil {
		return IngestResult{Status: "accepted", Deduped: true}, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IngestResult{}, err
	}
	defer tx.Rollback()
	changed, err := s.upsertPerformance(ctx, tx, req, s.now().UTC())
	if err != nil {
		return IngestResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return IngestResult{}, err
	}
	return IngestResult{Status: "accepted", Deduped: true, Updated: changed}, nil
}

func (s *Store) upsertPerformance(ctx context.Context, tx *sql.Tx, req IngestRequest, now time.Time) (bool, error) {
	if !s.performanceEnabled.Load() || req.Performance == nil {
		return false, nil
	}
	r := req.Performance.DeepClone()
	r.ID = strings.TrimSpace(r.ID)
	r.Namespace = strings.TrimSpace(r.Namespace)
	if r.ID == "" || r.Namespace == "" {
		return false, nil
	} // never manufacture identities from mutable values
	r.SourceSystem = string(req.SourceSystem)
	if r.AccountID == "" {
		r.AccountID = core.FirstNonEmpty(req.AccountID, "unknown")
	}
	if r.SourceSystem == "" {
		return false, nil
	}
	if r.ObservedAt.IsZero() {
		r.ObservedAt = now
	}
	// Derived values are recalculated by the read model, never trusted from a hook.
	r.ResponseTokS = nil
	if r.RecordedAt.IsZero() {
		r.RecordedAt = req.OccurredAt
	}
	if r.DurationMS != nil && (math.IsNaN(*r.DurationMS) || math.IsInf(*r.DurationMS, 0)) {
		r.DurationMS = nil
		r.TimingStatus = core.TimingInvalid
		r.ExclusionReason = "non_finite_duration"
	}
	r = performance.NormalizeRequest(r)
	r.ResponseTokS = nil
	sourcePath := core.FirstNonEmpty(req.PerformanceSourcePath, r.SourcePath)
	key := performanceKey(r)
	var oldJSON, oldPath string
	var oldOccurred int64
	err := tx.QueryRowContext(ctx, `SELECT record_json,source_path,occurred_at_ms FROM performance_requests WHERE request_key=?`, key).Scan(&oldJSON, &oldPath, &oldOccurred)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	occurred := req.OccurredAt.UnixMilli()
	if r.EndedAt != nil && !r.EndedAt.IsZero() {
		occurred = r.EndedAt.UnixMilli()
	}
	if err == nil {
		var old core.PerformanceRequest
		if err := json.Unmarshal([]byte(oldJSON), &old); err != nil {
			return false, fmt.Errorf("decode local performance observation: %w", err)
		}
		r = mergePerformanceObservation(old, r)
		// A replay is not new evidence or a heartbeat. Preserve stable provenance.
		r.ObservedAt = old.ObservedAt
		if oldPath != "" {
			sourcePath = oldPath
		}
		if r.EndedAt != nil {
			occurred = r.EndedAt.UnixMilli()
		} else if !r.RecordedAt.IsZero() {
			// A promoted final record without request timing still has its
			// own source event time; do not keep the partial observation's day.
			occurred = r.RecordedAt.UnixMilli()
		} else {
			occurred = oldOccurred
		}
		encoded, err := json.Marshal(r)
		if err != nil {
			return false, err
		}
		if string(encoded) == oldJSON && sourcePath == oldPath && occurred == oldOccurred {
			return false, nil
		}
		r.ObservedAt = now
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return false, fmt.Errorf("encode performance observation: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE performance_meta SET value=value+1 WHERE key='sequence'`); err != nil {
		return false, err
	}
	var seq int64
	if err = tx.QueryRowContext(ctx, `SELECT value FROM performance_meta WHERE key='sequence'`).Scan(&seq); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO performance_requests(request_key,source_system,account_id,occurred_at_ms,record_json,source_path,change_seq) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(request_key) DO UPDATE SET occurred_at_ms=excluded.occurred_at_ms,record_json=excluded.record_json,source_path=excluded.source_path,change_seq=excluded.change_seq`,
		key, r.SourceSystem, r.AccountID, occurred, string(encoded), sourcePath, seq)
	return err == nil, err
}

func mergePerformanceObservation(old, in core.PerformanceRequest) core.PerformanceRequest {
	out := old.DeepClone()
	oldCodexV2 := codexNativeAdapter(old, "codex_native_sqlite_v2")
	reviewedCodexOutcome := codexReviewedOutcome(in)
	upgradeCodexV1 := codexNativeAdapter(old, "codex_native_sqlite_v1") && reviewedCodexOutcome
	// Final observations supersede partials. Once final, a delayed partial never
	// regresses it. Source revisions allow downward as well as upward corrections.
	replace := (!old.Final && in.Final) || (in.Revision > old.Revision && (!old.Final || in.Final))
	if replace {
		out = in.DeepClone()
		// Retain the original session association when a resume/fork copied the
		// same native request into a new transcript. Separate retries have new IDs.
		out.SessionID = old.SessionID
	} else if old.OutputTokens == nil && in.OutputTokens != nil && (!old.Final || in.Final) {
		v := *in.OutputTokens
		out.OutputTokens = &v
		out.OutputBasis = in.OutputBasis
	}
	// Completion evidence can arrive after usage without changing its native
	// revision. Absence of that evidence on a later scan is not a retraction.
	knownOutcome := func(outcome core.PerformanceOutcome) bool {
		return outcome == core.OutcomeSuccess || outcome == core.OutcomeFailed
	}
	if out.Final && !knownOutcome(out.Outcome) {
		if in.Final && in.Revision >= old.Revision && knownOutcome(in.Outcome) {
			out.Outcome = in.Outcome
		} else if old.Final && knownOutcome(old.Outcome) {
			out.Outcome = old.Outcome
		}
	}
	// v1 confused an internal Completed event with provider success. A reviewed
	// v2 observation revalidates that claim even when its logs have expired. Once
	// v2 has proof, missing logs/older adapters cannot erase or recreate it. A
	// positive interruption is terminal for this native response identity.
	if upgradeCodexV1 {
		out.Outcome = in.Outcome
	} else if oldCodexV2 {
		out.Outcome = old.Outcome
		if reviewedCodexOutcome && codexOutcomeRank(in.Outcome) > codexOutcomeRank(old.Outcome) {
			out.Outcome = in.Outcome
		}
	}
	outcomeEnriched := in.Final && in.Revision >= old.Revision && knownOutcome(in.Outcome) &&
		out.Outcome == in.Outcome && (!old.Final || old.Outcome != out.Outcome)
	if reviewedCodexOutcome && (upgradeCodexV1 || oldCodexV2) && old.Outcome != out.Outcome && out.Outcome == in.Outcome {
		// Native usage revisions order counters, not separately observed outcome
		// evidence. Keep the reason for an accepted reclassification as well.
		outcomeEnriched = true
	}
	oldRank, newRank := performanceTimingRank(old), performanceTimingRank(in)
	// A partial interval ends before the request is settled. Final output must
	// never use that partial end, even when its timing basis ranks higher.
	useNewTiming := (!out.Final || in.Final) && ((newRank > oldRank) || (replace && in.TimingBasis == old.TimingBasis && newRank >= oldRank && in.TimingStatus != core.TimingMissing))
	// A positive contradiction can revoke an inferred association even at the
	// same usage revision. Missing logs cannot. Keep the revocation sticky for
	// that revision so replaying earlier valid evidence cannot resurrect it.
	inferredRevoked := func(r core.PerformanceRequest) bool {
		return r.TimingBasis == core.TimingInferredLogBoundary &&
			(r.TimingStatus == core.TimingAmbiguous || r.TimingStatus == core.TimingInvalid)
	}
	if outcomeEnriched && oldRank == 0 && newRank == 0 && !inferredRevoked(old) {
		// A known completion may still lack its start. Promote that more precise
		// diagnostic without claiming a duration or retaining "no completion".
		useNewTiming = true
	}
	// A completed response whose JSONL identity has not arrived is a temporary
	// association barrier, not proof that the prior association was false.
	recoverableBoundary := func(r core.PerformanceRequest) bool {
		return r.ExclusionReason == "ambiguous_unmapped_completion" ||
			(codexNativeAdapter(r, "codex_native_sqlite_v2") && r.ExclusionReason == "ambiguous_parallel_sampling")
	}
	stickyRevocation := old.Final && inferredRevoked(old) && !recoverableBoundary(old) && in.Revision <= old.Revision && !upgradeCodexV1
	if stickyRevocation && in.TimingBasis == core.TimingInferredLogBoundary {
		useNewTiming = false
	}
	if in.Final && in.Revision >= old.Revision && inferredRevoked(in) && old.TimingBasis == core.TimingInferredLogBoundary && (!stickyRevocation || !recoverableBoundary(in)) {
		useNewTiming = true
	}
	if upgradeCodexV1 && oldRank < 20 {
		// Do not combine newly proved success with an unreviewed v1 interval.
		// Missing v2 timing retires the old inference; only fresh valid evidence
		// can recover it. Independently direct timing keeps its higher priority.
		useNewTiming = true
	}
	if oldCodexV2 && !reviewedCodexOutcome && newRank <= oldRank {
		useNewTiming = false
	}
	if oldCodexV2 && codexNativeAdapter(in, "codex_native_sqlite_v1") {
		useNewTiming = false
	}
	if out.Final && !old.Final {
		useNewTiming = true
	}
	if useNewTiming {
		out.StartedAt = in.DeepClone().StartedAt
		out.EndedAt = in.DeepClone().EndedAt
		out.DurationMS = in.DeepClone().DurationMS
		out.TimingBasis = in.TimingBasis
		out.TimingStatus = in.TimingStatus
		out.Scope = in.Scope
		out.ExclusionReason = in.ExclusionReason
		out.Evidence = append([]string(nil), in.Evidence...)
		if in.AdapterVersion != "" {
			out.AdapterVersion = in.AdapterVersion
		}
	} else {
		out.StartedAt = old.DeepClone().StartedAt
		out.EndedAt = old.DeepClone().EndedAt
		out.DurationMS = old.DeepClone().DurationMS
		out.TimingBasis = old.TimingBasis
		out.TimingStatus = old.TimingStatus
		out.Scope = old.Scope
		out.ExclusionReason = old.ExclusionReason
		out.Evidence = append([]string(nil), old.Evidence...)
		if old.AdapterVersion != "" {
			out.AdapterVersion = old.AdapterVersion
		}
	}
	if outcomeEnriched {
		// Evidence is already sanitized at ingestion. Normalize again to keep
		// the combined allowlisted codes bounded and deduplicated.
		out.Evidence = append(append([]string(nil), in.Evidence...), out.Evidence...)
		out.Evidence = performance.NormalizeRequest(out).Evidence
		if in.AdapterVersion != "" {
			out.AdapterVersion = in.AdapterVersion
		}
	}
	if upgradeCodexV1 || oldCodexV2 {
		if upgradeCodexV1 {
			out.Evidence = append(append([]string(nil), in.Evidence...), out.Evidence...)
		}
		// Keep one outcome certificate matching the retained outcome. A newer
		// counter revision carries no authority to replace that certificate.
		evidence := make([]string, 0, len(out.Evidence)+1)
		certificate := "codex_outcome_v2:" + string(out.Outcome)
		certificateSeen := false
		for _, code := range out.Evidence {
			if strings.HasPrefix(code, "codex_outcome_v2:") {
				if !certificateSeen {
					evidence = append(evidence, certificate)
					certificateSeen = true
				}
			} else {
				evidence = append(evidence, code)
			}
		}
		if !certificateSeen {
			evidence = append([]string{certificate}, evidence...)
		}
		out.Evidence = evidence
		out.Evidence = performance.NormalizeRequest(out).Evidence
		out.AdapterVersion = "codex_native_sqlite_v2"
	}
	return out
}

func codexNativeAdapter(r core.PerformanceRequest, adapter string) bool {
	return r.Final && r.SourceSystem == "codex" && r.Namespace == "codex:response" && r.AdapterVersion == adapter
}

func codexReviewedOutcome(r core.PerformanceRequest) bool {
	if !codexNativeAdapter(r, "codex_native_sqlite_v2") {
		return false
	}
	count := 0
	for _, code := range r.Evidence {
		if strings.HasPrefix(code, "codex_outcome_v2:") {
			if code != "codex_outcome_v2:"+string(r.Outcome) {
				return false
			}
			count++
		}
	}
	return count == 1 && (r.Outcome == core.OutcomeUnknown || r.Outcome == core.OutcomeSuccess || r.Outcome == core.OutcomeFailed)
}

func codexOutcomeRank(outcome core.PerformanceOutcome) int {
	switch outcome {
	case core.OutcomeSuccess:
		return 1
	case core.OutcomeFailed:
		return 2
	default:
		return 0
	}
}

func performanceTimingRank(r core.PerformanceRequest) int {
	if r.TimingStatus != core.TimingValid && r.TimingStatus != core.TimingProvisional && r.TimingStatus != core.TimingMissing {
		return 0
	}
	if r.Scope != core.ScopeModelRequest {
		return 0
	}
	if r.DurationMS == nil || *r.DurationMS <= 0 || math.IsNaN(*r.DurationMS) || math.IsInf(*r.DurationMS, 0) {
		return 0
	}
	switch r.TimingBasis {
	case core.TimingProviderRequestTimestamps:
		if r.StartedAt == nil || r.EndedAt == nil || !r.EndedAt.After(*r.StartedAt) {
			return 0
		}
		return 30
	case core.TimingProviderReportedDuration:
		return 20
	case core.TimingInferredLogBoundary:
		if r.StartedAt == nil || r.EndedAt == nil || *r.DurationMS < 100 || *r.DurationMS > 900000 {
			return 0
		}
		return 10
	}
	return 0
}

// RecordPerformanceScan advances source freshness on the existing successful
// collection loop, even if no logs changed. It is not a request timestamp.
func (s *Store) RecordPerformanceScan(ctx context.Context, source, account string, at time.Time) error {
	if !s.performanceEnabled.Load() {
		return nil
	}
	if account == "" {
		account = "unknown"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO performance_sources(source_system,account_id,first_scan_ms,last_scan_ms) VALUES(?,?,?,?)
 ON CONFLICT(source_system,account_id) DO UPDATE SET last_scan_ms=excluded.last_scan_ms`, source, account, at.UnixMilli(), at.UnixMilli())
	return err
}

// PrunePerformance bounds local request metadata to the existing retention
// horizon. It does not touch accounting tables or create central history.
func (s *Store) PrunePerformance(ctx context.Context, before time.Time) (int64, error) {
	if !s.performanceEnabled.Load() {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM performance_requests WHERE occurred_at_ms < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE performance_meta SET value=value+1 WHERE key IN ('sequence','epoch')`); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
