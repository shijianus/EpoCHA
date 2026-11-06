package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/store"
)

const (
	sentinelSiteSecret = "SENTINEL-SITE-SECRET-4b7c9d1e2f"
	sentinelAdminKey   = "ak_SENTINEL-ADMIN-9d3f7a1c5e2b8046"
	sentinelMasterB64  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	sentinelPepperB64  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
)

func writeSitesFile(t *testing.T, secret string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sites.json")
	// allow_missing_origin 显式打开：这些测试与 devclient 一样走"无 Origin 的直接 API 调用"，
	// 而阶段 2 的默认策略是拒绝——放行必须是配置里的一个明确决定。
	body := `[{"name":"e2e","site_key":"sk-e2e","secret":"` + secret +
		`","pow_bits":12,"allow_missing_origin":true}]`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func testEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"EPOCHA_MASTER_KEY": sentinelMasterB64,
		"EPOCHA_PEPPER":     sentinelPepperB64,
		"EPOCHA_DB":         filepath.Join(t.TempDir(), "epocha.db"),
		"EPOCHA_SITES_FILE": writeSitesFile(t, sentinelSiteSecret),
		"EPOCHA_ADMIN_KEY":  sentinelAdminKey,
		"EPOCHA_LISTEN":     "127.0.0.1:0",
	}
}

func getenvOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestRunVersionNeedsNoKeys(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"--version"}, getenvOf(map[string]string{}), &out); err != nil {
		t.Fatalf("--version 不应要求密钥：%v", err)
	}
	if !strings.HasPrefix(out.String(), "epocha ") {
		t.Fatalf("版本输出不符：%q", out.String())
	}
}

// 缺密钥必须拒绝启动，且错误信息不回显任何密钥值（DESIGN §6、红线 12）。
func TestRunRefusesWithoutSecrets(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"缺主密钥":     func(m map[string]string) { delete(m, "EPOCHA_MASTER_KEY") },
		"缺 pepper": func(m map[string]string) { delete(m, "EPOCHA_PEPPER") },
		"密钥非法":     func(m map[string]string) { m["EPOCHA_MASTER_KEY"] = "%%%not-base64" },
		"密钥过短":     func(m map[string]string) { m["EPOCHA_PEPPER"] = base64.StdEncoding.EncodeToString([]byte("tiny")) },
		// 阶段 2 起站点配置是**可选**的一次性导入入口，所以"没有配置文件"必须能启动；
		// 这里改成"显式指向一个读不到的文件"，那才是真配置错误。
		"站点文件不可读": func(m map[string]string) { m["EPOCHA_SITES_FILE"] = "/nonexistent/sites.json" },
		"缺数据库路径":  func(m map[string]string) { delete(m, "EPOCHA_DB") },
	} {
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			mutate(env)
			var out bytes.Buffer
			err := run(nil, getenvOf(env), &out)
			if err == nil {
				t.Fatal("应拒绝启动")
			}
			if !errors.Is(err, config.ErrInvalid) && !strings.Contains(err.Error(), "config") {
				t.Logf("错误类型未归到 config.ErrInvalid：%v", err)
			}
			joined := err.Error() + out.String()
			for _, leak := range []string{sentinelSiteSecret, sentinelAdminKey, "AAAA", env["EPOCHA_MASTER_KEY"], env["EPOCHA_PEPPER"]} {
				if leak != "" && strings.Contains(joined, leak) {
					t.Fatalf("启动失败信息泄露密钥材料 %q：%s", leak, joined)
				}
			}
		})
	}
}

// 正常启动 → 健康检查可用 → 取消上下文后优雅退出；启动输出不含站点 secret。
func TestServeHealthzAndGracefulShutdown(t *testing.T) {
	env := testEnv(t)
	dbPath := env["EPOCHA_DB"]
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(nil, getenvOf(env))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	out := &syncWriter{}
	aln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.AdminListen = aln.Addr().String()
	go func() { done <- serve(ctx, cfg, ln, aln, out) }()

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}
	var res *http.Response
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res, err = client.Get(base + "/healthz")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("健康检查不可达：%v（启动输出：%s）", err, out.String())
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("健康检查响应不符：%d %s", res.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅关闭返回错误：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("优雅关闭超时")
	}

	startup := out.String()
	if strings.Contains(startup, sentinelSiteSecret) {
		t.Fatalf("启动输出泄露站点 secret：%s", startup)
	}
	if !strings.Contains(startup, "sk-e2e") {
		t.Errorf("启动输出应包含公开 site_key：%s", startup)
	}
	// 库文件确实建立
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("数据库未创建：%v", err)
	}
	d, err := store.Open(context.Background(), dbPath, bytes.Repeat([]byte("p"), 32))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if v, _ := d.SchemaVersion(context.Background()); v != 6 {
		t.Fatalf("迁移版本 = %d（阶段 3B 应为 4）", v)
	}
	if _, err := d.SiteByKey(context.Background(), "sk-e2e"); err != nil {
		t.Fatalf("站点未落库：%v", err)
	}
}

