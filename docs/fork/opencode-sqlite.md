# OpenCode SQLite compatibility

The reader accepts the existing v1 `session/message/part` layout and the v2
`session_v2/session_message` layout verified in OpenCode v2.0.32 and v2.1.1.
It validates columns before querying. Unknown layouts, invalid usage JSON,
missing configured databases and SQLite locking errors are distinguishable
from a supported empty database. An absent unconfigured optional source is
still skipped. Source errors never rename, repair or recreate the source DB.

Connections use an escaped file URI with `mode=ro`, query-only mode and a
bounded one-second busy timeout. A single read transaction covers schema and
record queries, preserving live WAL visibility. `immutable=1` is not used.
The fingerprint is cached only if the main DB and WAL remain unchanged across
the read; a concurrent update is therefore retried on the next cycle.

V2 assistant messages are individual usage records. Parent/child sessions are
separate; session cumulative counters and user messages are never summed.
A stable session/message identity deduplicates repeated reads and Hook input.
Updates to an existing message are re-emitted. Correct canonical replacement
and cache invalidation require the companion `fix/opencode-final-usage` patch.

V2 input excludes cache, output excludes reasoning, and cache read/write are
separate. Total is the sum of these five disjoint buckets, once each. Missing
values remain unknown; explicit zero remains zero. Negative, fractional or
overflowing token counts fail parsing. Source `cost` is provider/API-equivalent
usage cost and is not evidence of a subscription charge or actual invoice.
The fork does not change global token display normalization.

Completed/failed tool identities and statuses are collected from structured
content. Text, reasoning text, prompts, tool arguments/results and native
provider replay data are not copied. The source path, record identity and
revision remain in local provenance; public fixtures contain synthetic data.

References: [v2.0.32 schema](https://github.com/anomalyco/opencode/blob/v2.0.32/packages/core/src/session/sql.ts),
[v2.1.1 schema](https://github.com/anomalyco/opencode/blob/v2.1.1/packages/core/src/session/sql.ts),
[v2 token definitions](https://github.com/anomalyco/opencode/blob/v2.1.1/packages/schema/src/token-usage.ts).
