#!/usr/bin/env python3
"""Explicit, local-only canary operations. All commands default to dry-run.

Real deployment approval is a human decision, not permission granted by a flag.
The required approval reference records that decision; it is not an authorization
service. No credentials are printed, and no network, Hub or updater is invoked.
"""
import argparse
from contextlib import closing
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import sqlite3
import stat
import subprocess
import tempfile
import time
import uuid

REPO = Path(__file__).resolve().parents[2]


def digest(path):
    checksum = hashlib.sha256()
    with open(path, "rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            checksum.update(chunk)
    return checksum.hexdigest()


def outside_repo(path):
    path = Path(path).expanduser()
    if not path.is_absolute() or path.resolve() == REPO or REPO in path.resolve().parents:
        raise ValueError("Operational paths must be absolute and outside the source repository")
    return path


def read_manifest(path):
    cfg = json.loads(Path(path).read_text())
    if cfg.get("platform") not in ("linux_arm64", "darwin_arm64"):
        raise ValueError("Supported targets are native Linux/macOS ARM64")
    if not re.fullmatch(r"[0-9a-f]{40}", cfg.get("approved_sha", "")):
        raise ValueError("approved_sha must be the full reviewed fork commit SHA")
    if not re.fullmatch(r"[0-9a-f]{64}", cfg.get("binary_sha256", "")):
        raise ValueError("binary_sha256 must come from the reviewed build artifact")
    if not Path(cfg["artifact_dir"]).is_absolute():
        raise ValueError("artifact_dir must be absolute")
    for key in ("install_root", "backup_root"):
        cfg[key] = str(outside_repo(cfg[key]))
    if Path(cfg["backup_root"]) == Path(cfg["install_root"]) or Path(cfg["install_root"]) in Path(cfg["backup_root"]).parents:
        raise ValueError("Backups must be separate from the versioned installation")
    for entry in cfg.get("files", []):
        outside_repo(entry["path"])
        if entry["role"] not in ("binary", "config", "credentials", "hook", "service"):
            raise ValueError("Unknown backup file role")
    for entry in cfg.get("databases", []):
        outside_repo(entry["path"])
        if entry["role"] not in ("telemetry", "opencode-source"):
            raise ValueError("Unknown database role")
        if entry["path"] in {f["path"] for f in cfg.get("files", [])}:
            raise ValueError("SQLite databases must use the backup API, not ordinary file copying")
    for entry in cfg.get("replacements", []):
        outside_repo(entry["source"])
        outside_repo(entry["target"])
        if entry["role"] not in ("config", "hook", "service") or not re.fullmatch(r"[0-9a-f]{64}", entry["sha256"]):
            raise ValueError("Replacements require an explicit role and SHA-256")
    for service in cfg.get("services", []):
        if service["role"] not in ("collector", "exporter"):
            raise ValueError("This canary helper does not manage the Hub or other services")
        if service["manager"] == "systemd-user":
            if cfg["platform"] != "linux_arm64" or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.@-]*\.service", service["name"]):
                raise ValueError("Invalid systemd user unit")
            outside_repo(service["definition"])
        elif service["manager"] == "launchd-user":
            if cfg["platform"] != "darwin_arm64" or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", service["name"]):
                raise ValueError("Invalid launchd user label")
            outside_repo(service["plist"])
        else:
            raise ValueError("Only systemd-user and launchd-user are supported")
    return cfg


