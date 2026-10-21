package telemetry

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestWALCheckpoint_TruncatesWAL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := configureSQLiteConnection(db); err != nil {
		t.Fatalf("configure: %v", err)
	}

	// Create a table and insert data to generate WAL entries.
	if _, err := db.Exec(`CREATE TABLE test_data (id INTEGER PRIMARY KEY, payload TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := 0; i < 500; i++ {
		if _, err := db.Exec(`INSERT INTO test_data (payload) VALUES (?)`, "some data for wal growth"); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// WAL file should exist and have content.
	walPath := dbPath + "-wal"
	walBefore := WALSizeBytes(dbPath)
	if walBefore == 0 {
		// Check if the file exists at all.
		if _, err := os.Stat(walPath); err != nil {
			t.Skipf("WAL file not created (may be in-memory only): %v", err)
		}
	}

	// Checkpoint should succeed.
	if err := WALCheckpoint(context.Background(), db); err != nil {
		t.Fatalf("WALCheckpoint: %v", err)
	}

	// After TRUNCATE checkpoint, WAL should be 0 bytes.
	walAfter := WALSizeBytes(dbPath)
	if walAfter != 0 {
		t.Errorf("WAL size after checkpoint = %d, want 0", walAfter)
	}
}

func TestConfigureSQLiteReadOnlyConnectionKeepsTempWorkInMemory(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := configureSQLiteReadOnlyConnection(db); err != nil {
		t.Fatalf("configure read-only connection: %v", err)
	}

	var tempStore int
	if err := db.QueryRow(`PRAGMA temp_store`).Scan(&tempStore); err != nil {
		t.Fatalf("query temp_store: %v", err)
	}
	if tempStore != 2 {
		t.Fatalf("temp_store = %d, want 2 (MEMORY)", tempStore)
	}
}

func TestWALSizeBytes_NonExistentFile(t *testing.T) {
	size := WALSizeBytes("/nonexistent/path/test.db")
	if size != 0 {
		t.Errorf("WALSizeBytes for nonexistent = %d, want 0", size)
	}
}

func TestWALCheckpoint_NilDB(t *testing.T) {
	if err := WALCheckpoint(context.Background(), nil); err != nil {
		t.Errorf("WALCheckpoint(nil) should not error, got: %v", err)
	}
}

func TestQuickIntegrityCheck_HealthyDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	if err := quickIntegrityCheck(db); err != nil {
		t.Errorf("healthy DB failed integrity check: %v", err)
	}
}

func TestQuickIntegrityCheck_NilDB(t *testing.T) {
	if err := quickIntegrityCheck(nil); err == nil || errors.Is(err, errIntegrityCheckFailed) {
		t.Errorf("nil DB should report unavailable check, not corruption: %v", err)
	}
}

func TestOpenStore_PreservesCorruptDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "telemetry.db")

	// Create a valid DB first.
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("initial OpenStore: %v", err)
	}
	store.Close()

	// Corrupt the DB by overwriting bytes in the middle of the file.
	f, err := os.OpenFile(dbPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	// Write garbage at offset 4096 (second page) to corrupt a B-tree.
	garbage := make([]byte, 4096)
	for i := range garbage {
		garbage[i] = 0xFF
	}
	if _, err := f.WriteAt(garbage, 4096); err != nil {
		f.Close()
		t.Fatalf("write corruption: %v", err)
	}
	f.Close()

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store2, err := OpenStore(dbPath)
	if store2 != nil {
		store2.Close()
		t.Fatal("damaged database was accepted")
	}
	if err == nil {
		t.Fatal("expected an integrity or SQLite corruption error")
	}
	after, err := os.ReadFile(dbPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("damaged database was changed: %v", err)
	}
	current, err := os.Stat(dbPath)
	if err != nil || !os.SameFile(info, current) {
		t.Fatalf("damaged database was replaced: %v", err)
	}
	entries, err := filepath.Glob(dbPath + ".corrupt.*")
	if err != nil || len(entries) != 0 {
		t.Fatalf("database must not be quarantined automatically: matches=%v err=%v", entries, err)
	}
}

func TestQuickIntegrityCheck_UnavailableIsNotCorruption(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := quickIntegrityCheck(db); err == nil || errors.Is(err, errIntegrityCheckFailed) {
		t.Errorf("closed handle should report unavailable check, not corruption: %v", err)
	}
}
