package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/pool"
	"github.com/shijianus/EpoCHA/internal/pow"
	"github.com/shijianus/EpoCHA/internal/risk"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
)

// 哨兵值：测试结尾按值扫描日志与响应，确认它们没有被写出去（红线 11、DESIGN §14-7）。
const (
	sentinelSecret    = "SENTINEL-SECRET-3f9a1c7b5e2d"
	sentinelSecretB   = "SENTINEL-SECRET-B-8d2a4c6e1f"
	sentinelMasterB64 = "TUNldGFIUjBjSE02THk5d1pXNTBhVzl1TVRneU1qUTBNemd3TlE9PQ=="
)

var testPepper = bytes.Repeat([]byte("p"), 32)

type harness struct {
	t       *testing.T
	srv     *Server
	db      *store.DB
	dbPath  string
	cfg     *config.Config
	gens    *generator.Registry
	logs    *bytes.Buffer
	bodies  []string
	bodiesM sync.Mutex // 并发测试里多路 goroutine 共用同一个 harness 记录响应体
	clock   time.Time
	// seedCount 给受控夹具的池行一个单调序号：既保证 id 不重复，
	// 也让"按 expires_at 升序领取"的顺序在多次 seed 之间仍然可复现。
	seedCount int
	siteA     *store.Site
	siteB     *store.Site
	cleanup   []func()
}

// allBodies 是并发安全地取回全部响应体（按值扫描哨兵用）。
func (h *harness) allBodies() string {
	h.bodiesM.Lock()
	defer h.bodiesM.Unlock()
	return strings.Join(h.bodies, "\n")
}

func newHarness(t *testing.T, tune func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Listen: "127.0.0.1:0", DBPath: filepath.Join(dir, "epocha.db"),
		MasterKey: bytes.Repeat([]byte{0x5a}, 32), Pepper: testPepper,
		Sites: []config.Site{
			{Name: "alpha", SiteKey: "sk-alpha", Secret: sentinelSecret, PowBits: 12},
			{Name: "bravo", SiteKey: "sk-bravo", Secret: sentinelSecretB, PowBits: 12},
		},
		ChallengeTTL: time.Minute, ResponseTTL: time.Minute,
		RateIPRPM: 100_000, RateSiteRPM: 100_000, RateMaxState: 4096,
		MaxActiveChallenges: 10_000, MaxActivePerSite: 5_000,
	}
	if tune != nil {
		tune(cfg)
	}
	h := &harness{t: t, logs: &bytes.Buffer{}, clock: time.Now().UTC().Truncate(time.Second), cfg: cfg}
	_ = testMasterKey

	db, err := store.Open(context.Background(), cfg.DBPath, cfg.Pepper)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	h.db, h.dbPath = db, cfg.DBPath
	for _, s := range cfg.Sites {
		// 站点由"一次性导入"进入库：secret 用测试哨兵值，TTL 用 1 分钟便于推进注入时钟，
		// allow_missing_origin 打开——测试请求大多不带 Origin，走的正是直接 API 调用那条路。
		policy := store.SitePolicy{
			PowBits: s.PowBits, ChallengeTTL: time.Minute, ResponseTTL: time.Minute,
			RateIPRPM: cfg.RateIPRPM, RateSiteRPM: cfg.RateSiteRPM, AllowMissingOrigin: true,
		}
		site, _, err := db.ImportSite(context.Background(), s.Name, s.SiteKey, s.Secret, policy)
		if err != nil {
			t.Fatalf("ImportSite(%s): %v", s.Name, err)
		}
		if s.Name == "alpha" {
			h.siteA = site
		} else {
			h.siteB = site
		}
	}
	signer, err := token.NewSigner(cfg.MasterKey, nil)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	log := slog.New(slog.NewTextHandler(h.logs, nil))
	gens := testRegistry(t, cfg.MasterKey)
	srv, err := New(cfg, db, signer, gens, log)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	// 闭包必须每次读 h.clock：早期版本按值捕获，导致 setClock 完全无效、
	// 过期相关测试全部假通过。
	srv.now = func() time.Time { return h.clock }
	h.srv, h.gens = srv, gens
	tracker, err := risk.NewTracker(risk.MaxTrackedSources, func() time.Time { return h.clock })
	if err != nil {
		t.Fatalf("risk.NewTracker: %v", err)
	}
	gate, err := risk.NewAccessibilityGate(risk.MaxTrackedSources, risk.AccessibilityLimit, risk.AccessibilityWindow, func() time.Time { return h.clock })
	if err != nil {
		t.Fatalf("risk.NewAccessibilityGate: %v", err)
	}
	h.srv.tracker, h.srv.access = tracker, gate
	_ = h.gens
	h.cleanup = append(h.cleanup, func() { _ = db.Close() })
	t.Cleanup(func() {
		for _, f := range h.cleanup {
			f()
		}
	})
	return h
}

// testMasterKey 与 newHarness 用的是同一把主密钥：restart 之后签发的 token 仍可验签。
var testMasterKey = bytes.Repeat([]byte{0x5a}, 32)

