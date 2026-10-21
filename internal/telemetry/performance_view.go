package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/performance"
)

var ErrPerformanceDisabled = errors.New("performance is not enabled in this database")

// PerformanceQuery reads only the local metadata table. Empty dimensions mean
// all values, never an account/provider attribution guess. Source is the tool;
// BillingProvider is the actual model vendor recorded by the source.
type PerformanceQuery struct {
	Source, Account, Model, Session, BillingProvider string
	Since, Until, Now                                time.Time
	BufferMinutes                                    int
	ExportIntervalMS                                 int64
	RequestLimit                                     int
	IncludeWindow                                    bool
}

type performanceCacheRecord struct {
	request  core.PerformanceRequest
	occurred int64
}
type performanceCacheEntry struct {
	mu                        sync.Mutex
	records                   map[string]performanceCacheRecord
	sequence, epoch, instance int64
	ready                     bool
	ring                      *performance.Ring
	ringNow                   time.Time
}

var performanceCache = struct {
	sync.Mutex
	entries map[string]*performanceCacheEntry
	order   []string
}{entries: make(map[string]*performanceCacheEntry)}

func getPerformanceCache(key string) *performanceCacheEntry {
	performanceCache.Lock()
	defer performanceCache.Unlock()
	if e := performanceCache.entries[key]; e != nil {
		return e
	}
	e := &performanceCacheEntry{}
	performanceCache.entries[key] = e
	performanceCache.order = append(performanceCache.order, key)
	if len(performanceCache.order) > 32 {
		delete(performanceCache.entries, performanceCache.order[0])
		performanceCache.order = performanceCache.order[1:]
	}
	return e
}

// QueryPerformance never initializes/migrates a store or scans provider files.
func QueryPerformance(ctx context.Context, dbPath string, q PerformanceQuery) (*core.PerformanceSnapshot, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	db, err := openReadOnlyDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return queryPerformanceWithDB(ctx, db, dbPath, q)
}

