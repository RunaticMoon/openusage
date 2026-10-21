# Log response throughput

This optional feature measures output reported by local request logs. It does
not call a model, estimate usage from quota changes, or alter the existing
Work/Cached/Total/Cost/Quota accounting rules. Enablement, migration, deployment
and rollback are separate operational decisions. The feature is disabled by
default; adding this source code does not enable it on a machine.

## Definitions

For one successfully completed model request:

```
response_tok_s = output_tokens * 1000 / request_duration_ms
weighted_response_tok_s = sum(output_tokens) * 1000 / sum(request_duration_ms)
```

Both sums use exactly the same eligible samples. Input, cache, total tokens,
cost and reasoning are never additional numerator terms. A provider output
counter that already includes reasoning retains that documented basis; a
separate reasoning counter is not added. Reported output zero with valid time
is **0**. Missing output or timing is **null**, not zero. Request elapsed time
may include first-token latency, so this is response speed, not pure decode
speed or a measurement of first-token latency.

For example, 150 output tokens over 2 seconds is 75 tok/s. Requests producing
100 tokens in 1 second and 100 in 2 seconds have a weighted result of 66.667
tok/s, not the arithmetic mean of their speeds (75). Verified direct intervals
may be shorter than 100 ms or longer than 15 minutes. Only inferred boundaries
have the 100 ms–15 minute guard; invalid, inverted, nonfinite and ambiguous
intervals are excluded with a reason code.

`PerformanceRequest` records the native ID, namespace, source, account, host,
session, model and billing provider separately. Unknown attribution stays
unknown. `RecordedAt` is the source event time used for period filtering;
`ObservedAt` is meaningful observation/update provenance and is never used as
request start or historical event time. Optional local `SourcePath` is excluded
from JSON and stored only in the local metadata column. Evidence contains
compact adapter reason codes, not message/tool/prompt text.

Timing provenance is explicit:

| Field | Values |
| --- | --- |
| `timing_basis` | `provider_request_timestamps`, `provider_reported_duration`, `inferred_log_boundary`, `unknown` |
| `scope` | `model_request`, `turn`, `session`, `unknown` |
| `timing_status` | `valid`, `missing`, `invalid`, `ambiguous`, `unsupported`, `provisional` |
| `outcome` | `success`, `failed`, `unknown` |

Scope and outcome are independently verified. A user/task/tool event, filesystem
mtime, session duration, polling period or Hub receive time is not a request
boundary. Direct duration without a start may support request speed but cannot
support placement in a wall-clock window. Provider adapters must establish the
same-request relationship before constructing an eligible observation.

## Coverage and revisions

Candidates are unique final model requests, including failed and unknown
outcomes. Provisional requests are counted separately, outside that denominator.
Only valid successful candidates contribute to `Summary.Stats`. Direct and
inferred statistics and counts are also returned separately; combined results
can include inferred samples only with that coverage visible. A session/turn
aggregate is not another request and cannot double count its children.

The stable identity is the JSON-framed tuple `(namespace, source, account,
native request ID)`. Mutable model, session, output, duration, path and polling
time do not enter that identity. A copied hook/log/fork/resume observation of the
same native request replaces or enriches metadata. A native retry ID remains a
separate request. Higher native revisions may lower an earlier output counter;
output revisions and timing authority merge independently so weaker timing
cannot displace stronger timing. A delayed provisional observation does not
regress a final request. Session association is retained for copied native IDs.
Cross-account copies are intentionally isolated, and cross-host aggregation is
explicit because copies across machines may exist.

## Distribution

`log2_16_v1` is a fixed histogram with 1,024 bins over `[2^-32, 2^32)` tok/s,
16 bins per doubling. Buckets are sparse in JSON, but their indices and range
are fixed. Zero has a separate exact count. Underflow and overflow are counted
explicitly; a percentile landing outside the range is null instead of clamped.
Exact minimum and maximum remain available.

P50/P90 use nearest rank `ceil(q * n)` and the selected bin's geometric
midpoint. In-range relative value error is at most `2^(1/32)-1`, about 2.19%.
This is a value error bound, not a statistical confidence interval. Empty
samples yield null percentiles; zero-only samples yield zero. A single nonzero
sample's percentile may be approximate even though min/max are exact.

