package main

// 示例业务后端的验收点：**只有服务端明确 success=true 才写入**，其余一律不写。
// 这里的 /siteverify 是 httptest 桩（快、可控、能计数）；
// 与真实服务端的闭环由 docs/scripts/stage4a.sh 和浏览器测试负责，两件事不互相冒充。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type stub struct {
	mu      sync.Mutex
	calls   int
	lastURL string
	form    url.Values
	reply   func(call int, form url.Values) (int, any)
}

func (s *stub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		form, _ := url.ParseQuery(string(body))
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.lastURL = r.URL.Path
		s.form = form
		reply := s.reply
		s.mu.Unlock()
		if reply == nil {
			reply = func(int, url.Values) (int, any) { return http.StatusOK, map[string]any{"success": true} }
		}
		code, payload := reply(n, form)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(payload)
	})
}

func (s *stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func newTestApp(t *testing.T, reply func(call int, form url.Values) (int, any)) (*app, *stub) {
	t.Helper()
	sb := &stub{reply: reply}
	ts := httptest.NewServer(sb.handler())
	t.Cleanup(ts.Close)
	a, err := newApp(context.Background(), &options{
		Listen: ts.Listener.Addr().String(), APIBase: ts.URL, AdminBase: "",
		SiteKey: "pk-example-test", SiteSecret: "sk_EXAMPLE_TEST_SECRET_DO_NOT_LEAK_0001",
		GuestbookDB: t.TempDir() + "/gb.db", WidgetFrom: "api", CSPMode: "blob",
		VerifyTO: 2 * time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() { _ = a.gb.Close() })
	a.o.APIBase = ts.URL
	return a, sb
}

func post(t *testing.T, a *app, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/guestbook", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Host = a.o.Listen
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

func mustCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%s", rec.Body.String())
	}
	if got["error"] != want {
		t.Fatalf("error = %v，期望 %q（body=%s）", got["error"], want, rec.Body.String())
	}
}

func countMessages(t *testing.T, a *app) int {
	t.Helper()
	var n int
	if err := a.gb.QueryRow(`SELECT count(*) FROM guestbook`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func bodyWith(token, text string) string {
	b, _ := json.Marshal(map[string]string{"response": token, "text": text})
	return string(b)
}

// --- 正面与反面的授权路径 ---

func TestPostWritesOnlyWhenVerifySucceeds(t *testing.T) {
	a, sb := newTestApp(t, nil)
	rec := post(t, a, bodyWith("tok-good", "第一条留言"), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("success=true 应写入：%d %s", rec.Code, rec.Body.String())
	}
	if sb.count() != 1 || sb.lastURL != "/siteverify" {
		t.Fatalf("应恰好调用一次 /siteverify，实得 %d 次（%s）", sb.count(), sb.lastURL)
	}
	if got := countMessages(t, a); got != 1 {
		t.Fatalf("留言数 = %d，期望 1", got)
	}
}

func TestPostRejectsMissingToken(t *testing.T) {
	a, sb := newTestApp(t, nil)
	rec := post(t, a, bodyWith("", "没有凭据也想写"), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无 token 应 403，实得 %d", rec.Code)
	}
	mustCode(t, rec, errMissingToken)
	// 关键演示点：前端的 onSuccess 不是授权 —— 没有 token 时**连 /siteverify 都不该被打**，
	// 更不能写入。
	if sb.count() != 0 {
		t.Fatalf("无 token 却调用了验证服务 %d 次", sb.count())
	}
	if got := countMessages(t, a); got != 0 {
		t.Fatalf("无 token 竟然写入了 %d 条留言", got)
	}
}

func TestPostRejectsInvalidToken(t *testing.T) {
	a, sb := newTestApp(t, func(int, url.Values) (int, any) {
		return http.StatusOK, map[string]any{"success": false, "error-codes": []string{"invalid-input-response"}}
	})
	rec := post(t, a, bodyWith("tok-bad", "坏 token"), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("坏 token 应 403，实得 %d %s", rec.Code, rec.Body.String())
	}
	mustCode(t, rec, errInvalidTok)
	if got := countMessages(t, a); got != 0 {
		t.Fatalf("坏 token 竟然写入了 %d 条", got)
	}
	if sb.form.Get("response") != "tok-bad" {
		t.Fatalf("验证调用没带上页面提交的那个 token：%+v", sb.form)
	}
}

func TestPostRejectsDuplicateToken(t *testing.T) {
	a, sb := newTestApp(t, func(call int, _ url.Values) (int, any) {
		if call == 1 {
			return http.StatusOK, map[string]any{"success": true}
		}
		return http.StatusOK, map[string]any{"success": false, "error-codes": []string{"timeout-or-duplicate"}}
	})
	if rec := post(t, a, bodyWith("tok-once", "第一次"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("第一次应成功：%d %s", rec.Code, rec.Body.String())
	}
	rec := post(t, a, bodyWith("tok-once", "第二次重放"), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重放应 409，实得 %d %s", rec.Code, rec.Body.String())
	}
	mustCode(t, rec, errDuplicateTok)
	if got := countMessages(t, a); got != 1 {
		t.Fatalf("重放后留言数 = %d，期望仍然是 1", got)
	}
	// 两次提交都真的打到了验证服务：重放是在服务端被拒，不是被页面挡下。
	if sb.count() != 2 {
		t.Fatalf("应调用 /siteverify 两次，实得 %d", sb.count())
	}
}

func TestPostFailsClosedWhenVerifyUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		reply  func(call int, form url.Values) (int, any)
		broken bool
	}{
		{"5xx 带 internal-error", func(int, url.Values) (int, any) {
			return http.StatusInternalServerError, map[string]any{"success": false, "error-codes": []string{"internal-error"}}
		}, false},
		{"200 但 body 不是 JSON", func(int, url.Values) (int, any) {
			return http.StatusOK, "<html>网关错误</html>"
		}, false},
		{"success 字段缺失", func(int, url.Values) (int, any) {
			return http.StatusOK, map[string]any{"challenge_ts": "2026-10-18T00:00:00Z"}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newTestApp(t, tc.reply)
			rec := post(t, a, bodyWith("tok-x", "验证服务不健康时的提交"), nil)
			if rec.Code != http.StatusForbidden && rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("应 fail closed（403/503），实得 %d %s", rec.Code, rec.Body.String())
			}
			if got := countMessages(t, a); got != 0 {
				t.Fatalf("验证不可用却写入了 %d 条", got)
			}
		})
	}
}

func TestPostFailsClosedWhenVerifyTimesOut(t *testing.T) {
	// 指向一个 accept 但不响应的服务器：超时必须是"不放行"，而不是"当通过"。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			time.AfterFunc(2*time.Second, func() { _ = c.Close() })
		}
	}()
	a, _ := newTestApp(t, nil)
	a.o.APIBase = "http://" + ln.Addr().String()
	a.verifyTO = 300 * time.Millisecond
	start := time.Now()
	rec := post(t, a, bodyWith("tok-slow", "验证服务挂起"), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("验证超时应 503，实得 %d %s", rec.Code, rec.Body.String())
	}
	mustCode(t, rec, errVerifyDown)
	if time.Since(start) > 3*time.Second {
		t.Fatalf("超时没有被执行：用了 %s", time.Since(start))
	}
	if got := countMessages(t, a); got != 0 {
		t.Fatalf("验证超时却写入了 %d 条", got)
	}
}

