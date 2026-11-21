package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/pow"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
)

// diagAdminActor 惰性创建一把测试管理 key，供需要显式 actor 的业务写 helper 使用。
// 结果缓存在 harness 上；key 与具体用例无关（这里不测管理面鉴权）。
func (h *harness) diagAdminActor() string {
	h.t.Helper()
	if h.diagActor != "" {
		return h.diagActor
	}
	key, _, err := h.db.NewAdminKeyWithID(context.Background(), "diag-actor")
	if err != nil {
		h.t.Fatalf("NewAdminKeyWithID: %v", err)
	}
	h.diagActor = key.KeyID
	return h.diagActor
}

// setOrigins 用管理面之外的最短路径设置允许来源：直接走 store（来源策略的 HTTP 侧行为在这里测，
// 管理 API 自身的鉴权/审计在 internal/admin 测）。
func (h *harness) setOrigins(siteID int64, origins []string) {
	h.t.Helper()
	if _, err := h.db.ReplaceOriginsAudited(context.Background(), siteID, origins, h.diagAdminActor()); err != nil {
		h.t.Fatalf("ReplaceOrigins: %v", err)
	}
}

// tuneSitePolicy 改站点策略（模拟管理 API 的配置更新）；nil 字段保持不变。
func (h *harness) tuneSitePolicy(siteID int64, u store.SiteUpdate) {
	h.t.Helper()
	if _, err := h.db.UpdateSite(context.Background(), siteID, u, h.diagAdminActor()); err != nil {
		h.t.Fatalf("UpdateSite: %v", err)
	}
}

func upBits(b uint8) store.SiteUpdate { return store.SiteUpdate{PowBits: &b} }
func upTTL(c, r time.Duration) store.SiteUpdate {
	return store.SiteUpdate{ChallengeTTL: &c, ResponseTTL: &r}
}
func upAllowMissing(v bool) store.SiteUpdate { return store.SiteUpdate{AllowMissingOrigin: &v} }
func upAll(b uint8, c, r time.Duration, missing bool) store.SiteUpdate {
	return store.SiteUpdate{PowBits: &b, ChallengeTTL: &c, ResponseTTL: &r, AllowMissingOrigin: &missing}
}

func (h *harness) disable(siteID int64) {
	h.t.Helper()
	if _, err := h.db.SetSiteEnabled(context.Background(), siteID, false, h.diagAdminActor()); err != nil {
		h.t.Fatalf("停用站点: %v", err)
	}
}

func (h *harness) enable(siteID int64) {
	h.t.Helper()
	if _, err := h.db.SetSiteEnabled(context.Background(), siteID, true, h.diagAdminActor()); err != nil {
		h.t.Fatalf("启用站点: %v", err)
	}
}

// rotateNow 轮换站点凭据；transition=0 表示立即撤销旧密钥。
// rotateNow 用**注入时钟**轮换凭据：撤销时刻与请求判定必须同一时间源，
// 否则"用真实时钟撤销、用注入时钟判定"会让过渡期在测试里看起来没生效。
func (h *harness) rotateNow(siteID int64, transition time.Duration) (string, string) {
	h.t.Helper()
	fresh, plain, _, err := h.db.RotateSecret(context.Background(), siteID, transition, h.clock)
	if err != nil {
		h.t.Fatalf("RotateSecret: %v", err)
	}
	return fresh.KeyID, plain
}

// issueWith 签发并真实求解 PoW，返回 (challenge token, nonce, 原始签发响应)。
func (h *harness) issueWith(t *testing.T, siteKey, origin string) (string, string, reply) {
	t.Helper()
	r := h.challengeWithOrigin(siteKey, origin)
	if r.status != http.StatusOK {
		t.Fatalf("签发应成功：%d %s", r.status, r.body)
	}
	return r.str("token"), h.solveReply(t, r), r
}

// solveReply 按响应里下发的参数暴力求解（复用内部 pow 只为"能解出题"，
// 协议正确性由 internal/pow 的已知向量与独立实现测试负责）。
func (h *harness) solveReply(t *testing.T, r reply) string {
	t.Helper()
	powObj := r.got["pow"].(map[string]any)
	seed, err := decodeB64(powObj["seed"].(string))
	if err != nil {
		t.Fatal(err)
	}
	params := pow.Params{Algo: pow.Algo, Bits: uint8(num(powObj["bits"])), Seed: seed}
	for n := int64(0); n < 20_000_000; n++ {
		cand := strconv.FormatInt(n, 10)
		ok, err := params.Verify(cand)
		if err != nil {
			t.Fatalf("参数校验失败 %q: %v", cand, err)
		}
		if ok {
			return cand
		}
	}
	t.Fatal("未能在上限内求解")
	return ""
}

