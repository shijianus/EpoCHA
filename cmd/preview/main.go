// Command preview 是阶段 3B 的**本地可操作预览**：一个浏览器页面 + 一个演示后端。
//
// 它存在的理由：curl 能证明协议形状，但证明不了"人能看见这张图并答对"、
// "浏览器自己算得动 PoW"、"图片字段在真实 DOM 里可用"。这些只有浏览器能回答。
//
// 边界（写在这里，也写在页面上）：
//   - 默认且**只**允许监听 loopback。这不是"默认值"而是硬性检查：
//     一个会替用户做题、还持有站点凭据的开发服务暴露到网卡上就是事故。
//   - 演示后端持有站点验证 secret，且**只**在进程内使用：不进 HTML、JS、页面配置或任何响应。
//   - 浏览器只能提交一个 response 串给 /demo/verify；**验证目标 URL 由服务端配置决定**，
//     页面无法指定让演示后端去访问别的地址（否则这就是一个 SSRF 门）。
//   - 没有"跳过验证""返回答案""改任意站点配置"的后门：这个进程唯一的额外能力是
//     用管理凭据创建/配置它自己的演示站点。
//
// 它不是正式 widget（阶段 4）：不承诺完整跨站点 SDK API，也未做完整无障碍合规验证。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/web/preview"
)

const (
	defaultListen     = "127.0.0.1:8090"
	defaultGenerator  = generator.TextImageID
	defaultDifficulty = 2
	defaultPowBits    = 12
	maxVerifyBody     = 8 << 10
	maxResponseToken  = 4096
	httpTimeout       = 10 * time.Second
)

var (
	errNotLoopback = errors.New("preview: 监听地址不是 loopback，拒绝启动")
	errBadConfig   = errors.New("preview: 参数不合法")
)

