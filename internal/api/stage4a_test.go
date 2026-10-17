package api

// 阶段 4A：公开端点的 site_key 一致性规则（widget 接入引出的安全口径）。
//
// 为什么要这条规则：JSON 请求会触发 CORS 预检，而**预检看不见请求体**，
// 只能从 URL 查询参数（或 X-Epocha-Site-Key 头）认站点。于是"预检按 A 站授权、
// 实际操作落到 B 站"成为一个真实形状。规则：**两处都出现时必须一致，不一致直接拒绝**，
// 而不是"以其中一处为准"——任何"取一个优先"的实现都会把这条差异变成可利用的歧义。
//
// 顺序同样是判定的一部分：冲突检查必须发生在**任何状态消费之前**，
// 所以一次被拒的 redeem 不能把受害者唯一的作答机会用掉（下面有断言钉住）。
//
// 题目类断言一律用受控夹具（seedQuestions），不混用后台补题：
// 领题按 expires_at 升序，两种来源混在一起会让"哪道题被发出去"重新变成运气问题。

import (
	"context"
	"net/http"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

// addPowOnlySite 建一个 pow_only 站点：这几条测试只关心"落到哪个站点"，不需要题目层。
func addPowOnlySite(t *testing.T, h *harness, siteKey string) *store.Site {
	t.Helper()
	site, _, err := h.db.CreateSite(context.Background(), "consistency "+siteKey, siteKey,
		store.SitePolicy{PowBits: 12, VerifyMode: store.ModePowOnly, AllowMissingOrigin: true})
	if err != nil {
		t.Fatalf("CreateSite %s: %v", siteKey, err)
	}
	return site
}

func countChallengeRows(t *testing.T, h *harness, siteIDs ...int64) int {
	t.Helper()
	db := rawSQLite(t, h.dbPath)
	total := 0
	for _, id := range siteIDs {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM challenges WHERE site_id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		total += n
	}
	return total
}

type bucketCounts struct{ Ready, Assigned, Terminal int }

func arithBucket(t *testing.T, h *harness, difficulty int) bucketCounts {
	t.Helper()
	db := rawSQLite(t, h.dbPath)
	var c bucketCounts
	err := db.QueryRow(`
SELECT coalesce(sum(status='ready'),0), coalesce(sum(status='assigned'),0), coalesce(sum(status='terminal'),0)
FROM pool WHERE generator_id = ? AND difficulty = ?`, generator.ArithID, difficulty).
		Scan(&c.Ready, &c.Assigned, &c.Terminal)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChallengeRejectsSiteKeyConflictBetweenURLAndBody(t *testing.T) {
	h := newHarness(t, nil)
	a := addPowOnlySite(t, h, "pk-cons-a")
	b := addPowOnlySite(t, h, "pk-cons-b")
	before := countChallengeRows(t, h, a.ID, b.ID)

	cases := []struct {
		name    string
		path    string
		bodyKey string
		wantOK  bool
	}{
		{"URL 是 A、体是 B", "/api/challenge?site_key=pk-cons-a", "pk-cons-b", false},
		{"URL 是 B、体是 A", "/api/challenge?site_key=pk-cons-b", "pk-cons-a", false},
		{"URL 是未知站点、体是已知站点", "/api/challenge?site_key=pk-cons-nope", "pk-cons-b", false},
		{"URL 上重复且不一致", "/api/challenge?site_key=pk-cons-a&site_key=pk-cons-b", "pk-cons-a", false},
		{"URL 上重复但一致", "/api/challenge?site_key=pk-cons-a&site_key=pk-cons-a", "pk-cons-a", true},
		{"两处一致", "/api/challenge?site_key=pk-cons-a", "pk-cons-a", true},
		{"只在体里（非浏览器客户端形状）", "/api/challenge", "pk-cons-a", true},
	}
	for _, tc := range cases {
		r := h.postJSON(tc.path, map[string]string{"site_key": tc.bodyKey})
		if tc.wantOK {
			if r.status != http.StatusOK || r.str("token") == "" {
				t.Fatalf("%s: 应 200 并给 token，实得 %d %s", tc.name, r.status, r.body)
			}
			continue
		}
		if r.status != http.StatusBadRequest || !r.hasCode(CodeInvalidRequest) {
			t.Fatalf("%s: 应 400 %s，实得 %d %v", tc.name, CodeInvalidRequest, r.status, r.body)
		}
		// 不透露是哪一站冲突（红线 8）：码集里只有一个码，也没有站点标识。
		if len(r.codes()) != 1 {
			t.Fatalf("%s: 只应有一个错误码，实得 %v", tc.name, r.codes())
		}
	}
	// 三次正例各写一行、四次被拒一律零行：拒绝必须发生在签发之前，
	// 否则随便编 site_key 就是免费的库写入放大面。
	if got, want := countChallengeRows(t, h, a.ID, b.ID)-before, 3; got != want {
		t.Fatalf("challenge 行增量 = %d，期望 %d（三次成功 + 被拒零副作用）", got, want)
	}
}

func TestRedeemSiteKeyConflictDoesNotConsumeTheChallenge(t *testing.T) {
	h := newHarness(t, nil)
	h.questionSite(t, "qs", "pk-redeem-site", 2)
	addPowOnlySite(t, h, "pk-redeem-other")
	h.seedQuestions(t, 2, arithQuestion("40 + 2", "42"), arithQuestion("14 - 7", "7"))

	r := h.postJSON("/api/challenge", map[string]string{"site_key": "pk-redeem-site"})
	if r.status != http.StatusOK {
		t.Fatalf("challenge: %d %s", r.status, r.body)
	}
	tok, nonce := r.str("token"), h.solvePow(t, r)
	start := arithBucket(t, h, 2)
	if start.Assigned == 0 {
		t.Fatal("领题后没有 assigned 行，测试形状不对")
	}

	bad := h.postJSON("/api/redeem?site_key=pk-redeem-other",
		map[string]any{"token": tok, "pow_nonce": nonce, "answer": "42"})
	if bad.status != http.StatusBadRequest || !bad.hasCode(CodeInvalidRequest) {
		t.Fatalf("site_key 与 token 站点不符应 400 %s，实得 %d %v",
			CodeInvalidRequest, bad.status, bad.codes())
	}
	if got := arithBucket(t, h, 2); got != start {
		t.Fatalf("被拒的 redeem 动了题池计数：%+v → %+v", start, got)
	}

	// 同一张 token 在匹配的 site_key 下仍可兑换 ⇒ 上面那次拒绝没消耗任何东西。
	if ok := h.postJSON("/api/redeem?site_key=pk-redeem-site",
		map[string]any{"token": tok, "pow_nonce": nonce, "answer": "42"}); ok.status != http.StatusOK ||
		ok.str("response") == "" {
		t.Fatalf("匹配 site_key 的 redeem 应成功：%d %s", ok.status, ok.body)
	}

	// 回归：不带 URL 参数的路径照旧可用（阶段 1/2/3A 的脚本与 devclient 不受这条规则影响）。
	r2 := h.postJSON("/api/challenge", map[string]string{"site_key": "pk-redeem-site"})
	if r2.status != http.StatusOK {
		t.Fatalf("challenge2: %d %s", r2.status, r2.body)
	}
	if res2 := h.postJSON("/api/redeem", map[string]any{
		"token": r2.str("token"), "pow_nonce": h.solvePow(t, r2), "answer": "7"}); res2.status != http.StatusOK {
		t.Fatalf("无 URL 参数的 redeem 回归：%d %s", res2.status, res2.body)
	}

	// 两次兑换之后：两条夹具题全部离开 ready 并进终态，一条都没回到池里。
	// （start 是在**第一次领题之后**读的，那时 ready=1/assigned=1，所以这里断言终态计数而不是差值。）
	end := arithBucket(t, h, 2)
	if end != (bucketCounts{Ready: 0, Assigned: 0, Terminal: 2}) {
		t.Fatalf("两条题都应进入终态且不回池，实得 %+v（起点 %+v）", end, start)
	}
}

// TestSiteKeyConflictIsCheckedBeforeAnswerJudgement 钉住"错 site_key 连错误答案终态都不该触发"：
// 上一条测的是正解路径，这里用**故意答错**的请求验证判定顺序。
func TestSiteKeyConflictIsCheckedBeforeAnswerJudgement(t *testing.T) {
	h := newHarness(t, nil)
	h.questionSite(t, "qs", "pk-judge-site", 2)
	h.seedQuestions(t, 2, arithQuestion("9 + 1", "10"))

	r := h.postJSON("/api/challenge?site_key=pk-judge-site", map[string]string{"site_key": "pk-judge-site"})
	if r.status != http.StatusOK {
		t.Fatalf("challenge: %d %s", r.status, r.body)
	}
	tok, nonce := r.str("token"), h.solvePow(t, r)
	start := arithBucket(t, h, 2)

	bad := h.postJSON("/api/redeem?site_key=pk-judge-nonexistent",
		map[string]any{"token": tok, "pow_nonce": nonce, "answer": "999"})
	if bad.status != http.StatusBadRequest || !bad.hasCode(CodeInvalidRequest) {
		t.Fatalf("应 400 %s，实得 %d %v", CodeInvalidRequest, bad.status, bad.codes())
	}
	if got := arithBucket(t, h, 2); got.Terminal > start.Terminal {
		t.Fatalf("site_key 冲突被误当成作答，题池进入终态：%+v", got)
	}

	// 同一 token 用正确 site_key 才第一次真正作答：错误答案 ⇒ 终态（ADR-011 单次机会不变）。
	if res := h.postJSON("/api/redeem?site_key=pk-judge-site",
		map[string]any{"token": tok, "pow_nonce": nonce, "answer": "999"}); res.status == http.StatusOK {
		t.Fatal("错误答案不应成功")
	}
	if got := arithBucket(t, h, 2); got.Terminal != start.Terminal+1 {
		t.Fatalf("错误答案应恰好消耗一次机会，实得 %+v", got)
	}
}
