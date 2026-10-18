package main

// 盲测记录服务的验收点：
//   1. 写入口是**白名单**的，且拒绝越界的耗时/档位/条数（它不该变成任意落盘口子）；
//   2. 导出里没有任何凭据、没有 site_key、没有用户敲进去的字符；
//   3. 页面在严格 CSP 下不需要内联脚本；
//   4. 只监听 loopback。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testSiteKey = "pk_blindtest_public_abcdef"

func newTestServer(t *testing.T) *server {
	t.Helper()
	out := t.TempDir() + "/records.jsonl"
	o := &options{
		Listen: "127.0.0.1:0", APIBase: "http://127.0.0.1:1", WidgetSrc: "http://127.0.0.1:1/widget/v1.js",
		Batch: 3, Out: out,
		Tiers: []tier{{Label: "低档（difficulty 1）", SiteKey: testSiteKey}},
	}
	return &server{o: o, sess: &session{id: "test-session"}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func recBody(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func goodRecord() map[string]any {
	return map[string]any{
		"tier": "低档（difficulty 1）", "item": 1, "outcome": "verified", "error_code": "",
		"answer_ms": 4200, "pow_ms": 38, "redeem_ms": 91, "refreshes": 0,
	}
}

func TestRecordWhitelistAndBounds(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantOK bool
	}{
		{"正常一条", recBody(t, goodRecord()), true},
		{"多带字段（answer 文本）", recBody(t, func() map[string]any {
			m := goodRecord()
			m["answer"] = "ABC234"
			return m
		}()), false},
		{"未知 outcome", recBody(t, func() map[string]any {
			m := goodRecord()
			m["outcome"] = "maybe"
			return m
		}()), false},
		{"未知档位", recBody(t, func() map[string]any {
			m := goodRecord()
			m["tier"] = "不存在"
			return m
		}()), false},
		{"item 越界", recBody(t, func() map[string]any {
			m := goodRecord()
			m["item"] = 99
			return m
		}()), false},
		{"耗时负数（answer）", recBody(t, func() map[string]any {
			m := goodRecord()
			m["answer_ms"] = -5
			return m
		}()), false},
		{"耗时荒谬（一小时）", recBody(t, func() map[string]any {
			m := goodRecord()
			m["answer_ms"] = 99999999
			return m
		}()), false},
		{"未观察到的 PoW 用 -1 表示", recBody(t, func() map[string]any {
			m := goodRecord()
			m["pow_ms"] = -1
			return m
		}()), true},
		{"JSON 后面还有内容", recBody(t, goodRecord()) + `{"item":2}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			req := httptest.NewRequest(http.MethodPost, "/record", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if gotOK := rec.Code == http.StatusOK; gotOK != tc.wantOK {
				t.Fatalf("want ok=%v, got status=%d body=%s", tc.wantOK, rec.Code, rec.Body.String())
			}
			s.sess.mu.Lock()
			n := len(s.sess.records)
			s.sess.mu.Unlock()
			if want := 0; tc.wantOK {
				if n != 1 {
					t.Fatalf("应记录 1 条，实得 %d", n)
				}
			} else if n != want {
				t.Fatalf("被拒的请求不应该落账：%d", n)
			}
		})
	}
}

// 导出面是这次人工数据唯一离开浏览器的通道：凭据、site_key、输入字符都不该出现在里面。
func TestExportCarriesNoCredentialsOrInput(t *testing.T) {
	s := newTestServer(t)
	for _, m := range []map[string]any{goodRecord(), func() map[string]any {
		r := goodRecord()
		r["item"] = 2
		r["outcome"] = "wrong"
		r["error_code"] = "invalid-challenge"
		return r
	}()} {
		req := httptest.NewRequest(http.MethodPost, "/record", strings.NewReader(recBody(t, m)))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("写入失败：%d %s", rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{"/export.json", "/export.csv"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		body := rec.Body.String()
		for _, banned := range []string{"sk_", "secret", testSiteKey, "admin", "pepper", "ABC234"} {
			if strings.Contains(strings.ToLower(body), strings.ToLower(banned)) {
				t.Fatalf("%s 导出里出现了 %q：%s", path, banned, body)
			}
		}
		if !strings.Contains(body, "answer_ms") {
			t.Fatalf("%s 导出缺少耗时字段：%s", path, body)
		}
		if !strings.Contains(body, "test-session") && path == "/export.json" {
			t.Fatalf("JSON 导出应带会话号：%s", body)
		}
	}
}

func TestPageIsCSPCleanAndHasNoSecrets(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "worker-src blob:", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP 缺 %q：%s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("CSP 不该需要例外：%s", csp)
	}
	body := rec.Body.String()
	tags := scriptTags(t, body)
	find := func(sub string) string {
		for _, o := range tags {
			if strings.Contains(o, sub) {
				return o
			}
		}
		return ""
	}
	// 除配置数据块外不允许无 src 的 script
	for _, head := range tags {
		if !strings.Contains(head, "src=") && !strings.Contains(head, `type="application/json"`) {
			t.Fatalf("页面里有内联 script%s", head)
		}
	}
	if strings.Contains(body, "sk_") {
		t.Fatal("页面里出现了验证凭据")
	}
	// 站点用的是公开 site_key：页面必须带它（widget 领题要用）
	if !strings.Contains(body, testSiteKey) {
		t.Fatal("页面没带 site_key，widget 无法领题")
	}
	// widget 脚本必须被引入，且**排在 /app.js 之前**：漏掉这个标签的失败方式很隐蔽——
	// 页面照常显示、按钮照常能点，但组件永远不出现（真实浏览器里实测到"进度停在 0/20"）。
	widgetTag := find("/widget/v1.js")
	if widgetTag == "" {
		t.Fatalf("页面没有引入 widget 脚本，app.js 里的 EpoCHA 必然未定义：%s", body)
	}
	if !strings.HasSuffix(widgetTag, " defer>") {
		t.Fatalf("widget 脚本缺 defer，与 app.js 之间是加载顺序竞争：%s", widgetTag)
	}
	appTag := find("/app.js")
	if appTag == "" || !strings.HasSuffix(appTag, " defer>") {
		t.Fatalf("/app.js 必须是 deferred 同源脚本：%q", appTag)
	}
	if strings.Index(body, "/widget/v1.js") > strings.Index(body, `src="/app.js"`) {
		t.Fatal("widget 脚本排在 app.js 之后：app.js 会撞上 EpoCHA is not defined")
	}
}

func TestStaticFilesAreServed(t *testing.T) {
	s := newTestServer(t)
	for _, p := range []string{"/app.js", "/app.css"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s = %d（nosniff=%q）", p, rec.Code, rec.Header().Get("X-Content-Type-Options"))
		}
	}
	// 页面里的 JS 不许用 innerHTML 拼服务端字符串
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if strings.Contains(rec.Body.String(), "innerHTML") {
		t.Fatal("盲测页面用了 innerHTML")
	}
}

func TestListenMustBeLoopback(t *testing.T) {
	for _, bad := range []string{"0.0.0.0:8491", "example.com:443"} {
		_, err := listenLoopback(bad)
		if err == nil {
			t.Fatalf("%s 竟然被允许监听", bad)
		}
	}
	for _, ok := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		ln, err := listenLoopback(ok)
		if err != nil {
			t.Fatalf("%s 应允许：%v", ok, err)
		}
		_ = ln.Close()
	}
}

func TestFlagGuards(t *testing.T) {
	env := func(string) string { return "" }
	if _, err := parseFlags([]string{"--api", "http://127.0.0.1:1", "--tiers", ""}, env); err == nil {
		t.Fatal("没给档位应当失败")
	}
	if _, err := parseFlags([]string{"--api", "http://127.0.0.1:1", "--tiers", "只有关键词"}, env); err == nil {
		t.Fatal("label=site_key 形式错了应当失败")
	}
	// 真实启动盲测时抓到的坑：标签里本来就有 `=`（"难度档 3（difficulty=3）"）。
	// 按第一个 `=` 切会把 `1）` 留在 site_key 前面，组件在浏览器里报 siteKey 形态不合法，
	// 看起来像"工具坏了"而不是"启动参数解析错了"。
	o, err := parseFlags([]string{"--api", "http://127.0.0.1:1",
		"--tiers", "难度档 3（difficulty=3）=pk_blind_d3_deadbeef"}, env)
	if err != nil {
		t.Fatalf("含 = 的标签应当能解析：%v", err)
	}
	if len(o.Tiers) != 1 || o.Tiers[0].SiteKey != "pk_blind_d3_deadbeef" {
		t.Fatalf("site_key 解析不对：%+v", o.Tiers)
	}
	if o.Tiers[0].Label != "难度档 3（difficulty=3）" {
		t.Fatalf("label 解析不对：%q", o.Tiers[0].Label)
	}
	// site_key 形状在启动期就要拦住：放过坏值等于让真人在浏览器里看组件报错
	if _, err := parseFlags([]string{"--api", "http://127.0.0.1:1",
		"--tiers", "d1=1）=pk_bad key"}, env); err == nil {
		t.Fatal("site_key 形态不合法应当失败")
	}
	if _, err := parseFlags([]string{"--api", "http://127.0.0.1:1", "--tiers", "d1=pk_a", "--batch", "500"}, env); err == nil {
		t.Fatal("batch 越界应当失败")
	}
	// widget 脚本的来源必须是同一个 api：否则等于让第三方站点决定页面执行什么
	if _, err := parseFlags([]string{"--api", "http://127.0.0.1:1", "--widget-src", "http://evil.example/v1.js", "--tiers", "d1=pk_a"}, env); err == nil {
		t.Fatal("跨站 widget-src 应当被拒")
	}
}

// 记录数有硬上限：这个服务只监听 loopback，但"只监听 loopback"不是不写满磁盘的理由。
func TestRecordCountIsBounded(t *testing.T) {
	s := newTestServer(t)
	s.o.Batch = maxRecords + 10
	s.sess.records = make([]record, maxRecords)
	m := goodRecord()
	m["item"] = maxRecords
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/record",
		strings.NewReader(recBody(t, m))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超过上限应 413，实得 %d %s", rec.Code, rec.Body.String())
	}
}

// 进程退出时把内存里的记录补写一次盘：真人做完 20 题后 Ctrl-C 不该丢数据。
func TestRunFlushesOnShutdown(t *testing.T) {
	dir := t.TempDir()
	out := dir + "/records.jsonl"
	ln, err := listenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--listen", addr, "--api", "http://127.0.0.1:1",
			"--tiers", "低档（difficulty 1）=" + testSiteKey, "--out", out}, func(string) string { return "" },
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	if _, err := waitReady("http://" + addr); err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(recBody(t, goodRecord()))
	res, err := http.Post("http://"+addr+"/record", "application/json", body)
	if err != nil {
		t.Fatalf("POST /record: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("record = %d", res.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("退出时应写出记录文件: %v", err)
	}
	if !strings.Contains(string(raw), `"outcome":"verified"`) {
		t.Fatalf("文件内容不含这条记录：%s", raw)
	}
}

func waitReady(base string) (bool, error) {
	for i := 0; i < 100; i++ {
		res, err := http.Get(base + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return true, nil
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	return false, context.DeadlineExceeded
}

// scriptTags 解析出页面里所有 <script **开始标签**（含尖括号），用于断言引入顺序与是否内联。
// 不用正则也不用切片窗口：窗口法在标签靠近文档末尾时会越界（这个坑踩过一次）。
func scriptTags(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for i := 0; i+7 <= len(body); {
		j := strings.Index(body[i:], "<script")
		if j < 0 {
			break
		}
		abs := i + j
		k := strings.Index(body[abs:], ">")
		if k < 0 {
			t.Fatal("页面里有未闭合的 <script 开始标签")
		}
		out = append(out, body[abs:abs+k+1])
		i = abs + k + 1
	}
	return out
}
