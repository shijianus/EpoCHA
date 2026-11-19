// 真实浏览器交互验收（阶段 4A 必需项）。
//
// 驱动方式：本机 Firefox 的 WebDriver BiDi（`--remote-debugging-port`）+ Node 内置 WebSocket。
// **不是截图**：截图看不到点击与输入。这里的交互是真实键盘/鼠标事件（input.performActions），
// 断言读取的是真实 DOM（含 shadow root）。
//
// 已知答案从哪来：`cmd/devseed` 往**本验收脚本自己启动的服务**的库里注入它新生成的题，
// 答案表只落在临时目录。题面图片按**字节**匹配到答案，不靠文件名（文件名与答案的对应关系
// 本身就是一种出口）。生产 API/页面没有任何"返回答案"的路径。
//
// 环境变量（**全部缺失即抛错退出，不静默跳过**）：
//   BIDI_WS                ws://127.0.0.1:PORT/session
//   EPOCHA_API             http://127.0.0.1:PORT       epocha 服务
//   EXAMPLE_URL            http://127.0.0.1:PORT/      exampleapp，CSP=blob
//   EXAMPLE_NOWORKER_URL   http://127.0.0.1:PORT/      exampleapp，CSP worker-src 'none'
//   SEED_JSON              devseed 答案表；SEED_DIR 它的 PNG 目录
//   SITE_KEY               演示站点 site_key
//   EXAMPLE_HARD_URL       http://127.0.0.1:PORT/      exampleapp，bits=20 站点
//   HARD_SITE_KEY          高难度演示站点 site_key
//   EXAMPLE_WORKERFILE_URL http://127.0.0.1:PORT/      exampleapp，CSP worker-src 'self'（方案 B）
//   EXAMPLE_TTL_URL        http://127.0.0.1:PORT/      exampleapp，response_ttl=2s 站点
//   TTL_SITE_KEY           短 TTL 演示站点 site_key
//
// 为什么这些是硬要求而不是 skip：一条**必需**交互因为编排没给齐变量就"跳过并算过"，
// 比直接失败更危险（门禁会绿在错误的原因上）。
//
// 本文件现在**没有任何 t.skip**：375px 窄屏原先被记成"BiDi 不提供视口命令"而 BLOCKED，
// 实际是客户端只试过 `browsingContext.resize` / `emulation.setViewport` 两个不存在的命令，
// W3C 规范的 `browsingContext.setViewport` 在 Firefox 147.0.2 上真实生效（innerWidth 实测 375/414）。
// 所以窄屏那几条是**执行**的用例，不是待办。若将来再出现真正的外部条件不足，必须用 t.skip
// 表达并由 stage4b.sh 换算成 BLOCKED，不许用"直接 return"冒充通过。

import test, { before, beforeEach, after, afterEach } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { BiDi } from "./bidi.mjs";
import { BROWSER_CASE_TITLES } from "./cases.mjs";
import { createWidgetCaseFixture } from "./widget_fixture.mjs";
import { assertImageDimensions } from "./image-dimensions.mjs";

const caseTitle = (id) => {
  const title = BROWSER_CASE_TITLES[id];
  if (!title) throw new Error(`browser manifest missing ${id}`);
  return title;
};

const need = (name) => {
  const v = process.env[name];
  if (!v) throw new Error(`缺少环境变量 ${name}（浏览器验收由 docs/scripts/stage4a.sh 编排）`);
  return v;
};
const BIDI_WS = need("BIDI_WS");
const SERVER_BIN = need("WIDGET_SERVER_BIN");
const DEVSEED_BIN = need("WIDGET_DEVSEED_BIN");
const EXAMPLEAPP_BIN = need("WIDGET_EXAMPLEAPP_BIN");
const CASE_PARENT = need("WIDGET_CASE_PARENT");

let EPOCHA_API;
let EXAMPLE_URL;
let NOWORKER_URL;
let SEED_JSON;
let SEED_DIR;
let SITE_KEY;
let HARD_URL;
let HARD_SITE_KEY;
let SLOW_SITE_KEY;
let WORKERFILE_URL;
let TTL_URL;
let TTL_SITE_KEY;

let seeds;
let answerByImageB64;
let FIXTURE_URL;
function applyFixture(fixture) {
  EPOCHA_API = fixture.EPOCHA_API;
  EXAMPLE_URL = fixture.EXAMPLE_URL;
  NOWORKER_URL = fixture.EXAMPLE_NOWORKER_URL;
  SEED_JSON = fixture.SEED_JSON;
  SEED_DIR = fixture.SEED_DIR;
  SITE_KEY = fixture.SITE_KEY;
  HARD_URL = fixture.EXAMPLE_HARD_URL;
  HARD_SITE_KEY = fixture.HARD_SITE_KEY;
  SLOW_SITE_KEY = fixture.SLOW_SITE_KEY;
  WORKERFILE_URL = fixture.EXAMPLE_WORKERFILE_URL;
  TTL_URL = fixture.EXAMPLE_TTL_URL;
  TTL_SITE_KEY = fixture.TTL_SITE_KEY;
  seeds = JSON.parse(readFileSync(SEED_JSON, "utf8"));
  if (!Array.isArray(seeds) || seeds.length === 0) throw new Error("答案表为空");
  answerByImageB64 = new Map();
  for (const s of seeds) {
    if (!s.png || !s.answer) continue;
    answerByImageB64.set(readFileSync(path.join(SEED_DIR, s.png)).toString("base64"), s.answer);
  }
  FIXTURE_URL = EXAMPLE_URL.replace(/\/+$/, "") + "/fixture";
}

const WIDGET_HOST = "#widget [data-epocha-widget]";
// 辅助函数默认服务首页（容器 #widget）；fixture 页的容器是 #captcha，所以把容器选择器做成参数。
// 刻意不让用例各自拼 shadow 选择器：shadow DOM 的进入方式只该有一处实现。
const hostOf = (container) => `${container} [data-epocha-widget]`;
let bidi;
const openTabs = [];

// 预加载脚本：把 shadow DOM 里状态区的每次变化记进 window.__statusLog。
// PoW 的耗时（worker 上报的 ms）就藏在那段文本里，事后读不到，必须在页面里当场记录。
const preloadFor = (container) => `()=>{
  window.__statusLog = [];
  const tick = () => {
    const host = document.querySelector(${JSON.stringify(hostOf(container))});
    if (!host || !host.shadowRoot) { setTimeout(tick, 25); return; }
    const st = host.shadowRoot.querySelector('[role=status]');
    if (!st || window.__observing) { setTimeout(tick, 25); return; }
    window.__observing = true;
    const push = () => window.__statusLog.push({
      text: st.textContent, kind: st.getAttribute('data-kind'), t: Math.round(performance.now()) });
    push();
    new MutationObserver(push).observe(st, { childList: true, characterData: true, subtree: true });
  };
  tick();
}`;

async function openPage(url, { viewport = null, container = "#widget" } = {}) {
  const c = await bidi.newTab();
  openTabs.push(c);
  // 视口必须在导航**之前**设：页面加载时的布局、图片解码尺寸都按当时的视口算。
  // 加载完再 resize 虽然也会重排，但那测的是"重排后的样子"，不是"窄屏进来时的样子"。
  if (viewport) await setViewportChecked(c, ...viewport);
  const pre = await bidi.send("script.addPreloadScript", { functionDeclaration: preloadFor(container) }).catch(() => null);
  await bidi.goto(c, url);
  await bidi.waitUntil(c, `!!document.querySelector(${JSON.stringify(hostOf(container))})`,
    { timeoutMs: 20000, label: "widget 容器出现" });
  if (pre && pre.script) await bidi.send("script.removePreloadScript", { script: pre.script }).catch(() => {});
  return c;
}

// setViewportChecked：调规范的 browsingContext.setViewport，然后**读回浏览器自己的度量**
// 来证明它真的生效。命令返回 {} 不等于视口变了；只相信启动参数或返回值就是"冒充已验证"。
// width 用 CSS px，断言 window.innerWidth 与 documentElement.clientWidth 两处一致。
async function setViewportChecked(c, width, height, dpr = null) {
  await bidi.setViewport(c, width, height, dpr);
  const m = await bidi.viewportMetrics(c);
  assert.equal(m.innerWidth, width,
    `setViewport(${width}×${height}) 之后 innerWidth 实得 ${m.innerWidth} ⇒ 视口没真的设进去，窄屏结论无效：${JSON.stringify(m)}`);
  assert.equal(m.clientWidth, width,
    `clientWidth 实得 ${m.clientWidth}，与 innerWidth 不一致（滚动条占位差异需单独判断）：${JSON.stringify(m)}`);
  assert.ok(m.innerHeight >= height - 40,
    `innerHeight 实得 ${m.innerHeight}，远低于请求的 ${height}（高方向没生效？）`);
  return m;
}

// 返回**对象**的表达式一律走 evaluateJSON：BiDi 对对象给句柄而不是内联值（见 bidi.mjs）。
const STATE_PROBE = `(()=>{const ids=window.EpoCHA?EpoCHA.ids():[];
  return {count:ids.length, ids, states:ids.map(i=>EpoCHA.state(i)), tokens:ids.map(i=>EpoCHA.getResponse(i))};})()`;
const probeState = (c) => bidi.evaluateJSON(c, STATE_PROBE);

async function waitFirstState(c, want, timeoutMs = 30000, container = "#widget") {
  try {
    await bidi.waitUntil(c,
    `(()=>{const host=document.querySelector(${JSON.stringify(hostOf(container))});
      const id=host&&host.getAttribute('data-epocha-widget');
      return !!id && !!window.EpoCHA && EpoCHA.state(id)===${JSON.stringify(want)};})()`,
      { timeoutMs, label: `${container} 实例进入 ${want}` });
  } catch (e) {
    const why = await bidi.evaluateJSON(c, `(()=>{const h=document.querySelector(${JSON.stringify(hostOf(container))});
      const ids=window.EpoCHA?EpoCHA.ids():'NO_EPOCHA';
      return {status: h&&h.shadowRoot ? h.shadowRoot.querySelector('[role=status]').textContent : '组件没挂载',
        ids, states: Array.isArray(ids) ? ids.map(i=>EpoCHA.state(i)) : null,
        hosts: document.querySelectorAll('[data-epocha-widget]').length,
        url: location.href};})()`).catch((err) => ({probeError: String(err)}));
    throw new Error(`${e.message}（现场：${JSON.stringify(why)}）`);
  }
  return bidi.evaluateJSON(c, STATE_PROBE);
}

