import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { createServer } from "node:http";
import { createFailureFixture } from "./browser/adminui_failure_fixture.mjs";

const ROOT = fileURLToPath(new URL("../..", import.meta.url));
const read = (path) => readFileSync(new URL(path, `file://${ROOT.replaceAll("\\", "/")}/`), "utf8");

const gate = read("docs/scripts/admin-ui-check.sh");
const browser = read("docs/scripts/browser/adminui_browser.test.mjs");

test("browser network failure never signals the shared gate-owned server", () => {
  assert.doesNotMatch(browser, /process\.kill\s*\(\s*Number\s*\(\s*ADMIN_SERVER_PID/);
  assert.doesNotMatch(browser, /ADMIN_SERVER_PID/);
});

test("real network-failure case uses an independently owned HTTP proxy fixture", () => {
  assert.match(browser, /adminui_failure_fixture\.mjs/);
  assert.match(browser, /createFailureFixture\(ADMIN_API\)/);
  assert.match(browser, /failureFixture\.setAPIUnavailable\(true\)/);
  assert.match(browser, /failureFixture\.blockedRequests/);
  const helper = read("docs/scripts/browser/adminui_failure_fixture.mjs");
  assert.match(helper, /createServer/);
  assert.match(helper, /httpRequest\(/);
  assert.match(helper, /socket\.destroy\(\)/);
  assert.match(helper, /async close\(/);
});

test("proxy failure is a real browser-visible connection refusal while upstream and normal forwarding remain live", async (t) => {
  const upstream = createServer((incoming, outgoing) => {
    outgoing.writeHead(200, { "Content-Type": "text/plain" });
    outgoing.end(`upstream:${incoming.url}`);
  });
  await new Promise((resolve) => upstream.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise((resolve) => upstream.close(resolve)));
  const address = upstream.address();
  const fixture = await createFailureFixture(`http://127.0.0.1:${address.port}`);
  t.after(() => fixture.close());

  const normal = await fetch(fixture.baseURL + "/admin");
  assert.equal(normal.status, 200);
  assert.equal(await normal.text(), "upstream:/admin");

  fixture.setAPIUnavailable(true);
  await assert.rejects(fetch(fixture.baseURL + "/api/admin/whoami"));
  assert.equal(fixture.blockedRequests, 1);

  fixture.setAPIUnavailable(false);
  const recovered = await fetch(fixture.baseURL + "/api/admin/whoami");
  assert.equal(recovered.status, 200);
  assert.equal(await recovered.text(), "upstream:/api/admin/whoami");
});

test("shared-service recovery reuses this run's DB and keys without replaying admin writes", () => {
  assert.match(gate, /start_admin_service\(\)/);
  const start = gate.indexOf("start_admin_service() {");
  const end = gate.indexOf("\n}\n", start);
  assert.ok(start >= 0 && end > start, "owned service restart function must be explicit");
  const restart = gate.slice(start, end);
  assert.match(restart, /EPOCHA_DB="\$WORK\/data\/epocha\.db"/);
  assert.match(restart, /EPOCHA_MASTER_KEY_FILE="\$WORK\/keys\/master"/);
  assert.match(restart, /EPOCHA_PEPPER_FILE="\$WORK\/keys\/pepper"/);
  assert.match(restart, /EPOCHA_ADMIN_KEY_FILE="\$WORK\/keys\/admin"/);
  assert.match(restart, /\/readyz/);
  assert.match(restart, /\/admin/);
  assert.doesNotMatch(restart, /-X (POST|PUT|PATCH|DELETE)|\/api\/admin\/sites/);
  assert.match(gate, /服务归属检查失败已记 FAIL[\s\S]*start_admin_service/);
  assert.match(gate, /start_admin_service[\s\S]*?恢复完成/);
});

test("all admin gate HTTP helpers use bounded connection and total timeouts without duplicate 000", () => {
  const helperStart = gate.indexOf("curl_status() {");
  const helperEnd = gate.indexOf("\n}\n", helperStart);
  assert.ok(helperStart >= 0 && helperEnd > helperStart, "missing shared curl status helper");
  const helper = gate.slice(helperStart, helperEnd);
  assert.match(helper, /--connect-timeout 2/);
  assert.match(helper, /--max-time 5/);
  assert.match(gate, /code_of\(\) \{ curl_status/);
  assert.match(gate, /acode_of\(\) \{[\s\S]*?curl_status/);
  const headerLine = gate.split(/\r?\n/).find((candidate) => candidate.startsWith("hdr_of()"));
  assert.ok(headerLine, "missing hdr_of");
  assert.match(headerLine, /--connect-timeout 2/);
  assert.match(headerLine, /--max-time 5/);
  assert.doesNotMatch(gate, /\|\| echo 000/);
});

test("the parent shell verifies its shared service before and after browser work", () => {
  assert.match(gate, /assert_admin_service\(\)/);
  const browserStart = gate.indexOf('log "第 8 步：真实浏览器交互验收');
  const browserExec = gate.indexOf("node --test --test-reporter=tap docs/scripts/browser/adminui_browser.test.mjs", browserStart);
  const postCheck = gate.indexOf('assert_admin_service "浏览器验收后"', browserExec);
  const nextWrites = gate.indexOf('log "第 9 步：真实写入一轮', browserStart);
  assert.ok(browserStart >= 0 && browserExec > browserStart && postCheck > browserExec && nextWrites > postCheck,
    "post-browser ownership/health assertion must run after the suite and before any remaining shared-service writes");
  assert.doesNotMatch(gate, /ADMIN_SERVER_PID=/);
});

test("gate cleanup reaps owned children and removes each tmpfs path separately", () => {
  assert.match(gate, /cleanup_child\(\)/);
  assert.match(gate, /kill -TERM/);
  assert.match(gate, /kill -KILL/);
  assert.match(gate, /wait "\$pid"/);
  assert.match(gate, /TMPFS_SCRATCH=\(\)/);
  assert.match(gate, /for path in "\$\{TMPFS_SCRATCH\[@\]\}"; do[\s\S]*rm -rf -- "\$path"/);
});

test("browser after hook closes owned browser resources even when tab/session cleanup fails", () => {
  assert.match(browser, /after\(async \(\) => \{\s*try\s*\{/);
  assert.match(browser, /finally\s*\{[\s\S]*failureFixture\?\.close\(\)/);
  assert.doesNotMatch(browser, /process\.kill\s*\(/);
});
