package main

// 盲测页面：HTML + 两个同源静态文件（JS/CSS）。
// 与 exampleapp 同样的理由不给内联块：严格 CSP（无 unsafe-inline）下要真能跑。

import (
	"io"
	"net/http"
	"strconv"
)

const pageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>epocha 人工可读性盲测（开发期）</title>
<link rel="stylesheet" href="/app.css">
</head>
<body>
<h1>人工可读性盲测（开发期工具）</h1>
<p class="warnbox">目的：由<b>真人</b>在生产 widget 的完整链路上做题，量"图能不能读出来"。
它不测 OCR、不测自动化。答案不显示也不记录：对错只来自服务端的兑换结果。
本阶段只有视觉通道，无法看清图片的人在这里就完不成测试（非视觉替代尚未实现）。</p>

<fieldset id="tiers"><legend>选择难度档</legend></fieldset>

<div class="box">
  <p class="meta">档位：<code id="cur-tier">（未选择）</code>　进度：<code id="cur-item">0</code> / <code id="cur-batch">0</code></p>
  <div id="host"></div>
  <p id="state" role="status" aria-live="polite" data-kind="idle">未开始：先选一个难度档。</p>
  <p class="btns">
    <button id="next" type="button" disabled>下一题</button>
    <button id="skip" type="button" disabled>这题看不清，记为失败并换题</button>
  </p>
  <p class="meta" id="timing"></p>
</div>

<div class="box">
  <h2>本次会话</h2>
  <p id="summary">尚无记录。</p>
  <p><a id="exp-csv" href="/export.csv">导出 CSV</a>　<a id="exp-json" href="/export.json">导出 JSON</a></p>
  <p class="meta">导出不含验证凭据、不含 site_key、也不含你输入的字符。</p>
</div>

