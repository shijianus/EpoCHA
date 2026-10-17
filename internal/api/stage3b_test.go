package api

// 阶段 3B 的公开 API 测试：视觉题走完整的 challenge → redeem → siteverify 闭环，
// 并且新增的两类风险各有归属：
//   - 图片资产必须**只**跟着它那张 challenge 出去（base64 内联，无裸媒体端点）；
//   - 媒体与资产不配对、生成器被禁用 ⇒ fail closed，绝不静默降级成"没有题的 PoW"。
//
// 闭环里"期望答案"的来源是 generator.TextImageAnswerFrom(同一条确定性熵流) —— 那是评测面，
// 不是答案出口：它只消费调用方自己提供的熵流，不读库、不接受 challenge id、不返回任何
// 已签发题目的答案。生产 HTTP API 仍然没有能读出答案的接口。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

// imageFixture 生成一道"图片与答案配对"的受控题目。
//
// 配对不是假设：两路同 seed 的确定性流给出同一个答案，其中一路喂给真实 Generate，
// 然后立刻用生成器自己的 Verify 复核 —— 复核不过就说明夹具不可信，直接失败。
func imageFixture(t *testing.T, difficulty int, seed string) seededQuestion {
	t.Helper()
	g, err := generator.NewTextImage(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	answer := imageAnswerFromSeed(t, seed)
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty},
		evalStream(t, "epocha-api-"+seed))
	if err != nil {
		t.Fatalf("生成图片题失败: %v", err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatalf("夹具答案 %q 与该图的判分不符 ⇒ 熵流位置不一致，夹具本身不可信", answer)
	}
	payload, err := json.Marshal(q.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatal(err)
	}
	return seededQuestion{
		GeneratorID: generator.TextImageID,
		Payload:     m,
		Answer:      answer,
		Asset:       q.Asset,
	}
}

// imageQuestionOf 取签发响应里的题目 DTO，并校验图片字段的可解码性与键集合。
func imageQuestionOf(t *testing.T, r reply) map[string]any {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("签发未成功：%d %s", r.status, r.body)
	}
	q, ok := r.got["question"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 question：%s", r.body)
	}
	assertKeys(t, q, "kind", "length", "charset", "answer_type", "max_answer_len",
		"image_width", "image_height", "generator", "generator_version", "media",
		"image_mime", "image_base64")
	if q["kind"] != "text-image" || q["generator"] != generator.TextImageID {
		t.Fatalf("题型标识不符：%v", q)
	}
	if q["media"] != generator.MediaImage || q["image_mime"] != "image/png" {
		t.Fatalf("图片字段不符：media=%v mime=%v", q["media"], q["image_mime"])
	}
	raw, err := base64.StdEncoding.DecodeString(q["image_base64"].(string))
	if err != nil {
		t.Fatalf("image_base64 不是合法 base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("下发的资产不是合法 PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != int(num(q["image_width"])) || b.Dy() != int(num(q["image_height"])) {
		t.Fatalf("图片实际尺寸 %v 与题面声明 %vx%v 不符", b, q["image_width"], q["image_height"])
	}
	if len(raw) > store.MaxAssetBytes {
		t.Fatalf("单题资产 %d 字节越界", len(raw))
	}
	return q
}

// TestTextImageHappyPath 是 3B 的工程闭环：领一道真图题 → 校验图 → 提交正确答案 → 一次性验证。
func TestTextImageHappyPath(t *testing.T) {
	h := newHarness(t, nil)
	site, secret := h.imageSiteWithSecret(t, "visual", "sk-img", 2)
	fx := imageFixture(t, 2, "happy")
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	r := h.challengeReply("sk-img")
	q := imageQuestionOf(t, r)
	if int(num(q["length"])) != generator.TextImageLength {
		t.Fatalf("length=%v", q["length"])
	}
	if q["charset"] != generator.TextImageCharset {
		t.Fatalf("charset=%v", q["charset"])
	}
	rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), fx.Answer)
	if rr.status != http.StatusOK {
		t.Fatalf("正确答案应兑换成功：%d %s", rr.status, rr.body)
	}
	v := h.verify(secret, rr.str("response"))
	if !v.hasSuccess() {
		t.Fatalf("siteverify 应成功：%s", v.body)
	}
	if v2 := h.verify(secret, rr.str("response")); v2.hasSuccess() {
		t.Fatal("同一 response 竟可二次验证（一次性失效）")
	}
	// 出网材料里不得出现私有字段名（答案串是 6 个大写字母，按值扫会撞合法字段，故按字段名扫）。
	// 这里**不**扫 "seed"：pow.seed 是协议里就该公开的 PoW 参数（DESIGN §10.1），
	// 与"可推导答案的种子"是两回事；后者由生成器侧的 TestTextImagePrivateFieldsNeverLeave 挡。
	combined := r.body + rr.body + v.body + h.allBodies() + h.logs.String()
	for _, k := range []string{"verifier", "asset_blob", "payload_json", `"answer"`, "pool_id", "private"} {
		if strings.Contains(combined, k) {
			t.Fatalf("响应或日志出现私有字段 %q", k)
		}
	}
	// 站点 secret 按值扫描（红线 11 的按值哨兵）
	if strings.Contains(combined, secret) {
		t.Fatal("站点 secret 出现在出网材料里")
	}
}