Merging sums compatible histogram counts and recomputes ranks. It never averages
host percentiles or host average speeds. Sample counts, output sums and integer
window allocations are bounded by `2^53-1`; invalid/inconsistent histograms or
aggregate overflow fail explicitly rather than returning an inaccurate value.

## Wall-clock windows

Request response speed and wall-clock throughput answer different questions.
Two parallel requests each producing 6,000 tokens over 60 seconds have a
weighted request response speed of 100 tok/s and a combined 60-second
wall-clock throughput of 200 tok/s.

The ring has 180 slots for 30 minutes (default) or 360 slots for 60 minutes.
Slots are aligned to 10-second UTC boundaries. The current slot can be partial;
no output is allocated after the explicit query clock. The latest 60-second
rate and each session's latest 120-second rate use their actual respective
wall-clock intervals, including idle time. They are not average request speeds
and are not interchangeable denominators.

For a request interval `[s,e)` and output N, cumulative allocated output at t is
`floor(N * clamp(t-s, 0, e-s) / (e-s))`. A bucket receives the difference of
cumulative values at its overlap boundaries. Adjacent differences telescope to
exactly N for the full request, even when N does not divide evenly across
buckets. Clipping an old request to the current window preserves only its
proportional share. Integer arithmetic avoids overflow/rounding loss. This is a
**time-allocation estimate**, not observed streaming token timestamps. Output
is never dumped wholesale into the request's ending bucket.

Late records and corrections replace previous contributions using stable ID.
Provisional output stays in a separate field and never enters final request
statistics or final wall-clock output. Missing starts are excluded from bucket
placement. Recent missing/ambiguous/unsupported observations mark coverage
partial, so a fresh source heartbeat alone cannot turn unobservable activity
into idle zero. An empty scan without any measured successful request in the
bounded ring remains unknown; a measured prior request plus complete fresh
coverage can establish idle zero. Once all such evidence ages out, the window
conservatively returns unknown again. Old unsupported records outside the live window do not distort
its coverage. Observed valid output zero remains zero even when other activity
is unobservable; the partial-coverage flag remains visible.

`DataAsOf` is the last successful existing source scan, not request completion
or network/log-arrival latency. `GeneratedAt` is aggregation time. The first
successful scan bounds known coverage. A snapshot is stale at
`max(120 seconds, 2 * export_interval)` without a successful scan, or when its
heartbeat is in the future. A request endpoint more than 10 seconds ahead of
the query clock is excluded as clock skew. Partial observed throughput may be
shown with its coverage label; unknown remains null and stale live rates are
not presented as current activity.

## Pure API and bounds

The `internal/performance` package has no filesystem, database, network or
model-call behavior. The caller supplies clocks and already collected records.

```go
NormalizeRequest(record core.PerformanceRequest) core.PerformanceRequest
PreferRequest(previous, incoming core.PerformanceRequest) core.PerformanceRequest
RequestKey(record core.PerformanceRequest) string
Build(records []core.PerformanceRequest, opts BuildOptions) (*core.PerformanceSnapshot, error)
Merge(now time.Time, snapshots ...*core.PerformanceSnapshot) (*core.PerformanceSnapshot, error)
NewRing(bufferMinutes int) (*Ring, error)
(*Ring).Upsert(record core.PerformanceRequest) error
(*Ring).Remove(record core.PerformanceRequest)
(*Ring).Advance(now time.Time) error
(*Ring).Snapshot(opts BuildOptions) (*core.PerformanceSnapshot, error)
```

`BuildOptions` supplies `Period`, `FilterID`, `HostID`, `QueryPath`, `Now`,
`DataAsOf`, `CoverageStart`, `BufferMinutes`, `ExportIntervalMS`, `RequestLimit`,
`GroupLimit` and `IncludeWindow`. The period is half-open `[start,end)`. A build
accepts at most 100,000 records and one host. Default detail bounds are 100
requests and 128 groups, each configurable up to 1,000. Truncation flags apply
to returned details; statistics still cover the complete accepted record set.
Group dimensions are session, model, account, billing provider, source and host.
Window session detail is capped at 1,000 with an explicit exclusion reason.

The ring retains a bounded map of active observations and a fixed slot array.
A snapshot rebuilds that bounded projection, ensuring arbitrary late/corrected
contributions cannot leave stale deltas behind. Allocation visits only
intersecting buckets. `Advance` evicts expired records and rejects a backwards
clock; callers can rebuild their projection when changing clocks. Historical
period statistics are built from retained local records, not from expired ring
slots. The read model reuses database change sequences and a bounded scope
cache rather than rescanning source logs for each display refresh.

