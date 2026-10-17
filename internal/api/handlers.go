package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shijianus/EpoCHA/internal/pow"
	"github.com/shijianus/EpoCHA/internal/risk"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
)

type challengeRequest struct {
	SiteKey string `json:"site_key"`
	Lang    string `json:"lang"`
}

type powPayload struct {
	Algo string `json:"algo"`
	Seed string `json:"seed"`
	Bits uint8  `json:"bits"`
}

type challengeResponse struct {
	Token     string     `json:"token"`
	ExpiresIn int64      `json:"expires_in"`
	Pow       powPayload `json:"pow"`
}

type redeemRequest struct {
	Token    string `json:"token"`
	PowNonce string `json:"pow_nonce"`
}

type redeemResponse struct {
	Response  string `json:"response"`
	ExpiresIn int64  `json:"expires_in"`
}

// --- 来源与 CORS ---

// originCheck 是浏览器来源裁决的结果。
type originCheck struct {
	// Canonical 是规范化后的 Origin（无 Origin 头时为 ""），会写进 challenge 行并被签名绑定。
	Canonical string
	// Allowed 表示该来源可以通过站点策略。
	Allowed bool
	// CORS 为 true 时才对响应挂 Access-Control-* 头。
	CORS bool
	// Reason 只用于日志，绝不进响应（红线 8）。
	Reason string
}

// checkOrigin 实现 DESIGN §10.3 的来源策略：
//   - 有 Origin：必须是合法规范形式且在站点允许列表里，否则拒绝（不做后缀匹配、不自动放行子域）。
//   - 无 Origin：只有站点显式打开 allow_missing_origin 才放行（演示客户端路径），不对所有站点静默放开。
//
// 必须记住的边界：Origin 由浏览器强制设置、脚本改不了，但**非浏览器客户端可以随便写**。
// 所以这一层防的是"别的站点/别的应用拿你的 site_key 做题"，不是"机器人 vs 真人"；
// CORS 也只决定"浏览器允不允许这个来源读响应"，它从来不阻止请求本身。
func (s *Server) checkOrigin(r *http.Request, site *store.Site) originCheck {
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		return originCheck{Allowed: site.Policy.AllowMissingOrigin, Reason: "missing-origin"}
	}
	norm, err := store.NormalizeOrigin(raw)
	if err != nil {
		return originCheck{Reason: "malformed-origin"}
	}
	ok, err := s.db.OriginAllowed(r.Context(), site.ID, norm)
	if err != nil || !ok {
		return originCheck{Canonical: norm, Reason: "origin-not-allowed"}
	}
	return originCheck{Canonical: norm, Allowed: true, CORS: true, Reason: "ok"}
}

// applyCORS 只在来源被允许时挂头，并且始终带 Vary: Origin——
// 少了 Vary，缓存会把"一个允许的来源"的响应喂给"一个被拒绝的来源"。
func applyCORS(w http.ResponseWriter, o originCheck) {
	if !o.CORS {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", o.Canonical) // 精确回显已验证的来源，永不用 *
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Allow-Credentials", "false")
}

// handlePreflight 只做预检应答：不签发 challenge、不消费 token、不写任何库行。
func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "OPTIONS, POST")
	// 顺序有讲究：先按来源地址限流与形态检查，再去查库解析站点。
	// 反过来的话，每个匿名 OPTIONS 都会先打一次 SQL——1 vCPU 机器上这是个便宜的放大面。
	if s.preflightRejected(w, r) {
		return
	}
	oc := s.preflightOrigin(r)
	if !oc.Allowed {
		// 来源不在白名单：不给任何 Allow-* 头，让浏览器判定预检失败。
		w.WriteHeader(http.StatusForbidden)
		return
	}
	applyCORS(w, oc)
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// preflightOrigin 用 site_key 解析来源是否被允许。预检不落库，所以未知/停用站点一律不给 CORS 头。
func (s *Server) preflightOrigin(r *http.Request) originCheck {
	siteKey := r.URL.Query().Get("site_key")
	if siteKey == "" {
		siteKey = r.Header.Get("X-Epocha-Site-Key")
	}
	if siteKey == "" || len(siteKey) > maxFieldLen {
		return originCheck{}
	}
	site, err := s.db.SiteByKey(r.Context(), siteKey)
	if err != nil || !site.Enabled {
		return originCheck{}
	}
	return s.checkOrigin(r, site)
}

