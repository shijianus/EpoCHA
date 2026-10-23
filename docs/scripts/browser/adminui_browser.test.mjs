// 管理控制台 M1 的真实浏览器验收（Firefox WebDriver BiDi + Node 内置 WebSocket）。
//
// **不是截图**：这里的登录、创建、编辑、启停、退出都是真实键盘/鼠标事件（input.performActions），
// 断言读的是真实 DOM 与真实服务端状态（Node 侧再打一次管理 API 对账）。
//
// 环境变量（全部缺失即抛错，不静默跳过 —— 必需项"跳过并算过"比失败更危险）：
//   BIDI_WS            ws://127.0.0.1:PORT/session   Firefox BiDi 端点
//   ADMIN_UI_URL       http://127.0.0.1:PORT/admin    管理控制台页面
//   ADMIN_API          http://127.0.0.1:PORT          管理监听器基址（Node 侧对账用）
//   ADMIN_KEY_FILE     运行时生成的演示管理密钥文件（0600）
//   BAD_ADMIN_KEY      形状合法但**未登记**的管理密钥（测 401 与 429 文案）
//   ADMIN_SERVER_PID   本轮服务进程 PID（只用于最后一个用例：真断网时的文案）
//
// 已知边界（如实标注，不冒充通过）：
//   - 403 与 5xx 两个分支**没有**被执行过：管理 API 在当前实现里不产生这两种响应
//     （鉴权只有 401/429，写入错误是 400/404/409，内部错误需要破坏 DB）。
//     所以"403/5xx 不被说成密码错误"这一条在浏览器层是 NOT_RUN，
//     本套件覆盖的是能真实触发的 401 / 400 / 409 / 429 / 连接失败 五支。
//   - 剪贴板写入在 headless Firefox 里没有权限，复制按钮走的是"选中 + 提示手动复制"的回落路径；
//     本套件断言的是**回落到真实发生**（按钮文案与选区），不宣称"已验证剪贴板可用"。
//   - 键入的**值**用 ASCII（gate-site-*）：WebDriver 的 key action 对非 ASCII 字符是否产生
//     文本不在本仓库实测过，不该拿它当被测点。页面文案（中文标签、状态句）仍然真实断言，
//     因为读 innerText 与敲键是两回事。服务端对中文名的接受度由门禁脚本的 curl 步骤覆盖。

import test, { before, after } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { BiDi } from "./bidi.mjs";

const need = (name) => {
  const v = process.env[name];
  if (!v) throw new Error(`缺少环境变量 ${name}（浏览器验收由 docs/scripts/admin-ui-check.sh 编排）`);
  return v;
};

const BIDI_WS = need("BIDI_WS");
const ADMIN_UI_URL = need("ADMIN_UI_URL");
const ADMIN_API = need("ADMIN_API").replace(/\/+$/, "");
const ADMIN_KEY_FILE = need("ADMIN_KEY_FILE");
const BAD_ADMIN_KEY = need("BAD_ADMIN_KEY");
const ADMIN_SERVER_PID = need("ADMIN_SERVER_PID");

const ADMIN_KEY = readFileSync(ADMIN_KEY_FILE, "utf8").trim();
if (!ADMIN_KEY) throw new Error("演示管理密钥为空");
const ADMIN_ORIGIN = new URL(ADMIN_UI_URL).origin;

let bidi = null;
let ctx = null;
// 本轮创建的资源（退出时清掉临时数据；服务与库本身由脚本的 cleanup 负责）。
let createdSiteID = 0;
let createdSiteKey = "";
let createdSecret = "";

// ---------- 页面交互助手（全部走真实事件） ----------

async function openPage() {
  await bidi.goto(ctx, ADMIN_UI_URL);
  await bidi.waitUntil(ctx, `!!document.getElementById("admin-key")`, { label: "页面壳加载" });
}

// 真实键盘输入到登录框：focus 之后逐键发送，不直接改 .value。
async function typeInto(selector, text, { clearFirst = true } = {}) {
  await bidi.evaluate(ctx, `document.querySelector(${JSON.stringify(selector)}).focus()`);
  if (clearFirst) await pressChord("Control", "a");
  await bidi.type(ctx, selector, text, { enter: false });
}

async function press(...keys) {
  for (const k of keys) {
    await sendKeys([{ type: "keyDown", value: keyValue(k) }, { type: "keyUp", value: keyValue(k) }]);
  }
}

// 一个 key action 序列里的 down/down/up/up 才是真的组合键：
// 分成两次 performActions 时 Control 已经抬起，"Ctrl+A" 只会留下一个 a。
async function pressChord(...keys) {
  const vals = keys.map(keyValue);
  const acts = vals.map((k) => ({ type: "keyDown", value: k }));
  for (let i = vals.length - 1; i >= 0; i--) acts.push({ type: "keyUp", value: vals[i] });
  await sendKeys(...acts);
}

