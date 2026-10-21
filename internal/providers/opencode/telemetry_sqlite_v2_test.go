package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"

	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

func newV2Fixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source ?# readonly.db")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE session_v2(id TEXT PRIMARY KEY, directory TEXT, parent_id TEXT, cost REAL, tokens_input INTEGER)`,
		`CREATE TABLE session_message(id TEXT PRIMARY KEY, session_id TEXT, type TEXT, seq INTEGER, time_created INTEGER, time_updated INTEGER, data TEXT)`,
		`INSERT INTO session_v2 VALUES ('parent', '/synthetic/project', NULL, 999, 999999), ('child', '/synthetic/project', 'parent', 999, 999999)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return db, path
}

func putV2Message(t *testing.T, db *sql.DB, id, session, role, data string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO session_message VALUES (?, ?, ?, 1, 1780000000000, 1780000005000, ?)
		ON CONFLICT(id) DO UPDATE SET data=excluded.data, time_updated=time_updated+1`, id, session, role, data)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteV2UsageAndTools(t *testing.T) {
	db, path := newV2Fixture(t)
	putV2Message(t, db, "msg_parent", "parent", "assistant", `{
	 "model":{"id":"model-one","providerID":"provider-one"},"agent":"builder",
	 "tokens":{"input":100,"output":20,"reasoning":7,"cache":{"read":40,"write":3}},"cost":0.5,
	 "time":{"created":1780000000000,"completed":1780000005000},"finish":"stop",
	 "content":[{"type":"text","text":"PRIVATE_SENTINEL"},
	 {"type":"tool","id":"call_done","name":"read","state":{"status":"completed","input":{"secret":"PRIVATE_SENTINEL"},"content":[{"text":"PRIVATE_SENTINEL"}]},"time":{"completed":1780000004000}},
	 {"type":"tool","id":"call_running","name":"read","state":{"status":"running"}}]}`)
	putV2Message(t, db, "msg_child", "child", "assistant", `{"model":{"id":"model-two","providerID":"provider-two"},"tokens":{"input":2},"time":{"completed":1780000005000}}`)
	putV2Message(t, db, "msg_user", "parent", "user", `{"text":"PRIVATE_SENTINEL","tokens":{"input":999999}}`)
	putV2Message(t, db, "msg_streaming", "parent", "assistant", `{"content":[],"time":{"created":1780000000000}}`)
	events, err := CollectTelemetryFromSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events=%d want 2 messages + 1 completed tool", len(events))
	}
	var total int64
	models := map[string]bool{}
	for _, e := range events {
		if e.TotalTokens != nil {
			total += *e.TotalTokens
		}
		models[e.ModelRaw] = true
		if e.WorkspaceID != "project" {
			t.Fatalf("workspace=%q", e.WorkspaceID)
		}
		if e.EventType == shared.TelemetryEventTypeToolUsage && e.ToolCallID != "call_done" {
			t.Fatalf("tool=%q", e.ToolCallID)
		}
		b, _ := json.Marshal(e.Payload)
		if strings.Contains(string(b), "PRIVATE_SENTINEL") {
			t.Fatal("private source text leaked to telemetry payload")
		}
	}
	if total != 172 {
		t.Fatalf("total=%d; session aggregates or token subsets were double counted", total)
	}
	if !models["model-one"] || !models["model-two"] {
		t.Fatalf("models=%v", models)
	}
}

func TestSQLiteV2UnknownVersusZero(t *testing.T) {
	db, path := newV2Fixture(t)
	putV2Message(t, db, "msg_none", "parent", "assistant", `{"time":{"completed":1780000005000}}`)
	putV2Message(t, db, "msg_zero", "parent", "assistant", `{"tokens":{"input":0,"output":0},"cost":0,"error":null}`)
	events, err := CollectTelemetryFromSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
	e := events[0]
	if e.Status != shared.TelemetryStatusOK || e.MessageID != "msg_zero" || e.TotalTokens == nil || *e.TotalTokens != 0 || e.ReasoningTokens != nil {
		t.Fatal("missing and zero usage conflated")
	}
}

