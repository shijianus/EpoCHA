"""Offline, append-only command receipts for the 2026-11-14 repository audit."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import time
from datetime import datetime, timezone

ROOT = Path(__file__).resolve().parents[4]
OUT = Path(__file__).resolve().parent
GO = Path("C:/Users/Smart Kevin/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.windows-amd64/bin/go.exe")
REPORT = "docs/reviews/PROJECT-STATUS-AUDIT-2026-11-14.md"
PREFIX = "docs/reviews/audit-evidence/2026-11-14/"


def now():
    return datetime.now(timezone.utc).isoformat()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def write_new(path, content):
    with path.open("x", encoding="utf-8", newline="\n") as stream:
        stream.write(content)


def environment(extra=None):
    env = dict(os.environ)
    for key in list(env):
        if key.upper().startswith(("EPOCHA_", "BIDI_", "ADMIN_", "BAD_ADMIN_", "EXAMPLE_")) or key.upper() in {
            "NODE_OPTIONS", "NODE_TEST_CONTEXT", "BASH_ENV", "ENV", "SHELLOPTS", "MAKEFLAGS", "MFLAGS",
            "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN", "GOWORK", "GOENV", "GOPATH",
        }:
            del env[key]
    env.update(GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off", GOENV="off", GOWORK="off", GOFLAGS="-mod=readonly")
    env["PATH"] = str(GO.parent) + os.pathsep + env.get("PATH", "")
    if extra:
        env.update(extra)
    return env


def run(name, argv, timeout=1200, extra=None):
    receipt_path = OUT / (name + ".receipt.json")
    if receipt_path.exists() or (OUT / (name + ".stdout.log")).exists():
        raise RuntimeError("Refusing to overwrite an existing receipt: " + name)
    start = now()
    clock = time.monotonic()
    error = None
    rc = None
    try:
        result = subprocess.run(argv, cwd=ROOT, env=environment(extra), stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=timeout, check=False)
        stdout, stderr, rc = result.stdout, result.stderr, result.returncode
    except subprocess.TimeoutExpired as exc:
        stdout, stderr = exc.stdout or b"", exc.stderr or b""
        error = "TIMEOUT"
    except OSError as exc:
        stdout, stderr = b"", str(exc).encode("utf-8")
        error = type(exc).__name__
    for suffix, data in (("stdout", stdout), ("stderr", stderr)):
        with (OUT / (name + "." + suffix + ".log")).open("xb") as stream:
            stream.write(data)
    receipt = {
        "schema_version": 1, "name": name, "started_at_utc": start, "finished_at_utc": now(),
        "duration_seconds": round(time.monotonic() - clock, 3), "cwd": str(ROOT), "argv": list(map(str, argv)),
        "raw_exit_code": rc, "execution_error": error,
        "environment_policy": {"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOENV": "off", "GOWORK": "off", "GOFLAGS": "-mod=readonly", **(extra or {})},
        "stdout": {"path": name + ".stdout.log", "bytes": len(stdout), "sha256": digest(stdout)},
        "stderr": {"path": name + ".stderr.log", "bytes": len(stderr), "sha256": digest(stderr)},
    }
    write_new(receipt_path, json.dumps(receipt, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({"name": name, "raw_exit_code": rc, "error": error, "seconds": receipt["duration_seconds"],
                      "stdout_bytes": len(stdout), "stderr_bytes": len(stderr)}, ensure_ascii=False), flush=True)
    return receipt


def git_bytes(*args):
    result = subprocess.run(["git", *args], cwd=ROOT, env=environment(), capture_output=True, check=True)
    return result.stdout


def snapshot(name):
    paths = sorted(set(git_bytes("ls-files", "--cached", "--others", "--exclude-standard", "-z").decode("utf-8").split("\0")) - {""})
    files = []
    for rel in paths:
        if rel.startswith(PREFIX) or rel == REPORT:
            continue
        path = ROOT / rel
        data = path.read_bytes() if path.is_file() else None
        files.append({"path": rel, "bytes": len(data) if data is not None else None,
                      "sha256": digest(data) if data is not None else None})
    value = {"schema_version": 1, "captured_at_utc": now(), "commit": git_bytes("rev-parse", "HEAD").decode().strip(),
             "branch": git_bytes("branch", "--show-current").decode().strip(), "files": files,
             "snapshot_sha256": digest(json.dumps(files, ensure_ascii=False, separators=(",", ":")).encode("utf-8")),
             "excluded_audit_outputs": [PREFIX, REPORT],
             "host": {"platform": sys.platform, "machine": platform.machine(), "python": platform.python_version()},
             "go_executable": str(GO)}
    write_new(OUT / (name + ".json"), json.dumps(value, ensure_ascii=False, indent=2) + "\n")
    write_new(OUT / (name + ".status.txt"), git_bytes("status", "--porcelain=v1", "--untracked-files=all").decode("utf-8"))
    print(json.dumps({"snapshot": name, "files": len(files), "sha256": value["snapshot_sha256"]}), flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["initial", "final", "run"])
    parser.add_argument("--name")
    parser.add_argument("--timeout", type=int, default=1200)
    parser.add_argument("--env-json", default="{}")
    parser.add_argument("argv", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.action in {"initial", "final"}:
        snapshot("source-" + args.action)
        return
    if not args.name or not args.argv:
        parser.error("run requires --name and a command after --")
    argv = args.argv[1:] if args.argv[0] == "--" else args.argv
    argv = [str(GO) if item == "@GO@" else item for item in argv]
    receipt = run(args.name, argv, args.timeout, json.loads(args.env_json))
    sys.exit(0 if receipt["execution_error"] is None else 2)


if __name__ == "__main__":
    main()
