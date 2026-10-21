import unittest
from unittest.mock import patch

from upstream_base import resolve, select_tag


class BaseTests(unittest.TestCase):
    def test_numeric_versions_and_stable_tags_only(self):
        self.assertEqual(select_tag(["v0.26.1", "v0.27.0", "v0.99.0-rc1", "fork/v0.99.0-p1"]), "v0.27.0")
        self.assertEqual(select_tag(["v1.9.0", "v1.10.0"]), "v1.10.0")
        with self.assertRaises(ValueError):
            select_tag(["fork/v1.0.0-p1", "v1.0.0-rc1"])

    @patch("upstream_base.git")
    def test_merged_new_release_updates_metadata_and_supports_annotated_tag(self, git):
        sha = "a" * 40
        official = "b" * 40
        git.side_effect = ["v0.26.1\nv0.27.0", official,
                           f"{'c' * 40}\trefs/tags/v0.27.0\n{official}\trefs/tags/v0.27.0^{{}}"]
        self.assertEqual(resolve(sha), {"upstream_base_tag": "v0.27.0", "upstream_base_sha": official})
        self.assertEqual(git.call_args_list[0].args, ("tag", "--merged", sha, "--list", "v*"))

    @patch("upstream_base.git")
    def test_retagged_or_local_only_version_rejected(self, git):
        git.side_effect = ["v0.27.0", "a" * 40, f"{'b' * 40}\trefs/tags/v0.27.0"]
        with self.assertRaisesRegex(ValueError, "does not match"):
            resolve("c" * 40)


if __name__ == "__main__":
    unittest.main()
