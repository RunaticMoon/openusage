# Maintenance patch registry

Base: official v0.26.1,
`299e3dbffb5a6827bd89d9037ef8e483e5073bfa`. Entries identify development commits;
the approved integration SHA and resulting release metadata are the deployment
identity. None of these rows implies merge approval or production installation.

| Patch | Purpose / source | Development commit(s) | Verification and upstream status |
| --- | --- | --- | --- |
| Store-open safety | Preserve existing telemetry data when opening/migrating a store fails. [Upstream Issue #423](https://github.com/janekbaraniewski/openusage/issues/423) is investigation context, not proof that every DB error is corruption. | `dc985cc` · [fork PR #1](https://github.com/RunaticMoon/openusage/pull/1) | Regression coverage for transient/permission/open failures and corruption behavior; inspect exact PR CI. Fork change pending review; no production DB migration is introduced. |
| OpenCode SQLite compatibility | Inspect and support actual old/v2 structures without changing the source DB; distinguish unsupported schema and absent usage, retain partial-field semantics. | `2c93d61`, `188e95d`, `90b8fda` · [fork PR #2](https://github.com/RunaticMoon/openusage/pull/2) | Synthetic schema/WAL/usage fixtures plus read-only installed v2.1.1 source comparison and canonical replay. No original DB or raw usage totals in Git. Check upstream diff at each sync. |
| Mutable-message reconciliation | Collect later revisions and preserve stable request identity instead of missing final values or double counting Hook/DB replay. | `4e02258` · [fork PR #3](https://github.com/RunaticMoon/openusage/pull/3) | Updated-row, partial/final and replay regression coverage; integrate with SQLite/plugin patches and rerun the full suite. |
| OpenCode plugin lifecycle | Restore current factory/lifecycle contract and usage delivery while preserving older event forms and non-blocking failure behavior. [Upstream PR #421](https://github.com/janekbaraniewski/openusage/pull/421) was reviewed as context; the fork does not assume the whole PR should be imported. | `0b3785b`, `70085c8` · [fork PR #5](https://github.com/RunaticMoon/openusage/pull/5) | Rendered plugin execution/contract tests, including numeric-field corrections; inspect exact PR CI before recording completion. Existing operational hook files remain unchanged. |
| Fork automation/provenance | Exact main mirror, review-only sync PRs, isolated exact-SHA ARM64 CI, identifiable artifacts and approved manual draft release. | `4e4c62a`…`f04e196` · [fork PR #4](https://github.com/RunaticMoon/openusage/pull/4), approved merge `8b024fb` | Synthetic Git safety/retry tests, workflow lint, native Linux build helper and real read-only sync audit. Workflows are now present on custom; actual manual-run results are separate evidence. No auto-merge/deploy. |
| Collection diagnostics | Expose additive collection/freshness diagnostics without replacing existing JSON usage fields. | `abade98` · [fork PR #6](https://github.com/RunaticMoon/openusage/pull/6) | Inspect its exact JSON compatibility and source-state tests; diagnostics do not turn process liveness into successful usage collection. |
| OpenCode source/account projection | Correct the snapshot read filter when the execution source is OpenCode but the billing provider is a vendor such as Anthropic. | Follow-up patch in progress; record its reviewed commit/PR when available | Canonical ingestion/replay passing does not prove OpenCode tile totals. Snapshot/Hub projection validation remains pending this fix. |
| Operations and documentation | Private consistent SQLite backups, explicit staging/canary/rollback and accurate Hub/provider boundaries. | This documentation/operations PR | Synthetic filesystem/WAL/rollback tests; services mocked. Actual service replacement is separately approved. |

Any later integration corrections must be added with their actual commit/PR
and JSON compatibility evidence when ready. Do not mark a planned field as
already delivered. Native CI links
and the final integration SHA are recorded in the associated PRs rather than
hardcoding a transient green status into this file.

No external commit has been silently represented as an original fork commit.
If a future change cherry-picks upstream code, use `cherry-pick -x` or preserve
the original author/SHA explicitly here. Upstream may supersede these patches;
compare both behavior and the resulting diff before removing fork changes.
