package adminui

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// 高熵凭据形状。index.html 与 app.js 里出现的 "ak_…"/"pk_…" 是给人看的**前缀说明**，
// 后面跟的是省略号而不是 20 个以上字符，所以不会被这两条命中。
var (
	reAdminKey = regexp.MustCompile(`ak_[0-9A-Za-z_-]{20,}`)
	reSecret   = regexp.MustCompile(`sk_[0-9a-fA-F]{24,}`)
)

func allAssets() []string { return []string{AssetHTML, AssetCSS, AssetJS} }

func TestAssetWhitelist(t *testing.T) {
	for _, name := range allAssets() {
		b, err := AssetBytes(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(b) == 0 {
			t.Errorf("%s 内嵌内容为空", name)
		}
	}
	// 白名单之外一律拒绝：不存在"按名字拼路径"这件事，因此目录穿越在结构上不可能。
	for _, bad := range []string{"", "../go.mod", "index.html/", "app.jss", "adminui.go", "/etc/hosts"} {
		if _, err := AssetBytes(bad); err == nil {
			t.Errorf("AssetBytes(%q) 应当失败", bad)
		}
	}
}

func TestETagIsContentDerived(t *testing.T) {
	for _, name := range allAssets() {
		b, err := AssetBytes(name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		want := `"` + hex.EncodeToString(sum[:8]) + `"`
		if got := ETag(name); got != want {
			t.Errorf("%s ETag=%s want=%s", name, got, want)
		}
	}
	if got := ETag("不存在"); got != "" {
		t.Errorf("未知资源的 ETag 应为空串，got %q", got)
	}
}

func TestNoCredentialShapesInAssets(t *testing.T) {
	for _, name := range allAssets() {
		b, err := AssetBytes(name)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if m := reAdminKey.FindString(s); m != "" {
			t.Errorf("%s 含疑似管理密钥串：%s…", name, m[:12])
		}
		if m := reSecret.FindString(s); m != "" {
			t.Errorf("%s 含疑似站点 secret 串：%s…", name, m[:12])
		}
	}
}

// TestSecurityHeaders 是页面侧红线的落点：CSP 不含 unsafe-inline/unsafe-eval、
// frame-ancestors 'none'、no-store、nosniff；且脚本与样式都走同源外部文件。
func TestSecurityHeaders(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	cases := []struct {
		path string
		mime string
	}{
		{URLIndex, "text/html; charset=utf-8"},
		{URLIndex + "/", "text/html; charset=utf-8"},
		{URLJS, "text/javascript; charset=utf-8"},
		{URLCSS, "text/css; charset=utf-8"},
	}
	for _, c := range cases {
		resp, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("读 %s 响应体: %v", c.path, err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 应 200，got %d", c.path, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != c.mime {
			t.Errorf("%s Content-Type=%q want %q", c.path, got, c.mime)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if csp == "" {
			t.Fatalf("%s 没有 CSP 头", c.path)
		}
		for _, bad := range []string{"unsafe-inline", "unsafe-eval"} {
			if strings.Contains(csp, bad) {
				t.Errorf("%s 的 CSP 含 %s：%s", c.path, bad, csp)
			}
		}
		for _, want := range []string{"frame-ancestors 'none'", "default-src 'none'", "script-src 'self'", "connect-src 'self'", "base-uri 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s 的 CSP 缺 %q：%s", c.path, want, csp)
			}
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control=%q", c.path, cc)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s nosniff=%q", c.path, got)
		}
		if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s X-Frame-Options=%q", c.path, got)
		}
		if got := resp.Header.Get("ETag"); got != ETag(assetForPath(c.path)) {
			t.Errorf("%s ETag=%q 与内容哈希不符", c.path, got)
		}
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			want := itoa(len(raw))
			if cl != want {
				t.Errorf("%s Content-Length=%s 与实际字节 %s 不符", c.path, cl, want)
			}
		} else {
			t.Errorf("%s 缺少 Content-Length", c.path)
		}
		// 页面壳不得内联脚本或内联事件：CSP 已经挡住，这里再钉一次"写法本身也是干净的"。
		if c.mime == "text/html; charset=utf-8" {
			lower := strings.ToLower(string(raw))
			for _, forbidden := range []string{"<script>", "onclick=", "onerror=", "style=\""} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("页面内联了 %q（CSP 不允许，也不该依赖 CSP 兜底）", forbidden)
				}
			}
			if !strings.Contains(lower, `<script src="/admin/app.js"`) {
				t.Error("页面没有引用同源的 app.js")
			}
			if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
				t.Errorf("页面源代码含外部绝对 URL（不得加载第三方资源）")
			}
		}
	}
}

func assetForPath(path string) string {
	switch path {
	case URLJS:
		return AssetJS
	case URLCSS:
		return AssetCSS
	default:
		return AssetHTML
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestUnknownSubpathsAreNotFound 防的是"子树模式把整棵 /admin/ 都当成首页"：
// 那等于给管理监听器开一个可枚举的目录面。
func TestUnknownSubpathsAreNotFound(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	for _, p := range []string{
		"/admin/app.js.map", "/admin/index.html", "/admin/assets/app.js",
		"/admin/../etc/passwd", "/admin/%2e%2e/%2e%2e/etc/hosts", "/admin/pool",
	} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s 竟然 200：%s", p, trunc(string(raw)))
		}
		if strings.Contains(string(raw), "root:") || strings.Contains(string(raw), "/bin/bash") {
			t.Errorf("GET %s 读到了系统文件内容", p)
		}
	}
}

// TestMethodNotGetIsRejected：静态面只读，不接受写方法。
func TestMethodNotGetIsRejected(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(m, srv.URL+URLIndex, strings.NewReader("{}"))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s %s 不应 200", m, URLIndex)
		}
	}
	// HEAD 走同一条路径且带长度头但不带正文。
	req, _ := http.NewRequest(http.MethodHead, srv.URL+URLJS, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD %s 应 200，got %d", URLJS, resp.StatusCode)
	}
	if len(hb) != 0 {
		t.Errorf("HEAD 不应带正文，got %d 字节", len(hb))
	}
	if resp.Header.Get("Content-Length") == "" {
		t.Error("HEAD 应带 Content-Length")
	}
}

func trunc(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