// Firefox 147.0.2 的 input.performActions **不接受命名键**：能力探针实测
// value:"Control" / "Enter" / "ArrowDown" 一律回 "invalid argument"，而 W3C WebDriver 的
// 码点（U+E000–U+E03F）会真实生效——ArrowDown 让 select 换选项、Ctrl+A 真的整段选中、
// Enter 在 textarea 里插入换行、number 输入框全选后敲入是**替换**而不是追加。
// （把"我没实现的那个调用失败了"读成"浏览器不支持键盘操作"是上一轮记过的教训。）
// 所以调用点继续按名字写，但这里必须把名字翻成码点；表里没有的名字直接抛错，
// 否则将来加一个键只会得到一次无法解释的 "invalid argument"。
const WD_KEY = {
  Backspace: 0xE003, Tab: 0xE004, Enter: 0xE007, Shift: 0xE008, Control: 0xE009,
  Alt: 0xE00A, Escape: 0xE00C, ArrowUp: 0xE013, ArrowDown: 0xE015, ArrowLeft: 0xE012,
  ArrowRight: 0xE014, Home: 0xE011, End: 0xE010, Delete: 0xE017,
};
function keyValue(k) {
  if (typeof k !== "string" || k.length === 0) throw new Error(`非法键名：${String(k)}`);
  if (k.length === 1) return k; // 普通字符本身就是 UTF-16 码元
  const cp = WD_KEY[k];
  if (cp === undefined) {
    throw new Error(`键名 "${k}" 不在 WebDriver 码点表里；Firefox BiDi 只收码点，不收命名键`);
  }
  return String.fromCharCode(cp);
}

async function sendKeys(...actions) {
  await bidi.send("input.performActions", {
    context: ctx,
    actions: [{ type: "key", id: "k" + Math.random().toString(36).slice(2, 7), actions }]
  });
}

async function clickText(needle, tag = "button") {
  // 按可见文案找按钮再**真实点击**（中心坐标），不派发合成 click 事件。
  const pos = await bidi.evaluate(ctx,
    `(()=>{const els=[...document.querySelectorAll(${JSON.stringify(tag)})];
      const el=els.find(e=>(e.textContent||"").trim()===${JSON.stringify(needle)});
      if(!el) return null;
      el.scrollIntoView({block:'center', inline:'center'});
      const r=el.getBoundingClientRect();
      return [Math.round(r.left+r.width/2), Math.round(r.top+r.height/2)];})()`);
  if (!pos) throw new Error(`找不到文案为「${needle}」的 ${tag}`);
  return bidi.pointerClick(ctx, pos[0], pos[1]);
}

async function login(key) {
  await typeInto("#admin-key", key);
  const before = await bidi.evaluate(ctx, `document.getElementById("admin-key").value`);
  await clickText("验证身份");
  return before;
}

const text = () => bidi.evaluate(ctx, `document.body.innerText`);
const status = (sel) => bidi.evaluate(ctx, `(()=>{const n=document.querySelector(${JSON.stringify(sel)});return n?n.textContent:null;})()`);

// 页面里出现的高熵凭据形状（管理密钥 / 站点 secret）。
async function credentialShapes() {
  return bidi.evaluate(ctx,
    `(()=>{const t=document.body.innerHTML;
      const m=t.match(/sk_[0-9a-fA-F]{24,}/g)||[];
      const k=t.match(/ak_[0-9A-Za-z_-]{20,}/g)||[];
      return JSON.stringify({secrets:m,keys:k,url:location.href,hash:location.hash});})()`);
}

// Node 侧直接打管理 API 对账：证明页面显示的与服务端存的是同一件事。
async function api(method, path, body, key = ADMIN_KEY) {
  const opts = { method, headers: { "Cache-Control": "no-store" } };
  if (key) opts.headers["Authorization"] = "Bearer " + key;
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(ADMIN_API + path, opts);
  const raw = await resp.text();
  let data = null;
  try { data = JSON.parse(raw); } catch { /* 非 JSON 原样返回 */ }
  return { status: resp.status, data, raw };
}

// ---------- 判据的前提守卫 ----------

// mustID：页面里找不到那个控件时，**必须**在这里失败并说清是哪个控件。
// 少了这道守卫，null 会被模板串拼成 `#[object Object]`，报出来的是"选择器不合法"，
// 看起来像浏览器或页面的毛病，而真实结论是"这一页根本没有这个字段"。
function mustID(id, what) {
  assert.equal(typeof id, "string", `找不到${what}（页面里没有这个控件，后续判据的前提不成立）`);
  assert.ok(id.length > 0, `找不到${what}（id 为空串）`);
  return id;
}

