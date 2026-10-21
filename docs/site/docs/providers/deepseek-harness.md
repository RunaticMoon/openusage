---
title: DeepSeek Harness
description: Local DSH session usage and optional response-speed coverage.
sidebar_label: DeepSeek Harness
---

# DeepSeek Harness

The `dsh` provider reads existing DeepSeek Harness v4 session logs. It tracks
provider-reported input, output, cache and reasoning counters without invoking
DSH or making model requests. The separate `deepseek` provider reads API balance.

Set `provider_paths.sessions_dir` to the approved persistence root for each
account. The default is `$DSH_HOME/sessions`, or `~/.dsh/sessions` when unset.
Only the newest canonical v4 log in each session directory is counted, with
inherited history excluded and native retry/replacement accounting preserved.
Reasoning is part of output and is not added to total tokens a second time.

## Response speed

When performance collection is enabled, a request with a recorded
`request/header` and a successful terminal stream `finish` can show inferred
response speed. The interval begins before dispatch and includes the wait for
the first output. It is not pure generation speed.

Headers are recorded conditionally, so some calls have token usage without a
speed measurement. The collector never substitutes the first chunk, a turn
timestamp or a previous request's header. Failed/interrupted calls, uncertain
replacements, invalid clocks and compaction summaries remain unmeasured.
Missing speed means unavailable timing, not zero output.

The option enriches the existing accounting pipeline and can backfill retained
logs. Existing token IDs, counters, costs and quota behavior are unchanged.
Performance metadata follows the normal local retention and Hub snapshots;
the collector does not send transcript text, tool arguments or credentials.
