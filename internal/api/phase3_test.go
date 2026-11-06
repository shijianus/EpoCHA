package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/dispatch"
	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/pool"
	"github.com/shijianus/EpoCHA/internal/store"
)

func (h *harness) siteWithPool(t *testing.T, name, siteKey string, pool []string, difficulty int) *store.Site {
	t.Helper()
	site, _, err := h.db.CreateSite(context.Background(), name, siteKey, store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion,
		GeneratorID: pool[0], GeneratorPool: pool, Difficulty: difficulty,
		AllowMissingOrigin: true,
	})
	if err != nil {
		t.Fatalf("CreateSite(pool): %v", err)
	}
	return site
}

func (h *harness) challengeReplyWith(body map[string]any) reply {
	return h.postJSON("/api/challenge", body)
}

func accessibleFixture(t *testing.T, difficulty int, seed string) seededQuestion {
	t.Helper()
	g, err := generator.NewAccessibleChoice(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := "epocha-api-accessible-" + seed
	answer, err := generator.AccessibleChoiceAnswerFrom(evalStream(t, stream), difficulty)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty}, evalStream(t, stream))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatal("无障碍夹具答案与 verifier 不匹配")
	}
	return seededQuestion{
		GeneratorID: generator.AccessibleChoiceID, Payload: payloadMap(t, q.Payload),
		Answer: answer, Verifier: q.Verifier,
	}
}

func TestSchedulerPicksFromSitePool(t *testing.T) {
	h := newHarness(t, nil)
	pool := []string{generator.ParallaxID, generator.BankShotID}
	site := h.siteWithPool(t, "pool", "sk-pool", pool, 2)
	h.seedQuestions(t, 2, parallaxFixture(t, 2, "pool"), bankShotFixture(t, 2, "pool"))
	h.srv.scheduler = dispatch.NewSchedulerWithIntn(func(n int) (int, error) { return 0, nil })

	r := h.challengeReply("sk-pool")
	q := questionOf(t, r)
	if q["generator"] != generator.ParallaxID {
		t.Fatalf("确定性调度应选池内第一项，got %v", q["generator"])
	}
	if site.Policy.GeneratorPool[0] != generator.ParallaxID {
		t.Fatal("站点池未持久化")
	}
}