// preflightRejected 对预检做限流与形态检查；返回 true 表示已写响应、调用方必须停手。
func (s *Server) preflightRejected(w http.ResponseWriter, r *http.Request) bool {
	if !s.allowGlobalIP(r, w) {
		return true
	}
	if am := r.Header.Get("Access-Control-Request-Method"); am != "" && !strings.EqualFold(am, "POST") {
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	if ah := r.Header.Get("Access-Control-Request-Headers"); ah != "" &&
		!strings.EqualFold(strings.TrimSpace(ah), "content-type") {
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	return false
}

// --- 公开端点 ---

// handleChallenge 签发一次 PoW 挑战。阶段 2 叠加站点策略与来源裁决；仍然没有题目层。
func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request) {
	var req challengeRequest
	if err := decodeJSON(w, r, maxBodyPublic, &req); err != nil {
		s.reject(w, CodeInvalidRequest)
		return
	}
	if !fieldOK(req.SiteKey, maxFieldLen) || len(req.Lang) > 35 {
		s.reject(w, CodeInvalidRequest)
		return
	}
	// 第一道只按来源地址限流：此时还不知道站点，若按调用方自报的 site_key 建条目，
	// 随便编造 site_key 就能污染站点维度的限流状态。
	if !s.allowGlobalIP(r, w) {
		return
	}
	ctx := r.Context()

	site, err := s.db.SiteByKey(ctx, req.SiteKey)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "查站点", err)
			return
		}
		s.reject(w, CodeInvalidRequest)
		return
	}
	// 停用的站点与不存在的站点给出同一个响应：不透露哪一层失败（红线 8）。
	if !site.Enabled {
		s.reject(w, CodeInvalidRequest)
		return
	}
	oc := s.checkOrigin(r, site)
	if !oc.Allowed {
		s.log.Info("来源被拒", "site_id", site.ID, "reason", oc.Reason)
		s.reject(w, CodeInvalidRequest)
		return
	}
	applyCORS(w, oc)
	if !s.allowSiteDims(r, site, w) {
		return
	}

	params, err := pow.NewParams(site.PowBits)
	if err != nil {
		s.internalError(w, r, "生成 PoW 参数", err)
		return
	}
	id, err := store.NewID()
	if err != nil {
		s.internalError(w, r, "生成 challenge id", err)
		return
	}
	now := s.now().UTC()
	ch := &store.Challenge{
		ID: id, SiteID: site.ID, Origin: oc.Canonical,
		PowSeed: params.Seed, PowBits: site.PowBits,
		CreatedAt: now, ExpiresAt: now.Add(site.Policy.ChallengeTTL),
	}
	if err := s.db.InsertChallenge(ctx, ch, s.caps); err != nil {
		if errors.Is(err, store.ErrCapExceeded) {
			// 存量到顶：对外与限流同族，不透露内部原因（红线 8）。
			s.rateLimited(w, time.Second)
			return
		}
		s.internalError(w, r, "写入 challenge", err)
		return
	}
	seedB64 := base64.RawURLEncoding.EncodeToString(params.Seed)
	tok, err := s.signer.Sign(token.PurposeChallenge, token.Claims{
		Sid: site.ID, Svid: site.Version, CID: id,
		Exp: ch.ExpiresAt.Unix(), Bits: site.PowBits, Seed: seedB64,
	}, now)
	if err != nil {
		s.internalError(w, r, "签名", err)
		return
	}
	writeJSON(w, http.StatusOK, challengeResponse{
		Token:     tok,
		ExpiresIn: int64(site.Policy.ChallengeTTL / time.Second),
		Pow:       powPayload{Algo: params.Algo, Seed: seedB64, Bits: site.PowBits},
	})
}

