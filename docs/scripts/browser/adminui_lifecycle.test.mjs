// Node-only behavior tests run the production admin script without exposing its internals.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { setImmediate } from "node:timers/promises";
import { BROWSER_CASE_TITLES } from "./cases.mjs";
import { flush, deferred, harness, initialSite } from "./adminui_lifecycle_harness.mjs";

function caseTitle(id) {
  const title = BROWSER_CASE_TITLES[id];
  if (!title) throw new Error(`browser manifest missing ${id}`);
  return title;
}


test(caseTitle("adminlife.cancel-focus"), async (t) => {
  const h = harness(t); await h.detail();
  const writes = h.requests.filter((item) => item.options.method !== "GET").length;
  await h.click("停用此站点");
  assert.ok(h.document.activeElement === h.button("确认停用"), "confirmation button must be focused after insertion");
  await assert.doesNotReject(h.click("取消"));
  assert.equal(h.document.querySelectorAll("button").some((node) => node.textContent === "确认停用"), false);
  assert.equal(h.requests.filter((item) => item.options.method !== "GET").length, writes);
});

test(caseTitle("adminlife.cancel-reopen"), async (t) => {
  const h = harness(t); await h.detail();
  await h.click("停用此站点");
  await assert.doesNotReject(h.click("取消"));
  await h.click("停用此站点");
  await assert.doesNotReject(h.click("取消"));
  assert.equal(h.requests.filter((item) => item.options.method !== "GET").length, 0);
});

test(caseTitle("adminlife.stale-generators"), async (t) => {
  const h = harness(t), pending = deferred();
  h.queue("GET", "/api/admin/generators", () => pending.promise);
  await h.login();
  await h.click("退出"); await h.login("second-key");
  await h.input('.create-site input', "unsaved-new-session");
  const count = h.requests.length;
  pending.resolve(h.response({ max_difficulty: 5, generators: [] })); await flush();
  assert.equal(h.requests.length, count, "old view must not start its next request");
  assert.equal(h.document.querySelectorAll(".create-site").length, 1);
  await h.click("查看");
  assert.equal(h.find("#guard").hidden, false, "new session draft must remain dirty");
});

test(caseTitle("adminlife.renamed-toggle"), async (t) => {
  const h = harness(t); await h.detail();
  const policyName = h.find('[id^="pol-name"]'), area = h.find("textarea");
  await h.input(policyName, "renamed");
  await h.input('[id^="pol-pow"]', "20");
  await h.input(area, "https://draft.example.com");
  h.queue("PUT", "/api/admin/sites/1", (request) => {
    const saved = { ...initialSite(), ...JSON.parse(request.options.body), version: 2 };
    h.setSite(saved);
    return Promise.resolve(h.response({ site: saved }));
  });
  await h.click("保存修改"); await flush();
  const versionLabel = h.document.querySelectorAll("dt").find((node) => node.textContent === "站点版本");
  assert.ok(versionLabel, "overview must contain its version fact");
  const facts = versionLabel.parentNode.children;
  assert.equal(facts[facts.indexOf(versionLabel) + 1].textContent, "2", "overview must use the reread version");
  assert.ok(h.find('[id^="pol-name"]') === policyName, "metadata sync must preserve the policy form");
  assert.ok(h.find("textarea") === area, "metadata sync must preserve the origin form");
  assert.equal(area.value, "https://draft.example.com");
  await h.click("停用此站点");
  assert.match(h.find("#site-actions").textContent, /站点「renamed」/);
  assert.match(h.find("#site-actions").textContent, /当前版本 2/);
  await h.click("取消"); await h.click("站点");
  assert.equal(h.find("#guard").hidden, false, "origin draft must still guard navigation");
});

test(caseTitle("adminlife.reload-policy"), async (t) => {
  const h = harness(t); await h.detail();
  await h.input('[id^="pol-name"]', "unsaved");
  await h.input("textarea", "https://draft.example.com");
  const area = h.find("textarea");
  await h.click("放弃修改并重新读取"); await flush();
  assert.equal(h.find('[id^="pol-name"]').value, "alpha");
  assert.ok(h.find("textarea") === area, "origin form must not be rebuilt");
  assert.equal(area.value, "https://draft.example.com");
  await h.click("站点"); assert.equal(h.find("#guard").hidden, false);
});

test(caseTitle("adminlife.response-boundary"), async (t) => {
  for (const raw of ["x".repeat(300_000), "not-json", "null", "{}"] ) {
    const h = harness(t); await h.detail();
    await h.input('[id^="pol-name"]', "changed");
    h.queue("PUT", "/api/admin/sites/1", () => Promise.resolve({ status: 200, text: () => Promise.resolve(raw) }));
    await assert.doesNotReject(h.click("保存修改")); await flush();
    assert.equal(h.button("保存修改").disabled, false);
    assert.match(h.text(), /无法确认|尚未确认/);
    assert.doesNotMatch(h.text(), /已保存，并已重新读取/);
  }
});