<script id="cfg" type="application/json">{{CFG}}</script>
{{WIDGET_TAG}}
<script src="/app.js" defer></script>
</body>
</html>
`

// pageJS 把一批题的三段耗时与结果记到本地服务：
//
//	答题 = ready → solving（进入 solving 的时刻近似"按下提交"）
//	PoW  = solving → submitting
//	兑换 = submitting → 终态（一次往返 + 服务端判分；这一段在这个工具里**无法**再拆成
//	     RTT 与服务端处理时间，所以不假装能拆，报告里也只按这三段口径给数）
//
// 只用 widget 的公开观测面（onState / onSuccess / onError），不读它的内部状态。
const pageJS = `"use strict";
(function () {
  var CFG = JSON.parse(document.getElementById('cfg').textContent);
  var ids = ['tiers','cur-tier','cur-item','cur-batch','host','state','timing','summary','next','skip','exp-csv','exp-json'];
  var el = {};
  ids.forEach(function (k) { el[k] = document.getElementById(k); });
  el['cur-batch'].textContent = String(CFG.batch);

  var cur = { tier: '', widgetId: null, item: 0, refreshes: 0, t: {}, done: false };
  var counts = {};
  var nowMs = (typeof performance !== 'undefined' && performance.now)
    ? function () { return performance.now(); } : function () { return Date.now(); };

  function say(msg, kind) {
    el.state.textContent = msg;
    el.state.setAttribute('data-kind', kind || 'idle');
  }
  function diff(a, b) {
    if (typeof cur.t[a] !== 'number' || typeof cur.t[b] !== 'number') return -1;
    var d = cur.t[b] - cur.t[a];
    return d > 0 ? Math.round(d) : -1;
  }

  function onState(s) {
    var t = nowMs();
    if (s === 'ready') {
      // 同一个 item 内再次 ready ⇒ 中间换过题（组件内"换一张"或后端重试都算）
      if (typeof cur.t.ready === 'number' && cur.t.phase === 'asking') cur.refreshes++;
      cur.t = { ready: t, phase: 'asking' };
      say('看图输入 6 个字符，然后点组件里的"验证"。', 'idle');
      el.skip.disabled = false;
      el.next.disabled = true;
    } else if (s === 'solving') {
      cur.t.solving = t; say('本地计算中…', 'busy');
    } else if (s === 'submitting') {
      cur.t.submitting = t; say('提交验证中…', 'busy');
    } else if (s === 'verified' || s === 'expired' || s === 'error' || s === 'destroyed') {
      cur.t.phase = 'terminal';
    }
  }

  function payload(outcome, errorCode) {
    return {
      tier: cur.tier, item: cur.item, outcome: outcome, error_code: errorCode || '',
      answer_ms: diff('ready', 'solving'), pow_ms: diff('solving', 'submitting'),
      redeem_ms: diff('submitting', 'end'), refreshes: cur.refreshes
    };
  }

  function render(r) {
    counts[r.outcome] = (counts[r.outcome] || 0) + 1;
    var total = Object.keys(counts).reduce(function (a, k) { return a + counts[k]; }, 0);
    el.summary.textContent = '已记录 ' + total + ' 条：' +
      Object.keys(counts).sort().map(function (k) { return k + ' ' + counts[k]; }).join('　');
    el.timing.textContent = '本题耗时：答题 ' + r.answer_ms + 'ms　PoW ' + r.pow_ms +
      'ms　兑换 ' + r.redeem_ms + 'ms　本题内换题 ' + r.refreshes + ' 次' +
      '（-1 表示这一段没观察到，例如还没进入计算就结束）';
  }

  // 记录写本地服务：用 fetch，不阻塞 UI；按钮在写入完成前保持禁用，
  // 这样"导出"看到的数与页面上看到的数必然一致。
  function record(outcome, errorCode) {
    if (!cur.tier || cur.item < 1) return Promise.resolve(false);
    cur.t.phase = 'terminal';
    var r = payload(outcome, errorCode);
    r.answer_ms = r.answer_ms < 0 ? 0 : r.answer_ms;
    say('记录中…', 'busy');
    return fetch('/record', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(r), credentials: 'omit', cache: 'no-store'
    }).then(function (res) {
      if (!res.ok) { say('记录失败：HTTP ' + res.status, 'err'); return false; }
      cur.refreshes = 0;
      render(r);
      return true;
    }, function (e) {
      say('记录失败：' + String(e && e.message ? e.message : e), 'err');
      return false;
    });
  }

  function finish(outcome, code) {
    cur.t.end = nowMs();
    return record(outcome, code).then(function (ok) {
      if (!ok) return;
      el.skip.disabled = true;
      el.next.disabled = false;
    });
  }

  function handlers() {
    return {
      onSuccess: function () { say('验证通过。', 'ok'); finish('verified', ''); },
      onExpired: function () { say('凭据已过期（服务端 TTL）。', 'err'); },
      onError: function (e) {
        var code = e && e.code ? String(e.code) : 'unknown';
        say('本题结束：' + (e && e.message ? e.message : code), 'err');
        var outcome = 'error';
        if (code === 'invalid-challenge') outcome = 'wrong';
        else if (code === 'pow-timeout') outcome = 'timeout';
        finish(outcome, code);
      },
      onState: onState
    };
  }

  function mount(tierLabel) {
    var t = null;
    CFG.tiers.forEach(function (x) { if (x.label === tierLabel) t = x; });
    if (!t) { say('档位不存在。', 'err'); return; }
    if (cur.widgetId) { try { EpoCHA.destroy(cur.widgetId); } catch (e) { /* 已销毁 */ } cur.widgetId = null; }
    while (el.host.firstChild) el.host.removeChild(el.host.firstChild);
    counts = {};
    cur.tier = tierLabel; cur.item = 0; cur.refreshes = 0; cur.t = {}; cur.done = false;
    el['cur-tier'].textContent = tierLabel;
    el.summary.textContent = '尚无记录。';
    el.timing.textContent = '';
    var host = document.createElement('div');
    el.host.appendChild(host);
    var opts = {
      apiBase: CFG.api_base, siteKey: t.site_key, language: CFG.language,
      fieldName: '', workerUrl: CFG.worker_url || '', showExperimentalNote: true
    };
    Object.assign(opts, handlers());
    cur.widgetId = EpoCHA.render(host, opts);
    advance();
  }

  // advance 走组件自己的"换一张"路径（refresh），这样下一题是真的新题而不是缓存
  function advance() {
    if (cur.item >= CFG.batch) {
      cur.done = true;
      say('本档 ' + CFG.batch + ' 题已做完，可以导出了。', 'ok');
      el.next.disabled = true; el.skip.disabled = true;
      return;
    }
    cur.item++;
    el['cur-item'].textContent = String(cur.item);
    cur.t = {};
    if (cur.widgetId) { try { EpoCHA.refresh(cur.widgetId); } catch (e) { say('换题失败：' + String(e), 'err'); } }
  }

  el.next.addEventListener('click', function () { if (!cur.done) advance(); });
  el.skip.addEventListener('click', function () {
    // "看不清"是一种可读性结论：单独一档 unreadable，不混进 error（那会把它读成组件故障）
    cur.t.end = nowMs();
    record('unreadable', 'self-report').then(function (ok) {
      if (!ok) return;
      el.skip.disabled = true;
      advance();
    });
  });

  CFG.tiers.forEach(function (t) {
    var b = document.createElement('button');
    b.type = 'button'; b.textContent = t.label;
    b.addEventListener('click', function () { mount(t.label); });
    el.tiers.appendChild(b);
  });
  // 导出链接带上会话号：多次启动的记录不该被误当同一批
  el['exp-csv'].href = '/export.csv?s=' + encodeURIComponent(CFG.session);
  el['exp-json'].href = '/export.json?s=' + encodeURIComponent(CFG.session);
})();
`

const pageCSS = `body{font:15px/1.6 system-ui,sans-serif;margin:0 auto;max-width:46rem;padding:1.5rem 1rem 3rem;color:#1c1c1c}
h1{font-size:1.3rem}
.box{border:1px solid #b9b9b9;border-radius:6px;padding:12px;margin:14px 0}
.warnbox{border-left:4px solid #b0761a;background:#fff8ec;padding:10px 12px;border-radius:4px}
button{font:inherit;padding:6px 12px;margin:6px 6px 0 0;border:1px solid #7a7a7a;border-radius:4px;background:#f2f2f2;cursor:pointer}
button:focus-visible{outline:2px solid #1a5fb4;outline-offset:2px}
button[disabled]{opacity:.5;cursor:default}
fieldset{border:1px solid #c9c9c9;border-radius:6px;padding:10px}
legend{padding:0 6px;font-weight:600}
code{background:#f2f2f2;padding:1px 4px;border-radius:3px}
.meta{font-size:13px;color:#444}
#state{margin:10px 0 0;padding:6px 8px;border:1px solid #c9c9c9;border-radius:4px;min-height:1.2em}
#state[data-kind=ok]{border-color:#1c7d33;background:rgba(28,125,51,.08)}
#state[data-kind=err]{border-color:#a31414;background:rgba(163,20,20,.07)}
#state[data-kind=busy]{border-color:#1a5fb4;background:rgba(26,95,180,.07)}
`

func (s *server) handleStaticJS(w http.ResponseWriter, r *http.Request) {
	s.writeStatic(w, r, pageJS, "text/javascript; charset=utf-8")
}

func (s *server) handleStaticCSS(w http.ResponseWriter, r *http.Request) {
	s.writeStatic(w, r, pageCSS, "text/css; charset=utf-8")
}

func (s *server) writeStatic(w http.ResponseWriter, r *http.Request, body, ct string) {
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = io.WriteString(w, body)
}
