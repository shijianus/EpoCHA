// 共享的 adminui DOM 仿真 harness（从 adminui_lifecycle.test.mjs 提取；ADR-034 §7 的
// 生成器默认回归复用同一套设施，不复制生产脚本逻辑）。
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { setImmediate } from "node:timers/promises";

const source = readFileSync(new URL("../../../web/adminui/app.js", import.meta.url), "utf8");

export const flush = async () => { await setImmediate(); await setImmediate(); };
export function deferred() {
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

export { harness, initialSite, clock, Element, Events, matches, descendants };
