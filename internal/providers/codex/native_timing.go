package codex

// This reader uses only Codex's own logs SQLite database. The reviewed producer
// is openai/codex 83086188 (0.163.0): turn.rs sampling feedback tags precede
// client.rs's successful response feedback tag. The first boundary has no
// response ID; any duration derived here is inferred, never direct timing.

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-sqlite3"
)

const (
	nativeTimingReadLimit  = 32768
	nativeTimingEventLimit = 16384
	nativeTimingBodyLimit  = 64 * 1024
	nativeTimingUsageLimit = 8192
	nativeTimingMin        = 100 * time.Millisecond
	nativeTimingMax        = 15 * time.Minute
)

type nativeTimingUsage struct {
	ResponseID, ThreadID, TurnID, CLIVersion string
}

type nativeTimingMatch struct {
	Completed              bool
	StartedAt, CompletedAt time.Time
	Reason                 string
	// Contradicted means retained positive evidence disproves a previous
	// inference. Mere missing/rotated/temporarily unavailable data never does.
	Contradicted     bool
	EvidenceRevision int64
}

type nativeTimingStats struct {
	Reason                   string
	RowsRead, EventsRetained int
	Cursor                   int64
	Reset, More              bool
}

type nativeTimingEvent struct {
	id                              int64
	at, gapFrom                     time.Time
	process, thread, turn, response string
	start, barrier, retry           bool
}

type nativeTimingAnchor struct {
	id, seconds, nanos int64
	process            string
}

// Zero value is usable. One instance belongs to one profile/account. Changing
// the canonical database path discards its metadata. No raw log text is kept.
type nativeTimingReader struct {
	mu        sync.Mutex
	path      string
	info      os.FileInfo
	cursor    int64
	anchor    nativeTimingAnchor
	events    []nativeTimingEvent
	floor     time.Time
	more      bool
	available bool
}

