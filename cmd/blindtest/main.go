// Command blindtest 是**开发期人工可读性盲测**的记录服务（阶段 4B，DESIGN §17 阶段 4B）。
//
// 它只做两件 exampleapp 不该做的事：
//  1. 按固定批次（默认 20 题）连续出题，记录每题的结果与三段耗时；
//  2. 把这次会话导出成**不含任何凭据**的 JSON/CSV。
//
// 三条边界：
//   - 走**真实链路**：题目由 epocha 服务端下发，PoW 由浏览器算，兑换由 /api/redeem 判分。
//     它测的是"真人面对生产 widget 能不能做对"，不是"能不能看懂一张 PNG"。
//   - 页面/API **没有任何答案出口**。对错来自服务端裁决（verified vs 答错终结），
//     而不是把 ground truth 发给浏览器。
//   - 只监听 loopback；题库、站点与密钥都由 docs/scripts/blindtest.sh 在运行时生成，
//     本进程不认识任何凭据（它连 secret 都不需要：判定用的是 widget 的一次性兑换结果）。
//
// 三段耗时的口径（为什么可以分开、以及不能分开到哪一步）：
//
//	答题  = 题目就绪 → 用户点提交           （真人阅读+输入，唯一与可读性相关的量）
//	PoW   = solving → submitting            （浏览器内 worker 上报，与题目无关）
//	兑换  = submitting → verified/error     （一次 localhost 往返 + 服务端判分事务）
//	"兑换"这一段**无法**再拆成 RTT 与服务端处理时间：那需要在生产服务端插桩，
//	本工具不做，也不假装能分。精度只到毫秒级 JS 定时器，且受浏览器后台节流影响
//	（headless 与否都可能触发），所以它是"量级证据"，不是稳态基线。
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxBody       = 1 << 16
	maxRecords    = 5000 // 一次盲测批次的硬上限：防止页面被当成免费日志盘写满磁盘
	defaultListen = "127.0.0.1:8491"
)

// siteKeyRe 与 web/widget/v1.js 里 render() 的 siteKey 校验同一套形态。
// 两边不一致的话，参数解析通过、组件在浏览器里抛错，排查方向会被指错。
var siteKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, log); err != nil {
		fmt.Fprintln(os.Stderr, "blindtest:", err)
		os.Exit(1)
	}
}

type tier struct {
	Label   string // 人读的档位说明，例如 "低档（difficulty 1）"
	SiteKey string // 公开值：进页面，不进导出
}

type options struct {
	Listen    string
	APIBase   string
	WidgetSrc string
	WorkerURL string
	Tiers     []tier
	Batch     int
	Out       string
}

// record 是一条作答记录。**刻意不含**用户输入的字符串：
// 盲测要的是"对不对、多久、换了几次题"，把输入原样记下来只会在导出里多一个无用的敏感面。
type record struct {
	Session   string `json:"session"`
	Tier      string `json:"tier"`
	Item      int    `json:"item"`
	Outcome   string `json:"outcome"` // verified | wrong | timeout | error | unreadable
	ErrorCode string `json:"error_code,omitempty"`
	AnswerMs  int64  `json:"answer_ms"`  // 题目就绪 → 点提交
	PowMs     int64  `json:"pow_ms"`     // solving → submitting
	RedeemMs  int64  `json:"redeem_ms"`  // submitting → 终态
	Refreshes int    `json:"refreshes"`  // 本题内点"换一张"的次数
	StartedAt string `json:"started_at"` // RFC3339，只用于人核对顺序
}

