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
	sentinelMasterB64  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	sentinelPepperB64  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
)

func writeSitesFile(t *testing.T, secret string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sites.json")
	body := `[{"name":"e2e","site_key":"sk-e2e","secret":"` + secret + `","pow_bits":12}]`
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
		"缺站点配置":    func(m map[string]string) { delete(m, "EPOCHA_SITES_FILE") },
		"缺数据库路径":   func(m map[string]string) { delete(m, "EPOCHA_DB") },
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
			for _, leak := range []string{sentinelSiteSecret, "AAAA", env["EPOCHA_MASTER_KEY"], env["EPOCHA_PEPPER"]} {
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
	go func() { done <- serve(ctx, cfg, ln, out) }()

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
	if v, _ := d.SchemaVersion(context.Background()); v != 1 {
		t.Fatalf("迁移版本 = %d", v)
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
	go func() { _ = serve(ctx, cfg, ln, io.Discard); done <- nil }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
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
	if err := serve(context.Background(), cfg, ln, io.Discard); err == nil {
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