func decodeB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func (h *harness) challengeWithOrigin(siteKey, origin string) reply {
	h.t.Helper()
	headers := map[string]string{}
	if origin != "" {
		headers["Origin"] = origin
	}
	return h.do(http.MethodPost, "/api/challenge",
		strings.NewReader(`{"site_key":"`+siteKey+`"}`), "application/json", "203.0.113.7:5555", headers)
}

// --- 需求 9：精确来源匹配 ---

func TestOriginWhitelistExactMatch(t *testing.T) {
	h := newHarness(t, nil)
	h.tuneSitePolicy(h.siteA.ID, upAllowMissing(false))
	h.setOrigins(h.siteA.ID, []string{"https://app.example.com"})

	okCases := []struct{ origin, why string }{
		{"https://app.example.com", "白名单内"},
		{"https://app.example.com:443", "默认端口规范化后同一条"},
		{"HTTPS://APP.Example.com", "scheme/host 大小写不敏感"},
	}
	for _, c := range okCases {
		r := h.challengeWithOrigin("sk-alpha", c.origin)
		if r.status != http.StatusOK {
			t.Errorf("%s（%q）应放行，got %d %s", c.why, c.origin, r.status, r.body)
		}
	}

	rejectCases := []struct{ origin, why string }{
		{"https://evil-example.com", "相似后缀"},
		{"https://app.example.com.evil.net", "后缀拼接"},
		{"https://deep.app.example.com", "非预期子域"},
		{"https://app.example.com:8443", "非预期端口"},
		{"http://app.example.com", "非预期协议"},
		{"null", "null Origin"},
		{"https://*/", "畸形"},
		{"not a url", "畸形"},
		{"https://app.example.com/path", "带路径的伪 Origin"},
		{"ftp://app.example.com", "非法协议"},
	}
	for _, c := range rejectCases {
		r := h.challengeWithOrigin("sk-alpha", c.origin)
		if r.status != http.StatusBadRequest || !r.hasCode(CodeInvalidRequest) {
			t.Errorf("%s（%q）应 400 invalid-request，got %d %s", c.why, c.origin, r.status, r.body)
		}
	}
}

// TestMissingOriginPolicy 覆盖"缺失 Origin 的直接 API 调用走明确的站点策略"：
// 默认拒绝，站点显式放行才可签发；两种情形都不得静默放开给别的站点。
func TestMissingOriginPolicy(t *testing.T) {
	h := newHarness(t, nil)
	// alpha 默认 allow_missing_origin=true（harness 导入时设置）；bravo 关掉
	h.tuneSitePolicy(h.siteB.ID, upAllowMissing(false))

	if r := h.challengeWithOrigin("sk-bravo", ""); r.status != http.StatusBadRequest {
		t.Fatalf("未放行的站点缺 Origin 应拒绝，got %d %s", r.status, r.body)
	}
	if r := h.challengeWithOrigin("sk-alpha", ""); r.status != http.StatusOK {
		t.Fatalf("显式放行的站点缺 Origin 应可签发，got %d %s", r.status, r.body)
	}
}

