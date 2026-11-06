/* EpoCHA 管理控制台。原生 JS，零依赖、零构建，不加载任何第三方资源。
 *
 * 关于"凭据只活在内存里"的诚实边界（必读）：
 *   - 管理凭据只存在本闭包的 session.key：不写 localStorage / sessionStorage / IndexedDB，
 *     不进 URL 与 hash，不进 console，不进任何"配置对象"；
 *   - 登录成功后立刻清空输入框里的原文；
 *   - 退出把引用置空字符串。JavaScript **不能**保证物理内存里的历史副本即刻消失，
 *     这一条不假装做到；它防的是"凭据被写进可被他人读取的持久介质"。
 *
 * 后端对每一个 /api/admin/* 请求都做 Bearer 校验。登录界面不是鉴权，只是递交凭据的载体。
 *
 * 这里的 BOUNDS 是**体验层**校验，权威判定始终在服务端（internal/store 的 Validate 与 DB CHECK）。
 * 数值与 internal/store/{sites,store}.go 和 internal/pow 的常量一一对应；
 * 两边不一致时以服务端为准，浏览器验收里有"越界值必须被服务端拒"的用例来钉这件事。
 */
"use strict";
(function () {
  var API_PREFIX = "/api/admin/";
  var PAGE = 20;

  var BOUNDS = {
    nameMax: 64,
    siteKeyMax: 64,
    powMin: 12, powMax: 24,
    ttlMin: 1, ttlMax: 3600,
    rpmMin: 1, rpmMax: 100000,
    diffMin: 1, diffMax: 5,
    originMax: 253,
    originCountMax: 32
  };

  // ---------- 最小 DOM 助手（只有 textContent，没有 innerHTML）----------

  function el(tag, props, kids) {
    var n = document.createElement(tag);
    if (props) {
      Object.keys(props).forEach(function (k) {
        var v = props[k];
        if (v === null || v === undefined || v === false) return;
        if (k === "class") n.className = v;
        else if (k === "text") n.textContent = String(v);
        // value/checked 走 IDL 属性：setAttribute 对表单当前值不生效（用户改过之后更是如此）。
        else if (k === "value") n.value = String(v);
        else if (k === "checked") { if (v === true) n.checked = true; }
        else if (k.length > 2 && k.slice(0, 2) === "on") n.addEventListener(k.slice(2).toLowerCase(), v);
        else if (v === true) n.setAttribute(k, "");
        else n.setAttribute(k, String(v));
      });
    }
    (kids || []).forEach(function (c) {
      if (c === null || c === undefined || c === false) return;
      n.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    });
    return n;
  }
  function div(props, kids) { return el("div", props, kids); }
  function p(text, cls) { return el("p", { class: cls, text: text }); }
  function h2(text) { return el("h2", { text: text }); }
  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }
  function label(text, target, cls) { return el("label", { for: target, class: cls, text: text }); }

  var uid = 0;
  function nextID(prefix) { uid += 1; return prefix + "-" + uid; }

  function field(id, labelText, node, note) {
    return div({ class: "field" }, [
      label(labelText, id),
      node,
      note ? div({ class: "note", text: note }) : null
    ]);
  }

  function textInput(id, value, opts) {
    var o = opts || {};
    return el("input", {
      type: "text", id: id, value: value == null ? "" : String(value),
      maxlength: o.maxlength || 253, autocomplete: "off", spellcheck: "false",
      placeholder: o.placeholder || null, "aria-describedby": o.describedby || null
    });
  }

  function numberInput(id, value, min, max) {
    return el("input", {
      type: "number", id: id, value: String(value),
      min: min, max: max, step: 1, inputmode: "numeric",
      "aria-describedby": id + "-note"
    });
  }

  var toastTimer = 0;
  function toast(msg) {
    var t = document.getElementById("toast");
    t.textContent = msg;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.textContent = ""; }, 4000);
  }

  // ---------- 会话与请求 ----------

  var session = { key: "", keyID: "", generation: 0 };
  var viewGeneration = 0;
  var pendingRequests = new Set();
  var requestTimeoutMS = 10000;
  var maxResponseBytes = 256 * 1024;
  var writesInFlight = 0;

  function scope() { return { session: session.generation, view: viewGeneration }; }
  function current(ticket) {
    return ticket && ticket.session === session.generation && ticket.view === viewGeneration;
  }
  function abortRequests() {
    pendingRequests.forEach(function (controller) { controller.abort(); });
    pendingRequests.clear();
  }
  function dropSession() {
    session.generation += 1;
    session.key = "";
    session.keyID = "";
    state.generators = null;
    state.pendingNav = null;
    state.siteCursor = [];
    abortRequests();
  }

  // 请求路径只能由本页的管理 API 路径构成，凭据不会发给调用方指定的上游。
  function apiURL(path) {
    if (typeof path !== "string" || path.slice(0, API_PREFIX.length) !== API_PREFIX) {
      throw new Error("非管理 API 路径：" + String(path));
    }
    return window.location.origin + path;
  }

  async function readResponse(resp) {
    var declared = resp.headers && resp.headers.get("Content-Length");
    if (declared && Number(declared) > maxResponseBytes) throw new Error("response-too-large");
    if (resp.body && resp.body.getReader) {
      var reader = resp.body.getReader();
      var decoder = new TextDecoder();
      var total = 0, text = "";
      try {
        for (;;) {
          var chunk = await reader.read();
          if (chunk.done) break;
          total += chunk.value.byteLength;
          if (total > maxResponseBytes) {
            reader.cancel().catch(function () {});
            throw new Error("response-too-large");
          }
          text += decoder.decode(chunk.value, { stream: true });
        }
        return text + decoder.decode();
      } finally { reader.releaseLock(); }
    }
    var raw = await resp.text();
    if (new TextEncoder().encode(raw).byteLength > maxResponseBytes) throw new Error("response-too-large");
    return raw;
  }

  // 超时覆盖响应体；即使传输层忽略 abort，race 也会结束等待，迟到结果不能再更新页面。
  async function call(method, path, body) {
    var ticket = scope();
    var controller = new AbortController();
    var timedOut = false;
    var opts = { method: method, headers: {}, credentials: "same-origin", cache: "no-store", signal: controller.signal };
    if (session.key) opts.headers["Authorization"] = "Bearer " + session.key;
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    pendingRequests.add(controller);
    var interrupted = new Promise(function (resolve) {
      controller.signal.addEventListener("abort", function () {
        resolve({ status: 0, data: null, netError: true, timeout: timedOut });
      }, { once: true });
    });
    var timer = setTimeout(function () { timedOut = true; controller.abort(); }, requestTimeoutMS);
    try {
      var res = await Promise.race([interrupted, (async function () {
        var resp = await fetch(apiURL(path), opts);
        var raw = await readResponse(resp);
        var data = null;
        if (raw) { try { data = JSON.parse(raw); } catch (e) { /* 由调用点校验响应形状 */ } }
        return { status: resp.status, data: data, netError: false };
      })()]);
      res.scope = ticket;
      return res;
    } catch (e) {
      controller.abort();
      return { status: 0, data: null, netError: true, timeout: timedOut, scope: ticket };
    } finally {
      clearTimeout(timer);
      pendingRequests.delete(controller);
    }
  }

  function failReason(res) {
    if (res.timeout) return "管理 API 请求超时，无法确认本次结果。请只读复核当前状态。";
    if (res.netError) return "无法连接管理 API（服务未启动或网络不通）。这不是凭据问题；写入结果尚未确认，请只读复核。";
    switch (res.status) {
      case 400: return "请求被服务端拒绝：字段不符合边界（400）。" + errCode(res);
      case 401: return "管理凭据无效或已失效。";
      case 403: return "该凭据没有此项操作的权限（服务拒绝了这个请求）。";
      case 404: return "没找到目标对象。";
      case 409: return "与现有记录冲突（站点名或 site_key 已存在）。";
      case 429: return "请求过于频繁，请稍后再试。";
      default:
        if (res.status >= 500) return "服务端错误（" + res.status + "），本次改动的结果尚未确认。请只读复核，不要自动重复提交。";
        return "服务响应无法确认（" + res.status + "）。" + errCode(res);
    }
  }

  function errCode(res) {
    var c = res && res.data && res.data.error;
    return c ? "错误码：" + String(c) + "。" : "";
  }

  // 旧会话与旧视图的响应，包括 401，不能影响当前会话。
  function handleAuth(res) {
    if (!current(res.scope)) return true;
    if (res.status !== 401) return false;
    dropSession();
    state.view = "login";
    state.reason = "管理凭据已失效，请重新输入。";
    render();
    return true;
  }

  function lockControls(host) {
    var controls = Array.prototype.map.call(host.querySelectorAll("input, select, textarea, button"), function (node) {
      var item = { node: node, disabled: node.disabled };
      node.disabled = true;
      return item;
    });
    return function () { controls.forEach(function (item) { item.node.disabled = item.disabled; }); };
  }

  function beginWrite(host, ticket) {
    var unlock = lockControls(host);
    writesInFlight += 1;
    return function () {
      unlock();
      if (current(ticket)) writesInFlight -= 1;
    };
  }

  // ---------- 视图状态 ----------

  var state = {
    view: "login",
    arg: null,
    reason: "",
    dirty: false,
    pendingNav: null,
    siteCursor: [],      // 上一页游标栈（API 只有前进游标，后退由页面自己记）
    generators: null     // GET /api/admin/generators 的缓存（本页面会话内）
  };

  // 未保存的 Origin 编辑。详情页的"策略"和"来源"是两张表单、两个端点，
  // 而保存策略会重画整个详情——没有这份草稿，保存策略就会顺手抹掉还没存的来源修改。
  var originsDraft = null;

  function markDirty(flag) { state.dirty = !!flag; }

  // 详情页同时挂着"策略"和"来源"两张表单，它们共用一个导航脏标记。
  // 如果只有一个布尔量，"保存完来源"会把"策略还没保存"这件事一起抹掉——
  // 于是切页时不提示，未保存的修改直接丢失。所以按来源分部记账。
  var dirtyParts = {};
  function setDirty(part, flag) {
    dirtyParts[part] = !!flag;
    var any = false;
    Object.keys(dirtyParts).forEach(function (k) { if (dirtyParts[k]) any = true; });
    markDirty(any);
  }
  function resetDirty() { dirtyParts = {}; markDirty(false); }

  // 有未保存修改时，切页要先把决定权交回给人（不是 window.confirm：它不可样式化、不可断言）。
  function navigate(view, arg, resetCursor, cursor) {
    if (writesInFlight) {
      toast("写入正在确认，请等待结果后再切换页面。退出仍可清除会话，但不会撤销服务端已收到的操作。");
      return;
    }
    if (state.dirty) {
      state.pendingNav = { view: view, arg: arg, resetCursor: resetCursor, cursor: cursor };
      render();
      return;
    }
    state.pendingNav = null;
    originsDraft = null;
    if (resetCursor) state.siteCursor = [];
    if (cursor) state.siteCursor = cursor;
    state.view = view;
    state.arg = arg;
    state.dirty = false;
    render();
  }

  // 创建表单的"脏"判据：任何输入即脏。配置表单不用它（那里与初始值逐项比对，见 bindFields）。
  function bindDirty(form, part) {
    var controls = Array.prototype.filter.call(form.elements, function (f) {
      return f.tagName === "INPUT" || f.tagName === "SELECT" || f.tagName === "TEXTAREA";
    });
    var init = controls.map(function (f) { return f.type === "checkbox" ? String(f.checked) : f.value; });
    function check() {
      var touched = controls.some(function (f, i) {
        var now = f.type === "checkbox" ? String(f.checked) : f.value;
        return now !== init[i];
      });
      setDirty(part, touched);
      return touched;
    }
    form.addEventListener("input", check);
    form.addEventListener("change", check);
    form.epochaClean = function () {
      controls.forEach(function (f, i) {
        if (f.type === "checkbox") f.checked = init[i] === "true";
        else f.value = init[i];
      });
      setDirty(part, false);
    };
    setDirty(part, false);
  }

  // bindFields 是配置表单的脏判据：**与服务端给回的初始值逐项比对**，不是"字段非空"。
  // 同一个描述表既算脏、又算"这次到底改了哪些字段"（部分更新，没改的不发）。
  function bindFields(fields, part) {
    function changed() {
      return fields.filter(function (f) { return f.get() !== f.base; });
    }
    fields.forEach(function (f) {
      var node = f.node;
      var evs = node.type === "checkbox" || node.tagName === "SELECT" ? ["change"] : ["input", "change"];
      evs.forEach(function (ev) {
        node.addEventListener(ev, function () { setDirty(part, changed().length > 0); });
      });
    });
    return { changed: changed, reset: function (list) { list.forEach(function (f) { f.base = f.get(); }); } };
  }

  // ---------- 外壳（标题栏 / 导航 / 登录面板）----------

  function paintShell() {
    var logged = !!session.key;
    var login = document.getElementById("login");
    var nav = document.getElementById("nav");
    var main = document.getElementById("view");
    var chip = document.getElementById("session-state");
    var out = document.getElementById("logout");

    login.hidden = logged;
    nav.hidden = !logged;
    main.hidden = !logged;
    out.hidden = !logged;
    if (logged) {
      chip.textContent = "已登录：" + session.keyID;
      chip.classList.add("on");
    } else {
      chip.textContent = "未登录";
      chip.classList.remove("on");
    }
    Array.prototype.forEach.call(nav.querySelectorAll(".tab"), function (b) {
      var on = b.dataset.view === state.view ||
        (state.view === "site" && b.dataset.view === "sites");
      if (on) b.setAttribute("aria-current", "page");
      else b.removeAttribute("aria-current");
    });
  }

  // ---------- 渲染入口 ----------

  var VIEWS = {};

  function render() {
    var body = document.getElementById("view-body");
    var title = document.getElementById("view-title");
    var guard = document.getElementById("guard");
    if (!session.key) {
      viewGeneration += 1;
      writesInFlight = 0;
      abortRequests();
      // 未登录 = 屏幕上不该还留着上一段会话的管理数据。退出与 401 都走这里，
      // 先把视图内容与草稿清干净再画登录面（敏感页面状态的清理由这一条保证）。
      clear(body);
      clear(guard);
      guard.hidden = true;
      resetDirty();
      originsDraft = null;
      paintShell();
      title.textContent = "";
      prepareLogin();
      // 401 之后回到登录面要把"为什么"带过来，而不是让人猜是网络问题。
      if (state.reason && loginLine) {
        loginLine.className = "status bad";
        loginLine.textContent = state.reason;
        state.reason = "";
        loginInput.focus();
      }
      return;
    }
    paintShell();
    if (state.pendingNav) {
      // 横幅**叠加**在表单之上，不重建内容：一旦重建，"留下继续编辑"拿回来的就是空表单，
      // 那等于用"提示未保存"这个动作本身把未保存的修改抹掉了。
      title.textContent = "离开当前页面？";
      clear(guard);
      guard.appendChild(renderNavGuard());
      guard.hidden = false;
      var first = guard.querySelector("button");
      if (first) first.focus();
      return;
    }
    viewGeneration += 1;
    writesInFlight = 0;
    abortRequests();
    guard.hidden = true;
    clear(guard);
    clear(body);
    resetDirty();
    var v = VIEWS[state.view] || VIEWS.sites;
    title.textContent = v.title;
    v.render(body, state.arg, scope());
    title.focus();
  }

  // 只换标题、不重画 #view-body：用于"留下继续编辑"时把横幅撤掉而保住已填内容。
  function dismissGuard() {
    state.pendingNav = null;
    var guard = document.getElementById("guard");
    guard.hidden = true;
    clear(guard);
    var v = VIEWS[state.view] || VIEWS.sites;
    document.getElementById("view-title").textContent = v.title;
    document.getElementById("view-title").focus();
  }

  function renderNavGuard() {
    var target = state.pendingNav;
    return div({ class: "banner warn" }, [
      p("当前表单有未保存的修改。"),
      div({ class: "panel-actions" }, [
        el("button", {
          type: "button", text: "放弃修改并离开", onclick: function () {
            state.pendingNav = null;
            resetDirty();
            navigate(target.view, target.arg, target.resetCursor, target.cursor);
          }
        }),
        el("button", {
          type: "button", class: "ghost", text: "留下继续编辑", onclick: dismissGuard
        })
      ])
    ]);
  }

  // ---------- 登录 ----------

  var loginPrepared = false;
  var loginInput = null;
  var loginLine = null;

  function prepareLogin() {
    if (loginPrepared) return;
    loginPrepared = true;
    var form = document.getElementById("login-form");
    var input = loginInput = document.getElementById("admin-key");
    var btn = document.getElementById("login-submit");
    var line = loginLine = document.getElementById("login-status");

    form.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      if (btn.disabled) return;
      var key = input.value;
      if (!key) {
        line.className = "status bad";
        line.textContent = "请输入管理凭据。";
        input.focus();
        return;
      }
      btn.disabled = true;
      input.disabled = true;
      line.className = "status wait";
      line.textContent = "正在验证身份…";
      // 凭据只在**这一次**请求头里；先清掉输入框，成功与否都不留原文。
      input.value = "";
      dropSession();
      session.key = key;
      var ticket = scope();
      var res = await call("GET", API_PREFIX + "whoami");
      if (!current(ticket)) return;
      if (res.status === 200 && res.data && res.data.key_id) {
        session.keyID = String(res.data.key_id);
        line.className = "status ok";
        line.textContent = "";
        state.reason = "";
        state.view = "sites";
        state.arg = null;
        state.siteCursor = [];
        markDirty(false);
        render();
        toast("已登录");
      } else {
        dropSession();
        input.value = "";
        line.className = "status bad";
        line.textContent = failReason(res);
        btn.disabled = false;
        input.disabled = false;
        input.focus();
      }
      btn.disabled = false;
      input.disabled = false;
    });
  }

  // ---------- 题型元数据（GET /api/admin/generators）----------

  async function loadGenerators() {
    if (state.generators) return state.generators;
    var res = await call("GET", API_PREFIX + "generators");
    if (handleAuth(res) || res.status !== 200) return null;
    state.generators = res.data || null;
    return state.generators;
  }

  function genLifecycle(g) {
    if (!g) return null;
    return { experimental: g.experimental === true, production_ready: g.production_ready === true };
  }

  // 实验题型的真实状态必须在页面上出现：arithmetic-baseline-v1 的题面可被脚本直接求值，
  // text-image-v1 的抗 OCR/抗多模态能力**未经**评测。控制台的措辞不能让人以为
  // 它们是已验证的生产防线（DESIGN §8.4、红线 14）——所以这两个徽标直接取自服务端的布尔值。
  function paintLifecycle(host, gens, id) {
    clear(host);
    var lc = genLifecycle(findGen(gens, id));
    if (!lc) {
      host.appendChild(el("span", { class: "pill off", text: "本进程未登记" }));
      return;
    }
    host.appendChild(el("span", {
      class: "pill " + (lc.experimental ? "experimental" : "on"),
      text: lc.experimental ? "实验性" : "非实验性"
    }));
    host.appendChild(el("span", {
      class: "pill " + (lc.production_ready ? "on" : "off"),
      text: lc.production_ready ? "生产可用" : "未达生产可用"
    }));
  }

  function genLabel(g) {
    var lc = genLifecycle(g);
    var tail = lc ? (lc.production_ready ? "生产可用" : "未达生产可用") : "状态未知";
    return g.id + " · " + (g.media || "未知介质") + " · " + (g.version || "无版本") + " · " + tail;
  }

  // 下拉选项：本进程登记的题型 + 站点当前值（即使它已不在登记表里，也要能如实显示）。
  function genOptions(gens, current) {
    var ids = [];
    var seen = {};
    ((gens && gens.generators) || []).forEach(function (g) {
      if (!seen[g.id]) { seen[g.id] = g; ids.push(g.id); }
    });
    var opts = ids.map(function (id) {
      return el("option", { value: id, text: genLabel(seen[id]), selected: id === current });
    });
    if (current && !seen[current]) {
      opts.unshift(el("option", {
        value: current, text: current + " · 本进程未登记（领题会直接失败）", selected: true
      }));
    }
    return opts;
  }

  // 生成器池是多选，不是第二个 generator_id。旧版表单不带这个字段时，
  // 服务端会保留原值；这里也只在用户真的改动时才发送 generator_pool。
  function poolPicker(gens, selected) {
    var chosen = {};
    (selected || []).forEach(function (id) { chosen[id] = true; });
    var ids = [];
    var seen = {};
    ((gens && gens.generators) || []).forEach(function (g) {
      if (!seen[g.id]) { seen[g.id] = g; ids.push(g.id); }
    });
    (selected || []).forEach(function (id) {
      if (!seen[id]) { seen[id] = { id: id, media: "本进程未登记" }; ids.push(id); }
    });
    var boxes = [];
    var root = div({ class: "poolbox" });
    ids.forEach(function (id) {
      var box = el("input", { type: "checkbox", value: id, checked: !!chosen[id] });
      boxes.push(box);
      var g = seen[id];
      root.appendChild(el("label", { class: "poolitem" }, [
        box, " " + (g.version ? genLabel(g) : id + " · 本进程未登记")
      ]));
    });
    function get() {
      var out = [];
      boxes.forEach(function (b) { if (b.checked) out.push(b.value); });
      return out.sort();
    }
    return {
      root: root,
      get: get,
      base: JSON.stringify((selected || []).slice().sort())
    };
  }

  function findGen(gens, id) {
    var list = (gens && gens.generators) || [];
    for (var i = 0; i < list.length; i++) if (list[i].id === id) return list[i];
    return null;
  }

  function diffOptions(gens, current) {
    var max = (gens && typeof gens.max_difficulty === "number") ? gens.max_difficulty : BOUNDS.diffMax;
    var out = [];
    for (var d = BOUNDS.diffMin; d <= max; d++) {
      out.push(el("option", { value: String(d), text: String(d), selected: d === current }));
    }
    if (current > max) out.unshift(el("option", { value: String(current), text: String(current), selected: true }));
    return out;
  }

  // ---------- 站点列表 ----------

  function siteRow(s) {
    var view = el("button", {
      type: "button", class: "ghost", text: "查看",
      "aria-label": "查看站点 " + s.name,
      onclick: function () { navigate("site", s.id, false); }
    });
    return el("tr", null, [
      el("td", { text: s.name }),
      el("td", { class: "keyid wrap-anywhere", text: s.site_key }),
      el("td", { text: s.verify_mode || "" }),
      el("td", { class: "wrap-anywhere", text: s.generator_id || "—" }),
      el("td", null, [pill(!!s.enabled)]),
      el("td", { class: "nowrap", text: String(s.version) }),
      el("td", null, [view])
    ]);
  }

  function pill(enabled) {
    return el("span", { class: "pill " + (enabled ? "on" : "off"), text: enabled ? "启用" : "停用" });
  }

  async function renderSites(body, unused, ticket) {
    var after = state.siteCursor.length ? state.siteCursor[state.siteCursor.length - 1] : 0;
    var create = await buildCreateSite(ticket);
    if (!current(ticket)) return;
    body.appendChild(create);

    var box = div({ class: "panel" });
    body.appendChild(box);
    box.textContent = "加载中…";

    var res = await call("GET", API_PREFIX + "sites?limit=" + PAGE + "&after_id=" + after);
    if (handleAuth(res)) return;
    clear(box);
    if (res.status === 200 && res.data && Array.isArray(res.data.sites)) {
      var sites = (res.data && res.data.sites) || [];
      var next = (res.data && typeof res.data.next_after_id === "number") ? res.data.next_after_id : 0;
      box.appendChild(h2("站点"));
      if (!sites.length) {
        box.appendChild(p(after ? "这一页没有站点。" : "库里还没有站点。用上面的表单创建一个。", "hint"));
      } else {
        box.appendChild(el("div", { class: "table-wrap" }, [
          el("table", null, [
            el("thead", null, [el("tr", null, [
              el("th", { text: "名称" }), el("th", { text: "site_key" }),
              el("th", { text: "验证模式" }), el("th", { text: "题型" }),
              el("th", { text: "状态" }), el("th", { text: "版本" }), el("th", { text: "" })
            ])]),
            el("tbody", null, sites.map(siteRow))
          ])
        ]));
      }
      box.appendChild(pager("第 " + (state.siteCursor.length + 1) + " 页",
        state.siteCursor.length > 0, sites.length === PAGE && next > 0,
        function nextFn() { if (next > 0) navigate("sites", null, false, state.siteCursor.concat([next])); },
        function prevFn() { navigate("sites", null, false, state.siteCursor.slice(0, -1)); }));
    } else {
      if (handleAuth(res)) return;
      box.appendChild(p(failReason(res), "status bad"));
      box.appendChild(el("button", { type: "button", class: "ghost", text: "重试", onclick: function () { navigate("sites", null, false); } }));
    }
  }

  function pager(text, canPrev, canNext, onNext, onPrev) {
    return div({ class: "panel-actions" }, [
      canPrev ? el("button", { type: "button", class: "ghost", text: "上一页", onclick: onPrev }) : null,
      el("span", { class: "hint nowrap", text: text }),
      canNext ? el("button", { type: "button", class: "ghost", text: "下一页", onclick: onNext }) : null
    ]);
  }

  // ---------- 创建站点 ----------

  async function buildCreateSite(ticket) {
    var gens = await loadGenerators();
    if (!current(ticket)) return null;
    var box = div({ class: "panel" });
    var idName = nextID("new-name"), idKey = nextID("new-sitekey"),
      idPow = nextID("new-pow"), idMode = nextID("new-mode"),
      idGen = nextID("new-gen"), idDiff = nextID("new-diff"),
      idPool = nextID("new-pool");

    var name = el("input", {
      type: "text", id: idName, name: "name", maxlength: BOUNDS.nameMax,
      required: true, autocomplete: "off", "aria-describedby": idName + "-note"
    });
    var siteKey = el("input", {
      type: "text", id: idKey, name: "site_key", maxlength: BOUNDS.siteKeyMax,
      autocomplete: "off", spellcheck: "false", placeholder: "留空由服务生成 pk_…"
    });
    var pow = numberInput(idPow, 18, BOUNDS.powMin, BOUNDS.powMax);
    var mode = el("select", { id: idMode, name: "verify_mode" }, [
      el("option", { value: "pow_only", text: "pow_only（只要求 PoW，不下发题目）", selected: true }),
      el("option", { value: "pow_question", text: "pow_question（PoW + 题目）" })
    ]);
    var first = gens && gens.generators && gens.generators.length ? gens.generators[0].id : "";
    var gen = el("select", { id: idGen, name: "generator_id" },
      genOptions(gens, first));
    var diff = el("select", { id: idDiff, name: "difficulty" }, diffOptions(gens, 1));
    var pool = poolPicker(gens, []);
    var genNote = div({ class: "note", id: idGen + "-note", text: noteOf(gens, gen.value) });
    var genLife = div({ class: "pills", "aria-label": "题型自我声明" });
    paintLifecycle(genLife, gens, gen.value);

    gen.addEventListener("change", function () {
      genNote.textContent = noteOf(gens, gen.value);
      paintLifecycle(genLife, gens, gen.value);
    });

    var form = el("form", { class: "create-site" }, [
      h2("新建站点"),
      div({ class: "row" }, [
        field(idName, "站点名称（1–" + BOUNDS.nameMax + " 个可见字符）", name, "审计与页面都会打印它：不含控制字符，站内唯一。"),
        field(idKey, "site_key（公开标识）", siteKey, "公开值，可写进前端；不含任何秘密。留空则由服务生成。")
      ]),
      div({ class: "row" }, [
        field(idMode, "验证模式", mode),
        field(idPow, "PoW 难度 bits（" + BOUNDS.powMin + "–" + BOUNDS.powMax + "）", pow)
      ]),
      div({ class: "row" }, [
        field(idGen, "题型 generator_id", gen),
        field(idDiff, "难度档（1–" + BOUNDS.diffMax + "）", diff)
      ]),
      genLife,
      el("fieldset", { class: "legend-box", id: idPool }, [
        el("legend", { text: "生成器池 generator_pool（可多选；留空则只用 generator_id）" }),
        pool.root,
        p("仅池内题型会参与风险/设备加权调度；无障碍题型必须显式加入池。", "hint")
      ]),
      genNote,
      p("pow_only 不下发题目，题型与难度档不生效；选 pow_question 时必须有一个本进程登记的题型。", "hint"),
      div({ class: "panel-actions" }, [
        el("button", { type: "submit", id: "new-submit", text: "创建站点" })
      ]),
      div({ id: "new-status", class: "status", role: "status", "aria-live": "polite" })
    ]);
    box.appendChild(form);
    bindDirty(form, "create");

    form.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      var btn = document.getElementById("new-submit");
      var line = document.getElementById("new-status");
      if (btn.disabled) return;
      var problems = checkCreateFields({ name: name, siteKey: siteKey, pow: pow });
      if (problems.length) {
        line.className = "status bad";
        line.textContent = problems.join(" ");
        name.focus();
        return;
      }
      var finish = beginWrite(form, ticket);
      line.className = "status wait";
      line.textContent = "正在创建…";

      var payload = {
        name: name.value.trim(),
        pow_bits: Number(pow.value),
        verify_mode: mode.value,
        difficulty: Number(diff.value)
      };
      if (siteKey.value.trim()) payload.site_key = siteKey.value.trim();
      if (mode.value === "pow_question") payload.generator_id = gen.value;
      if (mode.value === "pow_question" && pool.get().length) payload.generator_pool = pool.get();
      var res;
      try { res = await call("POST", API_PREFIX + "sites", payload); }
      finally { finish(); }
      if (handleAuth(res)) return;
      if (res.status === 201 && res.data && res.data.site && res.data.secret) {
        setDirty("create", false);
        var s = res.data.site || {};
        clear(form);
        form.appendChild(h2("站点已创建"));
        form.appendChild(kv([
          ["站点 ID", String(s.id)],
          ["名称", s.name],
          ["site_key", s.site_key],
          ["验证模式", s.verify_mode],
          ["题型", s.generator_id || "未设置"],
          ["生成器池", (s.generator_pool || []).join(", ") || "（回落到 generator_id）"],
          ["难度档", String(s.difficulty)],
          ["状态", s.enabled ? "启用" : "停用"]
        ]));
        // 创建响应是**唯一**一次带着明文 secret。库里只有带 pepper 的摘要，之后任何接口都取不回来。
        if (res.data.secret) form.appendChild(oncePanel(String(res.data.secret)));
        form.appendChild(p("接入当前图片 widget 前，请在详情配置 pow_question、text-image-v1 与精确 Origin，并确认题池、CSP/worker、表单字段及服务端 siteverify。题型仍为实验性；pow_only 保留供 API 场景使用。", "hint"));
        form.appendChild(div({ class: "panel-actions" }, [
          el("button", { type: "button", text: "进入站点详情", onclick: function () { navigate("site", s.id, false); } }),
          el("button", { type: "button", class: "ghost", text: "返回列表", onclick: function () { render(); } })
        ]));
        // clear(form) 已经把提交前那个 #new-status 摘出文档了：往游离节点上写文案，
        // 屏幕上不会有任何变化。成功提示必须落在**当前 DOM 里**的节点上，
        // 而且先入树、再改文本，aria-live 才会播报这次变化（辅助技术看不到 detached 节点）。
        var done = div({ id: "new-status", class: "status", role: "status", "aria-live": "polite" });
        form.appendChild(done);
        done.className = "status ok";
        done.textContent = "已创建并写入审计。";
        document.getElementById("view-title").focus();
      } else {
        if (handleAuth(res)) return;
        line.className = "status bad";
        line.textContent = failReason(res);
        line.appendChild(el("button", { type: "button", class: "ghost", text: "只读复核站点列表", onclick: async function () {
          var check = await call("GET", API_PREFIX + "sites?limit=500");
          if (handleAuth(check)) return;
          if (check.status !== 200 || !check.data || !Array.isArray(check.data.sites)) {
            toast("无法确认创建结果：" + failReason(check));
            return;
          }
          var matches = check.data.sites.filter(function (site) { return site.name === payload.name || payload.site_key && site.site_key === payload.site_key; });
          toast(matches.length ? "读到相同名称或 site_key 的站点。请进入详情核对；一次性 secret 无法重新读取。" : "当前列表未找到匹配站点，仍不能据此自动重放创建。请确认原请求结果后再操作。");
        } }));
        btn.disabled = false;
      }
    });
    return box;
  }

  function noteOf(gens, id) {
    var g = findGen(gens, id);
    if (!g) return "本进程没有登记这个题型：领题会直接失败。";
    var lc = genLifecycle(g);
    var head = "该题型的自我声明：" + (lc.experimental ? "实验性" : "非实验性") +
      " / " + (lc.production_ready ? "生产可用" : "未达生产可用") + "。";
    return head + (g.note || "");
  }

  // 体验层校验：与服务端同一套边界，但**不**代替服务端判定（越界照样发出去，由 400 兜底）。
  function checkCreateFields(f) {
    var out = [];
    var n = f.name.value.trim();
    if (!n) out.push("站点名称不能为空。");
    else if (n.length > BOUNDS.nameMax) out.push("站点名称超过 " + BOUNDS.nameMax + " 个字符。");
    if (f.siteKey.value.trim().length > BOUNDS.siteKeyMax) out.push("site_key 超过 " + BOUNDS.siteKeyMax + " 个字符。");
    var pb = Number(f.pow.value);
    if (!(pb >= BOUNDS.powMin && pb <= BOUNDS.powMax)) {
      out.push("PoW bits 须在 " + BOUNDS.powMin + "–" + BOUNDS.powMax + "。");
    }
    return out;
  }

  // ---------- 一次性凭据展示 ----------

  function oncePanel(secret) {
    var area = el("textarea", {
      readonly: true, id: "once-secret", rows: 3, "aria-label": "本次生成的 secret（只显示一次）",
      value: secret
    });
    var btn = el("button", {
      type: "button", class: "ghost", text: "复制", onclick: async function () {
        area.select();
        var ok = false;
        try {
          if (navigator.clipboard && navigator.clipboard.writeText) {
            await navigator.clipboard.writeText(secret);
            ok = true;
          }
        } catch (e) { ok = false; }
        if (ok) toast("已复制到剪贴板");
        else if (document.execCommand && document.execCommand("copy")) toast("已复制到剪贴板");
        else { btn.disabled = true; btn.textContent = "手动复制"; toast("剪贴板不可用：已选中，按 Ctrl/Cmd+C 复制"); }
      }
    });
    return div({ class: "once" }, [
      el("strong", { text: "只显示一次的凭据" }),
      p("请立即保存到业务后端。此后任何接口都不会再返回它；忘记只能轮换一把新的。", "hint"),
      area,
      div({ class: "panel-actions gap-top" }, [btn])
    ]);
  }

  function kv(pairs) {
    var dl = el("dl", { class: "kv" });
    pairs.forEach(function (x) {
      if (x[1] === null || x[1] === undefined) return;
      dl.appendChild(el("dt", { text: x[0] }));
      dl.appendChild(el("dd", { text: String(x[1]) }));
    });
    return dl;
  }

  // ---------- 站点详情：概览 + 编辑 + Origin + 启停 ----------

  async function renderSite(body, id, ticket) {
    var box = div({ class: "panel" }, [p("加载中…", "status wait")]);
    body.appendChild(box);
    var results = await Promise.all([
      call("GET", API_PREFIX + "sites/" + encodeURIComponent(String(id))),
      loadGenerators()
    ]);
    var res = results[0], gens = results[1];
    if (handleAuth(res) || !current(ticket)) return;
    clear(box);
    if (res.status !== 200 || !res.data || !res.data.site) {
      if (handleAuth(res)) return;
      box.appendChild(p(failReason(res), "status bad"));
      box.appendChild(el("button", {
        type: "button", class: "ghost", text: "返回站点列表",
        onclick: function () { navigate("sites", null, false); }
      }));
      return;
    }
    var s = res.data.site;
    var origins = res.data.origins || [];
    var secrets = res.data.secrets || [];

    box.appendChild(h2("概览"));
    var overview = div();
    box.appendChild(overview);
    function updateOverview(next) {
      clear(overview);
      overview.appendChild(kv([
        ["站点 ID", String(next.id)],
        ["site_key（公开）", next.site_key],
        ["状态", next.enabled ? "启用" : "停用"],
        ["站点版本", String(next.version)],
        ["创建时间", next.created_at]
      ]));
    }
    updateOverview(s);

    box.appendChild(h2("状态操作"));
    var actions = div({ id: "site-actions" });
    box.appendChild(actions);
    paintSiteActions(actions, s, ticket, updateOverview);
    function policyConfirmed(next) {
      Object.assign(s, next);
      updateOverview(s);
    }

    box.appendChild(buildPolicyForm(s, gens, ticket, policyConfirmed));
    box.appendChild(buildOriginsForm(s, origins, ticket));

    box.appendChild(h2("验证密钥（" + secrets.length + "）"));
    box.appendChild(p("这里只有公开的 key_id 与时间。secret 明文只在创建/轮换那一次响应里出现，" +
      "任何后续接口都取不回来。", "hint"));
    if (!secrets.length) box.appendChild(p("当前没有可用凭据：/siteverify 会一律失败。", "status bad"));
    else box.appendChild(el("div", { class: "table-wrap" }, [
      el("table", null, [
        el("thead", null, [el("tr", null, [
          el("th", { text: "key_id" }), el("th", { text: "状态" }),
          el("th", { text: "创建" }), el("th", { text: "撤销" })
        ])]),
        el("tbody", null, secrets.map(function (k) {
          return el("tr", null, [
            el("td", { class: "keyid wrap-anywhere", text: k.key_id }),
            el("td", null, [el("span", { class: "pill " + (k.active ? "on" : "off"), text: k.active ? "有效" : "已撤销" })]),
            el("td", { class: "nowrap", text: k.created_at }),
            el("td", { class: "nowrap", text: k.revoked_at || "—" })
          ]);
        }))
      ])
    ]));

    box.appendChild(el("button", {
      type: "button", class: "ghost", text: "返回站点列表",
      onclick: function () { navigate("sites", null, false); }
    }));
  }

  function buildPolicyForm(s, gens, ticket, policyConfirmed) {
    var idName = nextID("pol-name"), idMode = nextID("pol-mode"), idGen = nextID("pol-gen"),
      idDiff = nextID("pol-diff"), idPow = nextID("pol-pow"), idChal = nextID("pol-chal"),
      idResp = nextID("pol-resp"), idIP = nextID("pol-ip"), idSite = nextID("pol-site"),
      idMiss = nextID("pol-miss");

    var name = el("input", {
      type: "text", id: idName, value: s.name, maxlength: BOUNDS.nameMax, autocomplete: "off",
      "aria-describedby": idName + "-note"
    });
    var mode = el("select", { id: idMode }, [
      el("option", { value: "pow_only", text: "pow_only", selected: s.verify_mode === "pow_only" }),
      el("option", { value: "pow_question", text: "pow_question", selected: s.verify_mode === "pow_question" })
    ]);
    var gen = el("select", { id: idGen }, genOptions(gens, s.generator_id || ""));
    var pool = poolPicker(gens, s.generator_pool || []);
    var diff = el("select", { id: idDiff }, diffOptions(gens, s.difficulty));
    var pow = numberInput(idPow, s.pow_bits, BOUNDS.powMin, BOUNDS.powMax);
    var chal = numberInput(idChal, s.challenge_ttl_sec, BOUNDS.ttlMin, BOUNDS.ttlMax);
    var resp = numberInput(idResp, s.response_ttl_sec, BOUNDS.ttlMin, BOUNDS.ttlMax);
    var ip = numberInput(idIP, s.rate_ip_rpm, BOUNDS.rpmMin, BOUNDS.rpmMax);
    var siteRpm = numberInput(idSite, s.rate_site_rpm, BOUNDS.rpmMin, BOUNDS.rpmMax);
    var miss = el("input", { type: "checkbox", id: idMiss, checked: s.allow_missing_origin === true });
    var genNote = div({ class: "note", text: noteOf(gens, gen.value) });
    var genLife = div({ class: "pills", "aria-label": "题型自我声明" });
    paintLifecycle(genLife, gens, gen.value);

    var fields = [
      { key: "name", node: name, get: function () { return name.value.trim(); }, base: s.name },
      { key: "verify_mode", node: mode, get: function () { return mode.value; }, base: s.verify_mode },
      { key: "generator_id", node: gen, get: function () { return gen.value; }, base: s.generator_id || "" },
      { key: "generator_pool", node: pool.root, get: function () { return JSON.stringify(pool.get()); }, base: pool.base },
      { key: "difficulty", node: diff, get: function () { return String(Number(diff.value)); }, base: String(s.difficulty) },
      { key: "pow_bits", node: pow, get: function () { return String(Number(pow.value)); }, base: String(s.pow_bits) },
      { key: "challenge_ttl_sec", node: chal, get: function () { return String(Number(chal.value)); }, base: String(s.challenge_ttl_sec) },
      { key: "response_ttl_sec", node: resp, get: function () { return String(Number(resp.value)); }, base: String(s.response_ttl_sec) },
      { key: "rate_ip_rpm", node: ip, get: function () { return String(Number(ip.value)); }, base: String(s.rate_ip_rpm) },
      { key: "rate_site_rpm", node: siteRpm, get: function () { return String(Number(siteRpm.value)); }, base: String(s.rate_site_rpm) },
      { key: "allow_missing_origin", node: miss, get: function () { return miss.checked ? "1" : "0"; }, base: s.allow_missing_origin ? "1" : "0" }
    ];

    var status = div({ id: idMode + "-status", class: "status", role: "status", "aria-live": "polite" });
    var submit = el("button", { type: "submit", text: "保存修改" });
    var reload = el("button", { type: "button", class: "ghost", text: "放弃修改并重新读取" });

    var binding = bindFields(fields, "policy");

    var form = el("form", null, [
      h2("策略与名称"),
      field(idName, "站点名称", name, "改名字不提升站点版本，也不使已签发的题目或 token 失效。"),
      el("fieldset", { class: "legend-box" }, [
        el("legend", { text: "验证模式与题型" }),
        div({ class: "row" }, [field(idMode, "verify_mode", mode), field(idGen, "generator_id", gen)]),
        genLife,
        el("fieldset", { class: "legend-box", id: idPool }, [
          el("legend", { text: "生成器池 generator_pool（可多选；留空则只用 generator_id）" }),
          pool.root,
          p("旧版单字段表单不会清空此列表；只有勾选变化才会发送 generator_pool。", "hint")
        ]),
        genNote,
        field(idDiff, "难度档（1–" + BOUNDS.diffMax + "）", diff)
      ]),
      el("fieldset", { class: "legend-box" }, [
        el("legend", { text: "计算成本与有效期（秒）" }),
        div({ class: "row" }, [
          field(idPow, "PoW bits（" + BOUNDS.powMin + "–" + BOUNDS.powMax + "）", pow),
          field(idChal, "题目有效期 challenge_ttl_sec（" + BOUNDS.ttlMin + "–" + BOUNDS.ttlMax + "）", chal),
          field(idResp, "token 有效期 response_ttl_sec（" + BOUNDS.ttlMin + "–" + BOUNDS.ttlMax + "）", resp)
        ]),
        p("难度与 TTL 走签发快照：改动只影响此后新领的请求，已签发的那些仍按签发当时的参数复核。", "hint")
      ]),
      el("fieldset", { class: "legend-box" }, [
        el("legend", { text: "站点级限流（次/分钟）" }),
        div({ class: "row" }, [
          field(idIP, "按来源地址 rate_ip_rpm（" + BOUNDS.rpmMin + "–" + BOUNDS.rpmMax + "）", ip),
          field(idSite, "按站点 rate_site_rpm（" + BOUNDS.rpmMin + "–" + BOUNDS.rpmMax + "）", siteRpm)
        ])
      ]),
      el("fieldset", { class: "legend-box" }, [
        el("legend", { text: "Origin 缺失的处理" }),
        div({ class: "check" }, [
          miss,
          label("允许缺失 Origin（allow_missing_origin）", idMiss)
        ]),
        p("勾上后，没有 Origin 也没有 Sec-Fetch-Site 的请求（直接 API 调用、部分原生表单提交）会被放行；" +
          "有 Origin 但不在此站点允许列表里的请求仍然被拒。", "hint")
      ]),
      div({ class: "panel-actions" }, [submit, reload]),
      status
    ]);

    function repaintGenNote() {
      if (mode.value === "pow_only") {
        genNote.textContent = "pow_only 不下发题目：题型与难度档存着但不生效。";
      } else {
        genNote.textContent = noteOf(gens, gen.value);
      }
      paintLifecycle(genLife, gens, gen.value);
    }
    mode.addEventListener("change", repaintGenNote);
    gen.addEventListener("change", repaintGenNote);

    reload.addEventListener("click", async function () {
      if (reload.disabled) return;
      var unlock = lockControls(form);
      try {
        var res = await call("GET", API_PREFIX + "sites/" + encodeURIComponent(String(s.id)));
        if (handleAuth(res)) return;
        if (res.status === 200 && res.data && res.data.site) {
          var next = buildPolicyForm(res.data.site, gens, ticket, policyConfirmed);
          policyConfirmed(res.data.site);
          form.parentNode.replaceChild(next, form);
          setDirty("policy", false);
        } else {
          status.className = "status bad";
          status.textContent = "重新读取失败，编辑内容已保留：" + failReason(res);
        }
      } finally { unlock(); }
    });

    form.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      if (submit.disabled) return;            // 防重复提交：请求在途时按钮已禁用
      var changed = binding.changed();
      if (!changed.length) {
        status.className = "status wait";
        status.textContent = "没有需要保存的修改。";
        return;
      }
      var problems = checkPolicyFields(fields);
      if (problems.length) {
        status.className = "status bad";
        status.textContent = problems.join(" ");
        return;
      }
      var payload = {};
      changed.forEach(function (f) {
        if (f.key === "name") payload.name = f.get();
        else if (f.key === "verify_mode") payload.verify_mode = f.get();
        else if (f.key === "generator_id") payload.generator_id = f.get();
        else if (f.key === "generator_pool") payload.generator_pool = JSON.parse(f.get());
        else if (f.key === "difficulty") payload.difficulty = Number(f.get());
        else if (f.key === "pow_bits") payload.pow_bits = Number(f.get());
        else if (f.key === "challenge_ttl_sec") payload.challenge_ttl_sec = Number(f.get());
        else if (f.key === "response_ttl_sec") payload.response_ttl_sec = Number(f.get());
        else if (f.key === "rate_ip_rpm") payload.rate_ip_rpm = Number(f.get());
        else if (f.key === "rate_site_rpm") payload.rate_site_rpm = Number(f.get());
        else if (f.key === "allow_missing_origin") payload.allow_missing_origin = f.get() === "1";
      });

      var finish = beginWrite(form, ticket);
      status.className = "status wait";
      status.textContent = "正在保存" + changed.map(function (f) { return f.key; }).join("、") + "…";
      try {
        var res = await call("PUT", API_PREFIX + "sites/" + encodeURIComponent(String(s.id)), payload);
        if (handleAuth(res)) return;
        if (res.status === 200 && res.data && res.data.site) {
          var check = await confirmSaved(s.id, payload, res.data.site);
          if (!current(ticket) || check.stale) return;
          status.className = check.mismatch.length || check.failReason ? "status bad" : "status ok";
          if (check.mismatch.length) {
            status.textContent = "保存请求已返回，但复读结果不一致：" + check.mismatch.join("；") + "。编辑内容已保留，请核对后再操作。";
          } else if (check.failReason) {
            status.textContent = check.failReason;
          } else {
            binding.reset(changed);
            policyConfirmed(check.site);
            setDirty("policy", false);
            status.textContent = "已保存，并已重新读取站点确认落库值。";
          }
        } else {
          status.className = "status bad";
          status.textContent = "保存结果：" + failReason(res);
        }
        if (status.className === "status bad") {
          status.appendChild(el("button", { type: "button", class: "ghost", text: "只读复核策略", onclick: async function () {
            var check = await confirmSaved(s.id, payload, null);
            if (!current(ticket) || check.stale) return;
            status.className = check.failReason || check.mismatch.length ? "status bad" : "status ok";
            status.textContent = check.failReason || (check.mismatch.length ? "只读复核不一致：" + check.mismatch.join("；") : "只读复核：当前存储值与提交值一致。编辑内容已保留，未再次提交。") ;
          } }));
        }
      } finally { finish(); }
    });

    return form;
  }

  // confirmSaved 把"PUT 返回 200"变成"库里确实是这样"：逐字段与 GET 结果比对。
  async function confirmSaved(id, payload, echoed) {
    var out = { mismatch: [], failReason: "" };
    var res = await call("GET", API_PREFIX + "sites/" + encodeURIComponent(String(id)));
    if (handleAuth(res)) { out.stale = true; return out; }
    if (res.status !== 200 || !res.data || !res.data.site) {
      out.failReason = "重新读取失败：无法确认落库结果。" + failReason(res);
      return out;
    }
    var got = res.data.site;
    out.site = got;
    Object.keys(payload).forEach(function (k) {
      var want = payload[k], have = got[k];
      if (typeof want === "boolean") { if (!!have !== want) out.mismatch.push(k + " 期望 " + want + "，读到 " + have); return; }
      if (String(want) !== String(have)) out.mismatch.push(k + " 期望 " + want + "，读到 " + have);
    });
    if (echoed && Object.keys(payload).some(function (key) { return JSON.stringify(echoed[key]) !== JSON.stringify(got[key]); })) {
      out.mismatch.push("提交字段的 PUT 回显与 GET 结果不一致（可能有并发写入）");
    }
    return out;
  }

  function checkPolicyFields(fields) {
    var out = [];
    var map = {};
    fields.forEach(function (f) { map[f.key] = f; });
    var n = map.name.get();
    if (!n) out.push("站点名称不能为空。");
    else if (n.length > BOUNDS.nameMax) out.push("站点名称超过 " + BOUNDS.nameMax + " 个字符。");
    [["pow_bits", BOUNDS.powMin, BOUNDS.powMax],
      ["challenge_ttl_sec", BOUNDS.ttlMin, BOUNDS.ttlMax],
      ["response_ttl_sec", BOUNDS.ttlMin, BOUNDS.ttlMax],
      ["rate_ip_rpm", BOUNDS.rpmMin, BOUNDS.rpmMax],
      ["rate_site_rpm", BOUNDS.rpmMin, BOUNDS.rpmMax]].forEach(function (b) {
        var v = Number(map[b[0]].get());
        if (!isFinite(v) || Math.floor(v) !== v || v < b[1] || v > b[2]) {
          out.push(b[0] + " 须在 " + b[1] + "–" + b[2] + " 的整数。");
        }
      });
    if (map.verify_mode.get() === "pow_question" && !map.generator_id.get()) {
      out.push("pow_question 必须选一个已登记的题型。");
    }
    if (map.generator_pool && JSON.parse(map.generator_pool.get()).length > BOUNDS.diffMax + 3) {
      out.push("生成器池最多 8 项。");
    }
    return out;
  }

  function buildOriginsForm(s, origins, ticket) {
    var idArea = nextID("origins"), idStatus = nextID("origins-status");
    var base = origins.slice().sort();
    var draft = (originsDraft && originsDraft.site === s.id) ? originsDraft.text : null;
    var area = el("textarea", {
      id: idArea, value: draft !== null ? draft : base.join("\n"), spellcheck: "false", autocomplete: "off",
      "aria-describedby": idArea + "-note", rows: 5
    });
    var submit = el("button", { type: "submit", text: "保存允许来源" });
    var status = div({ id: idStatus, class: "status", role: "status", "aria-live": "polite" });
    var count = p("当前 " + base.length + " 条。", "hint");

    function lines() {
      return area.value.split("\n").map(function (x) { return x.trim(); }).filter(function (x) { return x !== ""; });
    }
    function dirty() { return lines().slice().sort().join("\n") !== base.join("\n"); }
    function note() { setDirty("origins", dirty()); }
    area.addEventListener("input", function () { originsDraft = { site: s.id, text: area.value }; note(); });
    area.addEventListener("change", note);
    if (draft !== null) setDirty("origins", dirty());

    var form = el("form", null, [
      h2("允许来源（精确 Origin 列表）"),
      p("一行一条，规范化形式是 scheme://host[:端口]；不做后缀匹配、不支持通配符、不自动放行子域。" +
        "空列表表示不接受任何浏览器来源（此时只有 allow_missing_origin 放行的直接调用能签发）。", "hint"),
      field(idArea, "Origin 列表", area, "上限 " + BOUNDS.originCountMax + " 条，单条不超过 " + BOUNDS.originMax + " 字符。"),
      count,
      div({ class: "panel-actions" }, [submit]),
      status
    ]);

    form.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      if (submit.disabled) return;
      var list = lines();
      if (list.length > BOUNDS.originCountMax) {
        status.className = "status bad";
        status.textContent = "超过 " + BOUNDS.originCountMax + " 条上限。";
        return;
      }
      var finish = beginWrite(form, ticket);
      status.className = "status wait";
      status.textContent = "正在替换允许来源…";
      var path = API_PREFIX + "sites/" + encodeURIComponent(String(s.id)) + "/origins";
      try {
        var res = await call("PUT", path, { origins: list });
        if (handleAuth(res)) return;
        if (res.status === 200 && res.data && Array.isArray(res.data.origins)) {
          var check = await call("GET", path);
          if (handleAuth(check)) return;
          if (check.status === 200 && check.data && Array.isArray(check.data.origins)) {
            var gotList = check.data.origins.slice().sort();
            if (JSON.stringify(gotList) === JSON.stringify(res.data.origins.slice().sort())) {
              originsDraft = null;
              base = gotList;
              area.value = gotList.join("\n");
              setDirty("origins", false);
              count.textContent = "当前 " + gotList.length + " 条。";
              status.className = "status ok";
              status.textContent = "服务端规范化后共 " + gotList.length + " 条，已重新读取并确认。";
              return;
            }
            status.textContent = "复读结果不一致，编辑内容已保留，请核对后再操作。";
          } else {
            status.textContent = "保存请求返回成功，但重新读取失败，无法确认当前允许来源。" + failReason(check);
          }
        } else {
          status.textContent = "保存结果：" + failReason(res);
        }
        status.className = "status bad";
        status.appendChild(el("button", { type: "button", class: "ghost", text: "只读复核允许来源", onclick: async function () {
          var check = await call("GET", path);
          if (handleAuth(check)) return;
          status.textContent = check.status === 200 && check.data && Array.isArray(check.data.origins)
            ? "只读复核：当前允许来源为 " + (check.data.origins.join("、") || "空列表") + "。编辑内容已保留，未再次提交。"
            : "无法确认当前允许来源：" + failReason(check);
        } }));
      } finally { finish(); }
    });

    return form;
  }

  function paintSiteActions(host, s, ticket, updateOverview) {
    clear(host);
    var enabling = !s.enabled;
    var toggle = el("button", {
      type: "button",
      class: enabling ? "ghost" : "danger",
      text: enabling ? "启用此站点" : "停用此站点",
      onclick: function () {
        if (!current(ticket) || toggle.disabled) return;
        toggle.disabled = true;
        var confirm = confirmToggle(host, s, enabling, ticket, updateOverview);
        host.appendChild(confirm);
        confirm.querySelector("button").focus();
      }
    });
    host.appendChild(toggle);
  }

  function confirmToggle(host, s, enable, ticket, updateOverview) {
    var note = enable
      ? "启用后新领题会按站点当前配置签发。站点版本会提升：停用期间没有任何东西被复活。"
      : "停用会让领题与兑换立即失败。站点版本会提升，因此停用前签发但尚未消费的 token 在重新启用后也不会复活（DESIGN §5.3）。";
    var target = "站点「" + s.name + "」  id=" + s.id + "  site_key=" + s.site_key + "  当前版本 " + s.version;
    var status = div({ class: "status", role: "status", "aria-live": "polite" });
    var box = div({ class: "banner warn" }, [
      el("strong", { text: (enable ? "确认启用：" : "确认停用：") }),
      p(target, "wrap-anywhere"),
      p(note),
      div({ class: "panel-actions" }, [
        el("button", {
          type: "button", text: enable ? "确认启用" : "确认停用", onclick: async function (ev) {
            if (!current(ticket) || ev.target.disabled) return;
            var finish = beginWrite(box, ticket);
            status.className = "status wait";
            status.textContent = "正在确认站点状态…";
            try {
              var res = await call("POST", API_PREFIX + "sites/" + s.id + "/" + (enable ? "enable" : "disable"), {});
              if (handleAuth(res)) return;
              var check = await call("GET", API_PREFIX + "sites/" + s.id);
              if (handleAuth(check)) return;
              if (check.status === 200 && check.data && check.data.site) {
                var next = check.data.site;
                Object.keys(next).forEach(function (key) { s[key] = next[key]; });
                updateOverview(s);
                paintSiteActions(host, s, ticket, updateOverview);
                var line = div({ class: res.status === 200 && next.enabled === enable ? "status ok" : "status bad", role: "status", "aria-live": "polite" });
                host.appendChild(line);
                line.textContent = res.status === 200 && next.enabled === enable
                  ? "站点已" + (enable ? "启用" : "停用") + "，并已只读复核。未保存的表单修改仍保留。"
                  : (res.status === 200 ? "启停请求返回成功，但复读状态不一致。" : failReason(res)) + " 当前读到站点为" + (next.enabled ? "启用" : "停用") + "，请核对。";
              } else {
                status.className = "status bad";
                status.textContent = "无法确认启停结果。" + failReason(check);
                status.appendChild(el("button", { type: "button", class: "ghost", text: "只读复核站点状态", onclick: async function () {
                  var check = await call("GET", API_PREFIX + "sites/" + s.id);
                  if (handleAuth(check)) return;
                  if (check.status === 200 && check.data && check.data.site) {
                    Object.keys(check.data.site).forEach(function (key) { s[key] = check.data.site[key]; });
                    updateOverview(s);
                    paintSiteActions(host, s, ticket, updateOverview);
                  } else { toast("无法确认当前状态：" + failReason(check)); }
                } }));
              }
            } finally { finish(); }
          }
        }),
        el("button", { type: "button", class: "ghost", text: "取消", onclick: function () {
          paintSiteActions(host, s, ticket, updateOverview);
          host.querySelector("button").focus();
        } })
      ]),
      status
    ]);
    return box;
  }

  VIEWS.sites = { title: "站点", render: renderSites };
  VIEWS.site = { title: "站点详情", render: renderSite };

  // ---------- 装配 ----------

  function boot() {
    Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (b) {
      b.addEventListener("click", function () { navigate(b.dataset.view, null, b.dataset.view === "sites"); });
    });

    document.getElementById("logout").addEventListener("click", function () {
      dropSession();
      state.generators = null;      // 题型元数据连同会话一起丢弃，退出后再进来必须重新鉴权才能读
      var input = document.getElementById("admin-key");
      if (input) input.value = "";
      var line = document.getElementById("login-status");
      line.className = "status";
      line.textContent = "";
      state.view = "login";
      state.dirty = false;
      state.pendingNav = null;
      state.siteCursor = [];
      render();
      toast("已退出：凭据已从页面状态移除（页面已刷新为未登录）");
      document.getElementById("login-title").focus();
    });

    // 关标签/刷新时提醒：这是**唯一**能拦住"未保存就没了"的浏览器机制。
    window.addEventListener("beforeunload", function (e) {
      if (!state.dirty) return;
      e.preventDefault();
      e.returnValue = "";
    });

    render();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
  else boot();
})();
