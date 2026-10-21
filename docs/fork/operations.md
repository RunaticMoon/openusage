# Local operations, canary and rollback

Production is still running the official build until a separate deployment
approval names a machine, reviewed fork SHA, binary checksum and affected files.
PR approval/merge, a draft release or a successful test does not authorize a
service replacement. Never switch the OCI Collector, Mac Collector and Hub
together. This helper does not manage the Hub.

Keep the operational handover, absolute paths, tailnet address, unit/plist names,
tokens, account/profile selection and backup manifests **outside Git** in a
private directory. The examples below deliberately contain placeholders. Do not
paste real secrets into terminal arguments, PRs or CI output.

## Current layout and boundaries

Each machine has its own Collector and local telemetry SQLite/History. Export
is part of the daemon when `export.target` is configured; a deployment may also
have a separate wrapper/service. Enumerate the actual processes first instead
of inventing a second exporter. OCI runs the existing Hub in addition. Retain
its existing tailnet-only listener, authentication and network policy.

Before a canary, capture the actual binary resolved by the service, CLI version,
service definition/environment file locations, settings, hooks, SQLite path,
socket path, retention setting and account list. Select only personal profiles;
an alternate `CODEX_HOME` or company profile is not automatically approved for
the personal Hub. Do not dump environment variables or authentication files.

The upstream local detailed-event retention default is 90 days, with daily
rollup downsampling beyond the hot window. Preserve the actual configured
retention and local History; this work does not reset or migrate it. The Hub is
an in-memory latest-snapshot store and is not a History backup.

## Isolated validation

Use the reviewed checkout and `scripts/fork/build.sh` to create identifiable
artifacts. `metadata.json` names the fork SHA, verified included upstream release
and toolchain/platform; `checksums.txt` covers all staged files. Native CI and
local source tests are distinct from actual provider collection success.

Test Collector processes need separate configuration, state directory, SQLite,
socket and spool, with `export.target` empty or a loopback test Hub. They must not
discover operational credentials implicitly. Supply a synthetic/minimal explicit
account set and allowlist only test input paths. The operational OpenCode source
DB must remain read-only; use a consistent private snapshot when testing the
collector or replay. Never place that snapshot in this repository or a CI
artifact. Do not launch paid model sessions as part of regression tests.

## Backup and stage helper

`scripts/fork/operate.py` has `backup`, `stage`, `deploy` and `rollback` commands.
Every command is a dry run unless `--execute` is supplied. Deployment and
rollback also require `--approval-ref`, which records the actual human approval;
the flag itself does not grant permission. Dry runs print an action summary and
do not create directories, copy files, start processes or modify services.

For a real backup, regular files include the currently used binary, settings,
relevant credential/environment files, hooks and service definitions. Data is
never printed. Backup directories are mode 0700 and snapshot files/manifests
mode 0600. Manifests include private source paths, so they must remain outside
Git. The script rejects an existing backup root with broader permissions.

SQLite uses Python's SQLite **online backup API** from a `mode=ro` source, with
bounded waiting and `quick_check` on the result. It includes committed data in a
live WAL and does not use `immutable=1`, copy only the DB main file, rename the
source or delete sidecars. Failed/incomplete backups are not presented as
complete. This follows SQLite's [online backup API](https://sqlite.org/backup.html)
and Python's [Connection.backup](https://docs.python.org/3/library/sqlite3.html#sqlite3.Connection.backup).

A manifest has this shape; save the real version outside Git after reviewing
every path and hash. `services` lists the stop order (a separate Exporter first,
then Collector); startup reverses it. Do not list an embedded exporter as a
second service. Omit replacements that are outside the approved scope.

```json
{
  "platform": "linux_arm64",
  "approved_sha": "<40-character-reviewed-fork-SHA>",
  "binary_sha256": "<64-character-reviewed-binary-SHA256>",
  "artifact_dir": "/<private-staging>/<platform-artifact>",
  "install_root": "/<private-install>/openusage-fork",
  "backup_root": "/<private-backups>/openusage-fork",
  "files": [
    {"role": "binary", "path": "/<actual-original-binary>"},
    {"role": "config", "path": "/<actual-settings.json>"},
    {"role": "credentials", "path": "/<actual-private-service-environment-file>"},
    {"role": "hook", "path": "/<actual-installed-hook>"}
  ],
  "databases": [
    {"role": "telemetry", "path": "/<actual-local-telemetry.db>"},
    {"role": "opencode-source", "path": "/<optional-read-only-opencode.db>"}
  ],
  "replacements": [
    {
      "role": "service",
      "source": "/<reviewed-staged-user-unit>",
      "target": "/<actual-user-unit>",
      "sha256": "<64-character-staged-unit-SHA256>"
    }
  ],
  "services": [
    {
      "role": "collector",
      "manager": "systemd-user",
      "name": "<actual-collector>.service",
      "definition": "/<actual-user-unit>"
    }
  ]
}
```

