/*! epocha widget v1 — 零依赖原生 JS 人机验证组件（DESIGN §11、D10）
 * 用法与选项见 docs/WIDGET.md。安全边界：Shadow DOM 只是样式与命名隔离，**不是**安全边界；
 * 浏览器里出现的唯一凭据是 site_key，服务端到服务端的验证凭据永远只在业务后端。
 */
(function (global) {
  "use strict";

  var WIDGET_VERSION = "v1";
  var MIME_PNG = "image/png";
  // 单题 PNG 上限 65536 字节（store 0004 的 CHECK），base64 后约 87382 字符。
  // 上限写在这里的作用是"畸形的巨大响应不会先展开再丢弃"，不是安全边界。
  var MAX_IMAGE_B64_CHARS = 87384;
  var POW_ALGO = "sha256-prefix";

  // 与 web/widget/v1.worker.js **逐字节相同**（由 widget_test.go 的防漂移测试钉住）：
  // Blob Worker 用这段源码，同源部署 workerUrl 用那个文件，两条路径必须跑同一份代码。
  var POW_WORKER_SOURCE = String.raw`/*! epocha widget PoW worker v1 — sha256-prefix 工作量证明（DESIGN §7）
 *
 * 这份文件是 PoW 求解的**唯一实现**。它以两种方式加载：
 *   A) 默认：widget 读取内嵌的同一份源码（见 v1.js 里的 POW_WORKER_SOURCE）创建 Blob Worker，
 *      需要页面 CSP 允许 worker-src blob:；
 *   B) 可选：接入方把本文件与 v1.js 同源部署，用 workerUrl 指定。
 * 两条路径跑的是**逐字节相同**的源码，由 web/widget/widget_test.go 的防漂移测试钉住。
 *
 * 为什么自己实现 SHA-256 而不是 crypto.subtle.digest：
 * 后者每次摘要都要跨一次异步边界，实测吞吐约 2–4k 次/秒，bits=18（期望 26 万次）
 * 要几十秒，只能靠调低服务端难度掩盖——那是红线（不许降低难度）。纯 JS 的紧凑循环
 * 在这个输入长度（≤32 字节）上是每秒数十万到百万级。
 * 代价是**必须**有跨实现一致性的证据：固定测试向量见 internal/pow/testdata/pow_vectors.json，
 * Go 侧断言"由实现重算等于文件内容"，Node 侧断言"由这份 JS 重算等于同一文件内容"。
 */
"use strict";

// K[0..63]：SHA-256 轮常量（前 64 个素数立方根小数部分的前 32 位）。
// 这张表**不是手抄的**：由 FIPS 180-4 的定义算出，并与 $(go env GOROOT)/src/crypto/
// internal/fips140/sha256 里的表逐项比过（手抄出错过——本轮 10 项是错的，跨实现向量测试抓到）。
var K = [
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2
];

var H0 = [
  0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19
];

var w = new Int32Array(64);
var digestOut = new Uint8Array(32);

// sha256Into 把 msg 的摘要写进 out（避免每次分配新数组：这是热循环）。
function sha256Into(msg, out) {
  var h0 = H0[0], h1 = H0[1], h2 = H0[2], h3 = H0[3], h4 = H0[4], h5 = H0[5], h6 = H0[6], h7 = H0[7];
  var len = msg.length;
  var bitLen = len * 8;
  // 填充：0x80 + 若干 0 + 64 位大端长度，总长 ≡ 0 (mod 64)。
  var padLen = ((len + 9 + 63) & ~63);
  var buf = sha256Into._buf;
  if (!buf || buf.length < padLen) {
    buf = sha256Into._buf = new Uint8Array(padLen);
  }
  buf.fill(0, 0, padLen);
  buf.set(msg.subarray(0, len));
  buf[len] = 0x80;
  // 长度只写低 32 位即可：我们的输入远小于 512MB，但按规范两个 32 位字都写。
  var hi = Math.floor(bitLen / 4294967296);
  var lo = bitLen >>> 0;
  buf[padLen - 8] = (hi >>> 24) & 0xff;
  buf[padLen - 7] = (hi >>> 16) & 0xff;
  buf[padLen - 6] = (hi >>> 8) & 0xff;
  buf[padLen - 5] = hi & 0xff;
  buf[padLen - 4] = (lo >>> 24) & 0xff;
  buf[padLen - 3] = (lo >>> 16) & 0xff;
  buf[padLen - 2] = (lo >>> 8) & 0xff;
  buf[padLen - 1] = lo & 0xff;

  for (var off = 0; off < padLen; off += 64) {
    for (var i = 0; i < 16; i++) {
      var j = off + i * 4;
      w[i] = ((buf[j] << 24) | (buf[j + 1] << 16) | (buf[j + 2] << 8) | buf[j + 3]) | 0;
    }
    for (i = 16; i < 64; i++) {
      var s0 = w[i - 15], s1 = w[i - 2];
      var d0 = ((s0 >>> 7) | (s0 << 25)) ^ ((s0 >>> 18) | (s0 << 14)) ^ (s0 >>> 3);
      var d1 = ((s1 >>> 17) | (s1 << 15)) ^ ((s1 >>> 19) | (s1 << 13)) ^ (s1 >>> 10);
      w[i] = (w[i - 16] + d0 + w[i - 7] + d1) | 0;
    }
    var a = h0, b = h1, c = h2, d = h3, e = h4, f = h5, g = h6, hh = h7;
    for (i = 0; i < 64; i++) {
      var S1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
      var ch = (e & f) ^ (~e & g);
      var t1 = (hh + S1 + ch + K[i] + w[i]) | 0;
      var S0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
      var maj = (a & b) ^ (a & c) ^ (b & c);
      var t2 = (S0 + maj) | 0;
      hh = g; g = f; f = e; e = (d + t1) | 0;
      d = c; c = b; b = a; a = (t1 + t2) | 0;
    }
    h0 = (h0 + a) | 0; h1 = (h1 + b) | 0; h2 = (h2 + c) | 0; h3 = (h3 + d) | 0;
    h4 = (h4 + e) | 0; h5 = (h5 + f) | 0; h6 = (h6 + g) | 0; h7 = (h7 + hh) | 0;
  }

  var vals = [h0, h1, h2, h3, h4, h5, h6, h7];
  for (i = 0; i < 8; i++) {
    var v = vals[i];
    out[i * 4] = (v >>> 24) & 0xff;
    out[i * 4 + 1] = (v >>> 16) & 0xff;
    out[i * 4 + 2] = (v >>> 8) & 0xff;
    out[i * 4 + 3] = v & 0xff;
  }
  return out;
}

function sha256Bytes(msg) {
  var out = new Uint8Array(32);
  sha256Into(msg, out);
  return out;
}

function sha256Hex(msg) {
  var d = sha256Bytes(msg);
  var s = "";
  for (var i = 0; i < 32; i++) s += (d[i] < 16 ? "0" : "") + d[i].toString(16);
  return s;
}

// leadingZeroBits 与 internal/pow.LeadingZeroBits 同义：按字节序、高位优先计数。
function leadingZeroBits(b) {
  var n = 0;
  for (var i = 0; i < b.length; i++) {
    var c = b[i];
    if (c === 0) { n += 8; continue; }
    for (var mask = 0x80; (mask & c) === 0; mask >>= 1) n++;
    return n;
  }
  return n;
}

function b64urlToBytes(s) {
  // 只依赖标准 Web API：atob 由浏览器 worker 提供，Node 侧的向量测试在自己的沙箱里注入它。
  // 这里刻意不写 Buffer 回落：生产脚本里不该有"按环境挑实现"的分支。
  var bin = atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice(0, (4 - (s.length % 4)) % 4));
  var out = new Uint8Array(bin.length);
  for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i) & 0xff;
  return out;
}

// MAX_NONCE 与 Go 侧 pow.maxNonceValue 一致：十进制、≤16 位、≤1e12。
// 越过这个界就不是"还没解出来"，而是"解会被服务端当成格式非法"，必须停。
var MAX_NONCE = 1e12;

// solveChunk 从 start 开始试 tries 个候选；返回 nonce 或 null（表示这一段没中）。
// 缓冲复用：seed 在前，nonce 的 ASCII 数字在后，长度就是本轮 nonce 的十进制长度。
function solveChunk(job, start, tries) {
  var seed = job.seedBytes, bits = job.bits, buf = job.buf;
  buf.set(seed);
  var end = start + tries;
  if (end > MAX_NONCE) end = MAX_NONCE;
  for (var n = start; n < end; n++) {
    var s = String(n);
    for (var j = 0; j < s.length; j++) buf[seed.length + j] = s.charCodeAt(j);
    sha256Into(buf.subarray(0, seed.length + s.length), job.out);
    if (leadingZeroBits(job.out) >= bits) return { nonce: s, tries: n + 1 };
  }
  return null;
}

function startJob(m) {
  var p = m.data || {};
  if (p.type !== "solve") return;
  var jobId = p.job;
  if (p.algo !== "sha256-prefix") {
    post({ type: "failed", job: jobId, reason: "unknown-algo" });
    return;
  }
  var bits = Number(p.bits);
  if (!(bits >= 12 && bits <= 24)) { post({ type: "failed", job: jobId, reason: "bits-out-of-range" }); return; }
  var seed;
  try { seed = b64urlToBytes(String(p.seed)); } catch (e) { post({ type: "failed", job: jobId, reason: "bad-seed" }); return; }
  if (seed.length !== 16) { post({ type: "failed", job: jobId, reason: "bad-seed" }); return; }
  var maxTries = Math.min(Number(p.maxTries) > 0 ? Number(p.maxTries) : 5e7, MAX_NONCE);
  var chunk = Math.max(1000, Math.min(Number(p.chunk) > 0 ? Number(p.chunk) : 2e4, 1e6));

  var job = {
    bits: bits,
    seedBytes: seed,
    buf: new Uint8Array(seed.length + 16),
    out: new Uint8Array(32),
  };
  state = { jobId: jobId, next: 0, maxTries: maxTries, chunk: chunk, job: job, startedAt: nowMs(), lastReport: 0, cancelled: false };
  post({ type: "started", job: jobId, bits: bits, maxTries: maxTries });
  runChunk();
}

var state = null;

function nowMs() {
  return (typeof performance !== "undefined" && performance.now) ? performance.now() : Date.now();
}

function post(m) {
  if (typeof self !== "undefined" && typeof self.postMessage === "function") self.postMessage(m);
}

function runChunk() {
  var s = state;
  if (!s || s.cancelled) return;
  var budget = s.maxTries - s.next;
  if (budget <= 0) {
    post({ type: "failed", job: s.jobId, reason: "exhausted", tries: s.next, ms: Math.round(nowMs() - s.startedAt) });
    state = null;
    return;
  }
  var take = Math.min(s.chunk, budget);
  var got = solveChunk(s.job, s.next, take);
  s.next += take;
  if (got) {
    post({
      type: "solved", job: s.jobId, nonce: got.nonce, tries: got.tries,
      ms: Math.round(nowMs() - s.startedAt),
    });
    state = null;
    return;
  }
  // 进度按节流上报，避免"每个候选一条消息"把主线程拖垮。
  var elapsed = nowMs() - s.startedAt;
  if (s.next - s.lastReport >= 50000 || elapsed >= 250) {
    s.lastReport = s.next;
    post({ type: "progress", job: s.jobId, tries: s.next, ms: Math.round(elapsed) });
  }
  // 交还事件循环：同步死循环会吞掉 cancel 消息，"可取消"就不是事实。
  setTimeout(runChunk, 0);
}

self.addEventListener("message", function (m) {
  var p = m.data || {};
  if (p.type === "solve") { startJob(m); return; }
  if (p.type === "cancel") {
    if (state) {
      state.cancelled = true;
      post({ type: "cancelled", job: state.jobId });
      state = null;
    }
    return;
  }
  if (p.type === "ping") post({ type: "pong", job: p.job });
});

// 暴露给 worker 作用域本身（页面看不见 worker 的全局）。
// Node 侧的向量测试靠它在同一个 vm 里直接调用哈希实现，
// 于是"测的这份实现"与"浏览器里跑的那份实现"是同一份字节。
if (typeof self !== "undefined") {
  self.epochaPow = { sha256Hex: sha256Hex, sha256Bytes: sha256Bytes, leadingZeroBits: leadingZeroBits };
}`;

  var STATES = {
    IDLE: "idle", LOADING: "loading", READY: "ready", SOLVING: "solving",
    SUBMITTING: "submitting", VERIFIED: "verified", EXPIRED: "expired",
    ERROR: "error", DESTROYED: "destroyed"
  };
  // 需要用户介入（不能自动继续）的状态集合。
  var BUSY = { loading: 1, solving: 1, submitting: 1 };

  var I18N = {
    "zh-CN": {
      instruction: "请输入图片中显示的 {length} 个字符",
      charsetNote: "字符集：{charset}（不含易混字符 I O 0 1）；大小写不敏感。每题只有一次作答机会。",
      imageAlt: "人机验证图片：请输入图中显示的 {length} 个字符",
      submit: "提交答案",
      refresh: "换一张",
      retry: "重试",
      solving: "正在本地计算（不会把答案或服务端数据送出）…",
      solvingProgress: "正在本地计算 PoW：已尝试 {tries} 次（{ms} ms）",
      submitting: "计算完成，正在提交答案…",
      ready: "请看清图片后输入字符，然后提交。",
      verified: "验证通过。",
      expired: "这张题已过期，token 已作废。请换一张重新验证。",
      destroyed: "组件已销毁。",
      errNetwork: "无法连接验证服务，请检查网络后重试。",
      errRateLimited: "请求过于频繁，请稍后重试。",
      errUnavailable: "验证服务暂时给不出题目，稍后会自动重试。",
      errInvalidChallenge: "这张题已经不可用了（答错过期或被作废）。请换一张。",
      errInvalidRequest: "请求不被接受：站点标识或输入格式有问题。",
      errWorkerBlocked: "当前页面的 CSP 禁止创建计算线程，无法本地完成验证。"
        + "请把 worker 文件同源部署并配置 workerUrl，或在 CSP 中允许 worker-src blob:。",
      errWorkerUnsupported: "这个浏览器不支持 Worker，无法本地完成验证。",
      errPowTimeout: "本地计算超时（设备较慢或难度较高）。可以再试一次。",
      errPowExhausted: "本地计算达到尝试上限，未找到解。请换一张。",
      errImageUnusable: "题目图片无法显示。请换一张。",
      errBadResponse: "验证服务返回了无法识别的响应。",
      errExpiredSend: "题目已过期，请换一张后提交。",
      experimental: "实验性视觉题型：未经 OCR 与人工可读性评测，不代表抗机器能力。",
      a11yGap: "本题型只有视觉通道：无法看清图片的用户目前无法完成验证（非视觉等价通道尚未实现）。",
      answerPlaceholder: "6 个字符"
    },
    "en": {
      instruction: "Type the {length} characters shown in the image",
      charsetNote: "Charset: {charset} (no confusable I O 0 1); case-insensitive. One attempt per question.",
      imageAlt: "Verification image: type the {length} characters shown",
      submit: "Submit",
      refresh: "New image",
      retry: "Retry",
      solving: "Computing locally…",
      solvingProgress: "Solving proof-of-work locally: {tries} attempts ({ms} ms)",
      submitting: "Submitting answer…",
      ready: "Read the image, type the characters, then submit.",
      verified: "Verified.",
      expired: "This question expired and its token was discarded. Start a new one.",
      destroyed: "Widget destroyed.",
      errNetwork: "Cannot reach the verification service. Check the network and retry.",
      errRateLimited: "Too many requests. Please wait and retry.",
      errUnavailable: "The verification service has no question available right now; retrying shortly.",
      errInvalidChallenge: "This question is no longer usable. Get a new one.",
      errInvalidRequest: "Request rejected: bad site key or input format.",
      errWorkerBlocked: "This page's Content-Security-Policy blocks worker creation, so local "
        + "verification cannot run. Host the worker file same-origin with workerUrl, "
        + "or allow worker-src blob: in the CSP.",
      errWorkerUnsupported: "This browser does not support Web Workers, so local verification cannot run.",
      errPowTimeout: "Local computation timed out (slow device or high difficulty). You can retry.",
      errPowExhausted: "Local computation hit its attempt limit without finding a solution. Get a new image.",
      errImageUnusable: "The question image could not be displayed. Get a new one.",
      errBadResponse: "The verification service returned an unrecognized response.",
      errExpiredSend: "The question expired before submit. Get a new one.",
      experimental: "Experimental visual question type: not evaluated against OCR or human-readability "
        + "benchmarks, so it does not claim machine resistance.",
      a11yGap: "Visual channel only: users who cannot see the image cannot complete this check "
        + "(a non-visual equivalent is not implemented yet).",
      answerPlaceholder: "{length} characters"
    }
  };

  function pickLang(requested) {
    var want = String(requested || (global.navigator && (global.navigator.language || global.navigator.userLanguage)) || "en");
    var lower = want.toLowerCase();
    return lower.indexOf("zh") === 0 ? "zh-CN" : "en";
  }

  function tr(lang, key, vars) {
    var table = I18N[lang] || I18N.en;
    var s = table[key] || I18N.en[key] || key;
    if (vars) {
      s = s.replace(/\{(\w+)\}/g, function (m, name) {
        return Object.prototype.hasOwnProperty.call(vars, name) ? String(vars[name]) : m;
      });
    }
    return s;
  }

  // --- 小工具（全部是安全 DOM 构造：服务端字符串只进 textContent）---

  function el(tag, attrs, text) {
    var node = document.createElement(tag);
    if (attrs) for (var k in attrs) if (Object.prototype.hasOwnProperty.call(attrs, k)) node.setAttribute(k, attrs[k]);
    if (text != null) node.textContent = String(text);
    return node;
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  function base64Valid(s) {
    return typeof s === "string" && /^[A-Za-z0-9+/]+={0,2}$/.test(s) && s.length % 4 === 0;
  }

  function b64urlToBytes(s) {
    var bin = global.atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice(0, (4 - (s.length % 4)) % 4));
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i) & 0xff;
    return out;
  }

  // leadingZeroBits 与 internal/pow.LeadingZeroBits、worker 里的实现同义（三位一体由向量测试钉住）。
  function leadingZeroBits(b) {
    var n = 0;
    for (var i = 0; i < b.length; i++) {
      var c = b[i];
      if (c === 0) { n += 8; continue; }
      for (var mask = 0x80; (mask & c) === 0; mask >>= 1) n++;
      return n;
    }
    return n;
  }

  var STYLE_RULES = [
    ":host{all:unset;display:block;max-width:360px;font:14px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;color:#1c1c1c}",
    ".box{border:1px solid #b9b9b9;border-radius:6px;padding:10px;background:#fff}",
    ".img{display:flex;justify-content:center;align-items:center;min-height:96px;background:#fff;border:1px dashed #9a9a9a;border-radius:4px;overflow:hidden}",
    ".img img{max-width:100%;height:auto}",
    ".lbl{display:block;font-weight:600;margin:10px 0 4px}",
    "input.ans{width:100%;box-sizing:border-box;font:inherit;letter-spacing:.3em;text-transform:uppercase;padding:6px}",
    "input.ans[aria-invalid=true]{border-color:#a31414;border-width:2px}",
    "button{font:inherit;padding:6px 10px;margin:8px 6px 0 0;border-radius:4px;border:1px solid #7a7a7a;background:#f2f2f2;cursor:pointer}",
    "button:focus-visible,input:focus-visible{outline:2px solid #1a5fb4;outline-offset:2px}",
    "button[disabled]{opacity:.5;cursor:default}",
    ".st{margin:8px 0 0;padding:6px 8px;border:1px solid #c9c9c9;border-radius:4px;min-height:1.2em}",
    ".st[data-kind=ok]{border-color:#1c7d33;background:rgba(28,125,51,.08)}",
    ".st[data-kind=err]{border-color:#a31414;background:rgba(163,20,20,.07)}",
    ".st[data-kind=busy]{border-color:#1a5fb4;background:rgba(26,95,180,.07)}",
    ".note{margin:8px 0 0;font-size:12px;color:#555}",
    ".tag{display:inline-block;margin:8px 0 0;font-size:12px;border:1px solid #b0761a;border-radius:4px;padding:2px 6px;color:#7a5312}"
  ];
  var STYLE = STYLE_RULES.join("");

  // 构造式样式表不需要 CSP 的 style-src 例外；老浏览器回落到 <style> 时才需要，文档里写明。
  function applyStyle(root) {
    if (typeof CSSStyleSheet === "function" && typeof root.adoptedStyleSheets !== "undefined") {
      try {
        var sheet = new CSSStyleSheet();
        sheet.replaceSync(STYLE);
        root.adoptedStyleSheets = [sheet];
        return;
      } catch (e) { /* 落到下面的回落路径 */ }
    }
    var tag = document.createElement("style");
    tag.textContent = STYLE;
    root.appendChild(tag);
  }

  var registry = new Map();
  var seq = 0;
  var visibilityBound = false;

  function onVisibility() {
    if (global.document && global.document.visibilityState === "visible") {
      registry.forEach(function (w) { w._recheckDeadline(); });
    }
  }

  function bindVisibility() {
    if (visibilityBound || !global.document || !global.document.addEventListener) return;
    global.document.addEventListener("visibilitychange", onVisibility);
    visibilityBound = true;
  }

  function maybeUnbindVisibility() {
    if (visibilityBound && registry.size === 0 && global.document) {
      global.document.removeEventListener("visibilitychange", onVisibility);
      visibilityBound = false;
    }
  }

  function normalizeOptions(o) {
    o = o || {};
    if (!o.apiBase) throw new TypeError("EpoCHA.render: options.apiBase 必填");
    if (!o.siteKey) throw new TypeError("EpoCHA.render: options.siteKey 必填");
    var opt = {
      apiBase: String(o.apiBase).replace(/\/+$/, ""),
      siteKey: String(o.siteKey),
      lang: pickLang(o.language),
      fieldName: o.fieldName === undefined ? "epocha_response" : String(o.fieldName),
      difficulty: null, // 难度由服务端（站点配置）决定，客户端没有这个权限
      onSuccess: typeof o.onSuccess === "function" ? o.onSuccess : null,
      onError: typeof o.onError === "function" ? o.onError : null,
      onExpired: typeof o.onExpired === "function" ? o.onExpired : null,
      onState: typeof o.onState === "function" ? o.onState : null,
      workerUrl: o.workerUrl ? String(o.workerUrl) : "",
      powTimeoutMs: Math.min(Math.max(Number(o.powTimeoutMs) || 20000, 1000), 120000),
      powMaxTries: Math.min(Number(o.powMaxTries) || 0 || 50000000, 1e12),
      fetchTimeoutMs: Math.min(Math.max(Number(o.fetchTimeoutMs) || 10000, 1000), 60000),
      maxAutoRetry: Math.min(Number(o.maxAutoRetry) || 0 || 2, 5),
      showExperimentalNote: o.showExperimentalNote !== false,
      // form：<form> 元素或它的 id。容器不在目标 form 内时用它显式关联字段。
      // 刻意不做的事：不接受跨文档元素、不接受选择器字符串（querySelector 会把"随便挑一个 form"
      // 变成默认行为），也不接受服务端 challenge 里带回来的任何东西。
      form: o.form === undefined || o.form === null || o.form === "" ? null : o.form,
      ariaLabel: o.ariaLabel || ""
    };
    if (opt.workerUrl) {
      // 只允许同源 worker：跨域 URL 等于把可执行代码的来源交给服务端/第三方决定。
      var abs;
      try { abs = new URL(opt.workerUrl, global.location ? global.location.href : undefined); }
      catch (e) { throw new TypeError("EpoCHA.render: workerUrl 不是合法 URL"); }
      if (!global.location || abs.origin !== global.location.origin) {
        throw new TypeError("EpoCHA.render: workerUrl 必须与页面同源（不允许远程代码）");
      }
      opt.workerUrl = abs.href;
    }
    if (!/^[A-Za-z0-9_-]{1,64}$/.test(opt.siteKey)) {
      throw new TypeError("EpoCHA.render: siteKey 形态不合法");
    }
    if (opt.fieldName && !/^[A-Za-z0-9_.:\-\[\]]{0,128}$/.test(opt.fieldName)) {
      throw new TypeError("EpoCHA.render: fieldName 形态不合法");
    }
    return opt;
  }

  // resolveForm：把 `form` 选项解析成**当前文档里的真实 <form>**。
  // 返回 null 表示"没关联上"，调用方据此 fail loudly —— 隐藏字段落在 form 外等于
  // 什么都没提交，而页面看起来一切正常，是最难被发现的一类接入错误。
  function resolveForm(spec, container) {
    var doc = container.ownerDocument;
    if (!doc) return null;
    if (spec && typeof spec === "object" && spec.nodeType === 1) {
      // 传元素：必须是同一文档里的 FORM。跨文档（iframe/adopted node）不参与本文档的提交。
      return String(spec.tagName) === "FORM" && spec.ownerDocument === doc ? spec : null;
    }
    if (typeof spec === "string") {
      var byId = doc.getElementById(spec);
      return byId && String(byId.tagName) === "FORM" ? byId : null;
    }
    return null;
  }

  function Widget(id, container, o) {
    this.id = id;
    this.o = o;
    this.container = container;
    this.state = STATES.IDLE;
    this.challenge = null;   // {token, pow, question, deadlineEpoch}
    this.token = "";         // 一次性 response token
    this.tokenDeadline = 0;
    this.gen = 0;            // 世代计数：晚到的响应必须按世代丢弃
    this.worker = null;
    this.workerBlobUrl = "";
    this.jobSeq = 0;
    this.pending = {};       // jobId → {settle, cancel}；同一时刻最多一个在算
    this.inflightJob = 0;
    this.inflight = null;    // 当前 fetch 的 AbortController
    this.timer = 0;
    this.autoRetry = 0;
    this.destroyed = false;
    this.successFired = false;
    this.listeners = [];
    this._build();
  }

  Widget._ = function () {};

  Widget.prototype._build = function () {
    var o = this.o;
    // 隐藏字段必须挂在 **light DOM**：Shadow DOM 里的 input 不参与表单提交。
    this.field = null;
    if (o.fieldName) {
      // 先定目标 form，再插入字段：反过来（先插再判定）就留下一枚"验证过但没提交"的孤儿 input。
      var implicit = typeof this.container.closest === "function" ? this.container.closest("form") : null;
      var want = o.form ? resolveForm(o.form, this.container) : implicit;
      if (o.form && !want) {
        throw new TypeError("EpoCHA.render: form 选项没有指向当前文档里的真实 <form>");
      }
      if (!want) {
        // 静默不提交是比报错更糟的结果：宁可接入期就炸，也不让线上"验证过了但后端收不到"。
        throw new TypeError("EpoCHA.render: fieldName 要求把字段提交出去，但容器不在任何 <form> 内。"
          + "把容器放进目标 form、用 form 选项显式关联，或设 fieldName:'' 只走 onSuccess/getResponse。");
      }
      this.field = el("input", { type: "hidden", name: o.fieldName });
      this.field.setAttribute("data-epocha-field", this.id);
      // 容器已在目标 form 内 ⇒ 字段落进容器（跟着容器一起移动/移除）；
      // 显式 form 关联 ⇒ 字段落进那个 form，容器可以在页面任意位置。
      (want.contains(this.container) ? this.container : want).appendChild(this.field);
    }

    this.host = el("div", { "data-epocha-widget": this.id });
    this.container.appendChild(this.host);
    this.root = this.host.attachShadow({ mode: "open" });
    applyStyle(this.root);

    var box = el("div", { class: "box", role: "group" });
    var headingId = this.id + "-lbl";
    var statusId = this.id + "-st";
    var noteId = this.id + "-note";
    this.heading = el("label", { class: "lbl", id: headingId }, tr(o.lang, "instruction", { length: 6 }));
    this.imgWrap = el("div", { class: "img" });
    this.imgWrap.textContent = "…";
    this.answer = el("input", {
      class: "ans", type: "text", id: this.id + "-ans", autocomplete: "off",
      autocapitalize: "characters", spellcheck: "false", maxlength: "8",
      "aria-required": "true", "aria-invalid": "false", "aria-describedby": noteId + " " + statusId
    });
    this.answer.placeholder = tr(o.lang, "answerPlaceholder", { length: 6 });
    this.btnSubmit = el("button", { type: "button" }, tr(o.lang, "submit"));
    this.btnRefresh = el("button", { type: "button" }, tr(o.lang, "refresh"));
    this.status = el("p", { class: "st", id: statusId, role: "status", "aria-live": "polite" }, "…");
    this.note = el("p", { class: "note", id: noteId }, tr(o.lang, "charsetNote", { charset: "—" }));
    box.setAttribute("aria-labelledby", headingId);

    box.appendChild(this.heading);
    box.appendChild(this.imgWrap);
    box.appendChild(this.answer);
    box.appendChild(this.btnSubmit);
    box.appendChild(this.btnRefresh);
    box.appendChild(this.status);
    box.appendChild(this.note);
    if (o.showExperimentalNote) box.appendChild(el("p", { class: "tag" }, tr(o.lang, "experimental")));
    if (o.ariaLabel) box.setAttribute("aria-label", o.ariaLabel);
    this.root.appendChild(box);
    this.box = box;

    var self = this;
    this._on(this.btnSubmit, "click", function (e) { e.preventDefault(); self.submit(); });
    this._on(this.btnRefresh, "click", function (e) { e.preventDefault(); self.refresh(); });
    this._on(this.answer, "keydown", function (e) {
      if (e.key === "Enter") { e.preventDefault(); self.submit(); }
    });
  };

  Widget.prototype._on = function (node, type, fn) {
    node.addEventListener(type, fn);
    this.listeners.push([node, type, fn]);
  };

  Widget.prototype._setState = function (next) {
    this.state = next;
    if (this.o.onState) { try { this.o.onState(next, this.id); } catch (e) { /* 回调异常不影响组件 */ } }
  };

  Widget.prototype._say = function (text, kind) {
    this.status.textContent = String(text);
    this.status.setAttribute("data-kind", kind || "idle");
  };

  Widget.prototype._fail = function (code, i18nKey, vars, retryable) {
    this._clearDeadline();
    this._terminateWorker();
    this.challenge = null;
    this._setToken("");
    this._setState(STATES.ERROR);
    this._say(tr(this.o.lang, i18nKey, vars), "err");
    this.btnSubmit.disabled = true;
    this.btnRefresh.disabled = false;
    this.answer.disabled = true;
    this.answer.setAttribute("aria-invalid", "true");
    var err = { code: code, message: tr(this.o.lang, i18nKey, vars), retryable: !!retryable, widgetId: this.id };
    if (this.o.onError) { try { this.o.onError(err); } catch (e) { /* 同上 */ } }
    return err;
  };

  Widget.prototype._enable = function (busy) {
    this.btnSubmit.disabled = !busy;
    this.btnRefresh.disabled = !busy;
    this.answer.disabled = !busy;
  };

  Widget.prototype._clearDeadline = function () {
    if (this.timer) { global.clearTimeout(this.timer); this.timer = 0; }
  };

  // 客户端倒计时只为体验；服务端才是最终裁决（过期后库里一样拒绝）。
  // 服务端没给 expires_in 时**不猜**固定两分钟：不设倒计时，等服务端判定。
  Widget.prototype._armDeadline = function (deadlineEpoch, onExpire) {
    var self = this;
    this._clearDeadline();
    if (!deadlineEpoch) return;
    this.deadlineEpoch = deadlineEpoch;
    var ms = deadlineEpoch - Date.now();
    if (ms <= 0) { onExpire(); return; }
    this.timer = global.setTimeout(function () { self.deadlineEpoch = 0; onExpire(); }, ms);
  };

  Widget.prototype._recheckDeadline = function () {
    if (this.destroyed) return;
    if (!this.deadlineEpoch) return;
    if (Date.now() >= this.deadlineEpoch) {
      this.deadlineEpoch = 0;
      if (this.state === STATES.VERIFIED) this._expireToken();
      else if (this.challenge) this._expireChallenge();
    }
  };

  Widget.prototype._expireChallenge = function () {
    this.challenge = null;
    this._clearDeadline();
    this._terminateWorker();
    this._setState(STATES.EXPIRED);
    this._say(tr(this.o.lang, "expired"), "err");
    this.btnSubmit.disabled = true;
    this.btnRefresh.disabled = false;
  };

  // 过期必须**清掉 token**，不能只改 UI：否则 getResponse() 还能拿到一个已作废的值。
  Widget.prototype._expireToken = function () {
    this._setToken("");
    this._clearDeadline();
    this._setState(STATES.EXPIRED);
    this._say(tr(this.o.lang, "expired"), "err");
    this.btnRefresh.disabled = false;
    if (this.o.onExpired) { try { this.o.onExpired(this.id); } catch (e) { /* 同上 */ } }
  };

  Widget.prototype._setToken = function (v) {
    this.token = v || "";
    if (this.field) this.field.value = this.token;
  };

  Widget.prototype.load = function () {
    if (this.destroyed) return;
    var self = this, o = this.o;
    this.gen++;
    var my = this.gen;
    this._clearDeadline();
    this._terminateWorker();
    this.challenge = null;
    this._setToken("");
    this.successFired = false;
    this.answer.value = "";
    this.answer.setAttribute("aria-invalid", "false");
    this._setState(STATES.LOADING);
    this._say("…", "busy");
    this._enable(false);

    var controller = new AbortController();
    this.inflight = controller;
    var to = global.setTimeout(function () { controller.abort(); }, o.fetchTimeoutMs);

    this._post("/api/challenge", { site_key: o.siteKey, lang: o.lang }, controller.signal)
      .then(function (r) {
        global.clearTimeout(to);
        if (self.destroyed || my !== self.gen) return;      // 晚到的响应不能覆盖新题
        self.inflight = null;
        if (!r.ok) { self._handleHttpFailure(r); return; }
        var j = r.json;
        if (typeof j.token !== "string" || !j.pow || j.pow.algo !== POW_ALGO) {
          self._fail("bad-response", "errBadResponse", null, true); return;
        }
        if (j.mode !== "pow_question" || !j.question) {
          self._fail("bad-response", "errBadResponse", { why: "mode" }, false); return;
        }
        var q = j.question;
        if (q.media !== "image" || q.image_mime !== MIME_PNG || !base64Valid(q.image_base64 || "")
            || (q.image_base64 || "").length > MAX_IMAGE_B64_CHARS
            || !Number.isInteger(q.length) || q.length < 1 || q.length > 32) {
          self._fail("image-unusable", "errImageUnusable", null, true); return;
        }
        var iw = Number(q.image_width) || 0, ih = Number(q.image_height) || 0;
        if (iw < 1 || ih < 1 || iw > 640 || ih > 256) {
          self._fail("image-unusable", "errImageUnusable", null, true); return;
        }
        self.challenge = {
          token: j.token, pow: j.pow, question: q,
          expires: Number(j.expires_in) || 0
        };
        self._renderImage(q, iw, ih);
        self.heading.textContent = tr(o.lang, "instruction", { length: q.length });
        self.note.textContent = tr(o.lang, "charsetNote", { charset: q.charset || "—" });
        self.answer.placeholder = tr(o.lang, "answerPlaceholder", { length: q.length });
        // worker 在**领题成功后**就建立，而不是等用户按下提交：
        // CSP 的 worker-src 不允许 blob: 时，构造 Worker 会抛 SecurityError，
        // 早点探测能让环境不兼容在"还没输入答案"时就报出来，而不是等用户做完题。
        // 每个实例最多一个 worker（this.worker 复用），不存在"并发多个无上限计算 worker"。
        if (!self._ensureWorker()) return;
        self._enable(true);
        self._setState(STATES.READY);
        self._say(tr(o.lang, "ready"), "idle");
        self.autoRetry = 0;
        if (self.challenge.expires > 0) {
          self._armDeadline(Date.now() + self.challenge.expires * 1000, function () { self._expireChallenge(); });
        }
      })
      .catch(function (err) {
        global.clearTimeout(to);
        if (self.destroyed || my !== self.gen) return;
        self.inflight = null;
        self._fail(err && err.name === "AbortError" ? "timeout" : "network", "errNetwork", null, true);
      });
  };

  Widget.prototype._renderImage = function (q, iw, ih) {
    clear(this.imgWrap);
    var img = el("img", {
      src: "data:" + q.image_mime + ";base64," + q.image_base64,
      alt: tr(this.o.lang, "imageAlt", { length: q.length }),
      width: String(iw), height: String(ih), decoding: "async"
    });
    var self = this;
    img.addEventListener("error", function () {
      if (self.destroyed) return;
      clear(self.imgWrap);
      self.imgWrap.textContent = tr(self.o.lang, "errImageUnusable");
    });
    this.imgWrap.appendChild(img);
  };

  Widget.prototype._handleHttpFailure = function (r) {
    var codes = (r.json && r.json["error-codes"]) || [];
    var code = codes.length ? String(codes[0]) : "";
    var self = this;
    var o = this.o;
    if (r.status === 503 && code === "challenge-unavailable") {
      if (this.autoRetry < o.maxAutoRetry) {
        this.autoRetry++;
        this._setState(STATES.LOADING);
        this._say(tr(o.lang, "errUnavailable") + " (" + this.autoRetry + "/" + o.maxAutoRetry + ")", "busy");
        var wait = Math.min(Math.max(Number(r.headers && r.headers.get("Retry-After")) || 2, 1), 30) * 1000;
        global.setTimeout(function () { if (!self.destroyed) self.load(); }, wait);
        return;
      }
      this._fail("challenge-unavailable", "errUnavailable", null, true);
      return;
    }
    if (r.status === 429 || code === "rate-limited") {
      this._fail("rate-limited", "errRateLimited", null, true);
      return;
    }
    if (code === "invalid-challenge") {
      this._fail("invalid-challenge", "errInvalidChallenge", null, true);
      return;
    }
    this._fail(code || ("http-" + r.status), "errInvalidRequest", null, false);
  };

  Widget.prototype._post = function (path, payload, signal) {
    var o = this.o;
    // URL 上带 site_key：JSON 请求会触发 CORS 预检，而预检看不见请求体。
    // 服务端另外规定：URL 与体里的 site_key 同时出现时必须一致。
    var url = o.apiBase + path + "?site_key=" + encodeURIComponent(o.siteKey);
    return global.fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
      credentials: "omit",       // 验证服务不需要业务站的 cookie
      cache: "no-store",
      signal: signal
    }).then(function (res) {
      return res.text().then(function (text) {
        var json = null;
        try { json = JSON.parse(text); } catch (e) { json = null; }
        return { ok: res.ok, status: res.status, json: json, headers: res.headers };
      });
    });
  };

  // --- Worker ---
  //
  // 一个实例**最多一个** worker（this.worker），一次**最多一个**在算的 job（this.pending）。
  // 消息分发只在 worker 创建时挂**一个**监听器：solve/重试都不再加，
  // 否则反复验证会把同一个 worker 挂满处理器。

  Widget.prototype._ensureWorker = function () {
    if (this.worker) return this.worker;
    var o = this.o;
    if (typeof global.Worker !== "function" || typeof global.Blob !== "function"
        || !global.URL || typeof global.URL.createObjectURL !== "function") {
      this._fail("worker-unsupported", "errWorkerUnsupported", null, false);
      return null;
    }
    var blob = null;
    try {
      if (o.workerUrl) {
        this.worker = new global.Worker(o.workerUrl);              // 方案 B：接入方同源部署固定文件
      } else {
        blob = new global.Blob([POW_WORKER_SOURCE], { type: "text/javascript" });
        this.workerBlobUrl = global.URL.createObjectURL(blob);
        this.worker = new global.Worker(this.workerBlobUrl);        // 方案 A：需要 worker-src blob:
      }
    } catch (e) {
      // CSP 的 worker-src 不允许 blob: 时构造 Worker 会抛 SecurityError。
      // 这里明确失败——绝不"那就在主线程算"，也绝不"当作已通过"。
      this.worker = null;
      if (this.workerBlobUrl) { try { global.URL.revokeObjectURL(this.workerBlobUrl); } catch (e2) { /* noop */ } }
      this.workerBlobUrl = "";
      this._fail("worker-blocked", "errWorkerBlocked", null, false);
      return null;
    }
    var self = this;
    this.worker.addEventListener("message", function (ev) { self._dispatch(ev.data); });
    this.worker.addEventListener("error", function () {
      if (!self.destroyed) self._fail("worker-blocked", "errWorkerBlocked", null, false);
    });
    return this.worker;
  };

  Widget.prototype._dispatch = function (m) {
    if (!m || !m.job) return;
    var p = this.pending[m.job];
    if (!p) return;
    // 非终态消息**不**摘登记：started/progress 之后还必须能让同 job 的终态消息找到它。
    // 之前这里无条件 delete，于是 "started" 把 job 摘掉、真正的 "solved" 落地时无处可寻，
    // 组件就卡在 solving 直到超时（浏览器闭环测试抓到的第二个真实缺陷）。
    if (m.type === "started" || m.type === "progress") { p.settle(m); return; }
    delete this.pending[m.job];          // 终态：一次性结算
    p.settle(m);
  };

  Widget.prototype._terminateWorker = function () {
    for (var k in this.pending) {
      if (Object.prototype.hasOwnProperty.call(this.pending, k)) {
        var s = this.pending[k];
        delete this.pending[k];
        try { s.cancel(); } catch (e) { /* noop */ }
      }
    }
    if (this.worker) { try { this.worker.terminate(); } catch (e) { /* 已终止 */ } this.worker = null; }
    if (this.workerBlobUrl) {
      try { global.URL.revokeObjectURL(this.workerBlobUrl); } catch (e) { /* 非浏览器环境 */ }
      this.workerBlobUrl = "";
    }
  };

  // solvePow 返回 Promise：解出 → nonce 字符串；失败/取消 → reject({code, key})。
  // 取消一律归一到 code="cancelled"；调用方另外按世代（this.gen）再丢一次，
  // 于是"被取消的旧计算晚到"不可能覆盖新题或触发成功回调。
  Widget.prototype._solvePow = function (pow, onProgress) {
    var self = this, o = this.o;
    var worker = this._ensureWorker();
    if (!worker) return Promise.reject({ code: "worker-unavailable", key: "errWorkerBlocked" });

    var job = ++this.jobSeq;
    var bits = Number(pow.bits);
    // 搜索预算：期望 2^bits 次，给 16 倍余量（覆盖泊松长尾），且不超过服务端 nonce 上限 1e12。
    // 预算 = 期望次数的 64 倍（泊松尾 e^-64 ⇒ 正常难度下"预算耗尽"几乎不可能发生）；
    // 上限另受 o.powMaxTries 与服务端 nonce 上界 1e12 约束。
    var maxTries = Math.min(o.powMaxTries || 5e7, Math.pow(2, bits + 6), 1e12);
    if (!(maxTries >= 1)) maxTries = 1;

    return new Promise(function (resolve, reject) {
      var timer = 0;
      var lastPaint = 0;
      var stop = function () {
        if (timer) { global.clearTimeout(timer); timer = 0; }
        if (self.inflightJob === job) self.inflightJob = 0;
      };
      var settle = function (m) {
        stop();
        if (m.type === "solved") resolve(m.nonce);
        else if (m.type === "cancelled") reject({ code: "cancelled", key: null });
        else reject({
          code: m.reason === "exhausted" ? "pow-exhausted" : ("pow-" + (m.reason || "failed")),
          key: m.reason === "exhausted" ? "errPowExhausted" : "errPowTimeout"
        });
      };
      var cancel = function () {
        if (!self.pending[job]) return;
        delete self.pending[job];
        stop();
        try { worker.postMessage({ type: "cancel", job: job }); } catch (e) { /* 已终止 */ }
        reject({ code: "cancelled", key: null });
      };
      self.pending[job] = {
        cancel: cancel,
        settle: function (m) {
          // 只有终态消息才结算。worker 会先发 "started"，再发 0..n 条 "progress"：
          // 把这两类当终态处理会把一次正常求解立刻判成失败（本条由浏览器闭环测试抓到）。
          if (m.type === "started") return;
          if (m.type === "progress") {
            var now = Date.now();
            if (now - lastPaint >= 400 && typeof onProgress === "function") {
              lastPaint = now;
              onProgress(m);
            }
            return;
          }
          if (m.type === "started") return;
          settle(m);
        }
      };
      self.inflightJob = job;
      timer = global.setTimeout(function () {
        if (!self.pending[job]) return;
        delete self.pending[job];
        stop();
        try { worker.postMessage({ type: "cancel", job: job }); } catch (e) { /* noop */ }
        reject({ code: "pow-timeout", key: "errPowTimeout" });
      }, o.powTimeoutMs);
      worker.postMessage({
        type: "solve", job: job, algo: pow.algo, seed: pow.seed, bits: bits,
        maxTries: maxTries, chunk: 20000
      });
    });
  };

  Widget.prototype.submit = function () {
    if (this.destroyed) return;
    var self = this, o = this.o;
    if (this.state === STATES.SUBMITTING || this.state === STATES.SOLVING) return;  // 重复点击不二次兑换
    if (this.state === STATES.VERIFIED) return;                                       // 通过后不再触发
    if (this.state !== STATES.READY || !this.challenge) {
      if (this.state === STATES.EXPIRED) this._fail("expired", "errExpiredSend", null, true);
      return;
    }
    var val = String(this.answer.value || "").trim();
    if (!val) {
      this.answer.setAttribute("aria-invalid", "true");
      this._say(tr(o.lang, "errInvalidRequest"), "err");
      return;
    }
    var my = this.gen;
    var challenge = this.challenge;
    this._clearDeadline();
    this._setState(STATES.SOLVING);
    this._enable(false);
    this._say(tr(o.lang, "solving"), "busy");

    this._solvePow(challenge.pow, function (m) {
      if (self.destroyed || my !== self.gen) return;
      self._say(tr(o.lang, "solvingProgress", { tries: m.tries, ms: m.ms }), "busy");
    }).then(function (nonce) {
      if (self.destroyed || my !== self.gen) return;        // 被取消/换代：丢弃这次结果
      self._setState(STATES.SUBMITTING);
      self._say(tr(o.lang, "submitting"), "busy");
      return self._post("/api/redeem", {
        token: challenge.token, pow_nonce: nonce, answer: val
      }).then(function (r) {
        if (self.destroyed || my !== self.gen) return;
        if (!r.ok) { self._handleRedeemFailure(r); return; }
        var token = r.json && r.json.response;
        if (typeof token !== "string" || token.length === 0 || token.length > 4096) {
          self._fail("bad-response", "errBadResponse", null, false); return;
        }
        self._setToken(token);
        self.challenge = null;
        self._setState(STATES.VERIFIED);
        self._say(tr(o.lang, "verified"), "ok");
        self.btnSubmit.disabled = true;
        self.btnRefresh.disabled = false;
        var ttl = Number(r.json.expires_in) || 0;
        if (ttl > 0) {
          self.tokenDeadline = Date.now() + ttl * 1000;
          self._armDeadline(self.tokenDeadline, function () { self._expireToken(); });
        }
        if (!self.successFired) {           // verified 之后重复路径不再触发回调
          self.successFired = true;
          if (o.onSuccess) { try { o.onSuccess(token, self.id); } catch (e) { /* 回调异常隔离 */ } }
        }
      });
    }).catch(function (err) {
      if (self.destroyed || my !== self.gen) return;
      if (err && err.code === "cancelled") return;
      self._fail(err && err.code ? err.code : "pow-failed", err && err.key ? err.key : "errPowTimeout", null, true);
    });
  };

  Widget.prototype._handleRedeemFailure = function (r) {
    var codes = (r.json && r.json["error-codes"]) || [];
    var code = codes.length ? String(codes[0]) : "";
    if (code === "invalid-challenge") {
      // 答错或已被终结：这张题不可再用，且**必须**清空任何已存在的 token。
      this._setToken("");
      this._setState(STATES.ERROR);
      this._say(tr(this.o.lang, "errInvalidChallenge"), "err");
      this.answer.setAttribute("aria-invalid", "true");
      this.btnSubmit.disabled = true;
      this.btnRefresh.disabled = false;
      this.answer.disabled = false;
      if (this.o.onError) {
        try { this.o.onError({ code: "invalid-challenge", message: tr(this.o.lang, "errInvalidChallenge"), retryable: true, widgetId: this.id }); }
        catch (e) { /* 隔离 */ }
      }
      this.challenge = null;
      return;
    }
    if (r.status === 429 || code === "rate-limited") { this._fail("rate-limited", "errRateLimited", null, true); return; }
    if (r.status === 503) { this._fail("challenge-unavailable", "errUnavailable", null, true); return; }
    this._fail(code || ("http-" + r.status), "errInvalidRequest", null, false);
  };

  Widget.prototype.refresh = function () {
    if (this.destroyed) return;
    this._terminateWorker();
    this._setToken("");
    this.successFired = false;
    this.autoRetry = 0;
    this.load();
  };

  Widget.prototype.reset = function () {
    if (this.destroyed) return;
    if (this.inflight) { try { this.inflight.abort(); } catch (e) { /* noop */ } this.inflight = null; }
    this._terminateWorker();
    this._clearDeadline();
    this.deadlineEpoch = 0;
    this.challenge = null;
    this._setToken("");
    this.successFired = false;
    this.answer.value = "";
    this.answer.setAttribute("aria-invalid", "false");
    this._enable(true);
    this._setState(STATES.IDLE);
    this._say(tr(this.o.lang, "ready"), "idle");
  };

  Widget.prototype.destroy = function () {
    if (this.destroyed) return;
    this.destroyed = true;
    if (this.inflight) { try { this.inflight.abort(); } catch (e) { /* noop */ } this.inflight = null; }
    this._terminateWorker();
    this._clearDeadline();
    for (var i = 0; i < this.listeners.length; i++) {
      var l = this.listeners[i];
      try { l[0].removeEventListener(l[1], l[2]); } catch (e) { /* 已分离 */ }
    }
    this.listeners = [];
    registry.delete(this.id);
    maybeUnbindVisibility();
    if (this.field && this.field.parentNode) this.field.parentNode.removeChild(this.field);
    if (this.host && this.host.parentNode) this.host.parentNode.removeChild(this.host);
    this.token = "";
    this.challenge = null;
    this._setState(STATES.DESTROYED);
  };

  var api = {
    version: WIDGET_VERSION,
    states: STATES,
    render: function (container, options) {
      var node = typeof container === "string" ? document.querySelector(container) : container;
      if (!node || !node.appendChild) throw new TypeError("EpoCHA.render: container 不是元素");
      var o = normalizeOptions(options);
      var id = "epocha-" + (++seq);
      var w = new Widget(id, node, o);
      registry.set(id, w);
      bindVisibility();
      w.load();
      return id;
    },
    getResponse: function (id) {
      var w = registry.get(id);
      return w && w.state === STATES.VERIFIED ? w.token : "";
    },
    reset: function (id) { var w = registry.get(id); if (w) w.reset(); },
    refresh: function (id) { var w = registry.get(id); if (w) w.refresh(); },
    destroy: function (id) { var w = registry.get(id); if (w) w.destroy(); },
    state: function (id) { var w = registry.get(id); return w ? w.state : null; },
    ids: function () { return Array.from(registry.keys()); },
    // 给集成方用的纯函数（也便于单测）：位计数与 base64url 解码，与 worker/Go 同一语义。
    util: { leadingZeroBits: leadingZeroBits, b64urlToBytes: b64urlToBytes, lang: pickLang }
  };

  global.EpoCHA = api;
})(typeof window !== "undefined" ? window : globalThis);