// requireSite：后面五条用例都建立在"第 3 条真的创建了一个站点"之上。
// 不守住的话 createdSiteID 还是 0、createdSiteKey 还是空串，而 `"".includes` 判据
// 会**反向**成立（空串包含于任何字符串），于是"退出后没有站点数据"这种安全断言
// 变成一个永真式。失败方向反掉比直接失败更危险。
function requireSite(what) {
  assert.ok(createdSiteID > 0, `${what}：前置用例没建成站点（createdSiteID=${createdSiteID}），本条判据不成立`);
  assert.ok(createdSiteKey.length > 3, `${what}：前置用例没拿到 site_key，本条判据不成立`);
  assert.ok(createdSecret.length > 3, `${what}：前置用例没拿到一次性 secret，本条判据不成立`);
}

// ---------- 会话装配 ----------

before(async () => {
  bidi = new BiDi(BIDI_WS);
  await bidi.connect();
  await bidi.startSession();
  await bidi.subscribeEvents(["log.entryAdded"]);
  ctx = await bidi.newTab();
  await openPage();
});

after(async () => {
  if (ctx) await bidi.closeTab(ctx);
  if (bidi) bidi.close();
});

// ---------- 1. 错误凭据：真实 401，文案不是"网络问题"，输入框已清空 ----------

test("错误 key 登录失败：显示凭据无效、留在登录面、输入框不留原文", async () => {
  const keyBefore = await login(BAD_ADMIN_KEY);
  await bidi.waitUntil(ctx, `document.getElementById("login-status").textContent.includes("凭据无效")`,
    { label: "401 文案" });
  assert.equal(keyBefore, BAD_ADMIN_KEY, "提交前应能在输入框里看到原文（说明这是真人打进去的）");
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("admin-key").value`), "",
    "提交后必须立刻清空原文，失败也一样");
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("login").hidden`), false,
    "失败后应停在登录面");
  assert.equal(await bidi.evaluate(ctx, `!!document.getElementById("login").hidden`), false);
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("view").hidden`), true);
  assert.ok(/凭据无效或已失效/.test(await status("#login-status")), await status("#login-status"));
  // 未登录的页面壳里没有任何凭据
  const shapes = JSON.parse(await credentialShapes());
  assert.deepEqual(shapes.keys, []);
  assert.deepEqual(shapes.secrets, []);
});

// ---------- 2. 正确凭据：whoami 的 key_id 显示出来 ----------

test("正确 key 登录：显示非敏感 key_id，凭据不进任何浏览器存储", async () => {
  const who = await api("GET", "/api/admin/whoami");
  assert.equal(who.status, 200, JSON.stringify(who));
  await login(ADMIN_KEY);
  await bidi.waitUntil(ctx, `document.getElementById("session-state").classList.contains("on")`,
    { label: "登录成功状态" });
  const chip = await bidi.evaluate(ctx, `document.getElementById("session-state").textContent`);
  assert.ok(chip.includes(String(who.data.key_id)), `会话条应显示 key_id：${chip}`);
  assert.ok(!chip.includes(ADMIN_KEY), "会话条不得出现凭据原文");
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("login").hidden`), true);
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("nav").hidden`), false);
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("admin-key").value`), "",
    "登录成功后输入框必须是空的");

  // 浏览器存储四面：localStorage / sessionStorage / cookie / IndexedDB
  const store = await bidi.evaluateJSON(ctx,
    `(async()=>{const idb=await indexedDB.databases();
      return {ls:Object.getOwnPropertyNames(localStorage),
              ss:Object.getOwnPropertyNames(sessionStorage),
              cookie:document.cookie,
              idb:idb.map(d=>d.name)};})()`, true);
  assert.deepEqual(store.ls, [], `localStorage 不该有任何键：${JSON.stringify(store)}`);
  assert.deepEqual(store.ss, [], `sessionStorage 不该有任何键：${JSON.stringify(store)}`);
  assert.equal(store.cookie, "", `不该有 Cookie：${store.cookie}`);
  assert.deepEqual(store.idb, [], `不该有 IndexedDB 库：${JSON.stringify(store.idb)}`);
});

// ---------- 3+4. 创建站点：secret 只在这次结果里 ----------

