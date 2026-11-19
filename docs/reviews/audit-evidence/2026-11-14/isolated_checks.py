"""Run bounded follow-up checks in the isolated, hash-matched source copy."""
from pathlib import Path
import importlib.util
import json
import shutil
import subprocess
import time

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("audit_runner", HERE / "audit_runner.py")
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)
source = Path(json.loads((HERE / "isolated-source-manifest.json").read_text(encoding="utf-8"))["source_path"])


def execute(name, argv, extra=None, timeout=600):
    start = r.now()
    clock = time.monotonic()
    p = subprocess.run(argv, cwd=source, env=r.environment(extra), stdout=subprocess.PIPE,
                       stderr=subprocess.PIPE, timeout=timeout, check=False)
    rec = {"schema_version": 1, "name": name, "cwd": str(source), "argv": list(map(str, argv)),
           "raw_exit_code": p.returncode, "started_at_utc": start, "finished_at_utc": r.now(),
           "duration_seconds": round(time.monotonic() - clock, 3), "execution_error": None,
           "extra_environment": extra or {}}
    for key, data in (("stdout", p.stdout), ("stderr", p.stderr)):
        filename = name + "." + key + ".log"
        with (HERE / filename).open("xb") as f:
            f.write(data)
        rec[key] = {"path": filename, "bytes": len(data), "sha256": r.digest(data)}
    r.write_new(HERE / (name + ".receipt.json"), json.dumps(rec, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({"name": name, "raw_exit_code": p.returncode, "seconds": rec["duration_seconds"]}), flush=True)


execute("isolated-build-receipt-integration", [shutil.which("node"), "--test", "--test-reporter=tap", "docs/scripts/build-receipt.integration.test.mjs"], timeout=650)
execute("isolated-go-full", [str(r.GO), "test", "-json", "-count=1", "./..."], timeout=600)
execute("isolated-go-vet-linux-full", [str(r.GO), "vet", "./..."], {"GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0"})
for name in ["FuzzNormalizeNewEncodings", "FuzzCrowdFindNormalize", "FuzzProgressiveTargetNormalize", "FuzzShellTrackNormalize", "FuzzSilhouetteNormalize", "FuzzSymbolPathNormalize", "FuzzTileRotationNormalize", "FuzzVisualRankNormalize"]:
    execute("fuzz-" + name, [str(r.GO), "test", "./internal/generator", "-run=^$", "-fuzz=^" + name + "$", "-fuzztime=5s", "-parallel=2"], timeout=120)
