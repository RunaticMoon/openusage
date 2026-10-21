package telemetry

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneOldEventsBatchedRemovesBacklog(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Use the same WAL + single-connection setup the production store uses.
	// Without it the bulk insert below runs as thousands of FULL-synced
	// auto-commit transactions, which on Windows is slow enough to blow the
	// package test timeout.
	if err := configureSQLiteConnection(db); err != nil {
		t.Fatalf("configure: %v", err)
	}

	store := NewStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Insert a backlog larger than one batch so the loop must iterate, split
	// across "old" (beyond retention) and "recent" (within retention).
	const oldCount = pruneEventsBatch + 1500
	const recentCount = 50
	// Batch every insert into a single transaction. The prune logic under test
	// is unaffected by how rows arrive, and one commit instead of ~2n keeps the
	// test fast and deadlock-free across platforms.
	insert := func(n int, occurredAt time.Time) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("%s-%d", occurredAt.Format("20060102"), i)
			ts := occurredAt.Format(time.RFC3339Nano)
			if _, err := tx.Exec(`INSERT INTO usage_raw_events
				(raw_event_id, ingested_at, source_system, source_channel, source_schema_version, source_payload, source_payload_hash)
				VALUES (?, ?, 'test', 'api', 'v1', '{}', ?)`, id, ts, id); err != nil {
				t.Fatalf("insert raw: %v", err)
			}
			if _, err := tx.Exec(`INSERT INTO usage_events
				(event_id, occurred_at, provider_id, account_id, agent_name, event_type, status, dedup_key, raw_event_id, normalization_version)
				VALUES (?, ?, 'p', 'a', 'test', 'tool_usage', 'ok', ?, ?, 'v1')`, id, ts, id, id); err != nil {
				t.Fatalf("insert event: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit tx: %v", err)
		}
	}
	insert(oldCount, time.Now().Add(-90*24*time.Hour))
	insert(recentCount, time.Now().Add(-1*time.Hour))

	deleted, complete, err := store.PruneOldEvents(context.Background(), 30, "9999-12-31")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !complete {
		t.Errorf("expected backlog to be fully drained")
	}
	if deleted != oldCount {
		t.Errorf("deleted = %d, want %d", deleted, oldCount)
	}

	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != recentCount {
		t.Errorf("remaining = %d, want %d (recent events must survive)", remaining, recentCount)
	}
}

func TestPruneOldEventsCancelledContextReturnsProgress(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	store := NewStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	// Should return cleanly (0, not-complete, nil) rather than erroring on a dead context.
	deleted, complete, err := store.PruneOldEvents(ctx, 30, "9999-12-31")
	if err != nil {
		t.Errorf("cancelled prune should not error, got %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 on pre-cancelled context", deleted)
	}
	if complete {
		t.Errorf("complete should be false when context is cancelled before draining")
	}
}
