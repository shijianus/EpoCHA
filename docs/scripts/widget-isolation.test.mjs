import test from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { existsSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ResourceScope, waitFor } from "./browser/widget_fixture.mjs";

class FakeChild extends EventEmitter {
  constructor({ ignoreTerm = false } = {}) {
    super();
    this.exitCode = null;
    this.signalCode = null;
    this.signals = [];
    this.ignoreTerm = ignoreTerm;
  }

  kill(signal) {
    this.signals.push(signal);
    if (signal === "SIGKILL" || (signal === "SIGTERM" && !this.ignoreTerm)) {
      setTimeout(() => {
        this.exitCode = 0;
        this.emit("exit", 0, signal);
      }, 0);
    }
    return true;
  }
}

test("each widget case owns a distinct workspace and closes only its own processes", async () => {
  const parent = await mkdtemp(join(tmpdir(), "epocha-widget-isolation-test-"));
  const first = new ResourceScope({ parentDir: parent, termTimeoutMs: 50, killTimeoutMs: 50 });
  const second = new ResourceScope({ parentDir: parent, termTimeoutMs: 50, killTimeoutMs: 50 });
  const childA = new FakeChild();
  const childB = new FakeChild();
  try {
    const [dirA, dirB] = await Promise.all([first.createWorkspace(), second.createWorkspace()]);
    assert.notEqual(dirA, dirB);
    assert.notEqual(join(dirA, "epocha.db"), join(dirB, "epocha.db"));
    first.add(childA);
    second.add(childB);

    await first.close();
    assert.deepEqual(childA.signals, ["SIGTERM"]);
    assert.deepEqual(childB.signals, []);
    assert.equal(existsSync(dirA), false);
    assert.equal(existsSync(dirB), true);
    await second.close();
    assert.deepEqual(childB.signals, ["SIGTERM"]);
    assert.equal(existsSync(dirB), false);
  } finally {
    await first.close();
    await second.close();
    await rm(parent, { recursive: true, force: true });
  }
});

test("startup readiness wait is cancelled promptly", async () => {
  const controller = new AbortController();
  const started = Date.now();
  const pending = waitFor(() => false, { signal: controller.signal, timeoutMs: 5000, intervalMs: 10 });
  setTimeout(() => controller.abort(new Error("case cancelled")), 20);
  await assert.rejects(pending, /case cancelled|aborted/i);
  assert.ok(Date.now() - started < 1000, "cancelled case retained its readiness timer");
});

test("process cleanup has a bounded TERM-to-KILL escalation", async () => {
  const parent = await mkdtemp(join(tmpdir(), "epocha-widget-cleanup-test-"));
  const scope = new ResourceScope({ parentDir: parent, termTimeoutMs: 30, killTimeoutMs: 80 });
  const child = new FakeChild({ ignoreTerm: true });
  try {
    const dir = await scope.createWorkspace();
    scope.add(child);
    const started = Date.now();
    await scope.close();
    const elapsed = Date.now() - started;
    assert.deepEqual(child.signals, ["SIGTERM", "SIGKILL"]);
    assert.ok(elapsed < 500, `cleanup exceeded bound: ${elapsed}ms`);
    assert.equal(existsSync(dir), false);
  } finally {
    await scope.close();
    await rm(parent, { recursive: true, force: true });
  }
});