async function currentImageB64(c, container = "#widget") {
  return bidi.evaluate(c, `(()=>{const h=document.querySelector(${JSON.stringify(hostOf(container))});
    const img=h&&h.shadowRoot&&h.shadowRoot.querySelector('img');
    return img&&img.src?img.src.split('base64,')[1]:null;})()`);
}

async function answerOfCurrent(c, container = "#widget") {
  const b64 = await currentImageB64(c, container);
  assert.ok(b64, "页面上没有可读取的题目图片");
  const a = answerByImageB64.get(b64);
  assert.ok(a, "当前显示的题目不在受控注入集合里（注入不足或来源被污染）");
  return { answer: a, imageB64: b64 };
}

async function typeAnswer(c, text, container = "#widget") {
  await bidi.evaluate(c, `document.querySelector(${JSON.stringify(hostOf(container))})
    .shadowRoot.querySelector('input.ans').focus()`);
  await bidi.send("input.performActions", {
    context: c,
    actions: [{
      type: "key", id: "kk" + Math.random().toString(36).slice(2, 6),
      actions: [...text].flatMap((ch) => [{ type: "keyDown", value: ch }, { type: "keyUp", value: ch }]),
    }],
  });
}

async function statusText(c, container = "#widget") {
  return bidi.evaluate(c, `(()=>{const h=document.querySelector(${JSON.stringify(hostOf(container))});
    const s=h&&h.shadowRoot&&h.shadowRoot.querySelector('[role=status]');
    return s?s.textContent:null;})()`);
}

// 动画控件（暂停/重播/记录时机，6a4e5a8 起随 parallax/bank-shot 加入）排在提交/换一张
// 之前，静态题下不可见（rect 0×0）——按元素次序定位会把真实点击打到 (0,0)。
// 按可见文案找到按钮后直接取中心坐标真实点击：不用 nth-of-type，因为各按钮
// 分属 shadow root 里不同的容器，:nth-of-type 的计数范围与全局序号对不上。
// 示例页面语言固定 zh-CN，文案匹配是稳定的。
async function widgetButtonCenter(c, which, container = "#widget") {
  const labels = which === "submit" ? ["提交答案", "Submit"] : ["换一张", "New image"];
  const pos = await bidi.evaluateJSON(c, `(()=>{
    const host=document.querySelector(${JSON.stringify(hostOf(container))});
    const btns=host&&host.shadowRoot?[...host.shadowRoot.querySelectorAll("button")]:[];
    const el=btns.find((b)=>${JSON.stringify(labels)}.includes(b.textContent.trim()));
    if(!el) return null;
    el.scrollIntoView({block:"center"});
    const r=el.getBoundingClientRect();
    return [Math.round(r.left+r.width/2), Math.round(r.top+r.height/2)];
  })()`, true);
  if (!pos) throw new Error(`shadow DOM 里找不到按钮：${labels.join(" / ")}`);
  return pos;
}

async function clickWidgetButton(c, which, container = "#widget") {
  // which: "submit" | "refresh" —— 按可见文案定位（见 widgetButtonCenter），然后**真实鼠标点击**。
  const pos = await widgetButtonCenter(c, which, container);
  return bidi.pointerClick(c, pos[0], pos[1]);
}

// 窄屏布局度量表达式：全部在**真实窄视口**下取。
// 每一项回答的是"用户在这个宽度上能不能看见、能不能摸到"，而不是"CSS 里写了 max-width"。
// 命中测试用 shadowRoot.elementFromPoint：document 级的 elementFromPoint 会把 shadow 内容
// 重定向成宿主元素，那样"可命中"就退化成"宿主页面上有个块"，测不到里面的控件。
const NARROW_PROBE = `(()=>{
  const host=document.querySelector(${JSON.stringify(WIDGET_HOST)});
  const sr=host&&host.shadowRoot; if(!sr) return {noShadow:true};
  const vw=window.innerWidth, vh=window.innerHeight;
  const box=sr.querySelector('.box'), img=sr.querySelector('img'),
        inp=sr.querySelector('input.ans'), btns=[...sr.querySelectorAll('button')].filter(b=>['提交答案','换一张','Submit','New image'].includes(b.textContent.trim()));
  const R=(el)=>{ if(!el) return null; const r=el.getBoundingClientRect();
    return {x:Math.round(r.x),y:Math.round(r.y),w:Math.round(r.width),h:Math.round(r.height),
      right:Math.round(r.right),bottom:Math.round(r.bottom),
      hit:(r.width>0&&r.height>0)?(sr.elementFromPoint(r.left+r.width/2, r.top+r.height/2)===el):false};};
  const imgs = img ? {natural:[img.naturalWidth,img.naturalHeight],
                      rendered:[Math.round(img.getBoundingClientRect().width),Math.round(img.getBoundingClientRect().height)],
                      complete:img.complete} : null;
  const st=sr.querySelector('[role=status]');
  return {vw,vh,
    box:R(box), input:R(inp), buttons:btns.map(R), img:imgs,
    docScroll:document.documentElement.scrollWidth, bodyScroll:document.body.scrollWidth,
    statusOverflow:st?{sw:st.scrollWidth,cw:st.clientWidth,text:st.textContent}:null};})()`;

async function listCount() {
  const res = await fetch(EXAMPLE_URL.replace(/\/+$/, "") + "/guestbook", { cache: "no-store" });
  if (!res.ok) throw new Error("读留言列表失败：" + res.status);
  return (await res.json()).count;
}

function flipChar(answer) {
  const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789";
  const cur = answer[0];
  const next = charset[(charset.indexOf(cur) + 1) % charset.length];
  return next + answer.slice(1);
}

function parsePowMs(text) {
  if (!text) return null;
  const m = String(text).match(/（([\d,]+)\s*ms）/);
  return m ? Number(m[1].replace(/,/g, "")) : null;
}

// 用 **root hook**（`before`/`after`）而不是 `test.before`/`test.after`：
// 后者在 node:test 里是"每个用例前后各跑一次"，而 Firefox 的远程端点只允许**一个**活动会话，
// 第二个 `session.new` 会永远挂住。那种挂起看起来像"浏览器坏了"，其实是用例自己把会话抢没了。
before(async () => {
  bidi = await new BiDi(BIDI_WS).connect();
  await bidi.startSession();
});

let caseFixture;
beforeEach(async (t) => {
  caseFixture = await createWidgetCaseFixture({
    testName: t.name, parentDir: CASE_PARENT, serverBin: SERVER_BIN,
    devseedBin: DEVSEED_BIN, exampleappBin: EXAMPLEAPP_BIN, signal: t.signal,
  });
  applyFixture(caseFixture);
});

afterEach(async () => {
  try {
    for (const c of openTabs.splice(0)) await bidi.closeTab(c);
  } finally {
    await caseFixture?.close();
    caseFixture = null;
  }
});

after(async () => {
  if (!bidi) return;
  try { await bidi.send("session.end", {}).catch(() => {}); }
  finally { bidi.close(); }
});

test("闭环：显示图片 → 输入 → 浏览器内 PoW → redeem → 业务表单 → 服务端写入", async () => {
  const c = await openPage(EXAMPLE_URL);
  const s = await waitFirstState(c, "ready");
  assert.equal(s.count, 1, "页面应只自动创建一个组件实例");
  const { answer } = await answerOfCurrent(c);

  const dims = await bidi.evaluateJSON(c, `(()=>{const h=document.querySelector(${JSON.stringify(WIDGET_HOST)});
    const img=h.shadowRoot.querySelector('img');
    const r=img.getBoundingClientRect();
    return {natural:[img.naturalWidth,img.naturalHeight],
      attributes:[Number(img.getAttribute('width')),Number(img.getAttribute('height'))],
      rendered:[r.width,r.height], alt:img.alt};})()`);
  assertImageDimensions(dims);
  assert.ok(!dims.alt.includes(answer), `alt 里出现了答案：${dims.alt}`);

  // 未验证就提交：业务后端拒绝，且不写入 —— 这就是"前端 onSuccess 不是授权"的浏览器侧证据。
  const countBefore = await listCount();
  await bidi.evaluate(c, `document.getElementById('text').value='未验证也想提交'`);
  await bidi.click(c, "#send");
  await bidi.waitUntil(c, `document.getElementById('status').textContent.includes('epocha-missing-response')`,
    { timeoutMs: 10000, label: "无 token 提交被业务后端拒绝" });
  assert.equal(await listCount(), countBefore, "没有 token 竟然写入了留言");

  await typeAnswer(c, answer);
  const t0 = Date.now();
  await clickWidgetButton(c, "submit");
  await waitFirstState(c, "verified");
  const wallMs = Date.now() - t0;

  const tokens = (await probeState(c)).tokens;
  assert.ok(typeof tokens[0] === "string" && tokens[0].length > 20, "组件没有拿到 response token");

  await bidi.evaluate(c, `document.getElementById('text').value='真实闭环留言'`);
  await bidi.click(c, "#send");
  await bidi.waitUntil(c, `document.getElementById('status').textContent.includes('留言已写入')`,
    { timeoutMs: 12000, label: "业务后端写入成功" });
  assert.equal(await listCount(), countBefore + 1);

  // PoW 真的在浏览器里算过（状态区出现过带次数的进度文本），顺带留下时延实测。
  const log = await bidi.evaluateJSON(c, `window.__statusLog.map(x=>x.text)`);
  const progress = log.filter((t) => t && t.includes("已尝试"));
  const powMs = parsePowMs(progress[progress.length - 1]);
  // bits=12 时一次求解常在进度节流阈值（5 万次）之前完成 ⇒ "进度上报 0 次"是预期，不是没算。
  // 单次求解耗时与吞吐由浏览器侧的独立测量给出（docs/STAGE4A.md §4），这里只记墙钟。
  console.log(`  [实测] 浏览器 PoW（bits=12）：点击提交→verified 墙钟 = ${wallMs} ms；`
    + `进度上报 ${progress.length} 次（0 次属预期：求解快于 5 万次的进度节流阈值），解析出的耗时 = ${powMs} ms`);
});

