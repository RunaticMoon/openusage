#!/usr/bin/env python3
"""Fail-closed upstream mirror and review PR. No target code is executed.

Defaults to a read-only dry run. All Git operations use a temporary clone;
--apply permits only fast-forward main/sync pushes and fork PR create/update.
"""
import argparse
import json
import os
import re
import subprocess
import tempfile

UPSTREAM = "janekbaraniewski/openusage"
FORK = "RunaticMoon/openusage"
OWN_WORKFLOWS = {
    ".github/workflows/fork-ci.yaml",
    ".github/workflows/fork-upstream-sync.yaml",
    ".github/workflows/fork-release.yaml",
}
MARKER = re.compile(r"<!-- openusage-sync target=([0-9a-f]{40}) head=([0-9a-f]{40}) base=([0-9a-f]{40}) -->")


def run(*args, cwd=None, check=True, data=None):
    result = subprocess.run(args, cwd=cwd, input=data, text=True, capture_output=True)
    if check and result.returncode:
        raise RuntimeError(f"{args[0]} {args[1]} failed: {result.stderr.strip()}")
    return result


def api(path, method="GET", payload=None):
    args = ["gh", "api", "--method", method, path]
    if payload is not None:
        args += ["--input", "-"]
    output = run(*args, data=json.dumps(payload) if payload is not None else None).stdout
    return json.loads(output) if output.strip() else None


def paged(path, key=None):
    values = []
    for page in range(1, 100):
        data = api(path + ("&" if "?" in path else "?") + f"per_page=100&page={page}")
        batch = data[key] if key else data
        values.extend(batch)
        if len(batch) < 100:
            return values
    raise RuntimeError("API pagination limit reached; refusing an incomplete audit")


class Git:
    def __init__(self, path):
        self.path = path

    def call(self, *args, check=True):
        return run("git", *args, cwd=self.path, check=check)

    def text(self, *args):
        return self.call(*args).stdout.strip()

    def ancestor(self, old, new):
        code = self.call("merge-base", "--is-ancestor", old, new, check=False).returncode
        if code not in (0, 1):
            raise RuntimeError("Could not compare Git ancestry")
        return code == 0

    def mirror(self, old, new, apply):
        if not self.ancestor(old, new):
            raise RuntimeError("origin/main diverged from upstream/main; never force-push")
        if apply:
            self.call("push", "origin", f"{new}:refs/heads/main")
            actual = self.text("ls-remote", "origin", "refs/heads/main").split()[0]
            if actual != new:
                raise RuntimeError("origin/main SHA does not match the observed upstream/main")


def audit_workflows(git, upstream_sha, workflows):
    paths = set(git.text("ls-tree", "-r", "--name-only", upstream_sha, ".github/workflows").splitlines())
    paths = {p for p in paths if p.endswith((".yaml", ".yml"))}
    if paths & OWN_WORKFLOWS:
        raise RuntimeError("Upstream workflow collides with a trusted fork workflow name")
    states = {w["path"]: w["state"] for w in workflows}
    unsafe = sorted(p for p in paths if states.get(p) != "disabled_manually")
    unsafe += sorted(p for p, state in states.items() if p not in OWN_WORKFLOWS and state != "disabled_manually")
    if unsafe:
        raise RuntimeError("Inherited/new workflows must be disabled at repository level before mirroring: " + ", ".join(sorted(set(unsafe))))


def existing_head(pr, target, actual):
    match = MARKER.search(pr.get("body") or "")
    if not match or match[1] != target or match[2] != actual:
        raise RuntimeError("Sync branch changed outside the recorded automation head; preserve human commits")
    if pr["state"] != "open":
        raise RuntimeError("Sync PR is closed; preserve the maintainer's decision")
    return match[3]