// --- 结构边界 ---

func TestClientCannotChooseVerifyTarget(t *testing.T) {
	a, sb := newTestApp(t, nil)
	// 多带 target / api / secret 任一字段都拒绝；被拒的请求**一次都不会**打到验证服务。
	for _, extra := range []string{
		`"target":"http://127.0.0.1:1/security-dump"`,
		`"api_base":"http://attacker.example"`,
		`"secret":"sk_guessed"`,
	} {
		body := `{"response":"tok","text":"hi",` + extra + `}`
		rec := post(t, a, body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("带 %s 应 400，实得 %d %s", extra, rec.Code, rec.Body.String())
		}
		mustCode(t, rec, "epocha-bad-request")
	}
	if sb.count() != 0 {
		t.Fatalf("被拒请求竟触发了 %d 次验证调用", sb.count())
	}
}

func TestCrossSiteOriginRejected(t *testing.T) {
	a, sb := newTestApp(t, nil)
	rec := post(t, a, bodyWith("tok", "跨站表单提交"), map[string]string{"Origin": "http://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站 Origin 应 403，实得 %d", rec.Code)
	}
	mustCode(t, rec, errOrigin)
	if sb.count() != 0 || countMessages(t, a) != 0 {
		t.Fatal("跨站请求不应触达验证服务，也不应写入")
	}
	// 同源页面带的 Origin 必须通过（否则示例自己都跑不起来）。
	if rec := post(t, a, bodyWith("tok", "同源提交"), map[string]string{"Origin": "http://" + a.o.Listen}); rec.Code != http.StatusCreated {
		t.Fatalf("同源提交应成功：%d %s", rec.Code, rec.Body.String())
	}
}