def verify_artifact(cfg):
    directory = Path(cfg["artifact_dir"])
    expected = {}
    for line in (directory / "checksums.txt").read_text().splitlines():
        value, name = line.split("  ", 1)
        if not re.fullmatch(r"[0-9a-f]{64}", value) or Path(name).name != name or name in expected:
            raise ValueError("Invalid artifact checksum entry")
        expected[name] = value
    actual = {p.name for p in directory.iterdir() if p.name != "checksums.txt"}
    if actual != set(expected) or not {"openusage", "metadata.json"} <= actual:
        raise ValueError("Every artifact file must have a checksum")
    for name, value in expected.items():
        source = directory / name
        if source.is_symlink() or not source.is_file() or digest(source) != value:
            raise ValueError("Artifact checksum/type mismatch: " + name)
    metadata = json.loads((directory / "metadata.json").read_text())
    if metadata["fork_sha"] != cfg["approved_sha"] or metadata["platform"] != cfg["platform"]:
        raise ValueError("Artifact does not match the approved SHA and target platform")
    if digest(directory / "openusage") != cfg["binary_sha256"]:
        raise ValueError("Binary differs from the separately reviewed SHA-256")
    for entry in cfg.get("replacements", []):
        if digest(entry["source"]) != entry["sha256"]:
            raise ValueError("Replacement file changed after review")
        target = Path(entry["target"])
        if not target.parent.is_dir() or target.is_dir():
            raise ValueError("Replacement target parent must exist and target must not be a directory")
    return metadata


def sqlite_backup(source, destination, timeout=15):
    """SQLite backup API includes committed WAL; never immutable=1 or bare copy."""
    deadline = time.monotonic() + timeout

    def progress(status, remaining, total):
        if time.monotonic() > deadline:
            raise TimeoutError("SQLite backup remained busy; source was left unchanged")

    source = Path(source).resolve()
    with closing(sqlite3.connect(source.as_uri() + "?mode=ro", uri=True, timeout=5)) as reader:
        with closing(sqlite3.connect(str(destination), timeout=5)) as writer:
            reader.backup(writer, pages=128, progress=progress, sleep=0.05)
            if writer.execute("PRAGMA quick_check").fetchone()[0] != "ok":
                raise ValueError("SQLite backup validation failed")


def backup(cfg, approval):
    root = Path(cfg["backup_root"])
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    if stat.S_IMODE(root.stat().st_mode) & 0o077:
        raise ValueError("Backup root must already be private (mode 0700)")
    destination = root / (datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8])
    partial = Path(tempfile.mkdtemp(prefix=".partial-", dir=root))
    records = []
    try:
        entries = {e["path"]: dict(e) for e in cfg.get("files", [])}
        for entry in cfg.get("replacements", []):
            entries.setdefault(entry["target"], {"path": entry["target"], "role": entry["role"]})
        for service in cfg.get("services", []):
            path = service["definition"] if service["manager"] == "systemd-user" else service["plist"]
            entries.setdefault(path, {"path": path, "role": "service"})
        for index, entry in enumerate(entries.values()):
            source = Path(entry["path"])
            record = dict(entry, existed=source.exists() or source.is_symlink())
            if record["existed"]:
                if not source.is_file():
                    raise ValueError("Backup file must resolve to a regular file")
                name = f"file-{index}"
                shutil.copyfile(source, partial / name)
                (partial / name).chmod(0o600)
                record.update(stored=name, sha256=digest(partial / name), mode=stat.S_IMODE(source.stat().st_mode))
                if source.is_symlink():
                    record["symlink"] = os.readlink(source)
            records.append(record)
        databases = []
        for index, entry in enumerate(cfg.get("databases", [])):
            target = partial / f"database-{index}.sqlite"
            sqlite_backup(entry["path"], target)
            target.chmod(0o600)
            databases.append(dict(entry, stored=target.name, sha256=digest(target)))
        link = Path(cfg["install_root"]) / "current"
        if link.exists() and not link.is_symlink():
            raise ValueError("current must be a fork-owned symlink, never a real directory")
        record = dict(approval_ref=approval, approved_sha=cfg["approved_sha"], files=records,
                      databases=databases, previous_current=os.readlink(link) if link.is_symlink() else None,
                      install_root=cfg["install_root"], replaced_paths=[e["target"] for e in cfg.get("replacements", [])])
        (partial / "backup.json").write_text(json.dumps(record, indent=2) + "\n")
        (partial / "backup.json").chmod(0o600)
        partial.rename(destination)
        return destination
    except Exception:
        shutil.rmtree(partial)
        raise