test("创建站点：真实 201、一次性 secret 出现在结果面板、验证模式与题型按选择生效", async () => {
  // 空列表状态先落在眼睛能看见的地方
  await bidi.waitUntil(ctx, `document.querySelector("#view-body") && document.body.innerText.includes("新建站点")`,
    { label: "站点列表与创建表单" });
  assert.match(await text(), /库里还没有站点|这一页没有站点/, "初始应显示空列表状态");

  await typeInto(".create-site input[type=text]", "gate-site-a");
  // 验证模式：真实键盘把 pow_only 改成 pow_question（ArrowDown 选中下一个 option）
  await bidi.evaluate(ctx, `document.querySelector(".create-site select").focus()`);
  await press("ArrowDown");
  await bidi.evaluate(ctx, `document.activeElement.blur()`);
  const modeVal = await bidi.evaluate(ctx,
    `(()=>{const s=[...document.querySelectorAll(".create-site select")];return s[0].value;})()`);
  assert.equal(modeVal, "pow_question", `验证模式应被改成 pow_question，got ${modeVal}`);

  await clickText("创建站点");
  await bidi.waitUntil(ctx,
    `document.querySelector(".once textarea") && document.querySelector(".once textarea").value.length>10`,
    { label: "一次性 secret 面板" });

  createdSecret = await bidi.evaluate(ctx, `document.querySelector(".once textarea").value`);
  assert.match(createdSecret, /^sk_/, `secret 形状不符：${createdSecret}`);
  const panel = await text();
  assert.match(panel, /只显示一次的凭据/);
  assert.match(panel, /此后任何接口都不会再返回它|不会再返回它/);
  assert.match(panel, /gate-site-a/);
  // 成功提示必须落在**页面上**：结果面板是 clear(form) 之后重建的，
  // 早先的版本把这句话写到了刚被摘出文档的旧 #new-status 上——代码跑了，屏幕上什么都没有。
  assert.match(await status("#new-status") || "", /已创建并写入审计/,
    "创建成功的确认文案没有出现在页面上");

  // 与真实服务端对账：201 的那条就是库里这条
  const list = await api("GET", "/api/admin/sites");
  const row = list.data.sites.find((s) => s.name === "gate-site-a");
  assert.ok(row, `列表里没有刚创建的站点：${JSON.stringify(list.data)}`);
  createdSiteID = row.id;
  createdSiteKey = row.site_key;
  assert.equal(row.verify_mode, "pow_question");
  assert.ok(row.generator_id, "pow_question 站点必须有题型");

  // 复制按钮的真实行为：headless 无剪贴板权限时必须是**可见的回落**，不能假装成功
  await clickText("复制");
  await bidi.waitUntil(ctx,
    `(()=>{const t=document.getElementById("toast");return t && (t.textContent.includes("剪贴板")||t.textContent.includes("Ctrl"));})()`,
    { label: "复制结果提示" });
  const toastText = await bidi.evaluate(ctx, `document.getElementById("toast").textContent`);
  const selection = await bidi.evaluate(ctx,
    `(()=>{const a=document.querySelector(".once textarea");return [a.selectionStart,a.selectionEnd,a.value.length];})()`);
  assert.ok(selection[1] === selection[2] && selection[0] === 0, `secret 应被整段选中：${JSON.stringify(selection)}`);
  assert.match(toastText, /剪贴板|Ctrl/);
});

// ---------- 5. 进详情取不回 secret ----------

test("进入详情不再取回 secret：页面与只读 API 两侧都扫过", async () => {
  requireSite("进详情取不回 secret");
  await clickText("进入站点详情");
  await bidi.waitUntil(ctx, `document.body.innerText.includes("允许来源")`, { label: "详情页" });
  const page = await text();
  assert.ok(!page.includes(createdSecret), "详情页出现了创建时那把 secret 明文");
  const shapes = JSON.parse(await credentialShapes());
  assert.ok(!shapes.secrets.includes(createdSecret), `DOM 里仍能搜到 secret：${JSON.stringify(shapes)}`);

  // 只读端点逐个扫：任何一处回显 secret 都是红线 4 的破口
  for (const p of [`/api/admin/sites/${createdSiteID}`, "/api/admin/sites",
    `/api/admin/sites/${createdSiteID}/secrets`, `/api/admin/sites/${createdSiteID}/origins`,
    "/api/admin/audit?limit=200", "/api/admin/admin-keys", "/api/admin/generators", "/api/admin/pool"]) {
    const r = await api("GET", p);
    assert.equal(r.status, 200, `${p} = ${r.status}`);
    assert.ok(!r.raw.includes(createdSecret), `${p} 泄露了 secret 明文`);
  }
  const secrets = await api("GET", `/api/admin/sites/${createdSiteID}/secrets`);
  assert.equal(secrets.data.secrets.length, 1);
  assert.ok(secrets.data.secrets[0].key_id, "凭据视图至少有公开 key_id");
  assert.match(page, /任何后续接口都取不回来|取不回来/);
});

// ---------- 6. 改 Origin、难度与限流：保存后与库中一致 ----------

