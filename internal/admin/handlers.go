package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/shijianus/EpoCHA/internal/store"
)

// --- 视图 ---

type siteView struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	SiteKey            string `json:"site_key"`
	Enabled            bool   `json:"enabled"`
	Version            int64  `json:"version"`
	PowBits            uint8  `json:"pow_bits"`
	ChallengeTTL       int64  `json:"challenge_ttl_sec"`
	ResponseTTL        int64  `json:"response_ttl_sec"`
	RateIPRPM          int    `json:"rate_ip_rpm"`
	RateSiteRPM        int    `json:"rate_site_rpm"`
	AllowMissingOrigin bool   `json:"allow_missing_origin"`
	CreatedAt          string `json:"created_at"`
}

func viewSite(s *store.Site) siteView {
	return siteView{
		ID: s.ID, Name: s.Name, SiteKey: s.SiteKey, Enabled: s.Enabled, Version: s.Version,
		PowBits:            s.PowBits,
		ChallengeTTL:       int64(s.Policy.ChallengeTTL / time.Second),
		ResponseTTL:        int64(s.Policy.ResponseTTL / time.Second),
		RateIPRPM:          s.Policy.RateIPRPM,
		RateSiteRPM:        s.Policy.RateSiteRPM,
		AllowMissingOrigin: s.Policy.AllowMissingOrigin,
		CreatedAt:          s.CreatedAt.UTC().Format(time.RFC3339),
	}
}

type secretView struct {
	KeyID     string `json:"key_id"`
	CreatedAt string `json:"created_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
	// Active 是按"当前时刻"算出来的便利字段；权威判定始终在库里的 revoked_at 与请求时刻。
	Active bool `json:"active"`
}

func viewSecret(s *store.SiteSecret, now time.Time) secretView {
	v := secretView{
		KeyID:     s.KeyID,
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
		Active:    s.RevokedAt.IsZero() || s.RevokedAt.After(now),
	}
	if !s.RevokedAt.IsZero() {
		v.RevokedAt = s.RevokedAt.UTC().Format(time.RFC3339)
	}
	return v
}

type adminKeyView struct {
	KeyID     string `json:"key_id"`
	Label     string `json:"label"`
	CreatedAt string `json:"created_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
	Active    bool   `json:"active"`
}

func viewAdminKey(k *store.AdminKey, now time.Time) adminKeyView {
	v := adminKeyView{
		KeyID: k.KeyID, Label: k.Label,
		CreatedAt: k.CreatedAt.UTC().Format(time.RFC3339),
		Active:    k.RevokedAt.IsZero() || k.RevokedAt.After(now),
	}
	if !k.RevokedAt.IsZero() {
		v.RevokedAt = k.RevokedAt.UTC().Format(time.RFC3339)
	}
	return v
}

type auditView struct {
	ID      int64  `json:"id"`
	Ts      string `json:"ts"`
	Actor   string `json:"actor"`
	Action  string `json:"action"`
	Target  string `json:"target"`
	Success bool   `json:"success"`
}

// --- 请求解析 ---

// decodeAdmin 严格解析：体积上限、禁止未知字段、禁止尾随第二个 JSON 值。
func decodeAdmin(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyAdmin))
	if err != nil {
		fail(w, http.StatusBadRequest, CodeBadRequest)
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}") // 管理写操作允许空对象表示"全部用默认值"
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, CodeBadRequest)
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, CodeBadRequest)
		return false
	}
	return true
}

type createSiteRequest struct {
	Name               string   `json:"name"`
	SiteKey            string   `json:"site_key"`
	Origins            []string `json:"origins"`
	PowBits            *uint8   `json:"pow_bits"`
	ChallengeTTL       *int64   `json:"challenge_ttl_sec"`
	ResponseTTL        *int64   `json:"response_ttl_sec"`
	RateIPRPM          *int     `json:"rate_ip_rpm"`
	RateSiteRPM        *int     `json:"rate_site_rpm"`
	AllowMissingOrigin *bool    `json:"allow_missing_origin"`
}

type updateSiteRequest struct {
	PowBits            *uint8 `json:"pow_bits"`
	ChallengeTTL       *int64 `json:"challenge_ttl_sec"`
	ResponseTTL        *int64 `json:"response_ttl_sec"`
	RateIPRPM          *int   `json:"rate_ip_rpm"`
	RateSiteRPM        *int   `json:"rate_site_rpm"`
	AllowMissingOrigin *bool  `json:"allow_missing_origin"`
}

type originsRequest struct {
	Origins []string `json:"origins"`
}

type revokeSecretRequest struct {
	TransitionSec *int64 `json:"transition_sec"`
}

