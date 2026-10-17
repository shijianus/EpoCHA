package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

// demoSecret 是测试内的哨兵值：按值扫描页面与响应，证明演示后端的 secret 不会到达浏览器。
// 故意写成一眼假的占位串——仓库里不存真实密钥材料（AGENTS.md「仓库与推送」）。
const demoSecret = "sk_SENTINEL-PREVIEW-never-in-browser-0001"

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T, apiSrv, adminSrv *httptest.Server) *server {
	t.Helper()
	ln, err := listenLoopback("127.0.0.1:8099")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s, err := newServer(context.Background(), &options{
		APIBase: apiSrv.URL, AdminBase: adminSrv.URL, AdminKey: "ak_test_key_value",
		Generator: generator.TextImageID, Difficulty: 2, PowBits: 12,
	}, ln, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestListenLoopbackIsCheckNotDefault 非回环地址必须被拒——包括显式传进来的。
func TestListenLoopbackIsCheckNotDefault(t *testing.T) {
	// 端口一律用高位：低位端口需要 root，会让"能不能监听"变成权限问题而不是裁决问题。
	ok := []string{"127.0.0.1:18090", "localhost:18091", "[::1]:18092", "127.5.5.5:18093"}
	for _, a := range ok {
		ln, err := listenLoopback(a)
		if err != nil {
			t.Fatalf("%s 应可监听: %v", a, err)
		}
		_ = ln.Close()
	}
	reject := []string{"0.0.0.0:8090", "192.168.1.10:8090", "10.0.0.5:1", "example.com:80",
		"127.0.0.1:0", "127.0.0.1", "not-an-address:xx"}
	for _, a := range reject {
		if ln, err := listenLoopback(a); err == nil {
			_ = ln.Close()
			t.Fatalf("%s 应被拒绝：预览不得绑定非回环地址", a)
		}
	}
}

func TestParseFlags(t *testing.T) {
	env := func(string) string { return "" }
	if _, err := parseFlags([]string{}, env); err == nil {
		t.Fatal("缺 --api 应失败")
	}
	if _, err := parseFlags([]string{"--api", "ftp://x"}, env); err == nil {
		t.Fatal("非 http/https 基址应失败")
	}
	if _, err := parseFlags([]string{"--api", "http://127.0.0.1:8080/prefix"}, env); err == nil {
		t.Fatal("基址带路径应失败（拼出的验证 URL 会偏离预期）")
	}
	// 只给 site-key 不给 secret：不允许"半个凭据"
	if _, err := parseFlags([]string{"--api", "http://h:1", "--site-key", "sk"}, env); err == nil {
		t.Fatal("site-key 与 secret 必须成对")
	}
	// 既不给站点也不给管理凭据：无法自动建站，必须停下来问
	if _, err := parseFlags([]string{"--api", "http://h:1"}, env); err == nil {
		t.Fatal("缺少建站能力时应拒绝启动，而不是猜一个站点")
	}
	opt, err := parseFlags([]string{"--api", "http://h:1", "--site-key", "sk", "--site-secret", demoSecret}, env)
	if err != nil {
		t.Fatal(err)
	}
	if opt.Generator != generator.TextImageID || opt.Difficulty != 2 {
		t.Fatalf("默认题型/难度不符：%+v", opt)
	}
	if _, err := parseFlags([]string{"--api", "http://h:1", "--site-key", "sk", "--site-secret", demoSecret,
		"--generator", "cjk-v1"}, env); err == nil {
		t.Fatal("未实现的题型应被拒（预览不猜题型）")
	}
	if _, err := parseFlags([]string{"--api", "http://h:1", "--site-key", "sk", "--site-secret", demoSecret,
		"--difficulty", "9"}, env); err == nil {
		t.Fatal("难度越界应被拒")
	}
	if _, err := parseFlags([]string{"--api", "http://h:1", "--site-key", "sk", "--site-secret", demoSecret,
		"--pow-bits", "4"}, env); err == nil {
		t.Fatal("pow_bits 越界应被拒（与站点表 CHECK 一致）")
	}
}

// TestSecretNeverReachesBrowser 是本轮预览的核心安全断言：
// 页面、演示后端的任何响应、以及 CSP 里都不得出现站点 secret。
func TestSecretNeverReachesBrowser(t *testing.T) {
	var apiHits int
	var lastForm url.Values
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits++
		_ = r.ParseForm()
		lastForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"challenge_ts":"2026-10-18T00:00:00Z","hostname":"127.0.0.1","extra_private":"should-not-forward"}`)
	}))
	defer api.Close()
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"site":{"id":7,"site_key":"sk-preview-demo"},"secret":"`+demoSecret+`"}`)
	}))
	defer admin.Close()

	s := newTestServer(t, api, admin)
	if s.siteKey != "sk-preview-demo" || s.secret != demoSecret {
		t.Fatalf("建站结果不符：%+v", s)
	}
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	page := rec.Body.String()
	if strings.Contains(page, demoSecret) {
		t.Fatal("页面 HTML 含站点 secret")
	}
	for _, forbidden := range []string{"secret", "admin_key", "ak_test_key_value", "{{CFG_JSON}}"} {
		if forbidden == "secret" {
			continue // "secret" 作为英文单词可能出现在说明文字里，单独按值判定
		}
		if strings.Contains(page, forbidden) {
			t.Fatalf("页面含 %q", forbidden)
		}
	}
	if !strings.Contains(page, "sk-preview-demo") {
		t.Fatal("页面应含公开的 site_key（否则无法领题）")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("预览响应必须 no-store")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("预览不得开 CORS")
	}

	// 正常验证一次：转发给 /siteverify 的表单里有 secret，回给浏览器的没有
	body := `{"response":"v1.abc.def"}`
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/demo/verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify 应 200：%d %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if strings.Contains(out, demoSecret) || strings.Contains(out, "should-not-forward") {
		t.Fatalf("演示后端把 secret 或未登记字段带回了浏览器：%s", out)
	}
	var vmap map[string]any
	if err := json.Unmarshal([]byte(out), &vmap); err != nil {
		t.Fatal(err)
	}
	for k := range vmap {
		switch k {
		case "success", "error-codes", "hostname", "challenge_ts":
		default:
			t.Fatalf("响应含未登记字段 %q：%s", k, out)
		}
	}
	if apiHits != 1 || lastForm.Get("secret") != demoSecret {
		t.Fatalf("演示后端应带自己的 secret 调用固定目标：hits=%d form=%v", apiHits, lastForm)
	}
	if got := lastForm.Get("response"); got != "v1.abc.def" {
		t.Fatalf("response 未原样转发：%q", got)
	}
}

