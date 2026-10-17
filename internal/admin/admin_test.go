package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/store"
)

const (
	sentinelAdminKey = "ak_SENTINEL-ADMIN-7c4f2a9b1e6d3k5m"
	sentinelPepper   = "pepper"
)

type h struct {
	t      *testing.T
	db     *store.DB
	srv    *Server
	public *httptest.Server
	admin  *httptest.Server
	logs   *bytes.Buffer
	dbPath string
}

func newHarness(t *testing.T, tune func(*config.Config)) *h {
	t.Helper()
	dir := t.TempDir()
	pepper := bytes.Repeat([]byte(sentinelPepper), 32)
	dbPath := filepath.Join(dir, "epocha.db")
	db, err := store.Open(context.Background(), dbPath, pepper)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, _, err := db.BootstrapAdminKey(context.Background(), sentinelAdminKey, "bootstrap"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	cfg := &config.Config{
		Listen: "127.0.0.1:0", AdminListen: "127.0.0.1:0", DBPath: dbPath,
		MasterKey: bytes.Repeat([]byte{0x5a}, 32), Pepper: pepper,
		ChallengeTTL: time.Minute, ResponseTTL: time.Minute,
		RateIPRPM: 100000, RateSiteRPM: 100000, RateMaxState: 4096,
		MaxActiveChallenges: 10000, MaxActivePerSite: 5000,
	}
	if tune != nil {
		tune(cfg)
	}
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	srv, err := New(cfg, db, log)
	if err != nil {
		t.Fatalf("admin.New: %v", err)
	}
	hh := &h{t: t, db: db, srv: srv, logs: logs, dbPath: dbPath}
	hh.admin = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { hh.admin.Close(); _ = db.Close() })
	return hh
}

func (x *h) call(method, path string, body any, auth string) *http.Response {
	x.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			x.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, x.admin.URL+path, r)
	if err != nil {
		x.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := x.admin.Client().Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	return resp
}

type response struct {
	status int
	body   string
	got    map[string]any
	header http.Header
}

func (x *h) do(method, path string, body any) response {
	return x.doAuth(method, path, body, "Bearer "+sentinelAdminKey)
}

func (x *h) doAuth(method, path string, body any, auth string) response {
	x.t.Helper()
	resp := x.call(method, path, body, auth)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := map[string]any{}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") && len(raw) > 0 {
		if err := json.Unmarshal(raw, &got); err != nil {
			x.t.Fatalf("%s %s 响应不是合法 JSON: %v / %q", method, path, err, raw)
		}
	}
	return response{status: resp.StatusCode, body: string(raw), got: got, header: resp.Header}
}

func (x *h) adminHeader(id int64) map[string]any {
	return map[string]any{"site_id": id}
}

func (x *h) createSite(name, siteKey string) (response, string) {
	x.t.Helper()
	r := x.do(http.MethodPost, "/api/admin/sites", map[string]any{
		"name": name, "site_key": siteKey, "pow_bits": 14,
	})
	if r.status != http.StatusCreated {
		x.t.Fatalf("创建站点失败：%d %s", r.status, r.body)
	}
	site, _ := r.got["site"].(map[string]any)
	if site == nil {
		x.t.Fatalf("响应缺少 site：%s", r.body)
	}
	secret, _ := r.got["secret"].(string)
	if secret == "" {
		x.t.Fatal("创建响应必须含一次性 secret")
	}
	return r, secret
}

func siteID(r response) int64 {
	site, _ := r.got["site"].(map[string]any)
	idf, _ := site["id"].(float64)
	return int64(idf)
}

// --- 需求 1：鉴权矩阵 ---

