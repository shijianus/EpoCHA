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

// handleChallenge 签发一次 PoW 挑战。阶段 1 不返回题目字段（DESIGN §10.1）。
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
	if !s.allowIP(r, w) {
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
	// 站点维度限流按库中站点 id 计数（不是自报字符串），且必须在写库之前。
	if !s.allowSite(r, site.ID, w) {
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
		ID: id, SiteID: site.ID, PowSeed: params.Seed, PowBits: site.PowBits,
		CreatedAt: now, ExpiresAt: now.Add(s.challengeTTL),
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
		Sid: site.ID, CID: id, Exp: ch.ExpiresAt.Unix(), Bits: site.PowBits, Seed: seedB64,
	}, now)
	if err != nil {
		s.internalError(w, r, "签名", err)
		return
	}
	writeJSON(w, http.StatusOK, challengeResponse{
		Token:     tok,
		ExpiresIn: int64(s.challengeTTL / time.Second),
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
	if !s.allowIP(r, w) {
		return
	}
	ctx := r.Context()
	now := s.now().UTC()

	claims, err := s.signer.Verify(token.PurposeChallenge, req.Token, now)
	if err != nil {
		// 篡改、过期、用途混淆、未知 kid 一律同一结果：不给探测反馈。
		s.rejectChallenge(w, "")
		return
	}
	if !s.allowSite(r, claims.Sid, w) {
		return
	}
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
	resp, err := s.db.Redeem(ctx, ch.ID, now, s.responseTTL)
	if err != nil {
		if !errors.Is(err, store.ErrNotRedeemable) {
			s.internalError(w, r, "兑换", err)
			return
		}
		s.rejectChallenge(w, ch.ID)
		return
	}
	tok, err := s.signer.Sign(token.PurposeResponse, token.Claims{
		Sid: resp.SiteID, CID: resp.ChallengeID, RID: resp.ID,
		Exp: resp.ExpiresAt.Unix(),
	}, now)
	if err != nil {
		s.internalError(w, r, "签名", err)
		return
	}
	// 成功结果在事务提交之后才返回（DESIGN D12）。
	writeJSON(w, http.StatusOK, redeemResponse{
		Response:  tok,
		ExpiresIn: int64(s.responseTTL / time.Second),
	})
}

// handleSiteverify 返回 reCAPTCHA 风格 JSON。业务错误同样 HTTP 200（reCAPTCHA 语义）。
func (s *Server) handleSiteverify(w http.ResponseWriter, r *http.Request) {
	form, err := parseVerifyBody(w, r)
	if err != nil {
		verifyFail(w, ErrBadRequest)
		return
	}
	if !s.allowIP(r, w) {
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
	// 先定位站点：secret 不对就绝不触碰任何 token 状态（DESIGN §10.1）。
	site, err := s.db.FindSiteBySecret(ctx, secret)
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
	// 消费成功后才读一次状态；异常时返回内部错误而不是假成功。
	writeVerifySuccess(w, v.ChallengeCreatedAt, form["hostname"])
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
	短 := cid
	if len(短) > 8 {
		短 = 短[:8]
	}
	if 短 != "" {
		s.log.Info("challenge 被拒", "cid8", 短)
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error-codes": []string{CodeInvalidChallenge}})
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
	default:
		return "unspecified"
	}
}

// allowIP 是来源地址维度限流，任何端点的第一道闸门。
func (s *Server) allowIP(r *http.Request, w http.ResponseWriter) bool {
	key := "ip:" + risk.Key(s.src.ClientIP(r))
	if !s.ipLimit.Allow(key) {
		s.rateLimited(w, s.ipLimit.RetryAfter(key))
		return false
	}
	return true
}

// allowSite 是站点维度限流，key 取库中的站点 id 而不是调用方自报的 site_key：
// 用自报字符串作 key，随机编造的 site_key 就能挤掉真实站点的限流状态（阶段 1 复核发现）。
func (s *Server) allowSite(r *http.Request, siteID int64, w http.ResponseWriter) bool {
	key := "site:" + strconv.FormatInt(siteID, 10)
	if !s.siteLimit.Allow(key) {
		s.rateLimited(w, s.siteLimit.RetryAfter(key))
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

func writeVerifySuccess(w http.ResponseWriter, challengeTS time.Time, hostname string) {
	body := map[string]any{
		"success":      true,
		"challenge_ts": challengeTS.UTC().Format(time.RFC3339),
	}
	// 阶段 1 没有域名白名单，hostname 只是回显调用方自报值，不是安全依据（DESIGN §10.1）。
	if hostname != "" && len(hostname) <= maxHostnameLen {
		body["hostname"] = hostname
	}
	writeJSON(w, http.StatusOK, body)
}
