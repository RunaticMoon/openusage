# Operating optional log-based response speed

This feature is disabled by default. It extends the existing Collector, local
SQLite store, reports, snapshot Exporter and TUI. It does not install a new
service, change hooks, make model requests, add central history or change the
Hub's latest-snapshot retention. Source changes and synthetic tests do not
enable it on an operational machine.

Read the [measurement design](../PERFORMANCE_DESIGN.md) and
[source contracts and limitations](../PERFORMANCE_PROVIDERS.md) before enabling
it; [reference provenance](performance-references.md) records the audited
versions and licenses. The principal rate is output tokens divided by request
elapsed time. It may
include first-token waiting; it is neither decode speed nor TTFT. Direct and
inferred observations remain distinguishable, and missing timing is null/`—`,
not zero. Recent wall-clock rates are explicitly **time-allocation estimates**.

## Opt in without replacing existing settings

Merge only this block into the actual settings file, preserving other
`telemetry` keys, configured accounts, paths, authentication, export target,
service arguments and retention:

```json
{
  "telemetry": {
    "performance": {
      "enabled": true,
      "buffer_minutes": 30
    }
  }
}
```

The repository [example settings](../../configs/example_settings.json) keeps
`enabled: false`. The ring accepts 30 minutes (default) or 60 minutes, in
10-second UTC buckets. This is a bounded display window, not a new History
retention setting. Preserve the machine's existing `data.retention_days`.

The daemon reads this option at startup. Apply it only during the separately
reviewed deployment/restart of the existing Collector; do not start a second
daemon against its socket or DB. The feature adds no network probe, but the
daemon's existing provider polling remains governed by the existing account
configuration. It does not authorize new profiles or source directories.

Enabling the feature permits the existing source scans to backfill metadata
from available native logs. It does not retokenize canonical usage or infer
timers for historical entries that lack matching evidence. Initial scan time,
available log history, source schema and configured account boundaries limit
coverage. Check enabled versus disabled token/cost totals and repeated replay
on an isolated copy before a production canary.

## Backup and migration rehearsal

This feature introduces an **additive local migration**, unlike the earlier
compatibility-only fixes described in [operations.md](operations.md). At opt-in
daemon startup, one transaction creates `performance_meta`,
`performance_requests`, `performance_sources` and their indexes. Existing
accounting tables and their rows are retained. Migration failure aborts startup
with an error; it is not permission to quarantine, replace or recreate the DB.

