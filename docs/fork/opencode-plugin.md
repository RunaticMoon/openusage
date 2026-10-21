# OpenCode plugin compatibility

This patch targets the native contracts verified in OpenCode **2.0.32** (macOS
ARM64) and **2.1.1** (Linux ARM64). It also preserves the older v1 function
contract, including the v1.2.0 loader. It does not install or replace a live hook.
Deploying a fork binary and reinstalling a hook are separate operations.

## Installation contract

`openusage integrations install opencode` (or `upgrade opencode`) runs the
`opencode --version` executable resolved from the installing shell's PATH with a
three-second timeout. It renders exactly one default export:

| Host major | Installed export |
| --- | --- |
| 1 | Legacy async plugin function |
| 2 | `{ id: "openusage-telemetry", setup }` |
| Unknown, absent, failed command, unsupported major | Legacy function with an explicit installation warning |

The generated file records `openusage-opencode-contract: legacy-v1` or `v2`.
Use the same OpenCode executable on PATH that launches the target host. After an
OpenCode major upgrade, reinstall the integration; existing files do not change
automatically. Existing integration backups and JSONC preservation remain in
place. An unsupported host is **not** confirmed compatible by the legacy fallback.

Upstream OpenUsage [PR 421](https://github.com/janekbaraniewski/openusage/pull/421)
was open and unmerged when reviewed on 2026-10-21. Its combined `server`/`setup`
object works with recent v1 loaders, but
[OpenCode v1.2.0 calls every distinct module export as a function](https://github.com/anomalyco/opencode/blob/v1.2.0/packages/opencode/src/plugin/index.ts).
OpenCode 2 rejects a callable function even if it has `id` and `setup` properties
(verified with the actual Effect 4.0.1 schema). Install-time selection preserves
that earlier contract without guessing the host from process arguments.

## Collection and lifecycle

The v2 subscription uses the public `ctx.event.subscribe({signal})` API. It
records only terminal `session.step.ended` and `session.step.failed` events that
actually include token or cost usage. `session.step.started` supplies model/provider
attribution in a bounded in-memory cache. The emitted allowlist contains stable
session/message IDs, the event's workspace directory, timestamp, model/provider,
agent, token components and optional cost. It never forwards prompts, responses,
native provider state, tool arguments/results, files or error bodies.

`session.usage.updated` contains cumulative session totals and is ignored. Token
totals follow the official v2 contract: input + output + reasoning + cache read +
cache write. Missing component and cost fields remain unknown; explicit zero
remains zero. For a partial event the existing normalizer reports the subtotal
of known components, not a verified complete total. Malformed, negative, nonfinite,
fractional or unsafe token counters are rejected, including unsafe subtotals. The original v1 hooks remain available for v1 hosts. Native v2 live tool events are not emitted by this patch; SQLite collection supplies tool usage.

Native subscriptions are explicitly volatile: an overflow or disconnection can
lose events. The plugin reconnects with bounded backoff and aborts its subscription
when unloaded. It cannot replay missed native events. Local SQLite reconciliation
is required for gap recovery and attribution when the corresponding start event
was missed. It must use the same stable session/message IDs and event location
for deduplication. No durable history service is introduced.

A socket error, non-success HTTP response, malformed/oversized acknowledgement, or a native HTTP 200 acknowledgement with `failed > 0` spools the same minimal payload to
the existing hook spool. New spool files are mode 0600 and directories 0700.
Telemetry failures do not reject the host's plugin lifecycle. Disabled telemetry
does not subscribe or send data.

Official contracts checked:

- [v2.0.32 loader](https://github.com/anomalyco/opencode/blob/v2.0.32/packages/core/src/plugin/module.ts)
  and [v2.1.1 loader](https://github.com/anomalyco/opencode/blob/v2.1.1/packages/core/src/plugin/module.ts)
- [Promise subscription and scope cleanup](https://github.com/anomalyco/opencode/blob/v2.0.32/packages/plugin/src/promise/adapter.ts)
- [v2.1.1 session step events](https://github.com/anomalyco/opencode/blob/v2.1.1/packages/schema/src/session-event.ts)
  and [token semantics](https://github.com/anomalyco/opencode/blob/v2.0.32/packages/schema/src/token-usage.ts)
- [Volatile event API](https://github.com/anomalyco/opencode/blob/v2.0.32/packages/protocol/src/groups/event.ts)

## Verification

`go test ./internal/integrations ./internal/providers/opencode ./internal/daemon`
includes `TestOpenCodePluginRuntime`. With Node >=22.18 available, it executes the
rendered TypeScript plugin, posts synthetic events through the actual daemon HTTP
hook handler and checks a temporary SQLite database. No paid model, OAuth login,
production settings or live database is used. CI should pin Node 24 so this test
cannot be skipped for a missing runtime.

The test checks final tokens/cost/cache and attribution, stable-ID duplicates,
ignored cumulative/content events, failed requests, missed-start behavior,
subscription failure/reconnect, disablement, abort cleanup, v1 function invocation,
and daemon rejection/offline/malformed acknowledgement spooling with restrictive permissions and idempotent replay. Set
`OPENUSAGE_TEST_EFFECT_SCHEMA` to an externally installed Effect 4.0.1
`dist/Schema.js` to additionally validate the module against the host's actual
schema implementation; it is not a runtime dependency of OpenUsage.

Both installed native v2 hosts were also run with isolated HOME/XDG directories,
a private loopback port and disposable server authentication. `/api/plugin`
reported this plugin as `active` with server capability, and EOF shutdown exited
cleanly. Native activation is asynchronous: a single API request immediately
after startup can report an empty list, so validation polls until activation
settles. These checks verified loading and lifecycle; the synthetic daemon test
separately verified actual usage persistence. Live production hooks were unchanged.