// options 是演示后端的全部配置。apiBase 与 secret 只存在于这个进程里。
type options struct {
	Listen     string
	APIBase    string
	AdminBase  string
	AdminKey   string
	SiteKey    string
	SiteSecret string
	Generator  string
	Difficulty int
	PowBits    int
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(context.Background(), os.Args[1:], os.Getenv, log); err != nil {
		log.Error("预览退出", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, log *slog.Logger) error {
	opt, err := parseFlags(args, getenv)
	if err != nil {
		return err
	}
	ln, err := listenLoopback(opt.Listen)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()

	srv, err := newServer(ctx, opt, ln, log)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("预览监听: %w", err)
		}
	}()
	log.Info("预览已启动（仅 loopback）", "url", "http://"+ln.Addr().String()+"/",
		"api", opt.APIBase, "site_key", srv.siteKey, "generator", opt.Generator,
		"difficulty", opt.Difficulty)
	fmt.Printf("epocha 预览：用浏览器打开 http://%s/\n", ln.Addr())
	fmt.Printf("站点 site_key=%s（公开标识）。演示 secret 只在本进程内，未打印、不会下发。\n", srv.siteKey)

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("关闭预览: %w", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

func parseFlags(args []string, getenv func(string) string) (*options, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		listen     = fs.String("listen", firstNonEmpty(getenv("EPOCHA_PREVIEW_LISTEN"), defaultListen), "监听地址（必须 loopback）")
		apiBase    = fs.String("api", getenv("EPOCHA_PREVIEW_API"), "epocha 公共服务基址，如 http://127.0.0.1:8080（必填）")
		adminBase  = fs.String("admin", getenv("EPOCHA_PREVIEW_ADMIN"), "管理 API 基址（自动建站时必填）")
		adminKey   = fs.String("admin-key", getenv("EPOCHA_PREVIEW_ADMIN_KEY"), "管理凭据（自动建站时必填）")
		siteKey    = fs.String("site-key", getenv("EPOCHA_PREVIEW_SITE_KEY"), "已存在的站点公开标识")
		siteSecret = fs.String("site-secret", getenv("EPOCHA_PREVIEW_SECRET"), "已存在站点的验证凭据（只在进程内使用）")
		gen        = fs.String("generator", firstNonEmpty(getenv("EPOCHA_PREVIEW_GENERATOR"), defaultGenerator), "演示题型")
		difficulty = fs.Int("difficulty", intEnv(getenv, "EPOCHA_PREVIEW_DIFFICULTY", defaultDifficulty), "难度档 1..5")
		powBits    = fs.Int("pow-bits", intEnv(getenv, "EPOCHA_PREVIEW_POW_BITS", defaultPowBits), "演示站点 PoW 难度（12..24）")
	)
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("%w: 命令行参数", errBadConfig)
	}
	opt := &options{
		Listen: strings.TrimSpace(*listen), APIBase: strings.TrimSpace(*apiBase),
		AdminBase: strings.TrimSpace(*adminBase), AdminKey: strings.TrimSpace(*adminKey),
		SiteKey: strings.TrimSpace(*siteKey), SiteSecret: strings.TrimSpace(*siteSecret),
		Generator: strings.TrimSpace(*gen), Difficulty: *difficulty, PowBits: *powBits,
	}
	if opt.APIBase == "" {
		return nil, fmt.Errorf("%w: 缺少 --api（epocha 服务基址）", errBadConfig)
	}
	if err := validBase(opt.APIBase); err != nil {
		return nil, err
	}
	if opt.AdminBase != "" {
		if err := validBase(opt.AdminBase); err != nil {
			return nil, fmt.Errorf("admin 基址: %w", err)
		}
	}
	// 要么自带站点与凭据，要么能自动建站：没有第三条路（不猜默认站点）。
	if opt.SiteKey == "" || opt.SiteSecret == "" {
		if opt.AdminBase == "" || opt.AdminKey == "" {
			return nil, fmt.Errorf("%w: 未给 --site-key/--site-secret 时必须有 --admin 与 --admin-key", errBadConfig)
		}
		if opt.SiteKey != "" || opt.SiteSecret != "" {
			return nil, fmt.Errorf("%w: site-key 与 site-secret 必须同时给出或同时省略", errBadConfig)
		}
	}
	if opt.Difficulty < 1 || opt.Difficulty > generator.MaxDifficulties {
		return nil, fmt.Errorf("%w: 难度档须在 1..%d", errBadConfig, generator.MaxDifficulties)
	}
	if opt.PowBits < 12 || opt.PowBits > 24 {
		return nil, fmt.Errorf("%w: pow_bits 须在 12..24（与站点表的 CHECK 一致）", errBadConfig)
	}
	switch opt.Generator {
	case generator.TextImageID, generator.ArithID:
	default:
		return nil, fmt.Errorf("%w: 预览不支持题型 %q", errBadConfig, opt.Generator)
	}
	return opt, nil
}

func validBase(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: 基址不可解析", errBadConfig)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: 基址必须是 http/https", errBadConfig)
	}
	if u.Host == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: 基址只能是 scheme://host:port（不得带路径、查询或用户信息）", errBadConfig)
	}
	return nil
}

// listenLoopback 只允许在 loopback 上监听。
//
// 这是**检查**而不是默认值：把 0.0.0.0 写成默认已经是事故，允许命令行填一个非回环地址
// 同样会把"持有凭据、替人做题的开发服务"放到网线上。
func listenLoopback(addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: 监听地址 %q 需要 host:port 形式", errBadConfig, addr)
	}
	if port == "0" || port == "" {
		return nil, fmt.Errorf("%w: 端口不能为 0 或空（预览页要给出确定的来源地址给站点白名单）", errBadConfig)
	}
	bind := host
	if bind == "" {
		bind = "127.0.0.1"
	}
	if !isLoopbackHost(bind) {
		return nil, fmt.Errorf("%w: 拒绝在 %s 上监听（只允许 127.0.0.0/8、::1、localhost）", errNotLoopback, bind)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, port))
	if err != nil {
		return nil, fmt.Errorf("preview: 监听失败: %w", err)
	}
	if a, ok := ln.Addr().(*net.TCPAddr); ok && !a.IP.IsLoopback() {
		_ = ln.Close()
		return nil, fmt.Errorf("%w: 实际绑定地址 %s 不是 loopback", errNotLoopback, ln.Addr())
	}
	return ln, nil
}

