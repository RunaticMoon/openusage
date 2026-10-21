# Isolated fork compatibility checks

These tests require the combined OpenCode v2 plugin/parser, SQLite v2 reader,
canonical revision/cache update, account/source projection, and SQLite safety patches. They are an
integration gate for the combined candidate, not a claim that unmodified
v0.26.1 understands OpenCode v2.

Run the synthetic integration regression on that candidate:

```sh
CGO_ENABLED=1 go test -race -tags fork_integration ./internal/daemon \
  -run '^TestOpenCodeV2HookSQLiteReadModelHubCompatibility$' -count=1
```

The build tag keeps the test-only branch's ordinary test suite compatible with
the upstream base. Explicitly running this tagged test on v0.26.1 fails because
the native v2 hook is not parsed as message usage; it is not silently skipped.

The test passes native synthetic Hook and SQLite records through the actual
provider parsers, canonical ingestion, cached read model, exporter and an
authenticated loopback Hub. Both Hook-first and SQLite-first orders must keep
one stable event. Partial zero, final positive and downward correction values
must reach the Hub. Late Hooks and source restart replay must not duplicate or
roll back usage. Requests, input tokens, cost and source-content exclusion are
checked. All databases and listeners are temporary; no host credentials or
production services are used.

The matrix covers OpenCode, Anthropic, OpenAI and custom model vendors, plus a
Hook that lacks model attribution until SQLite supplies it. The Hook-first
cases prime the snapshot cache before the DB revision. This catches the original
projection gap where vendor-attributed OpenCode usage was stored successfully
but missing from the OpenCode snapshot.

For the binary protocol contract, build the combined candidate and supply an
official v0.26.1 binary for the same operating system and architecture:

```sh
python3 scripts/test-fork-compatibility.py \
  --original /path/to/original/openusage \
  --candidate /path/to/candidate/openusage \
  --report /path/to/private/compatibility.json
```

This launches real Collector/Exporter and headless Hub processes for both
directions: modified Collector to original Hub, then original Collector to
modified Hub. The first direction exercises SQLite v2 revisions through the
daemon's asynchronous collection and cache loop; the second uses the original
supported Hook contract with duplicate delivery. Both require authenticated
snapshots and reject unauthenticated reads. Python 3's standard library is the
only script dependency.

The script creates separate temporary HOME/XDG roots, explicit temporary
sockets, canonical databases and source fixtures, a fresh ephemeral token, and
loopback listeners. Its child environment excludes inherited provider keys and
tool paths; its single synthetic OpenCode account has no API key or browser
session and uses a loopback-only API base. There are no model calls, service
registrations or production exports. Temporary processes are terminated even
on failure; the optional report contains versions and numeric synthetic test
results, never the authentication token or source bodies.

These checks validate snapshot-wire compatibility and reconciliation, not
production freshness, live provider quotas, schema downgrade safety or network
configuration. The Hub remains an in-memory latest-snapshot service.