// testRegistry 登记本程序**全部**生成器（3A 的算术基线 + 3B/ADR-024 的视觉题型）；
// verifier 密钥走与生产同一条派生路径（token.DerivePurpose(master, "qver")）。
func testRegistry(t *testing.T, master []byte) *generator.Registry {
	t.Helper()
	r := generator.NewRegistry()
	k := token.DerivePurpose(master, "qver")
	register := func(g generator.Generator, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Register(g); err != nil {
			t.Fatal(err)
		}
	}
	register(generator.NewArithmeticBaseline(k))
	register(generator.NewTextImage(k))
	register(generator.NewTextImageV2(k))
	register(generator.NewTextImageV3(k))
	register(generator.NewShapeGrid(k))
	register(generator.NewShapeCount(k))
	register(generator.NewParallax(k))
	register(generator.NewBankShot(k))
	register(generator.NewAccessibleChoice(k))
	register(generator.NewEnergyWindow(k))
	register(generator.NewPipeConnect(k))
	register(generator.NewIntersectionPoint(k))
	register(newContractAngle(k))
	register(newContractPoint(k))
	register(newContractTiming(k))
	return r
}

// questionSite 建一个 pow_question 站点，并返回它的 secret（验证时要用**本站点**的凭据）。
// 默认 harness 的两个站点是 pow_only，正好用来验回归。
func (h *harness) questionSite(t *testing.T, name, siteKey string, difficulty int) (*store.Site, string) {
	t.Helper()
	mode := store.ModePowQuestion
	site, secret, err := h.db.CreateSite(context.Background(), name, siteKey, store.SitePolicy{
		PowBits: 12, VerifyMode: mode, GeneratorID: generator.ArithID, Difficulty: difficulty,
		AllowMissingOrigin: true,
	})
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	return site, secret
}