test("答错即终结：同一题不能重试；换题拿到的是另一道题", async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  const { answer, imageB64 } = await answerOfCurrent(c);

  await typeAnswer(c, flipChar(answer));
  await clickWidgetButton(c, "submit");
  await waitFirstState(c, "error");
  const st = await probeState(c);
  assert.equal(st.tokens[0], "", "答错之后组件还留着可用 token");
  assert.match(await statusText(c), /已经不可用|换一张/);

  // 答错的那道题不能再被兑换：直接拿同一个 token 重放，服务端必然拒绝。
  const replay = await fetch(EPOCHA_API + "/api/redeem?site_key=" + encodeURIComponent(SITE_KEY), {
    method: "POST", headers: { "Content-Type": "application/json" }, credentials: "omit",
    body: JSON.stringify({ token: st.tokens[0] || "", pow_nonce: "1", answer: answer }),
  });
  assert.notEqual(replay.status, 200, "答错的题居然还能兑换");

  await clickWidgetButton(c, "refresh");
  await waitFirstState(c, "ready");
  assert.notEqual(await currentImageB64(c), imageB64, "换题后拿到的还是同一道题");
});

test("一次性：同一个 token 重复提交必然被服务端拒绝", async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  const { answer } = await answerOfCurrent(c);
  await typeAnswer(c, answer);
  await clickWidgetButton(c, "submit");
  await waitFirstState(c, "verified");

  const countBefore = await listCount();
  await bidi.evaluate(c, `document.getElementById('text').value='重放测试留言'`);
  await bidi.click(c, "#send");
  await bidi.waitUntil(c, `document.getElementById('status').textContent.includes('留言已写入')`,
    { timeoutMs: 12000, label: "首次提交写入" });
  assert.equal(await listCount(), countBefore + 1);

  await bidi.click(c, "#resend");
  await bidi.waitUntil(c, `document.getElementById('status').textContent.includes('epocha-duplicate-response')`,
    { timeoutMs: 12000, label: "重放被拒绝" });
  assert.equal(await listCount(), countBefore + 1, "重放竟然又写了一条");
});

test("多实例隔离 + 隐藏字段：各自领题、各自写字段、销毁不串台", async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");

  await bidi.evaluate(c, `(()=>{
    const f = document.createElement('form'); f.id='f2'; document.body.appendChild(f);
    const d = document.createElement('div'); d.id='w2'; f.appendChild(d);
    window.__id2 = EpoCHA.render(d, { apiBase: ${JSON.stringify(EPOCHA_API)}, siteKey: ${JSON.stringify(SITE_KEY)},
      language: 'zh-CN', fieldName: 'epocha_b' });
    const g = document.createElement('form'); g.id='f3'; document.body.appendChild(g);
    const e = document.createElement('div'); e.id='w3'; g.appendChild(e);
    window.__id3 = EpoCHA.render(e, { apiBase: ${JSON.stringify(EPOCHA_API)}, siteKey: ${JSON.stringify(SITE_KEY)},
      language: 'en', fieldName: 'epocha_b' });   // 与 __id2 **同名**字段，落在不同 form 里
    return [window.__id2, window.__id3];})()`);
  await bidi.waitUntil(c, `EpoCHA.state(window.__id2)==='ready' && EpoCHA.state(window.__id3)==='ready'`,
    { timeoutMs: 30000, label: "两个附加实例进入 ready" });

  const imgs = await bidi.evaluateJSON(c, `(()=>[...document.querySelectorAll('[data-epocha-widget]')]
    .map(h=>h.shadowRoot.querySelector('img')).filter(Boolean).map(i=>i.src.split('base64,')[1]))()`);
  assert.equal(imgs.length, 3, "三个容器应有三张题图");
  assert.equal(new Set(imgs).size, 3, `三实例拿到了重复的题目：${imgs.map(s => s && s.slice(0, 6))}`);

  // 隐藏字段只在**各自实例的 light DOM**里（Shadow DOM 里的 input 不参与表单提交）。
  const fields = await bidi.evaluateJSON(c, `(()=>[...document.querySelectorAll('input[type=hidden][data-epocha-field]')]
    .map(f=>({name:f.name, owner:f.getAttribute('data-epocha-field'), form:f.form?f.form.id:null})))()`);
  assert.equal(fields.length, 2, `只应有两个配了字段的附加实例：${JSON.stringify(fields)}`);
  assert.deepEqual(fields.map(f => f.name).sort(), ["epocha_b", "epocha_b"], "字段名应各自等于配置值");
  assert.equal(new Set(fields.map(f => f.owner)).size, 2, "两个字段必须分属不同实例（不是互相覆盖）");
  assert.notEqual(fields[0].form, fields[1].form, "同名字段应落在不同 form 里，各自提交各自的");

  // 其中一个销毁：自己的字段消失，另一个不受影响。
  await bidi.evaluate(c, `EpoCHA.destroy(window.__id3)`);
  const afterState = await bidi.evaluateJSON(c, `(()=>({
    fields:[...document.querySelectorAll('input[type=hidden][data-epocha-field]')].map(f=>f.getAttribute('data-epocha-field')),
    hosts:document.querySelectorAll('[data-epocha-widget]').length,
    ids:EpoCHA.ids().length}))()`);
  assert.deepEqual(afterState.fields, [await bidi.evaluate(c, `window.__id2`)], "只应剩下 __id2 的字段");
  assert.equal(afterState.hosts, 2, "销毁后容器数");
  assert.equal(afterState.ids, 2, "注册表应同步减少");
  assert.equal(await bidi.evaluate(c, `EpoCHA.getResponse(window.__id3)`), "", "销毁后不该还能取到 token");
});

test("晚到的旧响应不能覆盖新题（刷新换代）", async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  const firstImage = await currentImageB64(c);

  // 让"下一个 challenge 响应"延迟 1.5s，然后连点两次换题：
  // 第二次先回来，第一次晚到。若按到达顺序渲染，界面就会退回旧题。
  await bidi.evaluate(c, `(()=>{
    window.__seen=[];
    const orig=window.fetch;
    window.__slowNext=true;
    window.fetch=function(url,opts){
      const p=orig.call(this,url,opts);
      if(String(url).includes('/api/challenge')){
        p.then(r=>r.clone().json()).then(j=>{if(j&&j.question)window.__seen.push(j.question.image_base64);}).catch(()=>{});
        if(window.__slowNext){window.__slowNext=false;return new Promise(res=>setTimeout(()=>res(p),1500));}
      }
      return p;};
  })()`);
  // 请求 A 走真实按钮（被延迟）；请求 B 走**公开 API** ——
  // loading 期间按钮被禁用是正确的产品行为，用它造不出两个重叠请求。
  await clickWidgetButton(c, "refresh");
  await new Promise((r) => setTimeout(r, 150));
  await bidi.evaluate(c, `EpoCHA.refresh(EpoCHA.ids()[0])`);     // 请求 B（正常速度）
  await waitFirstState(c, "ready");
  await new Promise((r) => setTimeout(r, 2000)); // 等 A 晚到

  const seen = await bidi.evaluateJSON(c, `window.__seen`);
  assert.ok(seen.length >= 2, `应观察到两次领题响应，实得 ${seen.length}`);
  const shown = await currentImageB64(c);
  assert.notEqual(shown, firstImage, "换题后仍是同一道题");
  assert.equal(shown, seen[seen.length - 1], "显示的不是最后一次请求的响应 ⇒ 晚到的旧响应覆盖了新题");
  assert.notEqual(shown, seen[0], "显示的是被延迟的那次旧响应");
});

test("reset 与 destroy：清空 token/输入、移除 DOM 与隐藏字段、注销注册表", async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  const id = (await probeState(c)).ids[0];

  await bidi.evaluate(c, `EpoCHA.reset(${JSON.stringify(id)})`);
  const st = await probeState(c);
  assert.equal(st.states[0], "idle", "reset 之后状态应是 idle");
  assert.equal(st.tokens[0], "", "reset 之后 token 必须清空");
  assert.equal(await bidi.evaluate(c, `document.querySelector(${JSON.stringify(WIDGET_HOST)})
    .shadowRoot.querySelector('input.ans').value`), "", "reset 之后输入框应清空");

  await bidi.evaluate(c, `EpoCHA.destroy(${JSON.stringify(id)})`);
  assert.equal(await bidi.evaluate(c, `!!document.querySelector(${JSON.stringify(WIDGET_HOST)})`), false,
    "destroy 之后 shadow host 应移除");
  assert.equal(await bidi.evaluate(c, `document.querySelectorAll('#widget input[type=hidden]').length`), 0,
    "destroy 之后隐藏字段应移除");
  assert.equal(await bidi.evaluate(c, `EpoCHA.ids().length`), 0, "destroy 之后注册表应为空");
});