def recover_unrecorded(git, target, head, current_base):
    """Recover push-success/PR-create-failure only for the exact original merge.

    Never write over a branch without a PR record. A changed tree, extra commit,
    different author or different parents requires manual review instead.
    """
    parents = git.text("show", "-s", "--format=%P", head).split()
    if len(parents) != 2 or parents[1] != target or not git.ancestor(parents[0], current_base):
        raise RuntimeError("Unrecorded sync branch has changed; preserve it for manual review")
    if git.text("show", "-s", "--format=%ae", head) != "41898282+github-actions[bot]@users.noreply.github.com":
        raise RuntimeError("Unrecorded sync branch is not an automation merge; preserve it")
    expected = git.text("merge-tree", "--write-tree", parents[0], target).splitlines()[0]
    if expected != git.text("rev-parse", f"{head}^{{tree}}"):
        raise RuntimeError("Unrecorded sync merge content changed; preserve it")
    return parents[0]


def merge(git, target, message):
    result = git.call("merge", "--no-ff", "--no-edit", "-m", message, target, check=False)
    if result.returncode:
        files = git.text("diff", "--name-only", "--diff-filter=U")
        raise RuntimeError("Sync conflict; no candidate branch was pushed. Resolve manually: " + files.replace("\n", ", "))


def body(git, target, head, base, label, previous="none"):
    files = git.text("diff", "--name-only", f"{base}...{target}").splitlines()
    groups = {
        "Dependencies/toolchain": [p for p in files if p in ("go.mod", "go.sum")],
        "Schema/collection": [p for p in files if any(s in p for s in ("telemetry", "opencode", "sqlite", "migration"))],
        "Hub/API": [p for p in files if any(s in p for s in ("hub", "export", "core/"))],
        "Workflows": [p for p in files if p.startswith(".github/workflows/")],
    }
    # - indicates patch-equivalent commits already upstream; reviewers decide removals.
    equivalents = git.text("cherry", target, base)
    equivalent_shas = [line[2:] for line in equivalents.splitlines() if line.startswith("- ")]
    lines = [f"Review upstream **{label}** without deploying it.", "",
             f"Upstream target: `{target}`", f"Custom base: `{base}`",
             f"Previous candidate SHA: `{previous}`", f"New candidate SHA: `{head}`", "",
             "Automated merge completed without conflicts. Tests/builds run separately against the exact candidate SHA; see `fork-sync/verified` status and its run link.", "",
             "Review dependency, schema and API changes below; filenames are risk indicators, not proof of compatibility."]
    for name, changed in groups.items():
        lines += ["", f"- {name}: " + (", ".join(f"`{p}`" for p in changed[:30]) or "no matching changed paths")]
    lines += ["", "Patch-equivalent custom commits already upstream: " + (", ".join(f"`{s}`" for s in equivalent_shas) or "none detected"),
              "Compare `docs/fork/patches.md` and behavior before retiring any patch. Equivalence cannot detect all semantic replacements.", "",
              "Merge only after explicit review, preserving ancestry (merge commit; no squash/rebase). No automatic merge or deployment is configured.", "",
              f"<!-- openusage-sync target={target} head={head} base={base} -->"]
    return "\n".join(lines)