func isLoopbackHost(h string) bool {
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// server 是演示后端。secret 是它唯一持有的秘密，且只被 verify 一条路径读取。
type server struct {
	apiBase    string
	adminBase  string
	adminKey   string
	siteKey    string
	secret     string
	generator  string
	difficulty int
	powBits    int

	page []byte
	log  *slog.Logger
	hc   *http.Client
}

func newServer(ctx context.Context, opt *options, ln net.Listener, log *slog.Logger) (*server, error) {
	s := &server{
		apiBase: strings.TrimRight(opt.APIBase, "/"), adminBase: strings.TrimRight(opt.AdminBase, "/"),
		adminKey: opt.AdminKey, siteKey: opt.SiteKey, secret: opt.SiteSecret,
		generator: opt.Generator, difficulty: opt.Difficulty, powBits: opt.PowBits,
		log: log, hc: &http.Client{Timeout: httpTimeout},
	}
	if s.siteKey == "" {
		key, secret, err := s.createDemoSite(ctx, ln.Addr())
		if err != nil {
			return nil, err
		}
		s.siteKey, s.secret = key, secret
		log.Info("已创建演示站点", "site_key", key, "generator", s.generator, "difficulty", s.difficulty)
	} else {
		log.Info("使用给定站点", "site_key", s.siteKey)
	}
	page, err := renderPage(s.apiBase, s.siteKey)
	if err != nil {
		return nil, err
	}
	s.page = page
	return s, nil
}

// createDemoSite 通过管理 API 建一个只用于本地预览的站点：
// 模式 = pow_question + 选定题型 + 指定难度，来源白名单 = 本预览页自己的 loopback 来源。
//
// 这是预览**唯一**动到配置的地方，且只创建自己的站点、不改别人的站点。
func (s *server) createDemoSite(ctx context.Context, addr net.Addr) (string, string, error) {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", "", err
	}
	origins := []string{
		"http://" + net.JoinHostPort("127.0.0.1", port),
		"http://" + net.JoinHostPort("localhost", port),
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.To4() == nil && ip.IsLoopback() {
		origins = append(origins, "http://"+net.JoinHostPort("::1", port))
	}
	suffix, err := randHex(4)
	if err != nil {
		return "", "", err
	}
	body := map[string]any{
		"name":                 "epocha-preview-" + suffix,
		"verify_mode":          "pow_question",
		"generator_id":         s.generator,
		"difficulty":           s.difficulty,
		"pow_bits":             s.powBits,
		"origins":              origins,
		"allow_missing_origin": false,
	}
	var out struct {
		Site struct {
			ID      int64  `json:"id"`
			SiteKey string `json:"site_key"`
		} `json:"site"`
		Secret string `json:"secret"`
	}
	if err := s.adminPost(ctx, "/api/admin/sites", body, &out); err != nil {
		return "", "", err
	}
	if out.Site.SiteKey == "" || out.Secret == "" {
		return "", "", errors.New("preview: 管理 API 未返回 site_key/secret")
	}
	return out.Site.SiteKey, out.Secret, nil
}

func (s *server) adminPost(ctx context.Context, path string, body any, dst any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.adminBase+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.adminKey)
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("preview: 管理 API 不可达: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		// 不回显响应体：管理错误里可能带站点信息，而预览的日志会被贴进 issue
		return fmt.Errorf("preview: 管理 API 返回 %d", resp.StatusCode)
	}
	if dst != nil {
		if err := json.Unmarshal(data, dst); err != nil {
			return fmt.Errorf("preview: 管理 API 响应不可解析: %w", err)
		}
	}
	return nil
}

