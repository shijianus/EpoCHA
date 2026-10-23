package admin

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/web/adminui"
)

// TestAdminUIMountedOnAdminHandler 钉三件事：
//  1. 页面壳挂在管理 Server 上，并且**不需要** Bearer 就能取到（未登录时页面本来就该能加载）；
//  2. 取到的是没有凭据的静态壳：内嵌内容里既没有引导用的管理密钥，也没有刚创建站点的 secret；
//  3. 挂页面不等于放开 API——同一个 Server 上未鉴权的 /api/admin/* 仍然 401。
func TestAdminUIMountedOnAdminHandler(t *testing.T) {
	x := newHarness(t, nil)
	_, secret := x.createSite("alpha", "pk_alpha")

	cases := []struct {
		path string
		mime string
	}{
		{"/admin", "text/html; charset=utf-8"},
		{"/admin/", "text/html; charset=utf-8"},
		{"/admin/app.js", "text/javascript; charset=utf-8"},
		{"/admin/app.css", "text/css; charset=utf-8"},
	}
	for _, c := range cases {
		resp := x.call(http.MethodGet, c.path, nil, "") // 故意不带 Authorization
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("读 %s: %v", c.path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s 应 200（页面壳可未登录加载），got %d", c.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != c.mime {
			t.Errorf("%s Content-Type=%q want %q", c.path, ct, c.mime)
		}
		body := string(raw)
		if strings.Contains(body, sentinelAdminKey) {
			t.Errorf("%s 泄露了管理凭据", c.path)
		}
		if strings.Contains(body, secret) {
			t.Errorf("%s 泄露了站点 secret", c.path)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control=%q", c.path, cc)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s CSP 缺 frame-ancestors 'none'：%s", c.path, csp)
		}
		for _, bad := range []string{"unsafe-inline", "unsafe-eval"} {
			if strings.Contains(csp, bad) {
				t.Errorf("%s CSP 含 %s", c.path, bad)
			}
		}
		// 管理面**不加任何 CORS 头**：浏览器同源脚本不需要它，跨源脚本拿不到 Bearer 头。
		for _, h := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Headers", "Access-Control-Allow-Methods", "Access-Control-Allow-Credentials"} {
			if v := resp.Header.Get(h); v != "" {
				t.Errorf("%s 出现了 CORS 头 %s=%s", c.path, h, v)
			}
		}
	}

	// 未鉴权的管理 API 仍然是 401：页面挂载点不是鉴权旁路。
	for _, p := range []string{"/api/admin/whoami", "/api/admin/sites", "/api/admin/audit"} {
		resp := x.call(http.MethodGet, p, nil, "")
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("未鉴权 GET %s 应 401，got %d %s", p, resp.StatusCode, raw)
		}
	}

	// 静态面只读、且没有目录：POST 与未知子路径都不能拿到内容。
	if resp := x.call(http.MethodPost, "/admin", map[string]any{"x": 1}, ""); resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		t.Error("POST /admin 不应 200")
	} else {
		resp.Body.Close()
	}
	for _, p := range []string{"/admin/app.js.map", "/admin/assets/app.js", "/admin/etc/passwd"} {
		resp := x.call(http.MethodGet, p, nil, "")
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s 不应 200：%s", p, strings.TrimRight(string(raw), "\n"))
		}
	}
}

// TestAdminUIAssetsMatchEmbeddedBytes 确认管理 Server 发出去的就是**内嵌那一份字节**
// （不是另一份磁盘拷贝，也不是服务端渲染过的 HTML）。
func TestAdminUIAssetsMatchEmbeddedBytes(t *testing.T) {
	x := newHarness(t, nil)
	cases := map[string]string{
		"/admin":         adminui.AssetHTML,
		"/admin/app.js":  adminui.AssetJS,
		"/admin/app.css": adminui.AssetCSS,
	}
	for path, asset := range cases {
		want, err := adminui.AssetBytes(asset)
		if err != nil {
			t.Fatalf("AssetBytes(%s): %v", asset, err)
		}
		resp := x.call(http.MethodGet, path, nil, "")
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		if string(raw) != string(want) {
			t.Errorf("GET %s 与内嵌的 %s 不一致（%d vs %d 字节）", path, asset, len(raw), len(want))
		}
		if got := resp.Header.Get("ETag"); got != adminui.ETag(asset) {
			t.Errorf("GET %s ETag=%s want=%s", path, got, adminui.ETag(asset))
		}
	}
}