// 优雅关闭必须真的关掉监听端口（回归"进程退了但端口还占着"这类问题）。
func TestServeStopsListening(t *testing.T) {
	env := testEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	cfg, err := config.Load(nil, getenvOf(env))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = addr
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// serve 的错误必须传到断言里：早退（比如库打不开）同样会让端口没关，那是另一个问题，
	// 不能伪装成"关闭后仍可连接"这种含糊的失败。
	go func() { done <- serve(ctx, cfg, ln, nil, io.Discard) }()
	if err := waitForHealthz("http://" + addr); err != nil {
		t.Fatalf("服务未就绪：%v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve 返回错误：%v", err)
	}
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err == nil {
		c.Close()
		t.Fatalf("关闭后端口 %s 仍可连接", addr)
	}
}

func TestServeFailsOnBadDBPath(t *testing.T) {
	env := testEnv(t)
	env["EPOCHA_DB"] = filepath.Join(t.TempDir(), "no-such-dir", "x.db")
	cfg, err := config.Load(nil, getenvOf(env))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(context.Background(), cfg, ln, nil, io.Discard); err == nil {
		t.Fatal("数据库路径不可写时应返回错误")
	}
}

// syncWriter 让测试可以安全地在服务运行期间读取被写的字节：
// 生产路径只有 slog 单一写入器，但测试里读日志与服务器写日志是分开的 goroutine。
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestPublicListenerHasNoAdminRoutes 是阶段 2 最重要的一条结构性保证：
// 管理路由只存在于 admin 监听器上，公共端口上根本没有这条路——
// 不是"有路由但鉴权失败"，而是"路由不存在"。
func TestPublicListenerHasNoAdminRoutes(t *testing.T) {
	env := testEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	aln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(nil, getenvOf(env))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen, cfg.AdminListen = ln.Addr().String(), aln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, ln, aln, io.Discard) }()
	if err := waitForHealthz("http://" + cfg.Listen); err != nil {
		t.Fatalf("公共面未就绪：%v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	// 公共端口：GET/POST 管理路径都应是 404/405，而不是 401（401 意味着路由挂上了、只差鉴权）
	for _, path := range []string{"/api/admin/sites", "/api/admin/audit", "/api/admin/whoami"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req, _ := http.NewRequest(method, "http://"+cfg.Listen+path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusOK {
				t.Fatalf("公共监听器竟暴露了管理路由：%s %s -> %d %s", method, path, resp.StatusCode, body)
			}
		}
	}
	// admin 端口：同一路径存在但要求鉴权（引导凭据没带上 → 401）
	resp, err := client.Get("http://" + cfg.AdminListen + "/api/admin/sites")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("管理监听器未鉴权请求应 401，got %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), sentinelAdminKey) {
		t.Fatalf("401 响应泄露管理凭据：%s", body)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("优雅关闭：%v", err)
	}
}

// TestNoAdminKeyMeansAdminRefuses 覆盖"缺少管理凭据不能意外开放管理功能"：
// 库里没有管理密钥记录时，带任何 Bearer 都被拒。
func TestNoAdminKeyMeansAdminRefuses(t *testing.T) {
	env := testEnv(t)
	delete(env, "EPOCHA_ADMIN_KEY")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	aln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(nil, getenvOf(env))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen, cfg.AdminListen = ln.Addr().String(), aln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	out := &syncWriter{}
	go func() { done <- serve(ctx, cfg, ln, aln, out) }()
	if err := waitForHealthz("http://" + cfg.Listen); err != nil {
		t.Fatalf("公共面未就绪：%v", err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, "http://"+cfg.AdminListen+"/api/admin/sites",
		strings.NewReader(`{"name":"rogue","site_key":"pk_rogue"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("没有登记管理凭据时应一律 401，got %d", resp.StatusCode)
	}
	if !strings.Contains(out.String(), "库中没有可用管理凭据") {
		t.Errorf("启动日志应提示管理面处于关闭状态：%s", out.String())
	}
	cancel()
	<-done
}

func waitForHealthz(base string) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get(base + "/healthz")
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("healthz 未在 3s 内可用")
}

// TestSiteFileDoesNotOverwriteDatabaseOnRestart 覆盖阶段 2 的权威数据源语义：
// 配置文件只在"库里还没有这个 site_key"时导入；重启绝不把旧配置覆盖回库，
// 因此"已停用站点"不会因为重启而复活。
func TestSiteFileDoesNotOverwriteDatabaseOnRestart(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Load(nil, getenvOf(env))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := cfg.DBPath
	ctx := context.Background()

	// 第一轮：导入站点
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	aln1, _ := net.Listen("tcp", "127.0.0.1:0")
	cfg.Listen, cfg.AdminListen = ln1.Addr().String(), aln1.Addr().String()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serve(runCtx, cfg, ln1, aln1, io.Discard) }()
	if err := waitForHealthz("http://" + cfg.Listen); err != nil {
		t.Fatal(err)
	}
	d, err := store.Open(ctx, dbPath, cfg.Pepper)
	if err != nil {
		t.Fatal(err)
	}
	site, err := d.SiteByKey(ctx, "sk-e2e")
	if err != nil {
		t.Fatalf("站点未导入：%v", err)
	}
	if !site.Enabled {
		t.Fatal("首导入站点应为启用")
	}
	// 运维停用该站点
	if _, err := d.SetSiteEnabled(ctx, site.ID, false); err != nil {
		t.Fatal(err)
	}
	versionAfterDisable := site.Version
	d.Close()
	cancel()
	<-done

	// 第二轮：用同一份配置文件重启
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	aln2, _ := net.Listen("tcp", "127.0.0.1:0")
	cfg2, err := config.Load(nil, getenvOf(env))
	if err != nil {
		t.Fatal(err)
	}
	cfg2.DBPath, cfg2.Listen, cfg2.AdminListen = dbPath, ln2.Addr().String(), aln2.Addr().String()
	runCtx2, cancel2 := context.WithCancel(ctx)
	done2 := make(chan error, 1)
	go func() { done2 <- serve(runCtx2, cfg2, ln2, aln2, io.Discard) }()
	if err := waitForHealthz("http://" + cfg2.Listen); err != nil {
		t.Fatal(err)
	}
	d2, err := store.Open(ctx, dbPath, cfg2.Pepper)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	after, err := d2.SiteByKey(ctx, "sk-e2e")
	if err != nil {
		t.Fatal(err)
	}
	if after.Enabled {
		t.Fatal("重启把已停用站点重新启用了：配置文件覆盖了权威数据源")
	}
	if after.Version <= versionAfterDisable {
		t.Fatalf("重启改动了站点版本（%d → %d）", versionAfterDisable, after.Version)
	}
	cancel2()
	<-done2
}

// TestAdminUISitsOnlyOnAdminListener 是管理控制台的**网络边界**验收：
// 页面壳只在 admin 监听器上有路由，公共监听器上连路由都没有（404 而不是 401——
// 401 意味着"挂上了、只差鉴权"，那是另一种暴露）。
// 这一条是"不因浏览器方便就把管理面挪到公共端口"的结构化防线。
func TestAdminUISitsOnlyOnAdminListener(t *testing.T) {
	env := testEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	aln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(nil, getenvOf(env))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen, cfg.AdminListen = ln.Addr().String(), aln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, ln, aln, io.Discard) }()
	if err := waitForHealthz("http://" + cfg.Listen); err != nil {
		t.Fatalf("公共面未就绪：%v", err)
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("优雅关闭：%v", err)
		}
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	publicPaths := []string{
		"/admin", "/admin/", "/admin/app.js", "/admin/app.css", "/admin/app.js.map",
		"/api/admin/whoami", "/api/admin/sites",
	}
	for _, path := range publicPaths {
		req, _ := http.NewRequest(http.MethodGet, "http://"+cfg.Listen+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("公共面 GET %s: %v", path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("公共监听器暴露了 %s：%s", path, strings.TrimRight(string(raw), "\n"))
		}
		if strings.Contains(string(raw), "<!doctype html") || strings.Contains(string(raw), "管理控制台") {
			t.Fatalf("公共监听器回了管理页面内容：%s", path)
		}
	}

	adminUI := []struct{ path, mime string }{
		{"/admin", "text/html; charset=utf-8"},
		{"/admin/app.js", "text/javascript; charset=utf-8"},
		{"/admin/app.css", "text/css; charset=utf-8"},
	}
	for _, c := range adminUI {
		resp, err := client.Get("http://" + cfg.AdminListen + c.path)
		if err != nil {
			t.Fatalf("管理面 GET %s: %v", c.path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("管理监听器 GET %s = %d", c.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != c.mime {
			t.Errorf("%s Content-Type=%q want %q", c.path, ct, c.mime)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control=%q", c.path, cc)
		}
		body := string(raw)
		// 页面壳是静态的：它**不该**含有本次运行的任何真实凭据，即便引导密钥与站点 secret 都已装载。
		if strings.Contains(body, sentinelAdminKey) {
			t.Errorf("%s 泄露了引导管理凭据", c.path)
		}
		if strings.Contains(body, sentinelSiteSecret) {
			t.Errorf("%s 泄露了站点 secret", c.path)
		}
	}

	// 页面挂载不改变 API 的鉴权要求：管理端口上无凭据仍是 401，公共端口上仍是 404。
	resp, err := client.Get("http://" + cfg.AdminListen + "/api/admin/whoami")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("管理端口未鉴权 /whoami 应 401，got %d", resp.StatusCode)
	}
}