func TestSQLiteV2RecollectUpdatedRowsAndRestart(t *testing.T) {
	db, path := newV2Fixture(t)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	opts := shared.TelemetryCollectOptions{Paths: map[string]string{"db_path": path, "account_id": "personal-synthetic"}, PathLists: map[string][]string{"events_dirs": {t.TempDir()}}}
	p := New()
	putV2Message(t, db, "msg_partial", "parent", "assistant", `{"tokens":{"input":5,"output":0}}`)
	one, err := p.Collect(context.Background(), opts)
	if err != nil || len(one) != 1 {
		t.Fatalf("first %d %v", len(one), err)
	}
	again, err := p.Collect(context.Background(), opts)
	if err != nil || len(again) != 0 {
		t.Fatalf("unchanged replay %d %v", len(again), err)
	}
	putV2Message(t, db, "msg_partial", "parent", "assistant", `{"tokens":{"input":5,"output":9},"time":{"completed":1780000005000}}`)
	final, err := p.Collect(context.Background(), opts)
	if err != nil || len(final) != 1 || *final[0].TotalTokens != 14 {
		t.Fatalf("updated row not collected: %d %v", len(final), err)
	}
	if one[0].MessageID != final[0].MessageID || final[0].AccountID != "personal-synthetic" {
		t.Fatal("identity/account drift")
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	restarted, err := New().Collect(context.Background(), opts)
	if err != nil || len(restarted) != 1 || *restarted[0].TotalTokens != 14 {
		t.Fatalf("restart %d %v", len(restarted), err)
	}
}

func TestSQLiteSchemaAndPayloadErrors(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"malformed", `{`}, {"null", `null`}, {"array", `[]`},
		{"invalid tokens", `{"tokens":"unknown"}`}, {"negative", `{"tokens":{"input":-1}}`},
		{"unsafe integer", `{"tokens":{"input":9007199254740992}}`},
		{"overflow", `{"tokens":{"input":9000000000000000000,"output":9000000000000000000}}`},
		{"fraction", `{"tokens":{"input":1.5}}`}, {"invalid cache", `{"tokens":{"cache":[]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, path := newV2Fixture(t)
			putV2Message(t, db, "msg", "parent", "assistant", tc.payload)
			_, err := CollectTelemetryFromSQLite(context.Background(), path)
			if !errors.Is(err, ErrSQLitePayload) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		_, err := CollectTelemetryFromSQLite(context.Background(), filepath.Join(t.TempDir(), "absent.db"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("error=%v", err)
		}
	})
	for _, stmt := range []string{"", `CREATE TABLE session_message(id TEXT)`} {
		t.Run("unsupported", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unknown.db")
			db, _ := sql.Open("sqlite3", path)
			defer db.Close()
			if stmt != "" {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.Ping(); err != nil {
					t.Fatal(err)
				}
			}
			_, err := CollectTelemetryFromSQLite(context.Background(), path)
			if !errors.Is(err, ErrUnsupportedSQLiteSchema) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	t.Run("supported empty", func(t *testing.T) {
		_, path := newV2Fixture(t)
		events, err := CollectTelemetryFromSQLite(context.Background(), path)
		if err != nil || len(events) != 0 {
			t.Fatalf("events=%d err=%v", len(events), err)
		}
	})
}

func TestSQLiteV2BusyIsNotCorruption(t *testing.T) {
	db, path := newV2Fixture(t)
	if _, err := db.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	_, err := CollectTelemetryFromSQLite(context.Background(), path)
	var se sqlite3.Error
	if !errors.As(err, &se) || (se.Code != sqlite3.ErrBusy && se.Code != sqlite3.ErrLocked) {
		t.Fatalf("expected busy: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("source moved", err)
	}
	if matches, _ := filepath.Glob(path + ".corrupt.*"); len(matches) != 0 {
		t.Fatal("source quarantined")
	}
	if _, err := db.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectTelemetryFromSQLite(context.Background(), path); err != nil {
		t.Fatal("retry", err)
	}
}

func TestSQLiteV2PermissionAndFilePreservation(t *testing.T) {
	db, path := newV2Fixture(t)
	putV2Message(t, db, "msg", "parent", "assistant", `{"tokens":{"input":1}}`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CollectTelemetryFromSQLite(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("read mutated source database")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })
	if _, err := CollectTelemetryFromSQLite(context.Background(), path); err == nil {
		t.Fatal("unreadable source reported success")
	}
}

func TestSQLiteV2MissingConfiguredAndOptionalSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := New()
	opts := p.DefaultCollectOptions()
	if _, err := p.Collect(context.Background(), opts); err != nil {
		t.Fatalf("absent optional source: %v", err)
	}
	opts.Paths["account_id"] = "personal"
	if _, err := p.Collect(context.Background(), opts); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured missing source: %v", err)
	}
}
