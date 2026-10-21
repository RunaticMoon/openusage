# Operating Codex native-log timing

Codex request speed can use its own JSONL usage records and native
`logs_2.sqlite`. The [adapter design](../CODEX_NATIVE_TIMING_DESIGN.md) explains
the verified 0.163.0 producer contract, inferred boundaries and exclusions.
There is no dependency on usage-limit, Paseo, a plugin, an OTLP collector or
another service. Existing Codex logging is read as-is; this feature does not
enable additional logging or make model requests.

This is an extension of the existing optional performance feature. The normal
usage ledger, account configuration, authentication, export target, Hub listener
and latest-snapshot behavior retain their existing roles. No new schema is
introduced by this adapter. The earlier performance feature's additive tables
remain governed by [performance-operations.md](performance-operations.md).

## Confirm the profile and source paths

Identify the running Collector's actual settings file and configured Codex
account before preparing a change. Preserve its account ID, binary, credentials,
retention, export configuration and other `provider_paths` entries.

An explicit timing source uses this **account fragment**, not a replacement
settings file:

```json
{
  "provider_paths": {
    "sessions_dir": "/<approved-personal-codex-profile>/sessions",
    "codex_logs_db": "/<approved-personal-codex-profile>/logs_2.sqlite"
  }
}
```

Use verified absolute paths. Without `codex_logs_db`, automatic resolution is
limited to the parent of a configured root whose basename is `sessions` or
`archived_sessions`. For example, `/profiles/personal/sessions` may use
`/profiles/personal/logs_2.sqlite`; `/profiles/personal/history-copy` requires an
explicit database path. There is no default-home fallback for a custom root.
An explicit path permits a recognized native schema at a different filename;
it does not authorize a different person's/profile's data.

The adapter does not add the other `sessions`/`archived_sessions` directory to
the configured scan. A historical audit that inspected both directories can
therefore have more candidates than the operational Collector. Do not expand
source scope just to make a rate appear.

Retain the existing opt-in setting or merge only the needed keys:

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

There is no separate Codex timing service, enable CLI or logging environment
variable to configure. The existing daemon reads its settings at startup;
apply a reviewed binary/settings change through the existing service. Do not
launch a second daemon or call an integration installer for this adapter.

## Prepare, validate and back up

Follow [operations.md](operations.md) for native, immutable-SHA artifacts,
private manifests and an explicitly reviewed deployment. Keep all preparatory
files outside Git and verify the artifact checksum, source SHA, actual service
executable and protected-file hashes. A source PR or successful build alone
does not change the operational machine.

For each host, preserve the current version link/binary, settings, service
definition, credentials/environment and existing hooks. Use the helper's online
SQLite backup for the local OpenUsage DB. If a private Codex source snapshot is
needed for testing, use SQLite's online backup API from a `mode=ro` source and
keep it on the same host in a directory with mode 0700; data files use mode 0600.
Treat such a snapshot as sensitive even though the adapter extracts only
allowlisted fields.

Never copy only the main file of a live WAL database, use `immutable=1` on the
live source, delete `-wal`/`-shm`, or repair Codex's DB. A source backup is for
recovery/evidence only; normal OpenUsage rollback never restores Codex logs.

The existing helper supports these dry runs; replace placeholders with the
reviewed private manifest and recovery path:

```sh
python3 scripts/fork/operate.py backup --manifest '/<private>/canary.json'
python3 scripts/fork/operate.py stage --manifest '/<private>/canary.json'
python3 scripts/fork/operate.py deploy --manifest '/<private>/canary.json'
```

Only the separately approved deployment uses `--execute --approval-ref`.
List only the existing Collector service and reviewed file replacements in the
manifest. An embedded Exporter is not a second service. A compatible deployed
performance-aware Hub does not need a change for this adapter.

Before a canary, run native CGO tests on the candidate SHA and an isolated
read-only source rehearsal. Verify ordinary accounting equality with performance
off/on, accepted/excluded timing counts, late evidence, repeated polling and
replay. Keep test HOME/config/state/socket/DB separate and export only to an
isolated test Hub. Do not copy real credentials into the test setup.

The optional `TestPerformanceRealCodexReadOnlyToHub` test requires both
`OPENUSAGE_PERFORMANCE_REAL_CODEX_ROOT` and
`OPENUSAGE_PERFORMANCE_REAL_CODEX_LOGS_DB`, pointing to explicitly approved
personal source paths. It requires at least one accepted inferred measurement
to prove the positive end-to-end path. A source with zero eligible records can
still collect ordinary usage correctly; record that scope and its exclusion
counts separately. Do not widen a production account to archives to satisfy a
test. A separately approved archived-source test is distinct from operational
active-source coverage. Full logs and source copies stay private on their host.

