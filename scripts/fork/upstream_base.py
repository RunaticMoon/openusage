#!/usr/bin/env python3
"""Resolve the newest stable upstream tag contained in the exact build commit."""
import argparse
import json
import re
import subprocess

UPSTREAM_URL = "https://github.com/janekbaraniewski/openusage.git"
STABLE_TAG = re.compile(r"v([0-9]+)\.([0-9]+)\.([0-9]+)$")


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def select_tag(tags):
    versions = [(tuple(map(int, match.groups())), tag) for tag in tags
                if (match := STABLE_TAG.fullmatch(tag))]
    if not versions:
        raise ValueError("No stable upstream release tag is an ancestor of this commit; fetch upstream tags")
    return max(versions)[1]


def resolve(sha):
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("A full build commit SHA is required")
    tag = select_tag(git("tag", "--merged", sha, "--list", "v*").splitlines())
    local = git("rev-parse", f"refs/tags/{tag}^{{commit}}")
    refs = dict(line.split()[::-1] for line in git(
        "ls-remote", "--exit-code", UPSTREAM_URL,
        f"refs/tags/{tag}", f"refs/tags/{tag}^{{}}").splitlines())
    official = refs.get(f"refs/tags/{tag}^{{}}", refs.get(f"refs/tags/{tag}"))
    if official != local:
        raise ValueError("Local stable tag does not match the official upstream tag; refusing incorrect provenance")
    return {"upstream_base_tag": tag, "upstream_base_sha": official}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("sha")
    parser.add_argument("--fields", action="store_true", help="Print validated tag and SHA for shell read")
    args = parser.parse_args()
    result = resolve(args.sha)
    print(" ".join(result.values()) if args.fields else json.dumps(result, indent=2))
