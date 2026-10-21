package telemetry

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mattn/go-sqlite3"
)

// Use a separate process: the daemon and the hook fallback do not share a
// database/sql pool, and must coordinate through SQLite's existing WAL locks.
func TestOpenStorePreservesLiveWALAcrossProcesses(t *testing.T) {
	const childPathEnv = "OPENUSAGE_TEST_LIVE_WAL_PATH"
	if path := os.Getenv(childPathEnv); path != "" {
		store, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM safety_marker`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("child cannot read committed WAL row: count=%d err=%v", count, err)
		}
		if _, err := store.db.Exec(`INSERT INTO safety_marker VALUES (2)`); err != nil {
			t.Fatal(err)
		}
		return
	}

	path := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`PRAGMA wal_autocheckpoint=0; CREATE TABLE safety_marker (id INTEGER PRIMARY KEY); INSERT INTO safety_marker VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	before := storeFileIdentities(t, path)
	child := exec.Command(os.Args[0], "-test.run=^TestOpenStorePreservesLiveWALAcrossProcesses$")
	child.Env = append(os.Environ(), childPathEnv+"="+path)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("second process: %v\n%s", err, output)
	}
	assertStoreFileIdentities(t, before)
	if _, err := store.db.Exec(`INSERT INTO safety_marker VALUES (3)`); err != nil {
		t.Fatalf("original writer after second opener: %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM safety_marker`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("original writer lost shared history: count=%d err=%v", count, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM safety_marker`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("history after restart: count=%d err=%v", count, err)
	}
}

func TestOpenStoreBusyPreservesDatabaseAndCanRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`CREATE TABLE safety_marker (id INTEGER PRIMARY KEY); INSERT INTO safety_marker VALUES (1); PRAGMA locking_mode=EXCLUSIVE; BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer store.db.Exec(`ROLLBACK`)
	before := storeFileIdentities(t, path)
	second, err := OpenStore(path)
	if second != nil {
		second.Close()
		t.Fatal("second writer unexpectedly opened while schema initialization was locked")
	}
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) || (sqliteErr.Code != sqlite3.ErrBusy && sqliteErr.Code != sqlite3.ErrLocked) {
		t.Fatalf("want SQLite busy/locked error, got %v", err)
	}
	assertStoreFileIdentities(t, before)
	if _, err := store.db.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	second, err = OpenStore(path)
	if err != nil {
		t.Fatalf("retry after writer releases lock: %v", err)
	}
	defer second.Close()
	var count int
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM safety_marker`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("locked database lost history: count=%d err=%v", count, err)
	}
}

func TestOpenStoreRecoversCommittedWALAfterUncleanExit(t *testing.T) {
	const childPathEnv = "OPENUSAGE_TEST_UNCLEAN_WAL_PATH"
	if path := os.Getenv(childPathEnv); path != "" {
		store, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`PRAGMA wal_autocheckpoint=0; CREATE TABLE safety_marker (id INTEGER PRIMARY KEY); INSERT INTO safety_marker VALUES (1)`); err != nil {
			t.Fatal(err)
		}
		// Deliberately bypass Close and deferred cleanup after a committed write.
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "telemetry.db")
	child := exec.Command(os.Args[0], "-test.run=^TestOpenStoreRecoversCommittedWALAfterUncleanExit$")
	child.Env = append(os.Environ(), childPathEnv+"="+path)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("unclean writer: %v\n%s", err, output)
	}
	if WALSizeBytes(path) == 0 {
		t.Fatal("fixture did not leave a committed WAL to recover")
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("SQLite WAL recovery: %v", err)
	}
	defer store.Close()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM safety_marker`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("committed WAL row lost after unclean exit: count=%d err=%v", count, err)
	}
}

func TestOpenStorePreservesPreviousQuarantines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	files := make(map[string][]byte)
	for _, suffix := range []string{".corrupt.20260101", ".corrupt.20260101-wal", ".corrupt.20260101-shm", ".corrupt.20260102", ".corrupt.20260103"} {
		name := path + suffix
		files[name] = []byte("synthetic recovery evidence " + suffix)
		if err := os.WriteFile(name, files[name], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for name, want := range files {
		got, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("previous quarantine must be preserved: %s err=%v", filepath.Base(name), err)
		}
	}
}

func storeFileIdentities(t *testing.T, path string) map[string]os.FileInfo {
	t.Helper()
	files := make(map[string]os.FileInfo)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		files[path+suffix] = info
	}
	return files
}

func assertStoreFileIdentities(t *testing.T, before map[string]os.FileInfo) {
	t.Helper()
	for name, original := range before {
		current, err := os.Stat(name)
		if err != nil || !os.SameFile(original, current) {
			t.Errorf("live database/sidecar replaced: %s err=%v", filepath.Base(name), err)
		}
	}
}
