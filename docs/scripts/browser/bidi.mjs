// 零安装的浏览器自动化客户端：Firefox 自带 WebDriver BiDi 端点 + Node 内置 WebSocket。
//
// 为什么不用 geckodriver / playwright：本机没有驱动，也不该为验收下载浏览器二进制。
// Firefox 147 的 `--remote-debugging-port` 直接在 ws://127.0.0.1:PORT/session 上提供
// WebDriver BiDi；Node 21+ 有全局 WebSocket。两端都在，不需要任何第三方包。
//
// 协议细节不是凭记忆写的：sessionless 连接上先发 `session.new`（Firefox 的
// WebDriverBiDiConnection 把 `session.new` / `session.status` 特判为静态命令），
// **同一条连接**随后就是会话连接（不需要再连 webSocketUrl）。
// pointer 的 `origin` 只接受 "viewport"/"pointer"/element 字符串形式之外的东西，
// 所以点击用视口坐标（由 getBoundingClientRect 算出），这点是实测出来的。

export class BiDi {
  constructor(url) {
    this.url = url;
    this.nextId = 0;
    this.pending = new Map();
    this.events = [];
    this.ws = null;
  }

  async connect() {
    this.ws = new WebSocket(this.url);
    await new Promise((resolve, reject) => {
      this.ws.addEventListener("open", resolve);
      this.ws.addEventListener("error", () => reject(new Error("WebSocket 连接失败：" + this.url)));
    });
    this.ws.addEventListener("message", (e) => {
      let m;
      try { m = JSON.parse(e.data); } catch { return; }
      if (m.id && this.pending.has(m.id)) {
        const p = this.pending.get(m.id);
        this.pending.delete(m.id);
        if (m.error) p.reject(new Error(`${p.label} → ${JSON.stringify(m.error)}`));
        else p.resolve(m.result);
      } else if (m.method) {
        this.events.push(m);
      }
    });
    return this;
  }

