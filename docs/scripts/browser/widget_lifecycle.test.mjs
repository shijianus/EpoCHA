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
  }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  getAttribute(name) { return this.attributes.get(name) ?? null; }
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

function challenge(token = "challenge-1", expires = 0) {
  return {
    token, mode: "pow_question", expires_in: expires,
    pow: { algo: "sha256-prefix", bits: 12, seed: "AAECAwQFBgcICQoLDA0ODw" },
    question: {
      media: "image", image_mime: "image/png", image_base64: "AQIDBA==",
      image_width: 120, image_height: 60, length: 6, charset: "ABCDEF",
    },
  };
}

function harness(t, { honorAbort = false, options = {} } = {}) {
  const time = clock();
  const requests = [], workers = [], successes = [], errors = [], states = [], expired = [];
  const urls = new Set();
  let blobSeq = 0;
  const document = new Events();
  document.visibilityState = "visible";
  document.createElement = (tag) => new Element(tag, document);
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
    document, navigator: { language: "en" }, location: new URL("https://page.invalid/"),
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
  const answer = find(host.shadowRoot, (node) => node.getAttribute("class") === "ans");
  const buttons = find(host.shadowRoot, (node) => node.getAttribute("class") === "box")
    .children.filter((node) => node.tagName === "BUTTON");
  const status = find(host.shadowRoot, (node) => node.getAttribute("role") === "status");
  const h = {
    api, id, time, requests, workers, urls, successes, errors, states, expired,
    document, container, host, field, answer, buttons, status,
    calls: (path) => requests.filter((request) => request.path === "/api/" + path),
    clickSubmit() { buttons[0].emit("click"); },
    async ready(request = requests.at(-1), token = "challenge-1", expires = 0) {
      request.respond(challenge(token, expires));
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