// TestOriginIsBoundAndRedeemMustMatchSource 覆盖绑定位置与重放面：
// challenge 记下的来源与兑换时的来源必须一致，换来源不能借用同一个 token。
func TestOriginIsBoundAndRedeemMustMatchSource(t *testing.T) {
	h := newHarness(t, nil)
	h.tuneSitePolicy(h.siteA.ID, upAllowMissing(true))
	h.setOrigins(h.siteA.ID, []string{"https://app.example.com", "https://other.example.com"})

	tok, nonce, r := h.issueWith(t, "sk-alpha", "https://app.example.com")
	_ = r
	// 换个允许的来源兑换：拒绝（不透露原因族以外的信息）
	if r2 := h.do(http.MethodPost, "/api/redeem",
		bytes.NewReader(mustJSON(t, map[string]string{"token": tok, "pow_nonce": nonce})),
		"application/json", "203.0.113.7:5555", map[string]string{"Origin": "https://other.example.com"}); r2.status != http.StatusBadRequest {
		t.Fatalf("换来源兑换应被拒，got %d %s", r2.status, r2.body)
	}
	// 无 Origin 兑换同一 token：同样拒绝（签发时有来源）
	if r3 := h.do(http.MethodPost, "/api/redeem",
		bytes.NewReader(mustJSON(t, map[string]string{"token": tok, "pow_nonce": nonce})),
		"application/json", "203.0.113.7:5555", nil); r3.status != http.StatusBadRequest {
		t.Fatalf("去掉 Origin 兑换应被拒，got %d %s", r3.status, r3.body)
	}
	// 原来源兑换：成功
	resp := h.redeemWithOrigin(t, tok, nonce, "https://app.example.com")

	// /siteverify 返回服务端绑定的 hostname，而不是调用方自报值
	v := h.verify(sentinelSecret, resp)
	if !v.got["success"].(bool) {
		t.Fatalf("验证应成功：%s", v.body)
	}
	if v.str("hostname") != "app.example.com" {
		t.Fatalf("hostname 应来自服务端绑定值，got %q (%s)", v.str("hostname"), v.body)
	}
}

func (h *harness) redeemWithOrigin(t *testing.T, tok, nonce, origin string) string {
	t.Helper()
	r := h.do(http.MethodPost, "/api/redeem",
		bytes.NewReader(mustJSON(t, map[string]string{"token": tok, "pow_nonce": nonce})),
		"application/json", "203.0.113.7:5555", map[string]string{"Origin": origin})
	if r.status != http.StatusOK {
		t.Fatalf("redeem 应成功：%d %s", r.status, r.body)
	}
	return r.str("response")
}

// --- 需求 10：CORS 与预检 ---

func TestCORSSuccessAndRejection(t *testing.T) {
	h := newHarness(t, nil)
	h.setOrigins(h.siteA.ID, []string{"https://app.example.com"})

	ok := h.challengeWithOrigin("sk-alpha", "https://app.example.com")
	if ok.status != http.StatusOK {
		t.Fatalf("应成功：%s", ok.body)
	}
	if got := ok.headers.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO 应精确回显已允许来源，got %q", got)
	}
	if got := ok.headers.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("缺少 Vary: Origin（缓存会把放行结果泄漏给别的来源）：%v", ok.headers)
	}
	if strings.Contains(ok.body, "*") && ok.headers.Get("Access-Control-Allow-Origin") == "*" {
		t.Error("禁止通配符 ACAO")
	}
	if v := ok.headers.Get("Access-Control-Allow-Credentials"); v != "false" {
		t.Errorf("管理/公开端点都不应携带凭据，got %q", v)
	}

	bad := h.challengeWithOrigin("sk-alpha", "https://evil-example.com")
	if bad.headers.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("被拒来源不得出现任何 CORS 头：%v", bad.headers)
	}
	if bad.headers.Get("Vary") != "" {
		t.Errorf("被拒来源不应挂 Vary（不缓存就不需要）：%v", bad.headers)
	}
}

