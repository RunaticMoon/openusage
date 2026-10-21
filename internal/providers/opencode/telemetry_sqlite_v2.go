package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

// These errors distinguish an unsupported layout or malformed record from a
// supported, empty database. SQLite errors remain wrapped for errors.As.
type sqliteCollectionError struct{ code, message string }

func (e *sqliteCollectionError) Error() string               { return e.message }
func (e *sqliteCollectionError) CollectionErrorCode() string { return e.code }

var (
	ErrUnsupportedSQLiteSchema = &sqliteCollectionError{"unsupported_schema", "unsupported opencode SQLite schema"}
	ErrSQLitePayload           = &sqliteCollectionError{"parse_error", "invalid opencode SQLite usage payload"}
)

// OpenCode v2.0.32 and v2.1.1 store assistant steps in session_message, with
// their discriminator and identity in columns rather than inside data JSON.
// See anomalyco/opencode packages/core/src/session/sql.ts at those tags.
func identifySQLiteSchema(ctx context.Context, db *sql.DB) (string, error) {
	v2, err := sqliteTableExists(ctx, db, "session_message")
	if err != nil {
		return "", fmt.Errorf("inspect opencode schema: %w", err)
	}
	tables := map[string][]string{
		"message": {"id", "session_id", "time_created", "time_updated", "data"},
		"session": {"id", "directory"},
	}
	schema := "v1"
	if v2 {
		schema = "v2"
		tables = map[string][]string{
			"session_message": {"id", "session_id", "type", "time_created", "time_updated", "data"},
			"session_v2":      {"id", "directory"},
		}
	} else {
		part, err := sqliteTableExists(ctx, db, "part")
		if err != nil {
			return "", err
		}
		if part {
			tables["part"] = []string{"id", "message_id", "session_id", "time_created", "time_updated", "data"}
		}
	}
	for table, required := range tables {
		// Identifiers come exclusively from the fixed table map above.
		rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
		if err != nil {
			return "", fmt.Errorf("inspect opencode columns: %w", err)
		}
		columns := map[string]bool{}
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var def any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
				_ = rows.Close()
				return "", err
			}
			columns[name] = true
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return "", err
		}
		for _, name := range required {
			if !columns[name] {
				return "", fmt.Errorf("%w: missing %s.%s", ErrUnsupportedSQLiteSchema, table, name)
			}
		}
	}
	return schema, nil
}

