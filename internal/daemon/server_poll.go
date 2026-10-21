package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

type pollProviderResult struct {
	accountID    string
	snapshot     core.UsageSnapshot
	shouldIngest bool
}

func pollSnapshotsForIngest(results []pollProviderResult) map[string]core.UsageSnapshot {
	snapshots := make(map[string]core.UsageSnapshot)
	for _, result := range results {
		if result.shouldIngest {
			snapshots[result.accountID] = result.snapshot
		}
	}
	return snapshots
}

// canRefreshUnchangedObservation is deliberately restricted to audited sources.
// Other adapters can return cached quotas or local session limits with a new
// snapshot timestamp. Until they expose trustworthy provenance, retain their
// existing change-only ingestion instead of making fallback data look current.
func canRefreshUnchangedObservation(snap core.UsageSnapshot) bool {
	if snap.Status != core.StatusOK && snap.Status != core.StatusNearLimit && snap.Status != core.StatusLimited {
		return false
	}
	switch snap.ProviderID {
	case "claude_code":
		return snap.Raw["usage_api_ok"] == "true"
	case "codex":
		return snap.Raw["rate_limit_source"] == "live" || snap.Raw["rate_limit_source"] == "cli_rpc"
	case "opencode", "dsh":
		// These adapters reread their source on each Fetch and do not substitute
		// previously successful quota responses on failure.
		return true
	default:
		return false
	}
}

func (s *Service) runPollLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	s.infof("poll_loop_start", "interval=%s", s.cfg.PollInterval)
	s.pollProviders(ctx)
	for {
		select {
		case <-ctx.Done():
			s.infof("poll_loop_stop", "reason=context_done")
			return
		case <-ticker.C:
			s.pollProviders(ctx)
		}
	}
}