type addAdminKeyRequest struct {
	Label string `json:"label"`
}

// updateFrom 把请求里的可选项转成 store 的部分更新（nil 字段原样传下去 = 不改）。
func updateFrom(req *updateSiteRequest) store.SiteUpdate {
	u := store.SiteUpdate{}
	if req.PowBits != nil {
		u.PowBits = req.PowBits
	}
	if req.ChallengeTTL != nil {
		d := time.Duration(*req.ChallengeTTL) * time.Second
		u.ChallengeTTL = &d
	}
	if req.ResponseTTL != nil {
		d := time.Duration(*req.ResponseTTL) * time.Second
		u.ResponseTTL = &d
	}
	if req.RateIPRPM != nil {
		u.RateIPRPM = req.RateIPRPM
	}
	if req.RateSiteRPM != nil {
		u.RateSiteRPM = req.RateSiteRPM
	}
	if req.AllowMissingOrigin != nil {
		u.AllowMissingOrigin = req.AllowMissingOrigin
	}
	return u
}

// --- handlers ---

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	a, err := actor(r)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key_id": a})
}

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request) {
	afterID := int64(0)
	if v := r.URL.Query().Get("after_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, CodeBadRequest)
			return
		}
		afterID = n
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, CodeBadRequest)
			return
		}
		limit = n
	}
	sites, err := s.db.ListSites(r.Context(), afterID, limit)
	if err != nil {
		// 分页参数越界是客户端错误（400），不是服务端故障：不要把它伪装成 500
		failFromErr(w, err)
		return
	}
	views := make([]siteView, 0, len(sites))
	for _, site := range sites {
		views = append(views, viewSite(site))
	}
	next := int64(0)
	if len(sites) > 0 {
		next = sites[len(sites)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": views, "next_after_id": next})
}

// handleCreateSite 创建站点。**只有这一个响应**含 secret 明文，之后任何接口都不再返回它。
func (s *Server) handleCreateSite(w http.ResponseWriter, r *http.Request) {
	var req createSiteRequest
	if !decodeAdmin(w, r, &req) {
		return
	}
	siteKey := req.SiteKey
	if siteKey == "" {
		// 不要求运维自己造公开标识：生成一个，避免站点之间 site_key 意外重复导致创建失败。
		generated, err := store.NewSiteKey()
		if err != nil {
			s.internal(w, "生成 site_key", err)
			return
		}
		siteKey = generated
	}
	policy := applyToUpdate(store.DefaultPolicy(), store.SiteUpdate{
		PowBits: req.PowBits, ChallengeTTL: reqTTL(req.ChallengeTTL), ResponseTTL: reqTTL(req.ResponseTTL),
		RateIPRPM: req.RateIPRPM, RateSiteRPM: req.RateSiteRPM, AllowMissingOrigin: req.AllowMissingOrigin,
	})
	site, secret, err := s.db.CreateSite(r.Context(), req.Name, siteKey, policy)
	if err != nil {
		s.auditFail(r.Context(), r, "site.create", "site:"+sanitizeTarget(siteKey))
		failFromErr(w, err)
		return
	}
	if len(req.Origins) > 0 {
		// 来源列表若有一条不合法就整批拒绝：站点已经建好，但宁可让调用方重来一次，
		// 也不能"部分生效"——半个白名单比没有白名单更危险。
		if _, err := s.db.ReplaceOrigins(r.Context(), site.ID, req.Origins); err != nil {
			s.auditFail(r.Context(), r, "site.create", "site:"+strconv.FormatInt(site.ID, 10))
			failFromErr(w, err)
			return
		}
	}
	target := "site:" + strconv.FormatInt(site.ID, 10)
	if err := s.auditOK(r.Context(), r, "site.create", target); err != nil {
		s.internal(w, "审计", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"site":   viewSite(site),
		"secret": secret, // 一次性展示；库中只有带 pepper 的摘要（DESIGN §6）
	})
}

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request) {
	id, ok := siteIDOf(w, r)
	if !ok {
		return
	}
	site, err := s.db.SiteByID(r.Context(), id)
	if err != nil {
		failFromErr(w, err)
		return
	}
	origins, err := s.db.Origins(r.Context(), id)
	if err != nil {
		s.internal(w, "列来源", err)
		return
	}
	secrets, err := s.db.ListSecrets(r.Context(), id)
	if err != nil {
		s.internal(w, "列凭据", err)
		return
	}
	now := s.now()
	sv := make([]secretView, 0, len(secrets))
	for _, x := range secrets {
		sv = append(sv, viewSecret(x, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site": viewSite(site), "origins": origins, "secrets": sv,
	})
}

// handleUpdateSite 改策略。**签发快照语义**：难度与 TTL 的改动不影响已签发的 challenge
// （那些参数存在账本行里并被签名覆盖），只影响此后新签的请求。
// 因此这里**不**提升站点版本——提升版本是"立即生效的安全动作"专用的机制。
func (s *Server) handleUpdateSite(w http.ResponseWriter, r *http.Request) {
	id, ok := siteIDOf(w, r)
	if !ok {
		return
	}
	var req updateSiteRequest
	if !decodeAdmin(w, r, &req) {
		return
	}
	cur, err := s.db.SiteByID(r.Context(), id)
	if err != nil {
		failFromErr(w, err)
		return
	}
	_ = cur
	site, err := s.db.UpdateSite(r.Context(), id, updateFrom(&req))
	if err != nil {
		s.auditFail(r.Context(), r, "site.update", "site:"+strconv.FormatInt(id, 10))
		failFromErr(w, err)
		return
	}
	if err := s.auditOK(r.Context(), r, "site.update", "site:"+strconv.FormatInt(id, 10)); err != nil {
		s.internal(w, "审计", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site)})
}

