# OpenUsage maintenance fork

This personal fork of [janekbaraniewski/openusage](https://github.com/janekbaraniewski/openusage)
keeps the Go implementation, module path, license and upstream history. Its initial
baseline is official **v0.26.1**, commit
`299e3dbffb5a6827bd89d9037ef8e483e5073bfa`.

The scope is OpenCode collection compatibility, safe local data handling,
reviewable upstream synchronization and native ARM64 artifacts. Existing
Collector/Exporter/Hub contracts remain the integration boundary. This fork does
not introduce a dashboard, Paseo plugin, central request history or token-speed
measurement.

## Branches and review

- `upstream` is the official repository; `origin` is `RunaticMoon/openusage`.
- `main` is an exact fast-forward mirror of upstream `main`, with no fork commits.
- `custom` is the fork's default integration branch, initially based on the
  compatible stable release above. It contains reviewed patches and automation.
- `fix/*` and `feat/*` hold changes for PR review against `custom`.
- `sync/upstream-<12-character SHA>` holds an upstream merge candidate. Keep its
  ancestry when merging; do not squash or rebase a synchronization PR.

No workflow merges PRs or deploys a binary. A prepared PR is not approval to merge.
Production Collector, Exporter, Hub, hooks and databases require separate
deployment approval. Keep private hosts, addresses, credentials and production
fixtures outside this public repository.

See [the patch registry](docs/fork/patches.md),
[synchronization procedure](docs/fork/upstream-sync.md),
[compatibility evidence](docs/fork/compatibility.md), and
[deployment/rollback procedure](docs/fork/operations.md).

## Native builds

`go.mod` currently declares Go 1.26.0 with toolchain Go 1.26.9. CGO is required by
`github.com/mattn/go-sqlite3`; a native C compiler and OS SDK are required. CI uses
GitHub-hosted `ubuntu-24.04-arm` and `macos-15` ARM64 VMs, never a personal Mac
self-hosted runner. Project tests and targeted regression
tests run with the race detector on both architectures.

From a clean, committed fork checkout on Linux or macOS ARM64:

```sh
GOMAXPROCS=2 go vet -p 1 ./...
GOMAXPROCS=2 go test -p 1 -race -count=1 ./...
bash scripts/fork/build.sh
```

The build script emits `dist/fork/<os>_arm64/openusage`, checksums, native linkage
information and JSON metadata. The version includes the fork SHA; metadata also
identifies the newest stable upstream tag already contained in the build SHA,
Go version, platform and patch registry checksum. Before building, the selected
tag's peeled commit is checked against the official upstream Git remote; fetch
upstream tags first, and allow read-only GitHub access for this verification.
After a stable release sync, the derived base and default fork version advance
automatically. A manual early-commit sync still names the latest included stable
release; its full fork SHA identifies the additional ancestry. The script runs
`--version` without installing a service. Record syncs in the patch registry. Artifacts are unsigned
unless a later documented signing process is added.

Fork release tags use **`fork/v<version>-p<N>`**, for example
`fork/v0.26.1-p1`. Never republish an upstream `v*` tag. `Fork draft release` is
manual-only, requires an exact SHA already in `custom`, re-runs native validation,
requires the protected `fork-release` environment's reviewer, and creates only a
draft GitHub release. The environment must have a required-reviewer rule; the
workflow checks this before building. Publishing the draft and deploying its
artifacts are separate manual operations.

Install approved builds into version-specific paths and configure service paths
explicitly as described in operations. Official `openusage` self-update and
Homebrew upgrade are not fork update mechanisms.

## Automation activation

Only `fork-ci.yaml`, `fork-upstream-sync.yaml` and `fork-release.yaml` are intended
to be active in the fork. Disable all inherited workflows at repository level,
including release, Pages, Dependabot auto-merge and branch-refresh workflows.
Do not modify `main` to accomplish this. A new upstream workflow stops automatic
mirroring until it is registered and disabled safely.

Initial implementation, repository settings, CI runs and scheduled execution are
separate verification steps. Scheduled workflows run from the default branch;
this feature cannot become scheduled on `custom` until the implementation PR is
approved and merged. Consult the sync document for exact setup and evidence.
