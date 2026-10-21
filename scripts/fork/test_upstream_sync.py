"""Synthetic Git graphs; no GitHub access or operational data."""
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

from upstream_sync import FORK, UPSTREAM, Git, audit_workflows, body, existing_head, merge, recover_unrecorded, run, sync


class SyncTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.origin = self.root / "origin.git"
        run("git", "init", "-q", "--initial-branch=main", str(self.repo))
        run("git", "init", "-q", "--bare", str(self.origin))
        self.git = Git(self.repo)
        self.git.call("config", "user.name", "github-actions[bot]")
        self.git.call("config", "user.email", "41898282+github-actions[bot]@users.noreply.github.com")
        self.base = self.commit("base.txt", "base")
        self.git.call("remote", "add", "origin", str(self.origin))
        self.git.call("push", "origin", "HEAD:main")

    def commit(self, name, text):
        (self.repo / name).parent.mkdir(parents=True, exist_ok=True)
        (self.repo / name).write_text(text)
        self.git.call("add", name)
        self.git.call("commit", "-qm", name)
        return self.git.text("rev-parse", "HEAD")

    def graph(self):
        target = self.commit("upstream.txt", "upstream")
        self.git.call("checkout", "-q", "--detach", self.base)
        custom = self.commit("custom.txt", "patch")
        return target, custom

    def test_mirror_is_exact_fast_forward_and_dry_run_does_not_push(self):
        target = self.commit("new.txt", "new")
        self.git.mirror(self.base, target, False)
        self.assertEqual(self.git.text("ls-remote", "origin", "refs/heads/main").split()[0], self.base)
        self.git.mirror(self.base, target, True)
        self.assertEqual(self.git.text("ls-remote", "origin", "refs/heads/main").split()[0], target)

    def test_divergence_and_concurrent_remote_update_are_preserved(self):
        target, custom = self.graph()
        with self.assertRaisesRegex(RuntimeError, "diverged"):
            self.git.mirror(custom, target, True)
        self.git.call("push", "origin", f"{custom}:main")
        with self.assertRaises(RuntimeError):
            self.git.mirror(self.base, target, True)
        self.assertEqual(self.git.text("ls-remote", "origin", "refs/heads/main").split()[0], custom)

    def test_human_commit_and_closed_pr_are_not_updated(self):
        target, custom = self.graph()
        merge(self.git, target, "merge for review")
        head = self.git.text("rev-parse", "HEAD")
        pr = {"state": "open", "body": body(self.git, target, head, custom, "test")}
        self.assertEqual(existing_head(pr, target, head), custom)
        human = self.commit("human.txt", "keep me")
        with self.assertRaisesRegex(RuntimeError, "preserve human"):
            existing_head(pr, target, human)
        pr["state"] = "closed"
        with self.assertRaisesRegex(RuntimeError, "closed"):
            existing_head(pr, target, head)
        self.assertEqual((self.repo / "human.txt").read_text(), "keep me")

    def test_pr_create_failure_can_recover_exact_merge_but_not_extra_commit(self):
        target, custom = self.graph()
        merge(self.git, target, "merge for review")
        head = self.git.text("rev-parse", "HEAD")
        self.assertEqual(recover_unrecorded(self.git, target, head, custom), custom)
        human = self.commit("human.txt", "preserve")
        with self.assertRaisesRegex(RuntimeError, "preserve"):
            recover_unrecorded(self.git, target, human, custom)

    def test_conflict_never_pushes_partial_merge(self):
        target = self.commit("base.txt", "upstream edit")
        self.git.call("checkout", "-q", "--detach", self.base)
        custom = self.commit("base.txt", "custom edit")
        with self.assertRaisesRegex(RuntimeError, "base.txt"):
            merge(self.git, target, "conflict")
        self.assertEqual(self.git.text("rev-parse", "HEAD"), custom)
        self.assertEqual(self.git.text("ls-remote", "origin", "refs/heads/main").split()[0], self.base)

    def test_new_or_active_inherited_workflow_blocks_before_mirror(self):
        target = self.commit(".github/workflows/publish.yaml", "name: publish\n")
        with self.assertRaisesRegex(RuntimeError, "publish.yaml"):
            audit_workflows(self.git, target, [])
        with self.assertRaises(RuntimeError):
            audit_workflows(self.git, target, [{"path": ".github/workflows/publish.yaml", "state": "active"}])
        audit_workflows(self.git, target, [{"path": ".github/workflows/publish.yaml", "state": "disabled_manually"}])

    def test_upstream_cannot_replace_trusted_automation(self):
        target = self.commit(".github/workflows/fork-ci.yaml", "untrusted")
        with self.assertRaisesRegex(RuntimeError, "collides"):
            audit_workflows(self.git, target, [])

    def simulate_github(self, latest_target):
        """Use real bare Git remotes with an in-memory GitHub PR API."""
        upstream = self.root / "upstream.git"
        run("git", "clone", "--quiet", "--bare", str(self.origin), str(upstream))
        upstream_git = Git(upstream)
        upstream_git.call("update-ref", "refs/heads/main", latest_target)
        upstream_git.call("update-ref", "refs/tags/v1.0.0", latest_target)
        prs = []
        writes = []

        def fake_run(*args, **kwargs):
            args = list(args)
            args = [str(self.origin) if a == f"https://github.com/{FORK}.git" else
                    str(upstream) if a == f"https://github.com/{UPSTREAM}.git" else a for a in args]
            return run(*args, **kwargs)

        def fake_api(path, method="GET", payload=None):
            if path == f"repos/{FORK}":
                return {"fork": True, "parent": {"full_name": UPSTREAM}, "default_branch": "custom"}
            if path == f"repos/{UPSTREAM}/releases/latest":
                return {"draft": False, "prerelease": False, "tag_name": "v1.0.0"}
            if method == "POST":
                writes.append("POST")
                head = Git(self.origin).text("rev-parse", f"refs/heads/{payload['head']}")
                pr = dict(payload, number=1, html_url="https://example.invalid/pr/1", state="open")
                pr["head"] = {"sha": head}
                prs.append(pr)
                return pr
            if method == "PATCH":
                writes.append("PATCH")
                prs[0].update(payload)
                branch = f"sync/upstream-{latest_target[:12]}"
                prs[0]["head"] = {"sha": Git(self.origin).text("rev-parse", f"refs/heads/{branch}")}
                return prs[0]
            raise AssertionError((path, method))

        def fake_paged(path, key=None):
            return [] if key == "workflows" else prs

        for name, replacement in (("run", fake_run), ("api", fake_api), ("paged", fake_paged)):
            mocked = patch("upstream_sync." + name, side_effect=replacement)
            mocked.start()
            self.addCleanup(mocked.stop)
        return prs, writes

    def test_full_rerun_reuses_one_pr_and_exact_head(self):
        target, custom = self.graph()
        self.git.call("push", "origin", f"{target}:main", f"{custom}:refs/heads/custom")
        prs, writes = self.simulate_github(target)
        args = SimpleNamespace(apply=True, candidate_sha="")
        first = sync(args)
        second = sync(args)
        self.assertEqual(first["candidate_sha"], second["candidate_sha"])
        self.assertEqual(len(prs), 1)
        self.assertEqual(writes, ["POST", "PATCH"])

    def test_full_dry_run_has_no_remote_or_api_writes(self):
        target, custom = self.graph()
        self.git.call("push", "origin", f"{target}:main", f"{custom}:refs/heads/custom")
        _, writes = self.simulate_github(target)
        before = self.git.text("ls-remote", "origin")
        result = sync(SimpleNamespace(apply=False, candidate_sha=""))
        self.assertTrue(result["candidate_sha"])
        self.assertEqual(writes, [])
        self.assertEqual(before, self.git.text("ls-remote", "origin"))

    def test_full_existing_human_update_preserved(self):
        target, custom = self.graph()
        self.git.call("push", "origin", f"{target}:main", f"{custom}:refs/heads/custom")
        _, writes = self.simulate_github(target)
        args = SimpleNamespace(apply=True, candidate_sha="")
        first = sync(args)
        self.git.call("fetch", "origin", first["branch"])
        self.git.call("checkout", "--detach", first["candidate_sha"])
        human = self.commit("human.txt", "preserve")
        self.git.call("push", "origin", f"{human}:{first['branch']}")
        with self.assertRaisesRegex(RuntimeError, "preserve human"):
            sync(args)
        self.assertEqual(writes, ["POST"])
        actual = self.git.text("ls-remote", "origin", f"refs/heads/{first['branch']}").split()[0]
        self.assertEqual(actual, human)


if __name__ == "__main__":
    unittest.main()
