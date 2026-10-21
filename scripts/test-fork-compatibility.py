#!/usr/bin/env python3
"""Run original/fork binaries against isolated, authenticated loopback Hubs.

No services are registered. HOME, XDG paths, source databases, sockets, config,
tokens, logs and hooks are temporary. No provider credentials or model calls.
"""

import argparse
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value))
    path.chmod(0o600)


def environment(home, token):
    home.mkdir(parents=True, exist_ok=True)
    # Deliberately do not inherit API keys, browser sessions or tool paths.
    return {
        "HOME": str(home), "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8",
        "XDG_CONFIG_HOME": str(home / ".config"),
        "XDG_STATE_HOME": str(home / ".local/state"),
        "XDG_DATA_HOME": str(home / ".local/share"),
        "XDG_CACHE_HOME": str(home / ".cache"),
        "OPENUSAGE_HUB_TOKEN": token,
    }


def request(url, token=None):
    headers = {"Authorization": "Bearer " + token} if token else {}
    with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=2) as response:
        return json.load(response)


def until(check, processes, description, timeout=35):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if any(process.poll() is not None for process in processes):
            raise RuntimeError("isolated process exited during " + description)
        try:
            value = check()
            if value:
                return value
        except (urllib.error.URLError, TimeoutError, ConnectionError):
            pass
        time.sleep(0.1)
    raise RuntimeError("timeout during " + description)


def snapshot_matches(snaps, amount):
    if len(snaps) != 1:
        return False
    snap = next(iter(snaps.values()))
    metrics = snap.get("metrics", {})
    if metrics.get("model_synthetic_model_input_tokens", {}).get("used") != amount:
        return False
    if metrics.get("window_requests", {}).get("used") != 1:
        return False
    if amount and metrics.get("window_cost", {}).get("used") != amount / 100:
        return False
    assert "SYNTHETIC_PRIVATE_BODY" not in json.dumps(snaps)
    assert snap["account_id"] == "synthetic-machine:opencode:synthetic-account"
    return True