// handleRedeem 校验 PoW 解并原子消费 challenge。
func (s *Server) handleRedeem(w http.ResponseWriter, r *http.Request) {
	var req redeemRequest
	if err := decodeJSON(w, r, maxBodyPublic, &req); err != nil {
		s.reject(w, CodeInvalidRequest)
		return
	}
	if !fieldOK(req.Token, 4096) || len(req.PowNonce) == 0 || len(req.PowNonce) > maxPowNonceLen {
		s.reject(w, CodeInvalidRequest)
		return
	}
	if !s.allowGlobalIP(r, w) {
		return
	}
	ctx := r.Context()
	now := s.now().UTC()

	claims, err := s.signer.Verify(token.PurposeChallenge, req.Token, now)
	if err != nil {
		// 篡改、过期、用途混淆、未知 kid、缺站点版本一律同一结果：不给探测反馈。
		s.rejectChallenge(w, "")
		return
	}
	site, err := s.db.SiteByID(ctx, claims.Sid)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "查站点", err)
			return
		}
		s.rejectChallenge(w, claims.CID)
		return
	}
	// 站点版本必须与签发时一致：停用（以及停用后再启用）让此前签发的 token 全部失效。
	if !site.Enabled || site.Version != claims.Svid {
		s.log.Info("站点状态使 token 失效", "site_id", site.ID, "cid8", shortCID(claims.CID))
		s.rejectChallenge(w, claims.CID)
		return
	}
	if !s.allowSiteDims(r, site, w) {
		return
	}
	oc := s.checkOrigin(r, site)
	if !oc.Allowed {
		s.log.Info("来源被拒", "site_id", site.ID, "reason", oc.Reason)
		s.rejectChallenge(w, claims.CID)
		return
	}
	applyCORS(w, oc)

	ch, err := s.db.Challenge(ctx, claims.CID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "读 challenge", err)
			return
		}
		s.rejectChallenge(w, claims.CID)
		return
	}
	// token 与库内记录必须完全一致：这是"签名之外再以库为权威"的纵深防御（DESIGN §6）。
	if ch.SiteID != claims.Sid || ch.PowBits != claims.Bits ||
		base64.RawURLEncoding.EncodeToString(ch.PowSeed) != claims.Seed {
		s.rejectChallenge(w, ch.ID)
		return
	}
	// 来源必须与签发时绑定的同一个：换个来源重放同一 token 不行。
	if ch.Origin != oc.Canonical {
		s.log.Info("来源与签发不一致", "cid8", shortCID(ch.ID))
		s.rejectChallenge(w, ch.ID)
		return
	}
	if ch.Status != "issued" || !now.Before(ch.ExpiresAt) {
		s.rejectChallenge(w, ch.ID)
		return
	}
	ok, err := pow.Params{Algo: pow.Algo, Bits: ch.PowBits, Seed: ch.PowSeed}.Verify(req.PowNonce)
	if err != nil {
		s.rejectChallenge(w, ch.ID)
		return
	}
	if !ok {
		if _, _, ferr := s.db.FailAttempt(ctx, ch.ID); ferr != nil && !errors.Is(ferr, store.ErrNotFound) {
			s.internalError(w, r, "记录失败尝试", ferr)
			return
		}
		s.rejectChallenge(w, ch.ID)
		return
	}
	resp, err := s.db.Redeem(ctx, ch.ID, now, site.Policy.ResponseTTL)
	if err != nil {
		if !errors.Is(err, store.ErrNotRedeemable) {
			s.internalError(w, r, "兑换", err)
			return
		}
		s.rejectChallenge(w, ch.ID)
		return
	}
	tok, err := s.signer.Sign(token.PurposeResponse, token.Claims{
		Sid: resp.SiteID, Svid: site.Version, CID: resp.ChallengeID, RID: resp.ID,
		Exp: resp.ExpiresAt.Unix(),
	}, now)
	if err != nil {
		s.internalError(w, r, "签名", err)
		return
	}
	// 成功结果在事务提交之后才返回（DESIGN D12）。
	writeJSON(w, http.StatusOK, redeemResponse{
		Response:  tok,
		ExpiresIn: int64(site.Policy.ResponseTTL / time.Second),
	})
}