For Mac use `platform: darwin_arm64` and a service entry such as
`{"role":"collector","manager":"launchd-user","name":"<actual.label>","plist":"/<actual-user.plist>"}`.
Use a foreground GUI user LaunchAgent domain, not a system daemon. The manifest
does not accept arbitrary shell service commands, and Hub service roles are
rejected. Required directories/files must already correspond to the verified
installation; the helper does not guess paths or generate secret-bearing units.

```sh
python3 scripts/fork/operate.py backup --manifest '/<private>/canary.json'
python3 scripts/fork/operate.py stage --manifest '/<private>/canary.json'
# Reversible preparation, after inspecting the dry-run:
python3 scripts/fork/operate.py backup --manifest '/<private>/canary.json' --execute
python3 scripts/fork/operate.py stage --manifest '/<private>/canary.json' --execute
```

Staging verifies metadata, every artifact checksum and the independently
reviewed binary checksum. It creates an immutable
`<install_root>/versions/<fork-SHA>/` directory and never switches `current`.
An existing version directory is reused only after validation. Configure the
reviewed service definition to use `<install_root>/current/openusage`; this
fork-owned path is separate from official/Homebrew installation paths. Do not
run official self-update or rely on `brew upgrade` to maintain that path.

## Approved canary

Review differences in settings and hooks separately from the binary. A changed
plugin template does not update an already installed OpenCode hook. Do not run a
blanket integration installer over user edits: stage the needed hook change,
review its diff, include its checksum as a `hook` replacement only when approved,
then preserve its previous contents in the backup.

```sh
python3 scripts/fork/operate.py deploy --manifest '/<private>/canary.json'
# Only after explicit approval of this machine, SHA, checksums and replacements:
python3 scripts/fork/operate.py deploy --manifest '/<private>/canary.json' \
  --execute --approval-ref '<approved-change-reference>'
```

The executed sequence is backup, verify/stage, stop only the listed services,
atomically replace reviewed files/current symlink, reload/start and check service
presence. The original binary is explicitly backed up and the old installation
is not removed. The command does not claim actual usage collection merely
because a process started. If it fails, the printed private backup location is
the recovery point; inspect service state before retrying. It does not silently
restore a database or hide a failed service start.

Afterward verify the executable used by the running service and original
`hub-view`/Hub API, per-provider collection success, last real usage event,
personal/company separation, local History continuity and no duplicate final
event. Compare CPU/RSS against samples taken under comparable workload/time
conditions. Keep exact samples in the private handover. A stale quota observation
or an idle provider is not evidence of a new usage event. Approve the second
machine only after the first canary passes. Keep the existing OCI Hub unchanged.

## Rollback

These compatibility fixes do not intentionally change the local telemetry schema.
After checking the final patch set, the normal rollback switches the binary and
restores only settings/hooks/service files listed as deployment replacements,
while **keeping current History**. Other backed-up files, refreshed credentials
and independently upgraded official/Homebrew binaries are preserved; taking a
backup does not authorize overwriting those files. The helper first makes
another complete preservation backup, so later
events and user file changes can be examined before anything is restored.

```sh
python3 scripts/fork/operate.py rollback --manifest '/<private>/canary.json' \
  --backup '/<private-backups>/<pre-canary-backup>'
# Only after explicit rollback approval:
python3 scripts/fork/operate.py rollback --manifest '/<private>/canary.json' \
  --backup '/<private-backups>/<pre-canary-backup>' \
  --execute --approval-ref '<approved-rollback-reference>'
```

`--restore-telemetry-db` is a separate, explicit choice for an incompatible or
damaged local History. It stops the declared writers and restores via SQLite's
backup API; newer events remain in the pre-rollback backup but are no longer in
the active History. Agree on preservation/recollection before using it. The
helper never restores an `opencode-source` database. Do not delete WAL/SHM files
or replace a live database main file manually. A later schema migration would
need its own compatibility decision and approved recovery procedure.

## Evidence and limits

Synthetic tests execute backup/staging and simulated deployment/rollback using
temporary files and SQLite WAL databases. They verify dry-run causes no writes,
committed WAL inclusion, private modes, checksum rejection, source preservation,
normal rollback retaining newer History and explicit DB restore retaining a
newer recovery snapshot. Service calls are mocked. They are not proof of a
production systemd/launchd deployment. No operational service, hook or database
has been changed by these tests.