// fillPool 直接驱动一轮补题（测试不等后台节奏），并确认真的备到了题。
func (h *harness) fillPool(t *testing.T, difficulty, n int) {
	t.Helper()
	cfg := pool.DefaultConfig()
	cfg.Prewarm = false
	cfg.LowWatermark = n
	cfg.HighWatermark = n
	cfg.BatchPerTick = n
	cfg.MaxRows = 500
	w, err := pool.New(h.db, h.gens, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := w.Tick(context.Background()); err != nil {
			t.Fatalf("补题失败: %v", err)
		}
	}
	spec := store.PoolSpec{GeneratorID: generator.ArithID, Version: "v1", Difficulty: difficulty}
	got, err := h.db.ReadyCount(context.Background(), spec, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got < n {
		t.Fatalf("备题不足：ready=%d，期望 ≥%d", got, n)
	}
}

// verifierOf 从 challenge token 解出 cid，再读回该题的私有 verifier 字节。
// 测试拿它**不是为了读答案**（verifier 是单向摘要，不可逆），而是为了得到一段必然唯一的
// 高熵串，用按值扫描证明私有材料没有出现在任何响应或日志里（红线 5、11）。
func (h *harness) verifierOf(t *testing.T, challengeToken string) []byte {
	t.Helper()
	signer, err := token.NewSigner(bytes.Repeat([]byte{0x5a}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := signer.Verify(token.PurposeChallenge, challengeToken, h.clock)
	if err != nil {
		t.Fatalf("解 challenge token: %v", err)
	}
	q, err := h.db.QuestionForChallenge(context.Background(), claims.CID)
	if err != nil {
		t.Fatalf("读绑定题目: %v", err)
	}
	return []byte(q.Verifier)
}

func (h *harness) setClock(d time.Duration) { h.clock = h.clock.Add(d) }

func (h *harness) handler() http.Handler { return h.srv.Handler() }

type reply struct {
	status  int
	body    string
	keys    []string
	got     map[string]any
	headers http.Header
}

func (h *harness) do(method, path string, body io.Reader, contentType, remoteAddr string, headers map[string]string) reply {
	h.t.Helper()
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler().ServeHTTP(rec, req)

	raw := rec.Body.String()
	var got map[string]any
	keys := []string{}
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&got); err == nil {
			for k := range got {
				keys = append(keys, k)
			}
		} else {
			h.t.Fatalf("%s %s 响应不是合法 JSON: %v / %q", method, path, err, raw)
		}
	}
	h.bodiesM.Lock()
	h.bodies = append(h.bodies, raw)
	h.bodiesM.Unlock()
	return reply{status: rec.Code, body: raw, keys: keys, got: got, headers: rec.Header()}
}

func (h *harness) postJSON(path string, payload any) reply {
	b, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.do(http.MethodPost, path, bytes.NewReader(b), "application/json", "203.0.113.7:5555", nil)
}

func (r reply) str(k string) string {
	v, _ := r.got[k].(string)
	return v
}

func (r reply) codes() []string {
	raw, ok := r.got["error-codes"]
	if !ok {
		return nil
	}
	list, _ := raw.([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func (r reply) hasCode(want string) bool {
	for _, c := range r.codes() {
		if c == want {
			return true
		}
	}
	return false
}

// assertKeys 校验一个嵌套对象的键集合（用于 pow / question 这类子对象）。
func assertKeys(t *testing.T, obj map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	slices.Sort(got)
	slices.Sort(want)
	if len(got) != len(want) {
		t.Fatalf("子对象 key 集合 = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("子对象 key 集合 = %v, want %v", got, want)
		}
	}
}

func assertKeySet(t *testing.T, r reply, want ...string) {
	t.Helper()
	got := append([]string{}, r.keys...)
	slices.Sort(got)
	want = append([]string{}, want...)
	slices.Sort(want)
	if len(got) != len(want) {
		t.Fatalf("响应 key 集合 = %v, want %v (body=%s)", got, want, r.body)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("响应 key 集合 = %v, want %v (body=%s)", got, want, r.body)
		}
	}
}

// challengeOK 领取一道 challenge 并真实求解 PoW，返回 token 与解。
func (h *harness) challengeOK(t *testing.T, siteKey string) (tok, nonce string) {
	t.Helper()
	r := h.postJSON("/api/challenge", map[string]string{"site_key": siteKey})
	if r.status != http.StatusOK {
		t.Fatalf("challenge status=%d body=%s", r.status, r.body)
	}
	seed, err := base64.RawURLEncoding.DecodeString(r.got["pow"].(map[string]any)["seed"].(string))
	if err != nil {
		t.Fatal(err)
	}
	bits := num(r.got["pow"].(map[string]any)["bits"])
	params := pow.Params{Algo: pow.Algo, Bits: uint8(bits), Seed: seed}
	for n := int64(0); n < 20_000_000; n++ {
		cand := strconv.FormatInt(n, 10)
		ok, err := params.Verify(cand)
		if err != nil {
			t.Fatalf("自身参数校验失败 %q: %v", cand, err)
		}
		if ok {
			return r.str("token"), cand
		}
	}
	t.Fatal("未能在上限内求解")
	return "", ""
}

func (h *harness) redeemOK(t *testing.T, tok, nonce string) string {
	t.Helper()
	r := h.postJSON("/api/redeem", map[string]string{"token": tok, "pow_nonce": nonce})
	if r.status != http.StatusOK {
		t.Fatalf("redeem status=%d body=%s", r.status, r.body)
	}
	return r.str("response")
}

func (h *harness) verify(secret, response string) reply {
	form := url.Values{"secret": {secret}, "response": {response}}
	return h.do(http.MethodPost, "/siteverify", strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded", "203.0.113.7:5555", nil)
}

// --- 正常流程 ---

func TestHappyPathFullFlow(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)

	r := h.verify(sentinelSecret, resp)
	if r.status != http.StatusOK || !r.got["success"].(bool) {
		t.Fatalf("siteverify 应成功：%s", r.body)
	}
	assertKeySet(t, r, "success", "challenge_ts")
	ts, err := time.Parse(time.RFC3339, r.str("challenge_ts"))
	if err != nil {
		t.Fatalf("challenge_ts 非 RFC3339：%v", err)
	}
	if ts.IsZero() || ts.After(time.Now().Add(time.Minute)) {
		t.Fatalf("challenge_ts 不合理：%s", r.str("challenge_ts"))
	}
	// 一次性：同一 response 再验证必须失败
	second := h.verify(sentinelSecret, resp)
	if second.got["success"].(bool) {
		t.Fatal("同一 response 二次验证竟然成功")
	}
	assertKeySet(t, second, "success", "error-codes")
	if !second.hasCode(ErrTimeoutOrDuplicate) {
		t.Fatalf("应 timeout-or-duplicate，实际 %v", second.codes())
	}
}

func TestChallengeResponseShapeAndNoStore(t *testing.T) {
	h := newHarness(t, nil)
	r := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-alpha", "lang": "zh-CN"})
	if r.status != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.status, r.body)
	}
	// 阶段 3A 的响应键集合：mode 恒在，question 只在 pow_question 站点出现。
	// 仍然不得出现任何暗示题库/风控存在的字段（ADR-005）。
	assertKeySet(t, r, "token", "expires_in", "pow", "mode")
	if r.str("mode") != store.ModePowOnly {
		t.Fatalf("默认站点应是 pow_only，got %q", r.str("mode"))
	}
	if _, ok := r.got["question"]; ok {
		t.Fatal("pow_only 站点不该带 question 字段")
	}
	if _, ok := r.got["challenge"]; ok {
		t.Fatal("出现了 DESIGN 未定义的 challenge 字段")
	}
	if _, ok := r.got["scores"]; ok {
		t.Fatal("出现了风控分数（阶段 3+ 才有）")
	}
	powObj, ok := r.got["pow"].(map[string]any)
	if !ok {
		t.Fatal("缺少 pow 对象")
	}
	assertKeys(t, powObj, "algo", "seed", "bits")
	if powObj["algo"] != "sha256-prefix" {
		t.Errorf("algo=%v", powObj["algo"])
	}
	if n := num(powObj["bits"]); n != 12 {
		t.Errorf("bits = %d, want 12（站点配置）", n)
	}
	if e := num(r.got["expires_in"]); e != 60 {
		t.Errorf("expires_in = %d, want 60", e)
	}
	for _, k := range []string{"Cache-Control", "X-Content-Type-Options", "Pragma"} {
		want := map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff", "Pragma": "no-cache"}[k]
		if got := r.headers.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// token 长度与形状
	if len(r.str("token")) > 4096 || !strings.HasPrefix(r.str("token"), "v1.") {
		t.Fatalf("token 形状异常：%d", len(r.str("token")))
	}
}

func TestSiteverifySupportsJSONAndFormAndIgnoresCallerHostname(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)

	jsonBody := mustJSON(t, map[string]string{
		"secret": sentinelSecret, "response": resp, "hostname": "app.example.com",
	})
	r := h.do(http.MethodPost, "/siteverify", bytes.NewReader(jsonBody), "application/json", "203.0.113.7:5555", nil)
	if !r.got["success"].(bool) {
		t.Fatalf("JSON 形式应成功：%s", r.body)
	}
	// 阶段 2：hostname 由服务端签发时绑定的来源裁决，调用方自报值**不采信也不回显**。
	// 这条流程没有 Origin（站点显式放行直接 API 调用），所以字段整体缺省。
	assertKeySet(t, r, "success", "challenge_ts")
	if strings.Contains(r.body, "app.example.com") {
		t.Fatalf("自报 hostname 被回显了：%s", r.body)
	}

	tok2, nonce2 := h.challengeOK(t, "sk-alpha")
	resp2 := h.redeemOK(t, tok2, nonce2)
	r2 := h.do(http.MethodPost, "/siteverify",
		strings.NewReader("secret="+sentinelSecret+"&response="+resp2+"&hostname=evil.example"),
		"application/x-www-form-urlencoded", "203.0.113.7:5555", nil)
	if !r2.got["success"].(bool) {
		t.Fatalf("表单形式应成功：%s", r2.body)
	}
	if strings.Contains(r2.body, "evil.example") {
		t.Fatalf("表单里的自报 hostname 被回显：%s", r2.body)
	}
}

func TestHealthzHasNoInternals(t *testing.T) {
	h := newHarness(t, nil)
	r := h.do(http.MethodGet, "/healthz", nil, "", "203.0.113.7:1", nil)
	if r.status != http.StatusOK {
		t.Fatalf("status=%d", r.status)
	}
	assertKeySet(t, r, "status")
	if r.str("status") != "ok" {
		t.Fatalf("健康响应不符：%s", r.body)
	}
	for _, leak := range []string{h.dbPath, "alpha", "sqlite", "bravo", "epocha.db", "no-store-reason"} {
		if strings.Contains(strings.ToLower(r.body), strings.ToLower(leak)) {
			t.Errorf("健康检查泄露内部信息 %q：%s", leak, r.body)
		}
	}
}

// --- 拒绝路径 ---

func TestChallengeRejectsBadRequests(t *testing.T) {
	h := newHarness(t, nil)
	cases := map[string]struct {
		payload any
		ctype   string
	}{
		"未知站点":       {map[string]string{"site_key": "sk-nope"}, "application/json"},
		"缺 site_key": {map[string]string{}, "application/json"},
		"空串":         {map[string]string{"site_key": ""}, "application/json"},
		"未知字段":       {map[string]string{"site_key": "sk-alpha", "admin": "1"}, "application/json"},
		"类型错误":       {map[string]any{"site_key": 12}, "application/json"},
		"lang 超长":    {map[string]string{"site_key": "sk-alpha", "lang": strings.Repeat("a", 200)}, "application/json"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := mustJSON(t, tc.payload)
			r := h.do(http.MethodPost, "/api/challenge", bytes.NewReader(b), tc.ctype, "203.0.113.7:5555", nil)
			if r.status != http.StatusBadRequest || !r.hasCode(CodeInvalidRequest) {
				t.Fatalf("应 400 invalid-request，实际 %d %s", r.status, r.body)
			}
			assertKeySet(t, r, "error-codes")
		})
	}
	// 畸形 JSON / 非 JSON / 空体
	for _, body := range []string{"{", "[1,2", "", `{"site_key":}`, "not json at all"} {
		r := h.do(http.MethodPost, "/api/challenge", strings.NewReader(body), "application/json", "203.0.113.7:5555", nil)
		if r.status != http.StatusBadRequest {
			t.Fatalf("畸形请求体 %q 应 400，实际 %d", body, r.status)
		}
	}
}