// Transport-only fault injection: real HTTP, image and PoW still run; release the completed
// response explicitly so cancellation cannot rely on the browser rejecting fetch in time.
async function prepareLifecycleCase(c, path, fetchTimeoutMs = 10000, siteKey = SITE_KEY, holdBody = false) {
  await bidi.evaluate(c, `(()=>{
    EpoCHA.ids().forEach(id=>EpoCHA.destroy(id));
    const form=document.createElement('form'); document.body.appendChild(form);
    const container=document.createElement('div'); container.id='lifecycle-widget'; form.appendChild(container);
    const orig=window.fetch;
    window.__life={calls:[],successes:[],errors:[],states:[],expired:0,held:false,released:false,status:null};
    let hold=true;
    window.fetch=function(url,opts){
      const request={path:new URL(url,location.href).pathname,signal:opts&&opts.signal};
      window.__life.calls.push(request);
      const response=orig.call(this,url,opts);
      if(!hold || request.path!==${JSON.stringify(path)}) return response;
      hold=false;
      if (${holdBody}) {
        return response.then(async res=>{
          const text=await res.text();
          const payload=JSON.parse(text);
          window.__life.status=res.status;
          window.__life.ttlSeconds=Number(payload.expires_in);
          window.__life.heldAt=performance.now();
          const body=new ReadableStream({start(controller){
            window.__life.release=()=>{
              window.__life.released=true;
              window.__life.releasedAt=performance.now();
              controller.enqueue(new TextEncoder().encode(text));
              controller.close();
            };
          }});
          window.__life.held=true;
          return new Response(body,{status:res.status,headers:res.headers});
        });
      }
      return new Promise((resolve,reject)=>{
        response.then(async res=>{
          const text=await res.text();
          window.__life.status=res.status;
          window.__life.release=()=>{
            window.__life.released=true;
            resolve(new Response(text,{status:res.status,headers:res.headers}));
          };
          window.__life.held=true;
        }).catch(reject);
      });
    };
    window.__life.id=EpoCHA.render(container,{
      apiBase:${JSON.stringify(EPOCHA_API)},siteKey:${JSON.stringify(siteKey)},language:'en',
      fetchTimeoutMs:${fetchTimeoutMs},
      onSuccess:token=>window.__life.successes.push(token),
      onError:err=>window.__life.errors.push(err.code),
      onState:state=>window.__life.states.push(state),
      onExpired:()=>window.__life.expired++
    });
  })()`);
}

const lifecycleProbe = (c) => bidi.evaluateJSON(c, `(()=>{
  const w=window.__life, id=w.id;
  const host=document.querySelector('#lifecycle-widget [data-epocha-widget]');
  const field=document.querySelector('#lifecycle-widget input[type=hidden]');
  const buttons=host ? [...host.shadowRoot.querySelectorAll('button')] : [];
  return {state:EpoCHA.state(id),token:EpoCHA.getResponse(id),field:field?field.value:null,
    calls:w.calls.map(r=>({path:r.path,aborted:!!(r.signal&&r.signal.aborted)})),
    successes:w.successes,errors:w.errors,refreshDisabled:buttons.find(b=>['换一张','New image'].includes(b.textContent.trim()))?.disabled};
})()`);

async function waitHeldResponse(c) {
  await bidi.waitUntil(c, `window.__life.held`, { timeoutMs: 20000, label: "真实 HTTP 响应已被测试传输层暂存" });
  assert.equal(await bidi.evaluate(c, `window.__life.status`), 200, "必须暂存真实成功响应");
}

for (const path of ["/api/challenge", "/api/redeem"]) {
  test(`在途 reset：${path} 晚到的真实响应不恢复题目/token/成功通知`, async () => {
    const c = await openPage(EXAMPLE_URL);
    await waitFirstState(c, "ready");
    await prepareLifecycleCase(c, path);
    if (path === "/api/redeem") {
      await waitFirstState(c, "ready", 30000, "#lifecycle-widget");
      const { answer } = await answerOfCurrent(c, "#lifecycle-widget");
      await typeAnswer(c, answer, "#lifecycle-widget");
      await clickWidgetButton(c, "submit", "#lifecycle-widget");
    }
    await waitHeldResponse(c);
    assert.equal((await lifecycleProbe(c)).state, path === "/api/redeem" ? "submitting" : "loading");
    await bidi.evaluate(c, `EpoCHA.reset(window.__life.id); window.__life.release();`);
    await new Promise((resolve) => setTimeout(resolve, 100));
    const result = await lifecycleProbe(c);
    assert.equal(result.state, "idle");
    assert.equal(result.token, "");
    assert.equal(result.field, "");
    assert.deepEqual(result.successes, []);
    assert.deepEqual(result.errors, []);
    const calls = result.calls.filter((request) => request.path === path);
    assert.equal(calls.length, 1);
    assert.equal(calls[0].aborted, true);
  });
}

test("兑换 HTTP 挂起：超时可换题，迟到的真实成功不能复活 token，也不自动重兑", async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  await prepareLifecycleCase(c, "/api/redeem", 5000);
  await waitFirstState(c, "ready", 30000, "#lifecycle-widget");
  const { answer } = await answerOfCurrent(c, "#lifecycle-widget");
  await typeAnswer(c, answer, "#lifecycle-widget");
  await clickWidgetButton(c, "submit", "#lifecycle-widget");
  await waitHeldResponse(c);
  await waitFirstState(c, "error", 12000, "#lifecycle-widget");
  const timedOut = await lifecycleProbe(c);
  assert.equal(timedOut.token, "");
  assert.equal(timedOut.field, "");
  assert.equal(timedOut.refreshDisabled, false);
  assert.deepEqual(timedOut.errors, ["timeout"]);
  assert.deepEqual(timedOut.successes, []);
  assert.equal(timedOut.calls.find((request) => request.path === "/api/redeem").aborted, true);
  await clickWidgetButton(c, "refresh", "#lifecycle-widget");
  await waitFirstState(c, "ready", 30000, "#lifecycle-widget");
  await bidi.evaluate(c, `window.__life.release()`);
  await new Promise((resolve) => setTimeout(resolve, 100));
  const late = await lifecycleProbe(c);
  assert.equal(late.state, "ready");
  assert.equal(late.token, "");
  assert.equal(late.field, "");
  assert.deepEqual(late.successes, []);
  assert.deepEqual(late.errors, ["timeout"]);
  assert.equal(late.calls.filter((request) => request.path === "/api/redeem").length, 1);
  assert.equal(late.calls.filter((request) => request.path === "/api/challenge").length, 2);
});

test("计算超时是真实约束：预算到点即明确失败，不去兑换", async () => {
  // 为什么用 bits=24 的专用站点，而不是首页那个 bits=20：
  // `powTimeoutMs` 服务端语义上**下限是 1000ms**（`Math.max(v,1000)`），而 bits=20 本机实测
  // 最快一次只有 309ms ⇒ 约 1/3 概率计算会抢在预算前解出来，那时看到的是"答错被终结"
  // 而不是"超时"。原先的写法（还写着 400ms，实际被钳成 1000ms）就是靠运气通过。
  // bits=24 把单次抢跑概率压到 ~2.5%，下面再叠一层**有界重抽**并要求至少观察到一次真实超时。
  // 这是为了让负面路径可判定，**不是**在生产里抬难度：默认站点仍是 bits=12，
  // bits=20 的时延实测由"高难度站点：取消…"那条继续产出。
  const c = await openPage(HARD_URL);          // bits=24 站点的允许来源就是这个页面的 origin
  await waitFirstState(c, "ready");
  const BUDGET_MS = 1000;                       // 明确写成钳位后的真实下限，不留"400"这种幻觉
  const MAX_TRIES = 3;
  const seen = [];
  let timedOut = null;
  for (let attempt = 1; attempt <= MAX_TRIES && !timedOut; attempt++) {
    const host = `wt${attempt}`;
    const id = await bidi.evaluateJSON(c, `(()=>{
      const d=document.createElement('div'); d.id=${JSON.stringify(host)}; document.body.appendChild(d);
      return EpoCHA.render(d, {apiBase: ${JSON.stringify(EPOCHA_API)}, siteKey: ${JSON.stringify(SLOW_SITE_KEY)},
        language:'zh-CN', fieldName:'', powTimeoutMs: ${BUDGET_MS}});
    })()`);
    await bidi.waitUntil(c, `EpoCHA.state(${JSON.stringify(id)})==='ready'`,
      { timeoutMs: 60000, label: `第 ${attempt} 次：预算实例进入 ready` });
    const sel = `#${host} [data-epocha-widget]`;
    await bidi.evaluate(c, `(()=>{const hs=document.querySelector(${JSON.stringify(sel)});
      hs.shadowRoot.querySelector('input.ans').value='ABCDEF';})()`);
    const bpos = await widgetButtonCenter(c, "submit", `#${host}`);
    assert.ok(bpos && (bpos[0] !== 0 || bpos[1] !== 0), `第 ${attempt} 次：预算实例的提交按钮坐标异常（${JSON.stringify(bpos)}）`);
    await bidi.pointerClick(c, bpos[0], bpos[1]);
    await bidi.waitUntil(c, `(()=>{const s=EpoCHA.state(${JSON.stringify(id)});
      return s==='error' || s==='verified';})()`,
      { timeoutMs: 90000, label: `第 ${attempt} 次：预算实例走到终态` });
    const txt = await bidi.evaluate(c, `(()=>{const hs=document.querySelector(${JSON.stringify(sel)});
      return hs.shadowRoot.querySelector('[role=status]').textContent;})()`);
    const st = await bidi.evaluate(c, `EpoCHA.state(${JSON.stringify(id)})`);
    const tok = await bidi.evaluate(c, `EpoCHA.getResponse(${JSON.stringify(id)})`);
    seen.push({ attempt, state: st, text: String(txt).slice(0, 40), tokenLen: String(tok || "").length });
    if (st === "error" && /超时/.test(String(txt))) timedOut = { attempt, txt };
    await bidi.evaluate(c, `EpoCHA.destroy(${JSON.stringify(id)})`);
  }
  assert.ok(timedOut,
    `${MAX_TRIES} 次尝试都没观察到超时分支（每次都在 1000ms 预算内解出来了？）：${JSON.stringify(seen)}`);
  // 超时分支必须同时满足三件事：明确失败、文案说清是超时、**没有留下可兑换的 token**。
  const last = seen[seen.length - 1];
  assert.equal(last.state, "error", `超时后状态应是 error，实得 ${JSON.stringify(seen)}`);
  assert.equal(last.tokenLen, 0, `超时后不该留着 token：${JSON.stringify(seen)}`);
  // 第 2..N 次尝试的存在本身就证明"抢跑"不是失败：只有观察不到超时才算缺陷。
  if (timedOut.attempt > 1) {
    console.log(`  [实测] 超时分支在第 ${timedOut.attempt}/${MAX_TRIES} 次尝试观察到；前几次抢跑：${JSON.stringify(seen.slice(0, -1))}`);
  }
});