func parseFlags(args []string, getenv func(string) string) (*options, error) {
	fs := flag.NewFlagSet("blindtest", flag.ContinueOnError)
	var o options
	var tiersRaw string
	fs.StringVar(&o.Listen, "listen", firstNonEmpty(getenv("BLINDTEST_LISTEN"), defaultListen), "监听地址（必须 loopback）")
	fs.StringVar(&o.APIBase, "api", firstNonEmpty(getenv("EPOCHA_API"), "http://127.0.0.1:8080"), "epocha 服务基址")
	fs.StringVar(&o.WidgetSrc, "widget-src", firstNonEmpty(getenv("BLINDTEST_WIDGET_SRC"), ""), "widget 脚本 URL（留空则用 <api>/widget/v1.js）")
	fs.StringVar(&o.WorkerURL, "worker-url", "", "同源 worker 文件（方案 B），留空走 Blob worker")
	fs.StringVar(&tiersRaw, "tiers", "", "档位列表：label=site_key，逗号分隔（必填）")
	fs.IntVar(&o.Batch, "batch", 20, "每档题数（1..100）")
	fs.StringVar(&o.Out, "out", "", "把记录追加写入的 JSONL 路径（留空则只在内存与导出接口里）")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	for _, part := range strings.Split(tiersRaw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// 按**最后一个** `=` 切：档位标签里出现 `=` 是常态（"难度档 3（difficulty=3）"），
		// 用第一个 `=` 切会把标签留在 key 上，widget 才在浏览器里报 siteKey 形态不合法——
		// 这个坑是真实启动盲测时抓到的。site_key 的字符集里没有 `=`，所以末次切分是无歧义的。
		i := strings.LastIndex(part, "=")
		label, key := "", ""
		if i >= 0 {
			label, key = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		if label == "" || key == "" {
			return nil, fmt.Errorf("--tiers 需要 label=site_key 形式，实得 %q", part)
		}
		// 启动期就校验 site_key 形状：这里放过一个坏 key，代价是真人打开页面后组件直接抛错，
		// 而那看起来像"工具坏了"，不是"启动参数写错了"。
		if !siteKeyRe.MatchString(key) {
			return nil, fmt.Errorf("档位 %q 的 site_key 形态不合法：%q", label, key)
		}
		o.Tiers = append(o.Tiers, tier{Label: label, SiteKey: key})
	}
	if len(o.Tiers) == 0 {
		return nil, errors.New("--tiers 至少要一个档位")
	}
	if len(o.Tiers) > 8 {
		return nil, errors.New("--tiers 最多 8 个档位（批次已经够长了）")
	}
	if o.Batch < 1 || o.Batch > 100 {
		return nil, errors.New("--batch 须在 1..100")
	}
	if o.WidgetSrc == "" {
		o.WidgetSrc = strings.TrimRight(o.APIBase, "/") + "/widget/v1.js"
	}
	if !strings.HasPrefix(o.WidgetSrc, o.APIBase) {
		return nil, errors.New("--widget-src 必须在 --api 之下（不从第三方站点加载执行代码）")
	}
	return &o, nil
}

type session struct {
	mu      sync.Mutex
	records []record
	id      string
}

func run(ctx context.Context, args []string, getenv func(string) string, log *slog.Logger) error {
	o, err := parseFlags(args, getenv)
	if err != nil {
		return err
	}
	ln, err := listenLoopback(o.Listen)
	if err != nil {
		return err
	}
	sess := &session{id: time.Now().UTC().Format("20060102T150405Z")}
	srv := &server{o: o, sess: sess, log: log}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	log.Info("盲测页面已就绪", "url", "http://"+ln.Addr().String()+"/", "batch", o.Batch, "tiers", len(o.Tiers))
	fmt.Printf("盲测入口：http://%s/（每档 %d 题，共 %d 档）\n", ln.Addr().String(), o.Batch, len(o.Tiers))
	if err := httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// 退出时把内存里的记录落一次盘：浏览器崩溃或用户直接 Ctrl-C 也不该丢掉这次人工数据。
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if o.Out != "" && len(sess.records) > 0 {
		if err := appendRecords(o.Out, sess.records); err != nil {
			log.Error("退出时写记录失败", "err", errKind(err))
		} else {
			log.Info("盲测记录已写入", "path", o.Out, "count", len(sess.records))
		}
	}
	return nil
}

type server struct {
	o    *options
	sess *session
	log  *slog.Logger
}

func (s *server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handlePage)
	mux.HandleFunc("GET /app.js", s.handleStaticJS)
	mux.HandleFunc("GET /app.css", s.handleStaticCSS)
	mux.HandleFunc("POST /record", s.handleRecord)
	mux.HandleFunc("GET /export.json", func(w http.ResponseWriter, r *http.Request) { s.export(w, "json") })
	mux.HandleFunc("GET /export.csv", func(w http.ResponseWriter, r *http.Request) { s.export(w, "csv") })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	return mux
}

func (s *server) headers(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// CSP：脚本走同源 + 服务端的 widget 脚本；worker 用 Blob（方案 A）。
		// 与其它示例页一致地**不给 unsafe-inline**：页面 JS/CSS 都是同源文件。
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self' "+s.o.APIBase+"; connect-src 'self' "+s.o.APIBase+
				"; img-src data: 'self'; style-src 'self'; worker-src blob:; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		next(w, r)
	}
}