// TestTextImageWrongAnswerTerminates 答错即终结，且同一 token 之后永远这个结果；
// 畸形答案不消耗作答机会（两类语义的对照）。
func TestTextImageWrongAnswerTerminates(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.imageSiteWithSecret(t, "visual", "sk-img", 3)
	fx := imageFixture(t, 3, "wrong")
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	r := h.challengeReply("sk-img")
	imageQuestionOf(t, r)
	if rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), otherAnswer(fx.Answer)); !rr.hasCode(CodeInvalidChallenge) {
		t.Fatalf("错误答案应返回 invalid-challenge：%d %s", rr.status, rr.body)
	}
	if rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), fx.Answer); rr.status == http.StatusOK {
		t.Fatal("答错一次之后正确答案竟可重试")
	}

	h.seedQuestions(t, site.Policy.Difficulty, imageFixture(t, 3, "grammar"))
	r2 := h.challengeReply("sk-img")
	if rr := h.redeemAnswer(r2.str("token"), h.solvePow(t, r2), "AB"); !rr.hasCode(CodeInvalidRequest) {
		t.Fatalf("长度不符的答案应是 invalid-request（不终态化）：%d %s", rr.status, rr.body)
	}
	if rr := h.redeemAnswer(r2.str("token"), h.solvePow(t, r2), "ABCDI1"); !rr.hasCode(CodeInvalidRequest) {
		t.Fatalf("含被剔除字符应是 invalid-request：%d %s", rr.status, rr.body)
	}
	if rr := h.redeemAnswer(r2.str("token"), h.solvePow(t, r2), imageAnswerFromSeed(t, "grammar")); rr.status != http.StatusOK {
		t.Fatalf("畸形提交之后正确答案仍应可用：%d %s", rr.status, rr.body)
	}
}

// TestTextImageAssetMissingFailsClosed 声明 image 却没有资产 ⇒ 503 可重试 + 作废该 challenge，
// 绝不下发"看不见的题"，也绝不退化成 pow_only。
func TestTextImageAssetMissingFailsClosed(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.imageSiteWithSecret(t, "visual", "sk-img", 1)
	fx := imageFixture(t, 1, "noasset")
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	// 把刚写进去的资产抹掉：模拟"资产丢失但题还在"的坏状态（独立句柄，走真实库约束）
	db := rawSQLite(t, h.dbPath)
	if _, err := db.ExecContext(context.Background(),
		`UPDATE pool SET asset_blob=NULL, asset_mime='' WHERE generator_id=?`, generator.TextImageID); err != nil {
		t.Fatal(err)
	}
	r := h.challengeReply("sk-img")
	if r.status != http.StatusServiceUnavailable || !r.hasCode(CodeChallengeUnavailable) {
		t.Fatalf("资产缺失应 503 challenge-unavailable：%d %s", r.status, r.body)
	}
	if strings.Contains(r.body, `"pow"`) || strings.Contains(r.body, `"token"`) {
		t.Fatalf("资产缺失却发出了 PoW 载荷（静默降级）：%s", r.body)
	}
	// 那张题必须已经终态化：坏题不能留在池里被反复领到
	rows, err := h.db.PoolStats(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if b.GeneratorID == generator.TextImageID && b.Ready != 0 {
			t.Fatalf("坏题仍可被领取：ready=%d", b.Ready)
		}
	}
	if n := h.activeChallenges(t); n != 0 {
		t.Fatalf("资产缺失的 challenge 应已作废，活动行 %d", n)
	}
}