func TestOversizeBodiesRejected(t *testing.T) {
	h := newHarness(t, nil)
	huge := strings.NewReader(strings.Repeat("A", maxBodyPublic+1))
	r := h.do(http.MethodPost, "/api/challenge", huge, "application/json", "203.0.113.7:5555", nil)
	if r.status != http.StatusBadRequest {
		t.Fatalf("公开端点超限应 400，实际 %d", r.status)
	}
	r = h.do(http.MethodPost, "/api/redeem", strings.NewReader(strings.Repeat("A", maxBodyPublic+1)),
		"application/json", "203.0.113.7:5555", nil)
	if r.status != http.StatusBadRequest {
		t.Fatalf("redeem 超限应 400，实际 %d", r.status)
	}
	r = h.do(http.MethodPost, "/siteverify", strings.NewReader("secret="+strings.Repeat("A", maxBodyVerify+1)),
		"application/x-www-form-urlencoded", "203.0.113.7:5555", nil)
	// /siteverify 的契约面是 reCAPTCHA 语义：判定失败也回 HTTP 200 + 错误码，
	// 但必须拒绝处理（bad-request），且不得消费任何状态。
	if r.status != http.StatusOK || !r.hasCode(ErrBadRequest) {
		t.Fatalf("siteverify 超限应 200 + bad-request，实际 %d %s", r.status, r.body)
	}
	if r.got["success"].(bool) {
		t.Fatal("超限请求不应成功")
	}
	assertKeySet(t, r, "success", "error-codes")
}

func TestMethodNotAllowed(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{"/api/challenge", "/api/redeem", "/siteverify"} {
		r := h.do(http.MethodGet, path, nil, "", "203.0.113.7:5555", nil)
		if r.status != http.StatusMethodNotAllowed {
			t.Errorf("GET %s 应 405，实际 %d", path, r.status)
		}
	}
}

