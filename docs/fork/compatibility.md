# Compatibility and evidence

Record the **tested SHA and scope** with every result. Source support, synthetic
tests, a native binary running, real data collection and approved production
deployment are different claims. Private paths, addresses, account data and raw
usage totals belong in the private operational handover.

## Baseline and targets

| Item | Observed baseline / current evidence |
| --- | --- |
| Upstream release | v0.26.1, `299e3dbffb5a6827bd89d9037ef8e483e5073bfa` |
| Observed upstream main | `430a1d39814d458e972cf4dce98135d6e786992c` on 2026-10-21 KST |
| OCI target | Linux ARM64, observed Ubuntu 26.04.1 LTS, 2 CPUs; the earlier 24.04 assumption was not used |
| Personal Mac target | M1 Max / macOS ARM64; exact host OS/configuration stays in private handover |
| Go / SQLite | Go 1.26.9 toolchain; CGO and a native C compiler required by `mattn/go-sqlite3` |
| Native CI | GitHub-hosted Ubuntu ARM64 and macOS ARM64, never an unrestricted personal runner |
| Installed operational binary | Official v0.26.1 remains in place until separate deployment approval |

## Collection and safety

| Boundary | Evidence / required distinction |
| --- | --- |
| Installed OpenCode source | v2.1.1 was inspected read-only using a consistent private SQLite backup. Independent assistant-usage bucket aggregation and canonical storage replay passed; no raw DB/totals are published. This does not yet establish correct OpenCode tile projection for a different billing provider. |
| SQLite old/current schemas | Explicit schema detection, legacy and v2 fixtures, WAL visibility and unsupported-schema diagnostics are covered by the SQLite compatibility patch. Empty/no-usage state differs from unsupported structure. |
| Provider/account mapping | Preserve tool, billing provider, model, session and stable original IDs; fixtures cover cache fields and reasoning. Missing fields are not silently converted into an invented complete total. |
| OpenCode snapshot projection | An additional read-filter defect was found: canonical OpenCode events attributed to a billing provider such as Anthropic can be filtered out of the OpenCode tile. A separate source/account projection fix is being prepared. Do not mark real OpenCode tile/Hub usage complete until its final commit and runtime tests pass. |
| Repeated/partial usage | Stable message IDs and mutable-row revisions address later final values and Hook/DB replay. Check the update patch and canonical replay evidence; do not add request values to session cumulatives. |
| Database open errors | Store-open failures preserve original data instead of treating every error as corruption. Lock, read-only, directory/permission, invalid header and migration/schema errors need distinct handling. |
| OpenCode plugin | The rendered plugin factory/lifecycle and usage delivery have dedicated contract tests. A plugin source patch does not prove the existing operational hook was replaced or that every host credential path works. |
| Provider diagnostics | Optional diagnostics must preserve JSON consumers and distinguish attempt/success, latest usage and no new event. Final integration tests determine the exact new fields; do not infer data freshness from process liveness. |
| Deployment | No patched production service/hook replacement is claimed by the source tests. Canary deployment remains separately approved. |

For usage interpretation, count cache read/write once according to the source's
definition, and do not add reasoning twice when it is part of output. When the
source provides a complete normalized input total, display
`Total = input_total + Output`, `Cached = cache_read`, `Work = Total - Cached`.
Missing components remain unknown/partial. Provider-reported/API-estimated cost
is not automatically a confirmed bill, especially for OAuth subscriptions.
The same account's quota observed on two machines must not be summed.

## Hub contract retained for future clients

The contract is the existing upstream implementation in
[`internal/core/remote.go`](../../internal/core/remote.go),
[`internal/hub/server.go`](../../internal/hub/server.go) and
[`internal/hub/store.go`](../../internal/hub/store.go).

| Endpoint | Contract |
| --- | --- |
| `POST /v1/push` | `RemoteEnvelope` contains `machine`, RFC3339 `sent_at` and `snapshots`; non-empty machine is required, body limit is 4 MiB. Bearer auth is required when configured. |
| `GET /v1/snapshots` | JSON map of current `UsageSnapshot` values; keys and returned account IDs are `machine:providerID:accountID`. Bearer auth is required when configured. |
| `/healthz` | Unauthenticated liveness status and currently non-stale machine names; still restrict the listener to the tailnet. |

The Hub stores one newest envelope per machine **in memory**. New pushes replace
that machine's prior batch. Eviction uses server receive time and configured
`hub.stale_timeout_seconds` (upstream default 300 seconds). After expiry the
machine disappears; the Hub does not provide offline history or durable central
request records. A Hub restart loses its in-memory state until machines export
again. Within one envelope duplicate provider/account pairs are ignored.

The daemon exporter starts immediately and then uses `export.interval_seconds`
(upstream default 60 seconds). `hub-view` has its own polling cadence and supports
`--interval`; delivery/polling/collection delays all contribute to visible age.
Do not claim that a successful HTTP push implies fresh quota or a new real usage
event. Existing production snapshots were observed through the original API,
but one Mac observation had an older timestamp; freshness and macOS credential
access remain explicitly tracked follow-ups.

Read-only examples use a preconfigured private token environment; no token value
belongs in these commands or documents:

```sh
openusage hub-view 'http://<tailnet-hub>:<verified-port>' --interval 10s
# Keep an authenticated curl config outside Git with mode 0600:
curl --config '/<private>/hub-read.curl' 'http://<tailnet-hub>:<verified-port>/v1/snapshots'
```

Test original Hub/new Collector and new Hub/original Collector with synthetic
envelopes on loopback before canary. Keep compatibility test run/SHA references
in the integration PR; a code review alone is not a cross-version runtime test.

## Build versus execution evidence

The automation branch's Linux ARM64 build helper was executed successfully,
including `--version`, dynamic linkage inspection and checksum verification. That
checks the helper and baseline native build, not the final integrated patch SHA.
The [automation PR](https://github.com/RunaticMoon/openusage/pull/4) has native CI;
inspect its exact run/SHA and the final integration run before using artifacts.
Mac compile success alone must never be reported as Mac execution success.

The operations helper's SQLite/staging/rollback regressions are synthetic and
mock service management. They preserve OpenCode source DBs and default to dry
run. Native systemd/launchd canary success and real collection remain separate
deployment evidence.
