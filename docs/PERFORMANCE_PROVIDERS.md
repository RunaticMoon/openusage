# Native log response-speed coverage

Performance collection is opt-in. Enabling it enriches the existing source
parser; it does not start another collector or make provider requests. The
accounting fields, cost estimates and quota snapshots keep their existing
semantics. A response speed is `output_tokens * 1000 / elapsed_ms`; it may
include first-token latency and is not pure generation/decode speed.

## Adapter coverage

| Source and inspected version | Identity and output | Request timing | Current behavior |
| --- | --- | --- | --- |
| Claude Code 2.1.304, personal macOS native JSONL | `requestId`, with native `message.id` fallback; `message.usage.output_tokens` | Exact same-file `api-request.id` → `assistant.requestRef` join; logged start/assistant timestamps | Supported **inferred log boundary**. Final stop reason and success are required for measured statistics. Missing or conflicting joins remain unmeasured. |
| Codex 0.163.0 native JSONL + native `logs_2.sqlite` | Exact `token_usage_record.response_id` → completed log tag; **only** `usage.output_tokens` | Unique sampling-start log in the same process/thread/turn → exact completed-response tag | Optional **inferred log boundary**, subject to version, scope, retry and ambiguity guards. Usage-only observations retain unknown outcome and unavailable timing. The `token_count` accounting stream is unchanged. See the [native timing design](CODEX_NATIVE_TIMING_DESIGN.md). |
| OpenCode v2.0.32 (macOS), v2.1.1 schema (Linux) | `session_message.id`, `tokens.output` | Native assistant `time.created` → `time.completed`, integer milliseconds | Supported direct provider request timestamps. A native successful finish is required. `tokens.reasoning` is separate and is never added to output. |
| OpenCode v1 / legacy JSONL and hooks | Existing usage identity | No additional verified request-clock adapter in this change | Existing usage remains available; no fabricated performance. A v2 SQLite observation can enrich the same request later. |
| DSH v0.2.1-rc.2, session format 4 | Existing native usage settlement identity and counters | Existing parser has action timestamps, routing context and retry markers; no verified matching request-start/end pair | Unsupported for speed. Do not join the latest context, turn, retry or prior action timestamp as an assumed request start. |
| Antigravity CLI 1.4.3 native transcript | Completed `PLANNER_RESPONSE`/`MODEL` step and output counter | The inspected parser has `created_at`, not a verified per-step start/end or elapsed field | Unsupported for speed. `DONE` proves the accepted usage state, not a request duration. Statusline latest/cumulative usage is not a substitute. |
| Ollama desktop / existing GIN log parser | Desktop telemetry estimates tokens from content; GIN logs have HTTP duration/status/path | GIN duration is not joined to a native output counter/request identity | Unsupported for speed. Existing HTTP latency summaries or desktop timestamps cannot be combined into request throughput. No new source inspection or instrumentation is introduced. |
| Copilot existing JSONL parser | `assistant.usage` has output and a `duration` field; current accounting message identity includes file line | Parser calls the field `duration_ms`, but this audit did not verify the installed producer's request scope, finality, missing-vs-zero semantics or fork-stable identity | Unsupported pending producer-contract verification. Session shutdown `totalApiDurationMs` is explicitly not a request timer. |
| Remaining providers | Existing accounting/quota only | No verified per-request clock added | Unsupported; no zero-speed placeholder and no timing inferred from quota polling. |

The extra-source review above used existing fork code only. It does not assert
that every upstream product lacks timing, or promise support for an uninspected
schema/version. No DSH, Antigravity, Ollama or Copilot parser was rewritten for
this feature. Provider-level lack of observations is not evidence of zero
traffic: the coverage denominator contains only identifiable request
observations accepted by a performance adapter.

## Meaning of the reported output basis

`output_basis` identifies the counter's semantics as well as its source field:

| Emitted basis | Meaning |
| --- | --- |
| `claude.message.usage.output_tokens` | The raw output counter reported in the native message, unchanged. The inspected local log does not independently establish thinking-token inclusion for every configured billing route. It is **not a promise of visible-text-only output**; no separate thinking/reasoning field is added or subtracted. |
| `codex.token_usage_record.usage.output_tokens` | The per-response reported output counter, unchanged. In Codex 0.163.0's OpenAI Responses mapping, reasoning is a detail within the reported output count, so it is not added again. Custom provider semantics are retained as reported rather than inferred from a model name. A rate requires additional accepted native-log completion and timing evidence. |
| `opencode.tokens.output_excludes_reasoning` | The native OpenCode v2 output bucket excludes its separate reasoning bucket. Only that output bucket is the numerator, including explicit zero when reasoning is positive. |