func (r *nativeTimingReader) Scan(ctx context.Context, path string) (stats nativeTimingStats, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.available = false
	defer func() {
		stats.Cursor, stats.EventsRetained = r.cursor, len(r.events)
		if err != nil && stats.Reason == "" {
			stats.Reason = nativeTimingErrorReason(err)
		}
	}()
	path, err = filepath.Abs(path)
	if err != nil {
		return stats, errors.New("codex native timing: invalid database path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return stats, err
	}
	if !info.Mode().IsRegular() {
		stats.Reason = "unsupported_schema"
		return stats, errors.New("codex native timing: database is not a regular file")
	}
	if resolved, e := filepath.EvalSymlinks(path); e == nil {
		path = resolved
	}
	reset := r.path != path || r.info == nil || !os.SameFile(r.info, info)
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_query_only", "1")
	q.Set("_busy_timeout", "100")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return stats, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := db.BeginTx(readCtx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return stats, err
	}
	defer tx.Rollback()
	if err = nativeTimingValidateSchema(readCtx, tx); err != nil {
		if errors.Is(err, errNativeTimingSchema) {
			stats.Reason = "unsupported_schema"
		}
		return stats, err
	}
	var maxID sql.NullInt64
	if err = tx.QueryRowContext(readCtx, "SELECT MAX(id) FROM logs").Scan(&maxID); err != nil {
		return stats, err
	}
	if maxID.Int64 < r.cursor {
		reset = true
	}
	if !reset && r.cursor > 0 {
		var a nativeTimingAnchor
		err = tx.QueryRowContext(readCtx, "SELECT id,ts,ts_nanos,CASE WHEN length(process_uuid)<=128 THEN process_uuid ELSE '' END FROM logs WHERE id=?", r.cursor).Scan(&a.id, &a.seconds, &a.nanos, &a.process)
		if errors.Is(err, sql.ErrNoRows) || err == nil && a != r.anchor {
			reset = true
			err = nil
		}
		if err != nil {
			return stats, err
		}
	}
	cursor, floor := r.cursor, r.floor
	var events []nativeTimingEvent
	if reset {
		cursor = 0
	} else {
		events = append(events, r.events...)
	}
	if reset {
		floor = time.Time{}
	}
	// LIMIT applies to all rows, not only feedback rows: a busy unrelated log
	// cannot cause an unbounded table scan. The CASE expression withholds large
	// or unrelated bodies before they cross the SQLite boundary.
	rows, err := tx.QueryContext(readCtx, `SELECT id,ts,ts_nanos,
	 CASE WHEN length(process_uuid)<=128 THEN process_uuid ELSE NULL END,
	 target='feedback_tags',
	 CASE WHEN target='feedback_tags' AND length(CAST(feedback_log_body AS BLOB))<=? THEN feedback_log_body ELSE NULL END
	 FROM logs WHERE id>? AND id<=? ORDER BY id LIMIT ?`, nativeTimingBodyLimit, cursor, maxID.Int64, nativeTimingReadLimit)
	if err != nil {
		return stats, err
	}
	anchor := r.anchor
	if reset {
		anchor = nativeTimingAnchor{}
	}
	for rows.Next() {
		var id, sec, nano int64
		var process, body sql.NullString
		var feedback sql.NullBool
		if err = rows.Scan(&id, &sec, &nano, &process, &feedback, &body); err != nil {
			rows.Close()
			return stats, err
		}
		stats.RowsRead++
		if sec < 0 || sec > 9223372036 || nano < 0 || nano >= 1e9 {
			// A malformed clock cannot safely establish a retention boundary.
			stats.Reason = "invalid_log_timestamp"
			rows.Close()
			return stats, errors.New("codex native timing: invalid log timestamp")
		}
		at := time.Unix(sec, nano).UTC()
		if cursor > 0 && id > cursor+1 {
			from, to := time.Unix(anchor.seconds, anchor.nanos).UTC(), at
			if from.After(to) {
				from, to = to, from
			}
			// A pruned range can hide another start or completion. Preserve only
			// a time-gap marker; it cannot establish a unique request interval.
			events = append(events, nativeTimingEvent{id: id, at: to, gapFrom: from, barrier: true})
		}
		cursor = id
		anchor = nativeTimingAnchor{id, sec, nano, process.String}
		if floor.IsZero() {
			floor = at
		}
		if !feedback.Bool {
			continue
		}
		e := nativeTimingEvent{id: id, at: at, process: process.String}
		if !body.Valid || !nativeTimingIdentifier(e.process) {
			e.barrier = true
			events = append(events, e)
			continue
		}
		fields, sampling := nativeTimingFields(body.String)
		if retry := fields["auth_retry_after_unauthorized"]; len(retry) == 1 && retry[0] == "true" {
			e.barrier, e.retry = true, true
			events = append(events, e)
			continue
		}
		if responses := fields["last_model_response_id"]; len(responses) > 0 {
			if len(responses) != 1 || !nativeTimingIdentifier(responses[0]) {
				e.barrier = true
			} else {
				e.response = responses[0]
			}
			events = append(events, e)
			continue
		}
		if sampling && len(fields["approval_policy"]) > 0 && len(fields["sandbox_policy"]) > 0 && len(fields["tags_json"]) > 0 {
			thread := nativeTimingUnique(fields, "thread_id", "thread.id", "conversation.id")
			turn := nativeTimingUnique(fields, "turn_id", "turn.id")
			if thread == "" || turn == "" {
				e.barrier = true
			} else {
				e.start = true
				e.thread = thread
				e.turn = turn
			}
			events = append(events, e)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return stats, err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return stats, err
	}
	afterInfo, statErr := os.Stat(path)
	if statErr != nil || !os.SameFile(info, afterInfo) {
		stats.Reason = "database_rotated"
		return stats, errors.New("codex native timing: database changed during read")
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].at.Equal(events[j].at) {
			return events[i].id < events[j].id
		}
		return events[i].at.Before(events[j].at)
	})
	if len(events) > nativeTimingEventLimit {
		n := len(events) - nativeTimingEventLimit
		if events[n-1].at.After(floor) {
			floor = events[n-1].at
		}
		events = append([]nativeTimingEvent(nil), events[n:]...)
	}
	r.path, r.info, r.cursor, r.floor, r.events = path, info, cursor, floor, events
	if stats.RowsRead > 0 {
		r.anchor = anchor
	} else if reset {
		r.anchor = nativeTimingAnchor{}
	}
	r.more = cursor < maxID.Int64
	r.available = true
	stats.Reset, stats.More = reset, r.more
	stats.Reason = "ok"
	if r.more {
		stats.Reason = "backfill_pending"
	}
	return stats, nil
}

var errNativeTimingSchema = errors.New("codex native timing: unsupported logs schema")