test(caseTitle("adminlife.stale-view"), async (t) => {
  const h = harness(t); await h.login();
  const pending = deferred();
  h.queue("GET", "/api/admin/sites/1", () => pending.promise);
  await h.click("查看"); await flush(); await h.click("站点"); await flush();
  pending.resolve(h.response(h.stateData())); await flush();
  assert.equal(h.document.querySelectorAll(".create-site").length, 1);
  assert.equal(h.document.querySelectorAll("textarea").length, 0);
});

test(caseTitle("adminlife.origin-mismatch"), async (t) => {
  const h = harness(t); await h.detail();
  await h.input("textarea", "https://wanted.example.com");
  h.queue("PUT", "/api/admin/sites/1/origins", () => Promise.resolve(h.response({ origins: ["https://wanted.example.com"] })));
  await h.click("保存允许来源"); await flush();
  assert.match(h.text(), /不一致/);
  assert.equal(h.find("textarea").value, "https://wanted.example.com");
  await h.click("站点"); assert.equal(h.find("#guard").hidden, false);
});

test(caseTitle("adminlife.discard-origin"), async (t) => {
  const h = harness(t); await h.detail();
  await h.input("textarea", "https://abandoned.example.com");
  await h.click("站点"); await h.click("放弃修改并离开"); await flush();
  await h.click("查看"); await flush();
  assert.equal(h.find("textarea").value, "https://old.example.com");
});

for (const outcome of ["confirmed", "mismatch", "read failure"]) {
  test(caseTitle({ confirmed: "adminlife.policy-confirmed", mismatch: "adminlife.policy-mismatch", "read failure": "adminlife.policy-read-failure" }[outcome]), async (t) => {
    const h = harness(t); await h.detail();
    await h.input('[id^="pol-name"]', "changed");
    await h.input("textarea", "https://draft.example.com");
    const original = h.find("textarea");
    h.queue("PUT", "/api/admin/sites/1", () => {
      h.setSite({ ...initialSite(), name: "changed" });
      h.queue("GET", "/api/admin/sites/1", () => Promise.resolve(outcome === "read failure" ? h.response({}, 500) :
        h.response({ ...h.stateData(), site: { ...initialSite(), name: outcome === "mismatch" ? "other" : "changed" } })));
      return Promise.resolve(h.response({ site: { ...initialSite(), name: "changed" } }));
    });
    await h.click("保存修改"); await flush();
    assert.match(h.text(), outcome === "confirmed" ? /已保存，并已重新读取/ : outcome === "mismatch" ? /不一致/ : /无法确认/);
    assert.ok(h.find("textarea") === original, "origin draft DOM must survive policy save");
    assert.equal(original.value, "https://draft.example.com");
    await h.click("站点");
    assert.equal(h.find("#guard").hidden, false, "unsaved origin must still guard navigation");
  });
}

test(caseTitle("adminlife.parallel-forms"), async (t) => {
  const h = harness(t); await h.detail();
  const name = h.find('[id^="pol-name"]');
  await h.input(name, "changed");
  await h.input("textarea", "https://new.example.com");
  const pending = deferred();
  h.queue("PUT", "/api/admin/sites/1", () => pending.promise);
  const saving = h.click("保存修改"); await flush();
  assert.equal(name.disabled, true);
  assert.equal(h.find("textarea").disabled, false);
  await h.click("保存允许来源");
  assert.equal(name.value, "changed");
  h.setSite({ ...initialSite(), name: "changed" });
  pending.resolve(h.response({ site: { ...initialSite(), name: "changed" } }));
  await saving; await flush();
  assert.equal(name.disabled, false);
  assert.equal(h.find("textarea").value, "https://new.example.com");
});

test(caseTitle("adminlife.toggle-drafts"), async (t) => {
  const h = harness(t); await h.detail();
  const name = h.find('[id^="pol-name"]'), area = h.find("textarea");
  await h.input(name, "unsaved"); await h.input(area, "https://draft.example.com");
  await h.click("停用此站点"); await h.click("确认停用"); await flush();
  assert.ok(h.find('[id^="pol-name"]') === name, "toggle must not rebuild unsaved policy");
  assert.equal(name.value, "unsaved"); assert.equal(area.value, "https://draft.example.com");
  await h.click("站点"); assert.equal(h.find("#guard").hidden, false);
});