test("高难度站点：取消正在进行的计算不会晚到生效", async () => {
  const c = await openPage(HARD_URL);
  await waitFirstState(c, "ready");
  const id = (await probeState(c)).ids[0];
  const { answer } = await answerOfCurrent(c);

  await typeAnswer(c, answer);
  await clickWidgetButton(c, "submit");
  // bits=20 时浏览器要算上百万次，"solving" 必然可被观察到（这同时也是生产难度的时延实测）。
  await bidi.waitUntil(c, `EpoCHA.state(${JSON.stringify(id)})==='solving'`,
    { timeoutMs: 10000, label: "高难度实例进入 solving" });
  const t0 = Date.now();
  await bidi.evaluate(c, `EpoCHA.reset(${JSON.stringify(id)})`);
  await new Promise((r) => setTimeout(r, 1500));
  const st = await probeState(c);
  assert.equal(st.states[0], "idle", "reset 之后应回到 idle");
  assert.equal(st.tokens[0], "", "被取消的计算不得留下 token");
  await new Promise((r) => setTimeout(r, 2000));
  const late = await probeState(c);
  assert.equal(late.states[0], "idle", `被取消的旧计算晚到并改变了状态：${JSON.stringify(late)}`);

  // 再完整跑一次，记录 bits=20 的浏览器侧时延（这是"降低难度掩盖性能"的反例证据）。
  // reset 的语义是"回到 idle 且**不**自动领新题"，所以这里要真实点一次换题按钮。
  await clickWidgetButton(c, "refresh");
  await waitFirstState(c, "ready", 60000);
  const hard = await answerOfCurrent(c);
  await typeAnswer(c, hard.answer);
  const t1 = Date.now();
  await clickWidgetButton(c, "submit");
  await waitFirstState(c, "verified", 180000);
  console.log(`  [实测] bits=20 浏览器侧 PoW + redeem 墙钟 = ${Date.now() - t1} ms（同一台机器 bits=12 的对照见闭环用例）`);
  void t0;
});

test("CSP 禁止 Worker：明确失败，绝不假装通过", async () => {
  const c = await openPage(NOWORKER_URL);
  await bidi.waitUntil(c, `(()=>{const h=document.querySelector(${JSON.stringify(WIDGET_HOST)});
    const s=h&&h.shadowRoot.querySelector('[role=status]');
    return s&&s.getAttribute('data-kind')==='err';})()`, { timeoutMs: 25000, label: "CSP 挡住 worker 后进入错误态" });
  const st = await probeState(c);
  const txt = await statusText(c);
  assert.equal(st.states[0], "error", `应停在 error，实得 ${st.states[0]}（文本：${txt}）`);
  assert.equal(st.tokens[0], "", "被 CSP 挡住时不该有任何 token");
  assert.match(txt, /CSP|Worker/);
  assert.equal(await bidi.evaluate(c, `document.getElementById('status').textContent.includes('留言已写入')`), false);
});

test("键盘可完成：Tab 到达输入框与按钮，Enter 提交", async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  const { answer } = await answerOfCurrent(c);

  await bidi.evaluate(c, `(()=>{const h=document.querySelector('h1');h.setAttribute('tabindex','-1');h.focus();})()`);
  const activeChain = `(()=>{const out=[];let n=document.activeElement;
    while(n){out.push(n.tagName+(typeof n.className==='string'&&n.className?'.'+n.className:''));
      n=n.shadowRoot?n.shadowRoot.activeElement:null;}return out;})()`;
  let reachedInput = false, reachedButton = false;
  for (let i = 0; i < 16 && !(reachedInput && reachedButton); i++) {
    await bidi.send("input.performActions", {
      context: c, actions: [{ type: "key", id: "tab" + i, actions: [{ type: "keyDown", value: "\uE004" }, { type: "keyUp", value: "\uE004" }] }],
    });
    const chain = await bidi.evaluateJSON(c, activeChain);
    if (chain.some((x) => x.includes("INPUT.ans"))) reachedInput = true;
    else if (reachedInput && chain.some((x) => x.startsWith("BUTTON"))) reachedButton = true;
  }
  assert.ok(reachedInput, "Tab 无法到达答案输入框");
  assert.ok(reachedButton, "从输入框继续 Tab 无法到达按钮");

  await typeAnswer(c, answer);
  await bidi.evaluate(c, `document.querySelector(${JSON.stringify(WIDGET_HOST)})
    .shadowRoot.querySelector('input.ans').focus()`);
  await bidi.send("input.performActions", {
    context: c, actions: [{ type: "key", id: "enter", actions: [{ type: "keyDown", value: "\uE007" }, { type: "keyUp", value: "\uE007" }] }],
  });
  await waitFirstState(c, "verified");
});

test("窄屏 375×667：视口真的设成 375，布局不溢出、图片比例正确、控件全部可命中", async () => {
  const c = await openPage(EXAMPLE_URL, { viewport: [375, 667] });
  // openPage 里已断言 innerWidth===375；这里再读一次，确认导航之后仍是 375
  const vp = await bidi.viewportMetrics(c);
  assert.equal(vp.innerWidth, 375, `导航后视口回落到 ${vp.innerWidth} ⇒ 后面所有窄屏结论无效`);
  void vp.devicePixelRatio; // DPR 只记录不假定：headless 下它是被我们设进去的值，不等于真机像素密度
  await waitFirstState(c, "ready");
  const m = await bidi.evaluateJSON(c, NARROW_PROBE);
  assert.equal(m.vw, 375);
  assert.ok(m.docScroll <= m.vw + 1, `文档出现横向滚动：docScroll=${m.docScroll} vw=${m.vw}`);
  assert.ok(m.bodyScroll <= m.vw + 1, `body 横向溢出：${m.bodyScroll}`);
  assert.ok(m.box.right <= m.vw + 1, `组件盒子超出视口右缘：${JSON.stringify(m.box)}`);
  assert.ok(m.box.w > 0 && m.box.h > 0, `组件盒子塌成零尺寸：${JSON.stringify(m.box)}`);
  // 图片：完整显示（宽度不超视口）+ 比例不变形（rendered 比例 ≈ natural 比例）
  assert.ok(m.img && m.img.complete, `题目图片在窄屏下没加载完或不存在：${JSON.stringify(m.img)}`);
  const [nw, nh] = m.img.natural, [rw, rh] = m.img.rendered;
  assert.ok(rw <= m.vw, `图片宽度 ${rw} 超过视口 ${m.vw}（会挤出横向滚动）`);
  const want = nw / nh, got = rw / rh;
  assert.ok(Math.abs(want - got) / want < 0.02,
    `图片比例变形：natural ${nw}×${nh}=${want.toFixed(3)}，rendered ${rw}×${rh}=${got.toFixed(3)}`);
  assert.ok(m.img.rendered[0] > 40 && m.img.rendered[1] > 20,
    `图片被压缩到不可读的尺寸：${JSON.stringify(m.img.rendered)}`);
  // 控件可达：非零尺寸 + 在视口内 + 中心点命中测试拿到的就是它自己（在 shadowRoot 内做命中）
  assert.ok(m.input && m.input.hit && m.input.right <= m.vw + 1,
    `输入框在窄屏下不可命中或超出视口：${JSON.stringify(m.input)}`);
  assert.equal(m.buttons.length, 2, `窄屏下按钮数量不对：${JSON.stringify(m.buttons)}`);
  for (const [i, b] of m.buttons.entries()) {
    assert.ok(b && b.hit && b.right <= m.vw + 1, `第 ${i + 1} 个按钮不可命中或超出视口：${JSON.stringify(b)}`);
  }
});

test("窄屏 375：真实鼠标点击 + 键盘输入走完验证与业务提交", async () => {
  const c = await openPage(EXAMPLE_URL, { viewport: [375, 667] });
  await waitFirstState(c, "ready");
  const { answer } = await answerOfCurrent(c);
  await typeAnswer(c, answer);
  await clickWidgetButton(c, "submit");   // 真实 pointer 动作，坐标按 375 宽算
  await waitFirstState(c, "verified");
  const id = (await probeState(c)).ids[0];
  assert.ok((await bidi.evaluate(c, `EpoCHA.getResponse(${JSON.stringify(id)})`)).length > 20,
    "窄屏下验证没拿到 token");
  // 业务提交也要在 375 下真跑通（这一档页面用 fetch 提交；带字段的原生 form 见 fixture 用例）
  const before = await listCount();
  await bidi.evaluate(c, `document.getElementById('text').value='窄屏 375 真实提交';1`);
  await bidi.click(c, "#send");
  await bidi.waitUntil(c,
    `(async()=>{const r=await fetch('/guestbook',{cache:'no-store'});const j=await r.json();
      return j.count===${before + 1};})()`,
    { timeoutMs: 20000, label: "窄屏下业务写入成功" });
});