func TestRedeemRejectsEverythingButAValidSolution(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")

	cases := map[string]map[string]string{
		"缺 token":   {"pow_nonce": nonce},
		"缺 nonce":   {"token": tok},
		"空 nonce":   {"token": tok, "pow_nonce": ""},
		"非数字 nonce": {"token": tok, "pow_nonce": "zzz"},
		"超长 nonce":  {"token": tok, "pow_nonce": strings.Repeat("9", 40)},
		"超上限 nonce": {"token": tok, "pow_nonce": "9999999999999999"},
		"错误解":       {"token": tok, "pow_nonce": "1"},
		"篡改 token":  {"token": tok[:len(tok)-3] + "AAA", "pow_nonce": nonce},
		"未知 kid":    {"token": "v1." + strings.Repeat("0", 16) + rest(tok), "pow_nonce": nonce},
		"版本伪造":      {"token": strings.Replace(tok, "v1.", "v2.", 1), "pow_nonce": nonce},
		"多余字段":      {"token": tok, "pow_nonce": nonce, "bits": "24"},
	}
	// 形态/长度问题 → invalid-request；签名、PoW、状态问题 → invalid-challenge。
	shapeCases := map[string]bool{"缺 token": true, "缺 nonce": true, "空 nonce": true,
		"超长 nonce": true, "多余字段": true}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			want := CodeInvalidChallenge
			if shapeCases[name] {
				want = CodeInvalidRequest
			}
			r := h.do(http.MethodPost, "/api/redeem", bytes.NewReader(mustJSON(t, payload)),
				"application/json", "203.0.113.7:5555", nil)
			if r.status != http.StatusBadRequest || !r.hasCode(want) {
				t.Fatalf("应 400 %s，实际 %d %s", want, r.status, r.body)
			}
			assertKeySet(t, r, "error-codes")
		})
	}
}

func rest(tok string) string {
	if i := strings.Index(tok, "."); i >= 0 {
		if j := strings.Index(tok[i+1:], "."); j >= 0 {
			return tok[i+1+j:]
		}
	}
	return ".x"
}

// 难度/TTL 由服务端签名并落库，客户端无法改动：篡改 payload 即验签失败。
func TestClientCannotModifyDifficultyOrTTL(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	payload := strings.Split(tok, ".")[2]
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(claims["bits"]) != "12" {
		t.Fatalf("token 内 bits 应为 12：%v", claims["bits"])
	}
	claims["bits"] = 4
	claims["exp"] = time.Now().Add(48 * time.Hour).Unix()
	tampered, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Split(tok, ".")
	mutated[2] = base64.RawURLEncoding.EncodeToString(tampered)
	r := h.postJSON("/api/redeem", map[string]string{"token": strings.Join(mutated, "."), "pow_nonce": nonce})
	if r.status != http.StatusBadRequest {
		t.Fatalf("改难度/TTL 必须失败，实际 %d %s", r.status, r.body)
	}
}

func TestExpiredChallengeRejected(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	h.setClock(2 * time.Minute) // 超过 1 分钟 challenge TTL
	r := h.postJSON("/api/redeem", map[string]string{"token": tok, "pow_nonce": nonce})
	if r.status != http.StatusBadRequest {
		t.Fatalf("过期 challenge 应拒绝，实际 %d %s", r.status, r.body)
	}
	// 过期不得被"复活"：再走一次完整流程必须成功，说明不是全局熔断
	tok2, nonce2 := h.challengeOK(t, "sk-alpha")
	if resp := h.redeemOK(t, tok2, nonce2); resp == "" {
		t.Fatal("新 challenge 兑换失败")
	}
}

func TestExpiredResponseRejected(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)
	h.setClock(3 * time.Minute)
	r := h.verify(sentinelSecret, resp)
	if r.got["success"].(bool) {
		t.Fatal("过期 response 不应通过")
	}
	if !r.hasCode(ErrTimeoutOrDuplicate) && !r.hasCode(ErrInvalidInputResponse) {
		t.Fatalf("错误码不符：%v", r.codes())
	}
}

func TestSiteverifyMissingAndBadInputs(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)

	cases := []struct {
		secret, response, want string
	}{
		{"", resp, ErrMissingInputSecret},
		{sentinelSecret, "", ErrMissingInputResponse},
		{"wrong-secret-value", resp, ErrInvalidInputSecret},
		{sentinelSecret, "garbage-token", ErrInvalidInputResponse},
		{sentinelSecret, strings.Repeat("A", 5000), ErrMissingInputResponse},
	}
	for _, tc := range cases {
		r := h.verify(tc.secret, tc.response)
		if r.status != http.StatusOK {
			t.Fatalf("siteverify 失败也必须是 HTTP 200（reCAPTCHA 语义），实际 %d", r.status)
		}
		if r.got["success"].(bool) {
			t.Fatalf("应失败：%s / %s", redact(tc.secret), redact(tc.response))
		}
		assertKeySet(t, r, "success", "error-codes")
		if !r.hasCode(tc.want) {
			t.Fatalf("want %s，got %v", tc.want, r.codes())
		}
	}
}

// 错误 secret 不得消费别的站点的合法 token，也不得消费本站点的。
func TestWrongSecretDoesNotConsume(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)

	// B 站点的正确 secret，配 A 站点签发的 token
	rB := h.verify(sentinelSecretB, resp)
	if rB.got["success"].(bool) || !rB.hasCode(ErrInvalidInputResponse) {
		t.Fatalf("跨站点 token 应 invalid-input-response，实际 %s", rB.body)
	}
	// 明显错误的 secret
	rBad := h.verify("totally-wrong-secret-value", resp)
	if !rBad.hasCode(ErrInvalidInputSecret) {
		t.Fatalf("应 invalid-input-secret，实际 %s", rBad.body)
	}
	// A 站点正确 secret 仍能成功：前面两次失败都没消费
	rOK := h.verify(sentinelSecret, resp)
	if !rOK.got["success"].(bool) {
		t.Fatalf("失败尝试竟消费了合法 token：%s", rOK.body)
	}
}

