package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
	"github.com/mattn/go-sqlite3"
)

// CollectorHealth describes the last attempted source read/provider fetch in
// this process. It is not an acknowledgement of durable telemetry ingestion.
// No provider error text, source path, payload or credential is retained here.
type CollectorHealth struct {
	Source         string    `json:"source"`
	AccountID      string    `json:"account_id,omitempty"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	LastAttemptAt  time.Time `json:"last_attempt_at,omitempty,omitzero"`
	LastSuccessAt  time.Time `json:"last_success_at,omitempty,omitzero"`
	LastEventAt    time.Time `json:"last_event_at,omitempty,omitzero"`
	CollectedCount int       `json:"collected_count"`
}

func collectorHealthKey(kind, source, account string) string {
	return kind + "/" + url.PathEscape(source) + "/" + url.PathEscape(account)
}

func (s *Service) beginCollectorAttempt(kind, source, account string) time.Time {
	at := s.now().UTC()
	key := collectorHealthKey(kind, source, account)
	s.collectorHealthMu.Lock()
	defer s.collectorHealthMu.Unlock()
	if s.collectorHealth == nil {
		s.collectorHealth = make(map[string]CollectorHealth)
	}
	entry, exists := s.collectorHealth[key]
	if !exists {
		entry = CollectorHealth{Source: source, AccountID: account, Kind: kind, Status: "unknown"}
	}
	if !at.Before(entry.LastAttemptAt) {
		entry.LastAttemptAt = at
	}
	s.collectorHealth[key] = entry
	return at
}

func (s *Service) finishCollectorAttempt(kind, source, account string, attempted time.Time, status string, count int, eventAt time.Time, success bool) {
	key := collectorHealthKey(kind, source, account)
	s.collectorHealthMu.Lock()
	defer s.collectorHealthMu.Unlock()
	entry, exists := s.collectorHealth[key]
	if !exists || attempted.Before(entry.LastAttemptAt) {
		return
	}
	entry.Status = status
	entry.CollectedCount = count
	if success {
		entry.LastSuccessAt = s.now().UTC()
	}
	if eventAt.After(entry.LastEventAt) {
		entry.LastEventAt = eventAt.UTC()
	}
	s.collectorHealth[key] = entry
}

func (s *Service) collectorHealthSnapshot() map[string]CollectorHealth {
	if s == nil {
		return nil
	}
	s.collectorHealthMu.RLock()
	defer s.collectorHealthMu.RUnlock()
	if len(s.collectorHealth) == 0 {
		return nil
	}
	out := make(map[string]CollectorHealth, len(s.collectorHealth))
	for key, value := range s.collectorHealth {
		out[key] = value
	}
	return out
}

func collectorErrorCode(err error) string {
	if err == nil {
		return "ok"
	}
	var coded interface{ CollectionErrorCode() string }
	if errors.As(err, &coded) {
		switch code := coded.CollectionErrorCode(); code {
		case "missing", "unsupported_schema", "database_busy", "permission", "parse_error", "auth_required", "error", "unknown":
			return code
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return "missing"
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission"
	}
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) && (sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked) {
		return "database_busy"
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return "parse_error"
	}
	return "error"
}

func (s *Service) collectWithHealth(ctx context.Context, collector telemetry.Collector) ([]telemetry.IngestRequest, error) {
	source, account := collector.Name(), ""
	if scoped, ok := collector.(*telemetry.SourceCollector); ok {
		account = scoped.AccountOverride
	}
	at := s.beginCollectorAttempt("source", source, account)
	reqs, err := collector.Collect(ctx)
	status := collectorErrorCode(err)
	count := 0
	var eventAt time.Time
	if err == nil {
		count = len(reqs)
		if count == 0 {
			status = "no_new_events"
		}
		for _, req := range reqs {
			switch req.EventType {
			case telemetry.EventTypeMessageUsage, telemetry.EventTypeToolUsage, telemetry.EventTypeTurnCompleted:
				if req.OccurredAt.After(eventAt) {
					eventAt = req.OccurredAt
				}
			}
		}
	}
	s.finishCollectorAttempt("source", source, account, at, status, count, eventAt, err == nil)
	return reqs, err
}

func (s *Service) recordProviderHealth(source, account string, at time.Time, snap core.UsageSnapshot, err error) {
	status := collectorErrorCode(err)
	success := false
	if err == nil {
		switch snap.Status {
		case core.StatusOK, core.StatusNearLimit, core.StatusLimited:
			success = true
		case core.StatusAuth:
			status = "auth_required"
		case core.StatusError:
			status = "error"
		default:
			status = "unknown"
		}
	}
	count := 0
	if success {
		count = 1
	}
	// A quota snapshot's observation timestamp is never a usage-event time.
	s.finishCollectorAttempt("provider_poll", source, account, at, status, count, time.Time{}, success)
}