test("修改策略与 Origin：保存、重新读取一致，越界值被服务端拒", async () => {
  requireSite("改策略与 Origin");
  const originA = ADMIN_ORIGIN;               // 真实来源：管理监听器自己
  const originB = "http://admin.example.test:8080";
  const expected = [originA, originB].sort();

  // 难度档：第二个 select 是难度（第一个是模式、第三个是题型；按 id 前缀找）
  const diffSel = mustID(await bidi.evaluate(ctx,
    `(()=>{const s=[...document.querySelectorAll("select")].find(e=>/难度档/.test((document.querySelector('label[for="'+e.id+'"]')||{}).textContent||""));
      return s?s.id:null;})()`), "难度档下拉");
  await bidi.evaluate(ctx, `document.querySelector('#${diffSel}').focus()`);
  await press("ArrowDown");
  await bidi.evaluate(ctx, `document.activeElement.blur()`);

  // PoW bits：选中已有内容再真实敲入新值
  const powID = mustID(await bidi.evaluate(ctx,
    `(()=>{const i=[...document.querySelectorAll("input[type=number]")].find(e=>/PoW bits/.test((document.querySelector('label[for="'+e.id+'"]')||{}).textContent||""));return i?i.id:null;})()`),
    "PoW bits 输入框");
  await bidi.evaluate(ctx, `document.querySelector('#${powID}').focus()`);
  await pressChord("Control", "a");
  await bidi.type(ctx, `#${powID}`, "20");

  // 站点级限流：按 label 文本找 rate_site_rpm
  const rpmID = mustID(await bidi.evaluate(ctx,
    `(()=>{const i=[...document.querySelectorAll("input[type=number]")].find(e=>/按站点|rate_site_rpm/.test((document.querySelector('label[for="'+e.id+'"]')||{}).textContent||""));return i?i.id:null;})()`),
    "按站点限流输入框");
  await bidi.evaluate(ctx, `document.querySelector('#${rpmID}').focus()`);
  await pressChord("Control", "a");
  await bidi.type(ctx, `#${rpmID}`, "420");

  // 名称：真实键盘改
  const nameID = mustID(await bidi.evaluate(ctx,
    `(()=>{const i=[...document.querySelectorAll("input[type=text]")].find(e=>/站点名称/.test((document.querySelector('label[for="'+e.id+'"]')||{}).textContent||""));return i?i.id:null;})()`),
    "站点名称输入框");
  await bidi.evaluate(ctx, `document.querySelector('#${nameID}').focus()`);
  await pressChord("Control", "a");
  await bidi.type(ctx, `#${nameID}`, "gate-site-renamed");

  // Origin 列表：一行一条
  const areaID = mustID(await bidi.evaluate(ctx, `(()=>{const t=document.querySelector("textarea");return t?t.id:null;})()`),
    "Origin 输入区");
  await bidi.evaluate(ctx, `document.querySelector('#${areaID}').focus()`);
  await pressChord("Control", "a");
  await bidi.type(ctx, `#${areaID}`, originA);
  await press("Enter");
  await bidi.type(ctx, `#${areaID}`, originB);

  assert.equal(await bidi.evaluate(ctx, `document.getElementById("view-title").textContent`), "站点详情",
    "编辑期间不该被导航守卫弹走（Origin 与策略同页）");

  // 保存前：未保存修改必须挡住切页（真实点顶栏"站点"）
  await clickText("站点", "button");
  await bidi.waitUntil(ctx, `!document.getElementById("guard").hidden`, { label: "未保存提示横幅" });
  const guard = await text();
  assert.match(guard, /有未保存的修改/);
  await clickText("留下继续编辑");
  assert.equal(await bidi.evaluate(ctx, `document.querySelector('#${nameID}').value`), "gate-site-renamed",
    "选择「留下继续编辑」后，已填内容必须还在");
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("guard").hidden`), true);

  // 保存策略
  await clickText("保存修改");
  await bidi.waitUntil(ctx,
    `(()=>{const s=[...document.querySelectorAll(".status")].find(e=>/已保存|失败|不一致/.test(e.textContent));return !!s;})()`,
    { label: "保存结果" });
  let line = await bidi.evaluate(ctx,
    `(()=>{const s=[...document.querySelectorAll(".status")].find(e=>/已保存|失败|不一致/.test(e.textContent));return s?s.textContent:null;})()`);
  assert.match(line, /已保存，并已重新读取/, line);

  // Origin 单独保存
  await clickText("保存允许来源");
  await bidi.waitUntil(ctx,
    `(()=>{const s=[...document.querySelectorAll(".status")].find(e=>/规范化后共/.test(e.textContent));return !!s;})()`,
    { label: "Origin 保存结果" });
  line = await bidi.evaluate(ctx,
    `(()=>{const s=[...document.querySelectorAll(".status")].find(e=>/规范化后共/.test(e.textContent));return s?s.textContent:null;})()`);
  assert.match(line, /规范化后共 2 条/, line);

  // 对账（一）：服务端存的确实是这些值
  const got = await api("GET", `/api/admin/sites/${createdSiteID}`);
  assert.equal(got.status, 200, `读回站点应 200，got ${got.status} ${got.raw}`);
  assert.ok(got.data && got.data.site, `响应里没有 site 对象：${got.raw}`);
  assert.equal(got.data.site.name, "gate-site-renamed");
  assert.equal(got.data.site.pow_bits, 20);
  assert.equal(got.data.site.difficulty, 2, "难度档应从 1 改成 2");
  assert.equal(got.data.site.rate_site_rpm, 420);
  assert.deepEqual(got.data.origins, expected, "Origin 应与提交的一致（规范化后）");

  // 对账（二）：页面重新读取后显示的也是这些值（保存后组件会重画）
  await openPage();
  await bidi.waitUntil(ctx, `document.body.innerText.includes("站点")`, { label: "重开页面回到登录面" });
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("login").hidden`), false,
    "重新加载后凭据不在了：必须重新登录（内存态凭据的预期行为）");
  await login(ADMIN_KEY);
  await bidi.waitUntil(ctx, `document.body.innerText.includes("gate-site-renamed")`, { label: "列表显示新名字" });
  await clickText("查看");
  await bidi.waitUntil(ctx, `document.body.innerText.includes("允许来源")`, { label: "详情页" });
  const detail = await text();
  assert.match(detail, /gate-site-renamed/);
  assert.match(detail, /420/, `详情页应显示限流：${detail.slice(0, 200)}`);
  assert.match(detail, /20/, "详情页应显示 PoW bits");
  assert.ok(!detail.includes(createdSecret), "改完一轮后详情页仍不得出现 secret");

  // 越界值：前端会挡，但这里要证明**服务端才是权威**（直接把 99 发给 API）
  const bad = await api("PUT", `/api/admin/sites/${createdSiteID}`, { pow_bits: 99 });
  assert.equal(bad.status, 400, `pow_bits=99 应 400，got ${bad.status} ${bad.raw}`);
  const after = await api("GET", `/api/admin/sites/${createdSiteID}`);
  assert.equal(after.data.site.pow_bits, 20, "被拒的写入不得生效");
});