test("窄屏 375：错误文案不撑破布局（CSP 禁 Worker 的档位）", async () => {
  const c = await openPage(NOWORKER_URL, { viewport: [375, 667] });
  // 这一档 CSP 是 worker-src 'none'：领题成功后建 worker 必然失败 ⇒ 组件进 error 并写长文案
  await bidi.waitUntil(c,
    `(()=>{const ids=window.EpoCHA?EpoCHA.ids():[];return ids.length>0 && EpoCHA.state(ids[0])==='error';})()`,
    { timeoutMs: 30000, label: "CSP 禁 Worker 时进入 error" });
  const m = await bidi.evaluateJSON(c, NARROW_PROBE);
  assert.equal(m.vw, 375, `错误态下视口不是 375：${JSON.stringify(m).slice(0, 200)}`);
  assert.ok(m.statusOverflow && m.statusOverflow.text.length > 0, "错误文案是空的");
  assert.ok(m.docScroll <= m.vw + 1,
    `错误文案撑破文档宽度：docScroll=${m.docScroll} vw=${m.vw} 文案=${JSON.stringify(m.statusOverflow.text).slice(0, 80)}`);
  assert.ok(m.box.right <= m.vw + 1, `错误态下组件盒超出右缘：${JSON.stringify(m.box)}`);
  assert.ok(m.statusOverflow.sw <= m.statusOverflow.cw + 1,
    `错误文案在状态区内横向溢出（滚动不可达）：sw=${m.statusOverflow.sw} cw=${m.statusOverflow.cw}`);
});

test("窄屏 414×896（较宽手机档）回归：无横向溢出、控件可命中、容器不吃满超宽", async () => {
  const c = await openPage(EXAMPLE_URL, { viewport: [414, 896] });
  await waitFirstState(c, "ready");
  const m = await bidi.evaluateJSON(c, NARROW_PROBE);
  assert.equal(m.vw, 414);
  assert.ok(m.docScroll <= m.vw + 1, `414 档出现横向滚动：${m.docScroll}`);
  assert.ok(m.box.right <= m.vw + 1, `414 档组件超出右缘：${JSON.stringify(m.box)}`);
  assert.ok(m.input && m.input.hit, `414 档输入框不可命中：${JSON.stringify(m.input)}`);
  for (const b of m.buttons) assert.ok(b && b.hit, `414 档按钮不可命中：${JSON.stringify(b)}`);
  // :host 的 max-width:360px 应当让它比视口窄，而不是被拉成 414
  const hostMax = await bidi.evaluate(c, `(()=>{const h=document.querySelector(${JSON.stringify(WIDGET_HOST)});
    return Math.round(h.getBoundingClientRect().width);})()`);
  assert.ok(hostMax <= 361, `容器宽度 ${hostMax} 突破了 :host 的 max-width:360px`);
});

// --- 文档 §1 最小接入示例的可运行 fixture ---
//
// 这一组用例的存在理由：文档示例的两类错误（脚本顺序竞争、容器在 form 外）
// 都只在"真加载 + 真提交"时现形，grep 文档字符串永远测不到。
// fixture 与首页共用同一个 exampleapp 进程与同一份 CSP，所以"示例页能跑"不能替它作证。
const FIXTURE_CONTAINER = "#captcha";

test("文档最小示例 fixture：deferred 脚本顺序确定，init 里 EpoCHA 已定义，字段属于目标 form", async () => {
  await bidi.subscribeEvents(["log.entryAdded"]);
  const c = await openPage(FIXTURE_URL, { container: FIXTURE_CONTAINER });
  await waitFirstState(c, "ready", 30000, FIXTURE_CONTAINER);
  // 走到这里已经证明：容器被挂载 ⇒ init 脚本执行了 ⇒ 它读到了 EpoCHA。
  // 若 widget 脚本没 defer 或顺序反了，ReferenceError 会让容器根本不出现，openPage 直接超时。
  const handle = await bidi.evaluateJSON(c, `(()=>({
    fixture: window.__fixture || null,
    hasEpoCHA: typeof window.EpoCHA === 'object' && typeof (window.EpoCHA||{}).render === 'function',
    inForm: !!(document.querySelector('#captcha') && document.querySelector('#captcha').closest('form') === document.getElementById('order')),
  }))()`);
  assert.ok(handle.hasEpoCHA, "页面上没有 EpoCHA.render");
  assert.ok(handle.fixture && typeof handle.fixture.id === "string", `init 没拿到实例句柄：${JSON.stringify(handle)}`);
  assert.equal(handle.inForm, true, "widget 容器不在 #order 这个 form 内");
  const st = await bidi.evaluate(c, `EpoCHA.state(${JSON.stringify(handle.fixture.id)})`);
  assert.equal(st, "ready", `fixture 上的实例没进入 ready（实得 ${st}）`);
  // 隐藏字段必须**真的属于**目标 form：属于别的 form 或不属于任何 form 都等于没提交。
  const own = await bidi.evaluateJSON(c, `(()=>{
    const f=document.getElementById('order');
    const i=f.querySelector('input[name="epocha_response"]');
    return {fieldInForm: !!i, fieldFormId: i && i.form ? i.form.id : null,
      widgetInsideForm: f.contains(document.querySelector('#captcha'))};})()`);
  assert.deepEqual(own, { fieldInForm: true, fieldFormId: "order", widgetInsideForm: true },
    `字段归属不对：${JSON.stringify(own)}`);
  const errs = (await bidi.consoleErrors()).filter((e) => /EpoCHA|ReferenceError|is not defined/.test(e));
  assert.deepEqual(errs, [], `fixture 加载期出现脚本错误：${JSON.stringify(errs)}`);
});

test("文档最小示例 fixture：FormData 带上 token，原生 form POST 真写入；没验证时同一按钮被服务端拒", async () => {
  // 反面先来：不完成验证直接按原生提交 —— 页面结构完全一样，差别只在 token。
  const cneg = await openPage(FIXTURE_URL, { container: FIXTURE_CONTAINER });
  await waitFirstState(cneg, "ready", 30000, FIXTURE_CONTAINER);
  await bidi.click(cneg, "form#order button[type=submit]");
  await bidi.waitUntil(cneg, `(()=>{const p=document.getElementById('post-result');
    return p && p.getAttribute('data-status')!=='200';})()`,
    { timeoutMs: 20000, label: "未验证提交后拿到结果页" });
  const neg = await bidi.evaluateJSON(cneg, `(()=>{const p=document.getElementById('post-result');
    return {status:p.getAttribute('data-status'), machine:p.getAttribute('data-machine')};})()`);
  assert.equal(neg.status, "403", `未验证的原生提交不该被接受：${JSON.stringify(neg)}`);
  assert.equal(neg.machine, "epocha-missing-response", `未验证提交的机器码不对：${JSON.stringify(neg)}`);

  const before = await listCount();
  const c = await openPage(FIXTURE_URL, { container: FIXTURE_CONTAINER });
  await waitFirstState(c, "ready", 30000, FIXTURE_CONTAINER);
  const id = (await bidi.evaluateJSON(c, `window.__fixture.id`));
  const { answer } = await answerOfCurrent(c, FIXTURE_CONTAINER);
  await typeAnswer(c, answer, FIXTURE_CONTAINER);
  await clickWidgetButton(c, "submit", FIXTURE_CONTAINER);
  await waitFirstState(c, "verified", 40000, FIXTURE_CONTAINER);

  // FormData 视角：字段里就是 getResponse() 的那个 token —— 证明"提交时会带上"。
  const fd = await bidi.evaluateJSON(c, `(()=>{
    const d=new FormData(document.getElementById('order'));
    const v=d.get('epocha_response');
    return {has:d.has('epocha_response'), value: v?String(v):null, keys:[...d.keys()].sort(),
            live: EpoCHA.getResponse(${JSON.stringify(id)})};})()`);
  assert.ok(fd.has, "FormData 里没有 epocha_response 字段");
  assert.equal(fd.value, fd.live, "FormData 里的 token 与 getResponse() 不一致");
  assert.ok(String(fd.value).length > 20, `token 看起来是空的：${JSON.stringify(fd.value)}`);
  assert.deepEqual(fd.keys, ["epocha_response", "text"], `表单字段集合不是预期的两个：${JSON.stringify(fd.keys)}`);

  // 真实导航提交（不是 fetch）：服务端只有 /siteverify 返回 success:true 才写库。
  await bidi.click(c, "form#order button[type=submit]");
  await bidi.waitUntil(c, `(()=>{const p=document.getElementById('post-result');
    return p && p.getAttribute('data-status')!=='200';})()`,
    { timeoutMs: 25000, label: "原生表单提交后拿到结果页" });
  const res = await bidi.evaluateJSON(c, `(()=>{const p=document.getElementById('post-result');
    return {status:p.getAttribute('data-status'), machine:p.getAttribute('data-machine'), text:p.textContent};})()`);
  assert.equal(res.status, "201", `原生提交应写入成功：${JSON.stringify(res)}`);
  assert.equal(await listCount(), before + 1, "结果页说成功，但留言列表没增加");
});

