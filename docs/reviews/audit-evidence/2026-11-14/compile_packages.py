"""Cross-compile each package to a unique test binary; these are not executed."""
from pathlib import Path
import importlib.util
import json

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("audit_runner", HERE / "audit_runner.py")
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)
text = (HERE / "go-list-linux-project.stdout.log").read_text(encoding="utf-8")
decoder = json.JSONDecoder()
packages = []
while text.strip():
    item, end = decoder.raw_decode(text.lstrip())
    text = text.lstrip()[end:]
    packages.append(item)
extra = {"GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0"}
results = []
for i, pkg in enumerate(packages):
    if not (pkg.get("TestGoFiles") or pkg.get("XTestGoFiles")):
        results.append({"package": pkg["ImportPath"], "status": "NO_TEST_FILES"})
        continue
    name = f"linux-test-compile-{i:02d}"
    target = r.ROOT / "release-artifacts" / "audit-2026-11-14-cross" / "tests" / f"{i:02d}-{pkg['Name']}.test"
    receipt = r.run(name, [str(r.GO), "test", "-c", "-o", str(target), pkg["ImportPath"]], timeout=1200, extra=extra)
    results.append({"package": pkg["ImportPath"], "status": "COMPILE_PASS" if receipt["raw_exit_code"] == 0 else "COMPILE_FAIL",
                    "executed": False, "receipt": name + ".receipt.json", "binary": str(target.relative_to(r.ROOT))})
r.write_new(HERE / "linux-test-compilation-summary.json", json.dumps(results, ensure_ascii=False, indent=2) + "\n")