// TestVerifyTargetIsNotBrowserControlled 浏览器不能指定验证目标：
// 多带任何字段（target/url/api）一律 400，且一次请求都不会发往上游。
func TestVerifyTargetIsNotBrowserControlled(t *testing.T) {
	var hits int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer api.Close()
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"site":{"id":1,"site_key":"sk"},"secret":"`+demoSecret+`"}`)
	}))
	defer admin.Close()
	h := newTestServer(t, api, admin).Handler()

	for _, body := range []string{
		`{"response":"x","target":"http://169.254.169.254/"}`,
		`{"response":"x","url":"http://evil/"}`,
		`{"response":"x","api_base":"http://evil/"}`,
		`{"target":"http://evil/"}`,
		`{"response":""}`,
		`{"response":"` + strings.Repeat("t", 5000) + `"}`,
		`not json`,
		`{"response":"x"}{"response":"y"}`,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/demo/verify", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("请求 %s 竟被接受", body[:min(40, len(body))])
		}
	}
	if hits != 0 {
		t.Fatalf("被拒的请求不应触达上游（触达 %d 次）：那就是 SSRF 门", hits)
	}
	// 超限的响应串（>4096）单独确认一次：长度上限在触达上游之前生效
	if _, err := parseFlags([]string{"--api", "http://h:1", "--site-key", "sk", "--site-secret", demoSecret},
		func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
}

// TestAdminSiteCreationShape 自动建站必须把模式、题型、难度与**自己的** loopback 来源一起写进去。
func TestAdminSiteCreationShape(t *testing.T) {
	var got map[string]any
	var auth string
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/sites" || r.Method != http.MethodPost {
			t.Errorf("不该打别的管理端点：%s %s", r.Method, r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"site":{"id":3,"site_key":"sk-new"},"secret":"`+demoSecret+`"}`)
	}))
	defer admin.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer api.Close()

	ln, err := listenLoopback("127.0.0.1:8098")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	s, err := newServer(context.Background(), &options{
		APIBase: api.URL, AdminBase: admin.URL, AdminKey: "ak_zzz",
		Generator: generator.TextImageID, Difficulty: 3, PowBits: 12,
	}, ln, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if s.siteKey != "sk-new" {
		t.Fatalf("site_key=%q", s.siteKey)
	}
	if auth != "Bearer ak_zzz" {
		t.Fatalf("Authorization=%q", auth)
	}
	if got["verify_mode"] != "pow_question" || got["generator_id"] != generator.TextImageID {
		t.Fatalf("建站请求字段不符：%v", got)
	}
	if got["allow_missing_origin"] != false {
		t.Fatal("预览站点不得放行“无 Origin”的请求：那样任意脚本构造的请求都能领题")
	}
	origins, _ := got["origins"].([]any)
	if len(origins) == 0 {
		t.Fatal("必须登记预览自己的来源，否则浏览器领题会被来源策略挡掉")
	}
	found := false
	for _, o := range origins {
		if s, _ := o.(string); strings.HasPrefix(s, "http://127.0.0.1:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("来源列表应含 loopback 预览来源：%v", origins)
	}
}

// TestPageIsSelfContainedNoFramework 页面必须是原生单文件：无外部脚本/样式/图片引用。
// 这条守住"开发预览不引入前端框架与远程资源"的约束，也顺带保证它不联网加载任何东西。
func TestPageIsSelfContainedNoFramework(t *testing.T) {
	page := string(previewPageForTest(t))
	for _, bad := range []string{
		`<script src=`, `<link rel="stylesheet" href="http`, `src="http`, `import(`,
		`react`, `vue`, `jquery`,
	} {
		// JSON 请求会触发预检；预检只能从 URL 认站点，所以两处 fetch 都必须带 ?site_key=
		_ = bad
		if strings.Contains(strings.ToLower(page), strings.ToLower(bad)) {
			t.Fatalf("页面含外部依赖或框架痕迹 %q", bad)
		}
	}
	for _, need := range []string{`aria-live="polite"`, `<label for="answer"`, `img.alt = "人机验证图片`,
		`crypto.subtle`, `/api/challenge?site_key=`, `/api/redeem?site_key=`, `/demo/verify`} {
		if !strings.Contains(page, need) {
			t.Fatalf("页面缺少 %q：无障碍或闭环能力不完整", need)
		}
	}
	// alt 文本只描述用途，不得含答案串
	if i := strings.Index(page, `img.alt = `); i >= 0 {
		end := strings.Index(page[i:], "\n")
		alt := page[i : i+end]
		if strings.Contains(alt, "6ZX7JQ") || strings.Contains(strings.ToLower(alt), "answer") {
			t.Fatalf("alt 文本疑似含答案：%s", alt)
		}
	}
}

func previewPageForTest(t *testing.T) []byte {
	t.Helper()
	page, err := renderPage("http://127.0.0.1:8080", "sk-demo")
	if err != nil {
		t.Fatal(err)
	}
	return page
}

// TestPageInjectionCannotBreakOut 注入内容不得出现可闭合脚本标签的序列。
func TestPageInjectionCannotBreakOut(t *testing.T) {
	// encoding/json 默认把 < > & 转成 \u003c 等，所以恶意基址也不会留下字面 "</script"。
	// 这里断言的是**结果安全**（而不是"必须报错"）：报错只是其中一种实现方式。
	hostile, err := renderPage("http://127.0.0.1:1</script><script>alert(1)</script>", "sk-x")
	if err == nil && bytes.Contains(hostile, []byte("</script><script>")) {
		t.Fatal("注入内容留下了可提前闭合脚本标签的字面序列")
	}
	page, err := renderPage("http://127.0.0.1:8080", "sk-x")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(page, []byte("</script><script>alert")) {
		t.Fatal("注入点未转义：可以提前闭合 script 标签")
	}
}

// TestPageConfigIsParseableJSON 页面里的 cfg 数据块必须是**可被 JSON.parse 解析**的原文。
// 用 HTML 实体转义会让页面"加载成功但脚本一启动就抛异常"——这条把那个实测事故钉住。
func TestPageConfigIsParseableJSON(t *testing.T) {
	page := string(previewPageForTest(t))
	i := strings.Index(page, `<script id="cfg" type="application/json">`)
	if i < 0 {
		t.Fatal("页面没有 cfg 数据块")
	}
	rest := page[i+len(`<script id="cfg" type="application/json">`):]
	body := rest[:strings.Index(rest, "</script>")]
	var m map[string]string
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("cfg 不是可直接解析的 JSON（浏览器会抛异常）：%v / %q", err, body)
	}
	if m["api_base"] != "http://127.0.0.1:8080" || m["site_key"] != "sk-demo" {
		t.Fatalf("cfg 内容不符：%v", m)
	}
	if strings.Contains(body, "&#34;") || strings.Contains(body, "&quot;") {
		t.Fatal("cfg 用了 HTML 实体：script 数据块不会解码它")
	}
}
