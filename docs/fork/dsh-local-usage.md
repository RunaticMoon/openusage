# DeepSeek Harness local usage

The `dsh` provider reads existing DeepSeek Harness v4 session logs locally. It
does not invoke the CLI, inspect credentials, call model APIs, or estimate cost.
This is a different source from the `deepseek` API balance provider and from
Factory's `droid` provider. No Hook or status-line integration is required.

## Configuration

```json
{
  "id": "dsh-personal",
  "provider": "dsh",
  "auth": "local",
  "provider_paths": { "sessions_dir": "~/.dsh/sessions" }
}
```

An explicit `sessions_dir` takes precedence. The default is `$DSH_HOME/sessions`,
or `$HOME/.dsh/sessions` when `DSH_HOME` is unset. The daemon's environment can
differ from an interactive shell; use an explicit path in that case. DSH stores
sessions under its persistence root, independently of named credential profiles.
Point each account at a root whose full contents are approved for that account;
this collector cannot select a credential profile within one shared root. Do not
configure the same root twice unless intentionally displaying it twice.

Auto-detection recognizes a `dsh` binary or an existing default sessions directory
without running the binary or reading profiles. Configured missing directories
produce a `missing` diagnostic. A missing, unconfigured default is silently
ignored, following other optional local sources.

## Accounting contract

The implementation is pinned to the official
[dsh v0.2.1-rc.2 contract](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317).
The relevant definitions are the
[usage projection](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/llm/token-meter/src/usage-projection.ts),
[fork prefix](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/core/session/src/fork.ts),
and [persistence format](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/session/session-persistence-jsonl/README.md).

- Select the highest numeric canonical generation per session directory. Retained
  older generations are not additional sessions. Only generation/header v4 is
  supported; an unsupported newer generation fails closed without older fallback.
- Exclude the inherited prefix through the last tagged `session/end-seed` boundary.
  An ordinary restore boundary does not remove the session's own history.
- Use explicit assistant message usage, or the last embedded usage chunk for
  messages/attempts that lack it. Missing usage is unknown, not a zero-token call.
- Consecutive settlements of the same turn/step replace the prior sample. A
  matching `llm/retry-started` closes that slot; the retry adds a new observed attempt.
  Returning to the same tuple after another step also creates a new slot.
- Input means **uncached input**. Total tokens are input + output + cache read +
  cache write. Reasoning is an optional output detail and is never added again;
  absent reasoning stays unreported. Provider totals must agree with these buckets.
- Include explicitly reported `compaction/summary` usage as additional summarizer
  usage. This is **additional scope beyond DSH's native assistant token meter**;
  compare assistant and compaction totals separately. Summarization/surface
  replacement does not erase the original observed assistant calls.
- Reported usage from failed or interrupted attempts remains counted. Canonical
  status `ok` means a valid accounting observation, not a successful model task;
  allowlisted `outcome` metadata retains `error` or `aborted` when present.

Source is `dsh`; reported provider/model remain the reported route. Canonical views
scope by the exact configured account and `dsh` source, preventing vendor links
from blending these events with other tools or accounts. Stable message identity
contains an account namespace, the source session identity, and the first
settlement sequence of its replacement slot. Later corrections preserve that
identity and time and can decrease counters. Durable revision updates invalidate
the cached view. Raw pruning retains only the source revision; its hash continues
to identify the original accounting payload so an unchanged replay does not write
or restore pruned details. This authority applies only to `dsh_jsonl_v4` messages.

## Privacy and limits

No prompt, response, tool body, workspace path, email, credential, or arbitrary
stream fields are retained in raw payloads. The raw payload allowlist is the
settlement revision, event type, and reported outcome. Canonical session identity,
provider, and model are still normal OpenUsage metadata. Hub uses the existing
snapshot API and schema; it does not receive DSH logs or raw event payloads.

Both plaintext and concatenated zstd logs are supported. The decoder validates
frame checksums. Truncated frames, incomplete JSONL tails, changing files, invalid
numeric accounting, duplicate session identities, and ambiguous encodings reject
the collection. One invalid selected file rejects that whole account's current
batch, rather than exporting a partial aggregate; the next poll retries. Already
ingested events are not deleted. Source files are never repaired or locked for
writing. Diagnostics use fixed codes without source paths or error bodies.

Limits are 10,000 candidate files, 32 MiB encoded input per file, 128 MiB decoded
data per compressed file, 16 MiB per line, 500,000 events per file, and 250,000
emitted accounting records per collection. Symlink entries are not followed.
This implementation provides usage counters and optional response-speed
metadata, not account balance, quota, price estimation, or new permanent Hub history. Existing OpenUsage local
retention and latest-snapshot Hub behavior remain in effect.

## Optional response speed

With the existing performance opt-in, the same parser can enrich a request with
`outputTokens * 1000 / elapsed_ms`. The numerator already includes reasoning;
it is not augmented with reasoning, input, cached or total tokens. This is an
inferred response interval including time before the first output, not decode
speed or a first-token-latency measurement.

The reviewed DSH loop writes `request/header` before dispatch, conditionally
when an envelope or request series starts/changes. Within its active step, one
such header can pair only with the next assistant settlement's explicit final
`finish` timestamp. Headerless later requests stay unmeasured. Stream `time0`
and `dt` encode chunk timestamps; their first chunk is never used as the request
start. The [source contracts and exclusions](../PERFORMANCE_PROVIDERS.md)
describe terminal outcomes, timestamp validation and the inferred interval guard.

The parser consumes headers even when the settled request has no token usage,
and clears timing at retry, step, restore and inherited-history boundaries.
Accounting replacements retain their original IDs and corrected counters while
revoking an ambiguous old timing association. Compaction usage has no verified
request timer. Enabling the option rescans retained logs, so eligible old calls
can be backfilled; replay and restart reuse existing canonical/performance IDs.
The normal local store and authenticated exporter/Hub pipeline carry this
metadata without a new API, migration, model request or separate source scan.
Disabling performance keeps existing usage behavior and does not delete stored
history. Follow the existing [performance operations](performance-operations.md)
for activation, retention and rollback.

## Validation

```sh
go test -race ./internal/providers/dsh ./internal/telemetry ./internal/detect
go test -race ./internal/daemon -run TestDSHPerformance
go test -race -tags fork_integration ./internal/daemon -run TestDSH
```

The tagged test exercises the real parser, canonical revisions, replay, two
accounts sharing a synthetic session ID, and the existing authenticated Hub
snapshot endpoint. Fixtures are synthetic and perform no model requests. Provider
tests cover generations, checksum/torn inputs, fork boundaries, replacement and
retry slots, reported zero versus absent usage, route attribution, and privacy.
An optional `OPENUSAGE_PERFORMANCE_REAL_DSH_ROOT` test reads an explicitly
approved source root and compares every accounting event with performance off
and on. It writes no source data and reports only aggregate numeric evidence.
