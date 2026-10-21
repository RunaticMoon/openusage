"""Synthetic files/SQLite databases only; service management is mocked."""
import json
from pathlib import Path
import sqlite3
import stat
import tempfile
import unittest
from unittest.mock import patch

from operate import backup, digest, execute, read_manifest, verify_artifact


class OperationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name, value in {"old-binary": "old", "settings.json": "{}", "hook.js": "old hook",
                            "collector.service": "old service", "service.new": "new service",
                            "hook.new": "new hook"}.items():
            (self.root / name).write_text(value)
        self.db = self.root / "telemetry.sqlite"
        self.writer = sqlite3.connect(self.db)
        self.addCleanup(self.writer.close)
        self.writer.execute("PRAGMA journal_mode=WAL")
        self.writer.execute("PRAGMA wal_autocheckpoint=0")
        self.writer.execute("CREATE TABLE events(id INTEGER PRIMARY KEY, tokens INTEGER)")
        self.writer.execute("INSERT INTO events(tokens) VALUES (10)")
        self.writer.commit()
        self.artifact = self.root / "artifact"
        self.artifact.mkdir()
        (self.artifact / "openusage").write_text("synthetic executable; never run")
        (self.artifact / "metadata.json").write_text(json.dumps({"fork_sha": "a" * 40, "platform": "linux_arm64", "signed": False}))
        self.checksums()
        cfg = {"platform": "linux_arm64", "approved_sha": "a" * 40,
               "binary_sha256": digest(self.artifact / "openusage"),
               "artifact_dir": str(self.artifact), "install_root": str(self.root / "installation"),
               "backup_root": str(self.root / "backups"),
               "files": [{"role": "binary", "path": str(self.root / "old-binary")},
                         {"role": "config", "path": str(self.root / "settings.json")}],
               "databases": [{"role": "telemetry", "path": str(self.db)}],
               "replacements": [{"role": "service", "source": str(self.root / "service.new"), "target": str(self.root / "collector.service"), "sha256": digest(self.root / "service.new")},
                                {"role": "hook", "source": str(self.root / "hook.new"), "target": str(self.root / "hook.js"), "sha256": digest(self.root / "hook.new")}],
               "services": [{"role": "collector", "manager": "systemd-user", "name": "openusage-test.service", "definition": str(self.root / "collector.service")}]}
        manifest = self.root / "manifest.json"
        manifest.write_text(json.dumps(cfg))
        self.cfg = read_manifest(manifest)
        for name, value in (("system", "Linux"), ("machine", "aarch64")):
            mocked = patch("operate.platform." + name, return_value=value)
            mocked.start()
            self.addCleanup(mocked.stop)

    def checksums(self):
        (self.artifact / "checksums.txt").write_text("".join(f"{digest(p)}  {p.name}\n" for p in sorted(self.artifact.iterdir()) if p.name != "checksums.txt"))

    def tree(self):
        return {str(p.relative_to(self.root)): digest(p) for p in self.root.rglob("*") if p.is_file()}

    def rows(self, path):
        with sqlite3.connect(path) as connection:
            return connection.execute("SELECT count(*), sum(tokens) FROM events").fetchone()

    def test_default_dry_run_changes_no_file_and_calls_no_service(self):
        before = self.tree()
        with patch("operate.subprocess.run") as service:
            result = execute(self.cfg, "deploy")
            self.assertTrue(result["dry_run"])
            service.assert_not_called()
        self.assertEqual(before, self.tree())

    def test_stage_validates_and_copies_without_switching_current(self):
        with patch("operate.subprocess.run") as service:
            result = execute(self.cfg, "stage", apply=True)
            service.assert_not_called()
        staged = Path(result["staged_directory"])
        self.assertEqual(digest(staged / "openusage"), self.cfg["binary_sha256"])
        self.assertTrue(stat.S_IMODE((staged / "openusage").stat().st_mode) & 0o111)
        self.assertFalse((Path(self.cfg["install_root"]) / "current").exists())
        self.assertEqual((self.root / "hook.js").read_text(), "old hook")

    def test_backup_includes_live_committed_wal_and_is_private(self):
        source_before = (digest(self.db), digest(str(self.db) + "-wal"))
        snapshot = backup(self.cfg, "synthetic")
        record = json.loads((snapshot / "backup.json").read_text())
        copied = snapshot / record["databases"][0]["stored"]
        self.assertEqual(self.rows(copied), (1, 10))
        self.assertEqual(source_before, (digest(self.db), digest(str(self.db) + "-wal")))
        self.assertEqual(stat.S_IMODE(snapshot.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(copied.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE((snapshot / "backup.json").stat().st_mode), 0o600)

    def test_apply_requires_deployment_approval_before_backup_or_service(self):
        before = self.tree()
        with self.assertRaisesRegex(ValueError, "approval"):
            execute(self.cfg, "deploy", apply=True)
        self.assertEqual(before, self.tree())

    def test_deploy_and_binary_hook_config_rollback_keep_new_history(self):
        with patch("operate.services") as services:
            deployed = execute(self.cfg, "deploy", apply=True, approval="synthetic-approved-change")
            self.assertEqual([c.args[1] for c in services.call_args_list], ["stop", "start"])
            self.assertEqual((self.root / "hook.js").read_text(), "new hook")
            self.writer.execute("INSERT INTO events(tokens) VALUES (20)")
            self.writer.commit()
            (self.root / "settings.json").write_text('{"refreshed": true}')
            (self.root / "old-binary").write_text("independent package upgrade")
            rolled = execute(self.cfg, "rollback", apply=True, approval="synthetic-rollback",
                             rollback_backup=deployed["preservation_backup"])
        self.assertEqual((self.root / "hook.js").read_text(), "old hook")
        self.assertEqual((self.root / "collector.service").read_text(), "old service")
        self.assertEqual((self.root / "settings.json").read_text(), '{"refreshed": true}')
        self.assertEqual((self.root / "old-binary").read_text(), "independent package upgrade")
        self.assertEqual(self.rows(self.db), (2, 30))
        self.assertTrue(Path(rolled["preservation_backup"]).exists())

    def test_explicit_db_restore_preserves_newer_snapshot_and_never_restores_source(self):
        source = self.root / "opencode.sqlite"
        with sqlite3.connect(source) as connection:
            connection.execute("CREATE TABLE source_events(tokens INTEGER)")
            connection.execute("INSERT INTO source_events VALUES (1)")
        self.cfg["databases"].append({"role": "opencode-source", "path": str(source)})
        original = backup(self.cfg, "before")
        self.writer.execute("INSERT INTO events(tokens) VALUES (20)")
        self.writer.commit()
        with sqlite3.connect(source) as connection:
            connection.execute("INSERT INTO source_events VALUES (2)")
        with patch("operate.services"):
            result = execute(self.cfg, "rollback", apply=True, approval="explicit-database-restore",
                             rollback_backup=original, restore_db=True)
        self.assertEqual(self.rows(self.db), (1, 10))
        newer = Path(result["preservation_backup"])
        record = json.loads((newer / "backup.json").read_text())
        self.assertEqual(self.rows(newer / record["databases"][0]["stored"]), (2, 30))
        with sqlite3.connect(source) as connection:
            self.assertEqual(connection.execute("SELECT sum(tokens) FROM source_events").fetchone()[0], 3)

    def test_modified_binary_rejected_even_if_artifact_checksums_are_rewritten(self):
        (self.artifact / "openusage").write_text("unreviewed binary")
        self.checksums()
        with self.assertRaisesRegex(ValueError, "separately reviewed"):
            verify_artifact(self.cfg)

    def test_bad_database_never_produces_complete_backup(self):
        bad = self.root / "unsupported.sqlite"
        bad.write_text("not SQLite")
        self.cfg["databases"] = [{"role": "telemetry", "path": str(bad)}]
        with self.assertRaises(sqlite3.DatabaseError):
            backup(self.cfg, "synthetic")
        self.assertEqual(list(Path(self.cfg["backup_root"]).iterdir()), [])
        self.assertEqual(bad.read_text(), "not SQLite")

    def test_rollback_dry_run_rejects_bad_backup_without_writes(self):
        original = backup(self.cfg, "synthetic")
        (original / "file-0").write_text("corrupted copy")
        before = self.tree()
        with patch("operate.services") as service:
            with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                execute(self.cfg, "rollback", rollback_backup=original)
            service.assert_not_called()
        self.assertEqual(before, self.tree())

    def test_failed_start_retains_recoverable_backup(self):
        with patch("operate.services", side_effect=[None, RuntimeError("synthetic startup failure")]):
            with self.assertRaisesRegex(RuntimeError, "startup failure"):
                execute(self.cfg, "deploy", apply=True, approval="synthetic-approved-change")
        snapshots = list(Path(self.cfg["backup_root"]).iterdir())
        self.assertEqual(len(snapshots), 1)
        self.assertTrue((snapshots[0] / "backup.json").exists())
        with patch("operate.services"):
            execute(self.cfg, "rollback", apply=True, approval="synthetic-approved-rollback", rollback_backup=snapshots[0])
        self.assertEqual((self.root / "hook.js").read_text(), "old hook")
        self.assertEqual(self.rows(self.db), (1, 10))


if __name__ == "__main__":
    unittest.main()
