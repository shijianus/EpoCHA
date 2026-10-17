// Package admin 是阶段 2 的管理面：站点与验证凭据生命周期、允许来源、站点级策略与最小审计
// （DESIGN §10.2、§10.3）。
//
// 三件事决定了这个包怎么写：
//  1. 它是**高危面**：只挂在独立的 admin 监听器上（默认 127.0.0.1），公共监听器根本没有这些路由；
//  2. 管理凭据与站点 secret 同样只存带 pepper 的 HMAC 摘要，明文只在创建响应里出现一次；
//  3. 所有写操作落审计（actor = 管理密钥的公开 key_id），审计写失败就不对外声称成功。
//
// 阶段 5 的后台页面、题池水位与统计仍不在这里。
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/risk"
	"github.com/shijianus/EpoCHA/internal/store"
)

// 管理面自有的有限错误码集：与公开端点的 /api/* 码集和 /siteverify 的 reCAPTCHA 码集都不同族。
const (
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeBadRequest   = "bad-request"
	CodeNotFound     = "not-found"
	CodeRateLimited  = "rate-limited"
	CodeInternal     = "internal-error"
)

const (
	maxBodyAdmin   = 64 << 10
	maxAuthFailRPM = 10          // 同一来源地址每分钟允许的管理鉴权尝试次数
	authWindow     = time.Minute // 与 risk.Limiter 的固定窗口一致
)

// Server 是管理 API。
type Server struct {
	db  *store.DB
	src *risk.Source
	// authFails 复用有界令牌桶做鉴权失败计数：条目数有硬上限，
	// 否则攻击者用随机源地址就能把失败计数表变成内存放大器（红线 6）。
	authFails *risk.Limiter
	log       *slog.Logger
	now       func() time.Time
}

// New 组装管理面。cfg 提供来源地址裁决参数与限流状态上限。
func New(cfg *config.Config, db *store.DB, log *slog.Logger) (*Server, error) {
	if cfg == nil || db == nil {
		return nil, errors.New("admin: 构造参数不完整")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	src, err := risk.NewSource(cfg.TrustProxy, cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	fails, err := risk.NewLimiter(maxAuthFailRPM, cfg.RateMaxState)
	if err != nil {
		return nil, err
	}
	return &Server{db: db, src: src, authFails: fails, log: log, now: time.Now}, nil
}

// Handler 返回管理路由。**只**应挂在 admin 监听器上。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/whoami", s.auth(s.handleWhoami))

	mux.HandleFunc("GET /api/admin/sites", s.auth(s.handleListSites))
	mux.HandleFunc("POST /api/admin/sites", s.auth(s.handleCreateSite))
	mux.HandleFunc("GET /api/admin/sites/{id}", s.auth(s.handleGetSite))
	mux.HandleFunc("PUT /api/admin/sites/{id}", s.auth(s.handleUpdateSite))
	mux.HandleFunc("DELETE /api/admin/sites/{id}", s.auth(s.handleDeleteSite))
	mux.HandleFunc("POST /api/admin/sites/{id}/enable", s.auth(s.handleSetEnabled(true)))
	mux.HandleFunc("POST /api/admin/sites/{id}/disable", s.auth(s.handleSetEnabled(false)))

	mux.HandleFunc("PUT /api/admin/sites/{id}/origins", s.auth(s.handlePutOrigins))
	mux.HandleFunc("GET /api/admin/sites/{id}/origins", s.auth(s.handleGetOrigins))

	mux.HandleFunc("GET /api/admin/sites/{id}/secrets", s.auth(s.handleListSecrets))
	mux.HandleFunc("POST /api/admin/sites/{id}/secrets", s.auth(s.handleAddSecret))
	mux.HandleFunc("POST /api/admin/sites/{id}/secrets/rotate", s.auth(s.handleRotateSecret))
	mux.HandleFunc("POST /api/admin/sites/{id}/secrets/{keyID}/revoke", s.auth(s.handleRevokeSecret))

	mux.HandleFunc("GET /api/admin/admin-keys", s.auth(s.handleListAdminKeys))
	mux.HandleFunc("POST /api/admin/admin-keys", s.auth(s.handleAddAdminKey))
	mux.HandleFunc("POST /api/admin/admin-keys/{keyID}/revoke", s.auth(s.handleRevokeAdminKey))

	mux.HandleFunc("GET /api/admin/audit", s.auth(s.handleAudit))

	return s.recoverMiddleware(s.headerMiddleware(s.accessLog(mux)))
}

// --- 中间件与鉴权 ---

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("admin panic", "path", r.URL.Path)
				fail(w, http.StatusInternalServerError, CodeInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// headerMiddleware 与公共端点同样禁缓存；管理面**不加任何 CORS 头**：
// 阶段 2 不做 Cookie 会话，跨源脚本本来也拿不到 Bearer 头，加 CORS 只会扩大暴露面。
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

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rw := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		// 白名单字段：不写 Authorization 头、不写请求体、不写 secret（红线 11）。
		s.log.Info("admin request",
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

// authCtxKey 是 context 的键类型：必须是不带数据的空结构体，
// 否则"写入用的键"与"读取用的键"会因为字段值不同而取不到值（Go 的 ctx 值按键类型+值比较）。
type authCtxKey struct{}

// auth 校验 Bearer 管理凭据。
//
// 三条硬性行为：
//   - 库里没有任何管理密钥记录 → 一律 401（缺凭据绝不等于放开管理功能）；
//   - 比对失败与被撤销的密钥给出同一个响应（401、同一码），不区分"错在哪"；
//   - 鉴权失败按来源地址计数并有上限，避免把这里变成密钥爆破的低成本面。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		key, ok := bearerToken(r)
		if !ok {
			s.denied(w, r, "missing-bearer")
			return
		}
		keyID, err := s.db.MatchAdminKey(ctx, key, s.now())
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Error("admin 鉴权读库失败", "err", errKind(err))
				fail(w, http.StatusInternalServerError, CodeInternal)
				return
			}
			s.denied(w, r, "unknown-or-revoked-key")
			return
		}
		next(w, r.WithContext(context.WithValue(ctx, authCtxKey{}, keyID)))
	}
}

