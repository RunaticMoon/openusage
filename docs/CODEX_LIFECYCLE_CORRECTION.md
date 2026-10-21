# Codex response and turn lifecycle correction

This corrects the original native timing implementation's claim that a
`last_model_response_id` feedback tag proves a successful response. It does not.
The audit uses the immutable Codex 0.163.0 source at
[`83086188ae0614bd882818ff1aa23f3194962012`](https://github.com/openai/codex/tree/83086188ae0614bd882818ff1aa23f3194962012), reviewed on 2026-10-21.
The initial PR #14 success classification must not be used as the producer contract.

## A native response terminal is not a success assertion

The native Responses decoder handles both `response.completed` and
`response.incomplete` in one branch. An incomplete response with reason
`interrupted` becomes the internal `ResponseEvent::Completed`, with
`end_turn: Some(false)`. Other incomplete reasons become errors. This is explicit
in [`sse/responses.rs:419–470`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/codex-api/src/sse/responses.rs#L419).

The shared mapper then emits `last_model_response_id` for that internal variant,
without preserving its original wire event type. It also calls
`sse_event_completed` and `record_completed` for the same variant
([`client.rs:2572–2600`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/client.rs#L2572)).
Consequently, the synthetic OTel `response.completed` label and the rollout trace
`InferenceCompleted` name are not independent success evidence.

The trace response summary stores response identity, token usage, and output
items, but neither original terminal type nor `end_turn`
([`inference.rs:80–92`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/rollout-trace/src/inference.rs#L80)).
Its completion method receives neither of those fields
([`inference.rs:199–232`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/rollout-trace/src/inference.rs#L199)).
The normal websocket event instrumentation records kind/count/duration without
an exact response identity, so it cannot safely repair the join
([`session_telemetry.rs:946–1003`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/otel/src/events/session_telemetry.rs#L946)).

Even `token_usage_record` is not a success signal: incomplete-response accounting
and normal internal completion both call `record_response_token_usage`, which
persists that record
([`session/mod.rs:4949–4961`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/mod.rs#L4949),
[`session/mod.rs:4991–5032`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/mod.rs#L4991)).

The corrected native reader can retain an inferred start-to-terminal interval
when its timing guards pass. With these sources alone, the individual response
outcome remains **unknown**. Unknown responses are excluded from the successful
response speed average; token accounting remains intact. An eventual successful
turn does not retroactively make each response in it successful, and an aborted
turn does not retroactively fail its earlier responses.

## A turn terminal can clear an old logical sampler

The normal task wrapper awaits `task.run` before invoking `on_task_finished`
([`tasks/mod.rs:371–404`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/tasks/mod.rs#L371)).
That handler constructs and emits `TurnComplete` after the task has returned
([`tasks/mod.rs:829–866`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/tasks/mod.rs#L829)).
`TurnComplete` can also carry a terminal error, so it is lifecycle evidence, not
a blanket success flag
([`protocol.rs:2266–2288`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/protocol/src/protocol.rs#L2266)).

The abort path cancels the task token, waits for completion or a 100 ms grace
timeout, requests task abortion, invokes cleanup, and then emits `TurnAborted`
([`tasks/mod.rs:930–1029`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/tasks/mod.rs#L930)).
It does not await a remote model completion. The terminal therefore establishes
local logical abandonment only. The sampling loop observes that cancellation
and exits with `TurnAborted`
([`session/turn.rs:2730–2764`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/turn.rs#L2730)).

Only explicit, reviewed-version `task_complete`, `turn_complete`, and
`turn_aborted` records from the same configured native JSONL scope are accepted.
They must have a valid native thread identity, explicit turn identity, and valid
record timestamp. The timestamp is used only if:

```
old sampling start < matching turn terminal < later sampling start
```

The native header's `id` identifies the actual thread; its separate `session_id`
identifies the root thread and can differ for a child
([`protocol.rs:3252–3254`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/protocol/src/protocol.rs#L3252)).
Lifecycle scope uses `id` only, preserving the pre-existing accounting session
association separately. No root-session alias is substituted for a missing
thread identity.

It is never a model-request start/end, duration, output-token count, or outcome.
The matcher does not cache lifecycle input across callers. Its caller must keep
account/profile scopes separate; oversized lifecycle input fails closed.

## Recoverable missing closure versus observed overlap

- `ambiguous_parallel_sampling`: an earlier different turn in the same native
  thread/process has no observed closure. This is recoverable when an exact
  lifecycle record arrives later. No age cutoff silently declares it finished.
- `ambiguous_overlapping_sampling`: a different turn starts inside the candidate
  interval in that same thread/process. This remains a positive contradiction;
  a terminal after the overlap cannot erase it.
- Retry, duplicate response, conflicting identity, partial retention, unsupported
  version, missing start, and duration guards remain in force. A closure cannot
  bypass those guards or manufacture an interval.

The reviewed version gate uses the persisted session creation version. It does
not independently attest the executable used for every subsequent resumed
write. This correction does not enable new tracing, depend on external timing
plugins, alter Codex, or change production services by itself.