func TestSecretNeverReachesBrowser(t *testing.T) {
	a, sb := newTestApp(t, nil)
	const adminKey = "ak_ADMIN_KEY_MUST_NOT_APPEAR"
	a.o.AdminKey = adminKey

	// 页面、留言列表、错误响应三类浏览器可见内容全部按值扫描。
	parts := []string{}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	parts = append(parts, rec.Body.String())

	rec = post(t, a, bodyWith("", "无凭据"), nil)
	parts = append(parts, rec.Body.String())
	rec = post(t, a, bodyWith("tok", "正常"), nil)
	parts = append(parts, rec.Body.String())

	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/guestbook", nil))
	parts = append(parts, rec.Body.String())

	for _, p := range parts {
		if strings.Contains(p, a.secret) {
			t.Fatalf("验证凭据出现在浏览器可见内容里：%s", firstLine(p))
		}
		if strings.Contains(p, adminKey) {
			t.Fatal("管理密钥出现在浏览器可见内容里")
		}
	}
	// 正面控制：凭据确实被发给了固定的验证目标（否则上面的扫描是空转）。
	if sb.form.Get("secret") != a.secret {
		t.Fatalf("桩服务没收到预期凭据（calls=%d）——扫描断言没有覆盖到真实路径", sb.count())
	}
	if sb.lastURL != "/siteverify" {
		t.Fatalf("验证目标路径 = %q", sb.lastURL)
	}
	// 页面里的配置只允许公开值。
	if !strings.Contains(parts[0], a.siteKey) {
		t.Fatal("页面配置里应出现公开的 site_key（缺它组件无法工作）")
	}
}

func TestWriteFailureDoesNotCacheSuccess(t *testing.T) {
	// token 已被 /siteverify 消费；业务写入失败时必须拒绝"用同一次成功蒙过去"。
	calls := 0
	a, _ := newTestApp(t, func(call int, _ url.Values) (int, any) {
		calls = call
		if call == 1 {
			return http.StatusOK, map[string]any{"success": true}
		}
		return http.StatusOK, map[string]any{"success": false, "error-codes": []string{"timeout-or-duplicate"}}
	})
	a.writeFailOnce = true
	rec := post(t, a, bodyWith("tok-once", "验证通过但写入失败"), nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("注入写入失败应 500，实得 %d %s", rec.Code, rec.Body.String())
	}
	mustCode(t, rec, "epocha-write-failed")
	if got := countMessages(t, a); got != 0 {
		t.Fatalf("写入失败却落了 %d 条", got)
	}
	// 再提交同一个 token：必须走完整验证（桩会说是重复），**不**被本地缓存放行。
	rec = post(t, a, bodyWith("tok-once", "重试同一 token"), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复 token 应 409，实得 %d %s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Fatalf("两次提交都应打到 /siteverify，实得 %d 次", calls)
	}
	if got := countMessages(t, a); got != 0 {
		t.Fatalf("重放竟然写入了 %d 条", got)
	}
}

func TestCSPModesAreHonest(t *testing.T) {
	for mode, want := range map[string]string{
		"blob":        "worker-src blob:;",
		"worker-file": "worker-src 'self';",
		"no-worker":   "worker-src 'none';",
	} {
		a, _ := newTestApp(t, nil)
		a.o.CSPMode = mode
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, want) {
			t.Errorf("csp=%s 应含 %q，实得 %q", mode, want, csp)
		}
		if strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "unsafe-inline") {
			t.Errorf("csp=%s 不该要求 unsafe-*：%q", mode, csp)
		}
		_ = a
	}
	// none 档用于"没有 CSP 时也要能跑"的对照。
	a, _ := newTestApp(t, nil)
	a.o.CSPMode = "none"
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Error("csp=none 不应发 CSP 头")
	}
}

func TestListenMustBeLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8091", "10.0.5.9:8091", ":8091", "192.168.1.5:8091", "example.com:8091", "127.0.0.1:0"} {
		if addr == "127.0.0.1:0" {
			continue // 端口 0 由 parseFlags 之外的 assertLoopback 拒绝，这里单独验证
		}
		if err := assertLoopback(addr); err == nil {
			t.Errorf("assertLoopback(%q) 竟然通过了", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8091", "localhost:8091", "[::1]:8091"} {
		if err := assertLoopback(addr); err != nil {
			t.Errorf("assertLoopback(%q) 应通过：%v", addr, err)
		}
	}
	if err := assertLoopback("127.0.0.1:0"); err == nil {
		t.Error("随机端口要显式给一个非 0 值：0 会让服务监听不可预知地址")
	}
}

func TestParseFlagsGuards(t *testing.T) {
	env := map[string]string{"EPOCHA_ADMIN_KEY": "ak_x"}
	get := func(k string) string { return env[k] }
	if _, err := parseFlags([]string{"--api", "not a url"}, get); err == nil {
		t.Error("非法 --api 应被拒")
	}
	if _, err := parseFlags([]string{"--api", "http://127.0.0.1:8080", "--site-key", "pk"}, get); err == nil {
		t.Error("只给 site-key 不给 secret 应被拒")
	}
	if _, err := parseFlags([]string{"--api", "http://127.0.0.1:8080"}, get); err == nil {
		t.Error("既没有现成站点又没有 --admin 应被拒")
	}
	if _, err := parseFlags([]string{"--api", "http://127.0.0.1:8080", "--csp", "no-csp-at-all"}, get); err == nil {
		t.Error("未知 CSP 档位应被拒")
	}
	o, err := parseFlags([]string{"--api", "http://127.0.0.1:8080/", "--site-key", "pk", "--site-secret", "sk"}, get)
	if err != nil {
		t.Fatal(err)
	}
	if o.APIBase != "http://127.0.0.1:8080" {
		t.Fatalf("尾斜杠应被剥掉：%q", o.APIBase)
	}
}

func TestWidgetSelfModeServesSameOriginScript(t *testing.T) {
	a, _ := newTestApp(t, nil)
	a.o.WidgetFrom = "self"
	a.widgetSrc = "/widget/v1.js"
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/widget/v1.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "EpoCHA") {
		t.Fatalf("self 档应同源提供 widget：%d", rec.Code)
	}
	page := httptest.NewRecorder()
	a.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(page.Body.String(), `<script src="/widget/v1.js"`) {
		t.Error("self 档页面应引用同源脚本")
	}
}

