package widget

// 这几条测试守的是"两份拷贝不能分叉"和"生产脚本里不该出现的构造"。
// 它们都不需要浏览器：读字节、比字节、扫源码。浏览器里的真实行为由 BiDi 测试负责
// （docs/scripts/browser/，见 docs/STAGE4A.md §5）。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const workerMarker = "var POW_WORKER_SOURCE = String.raw"

func TestEmbeddedWorkerSourceMatchesPublishedFile(t *testing.T) {
	js, err := AssetBytes(JSPath)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := extractTemplateLiteral(string(js), workerMarker)
	if err != nil {
		t.Fatal(err)
	}
	published, err := AssetBytes(WorkerPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimRight(string(published), "\n"); embedded != want {
		t.Fatalf("v1.js 内嵌的 worker 源码与 v1.worker.js 不一致：内嵌 %d 字节 / 文件 %d 字节\n"+
			"（Blob 路径与同源路径会跑到两份不同代码上，必须只留一份真值）", len(embedded), len(want))
	}
	// 内嵌副本与工作树文件也必须一致（防止只改了嵌入块而忘了文件，或反过来）。
	onDisk, err := os.ReadFile("v1.worker.js")
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(published) {
		t.Fatal("go:embed 的内容与磁盘文件不一致（未保存？构建缓存？）")
	}
}

func TestWidgetSourceHygiene(t *testing.T) {
	sources := map[string]string{}
	for _, name := range []string{JSPath, WorkerPath} {
		b, err := AssetBytes(name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(b)
	}
	// 禁止的构造：这些都会把"服务端返回的字符串"或"任意字符串"变成可执行代码/标记。
	forbidden := []string{
		"eval(", "new Function", "document.write", "innerHTML", "outerHTML",
		"insertAdjacentHTML", "srcdoc", "import(", "importScripts(", "with (",
	}
	for name, src := range sources {
		for _, f := range forbidden {
			if strings.Contains(src, f) {
				t.Errorf("%s 含有被禁止的构造 %q", name, f)
			}
		}
	}
	// 生产脚本绝不碰管理面与 token 消费接口：那些是服务端到服务端的。
	for name, src := range sources {
		for _, path := range []string{"/siteverify", "/api/admin", "secret", "pepper", "master_key", "admin_key"} {
			if strings.Contains(strings.ToLower(src), path) {
				t.Errorf("%s 引用了 %q（widget 只允许用 site_key 打 /api/challenge 与 /api/redeem）", name, path)
			}
		}
	}
	js := sources[JSPath]
	// 这两条是"正面断言"：漏了任何一条，浏览器侧的安全口径就不成立。
	for _, want := range []string{
		`credentials: "omit"`,
		"?site_key=",
		`attachShadow({ mode: "open" })`,
		`role: "status"`,
		`aria-live`,
		"revokeObjectURL",
		"removeEventListener",
		"AbortController",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("v1.js 应包含 %q", want)
		}
	}
	// 算法标识必须写死成 sha256-prefix：服务端换算法时组件要**明确失败**，不能"猜一个"。
	if !strings.Contains(js, `POW_ALGO = "sha256-prefix"`) {
		t.Error("v1.js 没有钉住 PoW 算法标识")
	}
}

func TestHandlerServesAssets(t *testing.T) {
	h := Handler()
	for _, tc := range []struct {
		path, wantHead string
	}{
		{URLJS, "/*! epocha widget v1"},
		{URLWorker, "/*! epocha widget PoW worker v1"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		res := rec.Result()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", tc.path, res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		text := string(body)
		if !strings.HasPrefix(text, tc.wantHead) {
			t.Errorf("%s 开头 = %q，期望 %q", tc.path, firstLine(text), tc.wantHead)
		}
		if ct := res.Header.Get("Content-Type"); ct != contentType {
			t.Errorf("%s Content-Type = %q", tc.path, ct)
		}
		if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
			t.Errorf("%s Cache-Control = %q", tc.path, cc)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s 缺 nosniff", tc.path)
		}
		if res.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s 不该带 CORS 头", tc.path)
		}
		// 服务端给的字节必须与内嵌资源逐字节相同（不是"差不多"）。
		want, err := AssetBytes(strings.TrimPrefix(tc.path, "/widget/"))
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != text {
			t.Errorf("%s 内容与内嵌资源不一致（%d vs %d 字节）", tc.path, len(text), len(want))
		}
	}
}

func TestHandlerConditionalRequest(t *testing.T) {
	h := Handler()
	et := ETag(JSPath)
	if et == "" || et == `""` {
		t.Fatal("ETag 没算出来")
	}
	req := httptest.NewRequest(http.MethodGet, URLJS, nil)
	req.Header.Set("If-None-Match", et)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != http.StatusNotModified {
		t.Fatalf("带匹配 ETag 的请求应 304，实得 %d", rec.Result().StatusCode)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("304 不应带响应体：%q", firstLine(body))
	}
	// 内容变了就必须不再是 304：否则"版本化 URL + 长缓存"会变成"永远发旧脚本"。
	req2 := httptest.NewRequest(http.MethodGet, URLJS, nil)
	req2.Header.Set("If-None-Match", `"deadbeefdeadbeef"`)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Result().StatusCode != http.StatusOK {
		t.Fatalf("不匹配 ETag 应 200，实得 %d", rec2.Result().StatusCode)
	}
}

func TestHandlerUnknownPaths(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/widget/", "/widget/v2.js", "/widget/../internal/config/config.go", "/api/challenge", "/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		res := rec.Result()
		code := res.StatusCode
		// Go 1.27.3's ServeMux uses 307 for path-cleaning redirects; older
		// toolchains used 301. Both must lead to a non-resource 404.
		if code == http.StatusMovedPermanently || code == http.StatusTemporaryRedirect {
			loc := res.Header.Get("Location")
			if strings.Contains(loc, "config") && !strings.HasPrefix(loc, "/internal") && !strings.HasPrefix(loc, "/widget") {
				t.Errorf("%s 的重定向目标可疑：%q", path, loc)
			}
			rec2 := httptest.NewRecorder()
			h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, loc, nil))
			if rec2.Result().StatusCode != http.StatusNotFound {
				t.Errorf("%s → %s = %d，期望跟随后 404", path, loc, rec2.Result().StatusCode)
			}
			continue
		}
		if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d，期望 404", path, code)
		}
		if body := rec.Body.String(); strings.Contains(body, "package config") || strings.Contains(body, "MasterKey") {
			t.Errorf("GET %s 泄露了源码：%s", path, firstLine(body))
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, URLJS, strings.NewReader("{}")))
	if code := rec.Result().StatusCode; code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
		t.Errorf("POST %s = %d，期望 405/404", URLJS, code)
	}
	// HEAD 由 ServeContent 支持且必须给正确的长度（接入方的 CDN 探测与镜像工具会发 HEAD）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, URLJS, nil))
	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("HEAD %s = %d", URLJS, rec.Result().StatusCode)
	}
}

func TestAssetBytesRejectsUnknownName(t *testing.T) {
	for _, name := range []string{"", "v1.js/", "../go.mod", "v1.jsx", "widget.go"} {
		if _, err := AssetBytes(name); err == nil {
			t.Errorf("AssetBytes(%q) 竟然成功了——资源名必须走白名单", name)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