  send(method, params = {}, timeoutMs = 20000) {
    const id = ++this.nextId;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject, label: method });
      // **文本帧**：Firefox 的 BiDi 端点只认 text frame，发二进制帧它会完全不回复
      // （表现是"命令超时"，而不是协议错误 —— 很容易误判成浏览器坏了）。
      this.ws.send(JSON.stringify({ id, method, params }));
      setTimeout(() => {
        if (this.pending.delete(id)) reject(new Error(`${method} 超时 ${timeoutMs}ms`));
      }, timeoutMs);
    });
  }

  async startSession() {
    const r = await this.send("session.new", { capabilities: { alwaysMatch: { browserName: "firefox" } } });
    this.sessionId = r.sessionId;
    return r;
  }

  async newTab() {
    return (await this.send("browsingContext.create", { type: "tab" })).context;
  }

  async goto(context, url, wait = "complete") {
    await this.send("browsingContext.navigate", { context, url, wait });
  }

  async evaluate(context, expression, awaitPromise = false) {
    const r = await this.send("script.evaluate", {
      expression, target: { context }, awaitPromise, resultOwnership: "none",
    });
    if (r.exceptionDetails) {
      throw new Error(`页面脚本异常：${r.exceptionDetails.text} @ ${r.exceptionDetails.line || "?"}`);
    }
    return unwrap(r.result);
  }

  // evaluateJSON：把返回值在页面里 JSON 化后取回。
  // Firefox 的 BiDi 对**对象**结果给的是 `{type:"object", handle:…}`（不含 value），
  // 于是直接返回对象的表达式在 Node 侧会变成空对象——数字/字符串/数组才有内联值。
  // 需要对象就在页面序列化，别依赖跨 realm 的句柄。
  async evaluateJSON(context, expression, awaitPromise = false) {
    const s = await this.evaluate(context, `JSON.stringify((${expression}))`, awaitPromise);
    if (typeof s !== "string") return s ?? null;
    try { return JSON.parse(s); } catch { return s; }
  }

  // waitUntil 轮询一个在页面里求值的布尔表达式；比"sleep 一下"更接近真实等待条件。
  async waitUntil(context, expression, { timeoutMs = 15000, everyMs = 60, label = expression } = {}) {
    const t0 = Date.now();
    for (;;) {
      let v;
      try { v = await this.evaluate(context, expression, true); } catch (e) { v = undefined; }
      if (v) return v;
      if (Date.now() - t0 > timeoutMs) throw new Error(`等待条件超时：${label}`);
      await new Promise((r) => setTimeout(r, everyMs));
    }
  }

  // 先滚进视口再算坐标：BiDi 的鼠标动作对**视口外**坐标直接报 "move target out of bounds"
  // （headless 视口只有一屏高，长页面里靠下的按钮必然在视口外）。
  async centerOf(context, selector) {
    return this.evaluate(context,
      `(()=>{const el=document.querySelector(${JSON.stringify(selector)});
        if(!el) return null;
        el.scrollIntoView({block:'center', inline:'center'});
        const r=el.getBoundingClientRect();
        return [Math.round(r.left+r.width/2), Math.round(r.top+r.height/2)];})()`);
  }

  // deepCenter：穿过 shadow root 找元素中心（widget 全在 shadow DOM 里，普通选择器进不去）。
  // 交互仍然是**真实鼠标事件**：这里只是算坐标，不代替点击。
  async deepCenter(context, hostSelector, innerSelector) {
    return this.evaluate(context,
      `(()=>{const h=document.querySelector(${JSON.stringify(hostSelector)});
        const hs=h && h.querySelector('[data-epocha-widget]');
        const el=hs && hs.shadowRoot && hs.shadowRoot.querySelector(${JSON.stringify(innerSelector)});
        if(!el) return null;
        el.scrollIntoView({block:'center', inline:'center'});
        const r=el.getBoundingClientRect();
        return [Math.round(r.left+r.width/2), Math.round(r.top+r.height/2)];})()`);
  }

  async clickDeep(context, hostSelector, innerSelector) {
    const pos = await this.deepCenter(context, hostSelector, innerSelector);
    if (!pos) throw new Error(`shadow DOM 里找不到元素：${hostSelector} → ${innerSelector}`);
    return this.pointerClick(context, pos[0], pos[1]);
  }

  async pointerClick(context, x, y) {
    await this.send("input.performActions", {
      context,
      actions: [{
        type: "pointer", id: "p" + Math.random().toString(36).slice(2, 7),
        parameters: { pointerType: "mouse" },
        actions: [
          { type: "pointerMove", x, y, duration: 10 },
          { type: "pointerDown", button: 0, width: 1, height: 1, pressure: 0.5 },
          { type: "pointerUp", button: 0, width: 1, height: 1, pressure: 0 },
        ],
      }],
    });
    await this.send("input.releaseActions", { context }).catch(() => {});
    return [x, y];
  }

  async click(context, selector) {
    const pos = await this.centerOf(context, selector);
    if (!pos) throw new Error(`找不到元素：${selector}`);
    return this.pointerClick(context, pos[0], pos[1]);
  }

  async type(context, selector, text, { enter = false } = {}) {
    await this.evaluate(context, `document.querySelector(${JSON.stringify(selector)}).focus()`);
    const keyActions = [];
    for (const ch of String(text)) {
      keyActions.push({ type: "keyDown", value: ch });
      keyActions.push({ type: "keyUp", value: ch });
    }
    if (enter) {
      keyActions.push({ type: "keyDown", value: "Enter" });
      keyActions.push({ type: "keyUp", value: "Enter" });
    }
    await this.send("input.performActions", {
      context, actions: [{ type: "key", id: "k" + Math.random().toString(36).slice(2, 7), actions: keyActions }],
    });
  }

  // 把 shadow DOM 里的文本取出来（widget 的状态区在 shadow root 里，选择器穿不进去）。
  async shadowText(context, hostSelector, innerSelector) {
    return this.evaluate(context,
      `(()=>{const h=document.querySelector(${JSON.stringify(hostSelector)});
        const s=h && h.querySelector('[data-epocha-widget]');
        const r=s && s.shadowRoot;
        const n=r && r.querySelector(${JSON.stringify(innerSelector)});
        return n ? n.textContent : null;})()`);
  }

  // setViewport：按 W3C WebDriver BiDi 规范调 `browsingContext.setViewport`。
  // 参数形状是 `{context, viewport:{width,height}|null, devicePixelRatio?}`。
  //
  // 为什么显式写这个方法而不是"启动时加个窗口尺寸参数"：headless Firefox 默认视口
  // 1366×768，把 CSS 容器缩到 375px **不会**改变 window.innerWidth，于是"测了窄屏"
  // 其实是"测了宽屏上的一个小盒子"——媒体查询、换行、溢出全都不是 375px 那套。
  // 调用成功不代表浏览器一定应用了，所以调用方**必须**再断言 innerWidth/clientWidth
  // （widget_browser.test.mjs 的窄屏用例就是这么做的）。
  //
  // 实测（Firefox 147.0.2 / linux-arm64）：该命令真实生效，375×667 与 414×896 都能设进去，
  // devicePixelRatio 也可覆写。旧文档里"BiDi 不提供视口命令"的说法是错的。
  async setViewport(context, width, height, devicePixelRatio = null) {
    const params = { context, viewport: { width, height } };
    if (devicePixelRatio !== null) params.devicePixelRatio = devicePixelRatio;
    return this.send("browsingContext.setViewport", params);
  }

  // viewportMetrics：读回浏览器**自己**认为的视口尺寸。
  // 用它来证明 setViewport 真的生效了，而不是相信启动参数或命令返回值。
  async viewportMetrics(context) {
    return this.evaluateJSON(context,
      `({innerWidth:window.innerWidth, clientWidth:document.documentElement.clientWidth,
          innerHeight:window.innerHeight, devicePixelRatio:window.devicePixelRatio,
          scrollWidth:document.documentElement.scrollWidth,
          bodyScrollWidth:(document.body?document.body.scrollWidth:null),
          docScrollWidth:document.scrollingElement?document.scrollingElement.scrollWidth:null})`);
  }

  // subscribeEvents：订阅 BiDi 事件（log.entryAdded 等）。
  // `contexts` 省略时**不发送**该字段 = 订阅整个会话的所有上下文；传空数组在规范里含义不同，
  // 所以这里刻意不把它折叠成 []。
  async subscribeEvents(events, contexts) {
    const params = { events };
    if (Array.isArray(contexts) && contexts.length > 0) params.contexts = contexts;
    return this.send("session.subscribe", params);
  }

  async consoleErrors() {
    return this.events
      .filter((e) => e.method === "log.entryAdded" && (e.params.level === "error" || e.params.level === "warn"))
      .map((e) => `${e.params.level}:${e.params.source}:${String(e.params.text).slice(0, 200)}`);
  }

  async closeTab(context) { await this.send("browsingContext.close", { context }).catch(() => {}); }

  close() { if (this.ws && this.ws.readyState === 1) this.ws.close(); }
}

function unwrap(v) {
  if (!v || typeof v !== "object") return v;
  if (v.type === "none") return null;
  if (v.type === "string" || v.type === "number" || v.type === "boolean") return v.value;
  if (v.type === "bigint") return v.value;
  if (v.type === "array") return (v.value || []).map(unwrap);
  if (v.type === "object") {
    const out = {};
    for (const p of v.value || []) out[p.key] = unwrap(p.value);
    return out;
  }
  return v.value === undefined ? v : v.value;
}

// 订阅 BiDi 事件：JS 异常、console、网络请求都要能看见，否则"页面没报错"是没测到的同义反复。
export async function subscribe(bidi, events, contexts) {
  await bidi.send("session.subscribe", { events, contexts });
}