// TestTextImageGradingDoesNotNeedAsset 已签发题目的**判分**不依赖图片：
// 签发之后把资产清掉，正确答案仍然通过。这条把"清理图片资产不破坏验证"落到测试上。
func TestTextImageGradingDoesNotNeedAsset(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.imageSiteWithSecret(t, "visual", "sk-img", 2)
	fx := imageFixture(t, 2, "lateclear")
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	r := h.challengeReply("sk-img")
	imageQuestionOf(t, r)
	db := rawSQLite(t, h.dbPath)
	if _, err := db.ExecContext(context.Background(), `UPDATE pool SET asset_blob=NULL`); err != nil {
		t.Fatal(err)
	}
	if rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), fx.Answer); rr.status != http.StatusOK {
		t.Fatalf("图片被清理后判分不应受影响：%d %s", rr.status, rr.body)
	}
}

// TestThreeVerificationPathsCoexist 三种路径同时可用：pow_only、算术题、视觉题。
// 这条是"旧流程继续可用"的回归位（阶段 1/2/3A 的业务形状不能因为 3B 而变）。
func TestThreeVerificationPathsCoexist(t *testing.T) {
	h := newHarness(t, nil)
	rPow := h.challengeReply("sk-alpha")
	if rPow.str("mode") != store.ModePowOnly {
		t.Fatalf("pow_only 站点 mode=%q", rPow.str("mode"))
	}
	if _, ok := rPow.got["question"]; ok {
		t.Fatalf("pow_only 站点不应带 question：%s", rPow.body)
	}
	rrPow := h.redeemAnswer(rPow.str("token"), h.solvePow(t, rPow), answerNoField)
	if rrPow.status != http.StatusOK {
		t.Fatalf("pow_only 兑换失败：%d %s", rrPow.status, rrPow.body)
	}
	if v := h.verify(sentinelSecret, rrPow.str("response")); !v.hasSuccess() {
		t.Fatalf("pow_only siteverify 失败：%s", v.body)
	}

	siteA, secretA := h.questionSite(t, "arith-site", "sk-arith", 4)
	h.fillPool(t, siteA.Policy.Difficulty, 2)
	rA := h.challengeReply("sk-arith")
	ansA := solveTaskText(t, h.questionOf(t, rA))
	rrA := h.redeemAnswer(rA.str("token"), h.solvePow(t, rA), strconv.FormatInt(ansA, 10))
	if rrA.status != http.StatusOK {
		t.Fatalf("算术题兑换失败：%d %s", rrA.status, rrA.body)
	}
	if v := h.verify(secretA, rrA.str("response")); !v.hasSuccess() {
		t.Fatalf("算术题 siteverify 失败：%s", v.body)
	}
	// 算术题的 DTO 形状必须与 3A 一致：3B 不得给它凭空长出 media 字段
	assertKeys(t, rA.got["question"].(map[string]any),
		"kind", "task", "answer_type", "max_answer_len", "generator", "generator_version")

	siteI, secretI := h.imageSiteWithSecret(t, "img-site", "sk-visual", 3)
	fx := imageFixture(t, 3, "three-paths")
	h.seedQuestions(t, siteI.Policy.Difficulty, fx)
	rI := h.challengeReply("sk-visual")
	imageQuestionOf(t, rI)
	rrI := h.redeemAnswer(rI.str("token"), h.solvePow(t, rI), fx.Answer)
	if rrI.status != http.StatusOK {
		t.Fatalf("视觉题兑换失败：%d %s", rrI.status, rrI.body)
	}
	if v := h.verify(secretI, rrI.str("response")); !v.hasSuccess() {
		t.Fatalf("视觉题 siteverify 失败：%s", v.body)
	}
}

