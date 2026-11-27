// Run without a browser or dependencies: node --test docs/scripts/browser/widget_lifecycle.test.mjs
// Load the unmodified production script; only browser APIs, transport and time are replaced.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { setImmediate } from "node:timers/promises";
import vm from "node:vm";
import { BROWSER_CASE_TITLES } from "./cases.mjs";

const caseTitle = (id) => {
  const title = BROWSER_CASE_TITLES[id];
  if (!title) throw new Error(`browser manifest missing ${id}`);
  return title;
};
const source = readFileSync(new URL("../../../web/widget/v1.js", import.meta.url), "utf8");
const interactiveSource = readFileSync(new URL("../../../web/widget/v1.interactive.js", import.meta.url), "utf8");
const flush = () => setImmediate();

class Events {
  listeners = new Map();
  addEventListener(type, fn) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type).add(fn);
  }
  removeEventListener(type, fn) { this.listeners.get(type)?.delete(fn); }
  emit(type, detail = {}) {
    for (const fn of [...(this.listeners.get(type) || [])]) {
      fn({ type, target: this, preventDefault() {}, ...detail });
    }
  }
  listenerCount() { return [...this.listeners.values()].reduce((n, fns) => n + fns.size, 0); }
}

class Element extends Events {
  constructor(tag, document) {
    super();
    this.tagName = tag.toUpperCase();
    this.nodeType = 1;
    this.ownerDocument = document;
    this.children = [];
    this.parentNode = null;
    this.attributes = new Map();
    this.value = "";
    this.disabled = false;
    this.text = "";
    this.style = {};
    Object.defineProperty(this.style, "cssText", {
      get: () => this.getAttribute("style") || "",
      set: (value) => this.setAttribute("style", value),
    });
  }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  getAttribute(name) { return this.attributes.get(name) ?? null; }
  removeAttribute(name) { this.attributes.delete(name); }
  appendChild(child) {
    if (child.parentNode) child.parentNode.removeChild(child);
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
  get firstChild() { return this.children[0] || null; }
  set textContent(text) {
    for (const child of [...this.children]) this.removeChild(child);
    this.text = String(text);
  }
  get textContent() { return this.text + this.children.map((child) => child.textContent).join(""); }
  contains(node) { return node === this || this.children.some((child) => child.contains(node)); }
  closest(tag) {
    for (let node = this; node; node = node.parentNode) {
      if (node.tagName === tag.toUpperCase()) return node;
    }
    return null;
  }
  attachShadow() { return this.shadowRoot = new Element("shadow-root", this.ownerDocument); }
}

function find(node, predicate) {
  if (predicate(node)) return node;
  for (const child of node.children) {
    const match = find(child, predicate);
    if (match) return match;
  }
  return null;
}

function clock() {
  let now = 1_000_000, seq = 0;
  const timers = new Map();
  return {
    timers,
    now: () => now,
    jump(ms) { now += ms; },
    setTimeout(fn, ms = 0) {
      const id = ++seq;
      timers.set(id, { fn, at: now + ms });
      return id;
    },
    clearTimeout(id) { timers.delete(id); },
    async advance(ms) {
      const end = now + ms;
      for (let turns = 0; ; turns++) {
        assert.ok(turns < 100, "timer loop must remain bounded");
        const next = [...timers].sort((a, b) => a[1].at - b[1].at || a[0] - b[0])[0];
        if (!next || next[1].at > end) break;
        const [id, timer] = next;
        timers.delete(id);
        now = timer.at;
        timer.fn();
        await flush();
      }
      now = end;
      await flush();
    },
  };
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function challenge(token = "challenge-1", expires = 0, question = null) {
  return {
    token, mode: "pow_question", expires_in: expires,
    pow: { algo: "sha256-prefix", bits: 12, seed: "AAECAwQFBgcICQoLDA0ODw" },
    question: question || {
      media: "image", image_mime: "image/png", image_base64: "AQIDBA==",
      image_width: 120, image_height: 60, length: 6, charset: "ABCDEF",
    },
  };
}

function harness(t, { honorAbort = false, options = {}, interactive = true, reducedMotion = false, coarsePointer = false } = {}) {
  const time = clock();
  const requests = [], workers = [], successes = [], errors = [], states = [], expired = [];
  const urls = new Set();
  let blobSeq = 0;
  const document = new Events();
  document.visibilityState = "visible";
  document.createElement = (tag) => new Element(tag, document);
  document.head = document.createElement("head");
  document.documentElement = document.createElement("html");
  // Phase 2 起 widget 会用 SVG 画角度辅助线；Node 工装只需要同样的元素外壳。
  document.createElementNS = (_ns, tag) => new Element(tag, document);
  const form = document.createElement("form");
  const container = form.appendChild(document.createElement("div"));
  class BrowserURL extends URL {
    static createObjectURL() {
      const url = "blob:widget-test-" + (++blobSeq);
      urls.add(url);
      return url;
    }
    static revokeObjectURL(url) { urls.delete(url); }
  }
  class Worker extends Events {
    constructor(url) {
      super();
      this.url = url;
      this.sent = [];
      this.terminated = false;
      workers.push(this);
    }
    postMessage(message) { this.sent.push(message); }
    terminate() { this.terminated = true; }
  }
  const sandbox = {
    document, console, navigator: { language: "en" }, location: new URL("https://page.invalid/"),
    matchMedia: (query) => ({
      matches: (reducedMotion && String(query).indexOf("prefers-reduced-motion") >= 0) ||
        (coarsePointer && String(query).indexOf("pointer: coarse") >= 0),
    }),
    URL: BrowserURL, Blob, Worker, AbortController, atob,
    Date: class extends Date { static now() { return time.now(); } },
    setTimeout: time.setTimeout, clearTimeout: time.clearTimeout,
    fetch(url, options) {
      const response = deferred();
      const request = {
        path: new URL(url).pathname, options,
        reject: response.reject,
        respond(json, status = 200, headers = {}) {
          this.respondBody(Promise.resolve(JSON.stringify(json)), status, headers);
        },
        respondBody(body, status = 200, headers = {}) {
          response.resolve({ ok: status >= 200 && status < 300, status,
            headers: new Headers(headers), text: () => body });
        },
      };
      requests.push(request);
      if (honorAbort && options.signal) {
        options.signal.addEventListener("abort", () => {
          response.reject(new DOMException("aborted", "AbortError"));
        }, { once: true });
      }
      return response.promise;
    },
  };
  vm.runInNewContext(source, sandbox, { filename: "web/widget/v1.js" });
  if (interactive) {
    vm.runInNewContext(interactiveSource, sandbox, { filename: "web/widget/v1.interactive.js" });
  }
  const api = sandbox.EpoCHA;
  const id = api.render(container, {
    apiBase: "https://captcha.invalid", siteKey: "pk_lifecycle", fetchTimeoutMs: 1000,
    onSuccess: (token) => successes.push(token), onError: (err) => errors.push(err),
    onState: (state) => states.push(state), onExpired: () => expired.push(id),
    ...options,
  });
  t.after(() => api.destroy(id));
  const host = find(container, (node) => node.getAttribute("data-epocha-widget") === id);
  const field = find(container, (node) => node.getAttribute("data-epocha-field") === id);
  // 按稳定 id 找文本答案框：load() 期间它会带 hidden class，按 class 精确匹配会找不到。
  const answer = find(host.shadowRoot, (node) => (node.getAttribute("id") || "").endsWith("-ans"));
  const buttons = find(host.shadowRoot, (node) => node.getAttribute("class") === "box")
    .children.filter((node) => node.tagName === "BUTTON");
  const status = find(host.shadowRoot, (node) => node.getAttribute("role") === "status");
  const h = {
    api, id, time, requests, workers, urls, successes, errors, states, expired,
    sandbox, document, container, host, field, answer, buttons, status,
    calls: (path) => requests.filter((request) => request.path === "/api/" + path),
    clickSubmit() { buttons[0].emit("click"); },
    async ready(request = requests.at(-1), token = "challenge-1", expires = 0, question = null) {
      request.respond(challenge(token, expires, question));
      await flush();
      assert.equal(api.state(id), "ready", "fixture must use real challenge loading");
    },
    async redeem() {
      assert.equal(api.state(id), "ready");
      answer.value = "ABCDEF";
      this.clickSubmit();
      assert.equal(api.state(id), "solving", "fixture must use real submit/PoW methods");
      const worker = workers.at(-1);
      const job = worker.sent.findLast((message) => message.type === "solve");
      assert.ok(job);
      worker.emit("message", { data: { type: "solved", job: job.job, nonce: "42" } });
      await flush();
      assert.equal(api.state(id), "submitting");
      return requests.at(-1);
    },
    empty(state = "idle") {
      assert.equal(api.state(id), state);
      assert.equal(api.getResponse(id), "");
      assert.equal(field.value, "");
      assert.deepEqual(successes, []);
    },
  };
  return h;
}

for (const outcome of ["success", "network failure"]) {
  test(caseTitle(outcome === "success" ? "lifecycle.reset-redeem-success" : "lifecycle.reset-redeem-network"), async (t) => {
    const h = harness(t);
    await h.ready();
    const request = await h.redeem();
    h.api.reset(h.id);
    h.empty();
    if (outcome === "success") request.respond({ response: "old-response", expires_in: 60 });
    else request.reject(new TypeError("offline"));
    await flush();
    h.empty();
    assert.equal(h.answer.value, "");
    assert.deepEqual(h.errors, []);
    assert.equal(request.options.signal?.aborted, true);
    assert.equal(h.time.timers.size, 0);
    assert.equal(h.calls("redeem").length, 1);
  });
}

for (const outcome of ["late success", "abort rejection", "late 503"]) {
  test(caseTitle({ "late success": "lifecycle.reset-challenge-success", "abort rejection": "lifecycle.reset-challenge-abort", "late 503": "lifecycle.reset-challenge-503" }[outcome]), async (t) => {
    const h = harness(t, { honorAbort: outcome === "abort rejection" });
    const request = h.requests[0];
    h.api.reset(h.id);
    h.empty();
    if (outcome === "late success") request.respond(challenge());
    if (outcome === "late 503") request.respond({ "error-codes": ["challenge-unavailable"] }, 503);
    await flush();
    h.empty();
    assert.equal(h.time.timers.size, 0);
    assert.deepEqual(h.errors, []);
    assert.equal(request.options.signal.aborted, true);
    await h.time.advance(60_000);
    h.empty();
    assert.equal(h.requests.length, 1);
  });
}

for (const action of ["reset", "refresh", "destroy"]) {
  test(caseTitle("lifecycle." + action + "-retry"), async (t) => {
    const h = harness(t);
    h.requests[0].respond({ "error-codes": ["challenge-unavailable"] }, 503, { "Retry-After": "2" });
    await flush();
    assert.equal(h.api.state(h.id), "loading");
    assert.equal(h.time.timers.size, 1);
    const queuedRetry = [...h.time.timers.values()][0].fn;
    h.api[action](h.id);
    const count = action === "refresh" ? 2 : 1;
    queuedRetry();
    await flush();
    assert.equal(h.requests.length, count, "old retry must not issue a new challenge request");
    assert.equal(h.time.timers.size, action === "refresh" ? 1 : 0);
    if (action === "refresh") await h.ready();
    await h.time.advance(60_000);
    h.empty(action === "destroy" ? null : action === "refresh" ? "ready" : "idle");
    assert.equal(h.requests.length, count);
    assert.deepEqual(h.errors, []);
  });
}

test(caseTitle("lifecycle.redeem-timeout"), async (t) => {
  const h = harness(t);
  await h.ready();
  const request = await h.redeem();
  h.clickSubmit();
  await h.time.advance(999);
  h.empty("submitting");
  await h.time.advance(1);
  h.empty("error");
  assert.deepEqual(h.errors.map((err) => [err.code, err.retryable]), [["timeout", true]]);
  assert.equal(request.options.signal.aborted, true);
  assert.equal(h.buttons[0].disabled, true);
  assert.equal(h.buttons[1].disabled, false);
  assert.equal(h.time.timers.size, 0);
  assert.equal(h.workers[0].terminated, true);
  assert.equal(h.urls.size, 0);
  h.clickSubmit();
  await h.time.advance(60_000);
  assert.equal(h.calls("redeem").length, 1, "a timeout must never replay a consumed challenge");
  h.buttons[1].emit("click");
  assert.equal(h.api.state(h.id), "loading");
  request.respond({ response: "late-response" });
  await flush();
  h.empty("loading");
  await h.ready(h.requests.at(-1), "challenge-2");
  const retry = await h.redeem();
  assert.equal(JSON.parse(retry.options.body).token, "challenge-2");
  retry.respond({ response: "fresh-response" });
  await flush();
  assert.equal(h.api.getResponse(h.id), "fresh-response");
  assert.equal(h.field.value, "fresh-response");
  assert.deepEqual(h.successes, ["fresh-response"]);
  assert.equal(h.calls("redeem").length, 2);
});

test(caseTitle("lifecycle.redeem-body-timeout"), async (t) => {
  const h = harness(t);
  await h.ready();
  const request = await h.redeem();
  const body = deferred();
  request.respondBody(body.promise);
  await flush();
  await h.time.advance(1000);
  h.empty("error");
  assert.equal(h.errors[0].code, "timeout");
  body.resolve(JSON.stringify({ response: "late-body" }));
  await flush();
  h.empty("error");
  assert.equal(h.errors.length, 1);
  assert.equal(h.time.timers.size, 0);
});

test(caseTitle("lifecycle.redeem-network"), async (t) => {
  const h = harness(t);
  await h.ready();
  const request = await h.redeem();
  request.reject(new TypeError("connection lost"));
  await flush();
  h.empty("error");
  assert.deepEqual(h.errors.map((err) => [err.code, err.retryable]), [["network", true]]);
  assert.equal(h.buttons[1].disabled, false);
  assert.equal(h.time.timers.size, 0);
});

test(caseTitle("lifecycle.refresh-redeem"), async (t) => {
  const h = harness(t);
  await h.ready();
  const old = await h.redeem();
  const queuedTimers = [...h.time.timers.values()].map((timer) => timer.fn);
  h.api.refresh(h.id);
  assert.equal(old.options.signal?.aborted, true);
  const current = h.requests.at(-1);
  old.respond({ response: "old-response" });
  for (const fn of queuedTimers) fn();
  await flush();
  h.empty("loading");
  assert.equal(current.options.signal.aborted, false);
  assert.equal(h.time.timers.size, 1, "old completion must not clear the new request's timeout");
  await h.time.advance(1000);
  h.empty("error");
  assert.equal(current.options.signal.aborted, true);
  assert.deepEqual(h.errors.map((err) => err.code), ["timeout"]);
});

test(caseTitle("lifecycle.destroy-redeem"), async (t) => {
  const h = harness(t, { honorAbort: true });
  await h.ready();
  const request = await h.redeem();
  h.api.destroy(h.id);
  await flush();
  h.empty(null);
  assert.equal(request.options.signal?.aborted, true);
  assert.equal(h.time.timers.size, 0);
  assert.equal(h.urls.size, 0);
  assert.ok(h.workers.every((worker) => worker.terminated));
  assert.equal(h.document.listenerCount(), 0);
  assert.equal(h.answer.listenerCount(), 0);
  assert.ok(h.buttons.every((button) => button.listenerCount() === 0));
  assert.equal(h.container.children.length, 0);
  assert.equal(h.api.ids().length, 0);
  request.respond({ response: "after-destroy" });
  await h.time.advance(60_000);
  h.empty(null);
  assert.deepEqual(h.errors, []);
});

test(caseTitle("lifecycle.reset-pow-image-deadline"), async (t) => {
  const h = harness(t);
  await h.ready(h.requests[0], "challenge-1", 30);
  const queuedDeadline = [...h.time.timers.values()][0].fn;
  const image = find(h.host.shadowRoot, (node) => node.tagName === "IMG");
  h.answer.value = "ABCDEF";
  h.clickSubmit();
  assert.equal(h.api.state(h.id), "solving");
  const worker = h.workers[0];
  const job = worker.sent.find((message) => message.type === "solve");
  const imageText = image.parentNode.textContent;
  h.api.reset(h.id);
  assert.equal(h.time.timers.size, 0, "cancel must settle PoW and remove its timeout");
  const status = h.status.textContent;
  assert.equal(image.parentNode, null, "reset must detach the old image");
  queuedDeadline();
  worker.emit("message", { data: { type: "solved", job: job.job, nonce: "42" } });
  worker.emit("error");
  image.emit("error");
  await flush();
  h.empty();
  assert.equal(h.status.textContent, status);
  assert.equal(image.parentNode, null, "old image callbacks must stay detached after reset");
  assert.deepEqual(h.errors, []);
  assert.equal(h.calls("redeem").length, 0);
});

for (const state of ["submitting", "verified", "error"]) {
  test(caseTitle("lifecycle.reset-state-" + state), async (t) => {
    let h;
    h = harness(t, { options: { onState(next, id) {
      if (next === state) h.api.reset(id);
    } } });
    await h.ready();
    h.answer.value = "ABCDEF";
    h.clickSubmit();
    const worker = h.workers[0];
    const job = worker.sent.find((message) => message.type === "solve");
    worker.emit("message", { data: { type: "solved", job: job.job, nonce: "42" } });
    await flush();
    if (state === "verified") h.requests.at(-1).respond({ response: "cancel-in-observer", expires_in: 2 });
    if (state === "error") h.requests.at(-1).reject(new TypeError("offline"));
    await flush();
    h.empty();
    assert.equal(h.status.getAttribute("data-kind"), "idle");
    assert.equal(h.time.timers.size, 0);
    assert.deepEqual(h.errors, []);
    assert.equal(h.calls("redeem").length, state === "submitting" ? 0 : 1);
  });
}

test(caseTitle("lifecycle.reset-503-observer"), async (t) => {
  let h;
  h = harness(t, { options: { onState(next, id) {
    if (h && next === "loading") h.api.reset(id);
  } } });
  h.requests[0].respond({ "error-codes": ["challenge-unavailable"] }, 503);
  await flush();
  h.empty();
  assert.equal(h.status.getAttribute("data-kind"), "idle");
  assert.equal(h.time.timers.size, 0);
  await h.time.advance(60_000);
  h.empty();
  assert.equal(h.requests.length, 1);
});

test(caseTitle("lifecycle.max-auto-retry-zero"), async (t) => {
  const h = harness(t, { options: { maxAutoRetry: 0 } });
  h.requests[0].respond({ "error-codes": ["challenge-unavailable"] }, 503, { "Retry-After": "1" });
  await flush();
  assert.equal(h.api.state(h.id), "error");
  assert.equal(h.requests.length, 1, "maxAutoRetry=0 must not schedule another challenge request");
  assert.equal(h.buttons[0].disabled, true);
  assert.equal(h.buttons[1].disabled, false);
  assert.deepEqual(h.errors.map((err) => err.code), ["challenge-unavailable"]);
  await h.time.advance(60_000);
  assert.equal(h.requests.length, 1);
});

test(caseTitle("lifecycle.image-error"), async (t) => {
  const h = harness(t);
  await h.ready();
  const image = find(h.host.shadowRoot, (node) => node.tagName === "IMG");
  assert.ok(image);
  image.emit("error");
  await flush();
  assert.equal(h.api.state(h.id), "error");
  assert.equal(h.errors.at(-1)?.code, "image-unusable");
  assert.equal(h.buttons[0].disabled, true);
  assert.equal(h.buttons[1].disabled, false);
  assert.equal(h.calls("redeem").length, 0);
  assert.equal(h.field.value, "");
});

test(caseTitle("lifecycle.label-binding"), async (t) => {
  const h = harness(t);
  const label = find(h.host.shadowRoot, (node) => node.tagName === "LABEL");
  assert.equal(label.getAttribute("for"), h.answer.getAttribute("id"), "label must target answer input");
  assert.match(h.answer.getAttribute("aria-labelledby") || "", new RegExp(h.id + "-lbl"),
    "answer input must expose the visible label as its accessible name");
});

test(caseTitle("lifecycle.reset-clears-image"), async (t) => {
  const h = harness(t);
  await h.ready();
  assert.ok(find(h.host.shadowRoot, (node) => node.tagName === "IMG"));
  h.api.reset(h.id);
  await flush();
  assert.equal(h.api.state(h.id), "idle");
  assert.equal(find(h.host.shadowRoot, (node) => node.tagName === "IMG"), null,
    "reset must not leave the old question image visible");
  assert.equal(h.buttons[0].disabled, true, "reset without a new challenge must not enable submit");
  assert.equal(h.buttons[1].disabled, false, "refresh is the only recovery path after reset");
  assert.equal(h.answer.disabled, true);
  assert.equal(h.field.value, "");
  h.buttons[1].emit("click");
  assert.equal(h.api.state(h.id), "loading");
});

test(caseTitle("lifecycle.token-conservative-expiry"), async (t) => {
  const h = harness(t, { options: { fetchTimeoutMs: 5_000 } });
  await h.ready();
  const redeem = await h.redeem();
  const body = deferred();
  redeem.respondBody(body.promise);
  await flush();
  await h.time.advance(1_500);
  body.resolve(JSON.stringify({ response: "too-late", expires_in: 1 }));
  await flush();
  assert.equal(h.api.state(h.id), "expired", "a response that arrived after its TTL must not become verified");
  assert.equal(h.api.getResponse(h.id), "");
  assert.equal(h.field.value, "");
  assert.deepEqual(h.successes, []);
  assert.equal(h.expired.length, 1);
  assert.equal(h.time.timers.size, 0);

  let slow;
  slow = harness(t, { options: { onState(state) {
    if (state === "verified") slow.time.jump(1_500);
  } } });
  await slow.ready();
  const next = await slow.redeem();
  next.respond({ response: "expired-in-observer", expires_in: 1 });
  await flush();
  assert.deepEqual(slow.successes, [], "a synchronous state observer must not defer expiry beyond onSuccess");
  slow.empty("expired");
});
test(caseTitle("lifecycle.challenge-conservative-expiry"), async (t) => {
  const h = harness(t, { options: { fetchTimeoutMs: 5_000 } });
  h.time.jump(1_500);
  h.requests[0].respond(challenge("expired-in-transit", 1));
  await flush();
  h.empty("expired");
  assert.equal(h.workers.length, 0);
  assert.equal(h.buttons[0].disabled, true);
});

test(caseTitle("lifecycle.image-error-inflight"), async (t) => {
  const h = harness(t);
  await h.ready();
  const image = find(h.host.shadowRoot, (node) => node.tagName === "IMG");
  const request = await h.redeem();
  image.emit("error");
  await flush();
  h.empty("error");
  assert.equal(request.options.signal.aborted, true);
  request.respond({ response: "late-after-image-error", expires_in: 60 });
  await flush();
  h.empty("error");
  assert.equal(h.calls("redeem").length, 1);
});

test(caseTitle("lifecycle.get-response-rechecks-deadline"), async (t) => {
  const h = harness(t);
  await h.ready();
  const request = await h.redeem();
  request.respond({ response: "deadline-response", expires_in: 1 });
  await flush();
  assert.equal(h.api.getResponse(h.id), "deadline-response");
  h.time.jump(1_000);
  assert.equal(h.api.getResponse(h.id), "", "getResponse must synchronously recheck a deadline after timer suspension");
  assert.equal(h.api.state(h.id), "expired");
  assert.equal(h.field.value, "");
  assert.equal(h.expired.length, 1);
});
test(caseTitle("lifecycle.success-clears-timeout"), async (t) => {
  const h = harness(t);
  await h.ready();
  const request = await h.redeem();
  h.clickSubmit();
  request.respond({ response: "valid-response", expires_in: 2 });
  await flush();
  h.clickSubmit();
  assert.equal(h.api.state(h.id), "verified");
  assert.equal(h.api.getResponse(h.id), "valid-response");
  assert.equal(h.field.value, "valid-response");
  assert.deepEqual(h.successes, ["valid-response"]);
  assert.equal(h.calls("redeem").length, 1);
  assert.equal(h.time.timers.size, 1, "only the token expiry timer remains");
  await h.time.advance(1000);
  assert.equal(h.api.state(h.id), "verified");
  assert.deepEqual(h.errors, []);
  await h.time.advance(1000);
  assert.equal(h.api.state(h.id), "expired");
  assert.equal(h.api.getResponse(h.id), "");
  assert.equal(h.field.value, "");
  assert.equal(h.expired.length, 1);
  assert.equal(h.time.timers.size, 0);
});

test("zh-CN answer placeholder follows the question length", async (t) => {
  const h = harness(t, { options: { language: "zh-CN" } });
  const ch = challenge();
  ch.question.length = 8;
  h.requests[0].respond(ch);
  await flush();
  assert.equal(h.api.state(h.id), "ready");
  assert.match(h.answer.placeholder, /8/, "placeholder must use the actual v2 length");
  assert.doesNotMatch(h.answer.placeholder, /6/, "placeholder must not keep the v1 hard-coded 6");
});

// --- ADR-024：cells / pairs 输入模式 ---

function findAll(node, predicate, out = []) {
  if (predicate(node)) out.push(node);
  for (const child of node.children) findAll(child, predicate, out);
  return out;
}

function cellsChallenge() {
  return {
    token: "grid-1", mode: "pow_question", expires_in: 0,
    pow: { algo: "sha256-prefix", bits: 12, seed: "AAECAwQFBgcICQoLDA0ODw" },
    question: {
      media: "image", image_mime: "image/png", image_base64: "AQIDBA==",
      image_width: 340, image_height: 340,
      answer_type: "cells", max_answer_len: 32,
      grid_cols: 3, grid_rows: 3, grid_x: 12, grid_y: 12, grid_cell: 100, grid_gap: 8,
      prompt_zh: "选出所有的三角形。", prompt_en: "Select every triangle.",
    },
  };
}

function pairsChallenge() {
  return {
    token: "pairs-1", mode: "pow_question", expires_in: 0,
    pow: { algo: "sha256-prefix", bits: 12, seed: "AAECAwQFBgcICQoLDA0ODw" },
    question: {
      media: "image", image_mime: "image/png", image_base64: "AQIDBA==",
      image_width: 480, image_height: 240,
      answer_type: "pairs", max_answer_len: 16,
      labels_zh: ["实心三角形", "空心圆形"], labels_en: ["filled triangles", "hollow circles"],
      min: 1, max: 6,
      prompt_zh: "数一数两类图形。", prompt_en: "Count the two kinds of shapes.",
    },
  };
}

function solveLastPow(h) {
  const worker = h.workers.at(-1);
  const job = worker.sent.filter((message) => message.type === "solve").at(-1);
  assert.ok(job);
  worker.emit("message", { data: { type: "solved", job: job.job, nonce: "42" } });
}

test("cells 输入（Node 工装）：9 个可点击格子、选中集合排序去重后提交", async (t) => {
  const h = harness(t);
  h.requests[0].respond(cellsChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready");
  const cells = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  assert.equal(cells.length, 9, "3×3 网格必须渲染 9 个格子");
  for (const cell of cells) {
    assert.equal(cell.getAttribute("aria-pressed"), "false");
    assert.match(cell.getAttribute("style"), /left:.*%;top:.*%;width:.*%;height:.*%/);
  }
  assert.equal(h.answer.getAttribute("class"), "ans hidden", "cells 模式必须隐藏文本输入");

  cells[2].emit("click");
  cells[6].emit("click");
  assert.equal(h.answer.value, "3,7", "选中集合必须按 1-based 序号排序");
  assert.equal(cells[2].getAttribute("aria-pressed"), "true");
  assert.equal(cells[6].getAttribute("aria-pressed"), "true");

  cells[2].emit("click");
  assert.equal(h.answer.value, "7", "再次点击必须取消选中");
  assert.equal(cells[2].getAttribute("aria-pressed"), "false");

  h.clickSubmit();
  assert.equal(h.api.state(h.id), "solving");
  solveLastPow(h);
  await flush();
  const redeem = h.requests.at(-1);
  assert.equal(redeem.path, "/api/redeem");
  assert.equal(JSON.parse(redeem.options.body).answer, "7");
});

test("pairs 输入（Node 工装）：两个数字输入合成为 a,b", async (t) => {
  const h = harness(t);
  h.requests[0].respond(pairsChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready");
  const inputs = findAll(h.host.shadowRoot,
    (node) => node.tagName === "INPUT" && node.getAttribute("type") === "number");
  assert.equal(inputs.length, 2, "pairs 模式必须渲染两个数字输入");
  // 标题 label 也会指向第一个输入（提示语与控件关联），所以这里按文本找成对的标签。
  const labels = findAll(h.host.shadowRoot,
    (node) => node.tagName === "LABEL" && node.textContent === "filled triangles");
  assert.equal(labels.length, 1);
  assert.equal(h.answer.getAttribute("class"), "ans hidden");

  // 越界值不合成提交串：服务端不会把畸形输入当成"答错"。
  inputs[0].value = "9";
  inputs[1].value = "2";
  inputs[0].emit("input");
  inputs[1].emit("input");
  assert.equal(h.answer.value, "", "超出题面 max 的值不得进入提交串");

  inputs[0].value = "4";
  inputs[0].emit("input");
  inputs[1].emit("input");
  assert.equal(h.answer.value, "4,2");

  h.clickSubmit();
  assert.equal(h.api.state(h.id), "solving");
  solveLastPow(h);
  await flush();
  assert.equal(JSON.parse(h.requests.at(-1).options.body).answer, "4,2");
});

test("cells 几何越界（Node 工装）：fail closed，不渲染可点击区域", async (t) => {
  const h = harness(t);
  const bad = cellsChallenge();
  bad.question.grid_cell = 160; // 12 + 3*160 > 340
  h.requests[0].respond(bad);
  await flush();
  assert.equal(h.api.state(h.id), "error");
  const cells = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  assert.equal(cells.length, 0, "越界几何不得渲染任何格子按钮");
});

// --- ADR-030：tile-rotation / symbol-path / visual-rank / crowd-find 输入 ---

function ids36() {
  const out = [];
  for (let i = 0; i < 36; i++) out.push(i.toString(16).padStart(4, "0"));
  return out;
}

function interactiveChallenge(question) {
  return {
    token: "interactive-1", mode: "pow_question", expires_in: 0,
    pow: { algo: "sha256-prefix", bits: 12, seed: "AAECAwQFBgcICQoLDA0ODw" },
    question: Object.assign({
      media: "image", image_mime: "image/png", image_base64: "AQIDBA==",
    }, question),
  };
}

function rotateChallenge() {
  return interactiveChallenge({
    image_width: 320, image_height: 320,
    answer_type: "masks", max_answer_len: 16,
    grid_cols: 3, grid_rows: 3, grid_x: 10, grid_y: 10, grid_cell: 96, grid_gap: 4,
    initial_masks: "1a2b3c4d5", style: "network-v1", input_mode: "rotate",
    prompt_zh: "旋转路径块。", prompt_en: "Rotate the tiles.",
  });
}

function pathChallenge() {
  return interactiveChallenge({
    image_width: 320, image_height: 360,
    answer_type: "path", max_answer_len: 32,
    grid_cols: 6, grid_rows: 6, grid_x: 20, grid_y: 70, grid_cell: 44, grid_gap: 3,
    target_len: 6, target_y: 12, target_h: 48, cell_ids: ids36(), input_mode: "path",
    prompt_zh: "连接符号。", prompt_en: "Trace the symbols.",
  });
}

function rankChallenge() {
  return interactiveChallenge({
    image_width: 500, image_height: 280,
    answer_type: "order", max_answer_len: 16, input_mode: "rank",
    cards: [
      { id: "C", x: 12, y: 12, w: 150, h: 120 },
      { id: "A", x: 172, y: 12, w: 150, h: 120 },
      { id: "D", x: 332, y: 12, w: 150, h: 120 },
      { id: "E", x: 95, y: 142, w: 150, h: 120 },
      { id: "B", x: 255, y: 142, w: 150, h: 120 },
    ],
    prompt_zh: "按线段数排序。", prompt_en: "Order by segments.",
  });
}

function selectChallenge() {
  return interactiveChallenge({
    image_width: 340, image_height: 420,
    answer_type: "select", max_answer_len: 8,
    grid_cols: 6, grid_rows: 6, grid_x: 18, grid_y: 104, grid_cell: 48, grid_gap: 3,
    sample_x: 138, sample_y: 16, sample_w: 64, sample_h: 64,
    cell_ids: ids36(), input_mode: "select",
    prompt_zh: "找出相同角色。", prompt_en: "Find the matching character.",
  });
}

test("interactive pending request times out and refresh starts a fresh attempt", async (t) => {
  const h = harness(t, { interactive: false });
  h.requests[0].respond(rotateChallenge());
  await flush();
  const old = find(h.document.head, (node) => node.tagName === "SCRIPT");
  const lateLoad = old.onload;
  await h.time.advance(1000);
  assert.equal(h.api.state(h.id), "error");
  assert.equal(h.errors.at(-1).code, "interactive-unavailable");
  assert.equal(h.buttons[1].disabled, false);
  assert.equal(h.time.timers.size, 0);
  assert.equal(old.parentNode, null);
  vm.runInNewContext(interactiveSource, h.sandbox);
  lateLoad();
  await flush();
  assert.equal(h.api.state(h.id), "error", "late module callback revived an expired attempt");
  h.api.refresh(h.id);
  h.requests.at(-1).respond(rotateChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready");
});

test("interactive challenge TTL expires before module timeout and late callback", async (t) => {
  const h = harness(t, { interactive: false });
  const reply = rotateChallenge(); reply.expires_in = 0.5;
  h.requests[0].respond(reply);
  await flush();
  const script = find(h.document.head, (node) => node.tagName === "SCRIPT");
  const lateLoad = script.onload;
  await h.time.advance(500);
  h.empty("expired");
  assert.equal(h.time.timers.size, 0);
  vm.runInNewContext(interactiveSource, h.sandbox);
  lateLoad(); await flush();
  h.empty("expired");
});

for (const action of ["reset", "destroy"]) {
  test(`interactive ${action} releases the pending attempt and ignores callbacks`, async (t) => {
    const h = harness(t, { interactive: false });
    h.requests[0].respond(rotateChallenge()); await flush();
    const script = find(h.document.head, (node) => node.tagName === "SCRIPT");
    const callback = script.onload;
    h.api[action](h.id);
    assert.equal(h.time.timers.size, 0);
    assert.equal(script.parentNode, null);
    vm.runInNewContext(interactiveSource, h.sandbox); callback(); await flush();
    assert.equal(h.api.state(h.id), action === "destroy" ? null : "idle");
    assert.equal(h.calls("redeem").length, 0);
  });
}

test("interactive shared attempt survives one instance reset and cleans up after the last", async (t) => {
  const h = harness(t, { interactive: false });
  const second = h.api.render(h.container, { apiBase: "https://captcha.invalid", siteKey: "pk_second", fetchTimeoutMs: 1000 });
  t.after(() => h.api.destroy(second));
  h.requests[0].respond(rotateChallenge()); h.requests[1].respond(rotateChallenge()); await flush();
  assert.equal(findAll(h.document.head, (node) => node.tagName === "SCRIPT").length, 1);
  h.api.reset(h.id);
  const script = find(h.document.head, (node) => node.tagName === "SCRIPT");
  assert.ok(script);
  vm.runInNewContext(interactiveSource, h.sandbox); script.onload(); await flush();
  assert.equal(h.api.state(second), "ready");
  assert.equal(h.api.state(h.id), "idle");
  assert.equal(h.time.timers.size, 0);
});

test("interactive background resume rechecks TTL while loading", async (t) => {
  const h = harness(t, { interactive: false });
  const reply = rotateChallenge(); reply.expires_in = 2;
  h.requests[0].respond(reply); await flush();
  h.time.jump(3000);
  h.document.emit("visibilitychange");
  h.empty("expired");
  assert.equal(h.time.timers.size, 0);
});

test("tile-rotation 输入：旋转后才有答案，Reset 清空本地操作", async (t) => {
  const h = harness(t);
  h.requests[0].respond(rotateChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready", JSON.stringify(h.errors) + " / " + h.status.textContent);
  assert.equal(h.buttons[0].disabled, true, "未旋转时 Verify 必须 disabled");
  const cells = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  assert.equal(cells.length, 9);
  assert.equal(cells[0].firstChild.getAttribute("class"), "tileimg");

  cells[0].emit("click");
  assert.equal(h.answer.value, "v1:2a2b3c4d5", "只有被点击的 tile 旋转；其余保持初始 mask");
  assert.equal(h.buttons[0].disabled, false);
  cells[0].emit("click");
  assert.equal(h.answer.value, "v1:2a2b3c4d5", "旋转动画未结束时同一 tile 不得重入");
  h.time.jump(200);
  cells[0].emit("keydown", { key: "Enter", shiftKey: true });
  assert.equal(h.answer.value, "v1:1a2b3c4d5", "Shift+Enter 必须逆时针转回");

  const aux = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "aux")[0];
  const btns = findAll(aux, (node) => node.tagName === "BUTTON");
  btns[1].emit("click");
  assert.equal(h.answer.value, "", "Reset 必须清空本地旋转");
  assert.equal(h.buttons[0].disabled, true);
});

test("交互模块按需加载成功：同一状态机进入 ready 并渲染控件", async (t) => {
  const h = harness(t, { interactive: false });
  h.requests[0].respond(rotateChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "loading");
  const script = find(h.document.head, (node) => node.tagName === "SCRIPT");
  assert.ok(script, "核心应当创建模块脚本");
  vm.runInNewContext(interactiveSource, h.sandbox, { filename: "web/widget/v1.interactive.js" });
  script.onload();
  await flush();
  assert.equal(h.api.state(h.id), "ready", JSON.stringify(h.errors));
  const cells = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  assert.equal(cells.length, 9);
});

test("交互模块加载失败：明确失败，不渲染半成品", async (t) => {
  const h = harness(t, { interactive: false });
  h.requests[0].respond(rotateChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "loading", "模块未加载时不能进入 ready");
  const script = find(h.document.head, (node) => node.tagName === "SCRIPT");
  assert.ok(script, "核心应当尝试加载交互模块");
  script.onerror();
  await flush();
  assert.equal(h.api.state(h.id), "error");
  assert.equal(h.errors.length, 1);
  assert.equal(h.errors[0].code, "interactive-unavailable");
  assert.equal(h.buttons[0].disabled, true);
  assert.equal(findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell").length, 0,
    "模块失败时不得渲染可点击格子");
});

test("交互模块失败后刷新可以重试，不会把页面永久钉死", async (t) => {
  const h = harness(t, { interactive: false });
  h.requests[0].respond(rotateChallenge());
  await flush();
  const first = find(h.document.head, (node) => node.tagName === "SCRIPT");
  assert.ok(first);
  first.onerror();
  await flush();
  assert.equal(h.api.state(h.id), "error");

  h.api.refresh(h.id);
  assert.equal(h.requests.length, 2);
  h.requests[1].respond(rotateChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "loading");
  const scripts = findAll(h.document.head, (node) => node.tagName === "SCRIPT");
  assert.equal(scripts.length, 1, "失败脚本必须清理，重试只保留一个新请求");
  assert.notEqual(scripts[0], first, "失败后重试必须重新加载模块");
  vm.runInNewContext(interactiveSource, h.sandbox, { filename: "web/widget/v1.interactive.js" });
  scripts.at(-1).onload();
  await flush();
  assert.equal(h.api.state(h.id), "ready", JSON.stringify(h.errors));
});

test("新交互题型的中文文案：指令与说明都来自既有 i18n 表", async (t) => {
  const h = harness(t, { options: { language: "zh-CN" } });
  h.requests[0].respond(rotateChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready", JSON.stringify(h.errors));
  assert.equal(h.host.getAttribute("lang"), "zh-CN");
  const note = find(h.host.shadowRoot, (node) => node.getAttribute("class") === "note");
  assert.ok(note.textContent.includes("路径块"), note.textContent);
  const heading = find(h.host.shadowRoot, (node) => node.getAttribute("class") === "lbl");
  assert.ok(heading.textContent.includes("旋转"), heading.textContent);
});

test("symbol-path 输入：相邻点击组成固定两位索引串，Backspace 撤销", async (t) => {
  const h = harness(t);
  h.requests[0].respond(pathChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready", JSON.stringify(h.errors) + " / " + h.status.textContent);
  const cells = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  assert.equal(cells.length, 36);
  const order = [0, 1, 2, 8, 14, 15];
  for (const i of order) cells[i].emit("click");
  assert.equal(h.answer.value, "v1:00.01.02.08.14.15");
  assert.equal(h.buttons[0].disabled, false);
  cells[15].emit("keydown", { key: "Backspace" });
  assert.equal(h.answer.value, "", "未完成目标长度时不得提交部分路径");
  assert.equal(h.buttons[0].disabled, true);
});

test("symbol-path 拖动在 pointerup 后结束，释放到格子外不会粘住命中状态", async (t) => {
  const h = harness(t);
  h.requests[0].respond(pathChallenge());
  await flush();
  const cells = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  cells[0].emit("pointerdown", { button: 0, pointerId: 7 });
  h.document.emit("pointerup", { pointerId: 7 });
  for (const i of [1, 2, 8, 14, 15]) cells[i].emit("pointerenter");
  assert.equal(h.answer.value, "", "释放后悬停不能继续追加路径");
  assert.equal(cells[1].getAttribute("data-path"), "false");
});

test("symbol-path 粗指针布局过密时 fail closed，不渲染不可用命中盒", async (t) => {
  const h = harness(t, { coarsePointer: true });
  h.requests[0].respond(pathChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "error");
  assert.equal(h.errors.at(-1).code, "touch-layout");
  assert.equal(findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell").length, 0);
});

test("visual-rank 输入：五张卡按选择顺序合成排列，Undo 撤销最后一张", async (t) => {
  const h = harness(t);
  h.requests[0].respond(rankChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready", JSON.stringify(h.errors) + " / " + h.status.textContent);
  const cards = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  assert.equal(cards.length, 5);
  const ids = cards.map((card) => card.getAttribute("data-card"));
  assert.ok(cards.every((card) => card.getAttribute("aria-pressed") === "false"));
  for (const card of cards) card.emit("click");
  assert.equal(h.answer.value, "v1:" + ids.join("."));
  assert.ok(cards.every((card) => card.getAttribute("aria-pressed") === "true"));
  assert.equal(h.buttons[0].disabled, false);
  const aux = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "aux")[0];
  const btns = findAll(aux, (node) => node.tagName === "BUTTON");
  btns[0].emit("click");
  assert.equal(h.answer.value, "");
  assert.equal(h.buttons[0].disabled, true);
});

test("crowd-find 输入：单格选择，改选只保留最后一个索引", async (t) => {
  const h = harness(t);
  h.requests[0].respond(selectChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready", JSON.stringify(h.errors) + " / " + h.status.textContent);
  const cells = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  assert.equal(cells.length, 36);
  cells[7].emit("click");
  assert.equal(h.answer.value, "v1:7");
  assert.equal(cells[7].getAttribute("aria-pressed"), "true");
  cells[20].emit("click");
  assert.equal(h.answer.value, "v1:20");
  assert.equal(cells[7].getAttribute("aria-pressed"), "false");
  assert.equal(cells[20].getAttribute("aria-pressed"), "true");
  assert.equal(h.buttons[0].disabled, false);
});

// --- ADR-030 Wave 2：progressive-target / shell-track / silhouette-assembly 输入 ---

function progressiveChallenge() {
  return interactiveChallenge({
    image_width: 320, image_height: 466,
    answer_type: "sequence", max_answer_len: 8,
    grid_cols: 3, grid_rows: 3, round_count: 3,
    round_y: 8, round_h: 150,
    sample_x: 12, sample_y: 47, sample_w: 56, sample_h: 56,
    grid_x: 168, grid_y: 5, grid_cell: 44, grid_gap: 4,
    input_mode: "progressive",
    prompt_zh: "三轮定位。", prompt_en: "Three progressive rounds.",
  });
}

function shellChallenge() {
  return interactiveChallenge({
    image_width: 280, image_height: 100,
    answer_type: "sequence", max_answer_len: 8,
    render_mode: "sprite_sheet", frame_count: 33,
    frame_cols: 6, frame_rows: 6, frame_ms: 170, frame_loop: false,
    rounds: 3, frames_per_round: 11,
    boxes: [
      { x: 14, y: 28, w: 52, h: 62 },
      { x: 84, y: 28, w: 52, h: 62 },
      { x: 154, y: 28, w: 52, h: 62 },
      { x: 224, y: 28, w: 52, h: 62 },
    ],
    input_mode: "shell",
    prompt_zh: "跟踪目标容器。", prompt_en: "Track the target container.",
  });
}

function placementsChallenge() {
  return interactiveChallenge({
    image_width: 400, image_height: 260,
    answer_type: "placements", max_answer_len: 32,
    pieces: [
      { id: "3", x: 45, y: 18, w: 54, h: 54 },
      { id: "0", x: 109, y: 18, w: 54, h: 54 },
      { id: "4", x: 173, y: 18, w: 54, h: 54 },
      { id: "1", x: 237, y: 18, w: 54, h: 54 },
      { id: "2", x: 301, y: 18, w: 54, h: 54 },
    ],
    slots: [
      { x: 20, y: 160, w: 64, h: 64 },
      { x: 94, y: 160, w: 64, h: 64 },
      { x: 168, y: 160, w: 64, h: 64 },
      { x: 242, y: 160, w: 64, h: 64 },
      { x: 316, y: 160, w: 64, h: 64 },
    ],
    input_mode: "placements",
    prompt_zh: "装配部件。", prompt_en: "Assemble the pieces.",
  });
}

test("progressive-target 输入：三轮逐轮推进，第三轮后才合成答案", async (t) => {
  const h = harness(t);
  h.requests[0].respond(progressiveChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready", JSON.stringify(h.errors) + " / " + h.status.textContent);
  assert.equal(h.buttons[0].disabled, true);
  let cells = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  assert.equal(cells.length, 9);
  cells[2].emit("click");
  assert.equal(h.answer.value, "", "第一轮选择后不得提前合成答案");
  assert.equal(h.buttons[0].disabled, true);
  cells = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  assert.equal(cells.length, 9);
  cells[5].emit("click");
  assert.equal(h.answer.value, "");
  cells = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell");
  cells[7].emit("click");
  assert.equal(h.answer.value, "v1:2.5.7");
  assert.equal(h.buttons[0].disabled, false);
});

test("sprite geometry uses the same responsive percentages as its hit targets", async (t) => {
  const h = harness(t);
  h.requests[0].respond(shellChallenge()); await flush();
  const view = find(h.host.shadowRoot, (node) => node.getAttribute("class") === "frameview");
  assert.equal(view.style.height, "auto");
  assert.equal(view.style.aspectRatio, "280 / 100");
  assert.equal(view.style.backgroundSize, "600% 600%");
  await h.time.advance(170 * 10);
  assert.equal(view.style.backgroundPosition, "80% 20%", "frame 10 must stay aligned at any display scale");
  const boxes = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell shellbox");
  assert.ok(boxes[3].getAttribute("style").includes("left:80%"));
});

test("shell-track 输入：动画结束前不可选，三轮后合成答案", async (t) => {
  const h = harness(t);
  h.requests[0].respond(shellChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready", JSON.stringify(h.errors) + " / " + h.status.textContent);
  let boxes = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell shellbox");
  assert.equal(boxes.length, 4);
  assert.equal(boxes[0].disabled, true, "动画结束前容器必须不可选");
  assert.equal(boxes[0].getAttribute("aria-pressed"), "false");
  await h.time.advance(170 * 11);
  boxes = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell shellbox");
  assert.equal(boxes[0].disabled, false, "动画结束后容器必须可选");
  boxes[1].emit("click");
  assert.equal(h.answer.value, "");
  boxes = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell shellbox");
  assert.equal(boxes[1].getAttribute("aria-pressed"), "false", "下一轮开始后不应把上一轮选择伪装成当前选中");
  assert.equal(boxes[0].disabled, true, "下一轮动画期间再次不可选");
  await h.time.advance(170 * 11);
  boxes = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell shellbox");
  boxes[2].emit("click");
  await h.time.advance(170 * 11);
  boxes = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell shellbox");
  boxes[1].emit("click");
  assert.equal(h.answer.value, "v1:1.2.1");
  assert.equal(boxes[1].getAttribute("aria-pressed"), "true");
  assert.equal(h.buttons[0].disabled, false);
});

test("shell-track + prefers-reduced-motion：明确失败，不强迫完成运动题", async (t) => {
  const h = harness(t, { reducedMotion: true });
  h.requests[0].respond(shellChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "error");
  assert.equal(h.errors.length, 1);
  assert.equal(h.errors[0].code, "motion-required");
  assert.equal(h.buttons[0].disabled, true);
  assert.equal(findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell").length, 0,
    "reduced-motion 下不得渲染可点击容器");
});

test("silhouette-assembly 输入：选择/旋转/放置，五格填满才合成答案", async (t) => {
  const h = harness(t);
  h.requests[0].respond(placementsChallenge());
  await flush();
  assert.equal(h.api.state(h.id), "ready", JSON.stringify(h.errors) + " / " + h.status.textContent);
  const pieces = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell piece");
  const slots = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell slot");
  assert.equal(pieces.length, 5);
  assert.equal(slots.length, 5);
  assert.equal(h.buttons[0].disabled, true);
  const ids = pieces.map((p) => p.getAttribute("data-piece"));
  for (let i = 0; i < 5; i++) {
    pieces[i].emit("click");
    const rotate = findAll(h.host.shadowRoot, (node) => node.tagName === "BUTTON" && node.textContent === "Rotate")[0];
    if (i === 0) rotate.emit("click");
    slots[i].emit("click");
  }
  assert.equal(h.answer.value, "v1:" + ids.map((id, i) => id + ":" + (i === 0 ? 1 : 0)).join(","));
  assert.equal(h.buttons[0].disabled, false);
});

test("silhouette-assembly 不覆盖已占用槽位，部件不会凭空丢失", async (t) => {
  const h = harness(t);
  h.requests[0].respond(placementsChallenge());
  await flush();
  const pieces = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell piece");
  const slots = findAll(h.host.shadowRoot, (node) => node.getAttribute("class") === "cell slot");
  pieces[0].emit("click");
  slots[0].emit("click");
  assert.equal(h.api.state(h.id), "ready");
  pieces[1].emit("click");
  slots[0].emit("click");
  assert.equal(h.answer.value, "", "覆盖操作不得生成不完整但不可恢复的答案");
  const summary = find(h.host.shadowRoot, (node) => node.getAttribute("class") === "cellsum");
  assert.match(summary.textContent, /occupied|已有部件/);
  assert.equal(slots[0].firstChild.getAttribute("class"), "pieceimg");
  assert.equal(pieces[0].getAttribute("data-used"), "true");
  assert.equal(pieces[1].getAttribute("data-used"), "false");
});
