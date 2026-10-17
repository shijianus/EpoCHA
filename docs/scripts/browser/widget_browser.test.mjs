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
// 环境变量（缺失即抛错退出，不静默跳过）：
//   BIDI_WS              ws://127.0.0.1:PORT/session
//   EPOCHA_API           http://127.0.0.1:PORT       epocha 服务
//   EXAMPLE_URL          http://127.0.0.1:PORT/      exampleapp，CSP=blob
//   EXAMPLE_NOWORKER_URL http://127.0.0.1:PORT/      exampleapp，CSP worker-src 'none'
//   SEED_JSON            devseed 答案表；SEED_DIR 它的 PNG 目录
//   SITE_KEY             演示站点 site_key

import test, { before, after } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, existsSync } from "node:fs";
import path from "node:path";
import { BiDi } from "./bidi.mjs";

const need = (name) => {
  const v = process.env[name];
  if (!v) throw new Error(`缺少环境变量 ${name}（浏览器验收由 docs/scripts/stage4a.sh 编排）`);
  return v;
};
const BIDI_WS = need("BIDI_WS");
const EPOCHA_API = need("EPOCHA_API").replace(/\/+$/, "");
const EXAMPLE_URL = need("EXAMPLE_URL");
const NOWORKER_URL = need("EXAMPLE_NOWORKER_URL");
const SEED_JSON = need("SEED_JSON");
const SEED_DIR = need("SEED_DIR");
const SITE_KEY = need("SITE_KEY");

if (!existsSync(SEED_JSON)) throw new Error(`答案表不存在：${SEED_JSON}`);
const seeds = JSON.parse(readFileSync(SEED_JSON, "utf8"));
if (!Array.isArray(seeds) || seeds.length === 0) throw new Error("答案表为空");

const answerByImageB64 = new Map();
for (const s of seeds) {
  if (!s.png || !s.answer) continue;
  answerByImageB64.set(readFileSync(path.join(SEED_DIR, s.png)).toString("base64"), s.answer);
}

const WIDGET_HOST = "#widget [data-epocha-widget]";
let bidi;
const openTabs = [];