`Merge` requires the same definition, filter and exact period, distinct nonempty
host IDs, and compatible window availability/alignment. It recomputes freshness
at the supplied query time, excludes stale hosts and lists them in
`ExcludedHosts`. Compatible histogram counters and numerator/denominator sums
are merged; host groups remain separate. Raw request details are not combined
into central history. The default remains one machine per snapshot.

## Storage and compatibility

Performance metadata uses opt-in additive local SQLite tables. It has no
independent permanent history policy: pruning follows the configured existing
local retention horizon. The ring is transient and is not a retained event log.
Disabling collection leaves metadata available for a later explicit choice;
rollback does not require a destructive down migration. Operational backup must
include SQLite consistently, using the approved existing backup procedure,
rather than copying a live main database without its WAL state.

`UsageSnapshot.performance` is optional and fully deep-cloned, including
histogram maps, request pointers, groups and windows. Old collectors continue
to omit it. An updated Hub preserves it in its latest-snapshot behavior. An
unmodified v0.26.1 Hub discards unknown JSON fields when decoding/reserializing,
so deploying only the collector does not make performance visible through that
old Hub. No central history or Hub TTL change is introduced.

Current adapter coverage is deliberately unequal: OpenCode's matching native
message timestamps can be direct; uniquely matched Claude request boundaries
are inferred and subject to guards; Codex records without a verified per-request
start remain unmeasured and may have unknown outcome. Other providers are not
silently assigned speeds. Local log availability and source schema determine
coverage; these measurements are not a claim of complete historical coverage.

## Regression fixture map

These are source-test references, not a claim that live production was enabled.
The overall PR validation records which suites and real isolated replay ran.

| Required case | Test location / principal fixture |
| --- | --- |
| 1. 75 tok/s | `performance/TestNormalizeResponseSpeed`, `telemetry/TestPerformanceReplayCorrectionDelayedTimingAndAccountingInvariant` |
| 2. Weighted 66.667, not 75 | `performance/TestWeightedCoverageAndOutputOnly` |
| 3. Null, zero, invalid and direct interval limits | `performance/TestNormalizeResponseSpeed` |
| 4. Output basis without reasoning addition | `performance/TestWeightedCoverageAndOutputOnly`, OpenCode `TestV2PerformanceOutputOnlyAndAccountingInvariant` |
| 5. Parallel request100 / wall200 | `performance/TestWindowParallelConservationAndZero` |
| 6. Ten stable replays | `telemetry/TestPerformanceReplayCorrectionDelayedTimingAndAccountingInvariant` |
| 7. Partial/final/correction/late timing | `performance/TestRingReplaceLateProvisionalEviction`, telemetry replay/correction fixture |
| 8. Hook/log/fork/retry/parent isolation | `telemetry/TestPerformanceIdentityIsolationReopenAndTimingAuthority`, `TestPerformanceFailureMissingZeroAndSensitivePayloadExcluded` |
| 9. Ambiguous parallel boundaries | Claude `TestClaudePerformanceAmbiguityMissingZeroAndGuardrails` |
| 10. Partial writes, rotation, restart and sleep | Claude `TestClaudePerformanceIncrementalPartialRotationAndFlagCache`, Claude guardrails, telemetry reopen fixture |
| 11. Bucket conservation and late replacement | `performance/TestWindowOverlapRoundingAndNoStart`, ring replacement fixture |
| 12. Empty/small/merged histogram | `performance/TestHistogramMergeAndPercentiles` |
| 13. Old/new JSON and clone ownership | core `TestPerformanceOptionalJSONAndDeepClone`, Hub/exporter performance tests |
| 14. Staleness, coverage and clock skew | `performance/TestWindowStaleClockSkewAndBounds`, `TestRecentUnknownCoverageIsNotIdleZero`, `TestMergeExpiresCachedHostsAtQueryTime` |
| 15. Disabled/enabled accounting equality | telemetry replay fixture and provider accounting-invariance fixtures |
| 16. Migration failure, rollback and backfill | `telemetry/TestPerformanceOptInMigrationRollbackBackfillAndRetention`, `TestPerformanceMigrationBusyPreservesAndRetries` |
| 17. Account/path isolation and no secrets | telemetry identity/sensitive-payload fixtures, provider path fixtures, core JSON privacy test |