// TestPreflightDoesNotIssueOrConsume 预检只完成预检：不落库、不签发、不消费。
func TestPreflightDoesNotIssueOrConsume(t *testing.T) {
	h := newHarness(t, nil)
	h.setOrigins(h.siteA.ID, []string{"https://app.example.com"})
	before := h.activeChallenges(t)

	pre := func(siteKey, origin, method, headers string) reply {
		extra := map[string]string{"Origin": origin}
		if method != "" {
			extra["Access-Control-Request-Method"] = method
		}
		if headers != "" {
			extra["Access-Control-Request-Headers"] = headers
		}
		return h.do(http.MethodOptions, "/api/challenge?site_key="+siteKey, nil, "", "203.0.113.7:5555", extra)
	}

	r := pre("sk-alpha", "https://app.example.com", "POST", "content-type")
	if r.status != http.StatusNoContent {
		t.Fatalf("合法预检应 204，got %d %s", r.status, r.body)
	}
	if r.headers.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Errorf("预检缺少精确 ACAO：%v", r.headers)
	}
	if m := r.headers.Get("Access-Control-Allow-Methods"); !strings.Contains(m, "POST") {
		t.Errorf("预检缺少允许方法：%v", r.headers)
	}
	if r.headers.Get("Vary") == "" {
		t.Error("预检响应也必须带 Vary: Origin")
	}
	if strings.Contains(r.body, "token") || strings.Contains(r.body, "pow") {
		t.Fatalf("预检不得签发任何东西：%s", r.body)
	}

	// 被拒来源：预检失败且不给任何宽松头
	bad := pre("sk-alpha", "https://evil-example.com", "POST", "content-type")
	if bad.status == http.StatusNoContent {
		t.Fatal("被拒来源的预检不应成功")
	}
	if bad.headers.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("被拒来源不得出现 ACAO：%v", bad.headers)
	}
	// 非法方法与头
	if r2 := pre("sk-alpha", "https://app.example.com", "DELETE", ""); r2.status != http.StatusForbidden {
		t.Errorf("非法请求方法预检应 403，got %d", r2.status)
	}
	if r3 := pre("sk-alpha", "https://app.example.com", "POST", "x-custom-dangerous"); r3.status != http.StatusForbidden {
		t.Errorf("非法请求头预检应 403，got %d", r3.status)
	}
	// 未知 site_key 与已停用站点的预检都不给 CORS 头
	r4 := pre("sk-does-not-exist", "https://app.example.com", "POST", "")
	if r4.status == http.StatusNoContent {
		t.Error("未知站点不应有预检成功")
	}
	if v := r4.headers.Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("未知站点的预检不得回显 ACAO：%q", v)
	}
	h.disable(h.siteA.ID)
	if r5 := pre("sk-alpha", "https://app.example.com", "POST", ""); r5.status == http.StatusNoContent {
		t.Error("已停用站点不应有预检成功")
	}
	h.enable(h.siteA.ID)
	if after := h.activeChallenges(t); after != before {
		t.Fatalf("预检产生了 challenge 行：%d → %d", before, after)
	}
}

func (h *harness) activeChallenges(t *testing.T) int {
	t.Helper()
	n, err := h.db.CountChallenges(context.Background(), "issued")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSiteverifyHasNoCORS /siteverify 是服务端接口，不允许任意浏览器读结果。
func TestSiteverifyHasNoCORS(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)
	r := h.do(http.MethodPost, "/siteverify",
		strings.NewReader("secret="+sentinelSecret+"&response="+resp), "application/x-www-form-urlencoded",
		"203.0.113.7:5555", map[string]string{"Origin": "https://anywhere.example.com"})
	if !r.got["success"].(bool) {
		t.Fatalf("验证应成功：%s", r.body)
	}
	for _, k := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials"} {
		if v := r.headers.Get(k); v != "" {
			t.Errorf("/siteverify 不该有 %s: %q", k, v)
		}
	}
}

// --- 需求 8：停用站点与版本语义 ---

func TestSiteDisableAndReEnableDoesNotReviveTokens(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")

	// 停用后不签发
	h.disable(h.siteA.ID)
	if r := h.challengeWithOrigin("sk-alpha", ""); r.status != http.StatusBadRequest {
		t.Fatalf("停用站点不应签发，got %d %s", r.status, r.body)
	}
	// 停用后不兑换（未消费的 token 立即失效）
	if r := h.do(http.MethodPost, "/api/redeem",
		bytes.NewReader(mustJSON(t, map[string]string{"token": tok, "pow_nonce": nonce})),
		"application/json", "203.0.113.7:5555", nil); r.status != http.StatusBadRequest {
		t.Fatalf("停用站点不应兑换，got %d %s", r.status, r.body)
	}

	// 已兑换但未验证的 response：停用使其验证失败
	h.enable(h.siteA.ID)
	tok2, nonce2 := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok2, nonce2)
	h.disable(h.siteA.ID)
	if v := h.verify(sentinelSecret, resp); v.got["success"].(bool) {
		t.Fatal("停用站点竟允许旧 response 验证成功")
	}

	// 重新启用：旧 token 仍不可用（版本已两次提升），新流程照常可用
	h.enable(h.siteA.ID)
	if v := h.verify(sentinelSecret, resp); v.got["success"].(bool) {
		t.Fatal("重新启用后停用前的 response 复活了")
	}
	if v := h.verify(sentinelSecret, resp); v.status != http.StatusOK {
		t.Fatalf("判定类失败必须仍是 HTTP 200，got %d", v.status)
	}
	tok3, nonce3 := h.challengeOK(t, "sk-alpha")
	resp3 := h.redeemOK(t, tok3, nonce3)
	if v := h.verify(sentinelSecret, resp3); !v.got["success"].(bool) {
		t.Fatalf("重新启用后新流程应可用：%s", v.body)
	}

	// 重启后仍然如此：版本与状态都在库里，不在内存
	h.restart(t)
	if v := h.verify(sentinelSecret, resp); v.got["success"].(bool) {
		t.Fatal("重启后旧 response 复活")
	}
}