// 站点绑定：B 站点的 secret 永远无法消费 A 站点 token，反之亦然。
func TestCrossSiteTokenRejected(t *testing.T) {
	h := newHarness(t, nil)
	tokB, nonceB := h.challengeOK(t, "sk-bravo")
	respB := h.redeemOK(t, tokB, nonceB)
	if r := h.verify(sentinelSecret, respB); r.got["success"].(bool) || !r.hasCode(ErrInvalidInputResponse) {
		t.Fatalf("A 站点 secret 不应消费 B 站点 token：%s", r.body)
	}
	if r := h.verify(sentinelSecretB, respB); !r.got["success"].(bool) {
		t.Fatalf("B 站点自己的 secret 应能消费：%s", r.body)
	}
}

func TestChallengeAttemptsAreBoundedThenInvalidated(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	for i := 1; i <= store.MaxFailedAttempts; i++ {
		r := h.postJSON("/api/redeem", map[string]string{"token": tok, "pow_nonce": "1"})
		if r.status != http.StatusBadRequest {
			t.Fatalf("第 %d 次错误解应 400，实际 %d", i, r.status)
		}
	}
	// 次数用尽后，即使提交正确解也必须失败
	r := h.postJSON("/api/redeem", map[string]string{"token": tok, "pow_nonce": nonce})
	if r.status != http.StatusBadRequest {
		t.Fatalf("尝试耗尽后仍兑换成功：%d %s", r.status, r.body)
	}
	n, err := h.db.CountChallenges(context.Background(), "invalid")
	if err != nil || n != 1 {
		t.Fatalf("应有 1 条 invalid challenge，实际 %d (%v)", n, err)
	}
}

// --- 并发与持久化 ---

func TestConcurrentRedeemExactlyOneSucceeds(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")

	const n = 40
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	wins := 0
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b := mustJSON(t, map[string]string{"token": tok, "pow_nonce": nonce})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/redeem", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			h.handler().ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d 路并发 redeem 成功 %d 次，应为 1", n, wins)
	}
}

func TestConcurrentSiteverifyExactlyOneSucceeds(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)

	const n = 40
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	wins := 0
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			form := url.Values{"secret": {sentinelSecret}, "response": {resp}}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/siteverify", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			h.handler().ServeHTTP(rec, req)
			var got map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if ok, _ := got["success"].(bool); ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d 路并发 siteverify 成功 %d 次，应为 1", n, wins)
	}
}

// 重启（同一路径重开库）后不得重放：证明状态在 SQLite 而不是内存。
func TestReplayAfterServerRestart(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)
	if r := h.verify(sentinelSecret, resp); !r.got["success"].(bool) {
		t.Fatalf("首次验证应成功：%s", r.body)
	}

	// 模拟进程重启：关掉句柄，用同一路径重建整套服务
	for _, f := range h.cleanup {
		f()
	}
	h.cleanup = nil
	reopened, err := store.Open(context.Background(), h.dbPath, testPepper)
	if err != nil {
		t.Fatalf("重开库: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	signer, err := token.NewSigner(bytes.Repeat([]byte{0x5a}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DBPath: h.dbPath, MasterKey: bytes.Repeat([]byte{0x5a}, 32), Pepper: testPepper,
		ChallengeTTL: time.Minute, ResponseTTL: time.Minute,
		RateIPRPM: 100_000, RateSiteRPM: 100_000, RateMaxState: 4096,
		MaxActiveChallenges: 10_000, MaxActivePerSite: 5_000,
	}
	srv2, err := New(cfg, reopened, signer, testRegistry(t, cfg.MasterKey), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"secret": {sentinelSecret}, "response": {resp}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/siteverify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv2.Handler().ServeHTTP(rec, req)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if ok, _ := got["success"].(bool); ok {
		t.Fatal("重启后重放成功：防重放状态未持久化")
	}
	if codes, _ := got["error-codes"].([]any); len(codes) != 1 || codes[0] != ErrTimeoutOrDuplicate {
		t.Fatalf("重启后应 timeout-or-duplicate，实际 %v", got["error-codes"])
	}
}

// --- 来源地址与限流 ---

func TestSpoofedProxyHeadersIgnoredByDefault(t *testing.T) {
	h := newHarness(t, nil)
	spoof := map[string]string{"CF-Connecting-IP": "198.51.100.7", "X-Forwarded-For": "192.0.2.1"}
	first := h.do(http.MethodPost, "/api/challenge", jsonBody(t, map[string]string{"site_key": "sk-alpha"}),
		"application/json", "203.0.113.7:1111", spoof)
	if first.status != http.StatusOK {
		t.Fatalf("首次应放行：%d", first.status)
	}
	// 同一连接地址、不同伪造头：必须落到同一个限流桶（否则伪造头可无限绕限）
	for i := 2; i <= 6; i++ {
		spoof2 := map[string]string{"CF-Connecting-IP": fmt.Sprintf("198.51.100.%d", i)}
		r := h.do(http.MethodPost, "/api/challenge", jsonBody(t, map[string]string{"site_key": "sk-alpha"}),
			"application/json", "203.0.113.7:1111", spoof2)
		if r.status == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次就被限流，说明伪造头影响了分桶", i)
		}
	}
}