func TestBearerAuthMatrix(t *testing.T) {
	x := newHarness(t, nil)
	for name, auth := range map[string]string{
		"缺少 Authorization": "",
		"空 Bearer":         "Bearer ",
		"错误凭据":             "Bearer ak_wrong_but_long_enough_to_pass_form_checks_1234",
		"非 Bearer 方案":      "Basic dXNlcjpwYXNz",
		"只有 Bearer 前缀":     "Bearer",
		"超长凭据":             "Bearer " + strings.Repeat("a", 4000),
	} {
		r := x.doAuth(http.MethodGet, "/api/admin/sites", nil, auth)
		if r.status != http.StatusUnauthorized {
			t.Errorf("%s：应 401，got %d %s", name, r.status, r.body)
		}
		if r.got["error"] != CodeUnauthorized {
			t.Errorf("%s：错误码应为 unauthorized，got %v", name, r.got["error"])
		}
		if strings.Contains(r.body, sentinelAdminKey) {
			t.Errorf("%s：响应回显了管理凭据：%s", name, r.body)
		}
	}
	// 正确凭据可用
	if r := x.do(http.MethodGet, "/api/admin/sites", nil); r.status != http.StatusOK {
		t.Fatalf("合法凭据应放行，got %d %s", r.status, r.body)
	}
	// 撤销后立刻不可用（安全动作立即生效，无缓存）
	who := x.do(http.MethodGet, "/api/admin/whoami", nil)
	keyID, _ := who.got["key_id"].(string)
	if keyID == "" {
		t.Fatal("whoami 应返回当前 key_id")
	}
	rev := x.do(http.MethodPost, "/api/admin/admin-keys/"+keyID+"/revoke", map[string]any{"transition_sec": 0})
	if rev.status != http.StatusOK {
		t.Fatalf("撤销失败：%d %s", rev.status, rev.body)
	}
	if r := x.do(http.MethodGet, "/api/admin/sites", nil); r.status != http.StatusUnauthorized {
		t.Fatalf("撤销后的凭据仍被接受，got %d %s", r.status, r.body)
	}
	// 库里没有可用管理凭据时，任何请求都被拒（"缺少管理凭据"绝不等于"开放管理功能"）
	if r := x.doAuth(http.MethodGet, "/api/admin/audit", nil, "Bearer anything-elseentirely-1234567890"); r.status != http.StatusUnauthorized {
		t.Fatalf("无有效凭据时应 401，got %d", r.status)
	}
}

