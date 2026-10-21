# Mutable OpenCode v2 message usage

This patch is limited to stable message IDs from the `opencode` source,
`sqlite` channel, `opencode_sqlite_v2` schema, and `message_usage` event type.
Other providers, OpenCode v1 rows, and tool events retain their existing merge
policy. It uses existing tables and columns; no migration is required.

OpenCode can first persist partial usage and later revise the same message. The
previous equal-priority merge retained any existing non-null value, including
zero, and therefore ignored final usage. A hook could also overwrite SQLite
usage repeatedly because the canonical row retained its original raw source.

For v2 messages the current SQLite snapshot is authoritative. Incoming non-null
usage and metadata replace the previous observation, including downward
corrections; nil usage preserves previously known fields. Stable message/event
identity is retained, so revisions do not count as additional requests. When a
SQLite row follows a hook, the existing raw provenance is atomically promoted to
SQLite with the current schema, payload, and digest. Later hook or v1 duplicates
cannot overwrite that authoritative message.

`db.time_updated` orders snapshots. A lower revision is ignored, including an
old spool retry after a restart. Equal timestamps use the latest observation;
they are not an independent total ordering for changes within the same clock
tick. This patch does not assume token counters only increase.

The source revision survives raw-payload retention as the minimal JSON object
`{"db":{"time_updated":123}}`. All other raw fields are removed on the normal
retention schedule. `source_payload_hash` intentionally remains the digest of the
original observation, **not** the compact marker. This prevents an identical
replay from restoring expired raw data. Already compact markers and identical
replays perform no database writes.

## Publishing a real revision

Updating an event is insufficient on its own: the original change-log trigger
coalesces duplicate update rows, and the usage-view cache fingerprints only row
count and maximum row ID. Both can hide later values for the same event.

For an actual v2 revision, the canonical fields, raw provenance, and one freshly
sequenced change-log row are updated in the same transaction. The existing
incremental reader is reused; a narrow cache probe detects pending v2 message
updates for its provider/account/window filter. Metadata-only changes also
invalidate the relevant view. Other provider update policies are unchanged.

`IngestResult.updated` is an optional JSON boolean, omitted when false. It is set
only for a changed v2 duplicate, independently of `deduped`, which remains true.
The direct collector and spool paths count these updates and invalidate daemon
read-model caches. Unchanged or stale duplicates do neither. Existing consumers
can continue decoding the old fields; an absent `updated` means false.

## Tests and limitations

Synthetic regressions cover partial zero to final usage, downward correction,
nil versus zero, hook-first and SQLite-first order, delayed hooks and spool
retries, source provenance, unchanged replay with zero writes, payload compaction
and restart, repeated revision delivery to a cached view, metadata-only updates,
transaction rollback, unchanged non-v2 policies, daemon/spool refresh, and JSON
backward compatibility.

Run `go test -race ./internal/telemetry ./internal/daemon` with CGO enabled.
No paid requests, OAuth credentials, private transcripts, or operational
databases are used by these tests. The separate OpenCode SQLite parser patch is
needed to emit the v2 schema marker; this patch alone does not add schema support.

No schema restoration is needed for a binary rollback. Older binaries can read
the stored rows but may again ignore later message revisions and apply their old
hook precedence. Raw payloads already compacted by retention cannot be restored
by rolling back a binary.