func TestRateLimitPerIPWithRetryAfter(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.RateIPRPM = 3 })
	var last, last429 reply
	for i := range 6 {
		last = h.postJSON("/api/challenge", map[string]string{"site_key": "sk-alpha"})
		if i < 3 && last.status != http.StatusOK {
			t.Fatalf("第 %d 次应放行，实际 %d", i+1, last.status)
		}
		if last.status == http.StatusTooManyRequests {
			last429 = last
		}
	}
	if last.status != http.StatusTooManyRequests || !last.hasCode(CodeRateLimited) {
		t.Fatalf("应 429 rate-limited，实际 %d %s", last.status, last.body)
	}
	if last429.status != http.StatusTooManyRequests {
		t.Fatal("未捕获到 429 响应")
	}
	if ra := last429.headers.Get("Retry-After"); ra == "" {
		t.Errorf("429 必须带 Retry-After，实际头=%v", last429.headers)
	} else if _, err := strconv.Atoi(ra); err != nil {
		t.Errorf("Retry-After 应为秒数，实际 %q", ra)
	}
	// 另一来源地址不受影响
	other := h.do(http.MethodPost, "/api/challenge", jsonBody(t, map[string]string{"site_key": "sk-alpha"}),
		"application/json", "192.0.2.99:5555", nil)
	if other.status != http.StatusOK {
		t.Fatalf("不同来源应独立计数，实际 %d", other.status)
	}
}

func TestRateLimitPerSite(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.RateSiteRPM = 2 })
	// 预热里的每一次请求都必须落在 {200, 429} 之内。
	// 老写法把其它状态码也当成"没触发限流"继续循环，于是一次瞬时的 500/503（库忙、池空）
	// 会让"第 4 次应该 429"变成"第 4 次其实第一次都没计数"——实测在高负载下会以
	// "站点维度限流未生效：200" 的形态出现，把原因指错到限流器上。这里直接判死因。
	for i := range 3 {
		if r := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-alpha"}); r.status == http.StatusTooManyRequests {
			break
		} else if r.status != http.StatusOK {
			t.Fatalf("预热第 %d 次请求既不是 200 也不是 429：%d %s（限流断言的前提不成立）", i+1, r.status, r.body)
		}
	}
	blocked := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-alpha"})
	if blocked.status != http.StatusTooManyRequests {
		t.Fatalf("站点维度限流未生效：%d %s", blocked.status, blocked.body)
	}
	other := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-bravo"})
	if other.status != http.StatusOK {
		t.Fatalf("另一站点不应受影响：%d %s", other.status, other.body)
	}
}

func TestTrustedProxyHeaderIsHonored(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.TrustProxy = true
		c.TrustedProxies = []string{"198.51.100.0/24"}
	})
	r := h.do(http.MethodPost, "/api/challenge", jsonBody(t, map[string]string{"site_key": "sk-alpha"}),
		"application/json", "198.51.100.20:443", map[string]string{"CF-Connecting-IP": "203.0.113.77"})
	if r.status != http.StatusOK {
		t.Fatalf("受信代理路径应正常：%d", r.status)
	}
	// 未受信来源的伪造头被忽略，两个不同伪造头应共用同一连接地址的桶
	h2 := newHarness(t, func(c *config.Config) {
		c.TrustProxy = true
		c.TrustedProxies = []string{"198.51.100.0/24"}
		c.RateIPRPM = 2
	})
	for i, ip := range []string{"8.8.8.8", "9.9.9.9", "1.1.1.1"} {
		rec := h2.do(http.MethodPost, "/api/challenge", jsonBody(t, map[string]string{"site_key": "sk-alpha"}),
			"application/json", "203.0.113.5:5555", map[string]string{"CF-Connecting-IP": ip})
		if i == 2 && rec.status != http.StatusTooManyRequests {
			t.Fatal("未受信来源伪造 CF 头以绕限，应命中同一个桶并被限流")
		}
	}
}

// 限流状态必须有界（1GB 内存的生存条件）。
func TestLimiterStatesAreBoundedUnderRandomSources(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.RateMaxState = 64; c.RateIPRPM = 5 })
	for i := range 2_000 {
		h.do(http.MethodPost, "/api/challenge", jsonBody(t, map[string]string{"site_key": "sk-alpha"}),
			"application/json", fmt.Sprintf("203.0.%d.%d:5555", i/254+1, i%254), nil)
	}
	total := h.srv.ipLimit.Len() + h.srv.siteLimit.Len()
	if total > 64+1 {
		t.Fatalf("限流状态条目 %d 超过配置上限 64+1", total)
	}
}

// --- 泄露扫描 ---

func TestNothingLeaksSecretsOrKeys(t *testing.T) {
	h := newHarness(t, nil)
	// 走一遍完整流程 + 各种失败路径
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)
	h.verify(sentinelSecret, resp)         // 成功
	h.verify(sentinelSecret, resp)         // 重放
	h.verify("wrong-secret", resp)         // 错 secret
	h.verify(sentinelSecret, "junk-token") // 坏 token
	h.postJSON("/api/redeem", map[string]string{"token": tok, "pow_nonce": "1"})
	h.postJSON("/api/challenge", map[string]string{"site_key": "sk-nope"})
	h.do(http.MethodGet, "/healthz", nil, "", "203.0.113.7:1", nil)

	leaks := []string{
		sentinelSecret, sentinelSecretB,
		h.dbPath,
	}
	logged := h.logs.String()
	for _, l := range leaks {
		if l == "" {
			continue
		}
		if strings.Contains(logged, l) {
			t.Errorf("日志中出现敏感值片段 %q", l)
		}
	}
	if strings.Contains(logged, "error-codes\":["+ErrInvalidInputSecret) {
		t.Error("日志不应包含对外错误码之外的判定细节")
	}
}

// num 读取用 UseNumber 解码的 JSON 数字。
func num(v any) int64 {
	n, ok := v.(json.Number)
	if !ok {
		return -1
	}
	i, err := n.Int64()
	if err != nil {
		return -1
	}
	return i
}

