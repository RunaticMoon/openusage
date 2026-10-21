# Quota observation freshness

An unchanged quota value can be a new observation. Conversely, a recently
completed provider fetch can still contain an old cached quota. This patch keeps
those cases separate using the existing snapshot timestamp and source evidence.

## Collection and persistence

Claude remote subscription usage can change on another machine while local
session files remain unchanged. Its change detector therefore allows polling
when the existing OAuth credentials file or supported desktop cookie database
is present, or after this provider instance has already fetched quota data.
The detector checks file metadata only; it does not resolve credentials or
invoke the macOS Keychain. The normal fetch path continues to own authentication.
Missing credentials on an otherwise idle installation do not create new work.

The existing adaptive scheduler remains in effect: the default 30-second base
interval can back off to 16 times the base interval for local change-detector
providers. A successful remote revalidation can therefore be up to roughly eight
minutes apart, plus the poll loop/fetch duration. No command or endpoint bypasses
that schedule in this change.

The daemon now persists unchanged observations only for reviewed sources:

- Claude requires a successful usage API response (`usage_api_ok`).
- Codex requires quota provenance from the live API or CLI RPC, rather than
  session-file fallback limits.
- OpenCode and DSH reread their sources during a successful fetch and do not
  substitute previously successful quota responses on failure.

These checks use server-side source metadata; they add no fields to the wire
contract. Unreviewed providers retain their previous change-only persistence.
In particular, a Cursor billing-cache fallback or a Codex session-only fallback
is not promoted to a new unchanged observation. Changed values and failure
statuses retain the existing ingestion behavior.

Quota persistence remains a `limit_snapshot` event, excluded from token usage
and request analytics. Observation IDs retain provider, account and timestamp
identity, so replaying the same observation does not create duplicate events.
Scheduler and unchanged-local-source cache skips do not create observations or
advance the read-model data version. Poll logs call the persisted result count
`observations`, because it can include identical values confirmed again.

## Cached Claude quota

The in-memory usage API cache now keeps the original observation timestamp
alongside its response. If authentication or a request fails, the fallback
preserves that timestamp and evaluates reset boundaries at that original time.
It does not invent a zero usage value merely because the clock passed a reset
while the API was unreachable.

The existing JSON snapshot has one timestamp. A snapshot containing newer local
telemetry and older cached quota therefore conservatively keeps the older quota
timestamp. Local counters remain present, but consumers may mark the whole
source as stale. This protects existing clients without requiring a per-metric
freshness schema. Successful quota recovery replaces the cache with a genuine
new observation. The change does not make cached credentials valid or refresh
the user's login.

## Verification and operations

Synthetic tests cover credential presence with unchanged local files, successful
unchanged polls reaching SQLite/read-model while adaptive backoff still grows,
cache skips and replay deduplication, and failed authentication retaining the
original quota timestamp through SQLite, Exporter serialization and the Hub.
Fixtures use temporary homes, temporary databases and local HTTP servers; no
operational credentials or paid model requests are required.

No database schema, retention policy, Hub configuration, account selection or
operational service is changed by this patch. Follow the existing
[native build and deployment procedure](operations.md) and obtain separate
approval for the reviewed binary before replacing a Collector. Compare actual
provider observation times with health attempts after installation; process
liveness and exporter heartbeats do not establish quota freshness.