// TestDisabledGeneratorIssuesNothingButKeepsLedger 生成器被禁用（重启后不再登记）的语义：
//   - 新请求 ⇒ 503 可重试，不降级；
//   - 之前已签发的图片 challenge ⇒ 判分器不存在 ⇒ 同样 fail closed，
//     且**不**消耗作答机会（状态不变，重新启用后仍可完成）。
//
// 第二条是刻意的选择：把"题型被下线"变成"用户的机会被白白烧掉"是一种可用性事故，
// 而 503 + 状态不动既不消耗题目也不签发 response，运维回滚配置即可恢复。
func TestDisabledGeneratorIssuesNothingButKeepsLedger(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.imageSiteWithSecret(t, "img", "sk-img", 2)
	fx := imageFixture(t, 2, "disabled")
	h.seedQuestions(t, site.Policy.Difficulty, fx)
	r := h.challengeReply("sk-img")
	imageQuestionOf(t, r)

	// 只登记算术题型：等价于 EPOCHA_GENERATORS=arithmetic-baseline-v1 后重启
	arithOnly := generator.NewRegistry()
	arith, err := generator.NewArithmeticBaseline(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := arithOnly.Register(arith); err != nil {
		t.Fatal(err)
	}
	h.restartWithRegistry(t, arithOnly)

	if rr := h.challengeReply("sk-img"); rr.status != http.StatusServiceUnavailable {
		t.Fatalf("禁用后新请求应 503：%d %s", rr.status, rr.body)
	}
	rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), fx.Answer)
	if rr.status != http.StatusServiceUnavailable {
		t.Fatalf("已签发题目的判分器缺失应 503（不是判错、不是降级）：%d %s", rr.status, rr.body)
	}
	// 重新登记后同一张 challenge 仍可完成（说明前面没有把它终结掉）
	h.restartWithRegistry(t, testRegistry(t, testMasterKey))
	if rr2 := h.redeemAnswer(r.str("token"), h.solvePow(t, r), fx.Answer); rr2.status != http.StatusOK {
		t.Fatalf("重新启用后应可完成，而不是把用户的机会白白消耗掉：%d %s", rr2.status, rr2.body)
	}
}

// TestConcurrentRedeemImageSingleWinner 同一 token 并发提交：只有一个成功。
func TestConcurrentRedeemImageSingleWinner(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.imageSiteWithSecret(t, "img", "sk-img", 1)
	fx := imageFixture(t, 1, "concurrent")
	h.seedQuestions(t, site.Policy.Difficulty, fx)
	r := h.challengeReply("sk-img")
	imageQuestionOf(t, r)
	nonce := h.solvePow(t, r)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer)
			mu.Lock()
			if rr.status == http.StatusOK {
				wins++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("并发兑换成功 %d 次，期望恰好 1", wins)
	}
}

// TestImageQuestionSurvivesRestart 重启后：终态 challenge 不可重放、
// 已分配但未答的题仍属于它自己的 challenge（不回到 ready、不丢资产）。
func TestImageQuestionSurvivesRestart(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.imageSiteWithSecret(t, "img", "sk-img", 4)
	fx := imageFixture(t, 4, "restart")
	fx2 := imageFixture(t, 4, "restart-2")
	h.seedQuestions(t, site.Policy.Difficulty, fx, fx2)

	r1 := h.challengeReply("sk-img")
	imageQuestionOf(t, r1)
	if rr := h.redeemAnswer(r1.str("token"), h.solvePow(t, r1), otherAnswer(fx.Answer)); rr.status == http.StatusOK {
		t.Fatal("错误答案不应成功")
	}
	h.restart(t)

	if again := h.redeemAnswer(r1.str("token"), h.solvePow(t, r1), fx.Answer); again.status == http.StatusOK {
		t.Fatal("重启后终态 challenge 竟可重放")
	}
	// 第二道题仍在池里可领，且仍带资产（重启不丢图片）
	r2 := h.challengeReply("sk-img")
	if q2 := imageQuestionOf(t, r2); q2["image_base64"] == "" {
		t.Fatal("重启后领到的题应仍带图片资产")
	}
}

// --- 夹具辅助 ---

// otherAnswer 给出一个**一定不同**的合法答案串（受控，不靠运气）。
func otherAnswer(a string) string {
	b := []byte(a)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}