// handleSiteverify 返回 reCAPTCHA 风格 JSON。业务错误同样 HTTP 200（reCAPTCHA 语义）。
// 这是服务端到服务端的接口：**不挂任何 CORS 头**，不让任意页面在浏览器里读验证结果。
func (s *Server) handleSiteverify(w http.ResponseWriter, r *http.Request) {
	form, err := parseVerifyBody(w, r)
	if err != nil {
		verifyFail(w, ErrBadRequest)
		return
	}
	if !s.allowGlobalIP(r, w) {
		return
	}
	ctx := r.Context()
	now := s.now().UTC()

	secret, response := form["secret"], form["response"]
	if secret == "" {
		verifyFail(w, ErrMissingInputSecret)
		return
	}
	if response == "" || len(response) > 4096 {
		verifyFail(w, ErrMissingInputResponse)
		return
	}
	// 先定位站点：secret 不对或已被撤销就绝不触碰任何 token 状态（DESIGN §10.1）。
	site, err := s.db.FindSiteBySecret(ctx, secret, now)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "查站点", err)
			return
		}
		verifyFail(w, ErrInvalidInputSecret)
		return
	}
	claims, err := s.signer.Verify(token.PurposeResponse, response, now)
	if err != nil {
		if errors.Is(err, token.ErrExpired) {
			verifyFail(w, ErrTimeoutOrDuplicate)
			return
		}
		verifyFail(w, ErrInvalidInputResponse)
		return
	}
	// 停用站点或版本变动后旧 response token 一律不放行；重新启用也不会让它复活。
	if !site.Enabled || site.Version != claims.Svid {
		verifyFail(w, ErrInvalidInputResponse)
		return
	}
	if claims.Sid != site.ID {
		verifyFail(w, ErrInvalidInputResponse)
		return
	}
	v, err := s.db.ConsumeResponse(ctx, claims.RID, site.ID, now)
	if err != nil {
		if errors.Is(err, store.ErrConsumed) || errors.Is(err, store.ErrNotFound) {
			// 已使用、不存在或已过期：与 reCAPTCHA 的 timeout-or-duplicate 语义一致。
			verifyFail(w, ErrTimeoutOrDuplicate)
			return
		}
		s.internalError(w, r, "消费 response", err)
		return
	}
	// 消费成功后才返回成功；异常时返回内部错误而不是假成功。
	writeVerifySuccess(w, v)
}

// writeVerifySuccess 的 hostname 取自**签发时绑定在账本行里的规范化 Origin**，
// 不采信调用方在 /siteverify 自报的 hostname（DESIGN §10.3）。
// 保证范围必须说清楚：它表示"这个 challenge 在这个允许来源上签发"，
// 而 Origin 头对非浏览器客户端可伪造——所以它不是机器人身份证明，也没有任何
// 硬件级 / 浏览器证明 / 可信远程证明的含义，只是服务端记录并签名绑定过的一个输入值。
// 直接 API 调用（无 Origin 且被站点放行）时该字段整体缺省，而不是回显调用方给的值。
func writeVerifySuccess(w http.ResponseWriter, v *store.Verification) {
	body := map[string]any{
		"success":      true,
		"challenge_ts": v.ChallengeCreatedAt.UTC().Format(time.RFC3339),
	}
	if host := store.OriginHost(v.Origin); host != "" && len(host) <= maxHostnameLen {
		body["hostname"] = host
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// 只回答"进程在跑"：不暴露版本、路径、站点数、依赖健康，也不代表验证语义可用。
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- 内部辅助 ---

func (s *Server) reject(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error-codes": []string{code}})
}

func (s *Server) rejectChallenge(w http.ResponseWriter, cid string) {
	// cid 前 8 字符可用于关联日志（不是密钥，也不足以定位 token）。
	if c := shortCID(cid); c != "" {
		s.log.Info("challenge 被拒", "cid8", c)
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error-codes": []string{CodeInvalidChallenge}})
}

func shortCID(cid string) string {
	if len(cid) > 8 {
		return cid[:8]
	}
	return cid
}

// internalError 按端点选择错误码族：/siteverify 是 reCAPTCHA 契约面，服务端故障只能用
// 唯一扩展码 internal-error 随 5xx 返回，业务方据此判断"验证未完成"而不是"验证失败"
// （DESIGN §10.1）。响应保持 {success:false, error-codes:[...]} 的文档形状，让调用方的
// 解析器不必为 5xx 特例处理缺失字段。/api/* 用自有码集，不引入该码。
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.Error("内部错误", "where", what, "err", errKind(err))
	if r.URL.Path == "/siteverify" {
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"success": false, "error-codes": []string{ErrInternalError}})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error-codes": []string{CodeInvalidRequest}})
}

// errKind 只保留错误类别，避免把库/驱动细节带进日志之外的任何地方。
func errKind(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, store.ErrNotFound):
		return "not-found"
	case errors.Is(err, store.ErrNotRedeemable):
		return "not-redeemable"
	case errors.Is(err, store.ErrConsumed):
		return "consumed"
	case errors.Is(err, store.ErrBadConfig):
		return "bad-config"
	case errors.Is(err, store.ErrCapExceeded):
		return "cap-exceeded"
	case errors.Is(err, store.ErrConflict):
		return "conflict"
	default:
		return "unspecified"
	}
}