func collectSQLiteV2Events(ctx context.Context, db *sql.DB, dbPath string, performanceEnabled ...bool) ([]shared.TelemetryEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.session_id, m.time_created, m.time_updated, m.data, COALESCE(s.directory, '')
		FROM session_message m
		LEFT JOIN session_v2 s ON s.id = m.session_id
		WHERE m.type = 'assistant'
		ORDER BY m.time_updated, m.id`)
	if err != nil {
		return nil, fmt.Errorf("query opencode v2 messages: %w", err)
	}
	defer rows.Close()
	var out []shared.TelemetryEvent
	for rows.Next() {
		var id, sessionID, raw, directory string
		var created, updated int64
		if err := rows.Scan(&id, &sessionID, &created, &updated, &raw, &directory); err != nil {
			return nil, fmt.Errorf("scan opencode v2 message: %w", err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload == nil {
			// Do not include record contents, prompts or tool arguments in errors.
			return nil, fmt.Errorf("%w: session_message.data must be a JSON object", ErrSQLitePayload)
		}
		if strings.TrimSpace(id) == "" || strings.TrimSpace(sessionID) == "" {
			return nil, fmt.Errorf("%w: missing stable message/session identity", ErrSQLitePayload)
		}
		if err := validateV2Usage(payload); err != nil {
			return nil, err
		}
		// Reuse v1's established token semantics: input excludes cache and output
		// excludes reasoning in OpenCode v2. Their five disjoint buckets are
		// summed once; session_v2's cumulative counters are never ingested.
		payload["role"] = "assistant"
		payload["modelID"] = shared.FirstPathString(payload, []string{"model", "id"})
		u := extractUsage(payload)
		completed := ptrInt64FromFloat(shared.FirstPathNumber(payload, []string{"time", "completed"}))
		createdAt := ptrInt64FromFloat(shared.FirstPathNumber(payload, []string{"time", "created"}))
		knownUsage := knownV2Usage(u)
		if knownUsage || (len(performanceEnabled) > 0 && performanceEnabled[0]) {
			ev := buildSQLiteMessageTableEvent(dbPath, id, sessionID, created, updated, completed, createdAt, directory, payload, partSummary{}, u)
			ev.PerformanceOnly = !knownUsage
			ev.SchemaVersion = "opencode_sqlite_v2"
			ev.Payload["source"].(map[string]any)["table"] = "session_message"
			if payload["error"] != nil {
				ev.Status = shared.TelemetryStatusError
			}
			if len(performanceEnabled) > 0 && performanceEnabled[0] {
				ev.Performance = sqliteV2Performance(ev, payload, dbPath, updated)
			}
			out = append(out, ev)
		}
		// Only structured tool identity/status is collected. Text, reasoning,
		// tool input, results, native replay data and snapshots are never copied.
		content, _ := payload["content"].([]any)
		for _, item := range content {
			tool, ok := item.(map[string]any)
			if !ok || tool["type"] != "tool" {
				continue
			}
			statusRaw := shared.FirstPathString(tool, []string{"state", "status"})
			status, include := mapToolStatus(statusRaw)
			if !include {
				continue
			}
			callID := shared.FirstPathString(tool, []string{"id"})
			if callID == "" {
				return nil, fmt.Errorf("%w: tool has no stable identity", ErrSQLitePayload)
			}
			part := map[string]any{
				"callID": callID, "tool": shared.FirstPathString(tool, []string{"name"}),
				"time": map[string]any{"end": ptrInt64FromFloat(shared.FirstPathNumber(tool, []string{"time", "completed"}))},
			}
			ev := buildSQLiteToolEvent(dbPath, callID, id, sessionID, created, updated, directory, part, payload, status, statusRaw)
			ev.SchemaVersion = "opencode_sqlite_v2"
			ev.Payload["source"].(map[string]any)["table"] = "session_message"
			out = append(out, ev)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read opencode v2 messages: %w", err)
	}
	return out, nil
}

func knownV2Usage(u usage) bool {
	// Explicit zero is a known usage value, unlike a missing streaming field.
	return u.InputTokens != nil || u.OutputTokens != nil || u.ReasoningTokens != nil ||
		u.CacheReadTokens != nil || u.CacheWriteTokens != nil || u.CostUSD != nil
}

func validateV2Usage(payload map[string]any) error {
	if raw, ok := payload["tokens"]; ok && raw != nil {
		tokens, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: tokens must be an object", ErrSQLitePayload)
		}
		for _, key := range []string{"input", "output", "reasoning"} {
			if err := validateV2Number(tokens, key, true); err != nil {
				return err
			}
		}
		if raw, ok := tokens["cache"]; ok && raw != nil {
			cache, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: cache must be an object", ErrSQLitePayload)
			}
			for _, key := range []string{"read", "write"} {
				if err := validateV2Number(cache, key, true); err != nil {
					return err
				}
			}
		}
	}
	u := extractUsage(payload)
	var total int64
	for _, value := range []*int64{u.InputTokens, u.OutputTokens, u.ReasoningTokens, u.CacheReadTokens, u.CacheWriteTokens} {
		if value != nil {
			if *value > math.MaxInt64-total {
				return fmt.Errorf("%w: token total overflow", ErrSQLitePayload)
			}
			total += *value
		}
	}
	return validateV2Number(payload, "cost", false)
}

func validateV2Number(object map[string]any, key string, integer bool) error {
	value, exists := object[key]
	if !exists || value == nil {
		return nil
	}
	n, ok := value.(float64)
	if !ok || n < 0 || (integer && (n > 1<<53-1 || n != float64(int64(n)))) {
		return fmt.Errorf("%w: invalid %s number", ErrSQLitePayload, key)
	}
	return nil
}
