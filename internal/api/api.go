// Package api 是阶段 1 的公开 HTTP 层：PoW challenge 签发、兑换与 reCAPTCHA 风格验证（DESIGN §10.1）。
//
// 阶段 1 只有 PoW 一层——不存在题目，也不做行为风控判定，响应与日志都不得暗示它们存在。
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/pow"
	"github.com/shijianus/EpoCHA/internal/risk"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
	"github.com/shijianus/EpoCHA/web/widget"
)

// 请求体上限：公开端点小，siteverify 略大（表单可能带 hostname/remoteip）。
const (
	maxBodyAdmin    = 64 << 10
	maxBodyPublic   = 4 << 10
	maxAnswerLen    = 64
	maxBodyVerify   = 8 << 10
	maxFieldLen     = 512
	maxPowNonceLen  = pow.MaxNonceLen
	maxHostnameLen  = 253
	maxResponseTime = 30 * time.Second
)

// /siteverify 使用 reCAPTCHA 已定义的码族，加一个明确的服务器故障扩展（DESIGN §10.1）。
const (
	ErrMissingInputSecret   = "missing-input-secret"
	ErrInvalidInputSecret   = "invalid-input-secret"
	ErrMissingInputResponse = "missing-input-response"
	ErrInvalidInputResponse = "invalid-input-response"
	ErrBadRequest           = "bad-request"
	ErrTimeoutOrDuplicate   = "timeout-or-duplicate"
	ErrInternalError        = "internal-error" // 本专案扩展：仅在 5xx 时出现
)

// 自有端点的有限码集：不区分篡改/过期/已消费/尝试耗尽，避免给探测反馈（红线 8）。
const (
	CodeInvalidRequest   = "invalid-request"
	CodeInvalidChallenge = "invalid-challenge"
	CodeRateLimited      = "rate-limited"
	// CodeChallengeUnavailable 是阶段 3A 新增的可重试码：池里没有满足本站策略的题，
	// 或站点配了一个本程序没登记的题型。两者折叠成同一个响应——不透露哪一层挡住的（红线 8）。
	// 它**绝不**被处理成"那就只发 PoW"：静默降级会让站点声明的策略失效而业务方毫无察觉。
	CodeChallengeUnavailable = "challenge-unavailable"
)

// ErrInternal 是内部错误的哨兵，对外一律折叠成通用响应。
var ErrInternal = errors.New("api: 内部错误")

type Server struct {
	db        *store.DB
	signer    *token.Signer
	gens      *generator.Registry
	src       *risk.Source
	ipLimit   *risk.Limiter
	siteLimit *risk.Limiter
	log       *slog.Logger
	now       func() time.Time
	caps      store.Caps
}

// New 组装服务。限流器与代理裁决来自配置。
func New(cfg *config.Config, db *store.DB, signer *token.Signer, gens *generator.Registry, log *slog.Logger) (*Server, error) {
	// gens 必传：允许 nil 会让"配了 pow_question 却没有注册表"变成运行时静默 503，
	// 那是接线错误，应该在构造时就失败。
	if cfg == nil || db == nil || signer == nil || gens == nil {
		return nil, errors.New("api: 构造参数不完整")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	src, err := risk.NewSource(cfg.TrustProxy, cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	ipLimit, err := risk.NewLimiter(cfg.RateIPRPM, cfg.RateMaxState)
	if err != nil {
		return nil, err
	}
	siteLimit, err := risk.NewLimiter(cfg.RateSiteRPM, cfg.RateMaxState)
	if err != nil {
		return nil, err
	}
	return &Server{
		db: db, signer: signer, gens: gens, src: src, ipLimit: ipLimit, siteLimit: siteLimit,
		log: log, now: time.Now,
		caps: store.Caps{MaxTotal: cfg.MaxLedgerRows, MaxActive: cfg.MaxActiveChallenges, MaxActivePerSite: cfg.MaxActivePerSite},
	}, nil
}

// Handler 返回**公共**监听器上的根路由。
//
// 这里刻意不挂任何 /api/admin/*：管理路由只在 admin 监听器上存在（internal/admin），
// 公共端口即便配了管理密钥也找不到入口。OPTIONS 只做预检，不落库、不消费状态。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/challenge", s.handleChallenge)
	mux.HandleFunc("OPTIONS /api/challenge", s.handlePreflight)
	mux.HandleFunc("POST /api/redeem", s.handleRedeem)
	mux.HandleFunc("OPTIONS /api/redeem", s.handlePreflight)
	mux.HandleFunc("POST /siteverify", s.handleSiteverify)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	// /readyz 与 /healthz 分列：前者是"能不能接流量"的存储层自检，后者只证明进程存在。
	// 容器健康检查用 /readyz（阶段 5A），存活探针继续用 /healthz。
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	// widget 静态资源挂在**公共**监听器上（DESIGN §10.1、D10）：浏览器要在业务页面上
	// 用 `<script src>` 跨域取它，且它不含任何按身份变化的内容。
	// 只有这两条固定版本化路径，没有目录、没有按参数拼路径。
	widgetAssets := widget.Handler()
	mux.Handle("GET "+widget.URLJS, widgetAssets)
	mux.Handle("GET "+widget.URLWorker, widgetAssets)
	return s.recoverMiddleware(s.headerMiddleware(s.accessLogMiddleware(mux)))
}

// --- 中间件 ---

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// 只记录路径与错误类型，不记录请求体（可能含 token）。
				s.log.Error("panic", "path", r.URL.Path, "type", panicType(rec))
				w.Header().Del("Content-Length")
				body := map[string]any{"error-codes": []string{CodeInvalidRequest}}
				if r.URL.Path == "/siteverify" {
					body = map[string]any{"success": false, "error-codes": []string{ErrInternalError}}
				}
				writeJSON(w, http.StatusInternalServerError, body)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func panicType(rec any) string {
	switch rec.(type) {
	case string:
		return "string"
	case error:
		return "error"
	default:
		return "other"
	}
}

func (s *Server) headerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Pragma", "no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rw := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		// 白名单字段：绝不写 token、secret、答案、payload（红线 11）。
		s.log.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", rw.status,
			"dur_ms", time.Since(start).Milliseconds(),
			"ip_key", risk.Key(s.src.ClientIP(r)))
	})
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// --- 公共辅助 ---

func writeJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		// 序列化失败属于内部错误，不回显细节。
		http.Error(w, `{"error-codes":["invalid-request"]}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// decodeJSON 严格解析：限制体积、禁止未知字段、限制总长度。
func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// 允许可选字段缺失，但不允许尾随第二个 JSON 值。
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("api: 请求体含多余内容")
	}
	return nil
}

func fieldOK(s string, max int) bool { return len(s) > 0 && len(s) <= max }

// powSeedFromToken 解出签名内的 seed；解码失败说明 token 与 claims 不一致。
func powSeedFromToken(b64 string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil || len(raw) == 0 {
		return nil, errors.New("api: seed 损坏")
	}
	return raw, nil
}

// constantTimeEqual 是给上层复用的比较入口，保持"比较一律常数时间"这条红线可被测试直接驱动。
func constantTimeEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

func retryAfterHeader(d time.Duration) string {
	sec := int64(d / time.Second)
	if d%time.Second != 0 {
		sec++
	}
	if sec < 1 {
		return "1"
	}
	return strconv.FormatInt(sec, 10)
}
