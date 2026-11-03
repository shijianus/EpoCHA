// Command exampleapp 是阶段 4A 的**开发专用**业务接入示例（DESIGN §17 阶段 4A、docs/WIDGET.md）。
//
// 它存在的目的只有一个：把"widget 拿到 token → 业务后端调 /siteverify → 只有服务端
// 明确 success=true 才执行受保护操作"这条链路跑成真的，并且能证明反例不成立
// （无 token / 错 token / 重复 token / 验证服务超时或 5xx 都不写入）。
//
// 三条边界必须说清楚：
//   - 它是**示例**，不是产品：没有账户系统、没有会话 cookie、没有队列，默认只监听 loopback。
//   - cmd/server **不链接**它（stage4a.sh 用 go list -deps 钉住），它是独立 main。
//   - 站点的验证凭据只在**本进程内存**里，不进 HTML、JS、页面配置或任何响应；
//     浏览器也**不能**指定验证目标——目标 URL 在启动时固定，请求体里出现 target/url 一律 400。
//
// CAPTCHA 通过 ≠ 业务授权：这里刻意不做登录、不做幂等键，文档里写清"这三件事 widget 不替代"。
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/shijianus/EpoCHA/web/widget"
)

const (
	maxBody         = 8 << 10
	maxTextLen      = 500
	maxTokenLen     = 4096
	siteKeyMax      = 512
	loopbackHint    = "示例应用默认只监听 loopback；要换地址请显式给 --listen，并自行加上来源与 TLS"
	errBadJSON      = "请求体不是合法 JSON 或带了未知字段"
	errMissingToken = "epocha-missing-response"
	errInvalidTok   = "epocha-invalid-response"
	errDuplicateTok = "epocha-duplicate-response"
	errVerifyDown   = "epocha-verify-unavailable"
	errOrigin       = "epocha-origin-rejected"
	errTooLarge     = "epocha-payload-too-large"
)

// options 是启动参数。验证目标（apiBase）在这里定死，之后不受任何请求影响。
type options struct {
	Listen      string
	APIBase     string
	AdminBase   string
	AdminKey    string
	SiteKey     string
	SiteSecret  string
	SiteName    string
	Generator   string
	Difficulty  int
	PowBits     int
	GuestbookDB string
	WidgetFrom  string // "api"（跨域加载，真实嵌入形状）或 "self"（同源，配 CSP worker-file 用）
	CSPMode     string // blob | worker-file | no-worker | none
	VerifyTO    time.Duration
}

