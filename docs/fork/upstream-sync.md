# Upstream synchronization and CI

## Behavior

The daily schedule is **03:23 UTC / 12:23 Asia/Seoul**, with manual dispatch as an
alternative. GitHub can delay scheduled runs and disable inactive schedules;
this is a check cadence, not a deployment promise. Workflows run from default
branch `custom` and are guarded to `RunaticMoon/openusage`.

`scripts/fork/upstream_sync.py` defaults to a read-only dry run. It clones into a
temporary directory, verifies repository identity/default branch, audits workflow
states, fetches upstream `main` and tags, and selects the latest non-prerelease
GitHub release. `--candidate-sha` accepts only a full upstream SHA reachable from
the observed upstream `main` for an explicitly selected early fix.

With `--apply`, it advances `origin/main` only by normal fast-forward push and
verifies the exact remote SHA. Unexpected divergence or upstream rewritten
history stops the operation. A candidate already included in `custom` is a
successful no-op. Otherwise it merges the candidate into a separate sync branch
and creates/updates one draft PR against `custom`. Conflict paths are reported;
no conflicted branch is pushed. Dependencies, collection/schema, Hub/API and
workflow changes are highlighted by path, and patch-equivalent custom commits
are listed for human comparison with the patch registry. This analysis does not
prove semantic equivalence or schema compatibility.

The PR records upstream SHA, custom base, previous candidate SHA and new
candidate SHA. The next run only updates a branch whose tip matches that record,
using a normal merge and fast-forward push. Extra human commits, a closed PR,
removed branch or unexpected record stop updates without deleting anything.
A push-success/PR-create-failure can be retried only when the branch still has
the exact two-parent, bot-authored merge and its tree equals Git's expected merge
tree. If the PR body update fails after a later push, the mismatch intentionally
requires review; do not edit the marker blindly to bypass it.

Do not squash sync PRs. After upstream adopts a patch, compare behavior and
remove only redundant fork differences. Do not mechanically revert a patch that
upstream now implements.

## CI and the exact candidate SHA

GitHub's current documented behavior suppresses ordinary workflows triggered by
`GITHUB_TOKEN` pushes. `opened`, `synchronize` and `reopened` PR events created by
that token produce approval-required runs; `workflow_dispatch` and
`repository_dispatch` are exceptions. See [GitHub's triggering documentation](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow).

Sync therefore calls the trusted reusable `fork-ci.yaml` directly, passing the
exact candidate SHA. Read-only native ARM64 jobs verify the checkout SHA before
format/module/vet/race tests, native build, version execution and artifact
upload. No operational credentials or extra PAT/App secrets are provided. A
separate job that does not check out or execute candidate code records
`fork-sync/verified` on that same SHA after checking the current PR head. The
reusable run's native checks alone belong to the caller's workflow context; the
explicit commit status is what connects sync results to the candidate SHA.

Ordinary PR CI checks out `pull_request.head.sha` explicitly. CI artifacts include
the SHA in their name and metadata. Branch protection should require the two
native `Fork CI` checks for ordinary patch PRs, and review the exact
`fork-sync/verified` status for automated sync PRs. Do not configure a universal
required status that only one of these paths produces without testing both PR
types. Repository policy must keep merge commits enabled for sync PRs.

The native labels are documented in [GitHub-hosted runner specifications](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).

## Repository setup and audit

Repository administration is separate from source code. Verify these through
GitHub settings/API before saying automation is active:

1. Default branch is `custom`; `main` SHA equals observed upstream `main`.
2. Disable every inherited workflow using its workflow ID, then verify state
   `disabled_manually`. This includes CI/security/comment workflows as well as
   publish, auto-merge, release and site deployment. The only allowed active
   paths are the three `fork-*` workflows above.
3. Enable repository Actions after inherited workflows have been disabled. Keep
   default token permissions read-only; allow Actions to create PRs. Write
   permissions are explicitly scoped to sync preparation and result reporting.
4. Configure branch rules with appropriate write restrictions/review checks;
   preserve fast-forward mirror updates on `main` and ancestry-preserving merge
   on `custom`. Never bypass a rejected branch rule to make sync appear healthy.
5. For manual draft releases, configure `fork-release` environment with required
   reviewers. Do not copy upstream publishing secrets or operational credentials.
6. After approval and merge to `custom`, run a manual dry run, then an apply
   invocation, inspect its run/PR/status SHA and verify the schedule is enabled.

Useful read-only commands:

```sh
gh api repos/RunaticMoon/openusage --jq '{default_branch,allow_merge_commit,allow_auto_merge}'
gh api repos/RunaticMoon/openusage/actions/permissions
gh api repos/RunaticMoon/openusage/actions/permissions/workflow
gh api repos/RunaticMoon/openusage/actions/workflows --paginate --jq '.workflows[] | {path,state}'
gh api repos/RunaticMoon/openusage/rulesets
gh api repos/RunaticMoon/openusage/branches/custom/protection
python3 scripts/fork/upstream_sync.py --dry-run
```

After the workflow exists on `custom`:

```sh
gh workflow run fork-upstream-sync.yaml -R RunaticMoon/openusage --ref custom -f apply=false
gh workflow run fork-upstream-sync.yaml -R RunaticMoon/openusage --ref custom -f apply=true
# An intentional early fix; replace the placeholder with a full observed SHA:
gh workflow run fork-upstream-sync.yaml -R RunaticMoon/openusage --ref custom \
  -f apply=true -f candidate_sha=FULL_UPSTREAM_COMMIT_SHA
```

An authorized maintainer may run the same script locally with `--apply`; it
creates review candidates only. A local invocation does not perform CI itself.
Inspect the resulting PR and run CI against its head explicitly. The script does
not alter the caller's worktree, service, database or operating configuration.

## New inherited workflows and failures

The workflow audit checks all upstream workflow paths against repository-level
disabled states before advancing `main`. Unknown workflow paths and collisions
with trusted `fork-*` names stop sync. For a new inherited path, temporarily
disable repository Actions, review/import the upstream workflow so GitHub can
register it, disable that workflow by ID, verify all inherited states, then
re-enable only the fork workflows. Never advance unreviewed publishing workflows
while repository Actions are active. A renamed workflow is a new path.

On a conflict or marker mismatch, preserve the branch and review it manually.
On GitHub permission/ruleset failures, report the exact blocked action and adjust
authorized settings; do not force push or auto-merge. On a failed validation,
`fork-sync/verified` remains failed/pending until the exact candidate is tested
again. No failure path installs or restarts a service.

## Initial activation evidence

The implementation PR supplies source and synthetic Git regression tests. At
initial fork creation the default branch is `custom` at v0.26.1 and repository
Actions are globally disabled for setup. The source files alone are not proof
of activation. Record settings audit, manual/daily run URL, candidate PR URL and
tested SHA in the PR/operational handover after they have actually been observed.
Before merge approval, daily scheduling remains pending because these workflows
have not yet reached `custom`.