func jsonBody(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	return bytes.NewReader(mustJSON(t, v))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// redact 只用于失败日志，避免把哨兵值原样写进测试输出。
func redact(v string) string {
	if len(v) <= 6 {
		return "<short>"
	}
	return v[:3] + "…"
}

// TestDatabaseFailureFailsClosed 覆盖红线 12：库故障时不得返回成功。
// 关掉句柄是最省事的故障注入——所有读写都会失败。
func TestDatabaseFailureFailsClosed(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)

	if err := h.db.Close(); err != nil {
		t.Fatalf("关闭库以注入故障: %v", err)
	}
	if r := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-alpha"}); r.status != http.StatusInternalServerError {
		t.Fatalf("库故障时签发必须 5xx，实际 %d %s", r.status, r.body)
	}
	if r := h.postJSON("/api/redeem", map[string]string{"token": tok, "pow_nonce": nonce}); r.status != http.StatusInternalServerError {
		t.Fatalf("库故障时兑换必须 5xx，实际 %d %s", r.status, r.body)
	}
	r := h.postJSON("/siteverify", map[string]string{"secret": sentinelSecret, "response": resp})
	if r.status != http.StatusInternalServerError {
		t.Fatalf("库故障时验证必须 5xx，实际 %d %s", r.status, r.body)
	}
	if strings.Contains(r.body, `"success":true`) {
		t.Fatalf("库故障竟返回 success=true: %s", r.body)
	}
	// fail closed 还要求"重放不会因故障而成功"：同一个 token 再验一次仍是 5xx，不是 200/true。
	if r2 := h.postJSON("/siteverify", map[string]string{"secret": sentinelSecret, "response": resp}); r2.status != http.StatusInternalServerError {
		t.Fatalf("故障期间重放应继续 5xx，实际 %d %s", r2.status, r2.body)
	}
}

// TestSiteverifyUsesRecaptchaErrorFamily 固定错误码族边界（DESIGN §10.1）：
// /siteverify 的服务端故障只能出现唯一扩展码 internal-error；/api/* 不得出现该码。
func TestSiteverifyUsesRecaptchaErrorFamily(t *testing.T) {
	h := newHarness(t, nil)
	tok, nonce := h.challengeOK(t, "sk-alpha")
	resp := h.redeemOK(t, tok, nonce)
	if err := h.db.Close(); err != nil {
		t.Fatal(err)
	}

	vr := h.postJSON("/siteverify", map[string]string{"secret": sentinelSecret, "response": resp})
	if !vr.hasCode(ErrInternalError) {
		t.Fatalf("/siteverify 内部错误应返回 internal-error，got %v (%s)", vr.codes(), vr.body)
	}
	assertKeySet(t, vr, "error-codes", "success")

	rd := h.postJSON("/api/redeem", map[string]string{"token": tok, "pow_nonce": nonce})
	if rd.hasCode(ErrInternalError) {
		t.Fatalf("/api/redeem 不得使用 reCAPTCHA 扩展码，got %v", rd.codes())
	}
	if !rd.hasCode(CodeInvalidRequest) {
		t.Fatalf("/api/redeem 内部错误应是 invalid-request，got %v", rd.codes())
	}
}

// TestChallengeCapReturnsRateLimited 验证存量上限触发时对外表现为限流，而不是 5xx 或假成功。
func TestChallengeCapReturnsRateLimited(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.MaxActivePerSite = 1
		c.MaxActiveChallenges = 100
	})
	if r := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-alpha"}); r.status != http.StatusOK {
		t.Fatalf("首签应成功，got %d %s", r.status, r.body)
	}
	r := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-alpha"})
	if r.status != http.StatusTooManyRequests || !r.hasCode(CodeRateLimited) {
		t.Fatalf("活动量到顶应 429 rate-limited，got %d %s", r.status, r.body)
	}
	if r.headers.Get("Retry-After") == "" {
		t.Error("429 必须带 Retry-After")
	}
	// 另一个站点在单站额度内仍可签发（说明封顶是存量而非全局禁用）
	if r2 := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-bravo"}); r2.status != http.StatusOK {
		t.Fatalf("另一站点应可签发，got %d %s", r2.status, r2.body)
	}
}

// TestUnknownSiteKeysDoNotPolluteSiteLimiter 是阶段 1 复核发现的真实问题的回归测试：
// 站点维度限流曾经按调用方自报的 site_key 计数，攻击者随机编造 site_key 就能挤掉
// 真实站点的限流状态、使其计数不断归零。现在必须按库中站点 id 计数。
func TestUnknownSiteKeysDoNotPolluteSiteLimiter(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.RateSiteRPM = 2
		c.RateMaxState = 64
	})
	for i := 0; i < 40; i++ {
		r := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-bogus-" + strconv.Itoa(i)})
		if r.status != http.StatusBadRequest {
			t.Fatalf("未知 site_key 应 400，got %d %s", r.status, r.body)
		}
	}
	if n := h.srv.siteLimit.Len(); n != 0 {
		t.Fatalf("未知 site_key 不得创建站点维度限流条目，现有 %d 条", n)
	}
	// 真实站点仍然只有 2 次/分钟：伪造流量没有把它的服务桶冲掉
	for i := 0; i < 2; i++ {
		if r := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-alpha"}); r.status != http.StatusOK {
			t.Fatalf("第 %d 次合法签发应放行，got %d %s", i+1, r.status, r.body)
		}
	}
	if r := h.postJSON("/api/challenge", map[string]string{"site_key": "sk-alpha"}); r.status != http.StatusTooManyRequests {
		t.Fatalf("超出站点 RPM 应 429，got %d %s", r.status, r.body)
	}
}