func TestRefreshPenaltyPoolPrewarmEndToEnd(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "prewarm", "sk-prewarm", []string{generator.BankShotID}, 3)
	cfg := pool.DefaultConfig()
	cfg.Prewarm = true
	cfg.LowWatermark = 2
	cfg.HighWatermark = 4
	cfg.ImageLowWatermark = 2
	cfg.ImageHighWatermark = 4
	cfg.BatchPerTick = 4
	cfg.MaxRows = 200
	w, err := pool.New(h.db, h.gens, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := w.Tick(context.Background()); err != nil {
			t.Fatalf("prewarm Tick: %v", err)
		}
	}
	ready := func(d int) int {
		n, err := h.db.ReadyCount(context.Background(), store.PoolSpec{
			GeneratorID: generator.BankShotID, Version: "v1", Difficulty: d,
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if ready(4) == 0 {
		t.Fatalf("池未预热 d4: d3=%d d4=%d d5=%d", ready(3), ready(4), ready(5))
	}
	for i := 1; i <= 2; i++ {
		r := h.challengeReplyWith(map[string]any{"site_key": "sk-prewarm", "session": "prewarm-session"})
		if r.status != http.StatusOK {
			t.Fatalf("第 %d 次签发失败: %d %s", i, r.status, r.body)
		}
		if bits := num(r.got["pow"].(map[string]any)["bits"]); bits != 12 {
			t.Fatalf("第 %d 次 bits = %v", i, bits)
		}
	}
	r := h.challengeReplyWith(map[string]any{"site_key": "sk-prewarm", "session": "prewarm-session"})
	if r.status != http.StatusOK {
		t.Fatalf("升档后仍应签发，got %d %s", r.status, r.body)
	}
	if bits := num(r.got["pow"].(map[string]any)["bits"]); bits != 14 {
		t.Fatalf("升档后 bits = %v，期望 14", bits)
	}
}

func TestRefreshPenaltyRaisesFloorAndPoW(t *testing.T) {
	h := newHarness(t, nil)
	site := h.siteWithPool(t, "refresh", "sk-refresh", []string{generator.BankShotID}, 3)
	h.seedQuestions(t, 3, bankShotFixture(t, 3, "refresh-3a"), bankShotFixture(t, 3, "refresh-3b"))
	h.seedQuestions(t, 4, bankShotFixture(t, 4, "refresh-4a"), bankShotFixture(t, 4, "refresh-4b"), bankShotFixture(t, 4, "refresh-4c"))

	for i := 1; i <= 2; i++ {
		r := h.challengeReplyWith(map[string]any{"site_key": "sk-refresh", "session": "refresh-session"})
		if r.status != http.StatusOK {
			t.Fatalf("第 %d 次签发失败: %d %s", i, r.status, r.body)
		}
		if bits := num(r.got["pow"].(map[string]any)["bits"]); bits != 12 {
			t.Fatalf("第 %d 次 bits = %v，期望 12", i, bits)
		}
	}
	r := h.challengeReplyWith(map[string]any{"site_key": "sk-refresh", "session": "refresh-session"})
	if r.status != http.StatusOK {
		t.Fatalf("第 3 次签发应成功: %d %s", r.status, r.body)
	}
	if bits := num(r.got["pow"].(map[string]any)["bits"]); bits != 14 {
		t.Fatalf("第 3 次 bits = %v，期望 14（刷新惩罚）", bits)
	}
	if site.Policy.Difficulty != 3 {
		t.Fatal("惩罚不应改写站点策略")
	}

	// 第 6 次进入冷却：必须在选池之前返回 429。
	for i := 4; i <= 6; i++ {
		r = h.challengeReplyWith(map[string]any{"site_key": "sk-refresh", "session": "refresh-session"})
		if i < 6 && r.status != http.StatusOK {
			t.Fatalf("第 %d 次签发失败: %d %s", i, r.status, r.body)
		}
	}
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("第 6 次应冷却 429，got %d %s", r.status, r.body)
	}
}

func TestTelemetryNeverAltersVerdict(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "telemetry", "sk-tel", []string{generator.BankShotID}, 2)
	fx := bankShotFixture(t, 2, "telemetry")
	h.seedQuestions(t, 2, fx)
	r := h.challengeReply("sk-tel")
	questionOf(t, r)
	nonce := h.solvePow(t, r)
	tel := map[string]any{"total_ms": 180, "first_input_ms": 10, "event_count": 1, "pointer_events": 1, "key_events": 0, "focus_blur": 0}
	rr := h.postJSON("/api/redeem", map[string]any{
		"token": r.str("token"), "pow_nonce": nonce, "answer": fx.Answer, "telemetry": tel,
	})
	if rr.status != http.StatusOK {
		t.Fatalf("异常遥测不应拒绝正确答案: %d %s", rr.status, rr.body)
	}
}

func TestTelemetryCannotRescueWrongAnswer(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "telemetry", "sk-tel-wrong", []string{generator.BankShotID}, 2)
	fx := bankShotFixture(t, 2, "telemetry-wrong")
	h.seedQuestions(t, 2, fx)
	r := h.challengeReply("sk-tel-wrong")
	questionOf(t, r)
	nonce := h.solvePow(t, r)
	a, _ := strconv.Atoi(fx.Answer)
	wrong := strconv.Itoa((a + 900) % 3600)
	tel := map[string]any{"total_ms": 4000, "first_input_ms": 900, "event_count": 40, "pointer_events": 30, "key_events": 10}
	rr := h.postJSON("/api/redeem", map[string]any{
		"token": r.str("token"), "pow_nonce": nonce, "answer": wrong, "telemetry": tel,
	})
	if rr.status == http.StatusOK {
		t.Fatal("遥测不得把错误答案放行")
	}
	if rr := h.postJSON("/api/redeem", map[string]any{
		"token": r.str("token"), "pow_nonce": nonce, "answer": fx.Answer, "telemetry": tel,
	}); rr.status == http.StatusOK {
		t.Fatal("答错后 challenge 已终结，正确答案也不应复用同一 token")
	}
}