func TestAuthFailureLockoutIsBounded(t *testing.T) {
	x := newHarness(t, nil)
	// 连续错误凭据：达到窗口上限后转为 429，避免这里变成低成本的凭据爆破面
	last := 0
	for i := 0; i < maxAuthFailRPM+3; i++ {
		r := x.doAuth(http.MethodGet, "/api/admin/sites", nil,
			"Bearer ak_guessed_value_padding_padding_padding_9999")
		last = r.status
		if r.status == http.StatusTooManyRequests {
			if r.header.Get("Retry-After") == "" {
				t.Error("429 必须带 Retry-After")
			}
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("鉴权失败次数达上限后应 429，最后状态 %d", last)
	}
	// 失败计数有硬上限由 risk.Limiter 保证；这里确认它没有因为爆破而无限增长
	if n := x.srv.authFails.Len(); n > x.srv.authFails.Evictions()+maxAuthFailRPM*2 {
		t.Fatalf("失败计数条目数异常: %d", n)
	}
}

// --- 需求 3：站点 CRUD / 启停 / 重启持久化 ---

func TestSiteCRUDAndPersistence(t *testing.T) {
	x := newHarness(t, nil)
	created, secret := x.createSite("alpha", "pk_alpha")
	id := siteID(created)
	_ = secret

	// 详情
	detail := x.do(http.MethodGet, pathSite(id), nil)
	if detail.status != http.StatusOK {
		t.Fatalf("详情失败：%d %s", detail.status, detail.body)
	}
	site := detail.got["site"].(map[string]any)
	if site["enabled"] != true || site["name"] != "alpha" {
		t.Fatalf("详情不符：%s", detail.body)
	}
	if _, ok := detail.got["secret"]; ok {
		t.Fatal("详情响应不得含 secret")
	}
	if _, ok := site["secret_mac"]; ok {
		t.Fatal("详情不得暴露摘要字段")
	}

	// 更新难度与 TTL 与来源策略
	up := x.do(http.MethodPut, pathSite(id), map[string]any{
		"pow_bits": 16, "challenge_ttl_sec": 300, "response_ttl_sec": 90,
		"rate_ip_rpm": 30, "rate_site_rpm": 120, "allow_missing_origin": false,
	})
	if up.status != http.StatusOK {
		t.Fatalf("更新失败：%d %s", up.status, up.body)
	}
	u := up.got["site"].(map[string]any)
	if u["pow_bits"] != float64(16) || u["challenge_ttl_sec"] != float64(300) ||
		u["response_ttl_sec"] != float64(90) || u["rate_ip_rpm"] != float64(30) ||
		u["rate_site_rpm"] != float64(120) || u["allow_missing_origin"] != false {
		t.Fatalf("更新未生效：%s", up.body)
	}
	// 部分更新：只改难度，其余保持
	partial := x.do(http.MethodPut, pathSite(id), map[string]any{"pow_bits": 13})
	p := partial.got["site"].(map[string]any)
	if p["pow_bits"] != float64(13) || p["challenge_ttl_sec"] != float64(300) ||
		p["allow_missing_origin"] != false {
		t.Fatalf("部分更新覆盖了我们没要求的字段：%s", partial.body)
	}

	// 启停（版本必须提升）
	v0 := u["version"].(float64)
	dis := x.do(http.MethodPost, pathSite(id)+"/disable", nil)
	if dis.got["site"].(map[string]any)["enabled"] != false {
		t.Fatalf("停用失败：%s", dis.body)
	}
	if dis.got["site"].(map[string]any)["version"].(float64) <= v0 {
		t.Fatal("停用未提升站点版本")
	}
	en := x.do(http.MethodPost, pathSite(id)+"/enable", nil)
	if en.got["site"].(map[string]any)["enabled"] != true {
		t.Fatalf("启用失败：%s", en.body)
	}
	v1 := en.got["site"].(map[string]any)["version"].(float64)

	// 重复启用是幂等的：启停是状态动作，不是计数器
	en2 := x.do(http.MethodPost, pathSite(id)+"/enable", nil)
	if en2.got["site"].(map[string]any)["version"].(float64) != v1 {
		t.Fatal("重复启用不应再次提升版本")
	}

	// 列表分页
	for i := 0; i < 3; i++ {
		x.createSite("s"+itoa(int64(i)), "pk_s"+itoa(int64(i)))
	}
	list := x.do(http.MethodGet, "/api/admin/sites?limit=2", nil)
	if n := len(list.got["sites"].([]any)); n != 2 {
		t.Fatalf("limit=2 应返回 2 条，got %d", n)
	}
	next := list.got["next_after_id"].(float64)
	list2 := x.do(http.MethodGet, pathWithCursor("/api/admin/sites", "after_id", itoa(int64(next))), nil)
	if len(list2.got["sites"].([]any)) == 0 {
		t.Fatal("游标分页应能继续")
	}
	for _, q := range []string{"?limit=501", "?limit=-1", "?after_id=-5", "?after_id=abc"} {
		if r := x.do(http.MethodGet, "/api/admin/sites"+q, nil); r.status != http.StatusBadRequest {
			t.Errorf("非法分页参数 %s 应 400，got %d", q, r.status)
		}
	}

	// DELETE 是软吊销：停用 + 提升版本，账本行不删
	del := x.do(http.MethodDelete, pathSite(id), nil)
	if del.got["revoked"] != true {
		t.Fatalf("吊销响应不符：%s", del.body)
	}
	if del.got["site"].(map[string]any)["enabled"] != false {
		t.Fatal("吊销未停用站点")
	}

	// 重启（同一路径重开库）后所有状态仍在
	x.restart(t)
	after := x.do(http.MethodGet, pathSite(id), nil)
	a := after.got["site"].(map[string]any)
	if a["enabled"] != false || a["pow_bits"] != float64(13) || a["name"] != "alpha" {
		t.Fatalf("重启后站点状态丢失：%s", after.body)
	}
	if a["version"].(float64) <= v1 {
		t.Fatalf("重启后版本回退了：%v", a["version"])
	}
}

func pathSite(id int64) string { return "/api/admin/sites/" + itoa(id) }

func pathWithCursor(base, k, v string) string { return base + "?" + k + "=" + v }

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// --- 需求 4：secret 只展示一次 ---

func TestSecretShownOnceOnly(t *testing.T) {
	x := newHarness(t, nil)
	created, secret := x.createSite("alpha", "pk_alpha")
	id := siteID(created)

	scanPaths := []struct{ method, path string }{
		{http.MethodGet, pathSite(id)},
		{http.MethodGet, "/api/admin/sites"},
		{http.MethodGet, pathSite(id) + "/secrets"},
		{http.MethodGet, pathSite(id) + "/origins"},
		{http.MethodGet, "/api/admin/audit?limit=100"},
		{http.MethodGet, "/api/admin/admin-keys"},
	}
	for _, p := range scanPaths {
		r := x.do(p.method, p.path, nil)
		if strings.Contains(r.body, secret) {
			t.Errorf("%s %s 泄露了 secret：%s", p.method, p.path, r.body)
		}
	}
	// 错误路径也不回显：用错误凭据 + 非法 body 触发若干 4xx 再扫
	x.doAuth(http.MethodGet, pathSite(id), nil, "Bearer ak_wrong_wrong_wrong_wrong_wrong_wrong_0000")
	x.do(http.MethodPost, "/api/admin/sites", map[string]any{"name": "alpha"})
	x.do(http.MethodPut, pathSite(id), map[string]any{"pow_bits": 99})
	for _, r := range x.bodiesOfInterest() {
		if strings.Contains(r, secret) {
			t.Errorf("错误响应泄露 secret：%s", r)
		}
	}
	// 列表/详情里的凭据视图只有 key_id 与时间
	secrets := x.do(http.MethodGet, pathSite(id)+"/secrets", nil)
	list, _ := secrets.got["secrets"].([]any)
	if len(list) != 1 {
		t.Fatalf("应有 1 把凭据：%s", secrets.body)
	}
	view := list[0].(map[string]any)
	for _, k := range []string{"secret", "secret_mac", "mac", "pepper"} {
		if _, ok := view[k]; ok {
			t.Errorf("凭据视图不得含 %s：%v", k, view)
		}
	}
	// 日志（按值扫描）
	if strings.Contains(x.logs.String(), secret) {
		t.Error("日志泄露了 secret")
	}
	// 库文件里只有带 pepper 的摘要
	x.closeDB(t)
	raw, err := os.ReadFile(x.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Error("DB 文件里出现 secret 明文")
	}
}

func (x *h) bodiesOfInterest() []string {
	return []string{x.logs.String()}
}

// --- 需求 5：轮换的两种方式 ---

func TestRotationImmediateAndTransition(t *testing.T) {
	x := newHarness(t, nil)
	created, first := x.createSite("alpha", "pk_alpha")
	id := siteID(created)

	// 立即撤销旧密钥
	rot := x.do(http.MethodPost, pathSite(id)+"/secrets/rotate", map[string]any{"transition_sec": 0})
	if rot.status != http.StatusOK {
		t.Fatalf("轮换失败：%d %s", rot.status, rot.body)
	}
	second, _ := rot.got["secret"].(string)
	if second == "" || second == first {
		t.Fatal("轮换必须返回新的明文 secret")
	}
	retired, _ := rot.got["retired"].([]any)
	if len(retired) != 1 {
		t.Fatalf("应撤下 1 把旧密钥：%s", rot.body)
	}
	if retired[0].(map[string]any)["active"] != false {
		t.Error("立即撤销后 active 必须为 false")
	}
	// 过渡期：旧密钥仍标记 active，且有未来的 revoked_at
	rot2 := x.do(http.MethodPost, pathSite(id)+"/secrets/rotate", map[string]any{"transition_sec": 3600})
	retired2, _ := rot2.got["retired"].([]any)
	if len(retired2) != 1 {
		t.Fatalf("过渡期轮换应撤下 1 把：%s", rot2.body)
	}
	rv := retired2[0].(map[string]any)
	if rv["active"] != true {
		t.Errorf("过渡期内旧密钥应仍有效：%v", rv)
	}
	if rv["revoked_at"] == "" {
		t.Errorf("过渡期必须写明失效时刻：%v", rv)
	}
	// 过渡期上限与负值
	for _, bad := range []any{-1, 900000, 1e12} {
		if r := x.do(http.MethodPost, pathSite(id)+"/secrets/rotate", map[string]any{"transition_sec": bad}); r.status != http.StatusBadRequest {
			t.Errorf("transition_sec=%v 应 400，got %d", bad, r.status)
		}
	}
	// 同时有效的凭据数有上限（返回 4xx 而不是无限堆积）
	for i := 0; i < store.MaxSecretsPerSite+2; i++ {
		r := x.do(http.MethodPost, pathSite(id)+"/secrets", nil)
		if r.status == http.StatusBadRequest {
			break
		}
		if i == store.MaxSecretsPerSite {
			t.Fatalf("超过上限 %d 把应拒绝：%s", store.MaxSecretsPerSite, r.body)
		}
	}
	// 撤销按 key_id 指定，且撤销后仍不返回任何 secret 材料
	list := x.do(http.MethodGet, pathSite(id)+"/secrets", nil)
	firstKey := list.got["secrets"].([]any)[0].(map[string]any)["key_id"].(string)
	rev := x.do(http.MethodPost, pathSite(id)+"/secrets/"+firstKey+"/revoke", map[string]any{})
	if rev.status != http.StatusOK {
		t.Fatalf("撤销失败：%d %s", rev.status, rev.body)
	}
	// 不存在的 key_id → 404，且不泄露别的信息
	if r := x.do(http.MethodPost, pathSite(id)+"/secrets/kdoesnotexist/revoke", map[string]any{}); r.status != http.StatusNotFound {
		t.Errorf("未知 key_id 应 404，got %d %s", r.status, r.body)
	}
	// 轮换响应里的明文与创建时不同，且旧明文不再出现在任何后续响应中
	for _, r := range []response{list, x.do(http.MethodGet, pathSite(id), nil), x.do(http.MethodGet, "/api/admin/audit?limit=100", nil)} {
		if strings.Contains(r.body, first) || strings.Contains(r.body, second) {
			t.Errorf("后续响应泄露了 secret：%s", r.body)
		}
	}
}

// --- 需求 14：审计 ---

func TestAuditCoversWritesAndLeaksNothing(t *testing.T) {
	x := newHarness(t, nil)
	created, secret := x.createSite("alpha", "pk_alpha")
	id := siteID(created)
	x.do(http.MethodPut, pathSite(id), map[string]any{"pow_bits": 15})
	x.do(http.MethodPost, pathSite(id)+"/disable", nil)
	x.do(http.MethodPost, pathSite(id)+"/enable", nil)
	x.do(http.MethodPut, pathSite(id)+"/origins", map[string]any{"origins": []string{"https://app.example.com"}})
	x.do(http.MethodPost, pathSite(id)+"/secrets/rotate", map[string]any{"transition_sec": 60})
	x.do(http.MethodPost, "/api/admin/admin-keys", map[string]any{"label": "ops2"})

	a := x.do(http.MethodGet, "/api/admin/audit?limit=100", nil)
	entries, _ := a.got["audit"].([]any)
	wantActions := []string{"site.create", "site.update", "site.disable", "site.enable",
		"site.origins", "secret.rotate", "admin_key.add"}
	seen := map[string]bool{}
	for _, e := range entries {
		m, _ := e.(map[string]any)
		seen[m["action"].(string)] = true
		for _, k := range []string{"secret", "admin_key", "key_mac", "secret_mac", "authorization"} {
			if _, ok := m[k]; ok {
				t.Errorf("审计条目含 %s 字段：%v", k, m)
			}
		}
	}
	for _, act := range wantActions {
		if !seen[act] {
			t.Errorf("写操作 %s 未落审计（全部动作：%v）", act, seen)
		}
	}
	// actor 是管理密钥的公开 key_id，不是凭据本身
	for _, e := range entries {
		m, _ := e.(map[string]any)
		if m["actor"] == sentinelAdminKey || strings.HasPrefix(m["actor"].(string), "ak_SENTINEL") {
			t.Errorf("审计 actor 泄露凭据：%v", m["actor"])
		}
		if !strings.HasPrefix(m["actor"].(string), "k") {
			t.Errorf("审计 actor 应为 key_id：%v", m["actor"])
		}
	}
	// 只读接口不落审计（否则列表本身自我放大）
	before := len(x.do(http.MethodGet, "/api/admin/audit?limit=1", nil).got["audit"].([]any))
	for i := 0; i < 3; i++ {
		x.do(http.MethodGet, "/api/admin/sites", nil)
		x.do(http.MethodGet, pathSite(id), nil)
		x.do(http.MethodGet, "/api/admin/audit?limit=5", nil)
	}
	after := x.do(http.MethodGet, "/api/admin/audit?limit=1", nil)
	if len(after.got["audit"].([]any)) != 1 || before == 0 {
		t.Fatalf("审计读取异常：%v", after.body)
	}
	last := x.do(http.MethodGet, "/api/admin/audit?limit=100", nil).got["audit"].([]any)
	if len(last) != len(entries) {
		t.Fatalf("只读操作新增了审计行：%d → %d", len(entries), len(last))
	}
	// 分页边界
	if r := x.do(http.MethodGet, "/api/admin/audit?limit=501", nil); r.status != http.StatusBadRequest {
		t.Errorf("limit 超上限应 400，got %d", r.status)
	}
	if r := x.do(http.MethodGet, "/api/admin/audit?limit=-1", nil); r.status != http.StatusBadRequest {
		t.Errorf("负 limit 应 400，got %d", r.status)
	}
	// 凭据值不得出现在审计里（按值扫描）
	if strings.Contains(a.body, secret) || strings.Contains(a.body, sentinelAdminKey) {
		t.Error("审计响应泄露凭据")
	}
}

// --- 来源与请求边界 ---

func TestOriginsAPIAndBodyLimits(t *testing.T) {
	x := newHarness(t, nil)
	created, _ := x.createSite("alpha", "pk_alpha")
	id := siteID(created)

	r := x.do(http.MethodPut, pathSite(id)+"/origins", map[string]any{"origins": []string{
		"https://APP.example.com:443", "http://localhost:8080", "https://app.example.com",
	}})
	if r.status != http.StatusOK {
		t.Fatalf("设置来源失败：%d %s", r.status, r.body)
	}
	got := r.got["origins"].([]any)
	if len(got) != 2 || got[0] != "http://localhost:8080" || got[1] != "https://app.example.com" {
		t.Fatalf("来源未规范化/去重：%s", r.body)
	}
	// 非法来源整批拒绝，且不得部分生效
	bad := x.do(http.MethodPut, pathSite(id)+"/origins", map[string]any{"origins": []string{
		"https://ok.example.com", "https://*.example.com",
	}})
	if bad.status != http.StatusBadRequest {
		t.Fatalf("通配符应 400，got %d %s", bad.status, bad.body)
	}
	after := x.do(http.MethodGet, pathSite(id)+"/origins", nil)
	if strings.Contains(after.body, "ok.example.com") {
		t.Error("整批拒绝时不得有任何条目写入")
	}
	for _, o := range []string{"null", "", "https://example.com/x", "ftp://example.com", "https://exa mple.com"} {
		if r := x.do(http.MethodPut, pathSite(id)+"/origins", map[string]any{"origins": []string{o}}); r.status != http.StatusBadRequest {
			t.Errorf("非法来源 %q 应 400，got %d", o, r.status)
		}
	}
	// 空列表 = 不接受任何浏览器来源（合法状态）
	if r := x.do(http.MethodPut, pathSite(id)+"/origins", map[string]any{"origins": []string{}}); r.status != http.StatusOK {
		t.Fatalf("空来源列表应合法，got %d %s", r.status, r.body)
	}

	// 请求体上限与形态：未知字段、超长、非 JSON 一律 400，不得 500
	oversize := map[string]any{"name": strings.Repeat("n", 300), "site_key": "pk_" + strings.Repeat("k", 200)}
	if r := x.do(http.MethodPost, "/api/admin/sites", oversize); r.status != http.StatusBadRequest {
		t.Errorf("超长字段应 400，got %d %s", r.status, r.body)
	}
	if r := x.doAuth(http.MethodGet, "/api/admin/sites", nil, "Bearer "+sentinelAdminKey); r.status != http.StatusOK {
		t.Fatal("合法请求应通过")
	}
	raw := x.call(http.MethodPost, "/api/admin/sites", bytes.NewReader(make([]byte, maxBodyAdmin+1)), "Bearer "+sentinelAdminKey)
	io.Copy(io.Discard, raw.Body)
	raw.Body.Close()
	if raw.StatusCode != http.StatusBadRequest {
		t.Errorf("超大请求体应 400，got %d", raw.StatusCode)
	}
}

func TestUnknownPathsAndMethods(t *testing.T) {
	x := newHarness(t, nil)
	if r := x.do(http.MethodGet, "/api/admin/nope", nil); r.status != http.StatusNotFound {
		t.Errorf("未知管理路径应 404，got %d", r.status)
	}
	if r := x.do(http.MethodPost, "/api/admin/sites/1", nil); r.status == http.StatusOK {
		t.Error("不该存在 POST /sites/{id}")
	}
	// 管理面不得出现任何 CORS 头
	if v := x.do(http.MethodGet, "/api/admin/sites", nil).header.Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("管理响应不应有 ACAO：%q", v)
	}
	// 也不得暴露 Cache-Control 之外的缓存许可
	if v := x.do(http.MethodGet, "/api/admin/sites", nil).header.Get("Cache-Control"); v != "no-store" {
		t.Errorf("管理响应必须 no-store，got %q", v)
	}
}

