"""Compile and vet the explicit project scope; never execute cross-built Linux artifacts."""
from pathlib import Path
import importlib.util
import json

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("audit_runner", HERE / "audit_runner.py")
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)

extra = {"GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0"}
products = runner.ROOT / "release-artifacts" / "audit-2026-11-14-cross"
products.mkdir(exist_ok=False)
(products / "tests").mkdir()

for name, args in [
    ("go-list-linux-project", ["list", "-json", "./cmd/...", "./internal/...", "./web/..."]),
    ("go-vet-linux-project", ["vet", "./cmd/...", "./internal/...", "./web/..."]),
    ("go-test-compile-linux-project", ["test", "-c", "-o", str(products / "tests"), "./cmd/...", "./internal/...", "./web/..."]),
    ("build-linux-server", ["build", "-trimpath", "-ldflags=-s -w", "-o", str(products / "epocha"), "./cmd/server"]),
    ("build-linux-ops", ["build", "-trimpath", "-ldflags=-s -w", "-o", str(products / "epocha-ops"), "./cmd/ops"]),
    ("server-dependencies", ["list", "-deps", "-json", "./cmd/server"]),
]:
    runner.run(name, [str(runner.GO), *args], timeout=1800, extra=extra)

manifest = []
for name in ["epocha", "epocha-ops"]:
    path = products / name
    if path.is_file():
        data = path.read_bytes()
        manifest.append({"path": str(path.relative_to(runner.ROOT)).replace("\\", "/"), "bytes": len(data),
                         "sha256": runner.digest(data), "magic_hex": data[:20].hex(),
                         "budget_bytes": 35 * 1024 * 1024, "within_size_budget": 0 < len(data) <= 35 * 1024 * 1024,
                         "target": "linux/arm64", "cgo_enabled": "0", "executed": False,
                         "version_ldflags": "-s -w only; not a formal version-stamped candidate"})
runner.write_new(HERE / "cross-build-manifest.json", json.dumps(manifest, ensure_ascii=False, indent=2) + "\n")
print(json.dumps(manifest, ensure_ascii=False), flush=True)
