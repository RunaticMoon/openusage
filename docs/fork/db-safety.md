# Preserve telemetry databases on open failures

This maintenance patch addresses upstream [issue #423](https://github.com/janekbaraniewski/openusage/issues/423).
As checked on 2026-10-21, the issue is open, has no maintainer response or linked
fix, and `internal/telemetry/store.go` and `sqlite.go` on upstream main
`430a1d39814d458e972cf4dce98135d6e786992c` are identical to the installed release
v0.26.1 (`299e3dbffb5a6827bd89d9037ef8e483e5073bfa`). This is an independently
implemented fork patch; no external patch has been copied.

## Behavior

Previously every `OpenStore` removed the shared WAL index (`-shm`). An unsuccessful
integrity check could then rename the database, remove its sidecars, and create an
empty replacement. Opening a store also deleted all but the newest prior
`.corrupt.*` backup. These actions did not establish that other processes had
closed the database. A daemon and its short-lived hook fallback could consequently
use different files while believing they were sharing the same database.

The patched open path leaves the database, WAL index, and historical quarantine
files under SQLite's control. SQLite retains responsibility for WAL locking and
crash recovery. A failed integrity check returns an error and leaves the database
in place; it does not silently replace history with an empty database. An error
that prevents the check, including a lock or I/O error, is returned with its
original cause and is not relabeled as confirmed corruption. No new sidecar lock
protocol, DB schema, migration, or storage format is introduced.

This deliberately changes recovery behavior: a damaged store can stop collection
until an operator investigates it. Automatic availability through an empty
replacement is no longer preferred over preserving evidence and history.
Existing `.corrupt.*` files are not assumed to be corrupt merely because of their
names, and are never automatically deleted on open.

The patch cannot prevent an older, unpatched process from deleting files itself.
Do not run old and patched writers against the same test database to validate the
patch. Cross-version verification must use separate databases and the existing
Hub protocol.

## Verification

Tests use only synthetic databases in temporary directories:

- A separate process opens a live WAL database, observes and adds committed rows,
  and exits. The original writer retains the same database and sidecar files;
  both writers' rows survive a subsequent restart.
- SQLite recovers a committed WAL after a writer exits without closing its store,
  with no application-side removal of the WAL index.
- An active write transaction makes a second open return a SQLite busy/locked
  error. Files and history survive, and retry succeeds after the lock is released.
- A deliberately damaged database is rejected without renaming or replacing it.
- Historical quarantine files and their sidecars remain byte-for-byte intact.
- Integrity-check failures remain distinguishable from an inconclusive check.

Run the focused tests with:

```sh
CGO_ENABLED=1 go test ./internal/telemetry -run 'Test(OpenStore|QuickIntegrityCheck)' -count=1
CGO_ENABLED=1 go test -race ./internal/telemetry -count=1
```

The live-WAL and historical-quarantine regressions were first run against the
unchanged v0.26.1 implementation. They failed: the second process replaced the
WAL index, the first process observed two marker rows rather than three, and two
historical quarantine files were removed. These tests do not use a user's private
database or assert that the reported race occurred in production. They directly
cover the unsafe operations and the intended failure behavior.

## Recovery and rollback

Investigate the returned error before taking action. A busy/locked error is
retryable after the competing writer has finished; do not delete `-wal` or `-shm`
to resolve it. For an integrity failure, first stop every process that can open
the affected store, and preserve the database and any sidecars together in a
restricted recovery location. Inspect a copy, retaining the original evidence.
A live backup must use SQLite's backup API or another consistent SQLite backup
method rather than copying only the main file.

Restoring a known-good backup or using SQLite recovery is an explicit offline
operation. Replacing a database may discard newer usage; preserve the failed
store and determine which local source records can be reimported before restoring.
The fork does not automate that destructive decision.

The patch itself requires no migration or DB restore to roll back: a binary
rollback remains schema-compatible. Returning to an unpatched binary also
restores its unsafe open/recovery behavior, so it is not a remedy for issue #423.

References: [SQLite corruption hazards](https://www.sqlite.org/howtocorrupt.html)
and [SQLite WAL behavior](https://www.sqlite.org/wal.html).