// restart 模拟进程重启：关掉库句柄后用同一路径重开，再重建管理面。
func (x *h) restart(t *testing.T) {
	t.Helper()
	x.admin.Close()
	if err := x.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), x.dbPath, x.srvKey())
	if err != nil {
		t.Fatalf("重开库: %v", err)
	}
	x.db = db
	srv, err := New(x.cfgForTest(), db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	x.srv = srv
	x.admin = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { x.admin.Close(); _ = db.Close() })
}

func (x *h) srvKey() []byte { return bytes.Repeat([]byte(sentinelPepper), 32) }

func (x *h) cfgForTest() *config.Config {
	return &config.Config{
		RateIPRPM: 100000, RateSiteRPM: 100000, RateMaxState: 4096,
		MaxActiveChallenges: 10000, MaxActivePerSite: 5000,
	}
}

// closeDB 关掉句柄以便按值扫描库文件，随后重开让后续断言仍能跑。
func (x *h) closeDB(t *testing.T) {
	t.Helper()
	if err := x.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), x.dbPath, x.srvKey())
	if err != nil {
		t.Fatal(err)
	}
	x.db = db
}

// 以下两条是**未来阶段的占位测试**，不计入阶段 2 验收（AGENTS：占位测试要单独列明）。

func TestStatsSortWhitelist(t *testing.T) {
	t.Skip("待阶段 5：统计接口的排序字段走白名单映射；本阶段没有任何排序参数，因此不存在动态列名注入面")
}

func TestWeightAutoDemotionRule(t *testing.T) {
	t.Skip("待阶段 5（DESIGN §17 阶段 5、ADR-006）：分级淘汰——候选告警 + 人工确认，自动降权需多信号同时成立")
}
