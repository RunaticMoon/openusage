package codex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var nativeTestEpoch = time.Unix(1700000000, 0).UTC()

func nativeTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logs_2.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(`CREATE TABLE logs(id INTEGER PRIMARY KEY,ts INTEGER,ts_nanos INTEGER,target TEXT,feedback_log_body TEXT,process_uuid TEXT)`); err != nil {
		t.Fatal(err)
	}
	return db, path
}

func nativeTestRow(t *testing.T, db *sql.DB, at time.Time, target, body, process string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO logs(ts,ts_nanos,target,feedback_log_body,process_uuid) VALUES(?,?,?,?,?)`, at.Unix(), at.Nanosecond(), target, body, process); err != nil {
		t.Fatal(err)
	}
}

func nativeTestStart(thread, turn string) string {
	return fmt.Sprintf(`try_run_sampling_request{thread_id=%q turn_id=%q}: model="model" approval_policy="never" sandbox_policy="workspace" tags_json="{}"`, thread, turn)
}
func nativeTestEnd(response string) string { return fmt.Sprintf(`last_model_response_id=%q`, response) }
func nativeTestUsage(response, thread, turn string) nativeTimingUsage {
	return nativeTimingUsage{response, thread, turn, "0.163.0"}
}
func nativeTestScan(t *testing.T, r *nativeTimingReader, path string) nativeTimingStats {
	t.Helper()
	stats, err := r.Scan(context.Background(), path)
	if err != nil {
		t.Fatalf("scan failed (%s)", stats.Reason)
	}
	return stats
}
func nativeTestBaseline(t *testing.T, db *sql.DB) {
	t.Helper()
	nativeTestRow(t, db, nativeTestEpoch, "codex", "unrelated log", "process")
}

func TestNativeTimingExactCompletionAndInferredSequence(t *testing.T) {
	db, path := nativeTestDB(t)
	nativeTestBaseline(t, db)
	nativeTestRow(t, db, nativeTestEpoch.Add(time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
	nativeTestRow(t, db, nativeTestEpoch.Add(3*time.Second), "feedback_tags", nativeTestEnd("response"), "process")
	var r nativeTimingReader
	s := nativeTestScan(t, &r, path)
	if s.RowsRead != 3 || s.EventsRetained != 2 || s.More {
		t.Fatalf("unexpected counts: %+v", s)
	}
	m := r.MatchBatch([]nativeTimingUsage{nativeTestUsage("response", "thread", "turn")})["response"]
	if !m.Completed || m.Reason != "inferred_native_sampling" || m.CompletedAt.Sub(m.StartedAt) != 2*time.Second || m.Contradicted {
		t.Fatalf("unexpected match: %+v", m)
	}
	s = nativeTestScan(t, &r, path)
	if s.RowsRead != 0 || s.Reset {
		t.Fatalf("replay read again: %+v", s)
	}
	if got := r.MatchBatch([]nativeTimingUsage{nativeTestUsage("response", "thread", "turn")})["response"]; got != m {
		t.Fatal("unchanged poll altered evidence")
	}
	// An unknown outcome can become successful even when its start is absent.
	nativeTestRow(t, db, nativeTestEpoch.Add(4*time.Second), "feedback_tags", nativeTestEnd("no-start"), "other-process")
	nativeTestScan(t, &r, path)
	got := r.MatchBatch([]nativeTimingUsage{nativeTestUsage("no-start", "other-thread", "other-turn")})["no-start"]
	if !got.Completed || !got.StartedAt.IsZero() || got.Reason != "missing_start" || got.Contradicted {
		t.Fatalf("completion lost: %+v", got)
	}
}

func TestNativeTimingConservativeRejections(t *testing.T) {
	tests := []struct {
		name                    string
		start, end              time.Duration
		extra                   func(*testing.T, *sql.DB)
		usage                   nativeTimingUsage
		reason                  string
		contradicted, completed bool
	}{
		{"retry", time.Second, 3 * time.Second, func(t *testing.T, d *sql.DB) {
			nativeTestRow(t, d, nativeTestEpoch.Add(2*time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
		}, nativeTestUsage("response", "thread", "turn"), "ambiguous_sampling_starts", true, true},
		{"unmapped-completion", time.Second, 3 * time.Second, func(t *testing.T, d *sql.DB) {
			nativeTestRow(t, d, nativeTestEpoch.Add(2*time.Second), "feedback_tags", nativeTestEnd("unknown"), "process")
		}, nativeTestUsage("response", "thread", "turn"), "ambiguous_unmapped_completion", false, true},
		{"parallel-turn", time.Second, 3 * time.Second, func(t *testing.T, d *sql.DB) {
			nativeTestRow(t, d, nativeTestEpoch.Add(2*time.Second), "feedback_tags", nativeTestStart("thread", "other-turn"), "process")
		}, nativeTestUsage("response", "thread", "turn"), "ambiguous_parallel_sampling", true, true},
		{"duplicate-completion", time.Second, 3 * time.Second, func(t *testing.T, d *sql.DB) {
			nativeTestRow(t, d, nativeTestEpoch.Add(4*time.Second), "feedback_tags", nativeTestEnd("response"), "process")
		}, nativeTestUsage("response", "thread", "turn"), "ambiguous_completion", true, true},
		{"too-short", time.Second, time.Second + 99*time.Millisecond, nil, nativeTestUsage("response", "thread", "turn"), "invalid_duration", true, true},
		{"too-long", time.Second, time.Second + nativeTimingMax + time.Millisecond, nil, nativeTestUsage("response", "thread", "turn"), "invalid_duration", true, true},
		{"clock-inversion", 3 * time.Second, time.Second, nil, nativeTestUsage("response", "thread", "turn"), "missing_start", false, true},
		{"wrong-thread", time.Second, 3 * time.Second, nil, nativeTestUsage("response", "wrong-thread", "turn"), "missing_start", false, true},
		{"missing-turn", time.Second, 3 * time.Second, nil, nativeTestUsage("response", "thread", ""), "missing_usage_scope", false, true},
		{"unknown-version", time.Second, 3 * time.Second, nil, nativeTimingUsage{"response", "thread", "turn", "0.162.0"}, "unsupported_codex_version", false, false},
		{"oversized-barrier", time.Second, 3 * time.Second, func(t *testing.T, d *sql.DB) {
			nativeTestRow(t, d, nativeTestEpoch.Add(2*time.Second), "feedback_tags", strings.Repeat("x", nativeTimingBodyLimit+1), "process")
		}, nativeTestUsage("response", "thread", "turn"), "ambiguous_unmapped_completion", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, path := nativeTestDB(t)
			nativeTestBaseline(t, db)
			nativeTestRow(t, db, nativeTestEpoch.Add(tc.start), "feedback_tags", nativeTestStart("thread", "turn"), "process")
			nativeTestRow(t, db, nativeTestEpoch.Add(tc.end), "feedback_tags", nativeTestEnd("response"), "process")
			if tc.extra != nil {
				tc.extra(t, db)
			}
			var r nativeTimingReader
			nativeTestScan(t, &r, path)
			got := r.MatchBatch([]nativeTimingUsage{tc.usage})["response"]
			if got.Reason != tc.reason || got.Completed != tc.completed || got.Contradicted != tc.contradicted || !got.StartedAt.IsZero() {
				t.Fatalf("unexpected result: %+v", got)
			}
		})
	}
}

func TestNativeTimingOutOfOrderLateContradictionAndPartialRetention(t *testing.T) {
	db, path := nativeTestDB(t)
	nativeTestBaseline(t, db)
	nativeTestRow(t, db, nativeTestEpoch.Add(3*time.Second), "feedback_tags", nativeTestEnd("response"), "process")
	var r nativeTimingReader
	nativeTestScan(t, &r, path)
	u := []nativeTimingUsage{nativeTestUsage("response", "thread", "turn")}
	if got := r.MatchBatch(u)["response"]; got.Reason != "missing_start" {
		t.Fatal(got.Reason)
	}
	// SQLite IDs are ingestion order, not producer event order.
	nativeTestRow(t, db, nativeTestEpoch.Add(time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
	nativeTestScan(t, &r, path)
	good := r.MatchBatch(u)["response"]
	if good.StartedAt.IsZero() {
		t.Fatal("late start not joined")
	}
	nativeTestRow(t, db, nativeTestEpoch.Add(2*time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
	nativeTestScan(t, &r, path)
	bad := r.MatchBatch(u)["response"]
	if !bad.Contradicted || bad.EvidenceRevision <= good.EvidenceRevision || !bad.StartedAt.IsZero() {
		t.Fatalf("late conflict did not revoke: %+v", bad)
	}
	partial, partialPath := nativeTestDB(t)
	nativeTestRow(t, partial, nativeTestEpoch.Add(time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
	nativeTestRow(t, partial, nativeTestEpoch.Add(3*time.Second), "feedback_tags", nativeTestEnd("response"), "process")
	var p nativeTimingReader
	nativeTestScan(t, &p, partialPath)
	if got := p.MatchBatch(u)["response"]; got.Reason != "partial_retention" || got.Contradicted {
		t.Fatalf("partial retention accepted: %+v", got)
	}
}

func TestNativeTimingSequentialAndProfileIsolation(t *testing.T) {
	db, path := nativeTestDB(t)
	nativeTestBaseline(t, db)
	for i := 0; i < 2; i++ {
		nativeTestRow(t, db, nativeTestEpoch.Add(time.Duration(1+i*3)*time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
		nativeTestRow(t, db, nativeTestEpoch.Add(time.Duration(3+i*3)*time.Second), "feedback_tags", nativeTestEnd(fmt.Sprintf("response-%d", i)), "process")
	}
	// Another process's missing usage cannot corrupt this process's inference.
	nativeTestRow(t, db, nativeTestEpoch.Add(2*time.Second), "feedback_tags", nativeTestEnd("unmapped"), "different-process")
	var r nativeTimingReader
	nativeTestScan(t, &r, path)
	u := []nativeTimingUsage{nativeTestUsage("response-0", "thread", "turn"), nativeTestUsage("response-1", "thread", "turn")}
	for _, m := range r.MatchBatch(u) {
		if m.StartedAt.IsZero() {
			t.Fatalf("sequential match failed: %+v", m)
		}
	}
	other, otherPath := nativeTestDB(t)
	nativeTestBaseline(t, other)
	nativeTestRow(t, other, nativeTestEpoch.Add(3*time.Second), "feedback_tags", nativeTestEnd("response-0"), "process")
	stats := nativeTestScan(t, &r, otherPath)
	if !stats.Reset {
		t.Fatal("profile did not reset")
	}
	if got := r.MatchBatch(u)["response-0"]; !got.StartedAt.IsZero() || got.Reason != "missing_start" {
		t.Fatalf("profile leaked: %+v", got)
	}
}

func TestNativeTimingRotationAndInPlaceReplacement(t *testing.T) {
	db, path := nativeTestDB(t)
	nativeTestBaseline(t, db)
	nativeTestRow(t, db, nativeTestEpoch.Add(time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
	nativeTestRow(t, db, nativeTestEpoch.Add(3*time.Second), "feedback_tags", nativeTestEnd("response"), "process")
	var r nativeTimingReader
	nativeTestScan(t, &r, path)
	// Reused IDs in the same inode are detected with the last-row anchor.
	if _, err := db.Exec("DELETE FROM logs"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		nativeTestRow(t, db, nativeTestEpoch.Add(time.Duration(20+i)*time.Second), "codex", "unrelated", "new-process")
	}
	if s := nativeTestScan(t, &r, path); !s.Reset {
		t.Fatal("in-place replacement missed")
	}
	u := []nativeTimingUsage{nativeTestUsage("response", "thread", "turn")}
	if r.MatchBatch(u)["response"].Completed {
		t.Fatal("old completion survived reset")
	}
	db.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	newDB, newPath := nativeTestDB(t)
	nativeTestBaseline(t, newDB)
	newDB.Close()
	if err := os.Rename(newPath, path); err != nil {
		t.Fatal(err)
	}
	if s := nativeTestScan(t, &r, path); !s.Reset {
		t.Fatal("inode rotation missed")
	}
}

func TestNativeTimingReadOnlyMissingSchemaLockedAndCancellation(t *testing.T) {
	var r nativeTimingReader
	missing := filepath.Join(t.TempDir(), "absent.sqlite")
	stats, err := r.Scan(context.Background(), missing)
	if err == nil || stats.Reason != "missing_db" {
		t.Fatalf("missing diagnostic: %+v", stats)
	}
	if _, err = os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("reader created missing database")
	}
	db, path := nativeTestDB(t)
	nativeTestBaseline(t, db)
	if _, err = db.Exec("ALTER TABLE logs RENAME COLUMN ts_nanos TO incompatible"); err != nil {
		t.Fatal(err)
	}
	stats, err = r.Scan(context.Background(), path)
	if err == nil || stats.Reason != "unsupported_schema" {
		t.Fatalf("schema diagnostic: %+v", stats)
	}
	locked, lockedPath := nativeTestDB(t)
	nativeTestBaseline(t, locked)
	if _, err = locked.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	stats, err = r.Scan(context.Background(), lockedPath)
	if _, e := locked.Exec("ROLLBACK"); e != nil {
		t.Fatal(e)
	}
	if err == nil || stats.Reason != "locked" {
		t.Fatalf("locked diagnostic: %+v", stats)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats, err = r.Scan(ctx, lockedPath)
	if err == nil || stats.Reason != "read_timeout" {
		t.Fatalf("cancel diagnostic: %+v", stats)
	}
	before, err := os.ReadFile(lockedPath)
	if err != nil {
		t.Fatal(err)
	}
	nativeTestScan(t, &r, lockedPath)
	after, err := os.ReadFile(lockedPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("read-only scan changed database bytes")
	}
}

func TestNativeTimingWALAndPrivacy(t *testing.T) {
	db, path := nativeTestDB(t)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	nativeTestBaseline(t, db)
	secret := "PRIVATE_CONTENT_MUST_NEVER_BE_RETAINED"
	nativeTestRow(t, db, nativeTestEpoch.Add(time.Second), "feedback_tags", nativeTestStart("thread", "turn")+` note="`+secret+`"`, "process")
	nativeTestRow(t, db, nativeTestEpoch.Add(3*time.Second), "feedback_tags", nativeTestEnd("response"), "process")
	// Strings embedded inside quoted text cannot impersonate top-level fields.
	nativeTestRow(t, db, nativeTestEpoch.Add(2*time.Second), "feedback_tags", `note="last_model_response_id=\"fake\" try_run_sampling_request{thread_id=\"thread\" turn_id=\"turn\"}"`, "process")
	var r nativeTimingReader
	nativeTestScan(t, &r, path)
	u := []nativeTimingUsage{nativeTestUsage("response", "thread", "turn"), nativeTestUsage("fake", "thread", "turn")}
	m := r.MatchBatch(u)
	if m["response"].StartedAt.IsZero() || m["fake"].Completed {
		t.Fatalf("quoted content became evidence: %+v", m)
	}
	if strings.Contains(fmt.Sprintf("%+v", r.events), secret) {
		t.Fatal("raw log data retained")
	}
	// Conflicting scope fields are a barrier, never silently first-wins.
	fields, _ := nativeTimingFields(`thread_id="one" thread.id="two" turn_id="turn"`)
	if nativeTimingUnique(fields, "thread_id", "thread.id") != "" {
		t.Fatal("conflicting scope accepted")
	}
}

func TestNativeTimingBoundedIncrementalBackfill(t *testing.T) {
	db, path := nativeTestDB(t)
	nativeTestBaseline(t, db)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO logs(ts,ts_nanos,target,feedback_log_body,process_uuid)VALUES(?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < nativeTimingReadLimit; i++ {
		if _, err = stmt.Exec(nativeTestEpoch.Unix(), i+1, "codex", "not selected", "process"); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	nativeTestRow(t, db, nativeTestEpoch.Add(time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
	nativeTestRow(t, db, nativeTestEpoch.Add(3*time.Second), "feedback_tags", nativeTestEnd("response"), "process")
	var r nativeTimingReader
	s := nativeTestScan(t, &r, path)
	if s.RowsRead < 1 || s.RowsRead > nativeTimingReadLimit || !s.More {
		t.Fatalf("unbounded backfill: %+v", s)
	}
	read := s.RowsRead
	for pages := 1; s.More; pages++ {
		previous := s.Cursor
		s = nativeTestScan(t, &r, path)
		read += s.RowsRead
		if s.RowsRead < 1 || s.RowsRead > nativeTimingReadLimit || s.Cursor <= previous || pages > nativeTimingReadLimit+3 {
			t.Fatalf("incremental page made no bounded progress: %+v", s)
		}
	}
	if read != nativeTimingReadLimit+3 {
		t.Fatalf("pages skipped or repeated rows: %d", read)
	}
	if m := r.MatchBatch([]nativeTimingUsage{nativeTestUsage("response", "thread", "turn")})["response"]; m.StartedAt.IsZero() {
		t.Fatalf("backfilled timing unavailable: %+v", m)
	}
}

func TestNativeTimingBoundsAndConflictingUsage(t *testing.T) {
	for _, duration := range []time.Duration{nativeTimingMin, nativeTimingMax} {
		t.Run(duration.String(), func(t *testing.T) {
			db, path := nativeTestDB(t)
			nativeTestBaseline(t, db)
			nativeTestRow(t, db, nativeTestEpoch.Add(time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
			nativeTestRow(t, db, nativeTestEpoch.Add(time.Second+duration), "feedback_tags", nativeTestEnd("response"), "process")
			var r nativeTimingReader
			nativeTestScan(t, &r, path)
			u := nativeTestUsage("response", "thread", "turn")
			if m := r.MatchBatch([]nativeTimingUsage{u})["response"]; m.StartedAt.IsZero() {
				t.Fatalf("valid boundary rejected: %+v", m)
			}
			bad := u
			bad.ThreadID = "other"
			if m := r.MatchBatch([]nativeTimingUsage{u, bad})["response"]; !m.Contradicted || m.Reason != "ambiguous_usage_identity" {
				t.Fatalf("identity conflict accepted: %+v", m)
			}
		})
	}
}

func TestNativeTimingUnknownBarrierRecoversButObservedRetryDoesNot(t *testing.T) {
	db, path := nativeTestDB(t)
	nativeTestBaseline(t, db)
	nativeTestRow(t, db, nativeTestEpoch.Add(time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
	nativeTestRow(t, db, nativeTestEpoch.Add(2*time.Second), "feedback_tags", nativeTestEnd("other-response"), "process")
	nativeTestRow(t, db, nativeTestEpoch.Add(3*time.Second), "feedback_tags", nativeTestEnd("response"), "process")
	var r nativeTimingReader
	nativeTestScan(t, &r, path)
	u := []nativeTimingUsage{nativeTestUsage("response", "thread", "turn")}
	m := r.MatchBatch(u)["response"]
	if m.Reason != "ambiguous_unmapped_completion" || m.Contradicted || !m.StartedAt.IsZero() {
		t.Fatalf("unknown identity was not recoverable: %+v", m)
	}
	u = append(u, nativeTestUsage("other-response", "other-thread", "other-turn"))
	if m = r.MatchBatch(u)["response"]; m.StartedAt.IsZero() {
		t.Fatalf("resolved unrelated identity did not recover: %+v", m)
	}
	nativeTestRow(t, db, nativeTestEpoch.Add(2*time.Second), "feedback_tags", `auth_retry_after_unauthorized="true"`, "process")
	nativeTestScan(t, &r, path)
	if m = r.MatchBatch(u)["response"]; !m.Contradicted || m.Reason != "ambiguous_transport_retry" || !m.StartedAt.IsZero() {
		t.Fatalf("known retry was accepted: %+v", m)
	}
}

func TestNativeTimingMetadataCapacityAndUsageCapacity(t *testing.T) {
	db, path := nativeTestDB(t)
	nativeTestBaseline(t, db)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO logs(ts,ts_nanos,target,feedback_log_body,process_uuid)VALUES(?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < nativeTimingEventLimit+2; i++ {
		if _, err = stmt.Exec(nativeTestEpoch.Unix()+int64(i+1), 0, "feedback_tags", nativeTestEnd(fmt.Sprintf("response-%d", i)), "process"); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var r nativeTimingReader
	s := nativeTestScan(t, &r, path)
	pages := 1
	for s.More {
		if s.RowsRead > nativeTimingReadLimit || s.Reason != "backfill_pending" {
			t.Fatalf("unbounded or prematurely complete page: %+v", s)
		}
		// Already observed completions may prove success while the remaining
		// pages could still contain conflicting starts; never infer yet.
		if s.Cursor >= 2 {
			id := fmt.Sprintf("response-%d", s.Cursor-2)
			if m := r.MatchBatch([]nativeTimingUsage{nativeTestUsage(id, "thread", "turn")})[id]; !m.StartedAt.IsZero() || m.Reason != "backfill_pending" {
				t.Fatalf("partial scan inferred a duration: %+v", m)
			}
		}
		previous := s.Cursor
		s = nativeTestScan(t, &r, path)
		pages++
		if s.RowsRead < 1 || s.Cursor <= previous || pages > nativeTimingEventLimit+3 {
			t.Fatalf("bounded backfill made no progress: %+v, pages=%d", s, pages)
		}
	}
	if s.RowsRead > nativeTimingReadLimit || pages < 2 {
		t.Fatalf("capacity fixture did not exercise bounded paging: %+v, pages=%d", s, pages)
	}
	if s.EventsRetained != nativeTimingEventLimit {
		t.Fatalf("unbounded retained metadata: %+v", s)
	}
	if m := r.MatchBatch([]nativeTimingUsage{nativeTestUsage("response-0", "thread", "turn")})["response-0"]; m.Completed {
		t.Fatal("evicted completion retained")
	}
	u := make([]nativeTimingUsage, nativeTimingUsageLimit+1)
	for i := range u {
		u[i] = nativeTestUsage(fmt.Sprintf("usage-%d", i), "thread", "turn")
	}
	if m := r.MatchBatch(u)["usage-0"]; m.Reason != "usage_cache_capacity" {
		t.Fatal("usage batch was unbounded")
	}
}

func TestNativeTimingPrunedInteriorRangeCannotProveUniqueStart(t *testing.T) {
	db, path := nativeTestDB(t)
	nativeTestBaseline(t, db)
	nativeTestRow(t, db, nativeTestEpoch.Add(time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
	nativeTestRow(t, db, nativeTestEpoch.Add(2*time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
	nativeTestRow(t, db, nativeTestEpoch.Add(3*time.Second), "feedback_tags", nativeTestEnd("response"), "process")
	if _, err := db.Exec("DELETE FROM logs WHERE id=3"); err != nil {
		t.Fatal(err)
	}
	var r nativeTimingReader
	nativeTestScan(t, &r, path)
	m := r.MatchBatch([]nativeTimingUsage{nativeTestUsage("response", "thread", "turn")})["response"]
	if !m.Completed || !m.StartedAt.IsZero() || m.Reason != "partial_retention" || m.Contradicted {
		t.Fatalf("pruned retry became unique: %+v", m)
	}
}

func TestNativeTimingSoftBudgetCommitsProgressWithoutSkippingNextRow(t *testing.T) {
	db, path := nativeTestDB(t)
	nativeTestBaseline(t, db)
	nativeTestRow(t, db, nativeTestEpoch.Add(time.Second), "feedback_tags", nativeTestStart("thread", "turn"), "process")
	nativeTestRow(t, db, nativeTestEpoch.Add(3*time.Second), "feedback_tags", nativeTestEnd("response"), "process")
	r := nativeTimingReader{pageBudget: time.Nanosecond}
	usage := []nativeTimingUsage{nativeTestUsage("response", "thread", "turn")}
	for page := int64(1); page <= 3; page++ {
		s := nativeTestScan(t, &r, path)
		if s.RowsRead != 1 || s.Cursor != page || s.More != (page < 3) {
			t.Fatalf("soft budget lost committed progress: %+v", s)
		}
		m := r.MatchBatch(usage)["response"]
		if page < 3 && !m.StartedAt.IsZero() {
			t.Fatal("partial snapshot supplied timing")
		}
		if page == 3 && (m.StartedAt.IsZero() || m.CompletedAt.Sub(m.StartedAt) != 2*time.Second) {
			t.Fatalf("advanced unprocessed row was skipped: %+v", m)
		}
	}
	if s := nativeTestScan(t, &r, path); s.RowsRead != 0 || s.More || s.Cursor != 3 {
		t.Fatalf("caught-up reader replayed rows: %+v", s)
	}
}
