"""Verify the audit delivery offline. No network, project writes, or secret reads."""
from pathlib import Path
import csv
import hashlib
import json
import math
import sys

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[3]


def read_json(path):
    return json.loads(path.read_text(encoding="utf-8"))


def sha(data):
    return hashlib.sha256(data).hexdigest()


def check(condition, message):
    if not condition:
        raise RuntimeError(message)


def wilson(solved, n):
    z = 1.959963984540054
    p = solved / n
    center = (p + z*z/(2*n)) / (1 + z*z/n)
    half = z*math.sqrt(p*(1-p)/n + z*z/(4*n*n)) / (1 + z*z/n)
    return center-half, center+half


def main():
    manifest = read_json(HERE / "SHA256SUMS.json")
    check(bool(manifest["files"]), "Empty evidence manifest")
    for item in manifest["files"]:
        path = ROOT / item["path"]
        check(path.is_file(), "Missing delivery file: " + item["path"])
        data = path.read_bytes()
        check(len(data) == item["bytes"] and sha(data) == item["sha256"], "Delivery hash mismatch: " + item["path"])
    receipt_count = 0
    for path in sorted(HERE.glob("*.receipt.json")):
        receipt = read_json(path)
        for kind in ["stdout", "stderr"]:
            item = receipt.get(kind)
            if not item:
                continue
            data = (HERE / item["path"]).read_bytes()
            check(len(data) == item["bytes"] and sha(data) == item["sha256"], "Receipt log mismatch: " + path.name)
        receipt_count += 1
    initial = read_json(HERE / "source-initial.json")
    final = read_json(HERE / "source-final.json")
    check(initial["files"] == final["files"] and initial["snapshot_sha256"] == final["snapshot_sha256"], "Audited source changed between snapshots")
    current_matches = 0
    for item in initial["files"]:
        path = ROOT / item["path"]
        if path.is_file() and sha(path.read_bytes()) == item["sha256"]:
            current_matches += 1
    summary = read_json(HERE / "isolated-release-gate/summary.json")
    check(summary["result"] == "INCOMPLETE" and summary["exit_code"] == 3 and summary["release_ready"] is False, "Release result changed")
    m4 = []
    for directory in [HERE / "m4-replication", ROOT / "docs/reviews/evidence"]:
        with (directory / "m4-results.csv").open(encoding="utf-8", newline="") as f:
            rows = list(csv.DictReader(f))
        data = read_json(directory / "m4-summary.json")
        check(len(rows) == 7000 and len(data["results"]) == 7, "M4 empty or incomplete denominator")
        total_solved = 0
        for result in data["results"]:
            samples = [r for r in rows if r["generator"] == result["generator"]]
            check(len(samples) == 1000 and sorted(int(r["index"]) for r in samples) == list(range(1000)), "M4 index gap/duplicate")
            solved = sum(r["solved"] == "true" for r in samples)
            lo, hi = wilson(solved, len(samples))
            check(solved == result["solved"] and abs(solved/1000-result["success_rate"]) < 1e-12, "M4 numerator mismatch")
            check(abs(lo-result["ci95_low"]) < 1e-6 and abs(hi-result["ci95_high"]) < 1e-6, "M4 confidence interval mismatch")
            total_solved += solved
        check(total_solved == data["overall"]["solved"], "M4 aggregate mismatch")
        m4.append({"directory": str(directory.relative_to(ROOT)), "solved": total_solved, "total": len(rows)})
    print(json.dumps({"verification": "PASS", "delivery_files_checked": len(manifest["files"]),
                      "receipts_checked": receipt_count, "source_start_end_unchanged": True,
                      "current_source_files_matching": current_matches, "audited_source_files": len(initial["files"]),
                      "current_source_is_same_snapshot": current_matches == len(initial["files"]),
                      "release_result": "INCOMPLETE", "release_exit_code": 3, "m4_counts_verified": m4,
                      "limitations": "Hashes are integrity checks, not signatures. Audit verification PASS is not project release/test PASS. Current source may legitimately change after the audit."}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print("EVIDENCE_VERIFICATION_FAIL: " + str(exc), file=sys.stderr)
        sys.exit(1)