func TestMalformedTelemetryDoesNotConsumeAttempt(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "telemetry", "sk-tel-bad", []string{generator.BankShotID}, 2)
	fx := bankShotFixture(t, 2, "telemetry-bad")
	h.seedQuestions(t, 2, fx)
	r := h.challengeReply("sk-tel-bad")
	questionOf(t, r)
	nonce := h.solvePow(t, r)
	bad := h.postJSON("/api/redeem", map[string]any{
		"token": r.str("token"), "pow_nonce": nonce, "answer": fx.Answer,
		"telemetry": map[string]any{"total_ms": -1},
	})
	if bad.status != http.StatusBadRequest {
		t.Fatalf("畸形遥测应 400，got %d %s", bad.status, bad.body)
	}
	good := h.postJSON("/api/redeem", map[string]any{
		"token": r.str("token"), "pow_nonce": nonce, "answer": fx.Answer,
		"telemetry": map[string]any{"total_ms": 3000, "first_input_ms": 600, "event_count": 10, "pointer_events": 8, "key_events": 2},
	})
	if good.status != http.StatusOK {
		t.Fatalf("畸形遥测不应消耗机会；正确答案应通过: %d %s", good.status, good.body)
	}
}

func TestAccessibilityChannelGateAndNoFallback(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "a11y", "sk-a11y", []string{generator.AccessibleChoiceID}, 2)
	fx := accessibleFixture(t, 2, "gate")
	h.seedQuestions(t, 2, fx)

	r := h.challengeReplyWith(map[string]any{
		"site_key": "sk-a11y", "accessibility": true, "session": "a11y-session",
	})
	if r.status != http.StatusOK {
		t.Fatalf("首次无障碍签发失败: %d %s", r.status, r.body)
	}
	q := questionOf(t, r)
	if q["answer_type"] != "text" || len(q["options"].([]any)) != 12 {
		t.Fatalf("无障碍题面形状不符: %v", q)
	}
	second := h.challengeReplyWith(map[string]any{
		"site_key": "sk-a11y", "accessibility": true, "session": "a11y-session",
	})
	if second.status != http.StatusTooManyRequests {
		t.Fatalf("24h 内第二次应 429，got %d %s", second.status, second.body)
	}

	// 站点池没有 accessible 候选：显式请求必须 503，不能回落到普通题/PoW-only。
	h2 := newHarness(t, nil)
	h2.siteWithPool(t, "noa11y", "sk-noa11y", []string{generator.BankShotID}, 2)
	h2.seedQuestions(t, 2, bankShotFixture(t, 2, "noa11y"))
	no := h2.challengeReplyWith(map[string]any{"site_key": "sk-noa11y", "accessibility": true})
	if no.status != http.StatusServiceUnavailable {
		t.Fatalf("无 accessible 候选应 503，got %d %s", no.status, no.body)
	}
}

func TestAccessibleRedeemIsOneTime(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "a11y", "sk-a11y-redeem", []string{generator.AccessibleChoiceID}, 2)
	fx := accessibleFixture(t, 2, "redeem")
	h.seedQuestions(t, 2, fx)
	r := h.challengeReplyWith(map[string]any{
		"site_key": "sk-a11y-redeem", "accessibility": true, "session": "a11y-redeem",
	})
	questionOf(t, r)
	nonce := h.solvePow(t, r)
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status != http.StatusOK {
		t.Fatalf("无障碍正确答案未通过: %d %s", rr.status, rr.body)
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("无障碍 challenge 重放不应通过")
	}
}
