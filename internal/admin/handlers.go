package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

// --- 视图 ---

type siteView struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	SiteKey string `json:"site_key"`
	Enabled bool   `json:"enabled"`
	Version int64  `json:"version"`
	PowBits uint8  `json:"pow_bits"`
	// 验证模式与题目策略（阶段 3A）。generator_id 是公开标识，不含任何凭据。
	VerifyMode         string   `json:"verify_mode"`
	GeneratorID        string   `json:"generator_id,omitempty"`
	GeneratorPool      []string `json:"generator_pool,omitempty"`
	Difficulty         int      `json:"difficulty"`
	ChallengeTTL       int64    `json:"challenge_ttl_sec"`
	ResponseTTL        int64    `json:"response_ttl_sec"`
	RateIPRPM          int      `json:"rate_ip_rpm"`
	RateSiteRPM        int      `json:"rate_site_rpm"`
	AllowMissingOrigin bool     `json:"allow_missing_origin"`
	CreatedAt          string   `json:"created_at"`
}

func viewSite(s *store.Site) siteView {
	return siteView{
		ID: s.ID, Name: s.Name, SiteKey: s.SiteKey, Enabled: s.Enabled, Version: s.Version,
		PowBits:            s.PowBits,
		VerifyMode:         s.Policy.VerifyMode,
		GeneratorID:        s.Policy.GeneratorID,
		GeneratorPool:      append([]string(nil), s.Policy.GeneratorPool...),
		Difficulty:         s.Policy.Difficulty,
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
	Name               string    `json:"name"`
	VerifyMode         *string   `json:"verify_mode"`
	GeneratorID        *string   `json:"generator_id"`
	GeneratorPool      *[]string `json:"generator_pool"`
	Difficulty         *int      `json:"difficulty"`
	SiteKey            string    `json:"site_key"`
	Origins            []string  `json:"origins"`
	PowBits            *uint8    `json:"pow_bits"`
	ChallengeTTL       *int64    `json:"challenge_ttl_sec"`
	ResponseTTL        *int64    `json:"response_ttl_sec"`
	RateIPRPM          *int      `json:"rate_ip_rpm"`
	RateSiteRPM        *int      `json:"rate_site_rpm"`
	AllowMissingOrigin *bool     `json:"allow_missing_origin"`
}

type updateSiteRequest struct {
	// Name 是显示名：与创建时同一套校验（1..64 个可见字符、站内唯一），nil 表示不改。
	Name *string `json:"name"`
	// SiteKey 故意不可改：它是公开标识，同时是签发时绑定与来源判定的锚点，
	// 改它等于换站点，而"换站点"会让业务后端里写死的 site_key 静默失效。
	VerifyMode         *string   `json:"verify_mode"`
	GeneratorID        *string   `json:"generator_id"`
	GeneratorPool      *[]string `json:"generator_pool"`
	Difficulty         *int      `json:"difficulty"`
	PowBits            *uint8    `json:"pow_bits"`
	ChallengeTTL       *int64    `json:"challenge_ttl_sec"`
	ResponseTTL        *int64    `json:"response_ttl_sec"`
	RateIPRPM          *int      `json:"rate_ip_rpm"`
	RateSiteRPM        *int      `json:"rate_site_rpm"`
	AllowMissingOrigin *bool     `json:"allow_missing_origin"`
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
	u := store.SiteUpdate{
		Name:          req.Name,
		VerifyMode:    req.VerifyMode,
		GeneratorID:   req.GeneratorID,
		GeneratorPool: req.GeneratorPool,
		Difficulty:    req.Difficulty,
	}
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
	u.GeneratorPool = req.GeneratorPool
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
		VerifyMode: req.VerifyMode, GeneratorID: req.GeneratorID,
		GeneratorPool: req.GeneratorPool, Difficulty: req.Difficulty,
	})
	if err := s.checkModePolicy(policy.VerifyMode, policy.GeneratorID, policy.GeneratorPool, policy.Difficulty); err != nil {
		fail(w, http.StatusBadRequest, CodeBadRequest)
		return
	}
	a, err := actor(r)
	if err != nil {
		s.internal(w, "鉴权上下文", err)
		return
	}
	site, secret, err := s.db.CreateSiteAudited(r.Context(), req.Name, siteKey, policy, req.Origins, a)
	if err != nil {
		s.auditFail(r.Context(), r, "site.create", "site:"+sanitizeTarget(siteKey))
		failFromErr(w, err)
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
	next := applyToUpdate(cur.Policy, updateFrom(&req))
	if err := s.checkModePolicy(next.VerifyMode, next.GeneratorID, next.GeneratorPool, next.Difficulty); err != nil {
		fail(w, http.StatusBadRequest, CodeBadRequest)
		return
	}
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
	a, err := actor(r)
	if err != nil {
		s.internal(w, "鉴权上下文", err)
		return
	}
	target := "site:" + strconv.FormatInt(id, 10)
	secret, plain, err := s.db.AddSecretAudited(r.Context(), id, a)
	if err != nil {
		s.auditFail(r.Context(), r, "secret.add", target)
		failFromErr(w, err)
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
	a, err := actor(r)
	if err != nil {
		s.internal(w, "鉴权上下文", err)
		return
	}
	target := "site:" + strconv.FormatInt(id, 10)
	fresh, plain, retired, err := s.db.RotateSecretAudited(r.Context(), id, transition, s.now(), a)
	if err != nil {
		s.auditFail(r.Context(), r, "secret.rotate", target)
		failFromErr(w, err)
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
	a, err := actor(r)
	if err != nil {
		s.internal(w, "鉴权上下文", err)
		return
	}
	key, plain, err := s.db.NewAdminKeyWithIDAudited(r.Context(), req.Label, a)
	if err != nil {
		s.auditFail(r.Context(), r, "admin_key.add", sanitizeTarget(req.Label))
		failFromErr(w, err)
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
	if u.VerifyMode != nil {
		base.VerifyMode = *u.VerifyMode
	}
	if u.GeneratorID != nil {
		base.GeneratorID = *u.GeneratorID
	}
	if u.GeneratorPool != nil {
		base.GeneratorPool = append([]string(nil), (*u.GeneratorPool)...)
	}
	if u.Difficulty != nil {
		base.Difficulty = *u.Difficulty
	}
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

// --- 阶段 3A：模式、生成器与题池观测 ---

// handleGenerators 返回本进程登记的题型与协议版本。
// 管理面写站点时必须从这个列表里选，否则站点会指向一个运行时不存在的题型（只能拿 503）。
func (s *Server) handleGenerators(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, id := range s.gens.IDs() {
		g, err := s.gens.Lookup(id)
		if err != nil {
			continue
		}
		// 自我标记必须是**默认拒绝**的：新登记的题型若没有明确的生产结论，
		// 就只能落在 experimental:true + production_ready:false 上（DESIGN §8.5）。
		exp, ready := generatorLifecycle(id)
		out = append(out, map[string]any{
			"id": id, "version": g.Version(), "media": g.Media(),
			"experimental":     exp,
			"production_ready": ready,
			"note":             noteForGenerator(id),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"generators": out, "max_difficulty": generator.MaxDifficulties})
}

// generatorLifecycle 返回 (experimental, production_ready)。
//
// 所有题型现在都是 false/true 的**反面**：都不生产可用。写在这里的意义是
// "未经评测不得改成 true"——评测通过也只证明"测过"，不等同于生产安全认证（ADR-002）。
func generatorLifecycle(id string) (experimental, productionReady bool) {
	switch id {
	case generator.ArithID, generator.TextImageID, generator.TextImageV2ID, generator.TextImageV3ID,
		generator.ShapeGridID, generator.ShapeCountID, generator.ParallaxID, generator.BankShotID,
		generator.AccessibleChoiceID, generator.EnergyWindowID, generator.PipeConnectID,
		generator.IntersectionPointID:
		return true, false
	default:
		// 没登记结论的题型：按最保守的一档报，绝不让"忘了写"变成"看起来可用"
		return true, false
	}
}

func noteForGenerator(id string) string {
	switch id {
	case generator.ArithID:
		return "工程基线题型：题面可被脚本直接求值，不抗自动解题，仅用于验证题池与提交生命周期"
	case generator.TextImageID:
		return "实验性视觉题型：图片中可见字符本来就要给答题者看，不具备密码学保密性；" +
			"已有固定工具的历史 OCR 基线：低/中/高档整串通过率约 96%/31%/3%，" +
			"这些历史数字不代表当前候选、跨工具、多模态或真人验证已完成；" +
			"仅用于验证图片资产的生成-存储-下发-判分链路"
	case generator.TextImageV2ID:
		return "强形变视觉题型 v2（8 字符，旋转/剪切/缩放/重叠/穿字线）：与 v1 共用判分路径；" +
			"图片中可见字符本来就要给答题者看，不具备密码学保密性；" +
			"当前候选的 OCR 基线结果以评测收据为准，历史 v1 数字不能外推；" +
			"跨工具、多模态、真人可读性与抗 AI 仍未验收，production_ready 保持 false"
	case generator.TextImageV3ID:
		return "抽象文字题型 v3（6 字符，模糊/正弦位移/擦除点/低对比，允许 1 个字符替换）：" +
			"逐字容差 verifier 只降低“认错一个字”的误伤，不改变无密钥时的安全边界；" +
			"图片中可见字符本来就要给答题者看，不具备密码学保密性；" +
			"M4/OCR 基线、多模态、人工可读性均未验收（NOT_RUN），production_ready 保持 false"
	case generator.ShapeGridID:
		return "图形点选题（3×3，点选所有满足判据的格子）：非纯文字输入；" +
			"图形对机器视觉并不难，M4 未测，不声称抗 AI；" +
			"人工可读性、辅助技术与真机移动端未验收，production_ready 保持 false"
	case generator.ShapeCountID:
		return "图形计数题（两类目标各 1..6 个，另 0..2 个干扰图形）：非文字识别，" +
			"但机器视觉可以直接数；M4 未测，不声称抗 AI；" +
			"人工可读性、辅助技术与真机移动端未验收，production_ready 保持 false"
	case generator.ParallaxID:
		return "时序运动视差题（4 位数字分散在 8–12 帧中，单帧只有部分笔画）：" +
			"M1/M2/M3 已计算，M4/OCR/多模态/人工可读性均未验收（NOT_RUN），" +
			"不声称抗 AI；辅助技术与真机移动端未验收，production_ready 保持 false"
	case generator.BankShotID:
		return "一次反弹物理直觉题（0.1° 量化，TSV 角度容差）：" +
			"M3 单次 ≤1.14%（d1，41/3600），3 次 ≤3.4%；M4 未测，不声称抗 AI；" +
			"人工可读性、辅助技术与真机移动端未验收，production_ready 保持 false"
	case generator.AccessibleChoiceID:
		return "无障碍等价通道的纯文本单选题（12 选 1，唯一偶数）：" +
			"只允许 accessibility:true + 站点池显式包含 + 24h 1 次；" +
			"M4/人工可读性/辅助技术实测均 NOT_RUN，不声称抗 AI；production_ready 保持 false"
	case generator.EnergyWindowID:
		return "瞬态时机题（36 帧雪碧图，TSV 时间桶容差；目标只在 400 个可行桶内）：" +
			"单次接受域 13/400≈3.25%，3 次上界≈9.4%；M4/人工可读性/辅助技术 NOT_RUN，" +
			"不声称抗 AI；reduced_motion 下自动排除；production_ready 保持 false"
	case generator.PipeConnectID:
		return "拓扑连通题（4×4 网格，点选主路径 2 个断点；断点与死路外观相同）：" +
			"M3 按空候选格 C(E,2) 算，d1 最少 E=9 → 1/36≈2.78%、3 次≈8.1%，d5 最少 E=13 → 1/78≈1.28%；" +
			"M4/人工可读性/辅助技术 NOT_RUN，不声称抗 AI；production_ready 保持 false"
	case generator.IntersectionPointID:
		return "交点外推题（两条虚线，点击延长线交点；point 量化 + 3×3 邻格 TSV，交点不画标记）：" +
			"M3 单次 d1 9/351≈2.56%、3 次≈7.5%，d5 9/1404≈0.64%；" +
			"M4/人工可读性/辅助技术 NOT_RUN，不声称抗 AI；production_ready 保持 false"

	default:
		return ""
	}
}

// handlePoolStatus 是**只读**水位观测。返回的全是计数与公开元数据：
// 不含 verifier、不含答案、不含内部随机数，也不含题面文本。
func (s *Server) handlePoolStatus(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.PoolStats(r.Context(), s.now())
	if err != nil {
		s.internal(w, "池统计", err)
		return
	}
	buckets := make([]map[string]any, 0, len(rows))
	for _, b := range rows {
		buckets = append(buckets, map[string]any{
			"generator_id": b.GeneratorID, "version": b.Version, "difficulty": b.Difficulty,
			"ready": b.Ready, "assigned": b.Assigned, "terminal": b.Terminal, "total": b.Total,
			"oldest_ready_expires": b.OldestReady,
			// 字节是**聚合值**：统计绝不把 asset_blob 读进 Go 内存（DESIGN §13 的图片纪律）。
			"ready_asset_bytes": b.ReadyAssetBytes, "total_asset_bytes": b.TotalAssetBytes,
		})
	}
	body := map[string]any{"buckets": buckets}
	// 全局图片预算的两个量：已用字节与配置上限。运维判断"池还能不能补图"看的是这两个，
	// 不是条数——一条图片题的字节可以是算术题的上百倍。
	used, err := s.db.ImageAssetBytes(r.Context())
	if err != nil {
		s.internal(w, "图片字节统计", err)
		return
	}
	body["image"] = map[string]any{
		"asset_bytes":            used,
		"budget_bytes":           s.imageBudget,
		"per_question_max_bytes": store.MaxAssetBytes,
	}
	if s.pool != nil {
		st := s.pool.Stats()
		body["worker"] = map[string]any{
			"generated": st.Generated, "failed": st.Failed, "rejected": st.Rejected,
			"last_error": st.LastErr, "last_tick": st.LastTick,
			// 明确告诉调用方这些数会归零：它是可观测性，不是账本
			"restart_resets": st.RestartResets,
		}
	} else {
		body["worker"] = map[string]any{"enabled": false}
	}
	writeJSON(w, http.StatusOK, body)
}

// checkModePolicy 在写入前验证模式与题型的组合：
// 库里存一个本进程没登记的 generator_id，等于给站点配了一条只会返回 503 的策略。
func (s *Server) checkModePolicy(mode, genID string, pool []string, difficulty int) error {
	if mode == "" || mode == store.ModePowOnly {
		return nil
	}
	if mode != store.ModePowQuestion {
		return errors.New("admin: 未知验证模式")
	}
	ids := pool
	if len(ids) == 0 && genID != "" {
		ids = []string{genID}
	}
	if len(ids) == 0 {
		return errors.New("admin: pow_question 缺少生成器")
	}
	for _, id := range ids {
		g, err := s.gens.Lookup(id)
		if err != nil {
			return err
		}
		if g.Version() == "" {
			return errors.New("admin: 生成器缺少协议版本")
		}
	}
	if difficulty < 1 || difficulty > generator.MaxDifficulties {
		return errors.New("admin: 难度档越界")
	}
	return nil
}
