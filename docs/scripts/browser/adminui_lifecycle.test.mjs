// Node-only behavior tests run the production admin script without exposing its internals.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { setImmediate } from "node:timers/promises";
import { BROWSER_CASE_TITLES } from "./cases.mjs";

function caseTitle(id) {
  const title = BROWSER_CASE_TITLES[id];
  if (!title) throw new Error(`browser manifest missing ${id}`);
  return title;
}

const source = readFileSync(new URL("../../../web/adminui/app.js", import.meta.url), "utf8");
const flush = async () => { await setImmediate(); await setImmediate(); };
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

class Events {
  listeners = new Map();
  addEventListener(type, fn) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(fn);
  }
  removeEventListener(type, fn) {
    this.listeners.set(type, (this.listeners.get(type) || []).filter((item) => item !== fn));
  }
  dispatch(type, event = {}) {
    const ev = { type, target: this, defaultPrevented: false,
      preventDefault() { this.defaultPrevented = true; }, ...event };
    const pending = [];
    for (let node = this; node; node = ev.bubbles ? node.parentNode : null) {
      for (const fn of [...(node.listeners.get(type) || [])]) pending.push(fn(ev));
    }
    return { event: ev, done: Promise.all(pending) };
  }
}

function descendants(root) {
  return root.children.flatMap((node) => [node, ...descendants(node)]);
}
function simpleMatch(node, selector) {
  if (!node.tagName) return false;
  const tag = selector.match(/^[a-zA-Z][\w-]*/)?.[0];
  if (tag && node.tagName !== tag.toUpperCase()) return false;
  for (const [, id] of selector.matchAll(/#([\w-]+)/g)) if (node.id !== id) return false;
  for (const [, name] of selector.matchAll(/\.([\w-]+)/g)) if (!node.classList.contains(name)) return false;
  for (const [, key, operator, value] of selector.matchAll(/\[([\w-]+)(?:([\^]?=)['"]?([^'"\]]*)['"]?)?\]/g)) {
    if (!node.hasAttribute(key)) return false;
    if (operator === "=" && node.getAttribute(key) !== value) return false;
    if (operator === "^=" && !node.getAttribute(key).startsWith(value)) return false;
  }
  return true;
}
function matches(node, selector) {
  const parts = selector.trim().split(/\s+/);
  if (!simpleMatch(node, parts.pop())) return false;
  let parent = node.parentNode;
  while (parts.length) {
    const part = parts.pop();
    while (parent && !simpleMatch(parent, part)) parent = parent.parentNode;
    if (!parent) return false;
    parent = parent.parentNode;
  }
  return true;
}
class Element extends Events {
  constructor(tag, document) {
    super();
    this.tagName = tag.toUpperCase();
    this.ownerDocument = document;
    this.children = [];
    this.parentNode = null;
    this.attributes = new Map();
    this.dataset = {};
    this.valueText = "";
    this.checked = false;
    this.disabled = false;
    this.hidden = false;
    this.text = "";
    this.classList = {
      contains: (name) => this.className.split(/\s+/).includes(name),
      add: (name) => { if (!this.classList.contains(name)) this.className = (this.className + " " + name).trim(); },
      remove: (name) => { this.className = this.className.split(/\s+/).filter((item) => item !== name).join(" "); },
      toggle: (name, force) => {
        const on = force === undefined ? !this.classList.contains(name) : force;
        this.classList[on ? "add" : "remove"](name);
        return on;
      },
    };
  }
  get className() { return this.attributes.get("class") || ""; }
  set className(value) { this.attributes.set("class", String(value)); }
  get id() { return this.attributes.get("id") || ""; }
  get type() { return this.attributes.get("type") || (this.tagName === "BUTTON" ? "submit" : "text"); }
  get value() {
    if (this.tagName === "SELECT") {
      return this.children.find((node) => node.selected)?.value || this.children[0]?.value || "";
    }
    return this.valueText;
  }
  set value(value) {
    this.valueText = String(value);
    if (this.tagName === "SELECT") this.children.forEach((node) => { node.selected = node.value === String(value); });
  }
  setAttribute(key, value) {
    this.attributes.set(key, String(value));
    if (["disabled", "hidden", "selected", "readonly"].includes(key)) this[key === "readonly" ? "readOnly" : key] = true;
    if (key === "value") this.value = value;
    if (key.startsWith("data-")) this.dataset[key.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = String(value);
  }
  getAttribute(key) { return this.attributes.get(key) ?? null; }
  hasAttribute(key) { return this.attributes.has(key); }
  removeAttribute(key) {
    this.attributes.delete(key);
    if (["disabled", "hidden", "selected"].includes(key)) this[key] = false;
  }
  appendChild(child) {
    child.parentNode?.removeChild(child);
    this.children.push(child);
    child.parentNode = this;
    return child;
  }
  removeChild(child) {
    assert.ok(this.children.includes(child));
    this.children.splice(this.children.indexOf(child), 1);
    child.parentNode = null;
    return child;
  }
  replaceChild(next, old) {
    const index = this.children.indexOf(old);
    assert.ok(index >= 0);
    next.parentNode?.removeChild(next);
    this.children[index] = next;
    next.parentNode = this;
    old.parentNode = null;
    return old;
  }
  get firstChild() { return this.children[0] || null; }
  get textContent() { return this.text + this.children.map((child) => child.textContent).join(""); }
  set textContent(value) {
    for (const child of [...this.children]) this.removeChild(child);
    this.text = String(value);
  }
  get isConnected() { return this === this.ownerDocument.body || !!this.parentNode?.isConnected; }
  get elements() { return descendants(this).filter((node) => /^(INPUT|SELECT|TEXTAREA|BUTTON|FIELDSET)$/.test(node.tagName)); }
  contains(node) { return this === node || this.children.some((child) => child.contains(node)); }
  querySelectorAll(selector) { return descendants(this).filter((node) => selector.split(",").some((part) => matches(node, part))); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  closest(selector) {
    for (let node = this; node; node = node.parentNode) if (matches(node, selector)) return node;
    return null;
  }
  focus() { if (this.isConnected && !this.disabled) this.ownerDocument.activeElement = this; }
  select() { this.selectionStart = 0; this.selectionEnd = this.value.length; this.focus(); }
}
function clock() {
  let now = 0, nextID = 0;
  const timers = new Map();
  return {
    timers,
    setTimeout(fn, delay = 0) { const id = ++nextID; timers.set(id, { fn, at: now + delay }); return id; },
    clearTimeout(id) { timers.delete(id); },
    async advance(ms) {
      const end = now + ms;
      for (let i = 0; ; i++) {
        assert.ok(i < 100, "bounded timer processing");
        const next = [...timers].sort((a, b) => a[1].at - b[1].at)[0];
        if (!next || next[1].at > end) break;
        timers.delete(next[0]); now = next[1].at; next[1].fn(); await flush();
      }
      now = end; await flush();
    },
  };
}
const initialSite = () => ({ id: 1, name: "alpha", site_key: "pk_alpha", enabled: true, version: 1,
  pow_bits: 18, verify_mode: "pow_only", generator_id: "arithmetic-baseline-v1", difficulty: 1,
  challenge_ttl_sec: 180, response_ttl_sec: 120, rate_ip_rpm: 60, rate_site_rpm: 300,
  allow_missing_origin: false, created_at: "2026-11-02T00:00:00Z" });
function harness(t) {
  const document = new Events();
  document.readyState = "complete";
  document.body = new Element("body", document);
  document.createElement = (tag) => new Element(tag, document);
  document.createTextNode = (text) => { const node = new Element("#text", document); node.textContent = text; return node; };
  document.querySelectorAll = (selector) => document.body.querySelectorAll(selector);
  document.querySelector = (selector) => document.body.querySelector(selector);
  document.getElementById = (id) => document.querySelector("#" + id);
  const make = (tag, id, parent = document.body, attrs = {}) => {
    const node = document.createElement(tag);
    node.setAttribute("id", id);
    for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, value);
    return parent.appendChild(node);
  };
  make("span", "session-state");
  make("button", "logout", document.body, { type: "button" }).textContent = "退出";
  const login = make("section", "login");
  make("h1", "login-title", login);
  const loginForm = make("form", "login-form", login);
  make("input", "admin-key", loginForm, { type: "password" });
  make("button", "login-submit", loginForm, { type: "submit" }).textContent = "验证身份";
  make("p", "login-status", login);
  const nav = make("nav", "nav");
  make("button", "sites-tab", nav, { class: "tab", "data-view": "sites", type: "button" }).textContent = "站点";
  make("div", "guard");
  const view = make("section", "view");
  make("h1", "view-title", view);
  make("div", "view-body", view);
  make("div", "toast");
  const time = clock(), requests = [];
  let site = initialSite(), origins = ["https://old.example.com"];
  const responders = [];
  const response = (data, status = 200) => ({ status, text: () => Promise.resolve(JSON.stringify(data)) });
  const stateData = () => ({ site: { ...site }, origins: [...origins], secrets: [] });
  const window = new Events();
  window.location = new URL("https://admin.invalid/admin");
  const sandbox = { window, document, navigator: {}, AbortController, TextDecoder, TextEncoder, URL, console,
    setTimeout: time.setTimeout, clearTimeout: time.clearTimeout,
    fetch(url, options) {
      const path = new URL(url).pathname + new URL(url).search;
      const request = { path, options };
      requests.push(request);
      const index = responders.findIndex((item) => item.method === options.method && item.path === path);
      if (index >= 0) return responders.splice(index, 1)[0].run(request);
      if (path === "/api/admin/whoami") return Promise.resolve(response({ key_id: options.headers.Authorization?.includes("second") ? "k-second" : "k-first" }));
      if (path === "/api/admin/generators") return Promise.resolve(response({ max_difficulty: 5, generators: [{ id: "arithmetic-baseline-v1", media: "text", version: "v1", experimental: true, production_ready: false }] }));
      if (path.startsWith("/api/admin/sites?")) return Promise.resolve(response({ sites: [{ ...site }], next_after_id: 1 }));
      if (path === "/api/admin/sites/1" && options.method === "PUT") site = { ...site, ...JSON.parse(options.body) };
      if (path === "/api/admin/sites/1") return Promise.resolve(response(stateData()));
      if (path.endsWith("/origins")) {
        if (options.method === "PUT") origins = JSON.parse(options.body).origins.map((value) => value.toLowerCase().replace(":443", "")).sort();
        return Promise.resolve(response({ origins: [...origins] }));
      }
      if (/\/(enable|disable)$/.test(path)) { site.enabled = path.endsWith("/enable"); site.version++; return Promise.resolve(response({ site: { ...site } })); }
      throw new Error("fixture has no route: " + options.method + " " + path);
    },
  };
  vm.runInNewContext(source, sandbox, { filename: "web/adminui/app.js" });
  const h = {
    document, time, requests, response, stateData,
    setSite(next) { site = { ...next }; },
    queue(method, path, run) { responders.push({ method, path, run }); },
    find(selector) { const node = document.querySelector(selector); assert.ok(node, "missing " + selector); return node; },
    button(text) {
      const node = document.querySelectorAll("button").find((node) => node.textContent === text && h.visible(node));
      assert.ok(node, "missing visible button " + text); return node;
    },
    visible(node) { for (; node; node = node.parentNode) if (node.hidden) return false; return true; },
    click(node) {
      if (typeof node === "string") node = h.button(node);
      if (node.disabled) return Promise.resolve();
      const event = node.dispatch("click");
      if (!event.event.defaultPrevented && node.type === "submit") {
        const form = node.closest("form");
        if (form) return Promise.all([event.done, form.dispatch("submit").done]);
      }
      return event.done;
    },
    input(node, value) {
      if (typeof node === "string") node = h.find(node);
      assert.equal(node.disabled, false, "a user cannot edit a disabled control");
      node.value = value;
      return node.dispatch("input", { bubbles: true }).done;
    },
    async login(key = "first-key") { await h.input("#admin-key", key); await h.click("验证身份"); await flush(); },
    async detail() { await h.login(); await h.click("查看"); await flush(); assert.ok(document.body.textContent.includes("保存允许来源")); },
    text() { return document.querySelectorAll(".status").filter((node) => h.visible(node)).map((node) => node.textContent).join("\n"); },
  };
  t.after(() => { for (const id of [...time.timers.keys()]) time.clearTimeout(id); });
  return h;
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