func (s *Service) pollProviders(ctx context.Context) {
	if s == nil || s.quotaIngest == nil {
		return
	}
	started := time.Now()

	accounts, modelNorm, err := LoadAccountsAndNorm()
	if err != nil {
		if s.shouldLog("poll_config_warning", 20*time.Second) {
			s.warnf("poll_config_warning", "error=%v", err)
		}
		return
	}
	if len(accounts) == 0 {
		if s.shouldLog("poll_no_accounts", 30*time.Second) {
			s.infof("poll_skipped", "reason=no_enabled_accounts")
		}
		return
	}

	results := make(chan pollProviderResult, len(accounts))
	var wg sync.WaitGroup

	for _, acct := range accounts {
		wg.Add(1)
		go func(account core.AccountConfig) {
			defer wg.Done()

			// Honour shutdown immediately so we don't run a fresh fetch on
			// an account when the parent ctx has already been cancelled.
			// Without this check the per-fetch 8s timeout (below) is the
			// only ceiling on shutdown — N goroutines × 8s on big setups.
			select {
			case <-ctx.Done():
				return
			default:
			}

			provider, ok := s.providerByID[account.Provider]
			if !ok {
				results <- pollProviderResult{
					accountID: account.ID,
					snapshot: core.UsageSnapshot{
						ProviderID: account.Provider,
						AccountID:  account.ID,
						Timestamp:  s.now().UTC(),
						Status:     core.StatusError,
						Message:    fmt.Sprintf("no provider adapter registered for %q (restart/reinstall telemetry daemon if recently added)", account.Provider),
					},
					shouldIngest: true,
				}
				return
			}

			_, hasDetector := provider.(core.ChangeDetector)

			// Adaptive backoff: skip providers that are in a backoff window.
			if !s.pollScheduler.ShouldPoll(account.ID, hasDetector) {
				s.pollStateMu.Lock()
				state := s.pollState[account.ID]
				s.pollStateMu.Unlock()
				if state != nil && state.hasSnap {
					results <- pollProviderResult{accountID: account.ID, snapshot: state.lastSnap}
					return
				}
				// No cached snapshot yet — must fetch.
			}

			// Check if provider data has changed since last fetch (optional interface).
			if cached := s.skipUnchangedProvider(provider, account); cached != nil {
				s.pollScheduler.RecordPoll(account.ID, false)
				results <- pollProviderResult{accountID: account.ID, snapshot: *cached}
				return
			}

			fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()

			attemptedAt := s.beginCollectorAttempt("provider_poll", account.Provider, account.ID)
			snap, fetchErr := provider.Fetch(fetchCtx, account)
			if fetchErr != nil {
				snap = core.UsageSnapshot{
					ProviderID: account.Provider,
					AccountID:  account.ID,
					Timestamp:  s.now().UTC(),
					Status:     core.StatusError,
					Message:    fetchErr.Error(),
				}
			}
			snap = core.NormalizeUsageSnapshotWithConfig(snap, modelNorm)
			s.recordProviderHealth(account.Provider, account.ID, attemptedAt, snap, fetchErr)

			// Track whether data actually changed for adaptive backoff.
			changed := s.pollScheduler.SnapshotChanged(account.ID, snap)
			s.pollScheduler.RecordPoll(account.ID, changed)

			// Record successful fetch for future change detection.
			s.pollStateMu.Lock()
			s.pollState[account.ID] = &providerPollState{
				lastFetchAt: s.now(),
				lastSnap:    snap,
				hasSnap:     true,
			}
			s.pollStateMu.Unlock()

			// A verified fetch is a new observation even when values did not
			// change. Keep adaptive backoff driven by value changes. Unverified
			// cached fallbacks retain the previous change-only behavior.
			results <- pollProviderResult{accountID: account.ID, snapshot: snap,
				shouldIngest: changed || canRefreshUnchangedObservation(snap)}
		}(acct)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	snapshots := make(map[string]core.UsageSnapshot, len(accounts))
	pollResults := make([]pollProviderResult, 0, len(accounts))
	statusCounts := map[core.Status]int{}
	errorCount := 0
	for result := range results {
		pollResults = append(pollResults, result)
		snapshots[result.accountID] = result.snapshot
		statusCounts[result.snapshot.Status]++
		if result.snapshot.Status == core.StatusError {
			errorCount++
		}
	}
	if len(snapshots) == 0 {
		return
	}

	observedSnapshots := pollSnapshotsForIngest(pollResults)
	var ingestErr error
	if len(observedSnapshots) > 0 {
		ingestCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		ingestErr = s.ingestQuotaSnapshots(ingestCtx, observedSnapshots)
		cancel()
	}
	if ingestErr != nil && s.shouldLog("poll_ingest_warning", 10*time.Second) {
		s.warnf("poll_ingest_warning", "error=%v", ingestErr)
	}
	if ingestErr == nil && len(observedSnapshots) > 0 {
		s.markDataIngested()
	}

	durationMs := time.Since(started).Milliseconds()
	if ingestErr != nil || errorCount > 0 || s.shouldLog("poll_cycle_info", 45*time.Second) {
		s.infof(
			"poll_cycle",
			"duration_ms=%d accounts=%d snapshots=%d observations=%d status_ok=%d status_auth=%d status_limited=%d status_error=%d status_unknown=%d ingest_error=%t",
			durationMs,
			len(accounts),
			len(snapshots),
			len(observedSnapshots),
			statusCounts[core.StatusOK],
			statusCounts[core.StatusAuth],
			statusCounts[core.StatusLimited],
			statusCounts[core.StatusError],
			statusCounts[core.StatusUnknown],
			ingestErr != nil,
		)
	}
}

// skipUnchangedProvider checks if a provider's data source has changed since the last
// fetch. Returns the cached snapshot if unchanged, nil if a fresh Fetch() is needed.
func (s *Service) skipUnchangedProvider(provider core.UsageProvider, acct core.AccountConfig) *core.UsageSnapshot {
	detector, ok := provider.(core.ChangeDetector)
	if !ok {
		return nil // provider doesn't support change detection, always fetch
	}

	s.pollStateMu.Lock()
	state := s.pollState[acct.ID]
	s.pollStateMu.Unlock()

	if state == nil || !state.hasSnap {
		return nil // no previous fetch, must run
	}

	now := s.now()
	if snapshotResetPassed(state.lastSnap, state.lastFetchAt, now) {
		core.Tracef("[poll] %s/%s: forcing refresh because a reset boundary passed after %s", acct.Provider, acct.ID, state.lastFetchAt.Format(time.RFC3339))
		return nil
	}

	changed, err := detector.HasChanged(acct, state.lastFetchAt)
	if err != nil || changed {
		return nil // error or changed — run Fetch()
	}

	core.Tracef("[poll] %s/%s: skipped (no change since %s)", acct.Provider, acct.ID, state.lastFetchAt.Format(time.RFC3339))
	snap := state.lastSnap
	return &snap
}

func snapshotResetPassed(snap core.UsageSnapshot, since, now time.Time) bool {
	if since.IsZero() || len(snap.Resets) == 0 {
		return false
	}
	for _, resetAt := range snap.Resets {
		if resetAt.IsZero() {
			continue
		}
		if resetAt.After(since) && !resetAt.After(now) {
			return true
		}
	}
	return false
}