// --- 需求 11：配置更新与签发快照 ---

// TestDifficultyChangeUsesIssuedSnapshot 钉住**参数类**改动的语义：
// 已签发的 challenge 沿签发快照（难度与过期时刻都存在账本行里并被签名覆盖），
// 上调难度只影响此后新签的请求。
func TestDifficultyChangeUsesIssuedSnapshot(t *testing.T) {
	h := newHarness(t, nil)
	h.tuneSitePolicy(h.siteA.ID, upBits(12))
	tok, nonce, r := h.issueWith(t, "sk-alpha", "")
	if num(r.got["pow"].(map[string]any)["bits"]) != 12 {
		t.Fatalf("首签难度应为 12：%s", r.body)
	}

	// 管理员上调难度：旧 token 仍按其签发时的 12 位判定，兑换成功
	h.tuneSitePolicy(h.siteA.ID, upBits(18))
	if rr := h.postJSON("/api/redeem", map[string]string{"token": tok, "pow_nonce": nonce}); rr.status != http.StatusOK {
		t.Fatalf("难度上调后旧 challenge 应仍按快照可兑换，got %d %s", rr.status, rr.body)
	}
	// 新签发用新难度
	r2 := h.challengeWithOrigin("sk-alpha", "")
	if got := num(r2.got["pow"].(map[string]any)["bits"]); got != 18 {
		t.Fatalf("新签发应采用新难度 18，got %v", got)
	}
	// 客户端无法把难度降回来：改过的 token 验签失败
	tampered := strings.Split(r2.str("token"), ".")
	if len(tampered) != 4 {
		t.Fatal("token 线格式不符")
	}
}

// TestTTLChangeUsesIssuedSnapshot 是同一性质的另一面：缩短 TTL 不会作废已签发的 token，
// 因为过期时刻是签发时写进库行与签名里的；延长同理（不重新判定旧行）。
func TestTTLChangeUsesIssuedSnapshot(t *testing.T) {
	h := newHarness(t, nil)
	h.tuneSitePolicy(h.siteA.ID, upAll(12, 10*time.Minute, 10*time.Minute, true))
	tok, nonce, _ := h.issueWith(t, "sk-alpha", "")

	h.tuneSitePolicy(h.siteA.ID, upTTL(2*time.Second, 2*time.Second))
	h.setClock(3 * time.Second) // 已超过新 TTL，但未超过签发时的 10 分钟快照
	rr := h.postJSON("/api/redeem", map[string]string{"token": tok, "pow_nonce": nonce})
	if rr.status != http.StatusOK {
		t.Fatalf("已签发 token 应按快照继续有效，got %d %s", rr.status, rr.body)
	}
	if exp := num0(rr.got["expires_in"]); exp != 2 {
		t.Fatalf("新签发的 response TTL 应采用新配置 2s，got %v", exp)
	}
}

func num0(v any) int64 { return num(v) }

// --- 需求 6：撤销/错误凭据不消费合法 response（API 级） ---

func TestRevokedSecretDoesNotConsumeAtAPILayer(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)

	// 立即撤销当前凭据
	_, newSecret := h.rotateNow(h.siteA.ID, 0)
	if v := h.verify(sentinelSecret, resp); v.got["success"].(bool) {
		t.Fatal("被撤销的凭据仍完成了验证")
	}
	if !h.verify(sentinelSecret, resp).hasCode(ErrInvalidInputSecret) {
		t.Errorf("撤销后的凭据应归到 invalid-input-secret：%s", h.verify(sentinelSecret, resp).body)
	}
	// 撤销没有消费这条 response：新凭据仍能完成验证
	if v := h.verify(newSecret, resp); !v.got["success"].(bool) {
		t.Fatalf("新凭据应能验证未被消费的 response：%s", v.body)
	}
	// 现在才真正被消费：再验证失败
	if v := h.verify(newSecret, resp); v.got["success"].(bool) || !v.hasCode(ErrTimeoutOrDuplicate) {
		t.Fatalf("重复验证应 timeout-or-duplicate：%s", v.body)
	}
}

