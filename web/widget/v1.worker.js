/*! epocha widget PoW worker v1 — sha256-prefix 工作量证明（DESIGN §7）
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
}
