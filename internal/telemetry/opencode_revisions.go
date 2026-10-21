package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func isOpenCodeV2Message(system SourceSystem, channel SourceChannel, schema string, eventType EventType, messageID string) bool {
	return system == "opencode" && channel == SourceChannelSQLite && schema == "opencode_sqlite_v2" && eventType == EventTypeMessageUsage && messageID != ""
}

// The source's monotonic revision survives payload compaction. Observation time
// cannot order queued retries: an older snapshot may be ingested much later.
func openCodeRevision(payload []byte) int64 {
	var record struct {
		DB struct {
			Updated json.Number `json:"time_updated"`
		} `json:"db"`
	}
	if json.Unmarshal(payload, &record) != nil {
		return 0
	}
	revision, _ := record.DB.Updated.Int64()
	return revision
}

func hasPendingAuthoritativeRevision(ctx context.Context, db *sql.DB, filter usageFilter, after int64) (bool, error) {
	where, args := usageWhereClause("e", filter)
	args = append([]any{after}, args...)
	var pending bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM usage_event_changes c
		JOIN usage_events e ON e.event_id = c.event_id
		JOIN usage_raw_events r ON r.raw_event_id = e.raw_event_id
		WHERE c.seq > ? AND c.operation = 'update'
		AND ((r.source_system = 'opencode' AND r.source_channel = 'sqlite'
		AND r.source_schema_version = 'opencode_sqlite_v2') OR
		(r.source_system = 'dsh' AND r.source_channel = 'jsonl' AND r.source_schema_version = 'dsh_jsonl_v4'))
		AND e.event_type = 'message_usage' AND `+where+`)`, args...).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("probe authoritative source revisions: %w", err)
	}
	return pending, nil
}
