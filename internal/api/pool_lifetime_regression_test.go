package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

// TestChallengePrefetchMatchesClaimLifetime 钉住候选预检与领取事务的寿命边界一致（ADR-034 §6）：
// 池里同时有 A（短命题：还活着但撑不满 ChallengeTTL）与 B（足寿命题）时，
// 预检不得把 A 算进候选——调度器选中 A 后领取必然失败，明明 B 可领却给调用方 503。
// 权重取自 dispatch 目录：text-image 低风险带 0.8×base1 远高于 arith 0.5×base0.2，
// 旧实现会以 ~89% 概率选中 A ⇒ 12 连发下旧代码必然红（漏绿概率 ~1e-12）。
func TestChallengePrefetchMatchesClaimLifetime(t *testing.T) {
	h := newHarness(t, nil)
	site, _, err := h.db.CreateSite(t.Context(), "lifetime", "sk-lifetime", store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion,
		GeneratorID: generator.TextImageID, Difficulty: 3,
		GeneratorPool:      []string{generator.TextImageID, generator.ArithID},
		AllowMissingOrigin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	short := contractFixture(t, h, generator.TextImageID, "ABCD23")
	short.Lifetime = 30 * time.Second // < ChallengeTTL(3min)；旧预检按 now 会把它当可用
	longArith := contractFixture(t, h, generator.ArithID, "17")
	seeds := []seededQuestion{short}
	for range 12 {
		seeds = append(seeds, longArith)
	}
	h.seedQuestions(t, site.Policy.Difficulty, seeds...)

	served := map[string]int{}
	for _, id := range site.Policy.GeneratorIDs() {
		g, err := h.gens.Lookup(id)
		if err != nil {
			t.Logf("diag %s: not registered: %v", id, err)
			continue
		}
		n, err := h.db.ReadyCount(t.Context(), store.PoolSpec{GeneratorID: id, Version: g.Version(), Difficulty: site.Policy.Difficulty}, h.clock.Add(site.Policy.ChallengeTTL))
		t.Logf("diag %s: ready(after challenge expiry)=%d err=%v ttl=%v", id, n, err, site.Policy.ChallengeTTL)
	}
	for range 12 {
		r := h.challengeReply("sk-lifetime")
		if r.status != http.StatusOK {
			t.Logf("diag server logs:\n%s", h.logs.String())
			t.Fatalf("同池存在足寿命题时领题不得 503：%d %s", r.status, r.body)
		}
		q := questionOf(t, r)
		gen, _ := q["generator"].(string)
		served[gen]++
		if gen == generator.TextImageID {
			t.Fatalf("短命题被当成候选签发（寿命不足以覆盖 challenge）：%v", q)
		}
	}
	if served[generator.ArithID] == 0 {
		t.Fatal("12 次领题没有一次落到足寿命题：候选边界可能过紧")
	}
}

// TestChallengeShortLifetimeOnlyPoolIsHonest503：池里**只有**短命题时，
// 预检排除后如实 503（不静默降级 pow_only、不无界重试），与修复前行为同形但归因正确。
func TestChallengeShortLifetimeOnlyPoolIsHonest503(t *testing.T) {
	h := newHarness(t, nil)
	site, _, err := h.db.CreateSite(t.Context(), "short-only", "sk-short-only", store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion,
		GeneratorID: generator.ArithID, Difficulty: 3,
		AllowMissingOrigin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fx := contractFixture(t, h, generator.ArithID, "17")
	fx.Lifetime = 30 * time.Second
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	r := h.challengeReply("sk-short-only")
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("只有撑不满 TTL 的题时应如实 503：%d %s", r.status, r.body)
	}
}