func parseFlags(argv []string, getenv func(string) string) (*options, error) {
	fs := flag.NewFlagSet("exampleapp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o options
	fs.StringVar(&o.Listen, "listen", "127.0.0.1:8091", "监听地址（默认 loopback）")
	fs.StringVar(&o.APIBase, "api", envOr(getenv, "EPOCHA_API_BASE", "http://127.0.0.1:8080"), "epocha 服务基址（业务后端调 /siteverify 的目标）")
	fs.StringVar(&o.AdminBase, "admin", envOr(getenv, "EPOCHA_ADMIN_BASE", ""), "管理面基址（只在缺 site-key/secret 时用于建演示站点）")
	fs.StringVar(&o.AdminKey, "admin-key", getenv("EPOCHA_ADMIN_KEY"), "管理密钥（与 Bearer 值逐字节相同）")
	fs.StringVar(&o.SiteKey, "site-key", getenv("EPOCHA_PREVIEW_SITE_KEY"), "已存在站点的 site_key")
	fs.StringVar(&o.SiteSecret, "site-secret", getenv("EPOCHA_PREVIEW_SECRET"), "已存在站点的验证凭据")
	fs.StringVar(&o.SiteName, "site-name", "exampleapp-demo", "自动建站时的站点名")
	fs.StringVar(&o.Generator, "generator", "text-image-v1", "演示站点题型")
	fs.IntVar(&o.Difficulty, "difficulty", 2, "演示站点难度档（1–5）")
	fs.IntVar(&o.PowBits, "pow-bits", 12, "演示站点 PoW 位数（12–24）")
	fs.StringVar(&o.GuestbookDB, "db", "", "留言库路径（留空则用临时目录，进程退出删除）")
	fs.StringVar(&o.WidgetFrom, "widget-origin", "api", "widget 脚本来源：api=跨域从 epocha 取（真实嵌入形状）；self=同源副本")
	fs.StringVar(&o.CSPMode, "csp", "blob", "页面 CSP 档位：blob | worker-file | no-worker | none")
	var toSec int
	fs.IntVar(&toSec, "verify-timeout-sec", 8, "业务后端调 /siteverify 的超时")
	if err := fs.Parse(argv); err != nil {
		return nil, err
	}
	o.VerifyTO = time.Duration(toSec) * time.Second

	o.APIBase = strings.TrimRight(o.APIBase, "/")
	if o.APIBase == "" {
		return nil, errors.New("exampleapp: 必须给 --api")
	}
	u, err := url.Parse(o.APIBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("exampleapp: --api 不是合法 URL：%q", o.APIBase)
	}
	if (o.SiteKey == "") != (o.SiteSecret == "") {
		return nil, errors.New("exampleapp: --site-key 与 --site-secret 必须同时给或同时省略")
	}
	if o.SiteKey == "" && o.AdminBase == "" {
		return nil, errors.New("exampleapp: 没有现成站点时必须给 --admin 与 --admin-key（演示站点由它自己建）")
	}
	switch o.WidgetFrom {
	case "api", "self":
	default:
		return nil, fmt.Errorf("exampleapp: --widget-origin 只接受 api|self，实得 %q", o.WidgetFrom)
	}
	switch o.CSPMode {
	case "blob", "worker-file", "no-worker", "none":
	default:
		return nil, fmt.Errorf("exampleapp: --csp 只接受 blob|worker-file|no-worker|none，实得 %q", o.CSPMode)
	}
	return &o, nil
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// app 是示例应用的状态：secret 与验证目标都在**服务端**，任何请求都改不了它们。
type app struct {
	o        *options
	secret   string
	siteKey  string
	gb       *sql.DB
	log      *slog.Logger
	verifyTO time.Duration
	// writeFailOnce 是给测试用的**故障注入**，不是后门：它只影响示例应用自己的留言写入，
	// 用来证明"token 已消费但业务写入失败"这条路径不缓存成功、提示重新验证。
	mu            sync.Mutex
	writeFailOnce bool
	httpClient    *http.Client
	widgetSrc     string
}

func newApp(ctx context.Context, o *options, log *slog.Logger) (*app, error) {
	a := &app{o: o, log: log, verifyTO: o.VerifyTO, httpClient: &http.Client{Timeout: o.VerifyTO}}
	if o.SiteKey != "" {
		a.siteKey, a.secret = o.SiteKey, o.SiteSecret
	} else {
		key, secret, err := a.createDemoSite(ctx)
		if err != nil {
			return nil, err
		}
		a.siteKey, a.secret = key, secret
	}
	switch o.WidgetFrom {
	case "self":
		a.widgetSrc = "/widget/v1.js"
	default:
		a.widgetSrc = o.APIBase + widget.URLJS
	}

	dbPath := o.GuestbookDB
	if dbPath == "" {
		dir, err := os.MkdirTemp("", "epocha-exampleapp-")
		if err != nil {
			return nil, err
		}
		dbPath = dir + "/guestbook.db"
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // 与主服务同理：写者唯一，避免 SQLITE_BUSY 抖动。
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS guestbook (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		text TEXT NOT NULL,
		verified_at TEXT NOT NULL,
		token_prefix TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("exampleapp: 建留言表: %w", err)
	}
	a.gb = db
	return a, nil
}

// createDemoSite 用管理 API 建**自己的**演示站点：pow_question + 指定题型 + 来源=本应用地址。
// 这是开发期方便，不是生产路径：生产站点的凭据由运维下发。
func (a *app) createDemoSite(ctx context.Context) (string, string, error) {
	base := strings.TrimRight(a.o.AdminBase, "/")
	if base == "" {
		return "", "", errors.New("exampleapp: 缺少 --admin")
	}
	// 演示站点的允许来源就是本示例应用的地址：真实接入时由运维登记业务页面来源。
	origin := "http://" + a.o.Listen
	body, _ := json.Marshal(map[string]any{
		"name": a.o.SiteName, "pow_bits": a.o.PowBits,
		"verify_mode": "pow_question", "generator_id": a.o.Generator, "difficulty": a.o.Difficulty,
		"origins": []string{origin}, "allow_missing_origin": false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/admin/sites", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.o.AdminKey)
	res, err := a.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("exampleapp: 建演示站点: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("exampleapp: 建演示站点 %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	var got struct {
		Site struct {
			SiteKey string `json:"site_key"`
			ID      int64  `json:"id"`
		} `json:"site"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return "", "", fmt.Errorf("exampleapp: 解析建站响应: %w", err)
	}
	if got.Site.SiteKey == "" || got.Secret == "" {
		return "", "", errors.New("exampleapp: 建站响应缺 site_key/secret")
	}
	return got.Site.SiteKey, got.Secret, nil
}

func (a *app) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app.css", a.handleStatic)
	mux.HandleFunc("GET /app.js", a.handleStatic)
	mux.HandleFunc("GET /fixture-init.js", a.handleStatic)
	mux.HandleFunc("GET /", a.handlePage)
	mux.HandleFunc("GET /fixture", a.handleFixture)
	mux.HandleFunc("POST /guestbook-form", a.handleFormPost)
	mux.HandleFunc("GET /guestbook", a.handleList)
	mux.HandleFunc("POST /guestbook", a.handlePost)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "site_key": a.siteKey})
	})
	if a.o.WidgetFrom == "self" {
		wh := widget.Handler()
		mux.Handle("GET "+widget.URLJS, wh)
		mux.Handle("GET "+widget.URLWorker, wh)
	}
	return mux
}

// cspFor 返回本档位要求页面携带的 CSP。**刻意给真实严格度**：
// no-worker 档就是要让 widget 明确失败，用来证明"CSP 禁 Worker 时不假装通过"。
func (a *app) cspFor() string {
	// apiBase 同时出现在 script-src（跨域加载 widget 脚本）与 connect-src（领题/兑换）。
	// 这里不给 'unsafe-inline'：页面的 CSS/JS 都在同源静态文件里。
	scriptSrc := fmt.Sprintf("'self' %s", a.o.APIBase)
	if a.o.WidgetFrom == "self" {
		scriptSrc = "'self'"
	}
	connectSrc := fmt.Sprintf("'self' %s", a.o.APIBase)
	switch a.o.CSPMode {
	case "none":
		return ""
	case "no-worker":
		return "default-src 'none'; script-src " + scriptSrc + "; connect-src " + connectSrc +
			"; img-src data: 'self'; style-src 'self'; worker-src 'none'; form-action 'self'; base-uri 'none'"
	case "worker-file":
		return "default-src 'none'; script-src " + scriptSrc + "; connect-src " + connectSrc +
			"; img-src data: 'self'; style-src 'self'; worker-src 'self'; form-action 'self'; base-uri 'none'"
	default: // blob
		return "default-src 'none'; script-src " + scriptSrc + "; connect-src " + connectSrc +
			"; img-src data: 'self'; style-src 'self'; worker-src blob:; form-action 'self'; base-uri 'none'"
	}
}

func (a *app) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	a.writePage(w, pageTemplate)
}

// handleFixture 渲染**文档里那份最小接入示例的可运行版本**（docs/WIDGET.md §1）。
//
// 为什么要专门做一个页面：文档示例最容易犯的两种错都不在文字里，只在浏览器里现形——
//
//	(a) 跨域 widget 脚本用 defer，后面紧跟的内联脚本立即调 EpoCHA.render ⇒ 竞争，
//	    首屏偶发 "EpoCHA is not defined"；本机缓存热了就测不出来。
//	(b) 容器放在 form 外 ⇒ 隐藏字段不属于目标 form，验证"成功"了但 POST 里根本没有 token。
//
// 这两条只有真加载、真提交才看得见，所以这里给一个能被浏览器用例驱动的页面，
// 而不是让用例去 grep 文档字符串。
func (a *app) handleFixture(w http.ResponseWriter, r *http.Request) {
	a.writePage(w, fixtureTemplate)
}

// writePage 注入**只有公开值**的配置块，并按 --widget-origin 决定脚本来源。
// 验证凭据不在这里，也不在任何响应里（按值扫描由 stage4b.sh 与 Go 测试各做一次）。
// 首页与 fixture 共用它：CSP 与注入方式都必须一致，否则"文档示例能跑"就推不出"页面能跑"。
func (a *app) writePage(w http.ResponseWriter, page string) {
	if csp := a.cspFor(); csp != "" {
		w.Header().Set("Content-Security-Policy", csp)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cfg := map[string]any{
		"api_base":   a.o.APIBase,
		"site_key":   a.siteKey,
		"widget_src": a.widgetSrc,
		"lang":       "zh-CN",
		"csp_mode":   a.o.CSPMode,
		// 只有同源部署脚本时才给 worker 文件路径（方案 B）。跨域时留空 ⇒ 组件走 Blob worker（方案 A）。
		// worker 脚本必须与页面同源：给一个跨域 URL 等于把可执行代码的来源交给外部决定，
		// 组件侧也会拒收（v1.js 的 normalizeOptions 检查 origin）。
		"worker_url": func() string {
			if a.o.WidgetFrom == "self" && a.o.CSPMode == "worker-file" {
				return "/widget/v1.worker.js"
			}
			return ""
		}(),
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, "config", http.StatusInternalServerError)
		return
	}
	page = strings.Replace(page, "{{CFG}}", templateJSEscape(string(blob)), 1)
	tag := `<script src="` + a.widgetSrc + `" defer></script>`
	if a.o.WidgetFrom == "self" {
		tag = `<script src="/widget/v1.js" defer></script>`
	}
	_, _ = io.WriteString(w, strings.Replace(page, "{{WIDGET_TAG}}", tag, 1))
}

// handleFormPost 收**原生表单**的 POST（application/x-www-form-urlencoded），
// 与 JSON 路径共用 commitProtected。它的存在是为了回答一个只有真提交才能回答的问题：
// "隐藏字段到底有没有跟着 form 走"。
func (a *app) handleFormPost(w http.ResponseWriter, r *http.Request) {
	if !a.sameSite(r) {
		a.log.Info("表单提交被来源拒绝", "origin", r.Header.Get("Origin"), "reason", "not-same-site")
		a.renderResult(w, http.StatusForbidden, errOrigin, "只接受来自本示例页面所在站的提交。")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		a.renderResult(w, http.StatusRequestEntityTooLarge, errTooLarge, "请求体过大或表单不可解析。")
		return
	}
	// 表单版同样走**字段白名单**：原生 form 的字段是页面里任何 input 都能加的，
	// 若这里宽松读取，"浏览器指定验证目标"就会从 POST 表单这条路绕回来。
	for k := range r.PostForm {
		if k != "text" && k != "epocha_response" {
			a.renderResult(w, http.StatusBadRequest, "epocha-bad-request", "表单里出现了未预期的字段。")
			return
		}
	}
	status, machine, message := a.commitProtected(r.Context(), r.PostForm.Get("text"), r.PostForm.Get("epocha_response"))
	a.renderResult(w, status, machine, message)
}

// renderResult 把结论写成**可断言**的 HTML：data-status/data-machine 给自动化用，
// 人也能直接读懂。不用 JSON 是因为这一页是浏览器导航的目标（form POST 的响应）。
func (a *app) renderResult(w http.ResponseWriter, status int, machine, message string) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; base-uri 'none'")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// 三个插值都走 HTML 转义：这一页是浏览器导航目标，把结论当纯文本拼进 HTML
	// 就是给自己留一个"错误消息变成标记"的注入面。
	_, _ = io.WriteString(w, strings.NewReplacer(
		"{{STATUS}}", strconv.Itoa(status),
		"{{MACHINE}}", html.EscapeString(machine),
		"{{MESSAGE}}", html.EscapeString(message),
	).Replace(resultTemplate))
}

// handleStatic 提供页面自己的 CSS 与 JS。放在**同源静态文件**而不是内联，
// 是为了让示例页面在严格 CSP（不给 'unsafe-inline'）下真的能跑：
// 实测内联写法被 script-src 挡下，页面看起来"就是不动"，而 curl 什么都看不出来。
func (a *app) handleStatic(w http.ResponseWriter, r *http.Request) {
	var body, ct string
	switch r.URL.Path {
	case "/app.css":
		body, ct = pageCSS, "text/css; charset=utf-8"
	case "/app.js":
		body, ct = pageJS, "text/javascript; charset=utf-8"
	case "/fixture-init.js":
		body, ct = fixtureInitJS, "text/javascript; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = io.WriteString(w, body)
}

// templateJSEscape 只处理 `</script>` 能提前闭合脚本块这一件事；
// JSON 本身的引号转义交给 encoding/json，不自己发明转义规则。
func templateJSEscape(s string) string {
	return strings.ReplaceAll(s, "</", "<\\/")
}

// postRequest 是**白名单**结构：多带任何字段（包括 target/url/api/secret）都拒绝。
// 这条是"浏览器不能指定验证目标"的结构保证，不是靠"读的时候忽略它"。
type postRequest struct {
	Text     string `json:"text"`
	Response string `json:"response"`
}

func (a *app) handlePost(w http.ResponseWriter, r *http.Request) {
	if !a.sameSite(r) {
		a.log.Info("留言提交被来源拒绝", "origin", r.Header.Get("Origin"), "reason", "not-same-site")
		a.reject(w, http.StatusForbidden, errOrigin, "只接受来自本示例页面所在站的提交。")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		a.reject(w, http.StatusRequestEntityTooLarge, errTooLarge, "请求体过大。")
		return
	}
	var req postRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		a.reject(w, http.StatusBadRequest, "epocha-bad-request", errBadJSON)
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		a.reject(w, http.StatusBadRequest, "epocha-bad-request", errBadJSON)
		return
	}
	text := strings.TrimSpace(req.Text)
	status, machine, message := a.commitProtected(r.Context(), text, req.Response)
	if status != http.StatusCreated {
		a.reject(w, status, machine, message)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "message": message,
	})
}

// commitProtected 是"受保护操作"的**唯一**实现：JSON 接口（app.js 的 fetch）与
// 原生表单 POST（/fixture 的 form）都走这一个函数。
//
// 为什么强调唯一：接入示例的价值在于"照抄就不会错"。若表单路径另写一份校验，
// 两份就会在"什么算已授权"上漂移，而漂移的方向通常是新那份更松。
//
// 返回 (status, machine, message)；status==201 才代表真的写入了。
func (a *app) commitProtected(ctx context.Context, text, response string) (int, string, string) {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > maxTextLen {
		return http.StatusBadRequest, "epocha-bad-request", "留言不能为空且不超过 500 字。"
	}
	if len(response) > maxTokenLen {
		return http.StatusBadRequest, "epocha-bad-request", "response 太长。"
	}
	if response == "" {
		// 关键演示点：**前端的 onSuccess 不是授权**。没带 token 就是没授权，
		// 哪怕页面看起来"已经绿了"。
		return http.StatusForbidden, errMissingToken, "没有人机验证凭据，不能写入留言。"
	}

	ok, codes, verifyErr := a.callSiteVerify(ctx, response)
	if verifyErr != nil {
		// 验证服务不可达/超时/响应不可解析 ⇒ fail closed。
		a.log.Error("siteverify 调用失败", "err", errKind(verifyErr))
		return http.StatusServiceUnavailable, errVerifyDown,
			"验证服务暂时不可用，留言**没有**写入。请稍后重新验证。"
	}
	if !ok {
		code := strings.Join(codes, ",")
		if strings.Contains(code, "timeout-or-duplicate") {
			a.log.Info("验证未通过，留言未写入", "code", code)
			return http.StatusConflict, errDuplicateTok, "人机验证未通过（token 无效、已过期或已被使用）。请重新验证。"
		}
		a.log.Info("验证未通过，留言未写入", "code", code)
		return http.StatusForbidden, errInvalidTok, "人机验证未通过（token 无效、已过期或已被使用）。请重新验证。"
	}

	// 到这一步服务端已经**消费**了这条 response（一次性账本已落账）。
	// 因此下面业务写入若失败，绝不能"把成功缓存起来下次再用"：那等于绕过一次性语义。
	if a.shouldFailWrite() {
		a.log.Error("留言写入失败（token 已被消费）")
		return http.StatusInternalServerError, "epocha-write-failed",
			"留言写入失败，而这次验证凭据**已经作废**（一次性）。请重新验证后再提交。"
	}
	verifiedAt := time.Now().UTC().Format(time.RFC3339)
	prefix := response[:minInt(len(response), 12)]
	if _, err := a.gb.Exec(`INSERT INTO guestbook(text, verified_at, token_prefix) VALUES (?,?,?)`,
		text, verifiedAt, prefix); err != nil {
		a.log.Error("留言写入失败（token 已被消费）", "err", errKind(err))
		return http.StatusInternalServerError, "epocha-write-failed",
			"留言写入失败，而这次验证凭据**已经作废**（一次性）。请重新验证后再提交。"
	}
	a.log.Info("留言已写入", "verified_at", verifiedAt)
	return http.StatusCreated, "", "留言已写入"
}

// shouldFailWrite 消费掉一次注入的失败标志（测试用）。
func (a *app) shouldFailWrite() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.writeFailOnce {
		return false
	}
	a.writeFailOnce = false
	return true
}

// callSiteVerify 用**服务端固定的目标**（a.o.APIBase + "/siteverify"）和进程内的凭据调用验证。
// 请求里没有任何字段能影响它——这是"不让浏览器指定验证服务目标地址"的实现。
func (a *app) callSiteVerify(ctx context.Context, response string) (bool, []string, error) {
	form := url.Values{"secret": {a.secret}, "response": {response}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.o.APIBase+"/siteverify", strings.NewReader(form.Encode()))
	if err != nil {
		return false, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := a.httpClient.Do(req)
	if err != nil {
		return false, nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return false, nil, err
	}
	// 5xx 与非 JSON 一律当作"没验证成"，不当作通过。
	var got struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return false, nil, fmt.Errorf("siteverify 响应不可解析（status=%d）", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		return false, got.ErrorCodes, nil
	}
	return got.Success, got.ErrorCodes, nil
}

// sameSite 是无 cookie 场景下的 CSRF 面收口：示例页面自己 POST 自己。
// 这里**没有** cookie/会话，因此不存在"带着会话凭据的跨站表单提交"这条路；
// 但仍然挡掉任意页面发起的跨站 POST（Origin 与 Sec-Fetch-Site 由浏览器强制设置，脚本改不了）。
// 既没有 Origin 也没有 Sec-Fetch-Site 的请求是非浏览器客户端（curl、自动化脚本）：
// 示例应用允许它（否则没法演示脚本路径），真实业务应当在这里拒绝。
//
// `Origin: null` 不是"另一个站"，而是**不透明来源**的表示：实测 Firefox 在原生 form
// POST 导航时会发 `Origin: null`（直接把 `"null"` 当 URL 解析会得到空 Host，于是每个
// 真实表单提交都被误拒）。这种请求不能凭 Origin 判定，必须回落到 Sec-Fetch-Site；
// 而 Sec-Fetch-Site 是浏览器强制设置的 forbidden header，脚本改不了，判定强度不降。
func (a *app) sameSite(r *http.Request) bool {
	host := r.Host
	if host == "" {
		host = a.o.Listen
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		if err != nil {
			return false
		}
		return u.Host == host
	}
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "cross-site", "cross-origin":
		return false
	case "same-origin", "same-site", "none":
		return true
	}
	// Origin: null 且没有 Sec-Fetch-Site：无法证明同源。示例应用仍放行（它是演示程序，
	// 且没有会话凭据可被 CSRF），但把这件事记进日志，避免被当成"来源检查起作用了"。
	if r.Header.Get("Origin") == "null" {
		a.log.Info("Origin 为 null 且无 Sec-Fetch-Site，按示例策略放行", "path", r.URL.Path)
	}
	return true
}

func (a *app) handleList(w http.ResponseWriter, r *http.Request) {
	rows, err := a.gb.Query(`SELECT id, text, verified_at FROM guestbook ORDER BY id DESC LIMIT 50`)
	if err != nil {
		a.reject(w, http.StatusInternalServerError, "epocha-storage-failed", "读留言失败。")
		return
	}
	defer rows.Close()
	type msg struct {
		ID       int64  `json:"id"`
		Text     string `json:"text"`
		At       string `json:"verified_at"`
		HasToken bool   `json:"-"`
	}
	out := []msg{}
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.ID, &m.Text, &m.At); err != nil {
			continue
		}
		out = append(out, m)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"messages": out, "count": len(out)})
}

func (a *app) reject(w http.ResponseWriter, status int, code, human string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": code, "detail": human})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		http.Error(w, `{"ok":false,"error":"epocha-internal"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func errKind(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "unspecified"
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	o, err := parseFlags(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "exampleapp:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := newApp(ctx, o, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "exampleapp:", err)
		os.Exit(1)
	}
	defer app.gb.Close()

	if err := assertLoopback(o.Listen); err != nil {
		fmt.Fprintln(os.Stderr, "exampleapp:", err, "（", loopbackHint, "）")
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "exampleapp:", err)
		os.Exit(1)
	}
	srv := &http.Server{
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	fmt.Printf("exampleapp：用浏览器打开 http://%s/\n", ln.Addr().String())
	fmt.Printf("  验证目标（服务端固定）：%s/siteverify\n", o.APIBase)
	fmt.Printf("  CSP 档位：%s；widget 来源：%s\n", o.CSPMode, app.widgetSrc)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "exampleapp:", err)
		os.Exit(1)
	}
}

// assertLoopback 与预览应用同一条规则：默认且强制 loopback。
// 示例应用带着站点凭据，不该在无人知情时绑到可路由地址上。
func assertLoopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen 形态不合法：%q", addr)
	}
	if host == "" {
		return errors.New("不接受空前缀地址（那是 0.0.0.0/::）")
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("端口不合法：%q", port)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("--listen 必须是 IP 或 localhost，实得 %q", host)
	}
	if ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("拒绝绑定非 loopback 地址 %q", host)
}

const pageTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>epocha 业务接入示例 · exampleapp</title>
<link rel="stylesheet" href="/app.css">
</head>
<body>
<h1>epocha 业务接入示例（exampleapp）</h1>
<p class="sub">受保护操作 = 往<strong>本示例应用自己的临时 SQLite</strong> 写一条留言。
业务后端持有站点验证凭据并调用 <code>/siteverify</code>；凭据不进页面、不进任何响应。</p>

<div class="box">
  <h2>1. 完成人机验证</h2>
  <div id="widget"></div>
  <p id="wstate" class="sub">组件状态：<code>idle</code></p>
  <label for="text">留言内容</label>
  <textarea id="text" maxlength="500" placeholder="写点什么…（不超过 500 字）"></textarea>
  <button id="send" type="button">提交留言</button>
  <button id="resend" type="button" title="用同一个 token 再提交一次，演示一次性语义">用同一个 token 再提交一次</button>
  <p id="status" role="status" aria-live="polite" data-kind="idle">还没提交。</p>
</div>

<div class="box">
  <h2>2. 当前留言</h2>
  <button id="reload" type="button">刷新列表</button>
  <ul id="list"><li class="sub">（加载中…）</li></ul>
</div>

<details class="warn">
  <summary>这个页面刻意演示的几条反例</summary>
  <ul>
    <li>「提交留言」按钮<strong>一直可点</strong>：不做人机验证也能提交，服务端会拒绝 —— 前端的
        <code>onSuccess</code> 不是授权，授权只由服务端 <code>/siteverify</code> 的
        <code>success:true</code> 决定。</li>
    <li>「用同一个 token 再提交一次」会把同一个 response 再发一遍 ⇒ 第二次必然被拒（一次性消费）。</li>
    <li>页面没有任何字段能指定验证目标：业务后端调哪个 <code>/siteverify</code> 在<strong>启动时</strong>就定死了。</li>
    <li>留言写入失败时（token 已被消费），提示重新验证；服务端不缓存那次成功。</li>
    <li>页面只有视觉验证通道：无法看清图片的用户本阶段无法完成它（非视觉等价通道尚未实现）。
        PoW-only 降级不是等效替代，它少掉的是"题目"这一层。</li>
  </ul>
</details>

<script id="cfg" type="application/json">{{CFG}}</script>
{{WIDGET_TAG}}
<script src="/app.js" defer></script>
</body>
</html>
`

// pageCSS / pageJS 是**同源静态文件**而不是内联块：严格 CSP 下不给 'unsafe-inline'，
// 内联样式与脚本都会被挡（本轮实测过：页面看起来"就是不动"，curl 完全看不出来）。
// 配置走 <script type="application/json"> 数据块 —— 它不会被执行，因此不受 script-src 约束。
// fixtureTemplate 是 docs/WIDGET.md §1 的最小接入示例**原样可运行版**。
//
// 三处刻意的设计，都是为了堵掉两个只在浏览器里现形的错误：
//  1. 容器 `<div id="captcha">` 在 `<form>` **内** —— 放在 form 外时隐藏字段不属于目标表单，
//     页面看着"验证通过"，POST 里却没有 token。
//  2. widget 脚本与初始化脚本**都带 defer**：deferred 脚本按文档顺序执行，
//     所以 init 一定跑在 EpoCHA 定义之后。一个 defer 一个同步才是竞争源头。
//  3. 初始化在**外部同源文件**里（/fixture-init.js），不靠内联 `<script>` 换便利：
//     CSP 保持 `script-src 'self' <apiBase>`，不需要 unsafe-inline。
const fixtureTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>epocha 最小接入示例（fixture）</title>
<link rel="stylesheet" href="/app.css">
</head>
<body>
<h1>最小接入示例（可运行 fixture）</h1>
<p class="sub">这一页就是文档 §1 那段示例的可运行版本：容器在 form 内、两个脚本都 defer 保序、
初始化走外部文件。受保护操作 = 原生表单 POST 到 /guestbook-form，服务端只有 /siteverify
返回 success:true 才写入。</p>

<form id="order" method="post" action="/guestbook-form">
  <h2>下单</h2>
  <label for="item">商品与备注</label>
  <input id="item" name="text" type="text" maxlength="500" value="一件示例商品">
  <div id="captcha"></div>
  <p id="wstate" class="sub">组件状态：<code>idle</code></p>
  <button type="submit">提交订单</button>
  <p class="sub">注意：提交按钮**一直可点**。没完成验证也会被服务端拒绝 —— 前端的
     onSuccess 不是授权。</p>
</form>

<script id="cfg" type="application/json">{{CFG}}</script>
{{WIDGET_TAG}}
<script src="/fixture-init.js" defer></script>
</body>
</html>
`

// fixtureInitJS 是页面自己的初始化脚本（同源、非内联）。它只用 site_key，
// 手里没有任何凭据，也不会去调 /siteverify —— 那一步只在业务后端。
const fixtureInitJS = `"use strict";
(function () {
  var CFG = JSON.parse(document.getElementById('cfg').textContent);
  var out = document.getElementById('wstate');
  var form = document.getElementById('order');
  // 状态一律走 textContent：拼 HTML 字符串会被门禁的按值扫描拦下，
  // 而且服务端/组件的错误文案是外部输入，不该有变成标记的机会。
  function setWState(s) { out.textContent = '组件状态：' + s; }
  var widgetId = window.EpoCHA.render(document.getElementById('captcha'), {
    apiBase: CFG.api_base,
    siteKey: CFG.site_key,
    language: CFG.lang,
    workerUrl: CFG.worker_url || '',
    fieldName: 'epocha_response',   // 与后端表单字段白名单一致
    form: form,                     // 容器已在 form 内时等价；表单外挂载就靠它显式关联
    onState: setWState,
    onSuccess: function () { setWState('verified（可以提交了；授权仍由服务端判定）'); },
    onExpired: function () { setWState('expired（凭据已清空，请重新验证）'); },
    onError: function (e) { setWState('error: ' + e.code); }
  });
  // 给自动化用的只读把手：不改变行为，只是让用例能拿到同一个实例句柄。
  window.__fixture = { id: widgetId, formId: 'order' };
})();
`

// resultTemplate 是原生表单 POST 的响应页（浏览器导航目标，所以是 HTML 不是 JSON）。
// data-* 两个属性让自动化能机器断言，同时人读得到同一句结论。
const resultTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>表单提交结果</title>
<link rel="stylesheet" href="/app.css">
</head>
<body>
<h1>表单提交结果</h1>
<p id="post-result" role="status" data-status="{{STATUS}}" data-machine="{{MACHINE}}">{{MESSAGE}}</p>
<p class="sub">HTTP 状态：<code>{{STATUS}}</code>　机器码：<code>{{MACHINE}}</code></p>
<a href="/fixture">回到示例页</a>
</body>
</html>
`

const pageCSS = `body{font:15px/1.6 system-ui,sans-serif;margin:0 auto;max-width:46rem;padding:1.5rem 1rem 3rem}
h1{font-size:1.2rem;margin:0 0 .25rem}
h2{font-size:1rem;margin:0 0 .5rem}
.sub{color:#555;font-size:.85rem}
.box{border:1px solid #bbb;border-radius:.5rem;padding:1rem;margin:1rem 0}
label{display:block;font-weight:600;margin:.6rem 0 .3rem}
textarea{width:100%;box-sizing:border-box;font:inherit;min-height:5rem;padding:.5rem}
button{font:inherit;padding:.5rem .9rem;margin:.6rem .5rem 0 0;cursor:pointer}
ul{padding-left:1.2rem}
li{margin:.2rem 0}
#status{margin-top:.6rem;padding:.5rem .7rem;border:1px solid #bbb;border-radius:.35rem;min-height:1.2rem}
#status[data-kind=ok]{border-color:#1c7d33}
#status[data-kind=err]{border-color:#a31414}
code{background:rgba(127,127,127,.15);padding:.05rem .3rem;border-radius:.2rem}
.warn{border-left:3px solid #b0761a;padding-left:.75rem}`

const pageJS = `"use strict";
// 这里只有公开配置（apiBase、site_key、组件脚本地址、CSP 档位）。
// 验证凭据与"验证目标"都不在页面里，浏览器也无法通过请求指定它们。
var CFG = JSON.parse(document.getElementById('cfg').textContent);
var els = {
  widget: document.getElementById('widget'), wstate: document.getElementById('wstate'),
  text: document.getElementById('text'), send: document.getElementById('send'),
  resend: document.getElementById('resend'), status: document.getElementById('status'),
  list: document.getElementById('list'), reload: document.getElementById('reload')
};
var lastToken = "";
var widgetId = "";
function say(msg, kind) { els.status.textContent = msg; els.status.dataset.kind = kind || 'idle'; }
function setWState(s) { els.wstate.textContent = '组件状态：' + s; }

function boot() {
  if (!window.EpoCHA) {
    say('组件脚本没有加载（检查 CSP 的 script-src，或 --api 是否可达）', 'err');
    return false;
  }
  widgetId = window.EpoCHA.render(els.widget, {
    apiBase: CFG.api_base,
    siteKey: CFG.site_key,
    language: CFG.lang,
    fieldName: '',            // 本示例程序化取 token，不用隐藏字段（集成方两种都可用）
    workerUrl: CFG.worker_url || '',
    onState: setWState,
    onSuccess: function (token) { lastToken = token; say('验证通过。现在提交才会被业务后端接受。', 'ok'); },
    onExpired: function () { lastToken = ''; say('验证凭据已过期，请重新完成验证。', 'err'); },
    onError: function (e) { say('组件报错：' + e.message, 'err'); }
  });
  return true;
}

// 提交给**业务后端**（示例应用自己），由它带凭据调 /siteverify。页面从不碰凭据。
function submitWith(token) {
  var text = els.text.value.trim();
  if (!text) { say('留言不能为空。', 'err'); return; }
  els.send.disabled = true; els.resend.disabled = true;
  say('正在提交…', 'idle');
  fetch('/guestbook', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text: text, response: token || '' })
  }).then(function (r) {
    return r.json().then(function (j) { return { status: r.status, j: j }; });
  }).then(function (res) {
    if (res.j.ok) { say('留言已写入（服务端验证通过）。', 'ok'); refresh(); }
    else { say('被拒绝 [' + res.j.error + ']：' + res.j.detail, 'err'); }
  }).catch(function (e) {
    say('提交失败：' + e.message, 'err');
  }).finally(function () { els.send.disabled = false; els.resend.disabled = false; });
}

function refresh() {
  fetch('/guestbook').then(function (r) { return r.json(); }).then(function (j) {
    els.list.textContent = '';
    var msgs = j.messages || [];
    if (!msgs.length) {
      var empty = document.createElement('li');
      empty.className = 'sub';
      empty.textContent = '（还没有留言）';
      els.list.appendChild(empty);
    }
    msgs.forEach(function (m) {
      var li = document.createElement('li');
      li.textContent = '#' + m.id + ' · ' + m.text + '（验证于 ' + m.verified_at + '）';
      els.list.appendChild(li);
    });
  }).catch(function () { els.list.textContent = '读取失败'; });
}

els.send.addEventListener('click', function () {
  // 先取**组件当前值**：这样"过期后组件清空 token"是能被观察到的真实行为，
  // 而不是靠页面自己记着的旧值。
  var live = widgetId && window.EpoCHA ? window.EpoCHA.getResponse(widgetId) : '';
  submitWith(live || lastToken);
});
els.resend.addEventListener('click', function () {
  if (!lastToken) { say('没有可重复使用的 token（先完成一次验证）。', 'err'); return; }
  submitWith(lastToken);
});
els.reload.addEventListener('click', refresh);

document.addEventListener('DOMContentLoaded', function () {
  if (!boot()) setWState('未加载');
  refresh();
});
`
