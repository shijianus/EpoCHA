"""Independently summarize machine evidence without turning missing checks into PASS."""
from pathlib import Path
import collections
import csv
import hashlib
import json
import math
import re
import statistics
import subprocess

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[3]


def load(path):
    return json.loads(path.read_text(encoding="utf-8"))


def events(name):
    return [json.loads(line) for line in (HERE / (name + ".stdout.log")).read_text(encoding="utf-8").splitlines() if line.strip()]


def summarize_go(name):
    ev = events(name)
    tests = [e for e in ev if e.get("Test") and e.get("Action") in {"pass", "fail", "skip"}]
    return {"receipt": name + ".receipt.json", "raw_exit_code": load(HERE / (name + ".receipt.json"))["raw_exit_code"],
            "test_event_counts": dict(collections.Counter(e["Action"] for e in tests)),
            "top_level_tests": dict(collections.Counter(e["Action"] for e in tests if "/" not in e["Test"])),
            "package_outcomes": [{"package": e["Package"], "action": e["Action"], "elapsed": e.get("Elapsed")} for e in ev if not e.get("Test") and e.get("Action") in {"pass", "fail", "skip"}],
            "coverage": [{"package": e.get("Package"), "text": e["Output"].strip()} for e in ev if e.get("Action") == "output" and e.get("Output", "").startswith("coverage:")],
            "build_errors": [e["Output"].strip() for e in ev if e.get("Action") == "build-output"]}


def wilson(solved, n):
    z = 1.959963984540054
    p = solved / n
    d = 1 + z*z/n
    center = (p + z*z/(2*n)) / d
    half = z*math.sqrt(p*(1-p)/n + z*z/(4*n*n)) / d
    return [max(0.0, center-half), min(1.0, center+half)]


def m4_check(directory):
    summary = load(directory / "m4-summary.json")
    with (directory / "m4-results.csv").open(encoding="utf-8", newline="") as f:
        rows = list(csv.DictReader(f))
    results = []
    all_ok = True
    for result in summary["results"]:
        g = result["generator"]
        samples = [r for r in rows if r["generator"] == g]
        solved = sum(r["solved"] == "true" for r in samples)
        indexes = sorted(int(r["index"]) for r in samples)
        ci = wilson(solved, len(samples))
        ok = (len(samples) == 1000 and indexes == list(range(1000)) and solved == result["solved"]
              and abs(solved/len(samples)-result["success_rate"]) < 1e-12
              and max(abs(ci[0]-result["ci95_low"]), abs(ci[1]-result["ci95_high"])) < 1e-6
              and all(int(r["generation_attempts"]) >= 1 for r in samples))
        all_ok = all_ok and ok
        results.append({"generator": g, "total": len(samples), "solved": solved,
                        "rate": solved/len(samples), "independent_wilson95": ci,
                        "p95_cpu_ms_reported": result["p95_cpu_ms"], "p95_wall_ms_reported": result["p95_wall_ms"],
                        "parse_failed": sum(r["parse_failed"] == "true" for r in samples),
                        "gave_up": sum(r["gave_up"] == "true" for r in samples),
                        "generation_retries": sum(int(r["generation_attempts"])-1 for r in samples),
                        "unique_indexes_and_summary_counts_match": ok, "measured_verdict": result["verdict"]})
    solved = sum(r["solved"] == "true" for r in rows)
    all_ok = all_ok and len(rows) == 7000 and solved == summary["overall"]["solved"]
    return {"directory": str(directory.relative_to(ROOT)).replace("\\", "/"),
            "aggregate_integrity_pass": all_ok, "total": len(rows), "solved": solved,
            "rate": solved/len(rows), "wilson95": wilson(solved, len(rows)), "results": results,
            "validation_scope": "CSV indexing/counts/denominators/Wilson independently recomputed; no claim of signed provenance or end-to-end HTTP/PoW cost"}


receipts = []
for path in sorted(HERE.glob("*.receipt.json")):
    rec = load(path)
    for kind in ["stdout", "stderr"]:
        item = rec.get(kind)
        if item:
            data = (HERE / item["path"]).read_bytes()
            if hashlib.sha256(data).hexdigest() != item["sha256"] or len(data) != item["bytes"]:
                raise RuntimeError("Receipt log mismatch: " + path.name)
    receipts.append({"file": path.name, "name": rec["name"], "raw_exit_code": rec["raw_exit_code"],
                     "execution_error": rec.get("execution_error"), "duration_seconds": rec.get("duration_seconds")})

gate = load(HERE / "isolated-release-gate/summary.json")
node = [{"id": c["id"], **c["counts"]} for c in gate["checks"] if c.get("counts", {}).get("tests") is not None]
project = {}
for name in ["cmd", "internal", "web"]:
    paths = list((ROOT / name).rglob("*.go"))
    project[name] = {"go_files": len(paths), "test_files": sum(p.name.endswith("_test.go") for p in paths),
                     "go_lines": sum(len(p.read_text(encoding="utf-8").splitlines()) for p in paths)}
text = "\n".join(p.read_text(encoding="utf-8") for name in ["cmd", "internal", "web"] for p in (ROOT/name).rglob("*_test.go"))
project["top_level_test_functions_static_count"] = len(re.findall(r"^func Test\w+\(", text, re.M))
project["fuzz_functions_static_count"] = len(re.findall(r"^func Fuzz\w+\(", text, re.M))

value = {"schema_version": 1, "purpose": "Auditable measurements, not release approval",
         "project_inventory": project, "receipts_log_hashes_verified": True, "receipts": receipts,
         "go_native_coverage": summarize_go("go-native-coverage"), "go_full_original": summarize_go("go-test-full"),
         "go_full_isolated": summarize_go("isolated-go-full"),
         "release_isolated": {"result": gate["result"], "raw_exit_code": gate["exit_code"], "release_ready": gate["release_ready"],
                              "check_counts": dict(collections.Counter(c["status"] for c in gate["checks"])),
                              "node_suites": node, "node_unique_tests_in_profile": sum(c["tests"] for c in node),
                              "checks": [{k:v for k,v in c.items() if k not in {"command"}} for c in gate["checks"]]},
         "cross_test_compilation": load(HERE / "linux-test-compilation-summary.json"),
         "cross_builds": load(HERE / "cross-build-manifest.json"),
         "m4_new": m4_check(HERE / "m4-replication"),
         "m4_existing": m4_check(ROOT / "docs/reviews/evidence"),
         "exclusions": ["Linux runtime/race/server DB integration", "real browsers and human accessibility/mobile", "target-host soak and public TLS", "fresh external OCR/AI model evaluation"]}
import sys
path = HERE / (sys.argv[1] if len(sys.argv) > 1 else "RESULTS.json")
with path.open("x", encoding="utf-8", newline="\n") as f:
    json.dump(value, f, ensure_ascii=False, indent=2)
    f.write("\n")
print(json.dumps({"receipts": len(receipts), "node_tests": sum(c["tests"] for c in node),
                  "m4_new_integrity": value["m4_new"]["aggregate_integrity_pass"],
                  "m4_existing_integrity": value["m4_existing"]["aggregate_integrity_pass"],
                  "report": str(path)}, ensure_ascii=False))