The pinned [Codex Responses conversion](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/codex-api/src/sse/responses.rs#L123)
copies the wire output count directly and copies the reasoning breakdown into a
separate detail field without adding it to output. The
[OpenAI Responses usage contract](https://developers.openai.com/api/reference/resources/responses/methods/retrieve)
defines reasoning as an output-token breakdown. These counters therefore must
not be advertised as a uniform visible-text tokenizer measurement across
providers. Coverage reports retain the output-basis breakdown for inspection.

## Zero, partial, failed and corrected observations

An explicit `output=0` with valid request time and successful completion is a
measured **0 tok/s**, including OpenCode responses with positive reasoning and
zero output. Missing output is unknown and produces a null speed. Reasoning is
never added to any source's output numerator; already-inclusive counters remain
unchanged. Input, cache, total tokens, work and cost are never numerators.

- Claude only joins the exact recorded request reference. Parallel requests do
  not share the latest user/tool/turn boundary. Conflicting starts or mismatched
  sessions are ambiguous. Inferred intervals outside 100 ms–15 min are excluded;
  this guard does not cap verified direct provider timestamps.
- Claude streaming chunks retain the existing MAX accounting merge. Performance
  independently follows the latest native observation, allowing a final output
  correction to replace the earlier measurement. An unterminated trailing line
  cannot finalize timing; the file offset is retried after completion.
  Final observations supersede partials even at the same millisecond. A delayed
  partial cannot regress a final. Two differing final records with identical
  native timestamps cannot be ordered reliably: the first accepted final is
  retained until a newer native revision arrives. File offsets are not used as
  revisions because copied/forked transcripts change them.
  Claude's performance billing provider remains `unknown`: the model vendor
  alone cannot distinguish direct Anthropic billing from Bedrock/Vertex routes.
- OpenCode uses the full created/completed interval. `time.streamed` was observed
  close to completion and is not used to claim generation speed. Database
  update times are revisions, never a substitute for request clocks. Missing,
  noninteger, nonpositive or invalid native timestamp values remain unmeasured.
  This source's milliseconds must fall within 2000-01-01–2100-01-01 UTC
  (end exclusive). Seconds/microseconds/nanoseconds are rejected, not guessed or
  rescaled. Valid direct intervals still have no inference-duration cutoff.
- Codex task/turn `duration_ms` and `time_to_first_token_ms` may cover multiple
  requests. Neither is assigned to an individual response. `turn_token_usage`
  and `thread_token_usage` are also excluded from the request output counter.
  The native-log adapter instead joins the exact completed response ID, then
  requires an unambiguous sampling-start event in the same process/thread/turn.
  Because that start has no request ID, the interval remains inferred and is
  limited to 100 ms–15 min. Known retries, competing starts, unsupported producer
  versions and missing/invalid evidence remain unmeasured. The source DB is read
  only when performance is enabled; it does not add another session/archive
  scan or fall back from a custom profile to a different Codex home.
- Error/aborted outcomes and partial responses do not enter successful measured
  statistics. A native timestamp alone does not prove a successful outcome.
  Unknown outcomes stay unknown even when accounting has settled.
- IDs use a native request namespace plus configured source/account. Output,
  timestamps, duration, file offsets and collection time are not request IDs.
  Replays and copied native identities merge; separate retry IDs remain distinct.
  Newer source observations can revise performance without retokenizing usage.

Source paths are local provenance only (`json:"-"` in the request). Adapter
metadata contains no prompt, text blocks, tool arguments/results, errors from
the native response, authentication data or configuration content. Disabled
collection keeps the previous cache key and parser path. Enabled cache entries
include the option state; rotation/truncation cannot reuse an unrelated start.

## Reference contracts

- This audit starts from OpenUsage fork custom commit
  `1fc969cb2dcdcbb830a10d3301db72426b830bda`. The core duration/rate definition and
  histogram/window behavior are in [PERFORMANCE_DESIGN.md](PERFORMANCE_DESIGN.md).
- Claude 2.1.304 exact-reference joining is an observed native log contract,
  **not** a published promise of network-only timing. The JSONL fixtures retain
  only the field structure; personal raw logs are not committed.
- Codex tag `rust-v0.163.0`, commit
  [`83086188ae0614bd882818ff1aa23f3194962012`](https://github.com/openai/codex/tree/83086188ae0614bd882818ff1aa23f3194962012):
  [`record_incomplete_response_usage` and the shared usage writer](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/mod.rs#L4948)
  accept incomplete responses too. The
  [rollout persistence policy](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/rollout/src/policy.rs#L160)
  does not persist the transient raw completed event in ordinary rollouts.
  Therefore a token-usage record cannot establish success by itself. Additional
  native diagnostic log evidence comes from the
  [sampling-entry feedback event](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/turn.rs#L2630)
  and the [successful completed-response feedback tag](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/client.rs#L2572).
  The [design](CODEX_NATIVE_TIMING_DESIGN.md) distinguishes this inferred join
  from direct request timing; [operations](fork/codex-native-timing.md) documents
  profile paths, optional enablement and data-preserving rollback.
- OpenCode [v2.0.32 SQLite schema](https://github.com/anomalyco/opencode/blob/v2.0.32/packages/core/src/session/sql.ts),
  [v2.1.1 SQLite schema](https://github.com/anomalyco/opencode/blob/v2.1.1/packages/core/src/session/sql.ts)
  and [token bucket definitions](https://github.com/anomalyco/opencode/blob/v2.1.1/packages/schema/src/token-usage.ts).
  The existing compatibility work is documented in
  [opencode-sqlite.md](fork/opencode-sqlite.md).
- DSH reference commit
  [`687f282e8625c6b8d11ff58211a90524be2e4317`](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317)
  supports the existing accounting projection, not a newly inferred timer; see
  [dsh-local-usage.md](fork/dsh-local-usage.md).
- Antigravity [headless output](https://antigravity.google/docs/cli/headless/)
  distinguishes step usage from cumulative results. The observed native 1.4.3
  transcript format is not promoted to a published duration contract here.

## Executable evidence

| Requirement | Test |
| --- | --- |
| 150 outputs / 2000 ms = 75; reasoning excluded; opt-in accounting identical | `opencode.TestV2PerformanceOutputOnlyAndAccountingInvariant` |
| Explicit zero, missing output/time, failed/partial, invalid units, revisions | `opencode.TestV2PerformanceZeroMissingFailedPartialAndCorrections` |
| Exact Claude join, final downward correction, unchanged accounting MAX | `claude_code.TestClaudeExactJoinFinalCorrectionsAndAccountingInvariant` |
| Parallel exact joins, ambiguity, session mismatch, missing/zero output, sleep guard | `claude_code.TestClaudePerformanceAmbiguityMissingZeroAndGuardrails` |
| Partial last line, incremental append, ten unchanged scans, replacement/truncation, opt-in cache | `claude_code.TestClaudePerformanceIncrementalPartialRotationAndFlagCache` |
| API failure and original parser's oversized-line prefix accounting | `claude_code.TestClaudePerformanceFailureAndAccountingPrefixPreserved` |
| Codex per-response output; turn totals/timers excluded; usage-only outcome unknown | `codex.TestCodexPerformanceNativeIdentityWithoutInventedTimingOrAccounting` |
| Codex native DB timing, profile isolation, late evidence, correction, retention and privacy | [Native timing regression map](CODEX_NATIVE_TIMING_DESIGN.md#evidence-and-validation-boundary) |
| Codex delayed native completion → existing store → Exporter → Hub | `daemon.TestCodexNativePerformanceLateCompletionToHub` |
| Explicit personal Codex JSONL/native DB → isolated store/Hub, accounting and metrics unchanged | opt-in `daemon.TestPerformanceRealCodexReadOnlyToHub` |
| Legacy hook plus native log, ten replays, unchanged canonical tokens/cost, 200 outputs / 3000 ms = 66.667, Exporter → authenticated original Hub | `daemon.TestPerformanceNativeSourceAccountingExporterHub` |
| Confirmed personal log → isolated SQLite → canonical snapshot → Exporter → isolated Hub, actual off/on accounting comparison | opt-in `daemon.TestPerformanceRealClaudeReadOnlyToHub` |

The real-source test requires the explicit environment variable
`OPENUSAGE_PERFORMANCE_REAL_CLAUDE_ROOT` pointing to a previously verified
personal Claude projects directory. It only reads that source. Stores,
alternate empty directory and loopback Hub are temporary, and no production
credentials or service configuration are read. Test output is numeric aggregate
metadata only. `cold_off_ms` and `warm_on_ms` are one ordered run with unequal
filesystem-cache conditions; they are operational measurements, not a controlled
performance benchmark or a statistically supported overhead claim.