func nativeTimingValidateSchema(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info(logs)")
	if err != nil {
		return err
	}
	defer rows.Close()
	required := map[string]string{"id": "INTEGER", "ts": "INTEGER", "ts_nanos": "INTEGER", "target": "TEXT", "feedback_log_body": "TEXT", "process_uuid": "TEXT"}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			return err
		}
		if expected, ok := required[name]; ok {
			if strings.ToUpper(strings.TrimSpace(typ)) != expected || name == "id" && pk != 1 {
				return errNativeTimingSchema
			}
			delete(required, name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(required) > 0 {
		return errNativeTimingSchema
	}
	return nil
}

func nativeTimingErrorReason(err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return "missing_db"
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission_denied"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "read_timeout"
	}
	var se sqlite3.Error
	if errors.As(err, &se) {
		if se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked {
			return "locked"
		}
		if se.Code == sqlite3.ErrPerm || se.Code == sqlite3.ErrAuth {
			return "permission_denied"
		}
		if se.Code == sqlite3.ErrNotADB || se.Code == sqlite3.ErrCorrupt {
			return "unsupported_schema"
		}
	}
	return "read_error"
}

func nativeTimingIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return false
		}
	}
	return true
}

// Parse tracing's scalar fields while skipping quoted contents. A prompt or
// JSON value containing text resembling an identifier cannot become evidence.
func nativeTimingFields(body string) (map[string][]string, bool) {
	allow := map[string]bool{"thread_id": true, "thread.id": true, "conversation.id": true, "turn_id": true, "turn.id": true, "last_model_response_id": true, "approval_policy": true, "sandbox_policy": true, "tags_json": true, "auth_retry_after_unauthorized": true}
	out := make(map[string][]string)
	sampling := false
	for i := 0; i < len(body); {
		if body[i] == '"' {
			_, end := nativeTimingQuoted(body, i)
			i = end
			continue
		}
		if !nativeTimingKeyByte(body[i]) {
			i++
			continue
		}
		start := i
		for i < len(body) && nativeTimingKeyByte(body[i]) {
			i++
		}
		key := body[start:i]
		j := i
		for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
			j++
		}
		if key == "try_run_sampling_request" && j < len(body) && body[j] == '{' {
			sampling = true
		}
		if !allow[key] || j >= len(body) || (body[j] != '=' && body[j] != ':') {
			continue
		}
		j++
		for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
			j++
		}
		if j >= len(body) {
			break
		}
		var value string
		if body[j] == '"' {
			value, i = nativeTimingQuoted(body, j)
		} else {
			start = j
			for j < len(body) && body[j] != ' ' && body[j] != '\t' && body[j] != '\n' && body[j] != '}' && body[j] != '{' && body[j] != ',' {
				j++
			}
			value = body[start:j]
			i = j
		}
		if key == "approval_policy" || key == "sandbox_policy" || key == "tags_json" {
			out[key] = []string{"present"}
			continue
		}
		if !nativeTimingIdentifier(value) {
			value = "" // retain only an invalid-field marker, never its raw contents
		}
		seen := false
		for _, v := range out[key] {
			if v == value {
				seen = true
			}
		}
		if !seen {
			out[key] = append(out[key], value)
		}
	}
	return out, sampling
}

func nativeTimingKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.'
}
func nativeTimingQuoted(s string, start int) (string, int) {
	for i := start + 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			v, err := strconv.Unquote(s[start : i+1])
			if err != nil {
				return "", i + 1
			}
			return v, i + 1
		}
	}
	return "", len(s)
}
func nativeTimingUnique(fields map[string][]string, keys ...string) string {
	v := ""
	for _, key := range keys {
		for _, s := range fields[key] {
			if s == "" {
				return ""
			}
			if v != "" && v != s {
				return ""
			}
			v = s
		}
	}
	return v
}

type nativeTimingScope struct{ process, thread, turn string }