def run_pair(collector, hub, use_v2):
    with tempfile.TemporaryDirectory(prefix="openusage-compatibility-") as directory:
        root = Path(directory)
        token = secrets.token_urlsafe(32)
        collector_env = environment(root / "collector", token)
        hub_env = environment(root / "hub", token)
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        url = "http://127.0.0.1:" + str(port)
        write_json(root / "hub/.config/openusage/settings.json", {"auto_detect": False, "accounts": []})
        source = root / "collector/.local/share/opencode/opencode.db"
        write_json(root / "collector/.config/openusage/settings.json", {
            "auto_detect": False,
            "accounts": [{"id": "synthetic-account", "provider": "opencode", "auth": "local",
                          "api_key_env": "SYNTHETIC_UNSET_API_KEY", "base_url": "http://127.0.0.1:9"}],
            "data": {"time_window": "1d", "retention_days": 1},
            "export": {"target": url, "machine_name": "synthetic-machine", "interval_seconds": 1},
        })
        now = int(time.time() * 1000)
        if use_v2:
            source.parent.mkdir(parents=True, exist_ok=True)
            with sqlite3.connect(source) as db:
                db.executescript("""
                    CREATE TABLE session_v2(id TEXT PRIMARY KEY, directory TEXT);
                    CREATE TABLE session_message(id TEXT PRIMARY KEY, session_id TEXT, type TEXT,
                        seq INTEGER, time_created INTEGER, time_updated INTEGER, data TEXT);
                    INSERT INTO session_v2 VALUES ('synthetic-session', '/synthetic/project');
                """)

        def put(amount, revision):
            data = {"model": {"id": "synthetic-model", "providerID": "anthropic"}, "agent": "opencode",
                    "tokens": {"input": amount, "output": 0}, "cost": amount / 100,
                    "time": {"created": now, "completed": now}, "finish": "stop",
                    "content": [{"type": "text", "text": "SYNTHETIC_PRIVATE_BODY"}]}
            with sqlite3.connect(source) as db:
                db.execute("""INSERT INTO session_message VALUES
                    ('synthetic-message','synthetic-session','assistant',1,?,?,?)
                    ON CONFLICT(id) DO UPDATE SET data=excluded.data,time_updated=excluded.time_updated""",
                           (now, now + revision, json.dumps(data)))

        if use_v2:
            put(0, 0)
        socket_path = root / "collector.sock"
        daemon_args = [str(collector), "telemetry", "daemon", "--socket-path", str(socket_path),
                       "--db-path", str(root / "canonical.db"), "--spool-dir", str(root / "spool"),
                       "--collect-interval", "1s", "--poll-interval", "1s"]
        processes = []
        revisions = []
        try:
            with (root / "hub.log").open("wb") as output:
                processes.append(subprocess.Popen([str(hub), "hub", "--headless", "--listen", "127.0.0.1:" + str(port)],
                                                   env=hub_env, cwd=root, stdout=output, stderr=output))
            until(lambda: request(url + "/healthz"), processes, "Hub readiness")
            try:
                request(url + "/v1/snapshots")
                raise AssertionError("unauthenticated Hub read succeeded")
            except urllib.error.HTTPError as error:
                assert error.code == 401
            with (root / "collector.log").open("wb") as output:
                processes.append(subprocess.Popen(daemon_args, env=collector_env, cwd=root, stdout=output, stderr=output))
            until(socket_path.exists, processes, "Collector socket readiness")

            def hook(payload):
                result = subprocess.run([str(collector), "telemetry", "hook", "opencode",
                                         "--socket-path", str(socket_path), "--account-id", "synthetic-account",
                                         "--db-path", str(root / "canonical.db"), "--spool-dir", str(root / "spool")],
                                        input=json.dumps(payload).encode(), env=collector_env, cwd=root,
                                        capture_output=True, timeout=10)
                assert result.returncode == 0, "isolated hook command failed"

            if use_v2:
                for revision, amount in enumerate((0, 30, 20)):
                    if revision:
                        put(amount, revision)
                    until(lambda: snapshot_matches(request(url + "/v1/snapshots", token), amount), processes,
                          "v2 canonical revision " + str(revision))
                    revisions.append(amount)
                hook({"event": {"type": "session.step.ended", "created": now,
                                "location": {"directory": "/synthetic/project"},
                                "data": {"sessionID": "synthetic-session", "assistantMessageID": "synthetic-message",
                                         "model": {"providerID": "anthropic", "id": "synthetic-model"},
                                         "tokens": {"input": 99, "output": 0}, "cost": 0.99}}})
            else:
                payload = {"type": "message.updated", "properties": {"info": {
                    "id": "synthetic-message", "sessionID": "synthetic-session", "role": "assistant",
                    "providerID": "opencode", "modelID": "synthetic-model",
                    "tokens": {"input": 20, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}},
                    "cost": 0.2, "time": {"created": now, "completed": now},
                    "path": {"cwd": "/synthetic/project"}}}}
                hook(payload)
                hook(payload)
                until(lambda: snapshot_matches(request(url + "/v1/snapshots", token), 20), processes, "legacy hook snapshot")
                revisions.append(20)

            with sqlite3.connect("file:" + str(root / "canonical.db") + "?mode=ro", uri=True) as db:
                count, total = db.execute("SELECT COUNT(*),SUM(input_tokens) FROM usage_events WHERE event_type='message_usage'").fetchone()
                assert (count, total) == (1, 20), "canonical replay changed usage"
            assert snapshot_matches(request(url + "/v1/snapshots", token), 20)
            return {"passed": True, "transport": "authenticated loopback HTTP",
                    "canonical_message_count": count, "final_input_tokens": total,
                    "received_revisions": revisions, "private_content_absent": True,
                    "unauthenticated_read_status": 401}
        finally:
            for process in reversed(processes):
                process.terminate()
            for process in reversed(processes):
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--original", required=True, type=Path)
    parser.add_argument("--candidate", required=True, type=Path)
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    original, candidate = args.original.resolve(), args.candidate.resolve()
    report = {"original_version": subprocess.check_output([str(original), "--version"], text=True).strip(),
              "candidate_version": subprocess.check_output([str(candidate), "--version"], text=True).strip(),
              "modified_collector_original_hub": run_pair(candidate, original, True),
              "original_collector_modified_hub": run_pair(original, candidate, False)}
    if args.report:
        write_json(args.report, report)
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
