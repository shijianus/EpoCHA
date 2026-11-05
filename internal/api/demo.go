package api

import (
	"io"
	"net/http"
)

// handleDemo 是**测试部署**用的只读页面：领题、答题、显示组件状态。
//
// 它不调用 /siteverify，也不读取任何 secret；页面里唯一的凭据是公开的 site_key。
// 生产部署不要设置 EPOCHA_DEMO_PAGE=1；这不是业务接入示例，接入请按 docs/WIDGET.md。
func (s *Server) handleDemo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, demoPageHTML)
}

const demoPageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>EpoCHA v2 测试页</title>
<style>
  :root { color-scheme: light dark; }
  body { max-width: 760px; margin: 2rem auto; padding: 0 1rem; font: 16px/1.6 system-ui, sans-serif; }
  .card { border: 1px solid #8884; border-radius: 12px; padding: 1rem 1.25rem; margin: 1rem 0; }
  code, pre { font-family: ui-monospace, monospace; }
  #token { word-break: break-all; white-space: pre-wrap; }
  .note { color: #777; font-size: 0.9rem; }
</style>
</head>
<body>
<h1>EpoCHA v2 测试页</h1>
<p class="note">只验证组件：领题 → 看图输入 → PoW → redeem。不调用 /siteverify，不写业务数据。</p>
<div class="card">
  <div id="captcha"></div>
  <p>组件状态：<code id="state">idle</code></p>
  <p id="msg"></p>
  <p>response token：</p>
  <pre id="token">（验证成功后显示）</pre>
</div>
<p class="note">
  这个页面由 <code>EPOCHA_DEMO_PAGE=1</code> 打开，默认关闭。
  业务接入请只在服务端调用 <code>/siteverify</code>；浏览器里的 onSuccess 不是授权。
</p>
<script src="/widget/v1.js" defer></script>
<script>
(function () {
  function qs(name) {
    try { return new URLSearchParams(location.search).get(name) || ""; } catch (e) { return ""; }
  }
  function start() {
    var siteKey = qs("site_key");
    var msg = document.getElementById("msg");
    var state = document.getElementById("state");
    var token = document.getElementById("token");
    if (!/^[A-Za-z0-9_-]{1,64}$/.test(siteKey)) {
      msg.textContent = "缺少合法的 site_key：请用 /demo?site_key=pk_... 打开。";
      return;
    }
    if (!window.EpoCHA || typeof window.EpoCHA.render !== "function") {
      msg.textContent = "widget 脚本未加载。";
      return;
    }
    window.EpoCHA.render(document.getElementById("captcha"), {
      apiBase: location.origin,
      siteKey: siteKey,
      language: "zh-CN",
      fieldName: "",
      onState: function (s) { state.textContent = s; },
      onSuccess: function (t) {
        state.textContent = "verified";
        token.textContent = t;
        msg.textContent = "验证成功。这个 token 只用于演示，不会在这里提交。";
      },
      onError: function (e) {
        msg.textContent = "错误：" + (e && e.code ? e.code : "unknown");
      }
    });
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();
</script>
</body>
</html>
`