// handleSetEnabled 启停站点。启停都会提升站点版本，因此停用前签发的未消费 token
// 不会随着"重新启用"而复活（DESIGN §5.3）。
func (s *Server) handleSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := siteIDOf(w, r)
		if !ok {
			return
		}
		site, err := s.db.SetSiteEnabled(r.Context(), id, enabled)
		if err != nil {
			s.auditFail(r.Context(), r, actionForEnabled(enabled), "site:"+strconv.FormatInt(id, 10))
			failFromErr(w, err)
			return
		}
		if err := s.auditOK(r.Context(), r, actionForEnabled(enabled), "site:"+strconv.FormatInt(id, 10)); err != nil {
			s.internal(w, "审计", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site)})
	}
}

// handleDeleteSite 是**软吊销**：停用 + 提升版本，不删任何行。
// 硬删除会拆掉防重放账本（已消费的 response 行是"重放必失败"的证据），
// 也会触发外键约束——账本行的生命周期由 TTL 与清理任务决定，不由管理接口决定。
func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	id, ok := siteIDOf(w, r)
	if !ok {
		return
	}
	site, err := s.db.SetSiteEnabled(r.Context(), id, false)
	if err != nil {
		s.auditFail(r.Context(), r, "site.revoke", "site:"+strconv.FormatInt(id, 10))
		failFromErr(w, err)
		return
	}
	if err := s.auditOK(r.Context(), r, "site.revoke", "site:"+strconv.FormatInt(id, 10)); err != nil {
		s.internal(w, "审计", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": viewSite(site), "revoked": true})
}

func (s *Server) handleGetOrigins(w http.ResponseWriter, r *http.Request) {
	id, ok := siteIDOf(w, r)
	if !ok {
		return
	}
	if _, err := s.db.SiteByID(r.Context(), id); err != nil {
		failFromErr(w, err)
		return
	}
	origins, err := s.db.Origins(r.Context(), id)
	if err != nil {
		s.internal(w, "列来源", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"origins": origins})
}

// handlePutOrigins 全量替换允许来源。空数组是合法状态（= 不接受任何浏览器来源）。
// 规范化与精确匹配在 store.NormalizeOrigin 里：scheme + hostname + 有效端口，无通配符、无后缀匹配。
func (s *Server) handlePutOrigins(w http.ResponseWriter, r *http.Request) {
	id, ok := siteIDOf(w, r)
	if !ok {
		return
	}
	var req originsRequest
	if !decodeAdmin(w, r, &req) {
		return
	}
	target := "site:" + strconv.FormatInt(id, 10)
	origins, err := s.db.ReplaceOrigins(r.Context(), id, req.Origins)
	if err != nil {
		s.auditFail(r.Context(), r, "site.origins", target)
		failFromErr(w, err)
		return
	}
	if err := s.auditOK(r.Context(), r, "site.origins", target); err != nil {
		s.internal(w, "审计", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"origins": origins})
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	id, ok := siteIDOf(w, r)
	if !ok {
		return
	}
	secrets, err := s.db.ListSecrets(r.Context(), id)
	if err != nil {
		failFromErr(w, err)
		return
	}
	now := s.now()
	views := make([]secretView, 0, len(secrets))
	for _, x := range secrets {
		views = append(views, viewSecret(x, now))
	}
	// 响应里没有任何 secret 材料：只有公开 key_id 与时间（DESIGN §6、红线 4）。
	writeJSON(w, http.StatusOK, map[string]any{"secrets": views})
}

// handleAddSecret 新增一把验证凭据（旧的同时保留，用于灰度切换）。
func (s *Server) handleAddSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := siteIDOf(w, r)
	if !ok {
		return
	}
	target := "site:" + strconv.FormatInt(id, 10)
	secret, plain, err := s.db.AddSecret(r.Context(), id)
	if err != nil {
		s.auditFail(r.Context(), r, "secret.add", target)
		failFromErr(w, err)
		return
	}
	if err := s.auditOK(r.Context(), r, "secret.add", target+"/"+secret.KeyID); err != nil {
		s.internal(w, "审计", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"secret": plain, "key": viewSecret(secret, s.now()),
	})
}