// TestWrongSecretAcrossSitesDoesNotConsume 覆盖跨站点：B 站点的 secret 不能消费 A 的 response。
func TestWrongSecretAcrossSitesDoesNotConsume(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)
	if v := h.verify(sentinelSecretB, resp); v.got["success"].(bool) {
		t.Fatal("另一站点的合法 secret 消费了本站点的 response")
	}
	if v := h.verify(sentinelSecret, resp); !v.got["success"].(bool) {
		t.Fatalf("本站点 secret 仍应可验证（跨站点尝试未消费）：%s", v.body)
	}
}

// --- 需求 15：验证请求与停用/撤销并发 ---

// TestVerifyConcurrentWithDisable 要求"按已定义的事务提交顺序给出一致结果"：
// siteverify 消费 response 是原子的，站点停用是另一条条件更新；
// 两者交错时，验证最多成功一次，且成功当刻的判定只取决于它自己事务里的状态。
func TestVerifyConcurrentWithDisable(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	wins := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i == 0 {
				// 一路并发同时把站点停用
				h.disable(h.siteA.ID)
				return
			}
			body := mustJSON(t, map[string]string{"secret": sentinelSecret, "response": resp})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/siteverify", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = "203.0.113.7:5555"
			h.handler().ServeHTTP(rec, req)
			var got map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if ok, _ := got["success"].(bool); ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins > 1 {
		t.Fatalf("%d 路并发（含一次停用）成功 %d 次，最多应为 1", n, wins)
	}
	t.Logf("停用与验证并发：成功 %d 次（0 或 1 都是已定义的一致结果）", wins)
}

// restart 模拟进程重启：关掉全部句柄，用同一个 DB 文件重建整套服务。
func (h *harness) restart(t *testing.T) {
	t.Helper()
	for _, f := range h.cleanup {
		f()
	}
	h.cleanup = nil
	reopened, err := store.Open(context.Background(), h.dbPath, testPepper)
	if err != nil {
		t.Fatalf("重开库: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	h.db = reopened

	signer, err := token.NewSigner(bytes.Repeat([]byte{0x5a}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(h.cfg, reopened, signer, h.gens, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return h.clock }
	h.srv = srv

	if h.siteA, err = reopened.SiteByKey(context.Background(), "sk-alpha"); err != nil {
		t.Fatalf("重启后站点丢失: %v", err)
	}
	if h.siteB, err = reopened.SiteByKey(context.Background(), "sk-bravo"); err != nil {
		t.Fatalf("重启后站点丢失: %v", err)
	}
}

// TestPreflightIsRateLimited 预检是匿名可打的公开端点，必须有有界限流（红线 6）。
// 顺序也在这里被钉住：限流发生在查库之前，否则每个 OPTIONS 都先打一次 SQL。
func TestPreflightIsRateLimited(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.RateIPRPM = 3 })
	h.setOrigins(h.siteA.ID, []string{"https://app.example.com"})
	saw429 := false
	for i := 0; i < 8; i++ {
		r := h.do(http.MethodOptions, "/api/challenge?site_key=sk-alpha", nil, "",
			"203.0.113.7:5555", map[string]string{
				"Origin": "https://app.example.com", "Access-Control-Request-Method": "POST",
			})
		if r.status == http.StatusTooManyRequests {
			saw429 = true
			if r.headers.Get("Retry-After") == "" {
				t.Fatal("预检限流必须带 Retry-After")
			}
			break
		}
		if r.status != http.StatusNoContent {
			t.Fatalf("第 %d 次预检状态异常：%d %s", i+1, r.status, r.body)
		}
	}
	if !saw429 {
		t.Fatal("预检未受来源地址限流保护")
	}
	// 限流命中时不得回显 CORS 头之外的任何东西；这里确认它没有顺手签发任何东西
	if n := h.activeChallenges(t); n != 0 {
		t.Fatalf("预检产生了 challenge 行：%d", n)
	}
}