// ---------- 7. 启停：二次确认 + 真实状态与版本语义 ----------

test("停用与启用：确认框展示名称与 site_key，状态与版本是服务端事实", async () => {
  requireSite("启停");
  const before = (await api("GET", `/api/admin/sites/${createdSiteID}`)).data.site;
  assert.ok(before, `站点 ${createdSiteID} 读不到（GET 应返回 site 对象）`);
  assert.equal(before.enabled, true);

  await clickText("停用此站点");
  await bidi.waitUntil(ctx, `document.body.innerText.includes("确认停用")`, { label: "确认横幅" });
  const banner = await text();
  assert.match(banner, /gate-site-renamed/, "确认框必须显示站点名称");
  assert.match(banner, new RegExp(createdSiteKey.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")), "确认框必须显示 site_key");
  assert.match(banner, /停用前签发但尚未消费的 token/, "必须说明对已签发 token 的语义");

  await clickText("确认停用");
  await bidi.waitUntil(ctx, `document.body.innerText.includes("启用此站点")`, { label: "停用后按钮翻面" });
  const stopped = await api("GET", `/api/admin/sites/${createdSiteID}`);
  assert.equal(stopped.data.site.enabled, false);
  assert.ok(stopped.data.site.version > before.version, "停用必须提升站点版本");
  assert.match(await text(), /停用/, "页面状态徽标应为停用");

  await clickText("启用此站点");
  await bidi.waitUntil(ctx, `document.body.innerText.includes("确认启用")`, { label: "启用确认" });
  await clickText("确认启用");
  await bidi.waitUntil(ctx, `document.body.innerText.includes("停用此站点")`, { label: "启用后回到可停用" });
  const started = await api("GET", `/api/admin/sites/${createdSiteID}`);
  assert.equal(started.data.site.enabled, true);
  assert.ok(started.data.site.version > stopped.data.site.version, "重新启用同样提升版本（旧 token 不复活）");
});

// ---------- 8. 题型状态 + 凭据撤不回 + 无第三方资源 ----------

test("题型面板显示实验性与生产就绪的真实结论，页面不请求任何外部源", async () => {
  requireSite("题型结论面板");   // 要点"查看"进详情，前提是列表里确实有一个站点
  const gens = await api("GET", "/api/admin/generators");
  assert.equal(gens.status, 200);
  await openPage();
  await login(ADMIN_KEY);
  await bidi.waitUntil(ctx, `document.body.innerText.includes("查看")`, { label: "列表" });
  await clickText("查看");
  await bidi.waitUntil(ctx, `document.body.innerText.includes("未达生产可用")`, { label: "题型结论徽标" });
  const page = await text();
  // 两个题型的实际结论必须都是"未达生产可用"：这不是样式，是把服务端的 production_ready 原样落地
  const notReady = (page.match(/未达生产可用/g) || []).length;
  assert.ok(notReady >= 1, `页面没有显示未达生产可用：${page.slice(0, 300)}`);
  for (const g of gens.data.generators) {
    assert.equal(g.production_ready, false, `服务端 ${g.id} 的 production_ready 不是 false`);
  }
  assert.match(page, /实验性/);
  assert.equal(await bidi.evaluate(ctx, `document.querySelectorAll("script[src]").length`), 1,
    "只应有一个同源脚本");
  assert.equal(await bidi.evaluate(ctx,
    `[...document.querySelectorAll("script[src]")].every(s=>new URL(s.src,location.href).origin===location.origin)`), true);
  assert.equal(await bidi.evaluate(ctx,
    `[...document.querySelectorAll("link[href]")].every(s=>new URL(s.href,location.href).origin===location.origin)`), true);
  const errs = await bidi.consoleErrors();
  assert.deepEqual(errs.filter((e) => e.startsWith("error:")), [], `控制台有错误：${errs.join(" | ")}`);
});

// ---------- 9. 退出后不能继续查数据 ----------

test("退出：页面回到登录面且无法再读管理数据（凭据是页面丢的，不是服务端失效）", async () => {
  requireSite("退出");
  await clickText("退出");
  await bidi.waitUntil(ctx, `document.getElementById("login").hidden===false`, { label: "回到登录面" });
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("nav").hidden`), true);
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("view").hidden`), true);
  assert.equal(await bidi.evaluate(ctx, `document.getElementById("admin-key").value`), "");
  const viewText = await bidi.evaluate(ctx, `document.getElementById("view-body").textContent`);
  assert.equal(viewText.trim(), "", `退出后视图 DOM 里仍留着管理数据：${viewText.slice(0, 120)}`);

  // 服务端侧这把 key 仍然有效 ⇒ 上面"查不到"是因为页面不再携带凭据
  const still = await api("GET", "/api/admin/whoami");
  assert.equal(still.status, 200, JSON.stringify(still));

  // 未登录的页面不能自己造出数据：视图区里既没有站点名也没有 secret
  const page = await text();
  assert.ok(!page.includes(createdSiteKey), "退出后页面仍在显示站点数据");
  assert.ok(!page.includes(createdSecret), "退出后页面仍在显示 secret");

  const store = await bidi.evaluateJSON(ctx,
    `(async()=>({ls:Object.getOwnPropertyNames(localStorage),ss:Object.getOwnPropertyNames(sessionStorage),cookie:document.cookie}))()`, true);
  assert.deepEqual(store.ls, []);
  assert.deepEqual(store.ss, []);
  assert.equal(store.cookie, "");
});

