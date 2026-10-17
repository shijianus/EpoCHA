package api

// 阶段 4A：widget 静态资源路由（DESIGN §10.1 的 `GET /widget/v1.js`、D10 的 go:embed）。
//
// 这里守的是**接线**边界，文件内容的防漂移锁在 web/widget/widget_test.go：
//   1. 路由形态：内容类型、可缓存、nosniff、无 CORS 头、方法不匹配 405、未知版本 404；
//   2. **不拓宽公开面**：挂上静态资源之后，公共端口上依然没有管理路由、目录不可列、
//      路径穿越探测拿不到任何仓库文件。

import (
	"net/http"
	"strings"
	"testing"
)

func TestWidgetAssetRoutes(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{"/widget/v1.js", "/widget/v1.worker.js"} {
		r := h.do(http.MethodGet, path, nil, "", "203.0.113.7:5555", nil)
		if r.status != http.StatusOK {
			t.Fatalf("%s status=%d", path, r.status)
		}
		if ct := r.headers.Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
			t.Fatalf("%s Content-Type = %q", path, ct)
		}
		if !strings.HasPrefix(r.body, "/*!") {
			t.Fatalf("%s 应以版本声明注释开头：%q", path, firstLine(r.body))
		}
		if !strings.Contains(r.body, "epocha") {
			t.Fatalf("%s 内容不像 widget 脚本（长度 %d）", path, len(r.body))
		}
		// 全局中间件给所有响应打 no-store；静态资源必须显式覆盖，
		// 否则每次领题都回源一次脚本，1 vCPU 机器上 widget 自己就成了负载。
		if cc := r.headers.Get("Cache-Control"); cc == "" || strings.Contains(cc, "no-store") {
			t.Fatalf("%s Cache-Control = %q，应可缓存", path, cc)
		}
		if nos := r.headers.Get("X-Content-Type-Options"); nos != "nosniff" {
			t.Fatalf("%s 缺 X-Content-Type-Options：%q", path, nos)
		}
		// `<script src>` 不需要 CORS 头；这里也刻意不给 `*`——
		// 给通配头会让"任意页面可读"看起来像对所有资源默认成立，而事实并非如此。
		if acao := r.headers.Get("Access-Control-Allow-Origin"); acao != "" {
			t.Fatalf("%s 不应带 CORS 头：%q", path, acao)
		}
	}

	if r := h.do(http.MethodPost, "/widget/v1.js", strings.NewReader("{}"), "application/json", "203.0.113.7:5555", nil); r.status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /widget/v1.js = %d, want 405", r.status)
	}
	// 版本化 URL 是刻意的：改协议就发 v2.js，旧页面不会被半路换掉实现。
	if r := h.do(http.MethodGet, "/widget/v2.js", nil, "", "203.0.113.7:5555", nil); r.status != http.StatusNotFound {
		t.Fatalf("GET /widget/v2.js = %d, want 404", r.status)
	}
	if r := h.do(http.MethodGet, "/widget/", nil, "", "203.0.113.7:5555", nil); r.status != http.StatusNotFound {
		t.Fatalf("GET /widget/ = %d, want 404（不可列目录）", r.status)
	}
}

func TestWidgetAssetsDoNotWidenPublicSurface(t *testing.T) {
	h := newHarness(t, nil)
	// 挂 widget 时最容易顺手把管理面也挂到公共端口——这条断言就是防这件事的。
	if r := h.do(http.MethodGet, "/api/admin/pool", nil, "", "203.0.113.7:5555", nil); r.status != http.StatusNotFound {
		t.Fatalf("公共端口上 GET /api/admin/pool = %d, want 404", r.status)
	}
	// 路径穿越：Go 的 ServeMux 会先净化路径，穿越探测只能得到 404，拿不到仓库文件。
	for _, probe := range []string{"/widget/../internal/config/config.go", "/widget/..%2finternal%2fconfig%2fconfig.go", "/widget/%2e%2e/v1.js"} {
		r := h.do(http.MethodGet, probe, nil, "", "203.0.113.7:5555", nil)
		if strings.Contains(r.body, "package config") || strings.Contains(r.body, "MasterKey") {
			t.Fatalf("路径穿越 %q 泄露了仓库文件：%s", probe, r.body)
		}
		if r.status == http.StatusOK && strings.HasPrefix(r.body, "/*!") && !strings.HasSuffix(probe, "v1.js") {
			t.Fatalf("路径穿越 %q 返回了 widget 内容", probe)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