// allowGlobalIP 是来源地址维度的第一道闸门（站点未知时也只走这一道）。
func (s *Server) allowGlobalIP(r *http.Request, w http.ResponseWriter) bool {
	key := "ip:" + risk.Key(s.src.ClientIP(r))
	if !s.ipLimit.Allow(key) {
		s.rateLimited(w, s.ipLimit.RetryAfter(key))
		return false
	}
	return true
}

// allowSiteDims 是站点定位后的两维计数：站点整体速率，以及"该站点 + 该来源地址"速率。
// key 一律取库中的站点 id，绝不取调用方自报的 site_key 字符串（阶段 1 复核发现的污染面）。
func (s *Server) allowSiteDims(r *http.Request, site *store.Site, w http.ResponseWriter) bool {
	siteDim := "site:" + strconv.FormatInt(site.ID, 10)
	perPair := "ips:" + strconv.FormatInt(site.ID, 10) + ":" + risk.Key(s.src.ClientIP(r))
	if !s.siteLimit.AllowN(siteDim, site.Policy.RateSiteRPM) {
		s.rateLimited(w, s.siteLimit.RetryAfter(siteDim))
		return false
	}
	if !s.ipLimit.AllowN(perPair, site.Policy.RateIPRPM) {
		s.rateLimited(w, s.ipLimit.RetryAfter(perPair))
		return false
	}
	return true
}

func (s *Server) rateLimited(w http.ResponseWriter, retry time.Duration) {
	if retry > 0 {
		w.Header().Set("Retry-After", retryAfterHeader(retry))
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error-codes": []string{CodeRateLimited}})
}

// parseVerifyBody 同时支持 form 与 JSON；只读白名单字段，其余忽略（reCAPTCHA 客户端会带额外字段）。
// 注意：这里读到的 hostname/remoteip 都不参与安全判定（DESIGN §10.3、§12）。
func parseVerifyBody(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyVerify))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var req struct {
			Secret   string `json:"secret"`
			Response string `json:"response"`
			RemoteIP string `json:"remoteip"`
			Hostname string `json:"hostname"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		out["secret"], out["response"] = req.Secret, req.Response
		out["remoteip"], out["hostname"] = req.RemoteIP, req.Hostname
		return out, nil
	}
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"secret", "response", "remoteip", "hostname"} {
		out[k] = vals.Get(k)
	}
	return out, nil
}

// verifyFail 只回 success 与 error-codes 两个键：不携带 challenge_ts/hostname，
// 避免"失败也回完整结构"成为探测面（DESIGN §10.1）。
func verifyFail(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusOK, map[string]any{"success": false, "error-codes": []string{code}})
}