// ---------- 10. 375px 真实视口下可操作 ----------

test("375×667：视口真的设成 375，登录、创建与保存控件都能命中", async () => {
  await bidi.setViewport(ctx, 375, 667);
  const m = await bidi.viewportMetrics(ctx);
  // 实测（Firefox 147.0.2 / linux-arm64，管理控制台页面）：
  //   window.innerWidth = 375      ⇒ 视口确实设成了 375
  //   documentElement.clientWidth = 363 = 375 − 12px **垂直**滚动条
  // 页面高度 1196 > 667，所以必然出现垂直滚动条，可用宽度就是 363。
  // 判据要盯的是"视口是不是 375"和"有没有横向溢出"，不是 clientWidth==375——
  // 后者在会垂直滚动的页面上**本来就是假的**，把它写成断言等于要求页面不许垂直滚动。
  assert.equal(m.innerWidth, 375, `视口未生效：${JSON.stringify(m)}`);
  assert.ok(m.clientWidth > 0 && m.clientWidth <= 375, `clientWidth 不在 (0,375]：${JSON.stringify(m)}`);
  assert.ok(m.clientWidth >= 363, `可用宽度比"扣掉一条滚动条"还窄，说明视口没设进去：${JSON.stringify(m)}`);

  await openPage();
  await bidi.waitUntil(ctx, `!!document.getElementById("admin-key")`, { label: "登录面" });
  await login(ADMIN_KEY);
  await bidi.waitUntil(ctx, `document.body.innerText.includes("新建站点")`, { label: "窄屏列表" });

  // 窄屏下主要控件仍可命中：中心坐标在视口内
  for (const needle of ["创建站点", "验证身份", "退出"]) {
    const found = await bidi.evaluate(ctx,
      `(()=>{const els=[...document.querySelectorAll("button")];
        const el=els.find(e=>(e.textContent||"").trim()===${JSON.stringify(needle)});
        if(!el) return null; el.scrollIntoView({block:'center'});
        const r=el.getBoundingClientRect();
        return [Math.round(r.left+r.width/2), Math.round(r.top+r.height/2)];})()`);
    assert.ok(found, `窄屏找不到按钮「${needle}」`);
    assert.ok(found[0] >= 0 && found[0] <= m.clientWidth && found[1] >= 0,
      `「${needle}」中心超出视口：${JSON.stringify(found)}`);
  }

  // 窄屏真实点击填表并创建第二个站点
  await typeInto(".create-site input[type=text]", "gate-site-narrow");
  await clickText("创建站点");
  await bidi.waitUntil(ctx, `document.querySelector(".once textarea")`, { label: "窄屏创建结果" });
  const narrow = await text();
  assert.match(narrow, /gate-site-narrow/);
  const s2 = (await api("GET", "/api/admin/sites")).data.sites.find((x) => x.name === "gate-site-narrow");
  assert.ok(s2, "窄屏创建未落库");
  createdSiteID = createdSiteID || s2.id;

  // 无横向溢出：内容不撑破 375
  const after = await bidi.viewportMetrics(ctx);
  assert.ok(after.scrollWidth <= after.clientWidth + 1, `出现横向滚动：${JSON.stringify(after)}`);
  assert.ok((after.bodyScrollWidth ?? 0) <= after.clientWidth + 1, `body 溢出：${JSON.stringify(after)}`);

  // 窄屏下也能进详情并保存一次（表单不是只能看）
  await clickText("站点", "button");
  await bidi.waitUntil(ctx, `document.body.innerText.includes("查看")`, { label: "列表" });
  await clickText("查看");
  await bidi.waitUntil(ctx, `document.body.innerText.includes("保存修改")`, { label: "详情表单" });
  await clickText("保存修改");
  await bidi.waitUntil(ctx,
    `(()=>{const s=[...document.querySelectorAll(".status")].find(e=>/没有需要保存|已保存|失败/.test(e.textContent));return !!s;})()`,
    { label: "窄屏保存结果" });
  const line = await bidi.evaluate(ctx,
    `(()=>{const s=[...document.querySelectorAll(".status")].find(e=>/没有需要保存|已保存|失败/.test(e.textContent));return s?s.textContent:null;})()`);
  assert.match(line, /没有需要保存的修改|已保存/, line);
});