def atomic_file(source, target, mode):
    target = Path(target)
    descriptor, name = tempfile.mkstemp(prefix=".openusage-", dir=target.parent)
    os.close(descriptor)
    try:
        shutil.copyfile(source, name)
        os.chmod(name, mode)
        os.replace(name, target)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def point_current(cfg, target):
    link = Path(cfg["install_root"]) / "current"
    if link.exists() and not link.is_symlink():
        raise ValueError("Refusing to replace a real current directory")
    temporary = link.with_name(".current-" + uuid.uuid4().hex)
    os.symlink(target, temporary)
    os.replace(temporary, link)


def stage(cfg):
    verify_artifact(cfg)
    versions = Path(cfg["install_root"]) / "versions"
    versions.mkdir(parents=True, exist_ok=True, mode=0o700)
    target = versions / cfg["approved_sha"]
    if target.exists():
        staged = dict(cfg, artifact_dir=str(target))
        verify_artifact(staged)
        return target
    partial = Path(tempfile.mkdtemp(prefix=".stage-", dir=versions))
    try:
        for source in Path(cfg["artifact_dir"]).iterdir():
            shutil.copyfile(source, partial / source.name)
        (partial / "openusage").chmod(0o755)
        verify_artifact(dict(cfg, artifact_dir=str(partial)))
        partial.rename(target)
    except Exception:
        shutil.rmtree(partial)
        raise
    return target


def services(cfg, action):
    configured = cfg.get("services", [])
    if not configured:
        raise ValueError("Deployment/rollback requires explicit Collector/Exporter service definitions")
    if action == "start":
        configured = list(reversed(configured))
        if cfg["platform"] == "linux_arm64":
            subprocess.run(["systemctl", "--user", "daemon-reload"], check=True)
    for service in configured:
        if service["manager"] == "systemd-user":
            subprocess.run(["systemctl", "--user", action, service["name"]], check=True)
            if action == "start":
                subprocess.run(["systemctl", "--user", "is-active", "--quiet", service["name"]], check=True)
        else:
            domain = f"gui/{os.getuid()}"
            command = ["launchctl", "bootout", f"{domain}/{service['name']}"] if action == "stop" else ["launchctl", "bootstrap", domain, service["plist"]]
            subprocess.run(command, check=True)
            if action == "start":
                subprocess.run(["launchctl", "print", f"{domain}/{service['name']}"], check=True, stdout=subprocess.DEVNULL)


def validate_backup(cfg, location, restore_db):
    location = outside_repo(location)
    record = json.loads((location / "backup.json").read_text())
    if record["install_root"] != cfg["install_root"]:
        raise ValueError("Backup belongs to a different installation")
    allowed = {entry["path"] for entry in cfg.get("files", [])} | {entry["target"] for entry in cfg.get("replacements", [])}
    allowed |= {s["definition"] if s["manager"] == "systemd-user" else s["plist"] for s in cfg.get("services", [])}
    for entry in record["files"]:
        if entry["path"] not in allowed:
            raise ValueError("Backup target is not included in the reviewed manifest")
    if not set(record["replaced_paths"]) <= {e["target"] for e in cfg.get("replacements", [])}:
        raise ValueError("Backup replacement scope differs from the reviewed manifest")
    for entry in record["files"] + record["databases"]:
        if entry.get("existed", True):
            if Path(entry["stored"]).name != entry["stored"] or digest(location / entry["stored"]) != entry["sha256"]:
                raise ValueError("Backup checksum mismatch")
    if restore_db:
        allowed_dbs = {entry["path"] for entry in cfg.get("databases", []) if entry["role"] == "telemetry"}
        if any(e["path"] not in allowed_dbs for e in record["databases"] if e["role"] == "telemetry"):
            raise ValueError("Unreviewed telemetry database target")
    return record


