# Performance reference and provenance audit

Retrieved/rechecked on **2026-10-21 (KST)**. These are the exact inspected
references, not moving branch guarantees. Reference repositories were read
separately; no plugin was installed and no personal log, credential or provider
configuration was copied into this repository.

| Reference | Inspected revision / scope |
| --- | --- |
| Official OpenUsage | [main `430a1d39814d458e972cf4dce98135d6e786992c`](https://github.com/janekbaraniewski/openusage/tree/430a1d39814d458e972cf4dce98135d6e786992c); latest stable at retrieval was [v0.26.1](https://github.com/janekbaraniewski/openusage/releases/tag/v0.26.1), based on `299e3dbffb5a6827bd89d9037ef8e483e5073bfa`. |
| Maintenance fork starting point | `1fc969cb2dcdcbb830a10d3301db72426b830bda`; the associated PR and release metadata record this feature's final reviewed revision. |
| Agent Observatory | [zhjiang22/paseo-plugins `9c15e82a7e15f00cf784440595e0940910f40ec8`](https://github.com/zhjiang22/paseo-plugins/tree/9c15e82a7e15f00cf784440595e0940910f40ec8/agent-observatory), package version 2.17.0. |
| Official Codex | [rust-v0.163.0 commit `83086188ae0614bd882818ff1aa23f3194962012`](https://github.com/openai/codex/tree/83086188ae0614bd882818ff1aa23f3194962012); the annotated tag object was `129b8ec7ee6f4511557a4856279f529025c25af4`. |

## Agent Observatory scope and license

The audit inspected `agent-observatory/server/tokens/parser.ts`, `data.ts`,
`live.ts`, `shared/tokens.ts`, and the ingestion/live/correction/Grok-Pi/snapshot/
persistence token tests at the revision above. Useful reference concepts were
paired usage-event deduplication, weighted timed-output aggregation, bounded
histograms, integer-conserving window allocation and correction regressions.

The subtree's [LICENSE](https://github.com/zhjiang22/paseo-plugins/blob/9c15e82a7e15f00cf784440595e0940910f40ec8/agent-observatory/LICENSE)
and package metadata explicitly identify **MIT**, copyright 2026 Agent
Observatory contributors. This is the subtree evidence; absence of repository
root license metadata was not treated as permission or as a conflicting
license. Copying substantial implementation would require retaining its
copyright and MIT permission/license text.

The subtree [NOTICE](https://github.com/zhjiang22/paseo-plugins/blob/9c15e82a7e15f00cf784440595e0940910f40ec8/agent-observatory/NOTICE)
also credits MIT-licensed scheduled-run, recap, suggested-reply and cache-clock
adaptations. It does not separately identify the audited token parser/data/live
files as those adaptations. This work imports neither those components nor
their implementation.

The fork implementation is independent Go code. No Agent Observatory
implementation was copied or translated line by line. Mathematical definitions
and test scenarios informed review, while the fork's own contracts, storage,
identity policy and tests determine behavior. The existing OpenUsage LICENSE
remains in place. Any future actual code adaptation needs a fresh provenance
review and the applicable notices retained with it.

In particular, the reference's broad user/task/tool-boundary start heuristics
and completion-bucket fallback for unknown starts were **not adopted**. They
cannot prove request start or observed streaming throughput. This fork requires
verified source evidence, excludes untimed output from rate numerators, keeps
unknown values null, and labels interval allocation as an estimate. Its privacy
schema also excludes source paths from snapshots rather than importing the
reference's broader workspace/title/path fields.

## Relevant upstream status at retrieval

- [OpenUsage PR #421](https://github.com/janekbaraniewski/openusage/pull/421)
  remained **open and unmerged**, head
  `f8b270bf957eecf1769577ddcf7b4a0257fc1077`; the API showed no issue comments
  or inline review comments. It concerns OpenCode plugin compatibility, not
  verified request timing or this response-speed contract.
- [OpenUsage Issue #423](https://github.com/janekbaraniewski/openusage/issues/423)
  remained **open**, with no comments. It concerns store-open/quarantine safety,
  not response speed. The timeline's link to this maintenance fork is not an
  official merged fix. Preserve the fork's DB-safety behavior during migration
  and do not infer permission to replace a locked or failed-to-open DB.

Neither item supplies an upstream implementation of this feature. Their state
must be rechecked at future upstream synchronization; this dated audit does not
reserve their numbers as permanent open issues.

Codex's official implementation was also checked beyond field comments:
[`record_incomplete_response_usage`](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/core/src/session/mod.rs#L4948)
uses the same accounting writer as successful responses, and the
[rollout policy](https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/rollout/src/policy.rs#L160)
does not persist the transient completion event in ordinary rollouts. A
`token_usage_record` therefore does not independently prove successful request
completion or timing. The adapter preserves unknown outcome and unsupported
timing instead of assigning task/turn duration to a response.

See [design](../PERFORMANCE_DESIGN.md),
[provider contracts](../PERFORMANCE_PROVIDERS.md) and
[operations/rollback](performance-operations.md) for the implemented behavior
and separately recorded validation/deployment evidence.