test("隐藏字段的三种归属：form 外无关联即抛错且不留孤儿字段、显式 form 关联可用、fieldName:'' 不建字段", async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  const probe = await bidi.evaluateJSON(c, `(()=>{
    const out = {throws:[], orphanFields:0, associated:null, noField:null, extra:[]};
    const base = {apiBase:${JSON.stringify(EPOCHA_API)}, siteKey:${JSON.stringify(SITE_KEY)}, language:'zh-CN'};
    // (1) 容器不在任何 form 内、也没给 form ⇒ 必须**抛错**。
    //     静默创建一个"永远不会被提交"的字段，等于把接入错误留到线上。
    const loose = document.createElement('div'); document.body.appendChild(loose);
    try { EpoCHA.render(loose, base);
      out.throws.push('no-throw'); } catch (e) { out.throws.push(e instanceof TypeError ? 'TypeError' : String(e).slice(0,30)); }
    out.orphanFields = loose.querySelectorAll('input[type=hidden]').length;
    // (2) 容器在 form 外，但用 form 选项显式关联 ⇒ 字段落进那个 form，FormData 才拿得到。
    const far = document.createElement('div'); document.body.appendChild(far);
    const f2 = document.createElement('form'); f2.id='assoc'; document.body.appendChild(f2);
    const id2 = EpoCHA.render(far, Object.assign({}, base, {fieldName:'assoc_response', form: f2}));
    out.extra.push(id2);
    out.associated = { inForm: !!f2.querySelector('input[name="assoc_response"]'),
      state: EpoCHA.state(id2),
      containerInsideForm: f2.contains(far) };
    // (3) fieldName:'' ⇒ 一个隐藏字段都不该出现（只要回调/getResponse 的接入方）。
    const f3 = document.createElement('form'); document.body.appendChild(f3);
    const d3 = document.createElement('div'); f3.appendChild(d3);
    const id3 = EpoCHA.render(d3, Object.assign({}, base, {fieldName:''}));
    out.extra.push(id3);
    out.noField = f3.querySelectorAll('input[type=hidden]').length;
    return out;})()`);
  assert.deepEqual(probe.throws, ["TypeError"],
    `form 外挂载且没给 form 应当抛 TypeError，实得 ${JSON.stringify(probe.throws)}`);
  assert.equal(probe.orphanFields, 0, `抛错路径不应留下孤儿 hidden input，实得 ${probe.orphanFields}`);
  assert.equal(probe.associated.inForm, true, "显式 form 关联没把字段放进目标 form");
  assert.equal(probe.associated.containerInsideForm, false,
    "用例前提变了：容器竟已在目标 form 内，那这条测的就不是显式关联");
  assert.ok(probe.associated.state, "显式关联的实例没有状态");
  assert.equal(probe.noField, 0, `fieldName:'' 仍然创建了隐藏字段（${probe.noField} 个）`);
  // 附加实例会各占一道题：不销毁就会挤掉后面用例需要的受控题目（题池只有注入的那几十条）。
  await bidi.evaluate(c, `(()=>{(${JSON.stringify(probe.extra)}).forEach(i=>EpoCHA.destroy(i)); return 1;})()`);
});

test("fixture：reset 从 FormData 里拿掉 token，destroy 移除字段本身", async () => {
  const c = await openPage(FIXTURE_URL, { container: FIXTURE_CONTAINER });
  await waitFirstState(c, "ready", 30000, FIXTURE_CONTAINER);
  const id = await bidi.evaluateJSON(c, `window.__fixture.id`);
  const { answer } = await answerOfCurrent(c, FIXTURE_CONTAINER);
  await typeAnswer(c, answer, FIXTURE_CONTAINER);
  await clickWidgetButton(c, "submit", FIXTURE_CONTAINER);
  await waitFirstState(c, "verified", 40000, FIXTURE_CONTAINER);
  const read = () => bidi.evaluateJSON(c, `(()=>{const d=new FormData(document.getElementById('order'));
    return {has:d.has('epocha_response'), value:(d.get('epocha_response')||'').length,
      live:EpoCHA.getResponse(${JSON.stringify(id)}).length};})()`);
  assert.ok((await read()).value > 0, "verified 之后 FormData 里没有 token");

  await bidi.evaluate(c, `EpoCHA.reset(${JSON.stringify(id)})`);
  const afterReset = await read();
  assert.equal(afterReset.value, 0, `reset 后 FormData 里仍留着 token：${JSON.stringify(afterReset)}`);
  assert.equal(afterReset.live, 0, "reset 后 getResponse 还有值");

  await bidi.evaluate(c, `EpoCHA.destroy(${JSON.stringify(id)})`);
  const gone = await bidi.evaluateJSON(c, `document.querySelectorAll('#order input[name="epocha_response"]').length`);
  assert.equal(gone, 0, "destroy 之后隐藏字段还在 DOM 里");
});

test("方案 B：同源 worker 文件路径可用（该页 CSP 禁 blob:，能过就说明确实走的是文件）", async () => {
  const c = await openPage(WORKERFILE_URL);
  await waitFirstState(c, "ready");
  const { answer } = await answerOfCurrent(c);
  // 这个档位下 CSP 是 `worker-src 'self'`：Blob worker 必然被拒（实测过），
  // 所以"能验证通过"本身就是"用了同源 worker 文件"的证据，不需要再去读组件内部状态。
  await typeAnswer(c, answer);
  await clickWidgetButton(c, "submit");
  await waitFirstState(c, "verified");
  const st = await probeState(c);
  assert.ok(st.tokens[0].length > 20, "同源 worker 路径没拿到 token");
  const errs = await bidi.evaluate(c, `window.__pageErrors || []`);
  void errs;
});

test("页面与组件的请求面：只打 /api/challenge 与 /api/redeem，不带凭据", async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  // 在页面里装一个 fetch 记录器，然后完整跑一次验证 + 一次业务提交。
  await bidi.evaluate(c, `(()=>{ window.__calls = [];
    const of = window.fetch;
    window.fetch = function (url, opts) { window.__calls.push(String(url)); return of.call(this, url, opts); };
  })()`);
  const { answer } = await answerOfCurrent(c);
  await typeAnswer(c, answer);
  await clickWidgetButton(c, "submit");
  await waitFirstState(c, "verified");
  await bidi.evaluate(c, `document.getElementById('text').value='请求面检查'`);
  await bidi.click(c, "#send");
  await bidi.waitUntil(c, `document.getElementById('status').textContent.includes('留言已写入')`,
    { timeoutMs: 12000, label: "业务写入" });

  const calls = await bidi.evaluateJSON(c, `window.__calls`);
  const external = calls.filter((u) => u.startsWith("http"));
  for (const u of external) {
    assert.match(u, /\/api\/(challenge|redeem)\?site_key=/, `组件打了不该打的地址：${u}`);
  }
  assert.ok(!calls.some((u) => u.includes("siteverify")), "浏览器侧发起了 /siteverify（那是服务端到服务端的接口）");
  assert.ok(!calls.some((u) => u.includes("/api/admin")), "浏览器侧发起了管理 API 请求");
  const html = await bidi.evaluate(c, `document.documentElement.outerHTML`);
  assert.ok(!/sk_[A-Za-z0-9]{16,}/.test(html), "页面 HTML 里出现了形似验证凭据的串");
  assert.ok(!/ak_[0-9a-f]{16,}/.test(html), "页面 HTML 里出现了形似管理密钥的串");
  assert.ok(html.includes(SITE_KEY), "页面应包含公开的 site_key");
});

test(caseTitle("widget-browser.token-conservative-expiry"), async () => {
  const c = await openPage(TTL_URL);
  await waitFirstState(c, "ready");
  await prepareLifecycleCase(c, "/api/redeem", 15000, TTL_SITE_KEY, true);
  await waitFirstState(c, "ready", 30000, "#lifecycle-widget");
  const { answer } = await answerOfCurrent(c, "#lifecycle-widget");
  await typeAnswer(c, answer, "#lifecycle-widget");
  await clickWidgetButton(c, "submit", "#lifecycle-widget");
  await waitHeldResponse(c);
  const held = await lifecycleProbe(c);
  assert.equal(held.state, "submitting", "必须在兑换响应体被暂扣时观测提交中状态");
  assert.deepEqual(held.successes, []);
  const ttl = await bidi.evaluate(c, `window.__life.ttlSeconds`);
  assert.equal(ttl, 2, "用例必须使用服务端配置的两秒 response TTL，不改写成功响应");
  const delayMS = ttl * 1000 + 500;
  await new Promise((resolve) => setTimeout(resolve, delayMS));
  assert.equal((await lifecycleProbe(c)).state, "submitting", "释放前必须仍在等待响应体，而不是已经超时或成功");
  await bidi.evaluate(c, `window.__life.release()`);
  await waitFirstState(c, "expired", 5000, "#lifecycle-widget");
  const result = await lifecycleProbe(c);
  const timing = await bidi.evaluateJSON(c, `({ttl:window.__life.ttlSeconds,elapsed:window.__life.releasedAt-window.__life.heldAt,expired:window.__life.expired,states:window.__life.states})`);
  assert.ok(timing.elapsed > timing.ttl * 1000, "必须确实延迟超过真实响应里的 TTL");
  assert.equal(result.state, "expired");
  assert.equal(result.token, "");
  assert.equal(result.field, "");
  assert.deepEqual(result.successes, [], "已过期的迟到响应不得触发 onSuccess");
  assert.deepEqual(result.errors, []);
  assert.equal(timing.expired, 1);
  assert.equal(timing.states.includes("verified"), false, "不得短暂进入 verified 再靠计时器过期");
  assert.equal(result.calls.filter((request) => request.path === "/api/redeem").length, 1);
});

test(caseTitle("widget-browser.image-error"), async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  await prepareLifecycleCase(c, null);
  await waitFirstState(c, "ready", 30000, "#lifecycle-widget");
  await bidi.waitUntil(c, `(()=>{const image=document.querySelector('#lifecycle-widget [data-epocha-widget]').shadowRoot.querySelector('img');return image&&image.complete&&image.naturalWidth>0;})()`,
    { timeoutMs: 5000, label: "图片故障注入前真实 PNG 已解码" });
  await bidi.evaluate(c, `(()=>{
    const image=document.querySelector('#lifecycle-widget [data-epocha-widget]').shadowRoot.querySelector('img');
    window.__life.decodeError=null;
    image.addEventListener('error',event=>{window.__life.decodeError={trusted:event.isTrusted,width:image.naturalWidth,complete:image.complete};},{once:true});
    image.src='data:image/png;base64,AQIDBA==';
  })()`);
  await waitFirstState(c, "error", 5000, "#lifecycle-widget");
  await bidi.waitUntil(c, `window.__life.decodeError!==null`, { timeoutMs: 5000, label: "浏览器实际解码损坏 PNG 并派发 error" });
  const decoded = await bidi.evaluateJSON(c, `window.__life.decodeError`);
  assert.deepEqual(decoded, { trusted: true, width: 0, complete: true });
  const probe = await lifecycleProbe(c);
  assert.equal(probe.token, "");
  assert.equal(probe.field, "");
  assert.deepEqual(probe.successes, []);
  assert.deepEqual(probe.errors, ["image-unusable"]);
  assert.equal(probe.calls.filter((request) => request.path === "/api/redeem").length, 0);
  const controls = await bidi.evaluateJSON(c, `(()=>{const root=document.querySelector('#lifecycle-widget [data-epocha-widget]').shadowRoot;return {image:!!root.querySelector('img'),submit:[...root.querySelectorAll('button')].find(b=>['提交答案','Submit'].includes(b.textContent.trim())).disabled,refresh:[...root.querySelectorAll('button')].find(b=>['换一张','New image'].includes(b.textContent.trim())).disabled};})()`);
  assert.deepEqual(controls, { image: false, submit: true, refresh: false });
  await clickWidgetButton(c, "submit", "#lifecycle-widget");
  assert.equal((await lifecycleProbe(c)).calls.filter((request) => request.path === "/api/redeem").length, 0,
    "失败后的禁用按钮不能发起兑换");
  assert.match(await statusText(c, "#lifecycle-widget"), /无法显示|could not be displayed/);
});