def restore(cfg, location, restore_db):
    location = outside_repo(location)
    record = validate_backup(cfg, location, restore_db)
    services(cfg, "stop")
    for entry in record["files"]:
        if entry["path"] not in record["replaced_paths"]:
            # Saved for recovery, but never changed by deployment: preserve
            # refreshed credentials, package-manager files and unrelated edits.
            continue
        target = Path(entry["path"])
        if not entry["existed"]:
            target.unlink(missing_ok=True)
        elif "symlink" in entry:
            temporary = target.with_name(".restore-" + uuid.uuid4().hex)
            os.symlink(entry["symlink"], temporary)
            os.replace(temporary, target)
        else:
            atomic_file(location / entry["stored"], target, entry["mode"])
    if restore_db:
        for entry in record["databases"]:
            if entry["role"] == "telemetry":
                sqlite_backup(location / entry["stored"], entry["path"])
    if record["previous_current"] is None:
        (Path(cfg["install_root"]) / "current").unlink(missing_ok=True)
    else:
        point_current(cfg, record["previous_current"])
    services(cfg, "start")


def execute(cfg, action, apply=False, approval="", rollback_backup=None, restore_db=False):
    if action in ("deploy", "rollback") and not cfg.get("services"):
        raise ValueError("Deployment/rollback requires explicit Collector/Exporter service definitions")
    if action == "deploy" and not any(e["role"] == "binary" and Path(e["path"]).is_file() for e in cfg.get("files", [])):
        raise ValueError("Deployment must back up an existing original binary explicitly")
    if action in ("stage", "deploy"):
        verify_artifact(cfg)
    if action == "rollback" and not rollback_backup:
        raise ValueError("Rollback requires an explicit backup directory")
    if action == "rollback":
        validate_backup(cfg, rollback_backup, restore_db)
    plan = dict(action=action, dry_run=not apply, approved_sha=cfg["approved_sha"],
                platform=cfg["platform"], service_count=len(cfg.get("services", [])),
                replacement_roles=[e["role"] for e in cfg.get("replacements", [])], restore_telemetry_db=restore_db)
    if not apply:
        return plan
    if action in ("deploy", "rollback") and not approval.strip():
        raise ValueError("--execute requires --approval-ref identifying explicit deployment/rollback approval")
    if action in ("stage", "deploy", "rollback"):
        operating_system = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system(), "unsupported")
        actual = operating_system + "_" + ("arm64" if platform.machine() in ("arm64", "aarch64") else platform.machine())
        if actual != cfg["platform"]:
            raise ValueError("Apply requires the matching native ARM64 machine")
    if action == "stage":
        plan["staged_directory"] = str(stage(cfg))
        return plan
    snapshot = backup(cfg, approval)
    plan["preservation_backup"] = str(snapshot)
    print(json.dumps({"preservation_backup": str(snapshot)}), flush=True)
    if action == "deploy":
        target = stage(cfg)
        services(cfg, "stop")
        for entry in cfg.get("replacements", []):
            if digest(entry["source"]) != entry["sha256"]:
                raise ValueError("Replacement changed while deployment was being prepared; services remain stopped")
            mode = stat.S_IMODE(Path(entry["target"]).stat().st_mode) if Path(entry["target"]).exists() else 0o600
            atomic_file(entry["source"], entry["target"], mode)
        point_current(cfg, target)
        services(cfg, "start")
    elif action == "rollback":
        restore(cfg, rollback_backup, restore_db)
    return plan


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("backup", "stage", "deploy", "rollback"))
    parser.add_argument("--manifest", required=True)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--execute", action="store_true", dest="apply")
    mode.add_argument("--dry-run", action="store_true", help="Explicit default: validate and print the plan without writes")
    parser.add_argument("--approval-ref", default="")
    parser.add_argument("--backup", help="Prior backup to restore during rollback")
    parser.add_argument("--restore-telemetry-db", action="store_true", help="Explicitly restore local History; newer events are preserved in the pre-rollback backup")
    args = parser.parse_args()
    try:
        cfg = read_manifest(args.manifest)
        print(json.dumps(execute(cfg, args.action, args.apply, args.approval_ref, args.backup, args.restore_telemetry_db), indent=2))
    except (ValueError, OSError, sqlite3.Error, subprocess.CalledProcessError) as error:
        raise SystemExit(f"Operation stopped: {error}. Preserve any reported backup; inspect service state before retrying.")