func queryPerformanceWithDB(ctx context.Context, db *sql.DB, namespace string, q PerformanceQuery) (*core.PerformanceSnapshot, error) {
	if q.Now.IsZero() {
		q.Now = time.Now().UTC()
	}
	if q.Until.IsZero() {
		q.Until = q.Now
	}
	if !q.Since.IsZero() && !q.Until.After(q.Since) {
		return nil, fmt.Errorf("performance period end must follow start")
	}
	if q.BufferMinutes != 60 {
		q.BufferMinutes = 30
	}
	if q.ExportIntervalMS <= 0 {
		q.ExportIntervalMS = 60000
	}
	filterBytes, _ := json.Marshal([]string{q.Source, q.Account, q.Model, q.Session, q.BillingProvider})
	opts := performance.BuildOptions{Period: core.PerformancePeriod{Start: q.Since, End: q.Until}, FilterID: string(filterBytes), Now: q.Now,
		BufferMinutes: q.BufferMinutes, ExportIntervalMS: q.ExportIntervalMS, RequestLimit: q.RequestLimit, GroupLimit: 128,
		QueryPath: "openusage telemetry performance --json", IncludeWindow: false}
	loadSince := q.Since
	windowStart := q.Now.Add(-time.Duration(q.BufferMinutes)*time.Minute - time.Minute)
	if q.IncludeWindow && !loadSince.IsZero() && windowStart.Before(loadSince) {
		loadSince = windowStart
	}
	// Rolling 3/7/30-day views change their cutoff every refresh. Cache a
	// superset from the UTC day boundary so repeated reads consume only new
	// change_seq rows; Build still applies the exact requested period. The
	// bounded projection reloads at a day rollover, not on each poll.
	if !loadSince.IsZero() {
		loadSince = loadSince.UTC().Truncate(24 * time.Hour)
	}
	keyBytes, _ := json.Marshal([]any{namespace, q.Source, q.Account, loadSince.UnixMilli(), q.BufferMinutes})
	entry := getPerformanceCache(string(keyBytes))
	entry.mu.Lock()
	defer entry.mu.Unlock()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='performance_meta'`).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrPerformanceDisabled
	}
	var seq, epoch, instance, version int64
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM performance_meta`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return nil, err
		}
		switch k {
		case "sequence":
			seq = v
		case "epoch":
			epoch = v
		case "instance":
			instance = v
		case "schema_version":
			version = v
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if version != 1 {
		return nil, fmt.Errorf("unsupported performance schema %d", version)
	}
	reset := !entry.ready || entry.instance != instance || entry.epoch != epoch || entry.sequence > seq
	if reset {
		entry.records = make(map[string]performanceCacheRecord)
		entry.sequence = 0
		ring, err := performance.NewRing(q.BufferMinutes)
		if err != nil {
			return nil, err
		}
		entry.ring = ring
	}
	if entry.ring != nil {
		if !entry.ringNow.IsZero() && q.Now.Before(entry.ringNow) {
			ring, err := performance.NewRing(q.BufferMinutes)
			if err != nil {
				return nil, err
			}
			entry.ring = ring
			if err := entry.ring.Advance(q.Now); err != nil {
				return nil, err
			}
			for _, record := range entry.records {
				if err := entry.ring.Upsert(record.request); err != nil {
					return nil, err
				}
			}
		}
		if err := entry.ring.Advance(q.Now); err != nil {
			return nil, err
		}
	}
	entry.ringNow = q.Now
	where := `change_seq <= ?`
	args := []any{seq}
	if !reset {
		where += ` AND change_seq > ?`
		args = append(args, entry.sequence)
	}
	if q.Source != "" {
		where += ` AND source_system = ?`
		args = append(args, q.Source)
	}
	if q.Account != "" {
		where += ` AND account_id = ?`
		args = append(args, q.Account)
	}
	if reset && !loadSince.IsZero() {
		where += ` AND occurred_at_ms >= ?`
		args = append(args, loadSince.UnixMilli())
	}
	if reset || seq != entry.sequence {
		rows, err := tx.QueryContext(ctx, `SELECT request_key,record_json,source_path,occurred_at_ms FROM performance_requests WHERE `+where, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, raw, path string
			var at int64
			if err := rows.Scan(&key, &raw, &path, &at); err != nil {
				rows.Close()
				return nil, err
			}
			var r core.PerformanceRequest
			if err := json.Unmarshal([]byte(raw), &r); err != nil {
				rows.Close()
				return nil, fmt.Errorf("decode performance row: %w", err)
			}
			r.SourcePath = path
			r.RecordedAt = time.UnixMilli(at).UTC()
			if !loadSince.IsZero() && at < loadSince.UnixMilli() {
				delete(entry.records, key)
				entry.ring.Remove(r)
				continue
			}
			if _, exists := entry.records[key]; !exists && len(entry.records) >= performance.MaxRecords {
				rows.Close()
				entry.ready = false
				return nil, fmt.Errorf("performance: local projection exceeds %d records; narrow source/account/period filter", performance.MaxRecords)
			}
			entry.records[key] = performanceCacheRecord{request: r, occurred: at}
			if err := entry.ring.Upsert(r); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	// Read freshness in the same SQLite snapshot as the request watermark.
	scanWhere := `1=1`
	scanArgs := []any{}
	if q.Source != "" {
		scanWhere += ` AND source_system=?`
		scanArgs = append(scanArgs, q.Source)
	}
	if q.Account != "" {
		scanWhere += ` AND account_id=?`
		scanArgs = append(scanArgs, q.Account)
	}
	var first, last sql.NullInt64
	scanRows, err := tx.QueryContext(ctx, `SELECT source_system,account_id,first_scan_ms,last_scan_ms FROM performance_sources WHERE `+scanWhere, scanArgs...)
	if err != nil {
		return nil, err
	}
	scanned := map[string]bool{}
	scanKey := func(source, account string) string {
		encoded, _ := json.Marshal([]string{source, account})
		return string(encoded)
	}
	for scanRows.Next() {
		var source, account string
		var began, checked int64
		if err := scanRows.Scan(&source, &account, &began, &checked); err != nil {
			scanRows.Close()
			return nil, err
		}
		scanned[scanKey(source, account)] = true
		if !first.Valid || began > first.Int64 {
			first = sql.NullInt64{Int64: began, Valid: true}
		}
		if !last.Valid || checked < last.Int64 {
			last = sql.NullInt64{Int64: checked, Valid: true}
		}
	}
	if err := scanRows.Err(); err != nil {
		scanRows.Close()
		return nil, err
	}
	scanRows.Close()
	// A heartbeat from one source/account cannot prove freshness for another
	// represented in this selection. Missing scan rows mean unknown coverage.
	for _, record := range entry.records {
		r := record.request
		if q.Model != "" && r.ModelID != q.Model || q.Session != "" && r.SessionID != q.Session || q.BillingProvider != "" && r.BillingProviderID != q.BillingProvider {
			continue
		}
		at := time.UnixMilli(record.occurred)
		inPeriod := !at.Before(opts.Period.Start) && at.Before(opts.Period.End)
		inWindow := q.IncludeWindow && !at.Before(q.Now.Add(-time.Duration(q.BufferMinutes)*time.Minute-10*time.Second)) && !at.After(q.Now)
		if (inPeriod || inWindow) && !scanned[scanKey(r.SourceSystem, r.AccountID)] {
			first.Valid = false
			last.Valid = false
			break
		}
	}
	if first.Valid {
		v := time.UnixMilli(first.Int64).UTC()
		opts.CoverageStart = &v
	}
	if last.Valid {
		opts.DataAsOf = time.UnixMilli(last.Int64).UTC()
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	entry.sequence, entry.epoch, entry.instance, entry.ready = seq, epoch, instance, true
	records := make([]core.PerformanceRequest, 0, len(entry.records))
	for _, v := range entry.records {
		r := v.request
		if q.Model != "" && r.ModelID != q.Model {
			continue
		}
		if q.Session != "" && r.SessionID != q.Session {
			continue
		}
		if q.BillingProvider != "" && r.BillingProviderID != q.BillingProvider {
			continue
		}
		records = append(records, r)
	}
	result, err := performance.Build(records, opts)
	if err != nil {
		return nil, err
	}
	if q.IncludeWindow {
		opts.IncludeWindow = true
		// Per-model/session filters share the durable/cache request set, but need
		// their own window projection. The default account view reuses its ring.
		var window *core.PerformanceSnapshot
		if q.Model == "" && q.Session == "" && q.BillingProvider == "" {
			window, err = entry.ring.Snapshot(opts)
		} else {
			window, err = performance.Build(records, opts)
		}
		if err != nil {
			return nil, err
		}
		result.Window = window.Window
	}
	return result, nil
}

func applyPerformanceView(ctx context.Context, db *sql.DB, path string, snaps map[string]core.UsageSnapshot, opts ReadModelOptions) (map[string]core.UsageSnapshot, error) {
	ids := make([]string, 0, len(snaps))
	for id := range snaps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		snap := snaps[id]
		account := core.FirstNonEmpty(snap.AccountID, id)
		p, err := queryPerformanceWithDB(ctx, db, path, PerformanceQuery{Source: strings.ToLower(snap.ProviderID), Account: account, Since: opts.Since, Now: opts.Now,
			BufferMinutes: opts.PerformanceBufferMinutes, ExportIntervalMS: opts.PerformanceExportIntervalMS, IncludeWindow: true})
		if err != nil {
			// Performance diagnostics must never suppress existing accounting/quota.
			snap.SetDiagnostic("performance_status", "unavailable")
			if errors.Is(err, ErrPerformanceDisabled) {
				snap.SetDiagnostic("performance_status", "disabled_or_not_migrated")
			}
		} else {
			snap.Performance = p
		}
		snaps[id] = snap
	}
	return snaps, nil
}