test(caseTitle("widget-browser.label-binding"), async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  const binding = await bidi.evaluateJSON(c, `(()=>{
    const root=document.querySelector(${JSON.stringify(WIDGET_HOST)}).shadowRoot;
    const label=root.querySelector('label'),input=root.querySelector('input.ans');
    input.blur();
    label.scrollIntoView({block:'center',inline:'center'});
    const rect=label.getBoundingClientRect();
    return {for:label.htmlFor||label.getAttribute('for'),id:input.id,
      labels:[...input.labels||[]].map(item=>item.textContent),
      center:[Math.round(rect.left+rect.width/2),Math.round(rect.top+rect.height/2)],
      size:[rect.width,rect.height],focused:root.activeElement===input};
  })()`);
  assert.ok(binding.id && binding.for, "label 与答案框必须有非空关联标识");
  assert.equal(binding.for, binding.id);
  assert.ok(binding.labels.some((text) => text.includes("characters") || text.includes("字符")));
  assert.ok(binding.size[0] > 0 && binding.size[1] > 0, "label 必须可见且有真实点击区域");
  assert.equal(binding.focused, false, "点击前先移出答案框焦点，防止永真断言");
  await bidi.pointerClick(c, binding.center[0], binding.center[1]);
  await bidi.waitUntil(c, `(()=>{const root=document.querySelector(${JSON.stringify(WIDGET_HOST)}).shadowRoot;return root.activeElement===root.querySelector('input.ans');})()`,
    { timeoutMs: 5000, label: "真实鼠标点击 label 后答案框得到焦点" });
});

test(caseTitle("widget-browser.reset-clears-image"), async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  await prepareLifecycleCase(c, null);
  await waitFirstState(c, "ready", 30000, "#lifecycle-widget");
  await bidi.evaluate(c, `EpoCHA.reset(window.__life.id)`);
  const result = await bidi.evaluateJSON(c, `(()=>{const h=document.querySelector('#lifecycle-widget [data-epocha-widget]');const r=h.shadowRoot;return {state:EpoCHA.state(window.__life.id),image:!!r.querySelector('img'),submit:[...r.querySelectorAll('button')].find(b=>['提交答案','Submit'].includes(b.textContent.trim())).disabled,refresh:[...r.querySelectorAll('button')].find(b=>['换一张','New image'].includes(b.textContent.trim())).disabled,token:EpoCHA.getResponse(window.__life.id)};})()`);
  assert.deepEqual(result, { state: "idle", image: false, submit: true, refresh: false, token: "" });
  await clickWidgetButton(c, "submit", "#lifecycle-widget");
  const idle = await lifecycleProbe(c);
  assert.equal(idle.state, "idle");
  assert.equal(idle.field, "");
  assert.equal(idle.calls.filter((request) => request.path === "/api/redeem").length, 0);
  assert.equal(idle.calls.filter((request) => request.path === "/api/challenge").length, 1, "reset 和无效提交不能自动领题");
  await clickWidgetButton(c, "refresh", "#lifecycle-widget");
  await waitFirstState(c, "ready", 30000, "#lifecycle-widget");
  const ready = await lifecycleProbe(c);
  assert.equal(ready.calls.filter((request) => request.path === "/api/challenge").length, 2);
  assert.equal(ready.calls.filter((request) => request.path === "/api/redeem").length, 0);
  assert.equal(ready.token, "");
  assert.deepEqual(ready.successes, []);
});

test(caseTitle("widget-browser.max-auto-retry-zero"), async () => {
  const c = await openPage(EXAMPLE_URL);
  await waitFirstState(c, "ready");
  await bidi.evaluate(c, `(()=>{
    EpoCHA.ids().forEach(id=>EpoCHA.destroy(id));
    const orig=window.fetch;
    window.__retry={calls:0,successes:[],errors:[]};
    window.fetch=function(url,opts){
      if(new URL(url,location.href).pathname==='/api/challenge'){
        window.__retry.calls++;
        return Promise.resolve(new Response(JSON.stringify({'error-codes':['challenge-unavailable']}),
          {status:503,headers:{'Content-Type':'application/json','Retry-After':'1'}}));
      }
      return orig.call(this,url,opts);
    };
    const form=document.createElement('form');document.body.appendChild(form);
    const container=document.createElement('div');container.id='retry-widget';form.appendChild(container);
    window.__retry.id=EpoCHA.render(container,{apiBase:${JSON.stringify(EPOCHA_API)},siteKey:${JSON.stringify(SITE_KEY)},
      maxAutoRetry:0,onSuccess:token=>window.__retry.successes.push(token),onError:err=>window.__retry.errors.push(err.code)});
  })()`);
  await waitFirstState(c, "error", 5000, "#retry-widget");
  await new Promise((resolve) => setTimeout(resolve, 1500));
  const result = await bidi.evaluateJSON(c, `(()=>{const w=window.__retry;return {calls:w.calls,state:EpoCHA.state(w.id),token:EpoCHA.getResponse(w.id),field:document.querySelector('#retry-widget input[type=hidden]').value,successes:w.successes,errors:w.errors};})()`);
  assert.equal(result.calls, 1);
  assert.equal(result.state, "error");
  assert.equal(result.token, "");
  assert.equal(result.field, "");
  assert.deepEqual(result.successes, []);
  assert.deepEqual(result.errors, ["challenge-unavailable"]);
});

test(caseTitle("widget.expiry-clears-response"), async () => {
  const c = await openPage(TTL_URL);
  await waitFirstState(c, "ready");
  // 这个演示站点的 response_ttl_sec=2（服务端权威），所以客户端必须在 2 秒后自己作废。
  const id = await bidi.evaluateJSON(c, `(()=>{
    const f=document.createElement('form'); document.body.appendChild(f);
    const d=document.createElement('div'); f.appendChild(d);
    window.__expired = 0;
    return EpoCHA.render(d, {apiBase: ${JSON.stringify(EPOCHA_API)}, siteKey: ${JSON.stringify(TTL_SITE_KEY)},
      language:'zh-CN', fieldName:'ttl_response',
      onExpired: function () { window.__expired++; }});
  })()`);
  await bidi.waitUntil(c, `EpoCHA.state(${JSON.stringify(id)})==='ready'`, { timeoutMs: 30000, label: "短 TTL 实例进入 ready" });
  const imgs = await bidi.evaluateJSON(c, `(()=>{const hs=[...document.querySelectorAll('[data-epocha-widget]')];
    const h=hs[hs.length-1]; const img=h.shadowRoot.querySelector('img');
    return img?img.src.split('base64,')[1]:null;})()`);
  const answer = answerByImageB64.get(imgs);
  assert.ok(answer, "短 TTL 实例的题目不在受控注入集合里");
  await bidi.evaluate(c, `(()=>{const hs=[...document.querySelectorAll('[data-epocha-widget]')];
    hs[hs.length-1].shadowRoot.querySelector('input.ans').focus();})()`);
  await bidi.send("input.performActions", { context: c, actions: [{
    type: "key", id: "kttl", actions: [...answer].flatMap((ch) => [{ type: "keyDown", value: ch }, { type: "keyUp", value: ch }]) }] });
  const pos = await bidi.evaluateJSON(c, `(()=>{const hs=[...document.querySelectorAll('[data-epocha-widget]')];
    const b=[...hs[hs.length-1].shadowRoot.querySelectorAll('button')].find(el=>el.textContent.trim()==='提交答案');
    b.scrollIntoView({block:'center'}); const r=b.getBoundingClientRect();
    return [Math.round(r.left+r.width/2), Math.round(r.top+r.height/2)];})()`);
  await bidi.pointerClick(c, pos[0], pos[1]);
  await bidi.waitUntil(c, `EpoCHA.state(${JSON.stringify(id)})==='verified'`, { timeoutMs: 30000, label: "短 TTL 实例验证通过" });
  const got = await bidi.evaluateJSON(c, `(()=>({token:EpoCHA.getResponse(${JSON.stringify(id)}),
    field:(document.querySelector('input[name=ttl_response]')||{}).value}))()`);
  assert.ok(got.token && got.token.length > 20, "刚验证通过时应有 token");
  assert.equal(got.field, got.token, "隐藏字段应与 getResponse() 同步");

  await bidi.waitUntil(c, `EpoCHA.state(${JSON.stringify(id)})==='expired'`,
    { timeoutMs: 15000, label: "服务端 TTL 到期后组件进入 expired" });
  const after = await bidi.evaluateJSON(c, `(()=>({token:EpoCHA.getResponse(${JSON.stringify(id)}),
    field:(document.querySelector('input[name=ttl_response]')||{value:'MISSING'}).value,
    expired:window.__expired, hosts:document.querySelectorAll('[data-epocha-widget]').length}))()`);
  assert.equal(after.token, "", "过期后 getResponse() 必须返回空串");
  assert.equal(after.field, "", "过期后隐藏字段必须清空（不能只改 UI）");
  assert.equal(after.expired, 1, "onExpired 应恰好触发一次");
  void id; void c;
});