func (s *server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.headers(func(w http.ResponseWriter, _ *http.Request) {
		cfg := map[string]any{
			"api_base":    s.o.APIBase,
			"widget_src":  s.o.WidgetSrc,
			"worker_url":  s.o.WorkerURL,
			"batch":       s.o.Batch,
			"session":     s.sess.id,
			"language":    "zh-CN",
			"tiers":       tierViews(s.o.Tiers),
			"max_records": maxRecords,
		}
		blob, err := json.Marshal(cfg)
		if err != nil {
			http.Error(w, "config", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// widget 脚本必须**在 /app.js 之前**且**同样带 defer**：deferred 脚本按文档顺序执行，
		// 这是 app.js 里 EpoCHA 已定义的唯一保证。漏掉这个标签的失败方式很隐蔽——
		// 页面正常显示、档位按钮能点，但组件永远不出现（实测就是"进度停在 0/20"）。
		page := strings.NewReplacer(
			"{{CFG}}", jsSafeJSON(string(blob)),
			"{{WIDGET_TAG}}", `<script src="`+jsSafeURL(s.o.WidgetSrc)+`" defer></script>`,
		).Replace(pageHTML)
		_, _ = io.WriteString(w, page)
	})(w, r)
}

// jsSafeURL 只处理一件事：URL 里出现 `</script>` 之类会提前终结数据块。
// widget 地址来自启动参数而不是请求，但拼进 HTML 就统一转义，不靠"它应该是干净的"。
func jsSafeURL(s string) string { return strings.ReplaceAll(s, "</", "<\\/") }

func tierViews(ts []tier) []map[string]string {
	out := make([]map[string]string, 0, len(ts))
	for _, t := range ts {
		// 只给 label 与 site_key：都是公开值。验证凭据从来不进这个进程，也就进不了页面。
		out = append(out, map[string]string{"label": t.Label, "site_key": t.SiteKey})
	}
	return out
}

// handleRecord 收页面记的一条结果。字段走**白名单结构体**，多余字段直接拒：
// 这个接口不该变成"页面想写什么就写什么"的落盘口子。
func (s *server) handleRecord(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	var in struct {
		Tier      string `json:"tier"`
		Item      int    `json:"item"`
		Outcome   string `json:"outcome"`
		ErrorCode string `json:"error_code"`
		AnswerMs  int64  `json:"answer_ms"`
		PowMs     int64  `json:"pow_ms"`
		RedeemMs  int64  `json:"redeem_ms"`
		Refreshes int    `json:"refreshes"`
	}
	if err := dec.Decode(&in); err != nil {
		http.Error(w, "bad-request", http.StatusBadRequest)
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		http.Error(w, "bad-request", http.StatusBadRequest)
		return
	}
	known := map[string]bool{"verified": true, "wrong": true, "timeout": true, "error": true, "unreadable": true}
	if !known[in.Outcome] {
		http.Error(w, "bad-outcome", http.StatusBadRequest)
		return
	}
	if in.Item < 0 || in.Item > s.o.Batch || in.Refreshes < 0 || in.Refreshes > 200 {
		http.Error(w, "bad-item", http.StatusBadRequest)
		return
	}
	// 耗时是页面自己量的：给一个"明显不合理"的上界而不是信任它，
	// 否则一次后台节流就能把统计拉到看不下去的地步，还看不出是哪种坏。
	const maxMs = 3_600_000
	if in.AnswerMs < 0 || in.AnswerMs > maxMs || in.PowMs < -1 || in.PowMs > maxMs || in.RedeemMs < -1 || in.RedeemMs > maxMs {
		http.Error(w, "bad-timing", http.StatusBadRequest)
		return
	}
	tierOK := false
	for _, t := range s.o.Tiers {
		if t.Label == in.Tier {
			tierOK = true
		}
	}
	if !tierOK {
		http.Error(w, "bad-tier", http.StatusBadRequest)
		return
	}
	rec := record{
		Session: s.sess.id, Tier: in.Tier, Item: in.Item, Outcome: in.Outcome,
		ErrorCode: in.ErrorCode, AnswerMs: in.AnswerMs, PowMs: in.PowMs, RedeemMs: in.RedeemMs,
		Refreshes: in.Refreshes, StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.sess.mu.Lock()
	over := len(s.sess.records) >= maxRecords
	if !over {
		s.sess.records = append(s.sess.records, rec)
	}
	out := s.o.Out
	s.sess.mu.Unlock()
	if over {
		http.Error(w, "too-many-records", http.StatusRequestEntityTooLarge)
		return
	}
	if out != "" {
		if err := appendRecords(out, []record{rec}); err != nil {
			s.log.Error("写记录失败", "err", errKind(err))
			http.Error(w, "storage", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func (s *server) export(w http.ResponseWriter, kind string) {
	s.sess.mu.Lock()
	recs := append([]record(nil), s.sess.records...)
	s.sess.mu.Unlock()
	if kind == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="blindtest.json"`)
		_ = json.NewEncoder(w).Encode(map[string]any{"session": s.sess.id, "records": recs, "count": len(recs)})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="blindtest.csv"`)
	cw := csv.NewWriter(w)
	// 表头里**没有** site_key、没有任何凭据，也没有用户敲进去的字符。
	_ = cw.Write([]string{"session", "tier", "item", "outcome", "error_code", "answer_ms", "pow_ms", "redeem_ms", "refreshes", "started_at"})
	for _, r := range recs {
		_ = cw.Write([]string{r.Session, r.Tier, itoa(r.Item), r.Outcome, r.ErrorCode,
			itoa(int(r.AnswerMs)), itoa(int(r.PowMs)), itoa(int(r.RedeemMs)), itoa(r.Refreshes), r.StartedAt})
	}
	cw.Flush()
}

func appendRecords(path string, recs []record) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func listenLoopback(addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("监听地址 %q 不合法: %w", addr, err)
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("blindtest 只允许监听 loopback，实得 %q", host)
	}
	return net.Listen("tcp", net.JoinHostPort(host, port))
}

func isLoopbackHost(h string) bool {
	if h == "" || h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func jsSafeJSON(s string) string { return strings.ReplaceAll(s, "</", "<\\/") }
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
func itoa(v int) string { return fmt.Sprintf("%d", v) }
func errKind(err error) string {
	var ne *net.OpError
	if errors.As(err, &ne) {
		return "net"
	}
	return "other"
}