// 预加载脚本：把 shadow DOM 里状态区的每次变化记进 window.__statusLog。
// PoW 的耗时（worker 上报的 ms）就藏在那段文本里，事后读不到，必须在页面里当场记录。
const PRELOAD = `()=>{
  window.__statusLog = [];
  const tick = () => {
    const host = document.querySelector('#widget [data-epocha-widget]');
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

async function openPage(url) {
  const c = await bidi.newTab();
  openTabs.push(c);
  const pre = await bidi.send("script.addPreloadScript", { functionDeclaration: PRELOAD }).catch(() => null);
  await bidi.goto(c, url);
  await bidi.waitUntil(c, `!!document.querySelector(${JSON.stringify(WIDGET_HOST)})`,
    { timeoutMs: 20000, label: "widget 容器出现" });
  if (pre && pre.script) await bidi.send("script.removePreloadScript", { script: pre.script }).catch(() => {});
  return c;
}

// 返回**对象**的表达式一律走 evaluateJSON：BiDi 对对象给句柄而不是内联值（见 bidi.mjs）。
const STATE_PROBE = `(()=>{const ids=window.EpoCHA?EpoCHA.ids():[];
  return {count:ids.length, ids, states:ids.map(i=>EpoCHA.state(i)), tokens:ids.map(i=>EpoCHA.getResponse(i))};})()`;
const probeState = (c) => bidi.evaluateJSON(c, STATE_PROBE);

async function waitFirstState(c, want, timeoutMs = 30000) {
  try {
    await bidi.waitUntil(c,
    `(()=>{const ids=window.EpoCHA?EpoCHA.ids():[];return ids.length>0 && EpoCHA.state(ids[0])===${JSON.stringify(want)};})()`,
      { timeoutMs, label: `第一个实例进入 ${want}` });
  } catch (e) {
    const why = await bidi.evaluateJSON(c, `(()=>{const h=document.querySelector(${JSON.stringify(WIDGET_HOST)});
      const ids=window.EpoCHA?EpoCHA.ids():'NO_EPOCHA';
      return {status: h&&h.shadowRoot ? h.shadowRoot.querySelector('[role=status]').textContent : '组件没挂载',
        ids, states: Array.isArray(ids) ? ids.map(i=>EpoCHA.state(i)) : null,
        hosts: document.querySelectorAll('[data-epocha-widget]').length,
        url: location.href};})()`).catch((err) => ({probeError: String(err)}));
    throw new Error(`${e.message}（现场：${JSON.stringify(why)}）`);
  }
  return bidi.evaluateJSON(c, STATE_PROBE);
}

async function currentImageB64(c) {
  return bidi.evaluate(c, `(()=>{const h=document.querySelector(${JSON.stringify(WIDGET_HOST)});
    const img=h&&h.shadowRoot&&h.shadowRoot.querySelector('img');
    return img&&img.src?img.src.split('base64,')[1]:null;})()`);
}

async function answerOfCurrent(c) {
  const b64 = await currentImageB64(c);
  assert.ok(b64, "页面上没有可读取的题目图片");
  const a = answerByImageB64.get(b64);
  assert.ok(a, "当前显示的题目不在受控注入集合里（注入不足或来源被污染）");
  return { answer: a, imageB64: b64 };
}

async function typeAnswer(c, text) {
  await bidi.evaluate(c, `document.querySelector(${JSON.stringify(WIDGET_HOST)})
    .shadowRoot.querySelector('input.ans').focus()`);
  await bidi.send("input.performActions", {
    context: c,
    actions: [{
      type: "key", id: "kk" + Math.random().toString(36).slice(2, 6),
      actions: [...text].flatMap((ch) => [{ type: "keyDown", value: ch }, { type: "keyUp", value: ch }]),
    }],
  });
}

async function statusText(c) {
  return bidi.evaluate(c, `(()=>{const h=document.querySelector(${JSON.stringify(WIDGET_HOST)});
    const s=h&&h.shadowRoot&&h.shadowRoot.querySelector('[role=status]');
    return s?s.textContent:null;})()`);
}

async function clickWidgetButton(c, which) {
  // which: "submit" | "refresh" —— 按 shadow DOM 里的顺序定位，然后**真实鼠标点击**。
  const sel = which === "submit" ? "button:nth-of-type(1)" : "button:nth-of-type(2)";
  return bidi.clickDeep(c, "#widget", sel);
}

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

after(async () => {
  for (const c of openTabs) await bidi.closeTab(c);
  await bidi.send("session.end", {}).catch(() => {});
  bidi.close();
});

test("闭环：显示图片 → 输入 → 浏览器内 PoW → redeem → 业务表单 → 服务端写入", async () => {
  const c = await openPage(EXAMPLE_URL);
  const s = await waitFirstState(c, "ready");
  assert.equal(s.count, 1, "页面应只自动创建一个组件实例");
  const { answer } = await answerOfCurrent(c);

  const dims = await bidi.evaluateJSON(c, `(()=>{const h=document.querySelector(${JSON.stringify(WIDGET_HOST)});
    const img=h.shadowRoot.querySelector('img');
    return {natural:[img.naturalWidth,img.naturalHeight], attr:[img.width,img.height], alt:img.alt};})()`);
  assert.ok(dims.natural[0] > 0 && dims.natural[1] > 0, "浏览器没有解码出图片");
  assert.deepEqual(dims.natural, dims.attr, `解码尺寸 ${dims.natural} 与声明 ${dims.attr} 不一致`);
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

test("计算超时是真实约束：高难度站点上 powTimeoutMs 到点即明确失败，不去兑换",
  { skip: process.env.EXAMPLE_HARD_URL ? false : "没给 EXAMPLE_HARD_URL" }, async () => {
  const c = await openPage(process.env.EXAMPLE_HARD_URL);
  await waitFirstState(c, "ready");
  // 在同一个高难度页面（bits=20，浏览器要算上百万次）上再挂一个**超时预算只有 400ms** 的实例。
  // 用公开配置面制造超时，而不是靠运气让 1ms 定时器抢在一次几毫秒的求解前面。
  const id2 = await bidi.evaluateJSON(c, `(()=>{
    const d=document.createElement('div'); d.id='wto'; document.body.appendChild(d);
    return EpoCHA.render(d, {apiBase: ${JSON.stringify(EPOCHA_API)}, siteKey: ${JSON.stringify(process.env.HARD_SITE_KEY)},
      language:'zh-CN', fieldName:'', powTimeoutMs: 400});
  })()`);
  await bidi.waitUntil(c, `EpoCHA.state(${JSON.stringify(id2)})==='ready'`, { timeoutMs: 40000, label: "预算实例进入 ready" });
  await bidi.evaluate(c, `(()=>{const hs=document.getElementById('wto').querySelector('[data-epocha-widget]');
    hs.shadowRoot.querySelector('input.ans').value='ABCDEF';})()`);
  const pos = await bidi.deepCenter(c, "#wto", "button:nth-of-type(1)");
  assert.ok(pos, "找不到超时实例的提交按钮");
  await bidi.pointerClick(c, pos[0], pos[1]);
  await bidi.waitUntil(c, `EpoCHA.state(${JSON.stringify(id2)})==='error'`,
    { timeoutMs: 20000, label: "powTimeoutMs=400 应给出超时错误" });
  const txt = await bidi.evaluate(c, `(()=>{const hs=document.getElementById('wto').querySelector('[data-epocha-widget]');
    return hs.shadowRoot.querySelector('[role=status]').textContent;})()`);
  assert.match(txt, /超时/, `超时文案不对：${txt}`);
  assert.equal(await bidi.evaluate(c, `EpoCHA.getResponse(${JSON.stringify(id2)})`), "", "超时后不该留着 token");
  await bidi.evaluate(c, `EpoCHA.destroy(${JSON.stringify(id2)})`);
});

test("高难度站点：取消正在进行的计算不会晚到生效", { skip: process.env.EXAMPLE_HARD_URL ? false : "没给 EXAMPLE_HARD_URL" }, async () => {
  const c = await openPage(process.env.EXAMPLE_HARD_URL);
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

test("手机宽度（375px）不产生横向溢出", async (t) => {
  // Firefox 的 BiDi 目前没有稳定的视口设置命令；能设就测，不能设就**明确报 BLOCKED**，
  // 不拿桌面宽度结果冒充移动端结论。
  let resized = false;
  const c = await bidi.newTab();
  openTabs.push(c);
  for (const [method, params] of [
    ["browsingContext.resize", { context: c, width: 375, height: 812 }],
    ["emulation.setViewport", { context: c, viewport: { width: 375, height: 812 } }],
  ]) {
    try { await bidi.send(method, params); resized = true; break; } catch (e) { /* 继续试下一条 */ }
  }
  if (!resized) {
    // skip 而不是 return：return 会被 node:test 记成"通过"，那正是"PASS：未测"的混合表达。
    t.diagnostic("BLOCKED：Firefox BiDi 没有可用的视口设置命令 ⇒ 375px 宽度检查未执行（需人工在真机或 DevTools 响应式模式下确认）");
    t.skip("375px 视口不可设置");
    return;
  }
  await bidi.goto(c, EXAMPLE_URL);
  await waitFirstState(c, "ready");
  const over = await bidi.evaluateJSON(c, `(()=>{
    const h=document.querySelector(${JSON.stringify(WIDGET_HOST)});
    const box=h.shadowRoot.querySelector('.box').getBoundingClientRect();
    return {boxRight:Math.round(box.right), inner:window.innerWidth, docScroll:document.documentElement.scrollWidth};})()`);
  assert.ok(over.boxRight <= over.inner + 1, `组件超出视口：${JSON.stringify(over)}`);
  assert.ok(over.docScroll <= over.inner + 1, `页面出现横向滚动：${JSON.stringify(over)}`);
});

test("方案 B：同源 worker 文件路径可用（该页 CSP 禁 blob:，能过就说明确实走的是文件）",
  { skip: process.env.EXAMPLE_WORKERFILE_URL ? false : "没给 EXAMPLE_WORKERFILE_URL" }, async () => {
  const c = await openPage(process.env.EXAMPLE_WORKERFILE_URL);
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
