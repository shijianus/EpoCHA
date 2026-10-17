// Node 侧的共享向量测试：把 **web/widget/v1.worker.js 的原始字节** 在 vm 里当作 worker 跑，
// 断言它的 SHA-256 与 PoW 搜索与 internal/pow/testdata/pow_vectors.json 完全一致。
//
// 为什么用 vm 而不是 jsdom/第三方包：这不是"模拟 DOM"，而是复刻 worker 的真实环境
// （一个 self、postMessage、atob、setTimeout），加载的就是浏览器要执行的那份字节。
// 零依赖，且"测的实现"与"跑的实现"是同一份。
//
// 运行：node --test docs/scripts/browser/
// 退出码：0 通过 / 1 失败。node --test 没有"3 = 未测"的语义 ⇒ 未测项由上层脚本判定并打印。

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { Buffer } from "node:buffer";
import vm from "node:vm";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "..", "..", "..");
const WORKER_PATH = path.join(ROOT, "web", "widget", "v1.worker.js");
const WIDGET_PATH = path.join(ROOT, "web", "widget", "v1.js");
const VECTORS_PATH = path.join(ROOT, "internal", "pow", "testdata", "pow_vectors.json");

function makeWorker(source) {
  const posted = [];
  const timers = [];
  const sandbox = {
    atob: (s) => Buffer.from(s, "base64").toString("binary"),
    setTimeout: (fn, ms) => { timers.push({ fn, ms: ms || 0 }); return timers.length; },
    clearTimeout: () => {},
    performance: { now: () => 0 },
    postMessage: (m) => posted.push(m),
    addEventListener: (_type, fn) => { sandbox.__onmessage = fn; },
    console,
    posted,
    timers,
    timersPending: () => timers.length,
    runNextTimer: () => {
      const t = timers.shift();
      if (!t) return false;
      t.fn();
      return true;
    },
    takePosted: () => posted.splice(0, posted.length),
  };
  sandbox.self = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox, { filename: "v1.worker.js" });
  // 刻意叫 send 而不是 post：worker 里有一个**全局函数 post()**，
  // 在 vm 里沙箱属性就是全局，同名会直接把生产实现覆盖掉（本 harness 第一版就踩了这个）。
  sandbox.send = (m) => sandbox.__onmessage({ data: m });
  return sandbox;
}

// runUntil：推进 worker 的 setTimeout 轮转，直到看到满足条件的消息或超过轮次上限。
// 上限存在的原因是"死循环"要变成一条可诊断的断言失败，而不是让测试挂住。
function runUntil(w, predicate, { maxRounds = 500000 } = {}) {
  const seen = [];
  const harvest = () => { while (w.posted.length) seen.push(...w.takePosted()); };
  harvest();
  let hit = seen.find(predicate);
  let rounds = 0;
  while (!hit && w.timersPending() > 0 && rounds++ < maxRounds) {
    w.runNextTimer();
    harvest();
    hit = seen.find(predicate);
  }
  return { hit, seen, rounds };
}

const vectors = JSON.parse(readFileSync(VECTORS_PATH, "utf8"));

test("摘要向量：worker 的 SHA-256 与 Go 侧逐条一致", () => {
  const api = makeWorker(readFileSync(WORKER_PATH, "utf8")).epochaPow;
  assert.ok(api && typeof api.sha256Hex === "function", "worker 没有暴露 epochaPow.sha256Hex");
  for (const dv of vectors.digest_vectors) {
    const bytes = new Uint8Array(Buffer.from(dv.input_b64url, "base64"));
    assert.equal(api.sha256Hex(bytes), dv.sha256_hex, `摘要不一致：${dv.why}`);
  }
});

test("前导零计数：与 internal/pow.LeadingZeroBits 同义（字节序、高位优先）", () => {
  const api = makeWorker(readFileSync(WORKER_PATH, "utf8")).epochaPow;
  assert.ok(vectors.leading_zero_bit_vectors?.length, "向量文件缺 leading_zero_bit_vectors");
  for (const z of vectors.leading_zero_bit_vectors) {
    const bytes = new Uint8Array(Buffer.from(z.bytes_b64url, "base64"));
    assert.equal(api.leadingZeroBits(bytes), z.leading_zero_bits,
      `leadingZeroBits 不一致：${z.why}（期望由 Go 实现生成，不是手算）`);
  }
});