test(caseTitle("adminlife.unconfirmed-500"), async (t) => {
  const h = harness(t); await h.detail();
  await h.input("textarea", "https://draft.example.com");
  h.queue("PUT", "/api/admin/sites/1/origins", () => Promise.resolve(h.response({}, 500)));
  await h.click("保存允许来源"); await flush();
  assert.match(h.text(), /尚未确认|无法确认/);
  assert.doesNotMatch(h.text(), /没有.*生效|整批未生效|请重试/);
  const writes = h.requests.filter((item) => item.options.method !== "GET").length;
  await h.click(h.document.querySelectorAll("button").find((node) => /只读复核/.test(node.textContent)));
  assert.equal(h.requests.filter((item) => item.options.method !== "GET").length, writes);
});

test(caseTitle("adminlife.origin-reread"), async (t) => {
  const h = harness(t); await h.detail();
  await h.input("textarea", "https://NEW.example.com:443");
  const before = h.requests.length;
  await h.click("保存允许来源"); await flush();
  const requests = h.requests.slice(before);
  assert.equal(requests[0].options.method, "PUT");
  assert.ok(requests.slice(1).some((item) => item.options.method === "GET" && item.path.endsWith("/origins")));
  assert.match(h.text(), /已重新读取/);
  assert.equal(h.find("textarea").value, "https://new.example.com");
});

for (const stage of ["fetch", "body"]) {
  test(caseTitle(stage === "fetch" ? "adminlife.fetch-timeout" : "adminlife.body-timeout"), async (t) => {
    const h = harness(t); await h.detail();
    await h.input('[id^="pol-name"]', "changed");
    const pending = deferred(); let request;
    h.queue("PUT", "/api/admin/sites/1", (item) => {
      request = item;
      return stage === "fetch" ? pending.promise : Promise.resolve({ status: 200, text: () => pending.promise });
    });
    const saving = h.click("保存修改"); await flush();
    await h.time.advance(20_000);
    assert.equal(h.button("保存修改").disabled, false);
    assert.equal(request.options.signal?.aborted, true);
    assert.match(h.text(), /超时/);
    assert.match(h.text(), /尚未确认|无法确认/);
    const before = h.text();
    pending.resolve(stage === "fetch" ? h.response({ site: initialSite() }) : JSON.stringify({ site: initialSite() }));
    await saving; await flush();
    assert.equal(h.text(), before);
  });
}

test(caseTitle("adminlife.body-error"), async (t) => {
  const h = harness(t); await h.detail();
  await h.input('[id^="pol-name"]', "changed");
  h.queue("PUT", "/api/admin/sites/1", () => Promise.resolve({ status: 200, text: () => Promise.reject(new Error("body disconnected")) }));
  await assert.doesNotReject(h.click("保存修改"));
  assert.equal(h.button("保存修改").disabled, false);
  assert.match(h.text(), /尚未确认|无法确认/);
});

test(caseTitle("adminlife.logout-late"), async (t) => {
  const h = harness(t); await h.login();
  await h.input('.create-site input', "new-site");
  const pending = deferred();
  h.queue("POST", "/api/admin/sites", () => pending.promise);
  const creating = h.click("创建站点"); await flush();
  await h.click("退出");
  pending.resolve(h.response({ site: { ...initialSite(), id: 2 }, secret: "late-plaintext" }, 201));
  await creating; await flush();
  assert.equal(h.find("#view-body").textContent, "");
  assert.equal(h.document.body.textContent.includes("late-plaintext"), false);
  await h.login();
  const reading = deferred();
  h.queue("GET", "/api/admin/sites/1", () => reading.promise);
  await h.click("查看"); await flush(); await h.click("退出");
  reading.resolve(h.response(h.stateData())); await flush();
  assert.equal(h.find("#view-body").textContent, "");
});

test(caseTitle("adminlife.old-401"), async (t) => {
  const h = harness(t); await h.detail();
  await h.input('[id^="pol-name"]', "changed");
  const pending = deferred();
  h.queue("PUT", "/api/admin/sites/1", () => pending.promise);
  const saving = h.click("保存修改"); await flush();
  await h.click("退出"); await h.login("second-key");
  pending.resolve(h.response({}, 401)); await saving; await flush();
  assert.match(h.find("#session-state").textContent, /k-second/);
  assert.equal(h.find("#login").hidden, true);
});

test(caseTitle("adminlife.current-401"), async (t) => {
  const h = harness(t); await h.detail();
  await h.input('[id^="pol-name"]', "changed");
  h.queue("PUT", "/api/admin/sites/1", () => {
    h.queue("GET", "/api/admin/sites/1", () => Promise.resolve(h.response({}, 401)));
    return Promise.resolve(h.response({ site: { ...initialSite(), name: "changed" } }));
  });
  await h.click("保存修改"); await flush();
  assert.equal(h.find("#login").hidden, false);
  assert.equal(h.find("#view-body").textContent, "");
});
