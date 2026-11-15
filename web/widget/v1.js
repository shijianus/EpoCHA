/*! epocha widget v1 — 零依赖原生 JS 人机验证组件（DESIGN §11、D10）
 * 用法与选项见 docs/WIDGET.md。安全边界：Shadow DOM 只是样式与命名隔离，**不是**安全边界；
 * 浏览器里出现的唯一凭据是 site_key，服务端到服务端的验证凭据永远只在业务后端。
 */
(function (global) {
  "use strict";

  var WIDGET_VERSION = "v1";
  var MIME_PNG = "image/png";
  var MAX_IMAGE_B64_CHARS = 87384;
  var POW_ALGO = "sha256-prefix";
  // 交互模块必须从核心脚本所在位置加载：document.currentScript 只在脚本执行期有效，
  // 等领题响应回来时它已经是 null。模块与核心同源；页面既然允许核心脚本，也允许它。
  var SCRIPT_SRC = (typeof document !== "undefined" && document.currentScript)
    ? String(document.currentScript.src || "") : "";


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
  var BUSY = { loading: 1, solving: 1, submitting: 1 };

  var I18N = {
    "zh-CN": {
      instruction: "请输入图片中显示的 {length} 个字符",
      charsetNote: "字符集：{charset}（不含易混字符 I O 0 1）；大小写不敏感。每题只有一次作答机会。",
      imageAlt: "人机验证图片：请输入图中显示的 {length} 个字符",
      submit: "提交答案",
      refresh: "换一张",
      retry: "重试",
      solving: "正在本地计算 PoW，完成后会将答案提交给验证服务…",
      solvingProgress: "正在本地计算 PoW：已尝试 {tries} 次（{ms} ms）",
      submitting: "计算完成，正在提交答案…",
      ready: "请看清图片后输入字符，然后提交。",
      verified: "验证通过。",
      expired: "这张题已过期，token 已作废。请换一张重新验证。",
      reset: "已重置。请换一张重新验证。",
      destroyed: "组件已销毁。",
      errNetwork: "无法连接验证服务，请检查网络后重试。",
      errRateLimited: "请求过于频繁，请稍后重试。",
      errUnavailable: "验证服务暂时给不出题目，请稍后换一张重试。",
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
      experimental: "实验性视觉题型：已有固定工具的 OCR 基线，但不代表抗多模态/抗机器能力；人工可读性尚未验收。",
      answerPlaceholder: "{length} 个字符",
      noteCells: "点选所有符合描述的格子；再次点击可取消。每题一次作答机会。",
      notePairs: "两个数字分别对应下方两个标签；每题一次作答机会。",
      noteAngle: "拖动滑块调整角度；滑块值直接作为本题答案。",
      notePoint: "点击图片设置目标位置；方向键可微调，Shift+方向键步长更小。",
      noteTiming: "观察动画，在目标时机点击“记录时机”；每题一次作答机会。",
      noteAccessible: "这是一个纯文本无障碍题；选择一项后提交，不加载图片。",
      btnReset: "重新开始本题",
      btnUndo: "撤销",
      btnRotate: "旋转",
      errInteractiveUnavailable: "交互题型模块加载失败（CSP 或网络）。请换一题。",
      errIncompleteOption: "请选择一个选项。",
      cellLabel: "第 {n} 格",
      selectedCount: "已选 {n} 格",
      cellSelected: "已选中第 {n} 格",
      cellUnselected: "已取消第 {n} 格",
      errIncompleteSelection: "请至少点选一个格子。",
      errIncompletePairs: "请填写两个数字。",
      errIncompleteAngle: "请先调整角度。",
      errIncompletePoint: "请先点击图片中的目标位置。",
      errIncompleteTiming: "请先记录时机。",
      errMotionRequired: "此设备启用了减少动态，无法完成动画题；请换一题。",
      tolerantNote: "本题最多允许 1 个字符认错；请仍然尽量按看到的填写。",
      imageAltCells: "验证图片：按提示点选格子",
      imageAltPairs: "验证图片：数出两类图形的数量",
      imageAltAngle: "验证图片：调整角度",
      imageAltPoint: "验证图片：点击目标位置",
      imageAltTiming: "验证图片：观察动画并记录时机",
      pointLabel: "点击图片选择位置",
      pointSelected: "已选择位置 {x},{y}",
      timingRecord: "记录时机",
      timingIdle: "等待你记录时机",
      timingRecorded: "已记录 {ms} ms",
      framePause: "暂停",
      framePlay: "继续",
      frameReplay: "重播",
      promptFallback: "按图片完成验证。"
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
      reset: "Reset. Get a new image to verify again.",
      destroyed: "Widget destroyed.",
      errNetwork: "Cannot reach the verification service. Check the network and retry.",
      errRateLimited: "Too many requests. Please wait and retry.",
      errUnavailable: "The verification service has no question available right now. Try a new image shortly.",
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
      experimental: "Experimental visual question type: a fixed-tool OCR baseline exists, but this "
        + "does not claim resistance to multimodal or automated solving; human readability is not yet accepted.",
      answerPlaceholder: "{length} characters",
      noteCells: "Click every cell that matches the prompt; click again to undo. One attempt per question.",
      notePairs: "Enter the two counts for the labels below. One attempt per question.",
      noteAngle: "Drag the slider to set the angle; the slider value is the answer.",
      notePoint: "Click the image to set the target; arrow keys nudge, Shift+arrow is finer.",
      noteTiming: "Watch the animation and click “Record timing” at the target moment. One attempt per question.",
      noteAccessible: "This is a text-only accessible question. Choose one option and submit; no image is loaded.",
      btnReset: "Restart this question",
      btnUndo: "Undo",
      btnRotate: "Rotate",
      errInteractiveUnavailable: "The interactive question module could not be loaded (CSP or network). Get another question.",
      errIncompleteOption: "Choose one option.",
      cellLabel: "Cell {n}",
      selectedCount: "{n} selected",
      cellSelected: "Selected cell {n}",
      cellUnselected: "Unselected cell {n}",
      errIncompleteSelection: "Select at least one cell.",
      errIncompletePairs: "Enter both numbers.",
      errIncompleteAngle: "Adjust the angle first.",
      errIncompletePoint: "Click the target position first.",
      errIncompleteTiming: "Record the timing first.",
      errMotionRequired: "Reduced motion is enabled, so this animated question cannot run. Get another question.",
      tolerantNote: "This question tolerates one misread character; still enter what you see.",
      imageAltCells: "Verification image: select cells as described",
      imageAltPairs: "Verification image: count the two kinds of shapes",
      imageAltAngle: "Verification image: adjust the angle",
      imageAltPoint: "Verification image: click the target position",
      imageAltTiming: "Verification image: watch the animation and record timing",
      pointLabel: "Click the image to choose a position",
      pointSelected: "Position {x},{y} selected",
      timingRecord: "Record timing",
      timingIdle: "Waiting for your timing",
      timingRecorded: "Recorded {ms} ms",
      framePause: "Pause",
      framePlay: "Resume",
      frameReplay: "Replay",
      promptFallback: "Complete the check shown in the image."
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

  function intField(obj, key) {
    var v = obj[key];
    return (typeof v === "number" && isFinite(v) && Math.floor(v) === v) ? v : null;
  }

  function validGrid(q, iw, ih) {
    var cols = intField(q, "grid_cols"), rows = intField(q, "grid_rows");
    var x = intField(q, "grid_x"), y = intField(q, "grid_y");
    var cell = intField(q, "grid_cell"), gap = intField(q, "grid_gap");
    if (cols === null || rows === null || x === null || y === null || cell === null || gap === null) return false;
    if (cols < 2 || cols > 6 || rows < 2 || rows > 6 || cols * rows > 36) return false;
    if (cell < 20 || cell > 160 || gap < 0 || gap > 32 || x < 0 || y < 0) return false;
    if (x + cols * cell + (cols - 1) * gap > iw) return false;
    if (y + rows * cell + (rows - 1) * gap > ih) return false;
    return typeof q.prompt_zh === "string" && typeof q.prompt_en === "string";
  }

  function validPairs(q) {
    var lz = q.labels_zh, le = q.labels_en;
    if (!Array.isArray(lz) || !Array.isArray(le) || lz.length !== 2 || le.length !== 2) return false;
    for (var i = 0; i < 2; i++) {
      if (typeof lz[i] !== "string" || !lz[i] || lz[i].length > 64) return false;
      if (typeof le[i] !== "string" || !le[i] || le[i].length > 64) return false;
    }
    var min = intField(q, "min"), max = intField(q, "max");
    if (min === null || max === null || min < 0 || max > 99 || min >= max) return false;
    return typeof q.prompt_zh === "string" && typeof q.prompt_en === "string";
  }

  function validPrompts(q) {
    return typeof q.prompt_zh === "string" && q.prompt_zh.length > 0 && q.prompt_zh.length <= 200
      && typeof q.prompt_en === "string" && q.prompt_en.length > 0 && q.prompt_en.length <= 200;
  }

  function validAngle(q) {
    var min = intField(q, "angle_min"), max = intField(q, "angle_max"), step = intField(q, "angle_step");
    if (min === null || max === null || step === null) return false;
    if (min < 0 || max > 3599 || min >= max) return false;
    if (step < 1 || step > 90 || (max - min) < step) return false;
    return validPrompts(q);
  }

  function validPoint(q) {
    return validPrompts(q);
  }

  function validAim(q, iw, ih) {
    var cx = intField(q, "cue_x"), cy = intField(q, "cue_y"), len = intField(q, "aim_len");
    if (cx === null || cy === null || len === null) return false;
    if (cx < 0 || cx > iw || cy < 0 || cy > ih) return false;
    return len >= 40 && len <= 300;
  }

  function validFrames(q, iw, ih) {
    if (q.render_mode !== "sprite_sheet") return false;
    var count = intField(q, "frame_count"), cols = intField(q, "frame_cols");
    var rows = intField(q, "frame_rows"), ms = intField(q, "frame_ms");
    if (count === null || cols === null || rows === null || ms === null) return false;
    if (count < 2 || count > 64 || cols < 1 || cols > 8 || rows < 1 || rows > 8) return false;
    if (cols * rows < count) return false;
    if (ms < 20 || ms > 500) return false;
    if (typeof q.frame_loop !== "boolean") return false;
    if (iw * cols > 2048 || ih * rows > 2048) return false;
    return true;
  }

  function validTextOptions(q) {
    if (q.answer_type !== "text" || q.length !== 1 || q.charset !== "ABCDEFGHIJKL") return false;
    if (!Array.isArray(q.options) || q.options.length < 4 || q.options.length > 12) return false;
    if (!validPrompts(q)) return false;
    var seen = {};
    for (var i = 0; i < q.options.length; i++) {
      var o = q.options[i];
      if (!o || typeof o.id !== "string" || !/^[A-L]$/.test(o.id) || seen[o.id]) return false;
      if (typeof o.label_zh !== "string" || !o.label_zh || o.label_zh.length > 32) return false;
      if (typeof o.label_en !== "string" || !o.label_en || o.label_en.length > 32) return false;
      seen[o.id] = true;
    }
    return true;
  }

  function sessionID() {
    if (global.crypto && typeof global.crypto.getRandomValues === "function") {
      var b = new Uint8Array(16);
      global.crypto.getRandomValues(b);
      var s = "";
      for (var i = 0; i < b.length; i++) s += (b[i] < 16 ? "0" : "") + b[i].toString(16);
      return s;
    }
    return "s" + Date.now().toString(36) + Math.random().toString(36).slice(2);
  }

  function deviceHints() {
    var coarse = !!(global.matchMedia && global.matchMedia("(pointer: coarse)").matches);
    var reduced = !!(global.matchMedia && global.matchMedia("(prefers-reduced-motion: reduce)").matches);
    return { pointer: coarse ? "coarse" : "fine", reduced_motion: reduced };
  }

  function promptText(q, lang) {
    var zh = typeof q.prompt_zh === "string" ? q.prompt_zh : "";
    var en = typeof q.prompt_en === "string" ? q.prompt_en : "";
    var s = lang === "zh-CN" ? (zh || en) : (en || zh);
    return s || tr(lang, "promptFallback");
  }

  function b64urlToBytes(s) {
    var bin = global.atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice(0, (4 - (s.length % 4)) % 4));
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i) & 0xff;
    return out;
  }

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
    ":host{all:unset;display:block;width:100%;max-width:480px;font:14px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;color:#182534}",
    ".box{border:1px solid #cbd5e1;border-radius:14px;padding:14px;background:linear-gradient(180deg,#fff 0%,#f8fbff 100%);box-shadow:0 8px 24px rgba(26,55,88,.10)}",
    ".img{display:flex;justify-content:center;align-items:center;min-height:112px;padding:8px;box-sizing:border-box;background:#f7fafc;border:1px solid #cbd5e1;border-radius:12px;overflow:hidden}",
    ".img img{display:block;max-width:100%;height:auto;border-radius:8px}",
    ".lbl{display:block;font-weight:650;margin:12px 0 6px;color:#22364d}",
    "input.ans{width:100%;min-height:44px;box-sizing:border-box;font:inherit;letter-spacing:.3em;text-transform:uppercase;padding:8px 10px;border:1px solid #9fb0c3;border-radius:8px;background:#fff}",
    "input.ans[aria-invalid=true]{border-color:#a31414;border-width:2px}",
    "button{font:inherit;min-height:44px;padding:8px 12px;margin:8px 6px 0 0;border-radius:8px;border:1px solid #8ea3ba;background:#f5f8fc;color:#1b334d;cursor:pointer;transition:background .12s ease,border-color .12s ease,transform .12s ease,box-shadow .12s ease}",
    "button:hover:not([disabled]){background:#e8f1fb;border-color:#5d83aa;box-shadow:0 2px 8px rgba(30,75,120,.12)}",
    "button:active:not([disabled]){transform:translateY(1px);box-shadow:none}",
    "button:focus-visible,input:focus-visible{outline:3px solid #2168b4;outline-offset:2px}",
    "button[disabled]{opacity:.5;cursor:default}",
    ".st{margin:8px 0 0;padding:6px 8px;border:1px solid #c9c9c9;border-radius:4px;min-height:1.2em}",
    ".st[data-kind=ok]{border-color:#1c7d33;background:rgba(28,125,51,.08)}",
    ".st[data-kind=err]{border-color:#a31414;background:rgba(163,20,20,.07)}",
    ".st[data-kind=busy]{border-color:#1a5fb4;background:rgba(26,95,180,.07)}",
    ".note{margin:8px 0 0;font-size:12px;color:#555}",
    ".tag{display:inline-block;margin:8px 0 0;font-size:12px;border:1px solid #b0761a;border-radius:4px;padding:2px 6px;color:#7a5312}",
    ".hidden{display:none!important}",
    ".qimg{position:relative;display:block;width:100%;max-width:100%;line-height:0;border-radius:10px;overflow:hidden}",
    ".qimg img{display:block;width:100%;max-width:100%;height:auto;border-radius:10px}",
    ".cells{position:absolute;inset:0;z-index:2}",
    ".cell{position:absolute;box-sizing:border-box;margin:0;padding:0;min-width:0;border:2px solid transparent;background:transparent;border-radius:8px;cursor:pointer;touch-action:manipulation}",
    ".cells[data-hit-mode=grid] .cell,.cells[data-hit-mode=rotate] .cell,.cells[data-hit-mode=placements] .cell{min-width:44px;min-height:44px}",
    ".cell:hover:not([disabled]){background:rgba(33,104,180,.12);border-color:rgba(33,104,180,.55)}",
    ".cell[aria-pressed=true]{background:rgba(33,104,180,.26);border-color:#2168b4;box-shadow:inset 0 0 0 2px rgba(255,255,255,.8)}",
    ".cell:focus-visible{outline:2px solid #1a5fb4;outline-offset:0}",
    ".cellsum{margin:6px 0 0;font-size:12px;color:#555}",
    ".ordersum{margin:6px 0 0;font-size:12px;color:#555}",
    ".tileimg{position:absolute;inset:0;background-repeat:no-repeat;pointer-events:none;transition:transform 140ms ease;transform-origin:50% 50%}",
    ".pieceimg{position:absolute;inset:0;background-repeat:no-repeat;pointer-events:none;transition:transform 120ms ease;transform-origin:50% 50%}",
    ".pathlayer{position:absolute;inset:0;z-index:1;pointer-events:none}",
    ".pathline{fill:none;stroke:#1a5fb4;stroke-width:5;stroke-linecap:round;stroke-linejoin:round}",
    ".pathnum{position:absolute;right:2px;bottom:1px;font-size:11px;font-weight:700;color:#123f78;background:#fff;border:1px solid #1a5fb4;border-radius:8px;min-width:14px;height:14px;line-height:14px;text-align:center;pointer-events:none}",
    ".rankno{position:absolute;left:2px;top:2px;font-size:12px;font-weight:700;color:#fff;background:#1a5fb4;border-radius:9px;min-width:18px;height:18px;line-height:18px;text-align:center;pointer-events:none}",
    ".roundcover{position:absolute;left:0;width:100%;z-index:1;background:#e9e9e9;border-top:1px solid #bbb;border-bottom:1px solid #bbb;pointer-events:none}",
    ".frameview{position:relative;background-repeat:no-repeat;max-width:100%;line-height:0}",
    ".cell[data-sel=true]{border-color:#1a5fb4;background:rgba(26,95,180,.22);box-shadow:inset 0 0 0 2px #1a5fb4}",
    ".cell[data-path=true]{border-color:#1a5fb4;background:rgba(26,95,180,.14)}",
    ".cell[data-used=true]{opacity:.45}",
    ".cell[data-busy=true]{opacity:.65}",
    ".aux{margin:6px 0 0}",
    ".aux button{margin:0 6px 0 0;padding:3px 8px}",
    ".pairs{margin:8px 0 0}",
    ".pairrow{display:flex;gap:8px;align-items:center;margin:4px 0}",
    ".pairrow label{flex:0 0 auto;max-width:55%;font-size:13px}",
    ".pairrow input{width:90px;min-height:44px;box-sizing:border-box;font:inherit;padding:8px;border:1px solid #9fb0c3;border-radius:8px}",
    ".angle{margin:8px 0 0}",
    ".angle input{width:100%;box-sizing:border-box}",
    ".angleout{display:inline-block;margin-left:8px;font-variant-numeric:tabular-nums}",
    ".pointlayer{position:absolute;inset:0;z-index:2;cursor:crosshair;outline:none}",
    ".pointlayer:focus-visible{outline:2px solid #1a5fb4;outline-offset:0}",
    ".pointmark{position:absolute;width:14px;height:14px;margin:-7px 0 0 -7px;border:2px solid #1a5fb4;border-radius:50%;background:rgba(26,95,180,.20);pointer-events:none}",
    ".aimlayer{position:absolute;inset:0;z-index:1;pointer-events:none}",
    ".aimline{stroke:#f2c94c;stroke-width:3;stroke-dasharray:8 6}",
    ".angledrag{position:absolute;inset:0;z-index:3;cursor:crosshair;touch-action:none}",
    ".timing{margin:8px 0 0}",
    ".timingout{margin-left:8px;font-variant-numeric:tabular-nums}",
    ".options{margin:10px 0 0;display:grid;gap:8px}",
    ".opt{display:block;width:100%;min-height:44px;text-align:left;margin:0;padding:9px 12px;border:1px solid #8ea3ba;border-radius:8px;background:#f7fafd;cursor:pointer;font:inherit}",
    ".opt[aria-pressed=true]{border-color:#2168b4;background:rgba(33,104,180,.14);box-shadow:inset 0 0 0 1px #2168b4}",
    ".frameplay{margin:6px 0 0;font-size:12px}",
    ".frameplay button{margin:0 6px 0 0;padding:3px 8px}",
    ".frameview{display:block;max-width:100%;background-repeat:no-repeat;border-radius:10px;overflow:hidden}"
  ];
  var STYLE = STYLE_RULES.join("");

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
      maxAutoRetry: (function () {
        if (o.maxAutoRetry === undefined) return 2;
        var n = Number(o.maxAutoRetry);
        if (!Number.isFinite(n) || n < 0) return 2;
        return Math.min(Math.floor(n), 5);
      })(),
      showExperimentalNote: o.showExperimentalNote !== false,
      accessibility: o.accessibility === true,
      form: o.form === undefined || o.form === null || o.form === "" ? null : o.form,
      ariaLabel: o.ariaLabel || ""
    };
    if (opt.workerUrl) {
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

  function resolveForm(spec, container) {
    var doc = container.ownerDocument;
    if (!doc) return null;
    if (spec && typeof spec === "object" && spec.nodeType === 1) {
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
    this.inflight = null;    // 当前 HTTP 请求的取消句柄（同时结算 Promise、清理超时）
    this.retryTimer = 0;
    this.timer = 0;
    this.autoRetry = 0;
    this.destroyed = false;
    this.successFired = false;
    this.listeners = [];
    this.frameAnim = null;
    this.frameView = null;
    this.frameStartedAt = 0;
    this.frameCycleMs = 0;
    this.frameAnimated = false;
    this.session = sessionID();
    this.tel = { start: 0, first: 0, events: 0, pointer: 0, key: 0 };
    this._build();
  }

  Widget._interactive = false;
  var interactiveLoad = null;

  function interactiveScriptURL(apiBase) {
    if (SCRIPT_SRC) {
      try { return new URL("v1.interactive.js", SCRIPT_SRC).href; } catch (e) { /* 回落到 apiBase */ }
    }
    return String(apiBase || "").replace(/\/+$/, "") + "/widget/v1.interactive.js";
  }

  Widget.prototype._cancelInteractive = function () {
    if (this.cancelInteractive) this.cancelInteractive();
  };

  Widget.prototype._ensureInteractive = function () {
    if (Widget._interactive) return Promise.resolve();
    var attempt = interactiveLoad, self = this;
    if (!attempt) {
      attempt = { waiters: [], script: null, done: false };
      interactiveLoad = attempt;
      attempt.finish = function (error) {
        if (attempt.done) return;
        attempt.done = true;
        if (interactiveLoad === attempt) interactiveLoad = null;
        var script = attempt.script;
        if (script) {
          script.onload = script.onerror = null;
          if (script.parentNode) script.parentNode.removeChild(script);
        }
        attempt.waiters.slice().forEach(function (finish) { finish(error); });
      };
    }
    return new Promise(function (resolve, reject) {
      var timer = 0, settled = false;
      function finish(error) {
        if (settled) return;
        settled = true;
        if (timer) global.clearTimeout(timer);
        var i = attempt.waiters.indexOf(finish);
        if (i >= 0) attempt.waiters.splice(i, 1);
        if (self.cancelInteractive === cancel) self.cancelInteractive = null;
        if (!attempt.waiters.length && !attempt.done) attempt.finish(new Error("cancelled"));
        if (error) reject(error); else resolve();
      }
      function cancel() { finish(new Error("cancelled")); }
      self.cancelInteractive = cancel;
      attempt.waiters.push(finish);
      // Reuse the bounded network budget; the instance's challenge timer may expire sooner.
      timer = global.setTimeout(function () { finish(new Error("interactive-unavailable")); }, self.o.fetchTimeoutMs);
      if (!attempt.script) {
        var doc = global.document;
        if (!doc || typeof doc.createElement !== "function" || !(doc.head || doc.documentElement)) {
          attempt.finish(new Error("interactive-unavailable")); return;
        }
        var script = doc.createElement("script");
        attempt.script = script;
        script.src = interactiveScriptURL(self.o.apiBase);
        script.async = true;
        script.onload = function () {
          attempt.finish(Widget._interactive ? null : new Error("interactive-unavailable"));
        };
        script.onerror = function () { attempt.finish(new Error("interactive-unavailable")); };
        (doc.head || doc.documentElement).appendChild(script);
      }
    });
  };

  Widget._ = function () {};

  Widget.prototype._build = function () {
    var o = this.o;
    this.field = null;
    if (o.fieldName) {
      var implicit = typeof this.container.closest === "function" ? this.container.closest("form") : null;
      var want = o.form ? resolveForm(o.form, this.container) : implicit;
      if (o.form && !want) {
        throw new TypeError("EpoCHA.render: form 选项没有指向当前文档里的真实 <form>");
      }
      if (!want) {
        throw new TypeError("EpoCHA.render: fieldName 要求把字段提交出去，但容器不在任何 <form> 内。"
          + "把容器放进目标 form、用 form 选项显式关联，或设 fieldName:'' 只走 onSuccess/getResponse。");
      }
      this.field = el("input", { type: "hidden", name: o.fieldName });
      this.field.setAttribute("data-epocha-field", this.id);
      (want.contains(this.container) ? this.container : want).appendChild(this.field);
    }

    this.host = el("div", { "data-epocha-widget": this.id, lang: o.lang });
    this.container.appendChild(this.host);
    this.root = this.host.attachShadow({ mode: "open" });
    applyStyle(this.root);

    var box = el("div", { class: "box", role: "group" });
    var headingId = this.id + "-lbl";
    var statusId = this.id + "-st";
    var noteId = this.id + "-note";
    this.heading = el("label", {
      class: "lbl", id: headingId, "for": this.id + "-ans"
    }, tr(o.lang, "instruction", { length: 6 }));
    this.imgWrap = el("div", { class: "img" });
    this.imgBox = el("div", { class: "qimg" });
    this.cellWrap = el("div", { class: "cells hidden", role: "group" });
    this.imgBox.appendChild(this.cellWrap);
    this.pathLayer = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    this.pathLayer.setAttribute("class", "pathlayer hidden");
    this.pathLayer.setAttribute("preserveAspectRatio", "none");
    this.pathLine = document.createElementNS("http://www.w3.org/2000/svg", "polyline");
    this.pathLine.setAttribute("class", "pathline");
    this.pathLine.setAttribute("points", "");
    this.pathLayer.appendChild(this.pathLine);
    this.imgBox.appendChild(this.pathLayer);
    this.pointLayer = el("div", {
      class: "pointlayer hidden", role: "button", tabindex: "-1", id: this.id + "-point",
      "aria-label": tr(o.lang, "pointLabel"), "aria-describedby": noteId + " " + statusId
    });
    this.pointMark = el("span", { class: "pointmark hidden" });
    this.pointLayer.appendChild(this.pointMark);
    this.imgBox.appendChild(this.pointLayer);
    var svgNS = "http://www.w3.org/2000/svg";
    this.aimLayer = document.createElementNS(svgNS, "svg");
    this.aimLayer.setAttribute("class", "aimlayer hidden");
    this.aimLayer.setAttribute("preserveAspectRatio", "none");
    this.aimLine = document.createElementNS(svgNS, "line");
    this.aimLine.setAttribute("class", "aimline");
    this.aimLine.setAttribute("x1", "0");
    this.aimLine.setAttribute("y1", "0");
    this.aimLine.setAttribute("x2", "0");
    this.aimLine.setAttribute("y2", "0");
    this.aimLayer.appendChild(this.aimLine);
    this.imgBox.appendChild(this.aimLayer);
    this.angleDragLayer = el("div", { class: "angledrag hidden" });
    this.imgBox.appendChild(this.angleDragLayer);
    this.imgWrap.appendChild(this.imgBox);
    this.imgMsg = el("p", { class: "imgmsg" }, "…");
    this.imgWrap.appendChild(this.imgMsg);
    this.imgEl = null;
    this.answer = el("input", {
      class: "ans", type: "text", id: this.id + "-ans", autocomplete: "off",
      autocapitalize: "characters", spellcheck: "false", maxlength: "8",
      "aria-required": "true", "aria-invalid": "false", "aria-labelledby": headingId,
      "aria-describedby": noteId + " " + statusId
    });
    this.answer.placeholder = tr(o.lang, "answerPlaceholder", { length: 6 });
    this.pairWrap = el("div", { class: "pairs hidden" });
    this.pairInputs = [];
    this.pairLabels = [];
    for (var pi = 0; pi < 2; pi++) {
      var pid = this.id + "-p" + pi;
      var pairLabel = el("label", { for: pid }, "");
      var pairInput = el("input", {
        type: "number", inputmode: "numeric", id: pid, min: "0", max: "99",
        autocomplete: "off", "aria-required": "true",
        "aria-describedby": noteId + " " + statusId
      });
      var pairRow = el("div", { class: "pairrow" });
      pairRow.appendChild(pairLabel);
      pairRow.appendChild(pairInput);
      this.pairWrap.appendChild(pairRow);
      this.pairLabels.push(pairLabel);
      this.pairInputs.push(pairInput);
    }
    this.cellStatus = el("p", { class: "cellsum hidden", "aria-live": "polite" });
    this.orderBar = el("p", { class: "ordersum hidden", "aria-live": "polite" });
    this.auxWrap = el("div", { class: "aux hidden" });
    this.btnUndo = el("button", { type: "button" }, tr(o.lang, "btnUndo"));
    this.btnReset = el("button", { type: "button" }, tr(o.lang, "btnReset"));
    this.btnRotate = el("button", { type: "button" }, tr(o.lang, "btnRotate"));
    this.auxWrap.appendChild(this.btnUndo);
    this.auxWrap.appendChild(this.btnReset);
    this.auxWrap.appendChild(this.btnRotate);
    this.angleWrap = el("div", { class: "angle hidden" });
    this.angleInput = el("input", {
      type: "range", id: this.id + "-angle", min: "0", max: "3599", step: "1", value: "0",
      "aria-describedby": noteId + " " + statusId
    });
    this.angleOut = el("output", { class: "angleout", "for": this.id + "-angle" }, "0.0°");
    this.angleWrap.appendChild(this.angleInput);
    this.angleWrap.appendChild(this.angleOut);
    this.timingWrap = el("div", { class: "timing hidden" });
    this.timingBtn = el("button", { type: "button", id: this.id + "-timing" }, tr(o.lang, "timingRecord"));
    this.timingOut = el("span", { class: "timingout", "aria-live": "polite" }, tr(o.lang, "timingIdle"));
    this.timingWrap.appendChild(this.timingBtn);
    this.timingWrap.appendChild(this.timingOut);
    this.frameWrap = el("div", { class: "frameplay hidden" });
    this.frameToggle = el("button", { type: "button" }, tr(o.lang, "framePause"));
    this.frameReplay = el("button", { type: "button" }, tr(o.lang, "frameReplay"));
    this.frameWrap.appendChild(this.frameToggle);
    this.frameWrap.appendChild(this.frameReplay);
    this.optionsWrap = el("div", { class: "options hidden", role: "group" });
    this.optionButtons = [];
    this.angleCueX = null;
    this.angleCueY = null;
    this.angleAimLen = 0;
    this.angleImageW = 0;
    this.angleImageH = 0;
    this.angleDragging = false;
    this.anglePointer = null;
    this.framePlaying = false;
    this.answerMode = "text";
    this.selection = {};
    this.cellButtons = [];
    this.tileMasks = [];
    this.tileTurns = [];
    this.tileBusyUntil = [];
    this.pathCells = [];
    this.pathTargetLen = 0;
    this.rankOrder = [];
    this.selectIndex = -1;
    this.gridCols = 0;
    this.pathDragging = false;
    this.progressiveRound = 0;
    this.progressiveSelections = [];
    this.roundCovers = [];
    this.shellRound = 0;
    this.shellSelections = [];
    this.shellAnimating = false;
    this.shellTimer = 0;
    this.shellFrame = 0;
    this.placeState = [];
    this.placeSelected = "";
    this.placeRotation = 0;
    this.placeHistory = [];
    this.pieceRects = {};
    this.btnSubmit = el("button", { type: "button" }, tr(o.lang, "submit"));
    this.btnRefresh = el("button", { type: "button" }, tr(o.lang, "refresh"));
    this.status = el("p", { class: "st", id: statusId, role: "status", "aria-live": "polite" }, "…");
    this.note = el("p", { class: "note", id: noteId }, tr(o.lang, "charsetNote", { charset: "—" }));
    box.setAttribute("aria-labelledby", headingId);

    box.appendChild(this.heading);
    box.appendChild(this.imgWrap);
    box.appendChild(this.optionsWrap);
    box.appendChild(this.frameWrap);
    box.appendChild(this.answer);
    box.appendChild(this.angleWrap);
    box.appendChild(this.pairWrap);
    box.appendChild(this.timingWrap);
    box.appendChild(this.cellStatus);
    box.appendChild(this.orderBar);
    box.appendChild(this.auxWrap);
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
    for (var pj = 0; pj < this.pairInputs.length; pj++) {
      this._on(this.pairInputs[pj], "input", function () { self._syncPairs(); });
      this._on(this.pairInputs[pj], "change", function () { self._syncPairs(); });
    }
    this._on(this.angleInput, "input", function () { self._syncAngle(); });
    this._on(this.angleInput, "change", function () { self._syncAngle(); });
    this._on(this.pointLayer, "click", function (e) { self._pointFromEvent(e); });
    this._on(this.pointLayer, "keydown", function (e) { self._pointKey(e); });
    this._on(this.timingBtn, "click", function (e) { e.preventDefault(); self._recordTiming(); });
    this._on(this.angleDragLayer, "pointerdown", function (e) { self._anglePointerDown(e); });
    this._on(this.angleDragLayer, "pointermove", function (e) { self._anglePointerMove(e); });
    this._on(this.angleDragLayer, "pointerup", function (e) { self._anglePointerUp(e); });
    this._on(this.angleDragLayer, "pointercancel", function (e) { self._anglePointerUp(e); });
    this._on(this.frameToggle, "click", function (e) { e.preventDefault(); self._toggleFrames(); });
    this._on(this.frameReplay, "click", function (e) { e.preventDefault(); self._replayFrames(); });
    this._on(this.btnReset, "click", function (e) { e.preventDefault(); self._resetCurrent(); });
    this._on(this.btnUndo, "click", function (e) { e.preventDefault(); self._undoCurrent(); });
    this._on(this.btnRotate, "click", function (e) { e.preventDefault(); self._placeRotate(); });
    this._on(this.host, "pointerdown", function () { self._telEvent("pointer"); });
    this._on(this.host, "pointermove", function () { self._telEvent("pointer"); });
    this._on(this.host, "keydown", function () { self._telEvent("key"); });
    this._on(this.host, "pointerup", function () { self.pathDragging = false; });
    this._on(this.host, "pointercancel", function () { self.pathDragging = false; });
  };

  Widget.prototype._on = function (node, type, fn) {
    node.addEventListener(type, fn);
    this.listeners.push([node, type, fn]);
  };

  Widget.prototype._off = function (node, type, fn) {
    try { node.removeEventListener(type, fn); } catch (e) { /* 已分离 */ }
    for (var i = 0; i < this.listeners.length; i++) {
      var l = this.listeners[i];
      if (l[0] === node && l[1] === type && l[2] === fn) { this.listeners.splice(i, 1); return; }
    }
  };

  Widget.prototype._clearCells = function () {
    for (var i = 0; i < this.cellButtons.length; i++) {
      var b = this.cellButtons[i];
      if (b._epochaClick) this._off(b, "click", b._epochaClick);
      if (b._epochaKey) this._off(b, "keydown", b._epochaKey);
      if (b._epochaDown) this._off(b, "pointerdown", b._epochaDown);
      if (b._epochaEnter) this._off(b, "pointerenter", b._epochaEnter);
    }
    this.cellButtons = [];
    this.selection = {};
    this.tileMasks = [];
    this.tileTurns = [];
    this.tileBusyUntil = [];
    this.pathCells = [];
    this.pathTargetLen = 0;
    this.rankOrder = [];
    this.selectIndex = -1;
    this.gridCols = 0;
    this.pathDragging = false;
    this.progressiveRound = 0;
    this.progressiveSelections = [];
    this.shellRound = 0;
    this.shellSelections = [];
    this.shellAnimating = false;
    this.shellFrame = 0;
    this.placeState = [];
    this.placeSelected = "";
    this.placeRotation = 0;
    this.placeHistory = [];
    this.pieceRects = {};
    this._stopShell();
    if (this.roundCovers) {
      for (var ci = 0; ci < this.roundCovers.length; ci++) {
        if (this.roundCovers[ci] && this.roundCovers[ci].parentNode) {
          this.roundCovers[ci].parentNode.removeChild(this.roundCovers[ci]);
        }
      }
    }
    this.roundCovers = [];
    if (this.btnRotate) this.btnRotate.setAttribute("class", "hidden");
    clear(this.cellWrap);
    this.cellWrap.setAttribute("class", "cells hidden");
    this.cellStatus.setAttribute("class", "cellsum hidden");
    this.cellStatus.textContent = "";
    if (this.pathLayer) {
      this.pathLayer.setAttribute("class", "pathlayer hidden");
      if (this.pathLine) this.pathLine.setAttribute("points", "");
    }
    if (this.orderBar) {
      this.orderBar.setAttribute("class", "ordersum hidden");
      this.orderBar.textContent = "";
    }
    if (this.auxWrap) this.auxWrap.setAttribute("class", "aux hidden");
  };

  Widget.prototype._resetInputs = function () {
    this._clearCells();
    this._clearPoint();
    this._clearAim();
    this._clearTiming();
    this._clearOptions();
    this._clearFrameAnim();
    this.answer.setAttribute("maxlength", "8");
    this.answerMode = "text";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans");
    this.answer.setAttribute("aria-invalid", "false");
    this.answer.disabled = false;
    this.heading.setAttribute("for", this.answer.getAttribute("id"));
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.optionsWrap.setAttribute("class", "options hidden");
    this.angleInput.value = "0";
    this.angleOut.textContent = "0.0°";
    for (var i = 0; i < this.pairInputs.length; i++) {
      this.pairInputs[i].value = "";
      this.pairInputs[i].disabled = false;
    }
  };

  Widget.prototype._clearPoint = function () {
    if (this.pointLayer) this.pointLayer.setAttribute("class", "pointlayer hidden");
    if (this.pointMark) this.pointMark.setAttribute("class", "pointmark hidden");
  };

  Widget.prototype._clearOptions = function () {
    for (var i = 0; i < this.optionButtons.length; i++) {
      var b = this.optionButtons[i];
      if (b._epochaClick) this._off(b, "click", b._epochaClick);
    }
    this.optionButtons = [];
    if (this.optionsWrap) {
      clear(this.optionsWrap);
      this.optionsWrap.setAttribute("class", "options hidden");
    }
  };

  Widget.prototype._clearAim = function () {
    if (this.aimLayer) this.aimLayer.setAttribute("class", "aimlayer hidden");
    if (this.angleDragLayer) this.angleDragLayer.setAttribute("class", "angledrag hidden");
    this.angleCueX = null;
    this.angleCueY = null;
    this.angleAimLen = 0;
    this.angleDragging = false;
    this.anglePointer = null;
  };

  Widget.prototype._clearTiming = function () {
    if (this.timingWrap) this.timingWrap.setAttribute("class", "timing hidden");
    if (this.timingOut) this.timingOut.textContent = tr(this.o.lang, "timingIdle");
  };

  Widget.prototype._clearFrameAnim = function () {
    if (this.frameAnim) {
      try { this.frameAnim.cancel(); } catch (e) { /* 已结束 */ }
      this.frameAnim = null;
    }
    this.frameStartedAt = 0;
    this.frameCycleMs = 0;
    this.frameAnimated = false;
    this.framePlaying = false;
    if (this.frameToggle) this.frameToggle.textContent = tr(this.o.lang, "framePause");
    this._showFrameControls(false);
  };

  Widget.prototype._showFrameControls = function (show) {
    if (!this.frameWrap) return;
    this.frameWrap.setAttribute("class", show ? "frameplay" : "frameplay hidden");
    this.frameToggle.disabled = !show;
    this.frameReplay.disabled = !show;
  };

  Widget.prototype._clearImage = function () {
    this._clearFrameAnim();
    if (this.frameView && this.frameView.parentNode) this.frameView.parentNode.removeChild(this.frameView);
    this.frameView = null;
    this._clearPoint();
    if (this.imgEl && this.imgEl.parentNode) this.imgEl.parentNode.removeChild(this.imgEl);
    this.imgEl = null;
  };

  Widget.prototype._showImageMessage = function (text) {
    this._clearImage();
    this.imgWrap.setAttribute("class", "img");
    this.imgBox.setAttribute("class", "qimg hidden");
    this.imgMsg.textContent = String(text);
    this.imgMsg.setAttribute("class", "imgmsg");
  };

  Widget.prototype._showImageBox = function () {
    this.imgWrap.setAttribute("class", "img");
    this.imgMsg.setAttribute("class", "imgmsg hidden");
    this.imgBox.setAttribute("class", "qimg");
  };

  Widget.prototype._syncCells = function () {
    var keys = [];
    for (var k in this.selection) {
      if (Object.prototype.hasOwnProperty.call(this.selection, k)) keys.push(Number(k));
    }
    keys.sort(function (a, b) { return a - b; });
    var parts = [];
    for (var i = 0; i < keys.length; i++) parts.push(String(keys[i] + 1));
    this.answer.value = parts.join(",");
    this.cellStatus.textContent = tr(this.o.lang, "selectedCount", { n: keys.length });
  };

  Widget.prototype._toggleCell = function (idx, btn) {
    var n = idx + 1;
    if (Object.prototype.hasOwnProperty.call(this.selection, idx)) {
      delete this.selection[idx];
      btn.setAttribute("aria-pressed", "false");
      this.cellStatus.textContent = tr(this.o.lang, "cellUnselected", { n: n });
    } else {
      this.selection[idx] = true;
      btn.setAttribute("aria-pressed", "true");
      this.cellStatus.textContent = tr(this.o.lang, "cellSelected", { n: n });
    }
    this._syncCells();
  };


  Widget.prototype._updateSubmit = function () {
    if (this.state !== STATES.READY) return;
    var incomplete = false;
    if (this.answerMode === "rotate" || this.answerMode === "path" ||
        this.answerMode === "rank" || this.answerMode === "select" ||
        this.answerMode === "progressive" || this.answerMode === "shell" ||
        this.answerMode === "placements") {
      incomplete = !String(this.answer.value || "");
    }
    this.btnSubmit.disabled = incomplete;
  };


  Widget.prototype._resetCurrent = function () {
    if (this.destroyed || this.state !== STATES.READY) return;
    if (this.answerMode === "rotate") {
      for (var i = 0; i < this.tileTurns.length; i++) {
        this.tileTurns[i] = 0;
        this.tileBusyUntil[i] = 0;
        var tile = this.cellButtons[i] && this.cellButtons[i].firstChild;
        if (tile) tile.style.transform = "rotate(0deg)";
      }
      this.answer.value = "";
      this.cellStatus.textContent = tr(this.o.lang, "rotateReset");
    } else if (this.answerMode === "path") {
      this.pathCells = [];
      this._syncPathAnswer(this.pathChallenge, this.pathIW, this.pathIH);
      this.cellStatus.textContent = tr(this.o.lang, "localReset");
    } else if (this.answerMode === "rank") {
      this.rankOrder = [];
      for (var k = 0; k < this.cellButtons.length; k++) {
        this.cellButtons[k].setAttribute("data-sel", "false");
        var badge = this.cellButtons[k].firstChild;
        badge.textContent = "";
        badge.setAttribute("class", "rankno hidden");
      }
      this._syncRank();
      this.cellStatus.textContent = tr(this.o.lang, "localReset");
    } else if (this.answerMode === "select") {
      this.selectIndex = -1;
      for (var j = 0; j < this.cellButtons.length; j++) this.cellButtons[j].setAttribute("data-sel", "false");
      this.answer.value = "";
      this.cellStatus.textContent = tr(this.o.lang, "localReset");
    } else if (this.answerMode === "progressive") {
      this.progressiveSelections = [-1, -1, -1];
      this.answer.value = "";
      this._progressiveShowRound(0);
      this.cellStatus.textContent = tr(this.o.lang, "localReset");
    } else if (this.answerMode === "shell") {
      this.answer.value = "";
      this._renderShell(this.shellChallenge, this.shellIW, this.shellIH);
      this._setInputsEnabled(true);
      this._updateSubmit();
      return;
    } else if (this.answerMode === "placements") {
      this.placeState = [null, null, null, null, null];
      this.placeSelected = "";
      this.placeRotation = 0;
      this.placeHistory = [];
      this._syncPlacements();
      this.cellStatus.textContent = tr(this.o.lang, "localReset");
    }
    this._updateSubmit();
  };


  Widget.prototype._undoCurrent = function () {
    if (this.destroyed || this.state !== STATES.READY) return;
    if (this.answerMode === "path" && this.pathCells.length) {
      this._pathUndo();
    } else if (this.answerMode === "rank" && this.rankOrder.length) {
      this._rankToggle(this.rankOrder[this.rankOrder.length - 1]);
    } else if (this.answerMode === "placements" && this.placeHistory.length) {
      this._placeUndo();
    }
  };

  // 只移除格子按钮与监听，不重置 progressive/shell/placements 的业务状态。

  Widget.prototype._stopShell = function () {
    if (this.shellTimer) {
      global.clearTimeout(this.shellTimer);
      this.shellTimer = 0;
    }
    this.shellAnimating = false;
  };

  Widget.prototype._renderCells = function (q, iw, ih) {
    this._clearCells();
    this.answerMode = "cells";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.cellWrap.setAttribute("class", "cells");
    this.cellWrap.setAttribute("data-hit-mode", "grid");
    this.cellStatus.setAttribute("class", "cellsum");
    var self = this;
    var cols = intField(q, "grid_cols"), rows = intField(q, "grid_rows");
    var x = intField(q, "grid_x"), y = intField(q, "grid_y");
    var cell = intField(q, "grid_cell"), gap = intField(q, "grid_gap");
    for (var r = 0; r < rows; r++) {
      for (var c = 0; c < cols; c++) {
        var idx = r * cols + c, n = idx + 1;
        var left = (x + c * (cell + gap)) / iw * 100;
        var top = (y + r * (cell + gap)) / ih * 100;
        var w = cell / iw * 100, h = cell / ih * 100;
        var btn = el("button", {
          type: "button", class: "cell", "data-cell": String(n),
          "aria-label": tr(this.o.lang, "cellLabel", { n: n }),
          "aria-pressed": "false", "aria-describedby": this.note.getAttribute("id") + " " + this.status.getAttribute("id"),
          style: "left:" + left + "%;top:" + top + "%;width:" + w + "%;height:" + h + "%"
        });
        var fn = (function (cellIdx, button) {
          return function (e) { e.preventDefault(); self._toggleCell(cellIdx, button); };
        })(idx, btn);
        btn._epochaClick = fn;
        this._on(btn, "click", fn);
        this.cellButtons.push(btn);
        this.cellWrap.appendChild(btn);
      }
    }
    this.cellStatus.textContent = tr(this.o.lang, "selectedCount", { n: 0 });
  };

  Widget.prototype._renderPairs = function (q) {
    this._clearCells();
    this.answerMode = "pairs";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.cellWrap.setAttribute("class", "cells hidden");
    this.cellWrap.removeAttribute("data-hit-mode");
    this.cellStatus.setAttribute("class", "cellsum hidden");
    this.pairWrap.setAttribute("class", "pairs");
    var min = intField(q, "min"), max = intField(q, "max");
    for (var i = 0; i < 2; i++) {
      var zh = q.labels_zh[i], en = q.labels_en[i];
      this.pairLabels[i].textContent = this.o.lang === "zh-CN" ? (zh || en) : (en || zh);
      this.pairInputs[i].value = "";
      this.pairInputs[i].setAttribute("min", String(min));
      this.pairInputs[i].setAttribute("max", String(max));
      this.pairInputs[i].disabled = false;
    }
    this.heading.setAttribute("for", this.pairInputs[0].getAttribute("id"));
  };

  Widget.prototype._syncPairs = function () {
    var a = String(this.pairInputs[0].value || "").trim();
    var b = String(this.pairInputs[1].value || "").trim();
    var min = Number(this.pairInputs[0].getAttribute("min"));
    var max = Number(this.pairInputs[0].getAttribute("max"));
    var aOK = /^[0-9]{1,2}$/.test(a) && Number(a) >= min && Number(a) <= max;
    var bOK = /^[0-9]{1,2}$/.test(b) && Number(b) >= min && Number(b) <= max;
    this.answer.value = aOK && bOK ? a + "," + b : "";
    this.pairInputs[0].setAttribute("aria-invalid", a !== "" && !aOK ? "true" : "false");
    this.pairInputs[1].setAttribute("aria-invalid", b !== "" && !bOK ? "true" : "false");
  };

  Widget.prototype._renderAngle = function (q, iw, ih) {
    this._clearCells();
    this._clearPoint();
    this._clearAim();
    this._clearTiming();
    this.answerMode = "angle";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle");
    var min = intField(q, "angle_min"), max = intField(q, "angle_max"), step = intField(q, "angle_step");
    this.angleInput.setAttribute("min", String(min));
    this.angleInput.setAttribute("max", String(max));
    this.angleInput.setAttribute("step", String(step));
    this.angleInput.value = String(min);
    var cueX = intField(q, "cue_x"), cueY = intField(q, "cue_y"), aimLen = intField(q, "aim_len");
    if (cueX !== null && cueY !== null && aimLen !== null) {
      this.angleCueX = cueX;
      this.angleCueY = cueY;
      this.angleAimLen = aimLen;
      this.angleImageW = iw;
      this.angleImageH = ih;
      this.aimLayer.setAttribute("viewBox", "0 0 " + iw + " " + ih);
      this.aimLayer.setAttribute("class", "aimlayer");
      this.angleDragLayer.setAttribute("class", "angledrag");
    }
    this._syncAngle();
    this.heading.textContent = promptText(q, this.o.lang);
    this.heading.setAttribute("for", this.angleInput.getAttribute("id"));
    this.note.textContent = tr(this.o.lang, "noteAngle");
  };

  Widget.prototype._syncAngle = function () {
    var v = Number(this.angleInput.value);
    if (!isFinite(v)) v = 0;
    this.answer.value = String(v);
    this.angleOut.textContent = (v / 10).toFixed(1) + "°";
    this.angleInput.setAttribute("aria-valuetext", this.angleOut.textContent);
    if (this.angleCueX !== null && this.angleImageW > 0 && this.angleImageH > 0) {
      var rad = v / 10 * Math.PI / 180;
      this.aimLine.setAttribute("x1", String(this.angleCueX));
      this.aimLine.setAttribute("y1", String(this.angleCueY));
      this.aimLine.setAttribute("x2", String(this.angleCueX + Math.cos(rad) * this.angleAimLen));
      this.aimLine.setAttribute("y2", String(this.angleCueY + Math.sin(rad) * this.angleAimLen));
    }
  };

  Widget.prototype._angleFromPointer = function (ev) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "angle") return;
    if (this.angleCueX === null || !this.angleImageW || !this.angleImageH) return;
    var rect = this.imgBox.getBoundingClientRect();
    if (!rect.width || !rect.height) return;
    var x = (ev.clientX - rect.left) / rect.width * this.angleImageW;
    var y = (ev.clientY - rect.top) / rect.height * this.angleImageH;
    var dx = x - this.angleCueX, dy = y - this.angleCueY;
    if (!dx && !dy) return;
    var deg = Math.atan2(dy, dx) * 180 / Math.PI;
    if (deg < 0) deg += 360;
    this.angleInput.value = String(Math.round(deg * 10) % 3600);
    this._syncAngle();
  };

  Widget.prototype._anglePointerDown = function (ev) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "angle") return;
    this.angleDragging = true;
    this.anglePointer = ev.pointerId;
    try { this.angleDragLayer.setPointerCapture(ev.pointerId); } catch (e) { /* 指针已释放 */ }
    this._angleFromPointer(ev);
    ev.preventDefault();
  };

  Widget.prototype._anglePointerMove = function (ev) {
    if (!this.angleDragging || this.anglePointer !== ev.pointerId) return;
    this._angleFromPointer(ev);
    ev.preventDefault();
  };

  Widget.prototype._anglePointerUp = function (ev) {
    if (this.anglePointer !== ev.pointerId) return;
    this.angleDragging = false;
    this.anglePointer = null;
    try { this.angleDragLayer.releasePointerCapture(ev.pointerId); } catch (e) { /* 已释放 */ }
  };

  Widget.prototype._renderPoint = function (q) {
    this._clearCells();
    this._clearAim();
    this._clearTiming();
    this.answerMode = "point";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.pointLayer.setAttribute("class", "pointlayer");
    this.pointMark.setAttribute("class", "pointmark hidden");
    this.pointLayer.setAttribute("aria-label", tr(this.o.lang, "pointLabel"));
    this.heading.textContent = promptText(q, this.o.lang);
    this.heading.removeAttribute("for");
    this.pointLayer.setAttribute("aria-labelledby", this.heading.getAttribute("id"));
    this.note.textContent = tr(this.o.lang, "notePoint");
  };

  Widget.prototype._renderTextOptions = function (q) {
    this._clearCells();
    this._clearPoint();
    this._clearAim();
    this._clearTiming();
    this._clearOptions();
    this._showFrameControls(false);
    this.answerMode = "options";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.timingWrap.setAttribute("class", "timing hidden");
    this.imgWrap.setAttribute("class", "img hidden");
    this.imgMsg.setAttribute("class", "imgmsg hidden");
    this.optionsWrap.setAttribute("class", "options");
    this.optionsWrap.setAttribute("aria-labelledby", this.heading.getAttribute("id"));
    this.heading.textContent = promptText(q, this.o.lang);
    this.heading.removeAttribute("for");
    this.note.textContent = tr(this.o.lang, "noteAccessible");
    var self = this;
    for (var i = 0; i < q.options.length; i++) {
      var o = q.options[i];
      var label = self.o.lang === "zh-CN" ? (o.label_zh || o.label_en) : (o.label_en || o.label_zh);
      var btn = el("button", { type: "button", class: "opt", "aria-pressed": "false" });
      btn.textContent = o.id + ". " + label;
      var fn = (function (opt, button) {
        return function (e) {
          e.preventDefault();
          if (self.state !== STATES.READY) return;
          self.answer.value = opt.id;
          for (var k = 0; k < self.optionButtons.length; k++) {
            self.optionButtons[k].setAttribute("aria-pressed", self.optionButtons[k] === button ? "true" : "false");
          }
        };
      })(o, btn);
      btn._epochaClick = fn;
      this._on(btn, "click", fn);
      this.optionButtons.push(btn);
      this.optionsWrap.appendChild(btn);
    }
  };

  Widget.prototype._setPoint = function (x, y) {
    x = Math.max(0, Math.min(999, Math.round(x)));
    y = Math.max(0, Math.min(999, Math.round(y)));
    this.answer.value = x + "," + y;
    this.pointMark.setAttribute("class", "pointmark");
    this.pointMark.style.left = (x / 999 * 100) + "%";
    this.pointMark.style.top = (y / 999 * 100) + "%";
    this.pointLayer.setAttribute("aria-label", tr(this.o.lang, "pointSelected", { x: x, y: y }));
  };

  Widget.prototype._pointFromEvent = function (ev) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "point") return;
    ev.preventDefault();
    var rect = this.imgBox.getBoundingClientRect();
    if (!rect.width || !rect.height) return;
    this._setPoint(
      (ev.clientX - rect.left) / rect.width * 999,
      (ev.clientY - rect.top) / rect.height * 999
    );
  };

  Widget.prototype._pointKey = function (ev) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "point") return;
    var cur = String(this.answer.value || "500,500").split(",");
    var x = Number(cur[0]), y = Number(cur[1]);
    if (!isFinite(x) || !isFinite(y)) { x = 500; y = 500; }
    var step = ev.shiftKey ? 1 : 10;
    var handled = true;
    switch (ev.key) {
      case "ArrowLeft": x -= step; break;
      case "ArrowRight": x += step; break;
      case "ArrowUp": y -= step; break;
      case "ArrowDown": y += step; break;
      case "Home": x = 500; y = 500; break;
      default: handled = false;
    }
    if (!handled) return;
    ev.preventDefault();
    this._setPoint(x, y);
  };

  Widget.prototype._renderTiming = function (q) {
    this._clearCells();
    this._clearPoint();
    this.answerMode = "timing";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.timingWrap.setAttribute("class", "timing");
    this.timingOut.textContent = tr(this.o.lang, "timingIdle");
    this._showFrameControls(false);
    this.heading.textContent = promptText(q, this.o.lang);
    this.heading.setAttribute("for", this.timingBtn.getAttribute("id"));
    this.note.textContent = tr(this.o.lang, "noteTiming");
    if (!this.frameAnimated) {
      this._fail("motion-required", "errMotionRequired", null, true);
      return false;
    }
    return true;
  };

  Widget.prototype._recordTiming = function () {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "timing") return;
    if (!this.frameStartedAt) return;
    var elapsed = Date.now() - this.frameStartedAt;
    if (this.frameCycleMs > 0) elapsed = elapsed % this.frameCycleMs;
    var ms = Math.max(0, Math.min(60000, Math.round(elapsed)));
    this.answer.value = String(ms);
    this.timingOut.textContent = tr(this.o.lang, "timingRecorded", { ms: ms });
  };

  function spritePosition(frame, cols, rows) {
    return (cols > 1 ? frame % cols / (cols - 1) * 100 : 0) + "% " +
      (rows > 1 ? Math.floor(frame / cols) / (rows - 1) * 100 : 0) + "%";
  }

  function spriteStyle(view, iw, ih, cols, rows) {
    view.style.width = iw + "px";
    view.style.height = "auto";
    view.style.aspectRatio = iw + " / " + ih;
    view.style.backgroundSize = (cols * 100) + "% " + (rows * 100) + "%";
    view.style.backgroundPosition = spritePosition(0, cols, rows);
  }

  Widget.prototype._renderFrames = function (q, iw, ih, alt) {
    if (!validFrames(q, iw, ih)) { this._imageFailed(); return false; }
    this._clearImage();
    var count = intField(q, "frame_count"), cols = intField(q, "frame_cols");
    var rows = intField(q, "frame_rows"), ms = intField(q, "frame_ms");
    var view = el("div", { class: "frameview", role: "img", "aria-label": alt });
    spriteStyle(view, iw, ih, cols, rows);
    view.style.backgroundImage = "url(data:" + q.image_mime + ";base64," + q.image_base64 + ")";
    this.frameView = view;
    this.imgBox.appendChild(view);
    this._showImageBox();

    var reduced = global.matchMedia && global.matchMedia("(prefers-reduced-motion: reduce)").matches;
    if (reduced) {
      this.frameAnimated = false;
      this.frameStartedAt = 0;
      this.frameCycleMs = 0;
      this._showFrameControls(false);
      return true;
    }
    if (typeof view.animate !== "function") { this._imageFailed(); return false; }
    var keyframes = [];
    for (var f = 0; f < count; f++) {
      keyframes.push({
        backgroundPosition: spritePosition(f, cols, rows),
        offset: f / count
      });
    }
    var anim;
    try {
      anim = view.animate(keyframes, {
        duration: count * ms,
        iterations: q.frame_loop ? Infinity : 1,
        easing: "steps(1, end)"
      });
    } catch (e) { this._imageFailed(); return false; }
    this.frameAnim = anim;
    this.frameAnimated = true;
    this.frameStartedAt = Date.now();
    this.frameCycleMs = q.frame_loop ? count * ms : 0;
    this.framePlaying = true;
    this.frameToggle.textContent = tr(this.o.lang, "framePause");
    this._showFrameControls(true);
    return true;
  };

  Widget.prototype._toggleFrames = function () {
    if (!this.frameAnim || !this.frameAnimated || this.answerMode === "timing") return;
    if (this.frameAnim.playState === "paused") {
      this.frameAnim.play();
      this.framePlaying = true;
      this.frameToggle.textContent = tr(this.o.lang, "framePause");
    } else {
      this.frameAnim.pause();
      this.framePlaying = false;
      this.frameToggle.textContent = tr(this.o.lang, "framePlay");
    }
  };

  Widget.prototype._replayFrames = function () {
    if (!this.frameAnim || !this.frameAnimated || this.answerMode === "timing") return;
    try {
      this.frameAnim.currentTime = 0;
      this.frameAnim.play();
      this.frameStartedAt = Date.now();
      this.framePlaying = true;
      this.frameToggle.textContent = tr(this.o.lang, "framePause");
    } catch (e) { /* 动画已结束 */ }
  };

  Widget.prototype._renderQuestionMedia = function (q, iw, ih, alt) {
    if (q.render_mode === "sprite_sheet") return this._renderFrames(q, iw, ih, alt);
    return this._renderImage(q, iw, ih, alt);
  };

  Widget.prototype._telBegin = function () {
    this.tel = { start: Date.now(), first: 0, events: 0, pointer: 0, key: 0 };
  };

  Widget.prototype._telEvent = function (kind) {
    if (this.state !== STATES.READY || !this.tel || !this.tel.start) return;
    if (this.tel.events >= 10000) return;
    this.tel.events++;
    if (!this.tel.first) this.tel.first = Math.max(0, Date.now() - this.tel.start);
    if (kind === "pointer") this.tel.pointer++;
    else this.tel.key++;
  };

  Widget.prototype._telemetrySummary = function () {
    var t = this.tel || {};
    var total = t.start ? Math.max(0, Date.now() - t.start) : 0;
    var events = Math.min(10000, Math.max(0, t.events || 0));
    var pointer = Math.min(events, Math.max(0, t.pointer || 0));
    var key = Math.min(events - pointer, Math.max(0, t.key || 0));
    return {
      total_ms: total,
      first_input_ms: Math.min(total, Math.max(0, t.first || 0)),
      event_count: events,
      pointer_events: pointer,
      key_events: key,
      focus_blur: 0
    };
  };

  Widget.prototype._setState = function (next) {
    var my = this.gen;
    this.state = next;
    if (this.o.onState) { try { this.o.onState(next, this.id); } catch (e) { /* 回调异常不影响组件 */ } }
    return !this.destroyed && my === this.gen && this.state === next;
  };

  Widget.prototype._say = function (text, kind) {
    this.status.textContent = String(text);
    this.status.setAttribute("data-kind", kind || "idle");
  };

  Widget.prototype._fail = function (code, i18nKey, vars, retryable) {
    this.gen++;
    this._cancelRequest();
    this._clearRetry();
    this._clearDeadline();
    this._terminateWorker();
    this._clearFrameAnim();
    this.challenge = null;
    this._setToken("");
    if (!this._setState(STATES.ERROR)) return;
    this._say(tr(this.o.lang, i18nKey, vars), "err");
    this.btnSubmit.disabled = true;
    this.btnRefresh.disabled = false;
    this._setInputsEnabled(false);
    this.answer.setAttribute("aria-invalid", "true");
    var err = { code: code, message: tr(this.o.lang, i18nKey, vars), retryable: !!retryable, widgetId: this.id };
    if (this.o.onError) { try { this.o.onError(err); } catch (e) { /* 同上 */ } }
    return err;
  };

  Widget.prototype._setInputsEnabled = function (enabled) {
    this.answer.disabled = !(enabled && this.answerMode === "text");
    for (var i = 0; i < this.pairInputs.length; i++) {
      this.pairInputs[i].disabled = !(enabled && this.answerMode === "pairs");
    }
    var gridMode = this.answerMode === "cells" || this.answerMode === "rotate" ||
      this.answerMode === "path" || this.answerMode === "rank" || this.answerMode === "select" ||
      this.answerMode === "progressive" || this.answerMode === "shell" ||
      this.answerMode === "placements";
    for (var j = 0; j < this.cellButtons.length; j++) {
      var gridDisabled = !(enabled && gridMode);
      if (this.answerMode === "shell" && this.shellAnimating) gridDisabled = true;
      this.cellButtons[j].disabled = gridDisabled;
    }
    this.angleInput.disabled = !(enabled && this.answerMode === "angle");
    this.timingBtn.disabled = !(enabled && this.answerMode === "timing");
    for (var oi = 0; oi < this.optionButtons.length; oi++) {
      this.optionButtons[oi].disabled = !(enabled && this.answerMode === "options");
    }
    this.angleDragLayer.style.pointerEvents = (enabled && this.answerMode === "angle") ? "auto" : "none";
    var framesOn = enabled && this.frameAnimated && this.answerMode !== "timing";
    this.frameToggle.disabled = !framesOn;
    this.frameReplay.disabled = !framesOn;
    var pointOn = enabled && this.answerMode === "point";
    this.pointLayer.setAttribute("tabindex", pointOn ? "0" : "-1");
    this.pointLayer.setAttribute("aria-disabled", pointOn ? "false" : "true");
    if (this.btnReset) this.btnReset.disabled = !(enabled && gridMode);
    if (this.btnUndo) this.btnUndo.disabled = !(enabled && (this.answerMode === "path" || this.answerMode === "rank" || this.answerMode === "placements"));
    if (this.btnRotate) {
      this.btnRotate.disabled = !(enabled && this.answerMode === "placements" && this.placeSelected !== "");
    }
  };

  Widget.prototype._enable = function (busy) {
    this.btnSubmit.disabled = !busy;
    this.btnRefresh.disabled = !busy;
    this._setInputsEnabled(busy);
  };

  Widget.prototype._clearDeadline = function () {
    this._cancelInteractive();
    if (this.timer) { global.clearTimeout(this.timer); this.timer = 0; }
    this.deadlineEpoch = 0;
  };

  Widget.prototype._cancelRequest = function () {
    if (this.inflight) this.inflight.abort();
  };

  Widget.prototype._clearRetry = function () {
    if (this.retryTimer) { global.clearTimeout(this.retryTimer); this.retryTimer = 0; }
  };

  Widget.prototype._armDeadline = function (deadlineEpoch, onExpire) {
    var self = this, my = this.gen;
    this._clearDeadline();
    if (!deadlineEpoch) return;
    this.deadlineEpoch = deadlineEpoch;
    var ms = deadlineEpoch - Date.now();
    if (ms <= 0) { this.deadlineEpoch = 0; onExpire(); return; }
    var timer = global.setTimeout(function () {
      if (self.destroyed || my !== self.gen || self.timer !== timer) return;
      self.timer = 0;
      self.deadlineEpoch = 0;
      onExpire();
    }, ms);
    this.timer = timer;
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
    this.gen++;
    this.challenge = null;
    this._clearDeadline();
    this._terminateWorker();
    this._clearFrameAnim();
    if (!this._setState(STATES.EXPIRED)) return;
    this._say(tr(this.o.lang, "expired"), "err");
    this.btnSubmit.disabled = true;
    this.btnRefresh.disabled = false;
    this._setInputsEnabled(false);
  };

  Widget.prototype._expireToken = function () {
    this._setToken("");
    this.challenge = null;
    this._clearDeadline();
    this._clearFrameAnim();
    if (!this._setState(STATES.EXPIRED)) return;
    this._say(tr(this.o.lang, "expired"), "err");
    this.btnRefresh.disabled = false;
    this._setInputsEnabled(false);
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
    this._cancelRequest();
    this._clearRetry();
    this._clearDeadline();
    this._terminateWorker();
    this.challenge = null;
    this._setToken("");
    this.successFired = false;
    this._resetInputs();
    this.answer.setAttribute("class", "ans hidden");
    this._showImageMessage("…");
    if (!this._setState(STATES.LOADING)) return;
    this._say("…", "busy");
    this._enable(false);

    var loadStartedAt = Date.now();
    this.tel = { start: 0, first: 0, events: 0, pointer: 0, key: 0 };
    this._request("/api/challenge", {
      site_key: o.siteKey, lang: o.lang, session: this.session,
      accessibility: o.accessibility === true, device: deviceHints()
    })
      .then(function (r) {
        if (self.destroyed || my !== self.gen) return;      // 晚到的响应不能覆盖新题
        if (!r.ok) { self._handleHttpFailure(r); return; }
        var j = r.json;
        if (typeof j.token !== "string" || !j.pow || j.pow.algo !== POW_ALGO) {
          self._fail("bad-response", "errBadResponse", null, true); return;
        }
        if (j.mode !== "pow_question" || !j.question) {
          self._fail("bad-response", "errBadResponse", { why: "mode" }, false); return;
        }
        var q = j.question;
        var at = String(q.answer_type || "text");
        var media = q.media === undefined ? (Array.isArray(q.options) ? "text" : "image") : String(q.media);
        var iw = 0, ih = 0;
        if (media === "image") {
          if (q.image_mime !== MIME_PNG || !base64Valid(q.image_base64 || "")
              || (q.image_base64 || "").length > MAX_IMAGE_B64_CHARS) {
            self._fail("image-unusable", "errImageUnusable", null, true); return;
          }
          iw = Number(q.image_width) || 0;
          ih = Number(q.image_height) || 0;
          if (iw < 1 || ih < 1 || iw > 640 || ih > 640) {
            self._fail("image-unusable", "errImageUnusable", null, true); return;
          }
        } else if (media === "text") {
          if (at !== "text" || !validTextOptions(q)) {
            self._fail("bad-response", "errBadResponse", { why: "text-options" }, false); return;
          }
        } else {
          self._fail("bad-response", "errBadResponse", { why: "media" }, false); return;
        }
        if (at !== "text" && at !== "cells" && at !== "pairs"
            && at !== "angle" && at !== "point" && at !== "timing"
            && at !== "masks" && at !== "path" && at !== "order" && at !== "select"
            && at !== "sequence" && at !== "placements") {
          self._fail("bad-response", "errBadResponse", { why: "answer_type" }, false); return;
        }
        var rm = q.render_mode === undefined ? "static" : String(q.render_mode);
        if (media === "image") {
          if (rm !== "static" && rm !== "sprite_sheet") {
            self._fail("bad-response", "errBadResponse", { why: "render_mode" }, false); return;
          }
          if (rm === "sprite_sheet" && !validFrames(q, iw, ih)) {
            self._fail("bad-response", "errBadResponse", { why: "frames" }, false); return;
          }
          if (at === "timing" && rm !== "sprite_sheet") {
            self._fail("bad-response", "errBadResponse", { why: "timing" }, false); return;
          }
        }
        if (at === "text" && media === "image"
            && (!Number.isInteger(q.length) || q.length < 1 || q.length > 32)) {
          self._fail("bad-response", "errBadResponse", { why: "length" }, false); return;
        }
        if (at === "cells" && !validGrid(q, iw, ih)) {
          self._fail("bad-response", "errBadResponse", { why: "grid" }, false); return;
        }
        if (at === "pairs" && !validPairs(q)) {
          self._fail("bad-response", "errBadResponse", { why: "pairs" }, false); return;
        }
        if (at === "angle" && !validAngle(q)) {
          self._fail("bad-response", "errBadResponse", { why: "angle" }, false); return;
        }
        if (at === "angle" && (q.cue_x !== undefined || q.cue_y !== undefined || q.aim_len !== undefined)
            && !validAim(q, iw, ih)) {
          self._fail("bad-response", "errBadResponse", { why: "aim" }, false); return;
        }
        if (at === "point" && !validPoint(q)) {
          self._fail("bad-response", "errBadResponse", { why: "point" }, false); return;
        }
        self.challenge = {
          token: j.token, pow: j.pow, question: q,
          expires: Number(j.expires_in) || 0
        };
        if (self.challenge.expires > 0) {
          self._armDeadline(loadStartedAt + self.challenge.expires * 1000, function () { self._expireChallenge(); });
          if (self.destroyed || my !== self.gen || !self.challenge) return;
        }
        var proceed = function () {
        if (self.destroyed || my !== self.gen || !self.challenge) return;
        if (at === "text") {
          if (media === "text") {
            self._renderTextOptions(q);
          } else {
            self.answer.setAttribute("class", "ans");
            if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAlt", { length: q.length }))) return;
            self.heading.textContent = tr(o.lang, "instruction", { length: q.length });
            self.heading.setAttribute("for", self.answer.getAttribute("id"));
            self.note.textContent = tr(o.lang, "charsetNote", { charset: q.charset || "—" });
            if (Number(q.tolerant) > 0) self.note.textContent += " " + tr(o.lang, "tolerantNote");
            self.answer.placeholder = tr(o.lang, "answerPlaceholder", { length: q.length });
          }
        } else if (at === "cells") {
          if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltCells"))) return;
          self._renderCells(q, iw, ih);
          self.heading.textContent = promptText(q, o.lang);
          self.heading.setAttribute("for", self.answer.getAttribute("id"));
          self.note.textContent = tr(o.lang, "noteCells");
        } else if (at === "masks") {
          if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltRotate"))) return;
          self._renderRotate(q, iw, ih);
          self.heading.textContent = promptText(q, o.lang);
          self.heading.setAttribute("for", self.answer.getAttribute("id"));
          self.note.textContent = tr(o.lang, "noteRotate");
        } else if (at === "path") {
          if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltPath"))) return;
          self._renderPath(q, iw, ih);
          self.heading.textContent = promptText(q, o.lang);
          self.heading.removeAttribute("for");
          self.note.textContent = tr(o.lang, "notePath");
        } else if (at === "order") {
          if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltRank"))) return;
          self._renderRank(q, iw, ih);
          self.heading.textContent = promptText(q, o.lang);
          self.heading.removeAttribute("for");
          self.note.textContent = tr(o.lang, "noteRank");
        } else if (at === "select") {
          if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltSelect"))) return;
          self._renderSelect(q, iw, ih);
          self.heading.textContent = promptText(q, o.lang);
          self.heading.removeAttribute("for");
          self.note.textContent = tr(o.lang, "noteSelect");
        } else if (at === "sequence") {
          if (q.input_mode === "shell") {
            if (!self._renderShell(q, iw, ih)) return;
            self.heading.textContent = promptText(q, o.lang);
            self.heading.removeAttribute("for");
            self.note.textContent = tr(o.lang, "noteShell");
          } else {
            if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltProgressive"))) return;
            self._renderProgressive(q, iw, ih);
            self.heading.textContent = promptText(q, o.lang);
            self.heading.removeAttribute("for");
            self.note.textContent = tr(o.lang, "noteProgressive");
          }
        } else if (at === "placements") {
          if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltPlacements"))) return;
          self._renderPlacements(q, iw, ih);
          self.heading.textContent = promptText(q, o.lang);
          self.heading.removeAttribute("for");
          self.note.textContent = tr(o.lang, "notePlacements");
        } else if (at === "pairs") {
          if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltPairs"))) return;
          self._renderPairs(q);
          self.heading.textContent = promptText(q, o.lang);
          self.note.textContent = tr(o.lang, "notePairs");
        } else if (at === "angle") {
          if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltAngle"))) return;
          self._renderAngle(q, iw, ih);
        } else if (at === "point") {
          if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltPoint"))) return;
          self._renderPoint(q);
        } else {
          if (!self._renderQuestionMedia(q, iw, ih, tr(o.lang, "imageAltTiming"))) return;
          if (!self._renderTiming(q)) return;
        }
        if (!self._ensureWorker()) return;
        self._enable(true);
        if (!self._setState(STATES.READY)) return;
        self._updateSubmit();
        self._telBegin();
        self._say(tr(o.lang, "ready"), "idle");
        self.autoRetry = 0;
        if (self.challenge.expires > 0) {
          self._armDeadline(self.deadlineEpoch, function () { self._expireChallenge(); });
        }
        };
        var needsInteractive = at === "masks" || at === "path" || at === "order" ||
          at === "select" || at === "sequence" || at === "placements";
        if (needsInteractive) {
          self._ensureInteractive().then(function () {
            if (self.destroyed || my !== self.gen) return;
            // 模块加载也是等待：不能在 TTL 已过之后才把题面画出来。
            if (self.challenge && self.challenge.expires > 0 && Date.now() >= self.deadlineEpoch) {
              self._expireChallenge();
              return;
            }
            if (typeof Widget._validInteractive !== "function" ||
                !Widget._validInteractive(at, q, iw, ih)) {
              self._fail("bad-response", "errBadResponse", { why: at }, false);
              return;
            }
            proceed();
          }, function () {
            if (self.destroyed || my !== self.gen) return;
            self._fail("interactive-unavailable", "errInteractiveUnavailable", null, true);
          });
          return;
        }
        proceed();
      })
      .catch(function (err) {
        if (self.destroyed || my !== self.gen || (err && err.code === "cancelled")) return;
        self._fail(err && err.code === "timeout" ? "timeout" : "network", "errNetwork", null, true);
      });
  };

  Widget.prototype._imageFailed = function () {
    if (this.destroyed) return;
    this._showImageMessage(tr(this.o.lang, "errImageUnusable"));
    this._fail("image-unusable", "errImageUnusable", null, true);
  };

  Widget.prototype._renderImage = function (q, iw, ih, alt) {
    this._clearImage();
    var img = el("img", {
      src: "data:" + q.image_mime + ";base64," + q.image_base64,
      alt: alt,
      width: String(iw), height: String(ih), decoding: "async"
    });
    var self = this, my = this.gen;
    img.addEventListener("error", function () {
      if (self.destroyed || my !== self.gen || img.parentNode !== self.imgBox) return;
      self._imageFailed();
    });
    this.imgEl = img;
    this.imgBox.appendChild(img);
    this._showImageBox();
    return true;
  };

  Widget.prototype._handleHttpFailure = function (r) {
    var codes = (r.json && r.json["error-codes"]) || [];
    var code = codes.length ? String(codes[0]) : "";
    var self = this;
    var o = this.o;
    if (r.status === 503 && code === "challenge-unavailable") {
      if (this.autoRetry < o.maxAutoRetry) {
        this.autoRetry++;
        if (!this._setState(STATES.LOADING)) return;
        this._say(tr(o.lang, "errUnavailable") + " (" + this.autoRetry + "/" + o.maxAutoRetry + ")", "busy");
        var wait = Math.min(Math.max(Number(r.headers && r.headers.get("Retry-After")) || 2, 1), 30) * 1000;
        var my = this.gen;
        this._clearRetry();
        var timer = global.setTimeout(function () {
          if (self.destroyed || my !== self.gen || self.retryTimer !== timer) return;
          self.retryTimer = 0;
          self.load();
        }, wait);
        this.retryTimer = timer;
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

  Widget.prototype._request = function (path, payload) {
    var self = this, my = this.gen;
    this._cancelRequest();
    return new Promise(function (resolve, reject) {
      var controller = new AbortController();
      var settled = false, timer = 0;
      var request = {
        abort: function () {
          if (!finish({ code: "cancelled" })) return;
          try { controller.abort(); } catch (e) { /* noop */ }
        }
      };
      function finish(err, response) {
        if (settled) return false;
        settled = true;
        global.clearTimeout(timer);
        if (self.inflight === request) self.inflight = null;
        if (err) reject(err);
        else resolve(response);
        return true;
      }
      self.inflight = request;
      timer = global.setTimeout(function () {
        if (self.destroyed || my !== self.gen || self.inflight !== request) return;
        if (!finish({ code: "timeout", key: "errNetwork" })) return;
        try { controller.abort(); } catch (e) { /* noop */ }
      }, self.o.fetchTimeoutMs);
      function failed(err) {
        finish({ code: err && err.name === "AbortError" ? "timeout" : "network", key: "errNetwork" });
      }
      try {
        self._post(path, payload, controller.signal).then(function (r) { finish(null, r); }, failed);
      } catch (e) { failed(e); }
    });
  };

  Widget.prototype._post = function (path, payload, signal) {
    var o = this.o;
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
      this.worker = null;
      if (this.workerBlobUrl) { try { global.URL.revokeObjectURL(this.workerBlobUrl); } catch (e2) { /* noop */ } }
      this.workerBlobUrl = "";
      this._fail("worker-blocked", "errWorkerBlocked", null, false);
      return null;
    }
    var self = this, my = this.gen;
    var worker = this.worker;
    worker.addEventListener("message", function (ev) {
      if (self.destroyed || my !== self.gen || self.worker !== worker) return;
      self._dispatch(ev.data);
    });
    worker.addEventListener("error", function () {
      if (self.destroyed || my !== self.gen || self.worker !== worker) return;
      self._fail("worker-blocked", "errWorkerBlocked", null, false);
    });
    return this.worker;
  };

  Widget.prototype._dispatch = function (m) {
    if (!m || !m.job) return;
    var p = this.pending[m.job];
    if (!p) return;
    if (m.type === "started" || m.type === "progress") { p.settle(m); return; }
    delete this.pending[m.job];          // 终态：一次性结算
    p.settle(m);
  };

  Widget.prototype._terminateWorker = function () {
    for (var k in this.pending) {
      if (Object.prototype.hasOwnProperty.call(this.pending, k)) {
        var s = this.pending[k];
        try { s.cancel(); } catch (e) { /* noop */ }
        delete this.pending[k];
      }
    }
    if (this.worker) { try { this.worker.terminate(); } catch (e) { /* 已终止 */ } this.worker = null; }
    if (this.workerBlobUrl) {
      try { global.URL.revokeObjectURL(this.workerBlobUrl); } catch (e) { /* 非浏览器环境 */ }
      this.workerBlobUrl = "";
    }
  };

  Widget.prototype._solvePow = function (pow, onProgress) {
    var self = this, o = this.o;
    var worker = this._ensureWorker();
    if (!worker) return Promise.reject({ code: "worker-unavailable", key: "errWorkerBlocked" });

    var job = ++this.jobSeq;
    var bits = Number(pow.bits);
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
      if (this.answerMode === "cells") this._say(tr(o.lang, "errIncompleteSelection"), "err");
      else if (this.answerMode === "pairs") this._say(tr(o.lang, "errIncompletePairs"), "err");
      else if (this.answerMode === "angle") this._say(tr(o.lang, "errIncompleteAngle"), "err");
      else if (this.answerMode === "point") this._say(tr(o.lang, "errIncompletePoint"), "err");
      else if (this.answerMode === "timing") this._say(tr(o.lang, "errIncompleteTiming"), "err");
      else if (this.answerMode === "options") this._say(tr(o.lang, "errIncompleteOption"), "err");
      else if (this.answerMode === "rotate") this._say(tr(o.lang, "errIncompleteRotate"), "err");
      else if (this.answerMode === "path") this._say(tr(o.lang, "errIncompletePath"), "err");
      else if (this.answerMode === "rank") this._say(tr(o.lang, "errIncompleteRank"), "err");
      else if (this.answerMode === "select") this._say(tr(o.lang, "errIncompleteSelect"), "err");
      else if (this.answerMode === "progressive") this._say(tr(o.lang, "errIncompleteProgressive"), "err");
      else if (this.answerMode === "shell") this._say(tr(o.lang, "errIncompleteShell"), "err");
      else if (this.answerMode === "placements") this._say(tr(o.lang, "errIncompletePlacements"), "err");
      else this._say(tr(o.lang, "errInvalidRequest"), "err");
      return;
    }
    var my = this.gen;
    var challenge = this.challenge;
    var redeemStartedAt = 0;
    this._clearDeadline();
    if (!this._setState(STATES.SOLVING)) return;
    this._enable(false);
    this._say(tr(o.lang, "solving"), "busy");

    this._solvePow(challenge.pow, function (m) {
      if (self.destroyed || my !== self.gen) return;
      self._say(tr(o.lang, "solvingProgress", { tries: m.tries, ms: m.ms }), "busy");
    }).then(function (nonce) {
      if (self.destroyed || my !== self.gen) return;        // 被取消/换代：丢弃这次结果
      if (!self._setState(STATES.SUBMITTING)) return;
      self._say(tr(o.lang, "submitting"), "busy");
      redeemStartedAt = Date.now();
      return self._request("/api/redeem", {
        token: challenge.token, pow_nonce: nonce, answer: val,
        telemetry: self._telemetrySummary()
      }).then(function (r) {
        if (self.destroyed || my !== self.gen) return;
        if (!r.ok) { self._handleRedeemFailure(r); return; }
        var token = r.json && r.json.response;
        if (typeof token !== "string" || token.length === 0 || token.length > 4096) {
          self._fail("bad-response", "errBadResponse", null, false); return;
        }
        var ttl = Number(r.json.expires_in) || 0;
        var tokenDeadline = ttl > 0 ? redeemStartedAt + ttl * 1000 : 0;
        if (tokenDeadline > 0 && Date.now() >= tokenDeadline) {
          self._expireToken();
          return;
        }
        if (tokenDeadline > 0) {
          self.tokenDeadline = tokenDeadline;
          self._armDeadline(tokenDeadline, function () { self._expireToken(); });
        }
        self._setToken(token);
        self.challenge = null;
        if (!self._setState(STATES.VERIFIED)) return;
        self._recheckDeadline();
        if (self.state !== STATES.VERIFIED) return;
        self._say(tr(o.lang, "verified"), "ok");
        self.btnSubmit.disabled = true;
        self.btnRefresh.disabled = false;
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
      this._setToken("");
      if (!this._setState(STATES.ERROR)) return;
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
    this.autoRetry = 0;
    this.load();
  };

  Widget.prototype.reset = function () {
    if (this.destroyed) return;
    this.gen++;
    this._cancelRequest();
    this._clearRetry();
    this._terminateWorker();
    this._clearDeadline();
    this.challenge = null;
    this._setToken("");
    this.successFired = false;
    this.autoRetry = 0;
    this._resetInputs();
    this._showImageMessage(tr(this.o.lang, "reset"));
    this.btnSubmit.disabled = true;
    this.btnRefresh.disabled = false;
    this._setInputsEnabled(false);
    if (!this._setState(STATES.IDLE)) return;
    this._say(tr(this.o.lang, "reset"), "idle");
  };

  Widget.prototype.destroy = function () {
    if (this.destroyed) return;
    this.gen++;
    this.destroyed = true;
    this._cancelRequest();
    this._clearRetry();
    this._terminateWorker();
    this._clearDeadline();
    this._clearFrameAnim();
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
      if (!w) return "";
      w._recheckDeadline();
      return w.state === STATES.VERIFIED ? w.token : "";
    },
    reset: function (id) { var w = registry.get(id); if (w) w.reset(); },
    refresh: function (id) { var w = registry.get(id); if (w) w.refresh(); },
    destroy: function (id) { var w = registry.get(id); if (w) w.destroy(); },
    state: function (id) { var w = registry.get(id); return w ? w.state : null; },
    ids: function () { return Array.from(registry.keys()); },
    util: { leadingZeroBits: leadingZeroBits, b64urlToBytes: b64urlToBytes, lang: pickLang }
  };

  api._installInteractive = function (install) {
    if (Widget._interactive) return;
    install({
      Widget: Widget, STATES: STATES, el: el, tr: tr, intField: intField,
      validGrid: validGrid, validPrompts: validPrompts, clear: clear, i18n: I18N, global: global,
      spriteStyle: spriteStyle, spritePosition: spritePosition
    });
    Widget._interactive = true;
  };

  global.EpoCHA = api;
})(typeof window !== "undefined" ? window : globalThis);