func (s *Server) denied(w http.ResponseWriter, r *http.Request, reason string) {
	ipKey := "admin:" + risk.Key(s.src.ClientIP(r))
	// 失败计数本身也要有界：AllowN 返回 false 表示该来源地址在窗口内失败次数已达上限。
	if !s.authFails.AllowN(ipKey, maxAuthFailRPM) {
		w.Header().Set("Retry-After", strconv.Itoa(int(authWindow/time.Second)))
		s.log.Warn("admin 鉴权失败次数达上限", "ip_key", risk.Key(s.src.ClientIP(r)))
		fail(w, http.StatusTooManyRequests, CodeRateLimited)
		return
	}
	s.log.Warn("admin 鉴权失败", "reason", reason)
	// 401 带 WWW-Authenticate 只说 scheme，不回显任何凭据片段。
	w.Header().Set("WWW-Authenticate", `Bearer realm="epocha-admin"`)
	fail(w, http.StatusUnauthorized, CodeUnauthorized)
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || h[:len(p)] != p {
		return "", false
	}
	tok := h[len(p):]
	if tok == "" || len(tok) > 512 {
		return "", false
	}
	return tok, true
}

// actor 取鉴权时命中的 key_id；缺失说明路由没经过 auth（编码错误，宁可失败也不写空 actor）。
func actor(r *http.Request) (string, error) {
	v, ok := r.Context().Value(authCtxKey{}).(string)
	if !ok || v == "" {
		return "", errors.New("admin: 缺少鉴权上下文")
	}
	return v, nil
}

// --- 响应与解析辅助 ---

func writeJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": code})
}

// auditOK / auditFail 把"写审计"变成每个管理写操作的收尾。审计写失败时对外不声称成功：
// 状态变动（撤销、停用）本身不回滚——安全方向是"更严"，回滚反而危险。
func (s *Server) auditOK(ctx context.Context, r *http.Request, action, target string) error {
	a, err := actor(r)
	if err != nil {
		return err
	}
	if err := s.db.AppendAudit(ctx, a, action, target, true); err != nil {
		s.log.Error("admin 审计写入失败", "action", action, "err", errKind(err))
		return err
	}
	return nil
}

func (s *Server) auditFail(ctx context.Context, r *http.Request, action, target string) {
	a, err := actor(r)
	if err != nil {
		return
	}
	if err := s.db.AppendAudit(ctx, a, action, target, false); err != nil {
		s.log.Error("admin 审计写入失败", "action", action, "err", errKind(err))
	}
}

func siteIDOf(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	if raw == "" || len(raw) > 20 {
		fail(w, http.StatusBadRequest, CodeBadRequest)
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		fail(w, http.StatusBadRequest, CodeBadRequest)
		return 0, false
	}
	return id, true
}

func errKind(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, store.ErrNotFound):
		return "not-found"
	case errors.Is(err, store.ErrBadConfig):
		return "bad-config"
	case errors.Is(err, store.ErrConflict):
		return "conflict"
	default:
		return "unspecified"
	}
}

func failFromErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, CodeNotFound)
	case errors.Is(err, store.ErrBadConfig):
		fail(w, http.StatusBadRequest, CodeBadRequest)
	case errors.Is(err, store.ErrConflict):
		fail(w, http.StatusConflict, CodeBadRequest)
	default:
		fail(w, http.StatusInternalServerError, CodeInternal)
	}
}
