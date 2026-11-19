"""Copy the frozen Git-visible source into a fresh local clone; no network or commits."""
from pathlib import Path
import importlib.util
import json
import shutil
import subprocess
import tempfile
import time

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("audit_runner", HERE / "audit_runner.py")
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)
initial = json.loads((HERE / "source-initial.json").read_text(encoding="utf-8"))
parent = Path(tempfile.mkdtemp(prefix="epocha-audit-20261114-"))
source = parent / "source"
commands = []


def command(argv, cwd):
    started = r.now()
    clock = time.monotonic()
    p = subprocess.run(argv, cwd=cwd, env=r.environment(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=1800)
    commands.append({"argv": list(map(str, argv)), "cwd": str(cwd), "started_at_utc": started,
                     "duration_seconds": round(time.monotonic() - clock, 3), "raw_exit_code": p.returncode,
                     "stdout_sha256": r.digest(p.stdout), "stderr_sha256": r.digest(p.stderr)})
    return p


p = command(["git", "clone", "--local", "--no-hardlinks", "--no-checkout", str(r.ROOT), str(source)], r.ROOT)
if p.returncode:
    raise RuntimeError(p.stderr.decode("utf-8", errors="replace"))
p = command(["git", "checkout", "--detach", initial["commit"]], source)
if p.returncode:
    raise RuntimeError(p.stderr.decode("utf-8", errors="replace"))
for item in initial["files"]:
    rel = item["path"]
    origin = r.ROOT / rel
    data = origin.read_bytes()
    if r.digest(data) != item["sha256"]:
        raise RuntimeError("Source drift before isolation: " + rel)
    target = source / rel
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(data)

manifest = [{"path": item["path"], "bytes": (source / item["path"]).stat().st_size,
             "sha256": r.digest((source / item["path"]).read_bytes())} for item in initial["files"]]
expected = initial["files"]
if manifest != expected:
    raise RuntimeError("Isolated source differs from initial source manifest")
r.write_new(HERE / "isolated-source-manifest.json", json.dumps({
    "source_path": str(source), "created_at_utc": r.now(), "source_files": len(manifest),
    "matches_initial_source_manifest": True, "source_snapshot_sha256": initial["snapshot_sha256"],
    "scope": "Git-visible tracked and untracked source only; original ignored artifacts intentionally not copied",
    "clone_commands": commands, "deleted_original_files": False, "created_commits": False, "network_used": False,
}, ensure_ascii=False, indent=2) + "\n")
argv = [shutil.which("node"), "docs/scripts/release-check.mjs", "--profile", "full", "--out", "release-artifacts/audit-isolated-full"]
p = command(argv, source)
for kind, data in (("stdout", p.stdout), ("stderr", p.stderr)):
    with (HERE / ("isolated-release-full." + kind + ".log")).open("xb") as stream:
        stream.write(data)
r.write_new(HERE / "isolated-release-full.receipt.json", json.dumps({
    "schema_version": 1, "name": "isolated-release-full", "cwd": str(source), "argv": argv,
    "raw_exit_code": p.returncode, "command_receipt": commands[-1], "finished_at_utc": r.now(),
    "stdout": {"path": "isolated-release-full.stdout.log", "bytes": len(p.stdout), "sha256": r.digest(p.stdout)},
    "stderr": {"path": "isolated-release-full.stderr.log", "bytes": len(p.stderr), "sha256": r.digest(p.stderr)},
}, ensure_ascii=False, indent=2) + "\n")
gate = source / "release-artifacts" / "audit-isolated-full"
copy = HERE / "isolated-release-gate"
copy.mkdir()
for path in sorted(gate.iterdir()):
    if path.is_file():
        shutil.copyfile(path, copy / path.name)
print(json.dumps({"source_path": str(source), "raw_exit_code": p.returncode,
                  "summary": str(copy / "summary.json")}, ensure_ascii=False), flush=True)