Use the existing private manifest and SQLite online-backup procedure in
[operations.md](operations.md#backup-and-stage-helper). The checked-in helper
supports these commands; placeholders must be replaced with the independently
reviewed manifest, outside Git:

```sh
python3 scripts/fork/operate.py backup --manifest '/<private>/canary.json'
# Execute only the reviewed backup plan:
python3 scripts/fork/operate.py backup --manifest '/<private>/canary.json' --execute
```

The helper's first command is a dry run. Its real SQLite backup opens the source
read-only, includes committed WAL data and checks the resulting DB. Do not copy
only a live `telemetry.db`, delete WAL/SHM sidecars or use `immutable=1` to ignore
a live WAL. Keep the backup and manifest private; the metadata DB contains
account/model/session identifiers and local source provenance even though it
contains no added prompt or authentication fields.

Rehearse on a **working copy of the consistent backup**, preserving the backup
itself. Give the test process separate HOME/config/state, DB, socket and spool;
allowlist only synthetic or separately approved read-only source paths. Set
`export.target` empty or use an authenticated loopback test Hub. The supported
daemon command is `openusage telemetry daemon run`, with `--db-path`,
`--socket-path` and `--spool-dir` overrides. Those overrides alone do not isolate
configuration or discovery; a separate HOME/config/state and explicit test
accounts are also required. Do not copy real authentication into a test setup.

Verify migration success, unchanged canonical IDs/totals, repeated deduplication,
late/corrected metadata, retained History, and startup of the intended rollback
binary against another disposable copy. There is no standalone `migrate` or
`performance enable` CLI in this patch. The query below never performs migration.

## Confirmed CLI queries

First verify the installed binary and supported command surface:

```sh
openusage --version
openusage telemetry performance --help
openusage telemetry daemon status --details
```

For an installation using a non-default socket, add the verified
`--socket-path '/<actual-socket>'` to the daemon status command. Use the actual
service's DB path when it differs from the platform default. Do not assume
Linux and macOS use the same state-directory layout.

```sh
# Existing local metadata only; defaults to the last 24 hours.
openusage telemetry performance

# Exact source and configured account/profile, with bounded request details.
openusage telemetry performance --source opencode --account '<account-id>' \
  --json --request-limit 100

# Explicit half-open period [start,end), using a timezone offset.
openusage telemetry performance --source claude_code \
  --since '2026-10-21T00:00:00+09:00' --until '2026-10-22T00:00:00+09:00' \
  --windows=false

# Narrow a private copied DB by native model/session or billing vendor.
openusage telemetry performance --db-path '/<private-copy>/telemetry.db' \
  --source opencode --account '<account-id>' --model '<logged-model-id>' \
  --session '<native-session-id>' --billing-provider '<vendor-id>' \
  --buffer-minutes 60 --json

# Cached daemon snapshot; this command does not push it to the Hub.
openusage export --source daemon --format json --output '/<private>/snapshot.json'
```

The dedicated query opens an existing SQLite DB read-only, never scans source
logs, migrates a store or calls a model. `--since` is inclusive and `--until`
exclusive; RFC3339 timestamps are unambiguous, while date-only values use the
local timezone. `--source` is the execution tool (`claude_code`, `codex`,
`opencode`), distinct from `--billing-provider`. Filters use exact IDs. Request
details default to 100, can be 1–1,000, and do not limit aggregate statistics.
The underlying projection is capped at 100,000 observations; narrow the
source/account/period if the command reports that bound.

JSON includes coverage reasons, output basis, direct/inferred statistics,
histograms, UTC buckets and bounded request detail. Treat it as private usage
metadata. The default snapshot also has bounded metadata; local source paths
are never serialized. An explicit query can read retained performance metadata
after collection is disabled, so query success does not prove it is still
being refreshed.

Existing `daily`, `weekly`, `monthly`, `session` and `blocks` reports append this
local projection only when opted in. Their ordinary usage collection behavior
is unchanged. For example, `openusage daily --provider opencode --source daemon
--offline --json` uses the existing report command. Its explicit performance
period may be shorter than older token history. A project-filtered report does
not attach performance because request project attribution is not stored.

## Interpret diagnostics before claiming collection success

| Observation | Meaning / next check |
| --- | --- |
| `performance is not enabled in this database` | No performance migration exists in that DB. Verify the exact DB and opt-in daemon startup; the query cannot enable it. |
| Snapshot diagnostic `performance_status=disabled_or_not_migrated` | The optional read model found no initialized performance store. Existing usage/quota output remains available. |
| Snapshot diagnostic `performance_status=unavailable` | The optional query failed. Check local store diagnostics; do not replace the DB or present missing rates as zero. |
| Zero candidates or no recent source heartbeat | No identifiable request observations in the selected scope/period, or no successful observed scan. This is not proof of idle traffic or a universally supported source. |
| Candidates but unmeasured/unknown reasons | Inspect JSON coverage and request timing/outcome. Codex records without a verified request clock remain unmeasured; completed accounting alone does not prove success. |
| Direct OpenCode or inferred Claude measurements | The accepted native contract supplied output, success and timing evidence. Confirm account, source, model, period and coverage before comparison. |
| DSH, Antigravity, Ollama, Copilot or another unadapted source | Existing accounting may work while response speed is unsupported. Do not synthesize a duration from task/user/tool boundaries or quota refreshes. |
| Partial recent window | Observed allocation can be displayed with the partial-coverage label. It is not complete host throughput. |
| Stale/unknown/clock-ahead source | Historical measurements remain historical; the TUI suppresses the current wall rate. Compare source heartbeat and collection health. |

See [collector health](collector-health.md) for the existing local Unix-socket
`/healthz` fields. Process liveness, a successful source read, durable metadata
ingestion, measured timing and Hub delivery are separate checks. `DataAsOf`
means the last successful source scan, not request completion or transport lag;
`GeneratedAt` means aggregation time. A stale heartbeat is detected at
`max(120 seconds, 2 * export interval)`; invalid intervals above 24 hours use the
120-second fallback. Unknown/future clocks are not presented as live activity.

## Export, Hub and TUI compatibility

The Exporter enriches its private snapshot copy with the configured machine
identity and export interval. It does not change request IDs or a local source
heartbeat. The optional `performance` field uses the existing snapshot channel.

| Collector payload | Hub | Result |
| --- | --- | --- |
| Old payload without `performance` | Updated Hub | Existing usage works; no speed is invented. |
| New optional field | Original v0.26.1 Hub | Existing usage works; the old typed decoder discards the new field. |
| New optional field | Updated Hub | Performance is preserved in the latest snapshot alongside usage. |

The existing `openusage hub-view '<existing-hub-url>'` and Hub
`GET /v1/snapshots` remain the viewing interfaces. Keep the existing tailnet-only
listener and authentication. Reuse the protected environment/token mechanism;
do not put a real token in `--token`, shell history, a document or a test.
No Hub upgrade, listener, token or service change is performed by enabling this
Collector option. A separately reviewed Hub binary upgrade is needed to retain
the field through an original v0.26.1 Hub.

TUI details show a separate Performance card within the existing Usage section;
the existing Other Data tile section contains a compact rate/coverage summary.
If those sections are hidden by user settings, their performance content is
hidden too. Old snapshots can omit the card; unsupported values in an available
performance section show `—`. No quota gauge uses these rates. Default snapshots
remain per machine; no automatic cross-host live total or permanent Hub history
is created.

## Disable or roll back while preserving current History

To stop the feature, set only `telemetry.performance.enabled` to `false` and
restart the existing Collector through the approved operational procedure.
Ordinary collection/snapshots no longer read or write the optional feature.
Existing performance tables stay in place; disabling is not data deletion.
While enabled, request-metadata pruning follows the existing
`data.retention_days`. There is no separate permanent retention policy; an old
binary or a disabled feature does not actively prune these optional tables.

For a binary rollback, use the reviewed [operations rollback procedure](operations.md#rollback),
restore only the approved binary/settings/service replacements, and **keep the
current telemetry DB and its additive performance tables**. No down-migration,
table drop, DB recreation or old-backup restore is required for normal rollback.
Preserve newly collected canonical History and independently refreshed
authentication. Rehearse exact-binary compatibility on a copy first.

The helper's `--restore-telemetry-db` remains a separate recovery action that
can remove newer active History. Do not use it merely to disable performance.
Migration failure, locked DB or permission failure must be investigated without
deleting source data. Keep the fresh preservation backup and stop all known
writers before any separately approved DB recovery.