test("PoW：worker 解出的第一个解与共享向量一致（含尝试次数）", () => {
  for (const pv of vectors.pow_vectors) {
    const w = makeWorker(readFileSync(WORKER_PATH, "utf8"));
    w.send({
      type: "solve", job: 1, algo: vectors.algorithm, seed: pv.seed_b64url,
      bits: pv.bits, maxTries: 5000000, chunk: 500,
    });
    const { hit, seen, rounds } = runUntil(w, (m) => m.type === "solved" || m.type === "failed");
    assert.ok(hit, `worker 没有在轮次上限内给出结论（bits=${pv.bits}，轮次=${rounds}）：${JSON.stringify(seen.slice(-3))}`);
    assert.equal(hit.type, "solved", `PoW 失败：${JSON.stringify(hit)}`);
    assert.equal(hit.nonce, pv.first_solution_nonce, "第一个解与共享向量不一致（JS/Go 分叉）");
    assert.equal(hit.tries, pv.attempts, "尝试次数与共享向量不一致");
  }
});

test("非法输入一律明确失败，不猜实现", () => {
  const cases = [
    [{ algo: "sha512-prefix", seed: "AAECAwQFBgcICQoLDA0ODw", bits: 12 }, "unknown-algo"],
    [{ algo: "sha256-prefix", seed: "AAECAwQFBgcICQoLDA0ODw", bits: 30 }, "bits-out-of-range"],
    [{ algo: "sha256-prefix", seed: "AAAA", bits: 12 }, "bad-seed"],
  ];
  for (const [payload, want] of cases) {
    const w = makeWorker(readFileSync(WORKER_PATH, "utf8"));
    w.send({ type: "solve", job: 1, ...payload });
    const got = w.takePosted().find((m) => m.type === "failed");
    assert.ok(got, `应给出 failed 消息：${JSON.stringify(payload)}`);
    assert.equal(got.reason, want);
  }
});

test("取消：cancel 之后不得再出现该 job 的 solved", () => {
  const pv = vectors.pow_vectors[0];
  const w = makeWorker(readFileSync(WORKER_PATH, "utf8"));
  // 难度选 bits=24（期望 1600 万次）配 500 的短 chunk：这一 job 在测试里**必然算不完**，
  // 于是"取消是否生效"不依赖第一个解出现在第几轮（上一版就是被这点骗了）。
  w.send({ type: "solve", job: 7, algo: vectors.algorithm, seed: pv.seed_b64url, bits: 24, maxTries: 5000000, chunk: 500 });
  w.runNextTimer();
  w.send({ type: "cancel", job: 7 });
  const { hit, seen } = runUntil(w, (m) => m.type === "cancelled" || m.type === "solved");
  assert.ok(hit, "cancel 之后没有任何结论");
  assert.equal(hit.type, "cancelled", "取消没有立刻生效");
  assert.ok(!seen.some((m) => m.type === "solved" && m.job === 7), "取消之后还解出来了");
});

test("内嵌副本不漂移：v1.js 里的 String.raw 块 == v1.worker.js", () => {
  const js = readFileSync(WIDGET_PATH, "utf8");
  const marker = "var POW_WORKER_SOURCE = String.raw";
  const i = js.indexOf(marker);
  assert.ok(i >= 0, "v1.js 里找不到内嵌标记");
  const rest = js.slice(i + marker.length);
  const open = rest.indexOf("`");
  const close = rest.indexOf("`", open + 1);
  assert.ok(open >= 0 && close > open, "内嵌模板字符串形态不对");
  const embedded = rest.slice(open + 1, close);
  const file = readFileSync(WORKER_PATH, "utf8").replace(/\n$/, "");
  assert.equal(embedded, file, "Blob 路径与同源路径会跑到两份不同代码上");
});

test("预算耗尽是可诊断的失败，不是无限循环", () => {
  const pv = vectors.pow_vectors[0];
  const w = makeWorker(readFileSync(WORKER_PATH, "utf8"));
  w.send({ type: "solve", job: 9, algo: vectors.algorithm, seed: pv.seed_b64url, bits: 24, maxTries: 4000, chunk: 1000 });
  const { hit } = runUntil(w, (m) => m.type === "failed" || m.type === "solved");
  assert.ok(hit, "预算上限没有产生结论");
  assert.equal(hit.reason, "exhausted", "超出 maxTries 应报 exhausted");
});
