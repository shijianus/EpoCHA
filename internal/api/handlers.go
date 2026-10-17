package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
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
	if err := decodeJSON(r, maxBodyPublic, &req); err != nil {
		s.reject(w, CodeInvalidRequest)
		return
	}
	if !fieldOK(req.SiteKey, maxFieldLen) || len(req.Lang) > 35 {
		s.reject(w, CodeInvalidRequest)
		return
	}
	if !s.allow(r, req.SiteKey, w) {
		return
	}
	ctx := r.Context()

	site, err := s.db.SiteByKey(ctx, req.SiteKey)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, "查站点", err)
			return
		}
		s.reject(w, CodeInvalidRequest)
		return
	}
	params, err := pow.NewParams(site.PowBits)
	if err != nil {
		s.internalError(w, "生成 PoW 参数", err)
		return
	}
	id, err := store.NewID()
	if err != nil {
		s.internalError(w, "生成 challenge id", err)
		return
	}
	now := s.now().UTC()
	ch := &store.Challenge{
		ID: id, SiteID: site.ID, PowSeed: params.Seed, PowBits: site.PowBits,
		CreatedAt: now, ExpiresAt: now.Add(s.challengeTTL),
	}
	if err := s.db.InsertChallenge(ctx, ch); err != nil {
		s.internalError(w, "写入 challenge", err)
		return
	}
	seedB64 := base64.RawURLEncoding.EncodeToString(params.Seed)
	tok, err := s.signer.Sign(token.PurposeChallenge, token.Claims{
		Sid: site.ID, CID: id, Exp: ch.ExpiresAt.Unix(), Bits: site.PowBits, Seed: seedB64,
	}, now)
	if err != nil {
		s.internalError(w, "签名", err)
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
	if err := decodeJSON(r, maxBodyPublic, &req); err != nil {
		s.reject(w, CodeInvalidRequest)
		return
	}
	if !fieldOK(req.Token, 4096) || len(req.PowNonce) == 0 || len(req.PowNonce) > maxPowNonceLen {
		s.reject(w, CodeInvalidRequest)
		return
	}
	if !s.allow(r, "", w) {
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
	ch, err := s.db.Challenge(ctx, claims.CID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, "读 challenge", err)
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
			s.internalError(w, "记录失败尝试", ferr)
			return
		}
		s.rejectChallenge(w, ch.ID)
		return
	}
	resp, err := s.db.Redeem(ctx, ch.ID, now, s.responseTTL)
	if err != nil {
		if !errors.Is(err, store.ErrNotRedeemable) {
			s.internalError(w, "兑换", err)
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
		s.internalError(w, "签名", err)
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
	form, err := parseVerifyBody(r)
	if err != nil {
		verifyFail(w, ErrBadRequest)
		return
	}
	if !s.allowIPOnly(r, w) {
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
			s.internalError(w, "查站点", err)
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
		s.internalError(w, "消费 response", err)
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

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.log.Error("内部错误", "where", what, "err", errKind(err))
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
	default:
		return "unspecified"
	}
}

func (s *Server) allow(r *http.Request, siteKey string, w http.ResponseWriter) bool {
	key := risk.Key(s.src.ClientIP(r))
	if !s.ipLimit.Allow("ip:" + key) {
		s.rateLimited(w, s.ipLimit.RetryAfter("ip:"+key))
		return false
	}
	if siteKey != "" && !s.siteLimit.Allow("site:"+siteKey) {
		s.rateLimited(w, s.siteLimit.RetryAfter("site:"+siteKey))
		return false
	}
	return true
}

func (s *Server) allowIPOnly(r *http.Request, w http.ResponseWriter) bool {
	key := risk.Key(s.src.ClientIP(r))
	if !s.ipLimit.Allow("ip:" + key) {
		s.rateLimited(w, s.ipLimit.RetryAfter("ip:"+key))
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
func parseVerifyBody(r *http.Request) (map[string]string, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyVerify+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyVerify {
		return nil, errors.New("api: siteverify 请求体超限")
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

var _ = strings.TrimSpace