func TestListDoesNotLeakTokenPrefix(t *testing.T) {
	a, _ := newTestApp(t, nil)
	if rec := post(t, a, bodyWith("tok-leak-check-abcdef0123456789", "看看列表泄露什么"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("提交失败：%d %s", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/guestbook", nil))
	if bytes.Contains(rec.Body.Bytes(), []byte("tok-leak-check-abcdef0123456789")) {
		t.Fatal("列表接口回显了完整 token（它是一次性凭据，虽然已消费，仍不该外传）")
	}
	var got struct {
		Messages []struct {
			ID   int64  `json:"id"`
			Text string `json:"text"`
			At   string `json:"verified_at"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Text != "看看列表泄露什么" {
		t.Fatalf("列表内容不对：%s", rec.Body.String())
	}
	fmt.Fprint(io.Discard, got.Messages[0].At)
}

// --- 最小接入示例 fixture 与原生表单 POST ---
//
// 这一组测的是"文档示例的后端半边"：页面结构是否真的把容器放进 form、
// 脚本顺序是否确定、原生表单提交是否走**同一条**授权判定。
// 浏览器侧（FormData 里有没有 token、真提交后有没有写库）由 widget_browser.test.mjs 负责。

func getAs(t *testing.T, a *app, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = a.o.Listen
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

func postForm(t *testing.T, a *app, values url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/guestbook-form", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = a.o.Listen
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

func TestFixturePageStructure(t *testing.T) {
	a, _ := newTestApp(t, nil)
	rec := getAs(t, a, "/fixture")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /fixture = %d", rec.Code)
	}
	htmlBody := rec.Body.String()
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "worker-src blob:") {
		t.Fatalf("fixture 页面没带上本档 CSP：%q", rec.Header().Get("Content-Security-Policy"))
	}
	// 容器必须在 <form> **之内**：放外面时隐藏字段不属于目标表单，验证"成功"却什么都不提交。
	formOpen := strings.Index(htmlBody, `<form id="order"`)
	formClose := strings.Index(htmlBody, `</form>`)
	captchaAt := strings.Index(htmlBody, `<div id="captcha"></div>`)
	if formOpen < 0 || formClose < 0 || captchaAt < 0 {
		t.Fatalf("fixture 结构缺元素：form=%d /form=%d captcha=%d", formOpen, formClose, captchaAt)
	}
	if !(formOpen < captchaAt && captchaAt < formClose) {
		t.Fatal("widget 容器不在目标 form 内 —— 这正是要防的接入错误")
	}

	// 逐个解析 <script **开始标签**（不切片窗口，避免越界）：
	// 两个脚本都必须 defer，且 widget 在 init **之前** —— deferred 脚本按文档顺序执行，
	// 这是"EpoCHA 在 init 运行时已定义"的唯一确定保证。
	var opens []struct {
		pos  int
		head string
	}
	for i := 0; i+7 <= len(htmlBody); {
		j := strings.Index(htmlBody[i:], "<script")
		if j < 0 {
			break
		}
		abs := i + j
		k := strings.Index(htmlBody[abs:], ">")
		if k < 0 {
			t.Fatal("页面里有未闭合的 <script 开始标签")
		}
		opens = append(opens, struct {
			pos  int
			head string
		}{abs, htmlBody[abs : abs+k+1]})
		i = abs + k + 1
	}
	find := func(sub string) string {
		for _, o := range opens {
			if strings.Contains(o.head, sub) {
				return o.head
			}
		}
		return ""
	}
	widgetTag := find("/widget/v1.js")
	initTag := find("/fixture-init.js")
	if widgetTag == "" || initTag == "" {
		t.Fatalf("缺脚本标签：widget=%q init=%q（页面：%s）", widgetTag, initTag, htmlBody)
	}
	if !strings.HasSuffix(widgetTag, " defer>") {
		t.Fatalf("widget 脚本没有 defer，与后面的 init 之间是加载顺序竞争：%s", widgetTag)
	}
	if !strings.HasSuffix(initTag, " defer>") {
		t.Fatalf("初始化脚本没有 defer：%s", initTag)
	}
	if strings.Index(htmlBody, widgetTag) > strings.Index(htmlBody, initTag) {
		t.Fatal("widget 脚本排在 init 之后：init 会撞上 EpoCHA is not defined")
	}
	// 除配置数据块外不得有任何**无 src** 的 <script>：那就是内联脚本，严格 CSP 下不执行。
	for _, o := range opens {
		if strings.Contains(o.head, "src=") || strings.Contains(o.head, `type="application/json"`) {
			continue
		}
		t.Fatalf("fixture 页面出现内联 <script>%s —— 需要 unsafe-inline，文档示例不该这样", o.head)
	}
	if strings.Contains(htmlBody, "sk_EXAMPLE_TEST_SECRET_DO_NOT_LEAK") {
		t.Fatal("页面里出现了验证凭据")
	}
}

func TestFormPostWritesOnlyWhenVerifySucceeds(t *testing.T) {
	a, sb := newTestApp(t, nil)
	v := url.Values{"text": {"原生表单提交"}, "epocha_response": {"tok-form"}}
	if rec := postForm(t, a, v, nil); rec.Code != http.StatusCreated {
		t.Fatalf("表单带有效 token 应写入：%d %s", rec.Code, rec.Body.String())
	}
	if sb.count() != 1 || sb.lastURL != "/siteverify" {
		t.Fatalf("表单路径应恰好调用一次 /siteverify，实得 %d（%s）", sb.count(), sb.lastURL)
	}
	if got := countMessages(t, a); got != 1 {
		t.Fatalf("留言数 = %d，期望 1", got)
	}
}

func TestFormPostRejectsMissingToken(t *testing.T) {
	a, sb := newTestApp(t, nil)
	rec := postForm(t, a, url.Values{"text": {"没有凭据"}}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无 token 的表单提交应 403，实得 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), errMissingToken) {
		t.Fatalf("结果页缺机器码：%s", rec.Body.String())
	}
	if sb.count() != 0 {
		t.Fatal("没有 token 却仍然去调了验证服务")
	}
	if got := countMessages(t, a); got != 0 {
		t.Fatalf("无 token 竟然写入：%d", got)
	}
}

// 原生表单的字段是页面上任何 input 都能加的。若后端宽松读取，
// "浏览器指定验证目标"就会从这条路绕回来 —— 所以白名单必须是**拒收**而不是忽略。
func TestFormPostRejectsUnexpectedField(t *testing.T) {
	for _, extra := range []string{"target", "api_base", "secret", "response"} {
		a, sb := newTestApp(t, nil)
		v := url.Values{"text": {"带额外字段"}, "epocha_response": {"tok-x"}, extra: {"http://127.0.0.1:1/"}}
		rec := postForm(t, a, v, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("字段 %s 应被白名单拒收 400，实得 %d %s", extra, rec.Code, rec.Body.String())
		}
		if sb.count() != 0 {
			t.Fatalf("字段 %s：竟然还是调用了验证服务", extra)
		}
		if got := countMessages(t, a); got != 0 {
			t.Fatalf("字段 %s：竟然写入了留言", extra)
		}
	}
}

func TestFormPostReplayRejected(t *testing.T) {
	a, sb := newTestApp(t, func(call int, _ url.Values) (int, any) {
		if call == 1 {
			return http.StatusOK, map[string]any{"success": true}
		}
		return http.StatusOK, map[string]any{"success": false, "error-codes": []string{"timeout-or-duplicate"}}
	})
	v := url.Values{"text": {"重放"}, "epocha_response": {"tok-once"}}
	if rec := postForm(t, a, v, nil); rec.Code != http.StatusCreated {
		t.Fatalf("首次表单提交应成功：%d", rec.Code)
	}
	rec := postForm(t, a, v, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重放应 409，实得 %d %s", rec.Code, rec.Body.String())
	}
	if got := countMessages(t, a); got != 1 {
		t.Fatalf("重放后留言数 = %d，期望仍然 1", got)
	}
	if sb.count() != 2 {
		t.Fatalf("重放应在服务端被拒（两次 /siteverify），实得 %d", sb.count())
	}
}

// 结果页是浏览器导航目标：结论必须是**文本**，不能把任何输入当标记拼回去。
func TestFormResultPageDoesNotReflectInput(t *testing.T) {
	a, _ := newTestApp(t, nil)
	canary := "<img src=x onerror=alert(1)>"
	v := url.Values{"text": {canary}, "epocha_response": {"tok"}}
	rec := postForm(t, a, v, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("应写入成功：%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, canary) || strings.Contains(body, "onerror") {
		t.Fatalf("结果页把用户输入原样拼进了 HTML：%s", body)
	}
	if strings.Contains(body, "<script") {
		t.Fatalf("结果页不应含任何 script 标记：%s", body)
	}
}

// Firefox 在**原生 form POST** 上会发 `Origin: null`（不透明来源的表示），这不是"另一个站"。
// 把 "null" 当 URL 解析得到空 Host ⇒ 每一次真实表单提交都被误拒（本轮浏览器用例就是这么抓到的）。
// 正确语义是回落到 Sec-Fetch-Site——同样是浏览器强制设置、脚本改不了的头。
func TestOriginNullFallsBackToSecFetchSite(t *testing.T) {
	cases := []struct {
		name   string
		sfs    string
		wantOK bool
	}{
		{"same-origin 放行", "same-origin", true},
		{"same-site 放行", "same-site", true},
		{"cross-site 必须拒", "cross-site", false},
		{"cross-origin 必须拒", "cross-origin", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newTestApp(t, nil)
			v := url.Values{"text": {"表单"}, "epocha_response": {"tok-null"}}
			rec := postForm(t, a, v, map[string]string{"Origin": "null", "Sec-Fetch-Site": tc.sfs})
			if got := rec.Code == http.StatusCreated; got != tc.wantOK {
				t.Fatalf("Origin:null + Sec-Fetch-Site:%s ⇒ %d（body=%s），期望放行=%v",
					tc.sfs, rec.Code, firstLine(rec.Body.String()), tc.wantOK)
			}
			if !tc.wantOK && countMessages(t, a) != 0 {
				t.Fatal("跨站的 null Origin 竟然写入了留言")
			}
		})
	}
	// 真·跨站 Origin（不是 null）仍然直接拒，且不看 Sec-Fetch-Site。
	a, _ := newTestApp(t, nil)
	rec := postForm(t, a, url.Values{"text": {"跨站"}, "epocha_response": {"tok"}},
		map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("恶意 Origin 应 403，实得 %d", rec.Code)
	}
}