// handleRotateSecret 轮换：建新密钥 + 按 transition_sec 处理旧密钥。
// transition_sec=0 立即撤销；>0 时旧密钥在该秒数内仍可用（上限 store.MaxTransitionSecond 秒）。
func (s *Server) handleRotateSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := siteIDOf(w, r)
	if !ok {
		return
	}
	var req revokeSecretRequest
	if !decodeAdmin(w, r, &req) {
		return
	}
	transition := time.Duration(0)
	if req.TransitionSec != nil {
		if *req.TransitionSec < 0 || *req.TransitionSec > store.MaxTransitionSecond {
			fail(w, http.StatusBadRequest, CodeBadRequest)
			return
		}
		transition = time.Duration(*req.TransitionSec) * time.Second
	}
	target := "site:" + strconv.FormatInt(id, 10)
	fresh, plain, retired, err := s.db.RotateSecret(r.Context(), id, transition, s.now())
	if err != nil {
		s.auditFail(r.Context(), r, "secret.rotate", target)
		failFromErr(w, err)
		return
	}
	if err := s.auditOK(r.Context(), r, "secret.rotate", target+"/"+fresh.KeyID); err != nil {
		s.internal(w, "审计", err)
		return
	}
	now := s.now()
	rv := make([]secretView, 0, len(retired))
	for _, x := range retired {
		rv = append(rv, viewSecret(x, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secret": plain, "key": viewSecret(fresh, now), "retired": rv,
	})
}

// handleRevokeSecret 撤销指定凭据。已撤销的行重复撤销返回当前状态（幂等，脚本可重跑）。
func (s *Server) handleRevokeSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := siteIDOf(w, r)
	if !ok {
		return
	}
	keyID := r.PathValue("keyID")
	if keyID == "" || len(keyID) > 64 {
		fail(w, http.StatusBadRequest, CodeBadRequest)
		return
	}
	var req revokeSecretRequest
	if !decodeAdmin(w, r, &req) {
		return
	}
	at := s.now().UTC()
	if req.TransitionSec != nil {
		if *req.TransitionSec < 0 || *req.TransitionSec > store.MaxTransitionSecond {
			fail(w, http.StatusBadRequest, CodeBadRequest)
			return
		}
		at = at.Add(time.Duration(*req.TransitionSec) * time.Second)
	}
	target := "site:" + strconv.FormatInt(id, 10) + "/" + sanitizeTarget(keyID)
	got, err := s.db.RevokeSecret(r.Context(), id, keyID, at)
	if err != nil {
		s.auditFail(r.Context(), r, "secret.revoke", target)
		failFromErr(w, err)
		return
	}
	if err := s.auditOK(r.Context(), r, "secret.revoke", target); err != nil {
		s.internal(w, "审计", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": viewSecret(got, s.now())})
}

func (s *Server) handleListAdminKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.db.ListAdminKeys(r.Context())
	if err != nil {
		s.internal(w, "列管理凭据", err)
		return
	}
	now := s.now()
	views := make([]adminKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, viewAdminKey(k, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"admin_keys": views})
}

// handleAddAdminKey 生成并登记一把管理凭据；明文只在此返回一次。
func (s *Server) handleAddAdminKey(w http.ResponseWriter, r *http.Request) {
	var req addAdminKeyRequest
	if !decodeAdmin(w, r, &req) {
		return
	}
	key, plain, err := s.db.NewAdminKeyWithID(r.Context(), req.Label)
	if err != nil {
		s.auditFail(r.Context(), r, "admin_key.add", sanitizeTarget(req.Label))
		failFromErr(w, err)
		return
	}
	if err := s.auditOK(r.Context(), r, "admin_key.add", "admin_key:"+key.KeyID); err != nil {
		s.internal(w, "审计", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"admin_key": plain, "key": viewAdminKey(key, s.now()),
	})
}