// ---------- 11. 429 与"连不上"必须有不同的说法（放在最后：会打满鉴权失败窗口） ----------

test("连续错误 key 触发 429 时不说成密码错误；服务真不可达时说明是连接问题", async () => {
  // 服务端的失败计数是"每分钟 10 次"（maxAuthFailRPM），第 11 次失败开始返回 429。
  // 每次尝试都串行等待结果：按钮在请求在途时是禁用的，不等就会点空。
  let line = "";
  for (let i = 0; i < 16; i++) {
    await bidi.evaluate(ctx, `document.getElementById("login-status").textContent = ""`);
    await typeInto("#admin-key", BAD_ADMIN_KEY);
    await clickText("验证身份");
    await bidi.waitUntil(ctx, `document.getElementById("login-status").textContent.length > 0`,
      { label: "第 " + (i + 1) + " 次尝试的结果", timeoutMs: 20000 });
    line = await status("#login-status");
    if (line.includes("请求过于频繁")) break;
  }
  assert.match(line, /请求过于频繁/, `16 次错误凭据后仍未出现限流文案：${line}`);
  assert.ok(!/密码|凭据无效/.test(line), `429 被说成了凭据问题：${line}`);

  // 限流只针对失败计数：正确凭据此时仍能进来（这是设计语义，不是漏洞）
  await typeInto("#admin-key", ADMIN_KEY);
  await clickText("验证身份");
  await bidi.waitUntil(ctx, `document.getElementById("session-state").classList.contains("on")`,
    { label: "正确凭据仍可登录" });
  await clickText("退出");

  // 服务进程停掉 ⇒ fetch 失败。这条必须在最后：之后没有可用的服务了。
  process.kill(Number(ADMIN_SERVER_PID), "SIGTERM");
  const t0 = Date.now();
  for (;;) {
    try {
      const r = await fetch(ADMIN_API + "/api/admin/whoami", { headers: { Authorization: "Bearer " + ADMIN_KEY } });
      await r.text();
    } catch {
      break;                                  // 端口已关闭
    }
    if (Date.now() - t0 > 15000) throw new Error("服务进程没有在 15s 内退出");
    await new Promise((r) => setTimeout(r, 100));
  }
  await openPage().catch(() => {});           // 服务没了，页面加载会失败：改用已加载的页面
  const onScreen = await bidi.evaluate(ctx, `!!document.getElementById("admin-key")`);
  if (!onScreen) {
    // 页面已经拿不到了：重新打开一个仍在本会话里的空白上下文没有意义，
    // 直接在当前文档里发真实 fetch（同源仍指向已关闭的端口）并驱动登录表单的文案路径。
    await bidi.evaluate(ctx, `document.getElementById("login").hidden=false`);
  }
  await typeInto("#admin-key", ADMIN_KEY);
  await clickText("验证身份");
  await bidi.waitUntil(ctx,
    `document.getElementById("login-status").textContent.includes("无法连接管理 API")`,
    { label: "连接失败文案", timeoutMs: 25000 });
  const dead = await status("#login-status");
  assert.match(dead, /无法连接管理 API/);
  assert.ok(!dead.includes("凭据无效"), `连接失败被说成了凭据问题：${dead}`);
});
