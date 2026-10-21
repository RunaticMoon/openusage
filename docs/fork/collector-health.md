# Local collector health

The daemon's existing `GET /healthz` Unix-socket API retains top-level
`status: "ok"` for process liveness. It optionally adds a `collectors` map;
old clients ignore that field, and new clients accept old responses without it.
No Hub API, exported snapshot, quota status or persistent history format changes.

```sh
curl --silent --show-error \
  --unix-socket "${XDG_STATE_HOME:-$HOME/.local/state}/openusage/telemetry.sock" \
  http://localhost/healthz
```

Each entry is keyed by `kind/escaped-source/escaped-account` and also contains
`kind`, `source` and optional `account_id`. An empty account identifies existing
source-wide attribution; it must not be assumed to represent a particular
configured account when multiple accounts share the same source paths.

| Field | Meaning |
| --- | --- |
| `kind: source` | Local telemetry source collection |
| `kind: provider_poll` | Actual provider fetch, including quota/auth status |
| `last_attempt_at` | UTC time the most recent actual read/fetch started |
| `last_success_at` | UTC time its most recent successful result completed |
| `last_event_at` | Latest actual message/tool/turn event timestamp observed by source collection |
| `collected_count` | Records returned by the latest source attempt, or 1 for a successful provider fetch; 0 on empty/error results |
| `status` | Fixed diagnostic code described below |

A provider snapshot's observation time is never treated as an actual usage-event
time. Raw envelopes and quota snapshots cannot advance `last_event_at` either.
Timestamps that are not yet known are omitted. Empty successful source reads
advance `last_success_at`, preserve `last_event_at` and report `no_new_events`.
Failures preserve the previous success/event times. Cached, unchanged and
backoff-skipped provider snapshots do not advance attempt/success times.

Codes are `ok`, `no_new_events`, `missing`, `unsupported_schema`, `database_busy`,
`permission`, `parse_error`, `auth_required`, `error` and `unknown`.

- `ok` / `no_new_events` describe successful **source reads**, not successful
  durable ingestion, deduplication, export delivery or complete usage coverage.
  Counts can include already-known records and are not new billable requests.
- Missing/permission classifications require an explicit filesystem error.
  Busy/locked classifications use SQLite's typed error codes. Schema/parse
  collectors may return a typed error with `CollectionErrorCode() string`.
  JSON decoding errors also map to `parse_error`.
- `auth_required` for provider polling comes from the provider's existing auth
  status. Normal, near-limit and limited quota snapshots are successful fetches;
  their original quota status is unchanged. Unsupported/unknown provider states
  remain `unknown` in this local diagnostic view.
- Errors and codes are allowlisted. Error text, paths, credentials, tokens,
  prompts and provider payloads are not copied into health responses.
- When an existing provider suppresses a missing source or parse failure and
  returns an empty successful result, this generic layer can only report
  `no_new_events`. It cannot infer a more specific cause from absent data.

The map is an in-memory record of attempts in the current daemon process. It
resets on restart and can retain the last observation for an account removed
from settings until restart. It is not an offline/retained history API. If the
map or an entry is absent, the client has no observation from this process; it
must not infer a failure or a zero usage total. Hook delivery and durable store
health require their own ingestion acknowledgements and verification.

Tests cover old/new JSON compatibility, omitted unknown timestamps, successful
empty/error/recovery transitions, real event versus poll timestamps, error
redaction/classification, cached-fetch skips and concurrent health reads/writes.