func (r *nativeTimingReader) MatchBatch(usage []nativeTimingUsage) map[string]nativeTimingMatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]nativeTimingMatch)
	known := make(map[string]nativeTimingUsage)
	conflict := make(map[string]bool)
	for _, u := range usage {
		if !nativeTimingIdentifier(u.ResponseID) {
			continue
		}
		if old, ok := known[u.ResponseID]; ok && old != u {
			conflict[u.ResponseID] = true
		}
		known[u.ResponseID] = u
	}
	for id := range known {
		out[id] = nativeTimingMatch{Reason: "missing_completion"}
	}
	if len(usage) > nativeTimingUsageLimit {
		for id := range out {
			out[id] = nativeTimingMatch{Reason: "usage_cache_capacity"}
		}
		return out
	}
	if !r.available {
		for id := range out {
			out[id] = nativeTimingMatch{Reason: "logs_unavailable"}
		}
		return out
	}
	starts := make(map[nativeTimingScope][]nativeTimingEvent)
	ends := make(map[string][]nativeTimingEvent)
	scopedEnds := make(map[nativeTimingScope][]nativeTimingEvent)
	var barriers []nativeTimingEvent
	for _, e := range r.events {
		if e.start {
			key := nativeTimingScope{e.process, e.thread, e.turn}
			starts[key] = append(starts[key], e)
		}
		if e.barrier {
			barriers = append(barriers, e)
		}
		if e.response != "" {
			ends[e.response] = append(ends[e.response], e)
			if u, ok := known[e.response]; ok && !conflict[e.response] && u.CLIVersion == "0.163.0" && nativeTimingIdentifier(u.ThreadID) && nativeTimingIdentifier(u.TurnID) {
				key := nativeTimingScope{e.process, u.ThreadID, u.TurnID}
				scopedEnds[key] = append(scopedEnds[key], e)
			} else {
				barriers = append(barriers, e)
			}
		}
	}
	for id, u := range known {
		m := out[id]
		if u.CLIVersion != "0.163.0" {
			m.Reason = "unsupported_codex_version"
			out[id] = m
			continue
		}
		if conflict[id] {
			m.Reason = "ambiguous_usage_identity"
			m.Contradicted = true
			out[id] = m
			continue
		}
		es := ends[id]
		if len(es) == 0 {
			continue
		}
		m.Completed = true
		m.CompletedAt = es[0].at
		for _, item := range es {
			m.EvidenceRevision = max(m.EvidenceRevision, item.id)
		}
		if len(es) != 1 {
			m.Reason = "ambiguous_completion"
			m.Contradicted = true
			out[id] = m
			continue
		}
		e := es[0]
		if !nativeTimingIdentifier(u.ThreadID) || !nativeTimingIdentifier(u.TurnID) {
			m.Reason = "missing_usage_scope"
			out[id] = m
			continue
		}
		if r.more {
			m.Reason = "backfill_pending"
			out[id] = m
			continue
		}
		key := nativeTimingScope{e.process, u.ThreadID, u.TurnID}
		previous := time.Time{}
		for _, other := range scopedEnds[key] {
			if other.response != id && other.at.Before(e.at) && other.at.After(previous) {
				previous = other.at
			}
		}
		var candidates []nativeTimingEvent
		for _, s := range starts[key] {
			if s.at.After(previous) && !s.at.After(e.at) {
				candidates = append(candidates, s)
			}
		}
		if len(candidates) != 1 {
			m.Reason = "missing_start"
			if len(candidates) > 1 {
				m.Reason = "ambiguous_sampling_starts"
				m.Contradicted = true
				for _, s := range candidates {
					m.EvidenceRevision = max(m.EvidenceRevision, s.id)
				}
			}
			out[id] = m
			continue
		}
		s := candidates[0]
		m.EvidenceRevision = max(m.EvidenceRevision, s.id)
		if !s.at.After(r.floor) {
			m.Reason = "partial_retention"
			out[id] = m
			continue
		}
		elapsed := e.at.Sub(s.at)
		if elapsed < nativeTimingMin || elapsed > nativeTimingMax {
			m.Reason = "invalid_duration"
			m.Contradicted = true
			out[id] = m
			continue
		}
		for _, b := range barriers {
			if !b.gapFrom.IsZero() {
				if !b.at.Before(s.at) && !b.gapFrom.After(e.at) && !m.Contradicted {
					m.Reason = "partial_retention"
				}
				continue
			}
			if (b.process == "" || b.process == e.process) && !b.at.Before(s.at) && !b.at.After(e.at) {
				if b.retry {
					m.Reason = "ambiguous_transport_retry"
					m.Contradicted = true
				} else if !m.Contradicted {
					m.Reason = "ambiguous_unmapped_completion"
				}
				m.EvidenceRevision = max(m.EvidenceRevision, b.id)
			}
		}
		// Different turns in the same thread/process may overlap. A prior start
		// is closed only by a known matching completion before this start.
		for otherKey, ss := range starts {
			if otherKey.process != key.process || otherKey.thread != key.thread || otherKey.turn == key.turn {
				continue
			}
			for _, other := range ss {
				if other.at.After(e.at) {
					continue
				}
				closed := false
				for _, end := range scopedEnds[otherKey] {
					if !end.at.Before(other.at) && end.at.Before(s.at) {
						closed = true
						break
					}
				}
				if !closed {
					m.Reason = "ambiguous_parallel_sampling"
					m.Contradicted = true
					m.EvidenceRevision = max(m.EvidenceRevision, other.id)
				}
			}
		}
		if m.Contradicted || strings.HasPrefix(m.Reason, "ambiguous_") || m.Reason == "partial_retention" {
			out[id] = m
			continue
		}
		m.StartedAt = s.at
		m.Reason = "inferred_native_sampling"
		out[id] = m
	}
	return out
}