def sync(args):
    repo = api(f"repos/{FORK}")
    if not repo.get("fork") or repo.get("parent", {}).get("full_name") != UPSTREAM or repo["default_branch"] != "custom":
        raise RuntimeError("Expected personal fork with custom default branch")
    workflows = paged(f"repos/{FORK}/actions/workflows", "workflows")
    release = api(f"repos/{UPSTREAM}/releases/latest")
    if release["draft"] or release["prerelease"]:
        raise RuntimeError("Latest release is not stable")
    with tempfile.TemporaryDirectory(prefix="openusage-sync-") as tmp:
        run("git", "clone", "--quiet", "--no-checkout", f"https://github.com/{FORK}.git", tmp)
        git = Git(tmp)
        git.call("remote", "add", "upstream", f"https://github.com/{UPSTREAM}.git")
        git.call("fetch", "--quiet", "--no-tags", "upstream", "refs/heads/main:refs/remotes/upstream/main", "refs/tags/*:refs/upstream-tags/*")
        main = git.text("rev-parse", "refs/remotes/upstream/main")
        origin_main = git.text("rev-parse", "refs/remotes/origin/main")
        base = git.text("rev-parse", "refs/remotes/origin/custom")
        label = args.candidate_sha or release["tag_name"]
        target = args.candidate_sha or git.text("rev-parse", f"refs/upstream-tags/{release['tag_name']}^{{commit}}")
        if not git.ancestor(target, main):
            raise RuntimeError("Candidate is not an ancestor of the observed upstream/main")
        audit_workflows(git, main, workflows)
        git.mirror(origin_main, main, args.apply)
        result = dict(apply=args.apply, upstream_main=main, previous_main=origin_main,
                      custom_base=base, target=target, release=release["tag_name"], candidate_sha="")
        if git.ancestor(target, base):
            result["outcome"] = "candidate already included in custom"
            return result
        branch = f"sync/upstream-{target[:12]}"
        prs = paged(f"repos/{FORK}/pulls?state=all&head={FORK.split('/')[0]}:{branch}&base=custom")
        if len(prs) > 1:
            raise RuntimeError("Multiple PRs for one target; manual review required")
        remote = git.text("ls-remote", "origin", f"refs/heads/{branch}")
        previous = remote.split()[0] if remote else "none"
        git.call("config", "user.name", "github-actions[bot]")
        git.call("config", "user.email", "41898282+github-actions[bot]@users.noreply.github.com")
        if remote:
            git.call("fetch", "--quiet", "origin", f"refs/heads/{branch}")
            if prs:
                existing_head(prs[0], target, previous)
            else:
                base = recover_unrecorded(git, target, previous, base)
            git.call("checkout", "--quiet", "--detach", previous)
            if not git.ancestor(target, previous):
                raise RuntimeError("Recorded branch does not contain the target")
            if not git.ancestor(base, previous):
                merge(git, base, "Merge current custom into upstream sync candidate")
        else:
            if prs:
                raise RuntimeError("Recorded sync branch was removed; preserve maintainer decision")
            git.call("checkout", "--quiet", "--detach", base)
            merge(git, target, f"Merge upstream {label} for review")
        head = git.text("rev-parse", "HEAD")
        result.update(branch=branch, previous_candidate_sha=previous, candidate_sha=head,
                      outcome="candidate prepared", changed_paths=git.text("diff", "--name-only", base, head).splitlines())
        pr_body = body(git, target, head, base, label, previous)
        if args.apply:
            git.call("push", "origin", f"{head}:refs/heads/{branch}")
            payload = dict(title=f"Sync upstream {label}", body=pr_body)
            if prs:
                pr = api(f"repos/{FORK}/pulls/{prs[0]['number']}", "PATCH", payload)
            else:
                payload.update(head=branch, base="custom", draft=True)
                pr = api(f"repos/{FORK}/pulls", "POST", payload)
            if pr["head"]["sha"] != head:
                raise RuntimeError("PR head changed during preparation; no verification claimed")
            result.update(pr_number=pr["number"], pr_url=pr["html_url"])
        return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--apply", action="store_true")
    mode.add_argument("--dry-run", action="store_true", help="Explicit read-only mode (the default)")
    parser.add_argument("--candidate-sha", default="")
    args = parser.parse_args()
    if args.candidate_sha and not re.fullmatch(r"[0-9a-f]{40}", args.candidate_sha):
        parser.error("--candidate-sha requires a full lowercase upstream commit SHA")
    result = sync(args)
    print(json.dumps(result, indent=2))
    if os.getenv("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            for key in ("candidate_sha", "pr_number", "branch"):
                output.write(f"{key}={result.get(key, '')}\n")
    if os.getenv("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
            summary.write("```json\n" + json.dumps(result, indent=2) + "\n```\n")


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as error:
        raise SystemExit(str(error))