After the first approved host succeeds, verify the second host under the same
approved scope. Confirm the intended executable/hash, service stability,
unchanged protected files and canonical History, successful source collection,
performance coverage and actual receipt through the existing Hub API. Process
liveness alone is not timing collection success.

## Query and interpret the result

Use the fork executable actually used by the service. A separate Homebrew or
official binary can still be older. Verify its version and help before querying:

```sh
/<actual-fork-current>/openusage --version
/<actual-fork-current>/openusage telemetry performance --help
/<actual-fork-current>/openusage telemetry daemon status --details
/<actual-fork-current>/openusage telemetry performance \
  --source codex --account '<existing-account-id>' --json --request-limit 100
```

The JSON query contains usage metadata and native request IDs. Keep complete
output private; share only coverage counts, numeric summaries and status codes.
Use the existing supported `--db-path`, period and socket overrides when the
installation requires them. The query reads existing performance metadata; it
does not scan or enable native logs.

Inspect the `inferred_log_boundary` classification, eligible output count,
elapsed time, successful outcome and exclusion reasons. Direct counts must not
increase from this adapter. Explicit zero output with valid evidence is zero
speed; absent output/timing is unknown. A valid completed tag may prove success
while an ambiguous or missing start still prevents a rate.

An idle or newly started profile, rotated diagnostic logs, non-0.163.0 history,
missing/busy/unreadable DB or ambiguous retries can leave zero measured
requests. This is compatible with successful ordinary Codex usage collection.
Do not turn task, item, tool or HTTP handshake timers into a fallback rate.
Historical measurements also do not imply a current wall-clock rate.

The scan is bounded: at most 32,768 DB rows per pass, 16,384 retained native
metadata events and 8,192 pending usage identities per profile/account scope.
Larger native DBs may need more existing Collector cycles to catch up, and
older request identities may no longer be eligible for late enrichment. No
extra collector loop or archive-wide scan is started to remove these limits.

Useful exclusion codes distinguish different causes:

| Code | Meaning |
| --- | --- |
| `codex_native_logs_path_unconfigured` | This source root has no safe automatic DB path or explicit `codex_logs_db`. |
| `codex_missing_db`, `codex_locked`, `codex_permission_denied`, `codex_read_timeout`, `codex_unsupported_schema` | Optional native metadata could not be read. Ordinary JSONL collection remains independent. |
| `unsupported_codex_version` | The accepted 0.163.0 session/version-scope evidence is absent. |
| `backfill_pending`, `partial_retention` | The bounded scan or retained log interval cannot yet prove a complete, unique association. |
| `missing_completion`, `missing_start`, `missing_usage_scope` | Required native identity or boundary evidence is absent. A completed request can still have no measurable start. |
| `ambiguous_unmapped_completion` | Another completion has unknown usage scope; later native JSONL may resolve it. |
| `ambiguous_sampling_starts`, `ambiguous_completion`, `ambiguous_parallel_sampling`, `ambiguous_transport_retry`, `native_response_scope_conflict` | Positive conflicting evidence prevents the inferred rate. |
| `invalid_duration` | The inferred interval fails the 100 ms–15 min guard. |

These codes describe request evidence. They do not replace local Collector
health, durable ingestion checks or Hub receipt validation. Temporary absence of
logs does not erase an already accepted historical measurement. Positive
contradictions can revoke it; a later ordinary replay does not restore it.

Check the existing fork `hub-view` and authenticated `GET /v1/snapshots` for
actual delivery, reusing the protected token mechanism. Never print the token
or pass its literal value in a documented command. Keep tailnet restrictions
and the Hub's latest-snapshot retention unchanged.

## Disable or roll back

To disable all optional performance collection, change only
`telemetry.performance.enabled` to `false` and restart the existing Collector
through the approved operation. Ordinary accounting continues; retained
performance metadata is not deleted. This switch affects the other performance
adapters too.

For a regression specific to this adapter, roll back to the reviewed prior
fork binary and restore only deployment-replaced settings. Preserve the current
OpenUsage DB, optional performance tables, canonical History, refreshed auth,
Codex source DB/logs and unrelated files. This adapter requires no down-migration
or DB restoration. Retained inferred records may remain queryable after rollback;
that does not mean the old binary is still refreshing native timing.

```sh
python3 scripts/fork/operate.py rollback --manifest '/<private>/canary.json' \
  --backup '/<private-backups>/<pre-canary-backup>'
```

That command is a dry run. An approved recovery can add `--execute` and the
recorded `--approval-ref`. Do not use `--restore-telemetry-db` merely to remove
this adapter; it can discard newer active History. If service activation fails,
inspect actual systemd/launchd state before retrying so a second daemon is not
started. The helper has no automatic rollback: use the concrete backup and
approved binary/settings recovery while preserving data.