// renderPage 注入的是**公开**配置（api 基址、site_key、演示路径），不含 secret。
//
// 转义方式选的是"JSON 自身转义 < > &"（encoding/json 默认行为），**不是** HTML 实体：
// `<script type="application/json">` 是数据块，浏览器不会解码 HTML 实体，
// 用 HTMLEscapeString 会让页面里的 JSON.parse 直接抛异常（本轮实测踩过：
// 页面看起来"加载成功但什么都不做"）。这里额外挡一次 "</"，
// 保证注入内容不可能提前闭合脚本标签。
func renderPage(apiBase, siteKey string) ([]byte, error) {
	cfg := map[string]string{
		"api_base":    apiBase,
		"site_key":    siteKey,
		"verify_path": "/demo/verify",
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	inject := string(raw)
	if strings.Contains(inject, "</") || strings.Contains(inject, "<!--") {
		return nil, errors.New("preview: 注入内容含可闭合脚本标签的序列")
	}
	page := strings.ReplaceAll(string(preview.IndexHTML), preview.ConfigPlaceholder, inject)
	if strings.Contains(page, preview.ConfigPlaceholder) {
		return nil, errors.New("preview: 页面配置注入点未替换")
	}
	return []byte(page), nil
}

func (s *server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handlePage)
	mux.HandleFunc("POST /demo/verify", s.handleVerify)
	return s.headers(mux)
}

func (s *server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Pragma", "no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// 刻意**不设**任何 Access-Control-* 头：页面与演示后端同源，
		// 跨源脚本不该拿到这个持凭据端点的响应。
		next.ServeHTTP(w, r)
	})
}

func (s *server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src data:; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self' "+s.apiBase+"; base-uri 'none'; form-action 'none'")
	_, _ = w.Write(s.page)
}

// verifyRequest 只有一个字段：浏览器从 /api/redeem 拿到的 response。
// 没有 target/url/api 字段——**验证目标由服务端配置决定**（DisallowUnknownFields 是这条的强制执行点）。
type verifyRequest struct {
	Response string `json:"response"`
}

func (s *server) handleVerify(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxVerifyBody))
	if err != nil {
		writeVerifyErr(w, http.StatusRequestEntityTooLarge, "bad-request")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req verifyRequest
	if err := dec.Decode(&req); err != nil {
		writeVerifyErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeVerifyErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	if req.Response == "" || len(req.Response) > maxResponseToken {
		writeVerifyErr(w, http.StatusBadRequest, "missing-input-response")
		return
	}
	// 只接受一个字段、只发往固定目标：这里拼不出任何由浏览器决定的 URL。
	form := url.Values{"secret": {s.secret}, "response": {req.Response}}
	ctx, cancel := context.WithTimeout(r.Context(), httpTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBase+"/siteverify",
		strings.NewReader(form.Encode()))
	if err != nil {
		writeVerifyErr(w, http.StatusBadGateway, "internal-error")
		return
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.hc.Do(httpReq)
	if err != nil {
		s.log.Error("siteverify 调用失败", "err", "unreachable")
		writeVerifyErr(w, http.StatusBadGateway, "internal-error")
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxVerifyBody))
	if err != nil {
		writeVerifyErr(w, http.StatusBadGateway, "internal-error")
		return
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		writeVerifyErr(w, http.StatusBadGateway, "internal-error")
		return
	}
	// 白名单转发：只回 success / error-codes / hostname / challenge_ts 四个既定字段。
	// 上游响应里若多了别的键，一律不带进浏览器。
	out := map[string]any{"success": false}
	if v, ok := parsed["success"].(bool); ok {
		out["success"] = v
	}
	if v, ok := parsed["error-codes"]; ok {
		out["error-codes"] = v
	}
	if v, ok := parsed["hostname"].(string); ok && v != "" {
		out["hostname"] = v
	}
	if v, ok := parsed["challenge_ts"].(string); ok && v != "" {
		out["challenge_ts"] = v
	}
	s.log.Info("演示验证完成", "success", out["success"], "status", resp.StatusCode)
	writeJSONPreview(w, http.StatusOK, out)
}

func writeVerifyErr(w http.ResponseWriter, status int, code string) {
	writeJSONPreview(w, status, map[string]any{"success": false, "error-codes": []string{code}})
}

func writeJSONPreview(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		http.Error(w, `{"success":false,"error-codes":["internal-error"]}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func intEnv(getenv func(string) string, name string, def int) int {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("preview: 生成站点名: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
