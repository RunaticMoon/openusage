# Codex native-log request timing

This adapter enriches Codex request observations with timing from Codex's own
SQLite diagnostic log. It uses the existing Collector, performance metadata
store and snapshot channel. It does not depend on a usage-limit service, Paseo
timing database, OTLP receiver, plugin, new daemon or a model request.

The measurement is **inferred log-boundary response speed**:
`reported output_tokens * 1000 / elapsed_ms`. It includes local sampling setup
and waiting before output. It is not direct provider timing, decode speed,
visible-text throughput or TTFT. Missing evidence remains unmeasured.

## Audited producer contract

The verified producer is Codex **0.163.0**, official tag `rust-v0.163.0`, commit
[`83086188ae0614bd882818ff1aa23f3194962012`](https://github.com/openai/codex/tree/83086188ae0614bd882818ff1aa23f3194962012).

| Observation | Producer and meaning |
| --- | --- |
| Native JSONL `token_usage_record.response_id` and `usage.output_tokens` | [The usage writer](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/mod.rs#L4948) accepts completed and incomplete responses. This gives request identity and output, not success or elapsed time. |
| `feedback_tags` event with sampling configuration fields | [`try_run_sampling_request`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/turn.rs#L2630) emits these at sampling entry. The log context identifies process, thread and turn, but has no response ID. |
| `feedback_tags.last_model_response_id` | [`map_response_events`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/client.rs#L2572) emits this in `ResponseEvent::Completed`. An exact native response-ID join supplies successful completion evidence and a logged end time. |
| SQLite `logs.ts`, `ts_nanos`, `process_uuid`, `feedback_log_body` | Native log event time, process scope and formatted fields. A log row timestamp is used only after recognizing the relevant producer event. |

The reported output counter is retained unchanged. The pinned Responses
[conversion](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/codex-api/src/sse/responses.rs#L123)
keeps reasoning as an output detail; the adapter does not add it again. Input,
cached input, cumulative turn/thread totals and estimated total tokens never
enter the speed numerator. Explicit output zero can produce valid zero speed;
missing output cannot.

The supported session producer version is 0.163.0. The gate requires the
allowlisted `session_meta.cli_version` and an explicit `token_usage_record`
thread ID; a session-name fallback cannot establish a timing scope. Other or
missing version evidence is not promoted to a verified contract. A persisted
session's creation version does not independently prove the binary version of
every later resumed process; the recognized event shape and conservative join
remain additional requirements. Installed `codex --version` alone is not a
version claim about old log records.

## Join and rejection rules

1. Parse request identity and output during the existing configured JSONL scan.
   Keep the existing performance identity: namespace `codex:response`, execution
   source `codex`, configured account and native response ID. Timing values,
   file offsets and polling time are not identity components.
2. Match a recognized completed log tag to that exact response ID. Recover the
   thread/turn from the native request observation; do not assume a completion
   log has its own thread context.
3. Require a unique compatible sampling start in the same process, thread and
   turn. A start with no native request ID is an inference, even if its scope
   and sequence appear unambiguous. Exclude competing starts, ambiguous
   completions, overlapping attempts and retry evidence rather than selecting
   the nearest timestamp.
4. Require a positive interval within **100 ms through 15 minutes**. Reject
   invalid clocks, reversed endpoints, stale/incomplete evidence and missing
   output. These are guards for this inferred adapter, not limits on other
   providers' verified direct clocks.
5. Emit the existing performance fields with `inferred_log_boundary`, model
   request scope and verified completion outcome. Never label this path
   `provider_request_timestamps`. Insufficient evidence keeps the request
   unmeasured and leaves accounting available.

The start occurs before all sampling setup has finished. Local preparation,
authentication recovery, transport fallback or event processing can be inside
the interval. Excluding an observable retry does not prove a pure single HTTP
attempt. A terminal usage record without a completed tag still has unknown
outcome. A completed tag without an acceptable start cannot supply a speed.

Do not substitute `thread_turns.duration_ms`, task duration, message/item
`started_at_ms`/`completed_at_ms`, command duration, HTTP handshake latency,
individual SSE event duration, previous user activity or file modification time.
These clocks have a different scope or lack a request-output join.

## Read scope, privacy and storage

The account's `provider_paths.codex_logs_db` is an optional explicit native
SQLite path. Without it, a configured source root named `sessions` or
`archived_sessions` can resolve only its parent's `logs_2.sqlite`. A custom root
with another basename requires the explicit path. There is no fallback from a
custom profile to the user's default Codex home.

The JSONL scan stays inside the configured `sessions_dir`. Reading a sibling
log DB does not authorize scanning its other sessions, archives or accounts.
The log DB must belong to that same approved profile. Exact IDs supplement
profile isolation; they do not replace it.

Open the source DB read-only, preserve committed WAL visibility and bound work
and waiting. Do not run migrations, checkpoints, integrity repair or sidecar
deletion on Codex's DB. Disabled performance does not open this timing source.
Missing, busy, locked, unsupported or inaccessible timing evidence does not
discard successfully parsed ordinary usage. The reader scans at most 8,192
database rows per pass, retains 16,384 allowlisted metadata events, accepts log
bodies up to 64 KiB and uses a two-second read deadline with a 100 ms SQLite
busy timeout. It tracks the row cursor, file identity and an anchor record to
detect rotation/replacement. A backlog remains unmeasured until the bounded
scan catches up. Observed row-ID gaps or an incomplete retained interval cannot
establish a unique start.

The enrichment cache retains the newest 8,192 request observations per
source-root/account/database scope. Older observations still reach the existing
metadata store, but this bounded cache cannot later enrich all of them. These
limits constrain historical coverage; they are not a new retention policy or
evidence that an unmeasured request had zero traffic.

Only numeric timing/output, request identity and allowlisted provenance belong
in performance metadata. Never copy `feedback_log_body`, HTTP headers, prompts,
response text, tool arguments, environment, authentication or arbitrary native
errors into OpenUsage events, diagnostics, snapshots or test artifacts. The
source path remains local-only. Error reporting uses bounded status codes.

This change uses the existing optional performance tables and read model. It
adds **no local schema migration**, accounting table, central history or Hub
protocol. Late log evidence enriches the same request without adding a second
usage row. Replays must not erase accepted timing with an older usage-only
observation; genuine newer corrections retain the performance merge policy.
The native usage revision orders output corrections, including downward ones;
replaying a usage-only record does not erase independently accepted timing.
Late DB records can enrich cached identities even if the last JSONL has since
moved out of the configured active directory. No archive is opened to do so.

Positive contradictions, such as competing starts or an observed authentication
retry, revoke an inference and remain excluded at the same usage revision.
Temporary ambiguity from a completed response whose usage scope has not yet
arrived can recover when later JSONL identifies that scope. A missing/locked or
rotated DB alone does not retract already accepted evidence. A late successful
completion without a start updates the outcome while retaining a specific
missing-clock reason; it does not manufacture a duration.

## Evidence and validation boundary

The initial read-only investigation found different coverage in active and
archived sessions and a mixture of producer versions on one host. Exploratory
sequence matches are **discovery candidates**, not accepted adapter coverage,
production measurements or a promise that enabling the feature will recover
those records. Version, retry, row-retention and ambiguity guards can reduce
coverage further. Actual source counts, rates, build SHAs and test run results
belong in private validation evidence, not mutable personal data in this design.

Synthetic and native-platform validation must establish:

- disabled versus enabled token/cost and canonical identity equality;
- exact completion-ID joins, profile/process isolation and stable replay;
- explicit zero, missing output, success/unknown/failed outcome distinctions;
- retries, multiple starts/completions, overlap, clock and version rejection;
- committed WAL visibility, busy/missing/permission/schema failure behavior;
- late log completion, log rotation/truncation and cache invalidation;
- Collector → existing store → snapshot → Exporter → existing Hub compatibility.

The executable cases below define the regression coverage. Their presence is
not a claim that a particular platform run or deployment has completed.

| Concern | Tests |
| --- | --- |
| Exact completion, inferred boundaries, zero and conservative exclusions | `codex.TestNativeTimingExactCompletionAndInferredSequence`, `TestNativeTimingConservativeRejections`, `TestNativeTimingBoundsAndConflictingUsage` |
| Profile isolation, WAL visibility, missing/locked/schema failure and privacy | `codex.TestNativeTimingSequentialAndProfileIsolation`, `TestNativeTimingReadOnlyMissingSchemaLockedAndCancellation`, `TestNativeTimingWALAndPrivacy` |
| Rotation, late/out-of-order evidence, bounded backfill and retention gaps | `codex.TestNativeTimingRotationAndInPlaceReplacement`, `TestNativeTimingOutOfOrderLateContradictionAndPartialRetention`, `TestNativeTimingBoundedIncrementalBackfill`, `TestNativeTimingMetadataCapacityAndUsageCapacity`, `TestNativeTimingPrunedInteriorRangeCannotProveUniqueStart` |
| Feature off, late arrival after archive, replay, output correction and provenance | `codex.TestCodexNativeTimingProfileResolutionAndFeatureOff`, `TestCodexNativeTimingLateDBReplayCorrectionAndRestart`, `TestCodexNativeTimingPendingAfterArchiveAndUnknownScopeRecovery`, `TestCodexNativeTimingContradictionDoesNotInventEvidence` |
| Temporary ambiguity recovery versus sticky positive contradiction | `codex.TestNativeTimingUnknownBarrierRecoversButObservedRetryDoesNot`, `telemetry.TestPerformanceUnmappedCompletionBarrierCanRecoverAfterIdentityArrives`, `TestPerformanceInferredRevocationIsStickyAndNeverReplacesDirect` |
| Stored outcome/clock diagnostics and unchanged canonical accounting | `telemetry.TestPerformanceLateFinalOutcomeAndCorrectionPreserveAccounting`, `TestPerformanceOutcomeEnrichmentRequiresIndependentFinalEvidence`, `TestPerformanceLateCompletionWithoutStartUpdatesMissingDiagnostic` |
| Parser → isolated store → Exporter → Hub, including delayed timing | `daemon.TestCodexNativePerformanceLateCompletionToHub` |
| Explicitly approved native sources, numeric-only output and isolated Hub | opt-in `daemon.TestPerformanceRealCodexReadOnlyToHub` |

Use private, explicitly approved read-only personal sources only for optional
real-data verification. Keep raw DBs and logs on their host. Record accepted
counts and exclusion reasons separately from exploratory candidates and never
publish request IDs or source content. Build and test evidence does not imply
deployment. See [operations](fork/codex-native-timing.md) for the rollout and
rollback boundary.