// handleRevokeAdminKey 撤销管理凭据。**不允许撤销调用者自己正在用的那把**之外还允许：
// 允许——否则运维把自己关在门外只能靠重启时的 bootstrap 环境变量救，
// 而 bootstrap 在库里已有记录时是不生效的（DESIGN §6）。因此响应里明确提示下一步。
func (s *Server) handleRevokeAdminKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("keyID")
	if keyID == "" || len(keyID) > 64 {
		fail(w, http.StatusBadRequest, CodeBadRequest)
		return
	}
	var req revokeSecretRequest
	if !decodeAdmin(w, r, &req) {
		return
	}
	at := s.now().UTC()
	if req.TransitionSec != nil {
		if *req.TransitionSec < 0 || *req.TransitionSec > store.MaxTransitionSecond {
			fail(w, http.StatusBadRequest, CodeBadRequest)
			return
		}
		at = at.Add(time.Duration(*req.TransitionSec) * time.Second)
	}
	a, err := actor(r)
	if err != nil {
		s.internal(w, "鉴权上下文", err)
		return
	}
	target := "admin_key:" + sanitizeTarget(keyID)
	got, err := s.db.RevokeAdminKey(r.Context(), keyID, at)
	if err != nil {
		s.auditFail(r.Context(), r, "admin_key.revoke", target)
		failFromErr(w, err)
		return
	}
	if err := s.auditOK(r.Context(), r, "admin_key.revoke", target); err != nil {
		s.internal(w, "审计", err)
		return
	}
	body := map[string]any{"key": viewAdminKey(got, s.now())}
	// 撤销自己正在用的这把（且已立即失效）要给提示：库里已有管理密钥记录时，
	// bootstrap 环境变量不再生效，把自己关在门外就只能靠重新登记一把才能进来了。
	if got.KeyID == a && !got.RevokedAt.IsZero() && !got.RevokedAt.After(s.now()) {
		body["warning"] = "self-revoked"
	}
	writeJSON(w, http.StatusOK, body)
}

// handleAudit 倒序游标分页读审计。只读操作不写审计（否则列表本身会无限自我追加）。
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	beforeID := int64(0)
	if v := r.URL.Query().Get("before_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, CodeBadRequest)
			return
		}
		beforeID = n
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, CodeBadRequest)
			return
		}
		limit = n
	}
	entries, err := s.db.ListAudit(r.Context(), beforeID, limit)
	if err != nil {
		failFromErr(w, err)
		return
	}
	views := make([]auditView, 0, len(entries))
	for _, e := range entries {
		views = append(views, auditView{
			ID: e.ID, Ts: e.Timestamp.UTC().Format(time.RFC3339),
			Actor: e.Actor, Action: e.Action, Target: e.Target, Success: e.Success,
		})
	}
	next := int64(0)
	if len(entries) > 0 {
		next = entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": views, "next_before_id": next})
}

// --- 小工具 ---

func actionForEnabled(enabled bool) string {
	if enabled {
		return "site.enable"
	}
	return "site.disable"
}

// sanitizeTarget 把调用方可控的标识压进"可见、无空白"的形状再写审计：
// 审计行会被日志与人工阅读，控制字符等于注入。
func sanitizeTarget(s string) string {
	r := []rune{}
	for i, c := range s {
		if i >= 64 {
			break
		}
		if c < 0x20 || c == 0x7f {
			r = append(r, '_')
			continue
		}
		r = append(r, c)
	}
	return string(r)
}

func (s *Server) internal(w http.ResponseWriter, where string, err error) {
	s.log.Error("admin 内部错误", "where", where, "err", errKind(err))
	fail(w, http.StatusInternalServerError, CodeInternal)
}

// reqTTL 把秒数指针转成时长指针（nil 表示"不改"）。
func reqTTL(sec *int64) *time.Duration {
	if sec == nil {
		return nil
	}
	d := time.Duration(*sec) * time.Second
	return &d
}

// applyToUpdate 用于创建路径：在默认策略上叠加显式给定的字段。
func applyToUpdate(base store.SitePolicy, u store.SiteUpdate) store.SitePolicy {
	if u.PowBits != nil {
		base.PowBits = *u.PowBits
	}
	if u.ChallengeTTL != nil {
		base.ChallengeTTL = *u.ChallengeTTL
	}
	if u.ResponseTTL != nil {
		base.ResponseTTL = *u.ResponseTTL
	}
	if u.RateIPRPM != nil {
		base.RateIPRPM = *u.RateIPRPM
	}
	if u.RateSiteRPM != nil {
		base.RateSiteRPM = *u.RateSiteRPM
	}
	if u.AllowMissingOrigin != nil {
		base.AllowMissingOrigin = *u.AllowMissingOrigin
	}
	return base
}
