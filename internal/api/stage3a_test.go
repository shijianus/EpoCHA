package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/pow"
	"github.com/shijianus/EpoCHA/internal/store"
)

// solveTaskText 是一个**独立**的表达式求值器（测试侧实现），把题面文本算成答案。
// 不复用被测生成器的计算路径：如果生成器"声称的答案"与"题面真正的值"不一致，
// 这里的兑换就会失败，从而把渲染/计算不一致钉死。
// 语法契约与生成器一致：“ 或 "(a op b) op c"，运算符 + - × ÷，÷ 保证整除。
func solveTaskText(t *testing.T, task string) int64 {
	t.Helper()
	inner := task
	if strings.HasPrefix(task, "(") {
		end := strings.Index(task, ")")
		if end < 0 {
			t.Fatalf("括号未闭合：%q", task)
		}
		v := evalFlatText(t, task[1:end])
		rest := strings.TrimSpace(task[end+1:])
		if rest == "" {
			return v
		}
		op := string([]rune(rest)[0])
		num, err := strconv.ParseInt(strings.TrimSpace(rest[len(op):]), 10, 64)
		if err != nil {
			t.Fatalf("括号后的片段不合法：%q", task)
		}
		return applyOp(t, v, op, num)
	}
	return evalFlatText(t, inner)
}

func evalFlatText(t *testing.T, s string) int64 {
	t.Helper()
	fields := strings.Fields(s)
	if len(fields) != 3 {
		t.Fatalf("表达式必须是 a op b：%q", s)
	}
	a, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	b, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return applyOp(t, a, fields[1], b)
}

func applyOp(t *testing.T, a int64, op string, b int64) int64 {
	t.Helper()
	switch op {
	case "+":
		return a + b
	case "-":
		return a - b
	case "×":
		return a * b
	case "÷":
		if b == 0 || a%b != 0 {
			t.Fatalf("题面含非整除除法：%d ÷ %d", a, b)
		}
		return a / b
	}
	t.Fatalf("未知运算符 %q", op)
	return 0
}

// questionOf 从签发响应里取题面文本；形状不符时干净失败并打印响应（不让测试以 panic 收场）。
func (h *harness) questionOf(t *testing.T, r reply) string {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("签发未成功：%d %s", r.status, r.body)
	}
	q, ok := r.got["question"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 question 字段：%s", r.body)
	}
	task, ok := q["task"].(string)
	if !ok || task == "" {
		t.Fatalf("question.task 不合法：%v (%s)", q["task"], r.body)
	}
	return task
}

// questionChallenge 领一张 pow_question 的 challenge 并返回公开题面文本。
func (h *harness) questionChallenge(t *testing.T, siteKey string) (tok, task string) {
	t.Helper()
	r := h.postJSON("/api/challenge", map[string]string{"site_key": siteKey})
	if r.status != http.StatusOK {
		t.Fatalf("challenge status=%d body=%s", r.status, r.body)
	}
	q, ok := r.got["question"].(map[string]any)
	if !ok {
		t.Fatalf("pow_question 站点应返回 question 字段：%s", r.body)
	}
	task, _ = q["task"].(string)
	if task == "" {
		t.Fatalf("question 缺 task：%v", q)
	}
	return r.str("token"), task
}

// redeemAnswer 带答案兑换。answerNoField 表示"请求里根本没有 answer 字段"
// （pow_question 下这是结构错误，不该消耗作答机会）。
const answerNoField = "\x00no-answer-field"

func (h *harness) redeemAnswer(tok, nonce, answer string) reply {
	body := map[string]any{"token": tok, "pow_nonce": nonce}
	if answer != answerNoField {
		body["answer"] = answer
	}
	return h.postJSON("/api/redeem", body)
}

// solvePow 按响应里的参数真实求解。
func (h *harness) solvePow(t *testing.T, r reply) string {
	t.Helper()
	p := r.got["pow"].(map[string]any)
	seed, err := base64.RawURLEncoding.DecodeString(p["seed"].(string))
	if err != nil {
		t.Fatal(err)
	}
	params := pow.Params{Algo: pow.Algo, Bits: uint8(num(p["bits"])), Seed: seed}
	for n := int64(0); n < 20_000_000; n++ {
		cand := strconv.FormatInt(n, 10)
		ok, err := params.Verify(cand)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return cand
		}
	}
	t.Fatal("未能在上限内求解 PoW")
	return ""
}

func (h *harness) challengeReply(siteKey string) reply {
	return h.postJSON("/api/challenge", map[string]string{"site_key": siteKey})
}

// --- 主路径 ---

// TestPowQuestionHappyPath 是阶段 3A 的闭环：领题 → 真实求解 PoW → 用**独立求出的答案**兑换 → 一次性验证。
func TestPowQuestionHappyPath(t *testing.T) {
	h := newHarness(t, nil)
	site, qSecret := h.questionSite(t, "questioned", "sk-q", 3)
	h.fillPool(t, site.Policy.Difficulty, 3)

	r := h.challengeReply("sk-q")
	if r.str("mode") != store.ModePowQuestion {
		t.Fatalf("mode 应为 pow_question，got %q (%s)", r.str("mode"), r.body)
	}
	q := r.got["question"].(map[string]any)
	// 出网字段白名单：不得出现答案、verifier、内部随机数
	assertKeys(t, q, "kind", "task", "answer_type", "max_answer_len", "generator", "generator_version")
	if q["generator"] != generator.ArithID || q["kind"] != "arithmetic" {
		t.Fatalf("题型标识不符：%v", q)
	}
	task := q["task"].(string)
	answer := strconv.FormatInt(solveTaskText(t, task), 10)
	nonce := h.solvePow(t, r)

	rr := h.redeemAnswer(r.str("token"), nonce, answer)
	if rr.status != http.StatusOK {
		t.Fatalf("正确 PoW + 正确答案应兑换成功：%d %s", rr.status, rr.body)
	}
	resp := rr.str("response")
	if resp == "" {
		t.Fatal("未拿到 response")
	}
	// 响应里不得回显答案：统一交给下面的字段级检查（子串匹配会在 token 的 base64 里撞上假阳性）
	v := h.verify(qSecret, resp)
	if !v.got["success"].(bool) {
		t.Fatalf("验证应成功：%s", v.body)
	}
	if v2 := h.verify(qSecret, resp); v2.got["success"].(bool) || !v2.hasCode(ErrTimeoutOrDuplicate) {
		t.Fatalf("response 必须只能用一次：%s", v2.body)
	}
	// 按值扫描（红线 5、11）：**私有 verifier** 与站点 secret 都不得出现在任何响应或日志里。
	// verifier 是 48 字节高熵串，可以当作必然唯一的哨兵；答案是小整数，
	// 用子串扫描会撞上日志里的合法数值（如耗时），所以答案走"字段级"检查（下面两条）。
	verifier := h.verifierOf(t, r.str("token"))
	joined := h.logs.String() + h.allBodies()
	for _, forbidden := range []string{
		qSecret,
		fmt.Sprintf("%x", verifier),
		base64.StdEncoding.EncodeToString(verifier),
		base64.RawURLEncoding.EncodeToString(verifier),
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("响应/日志里出现敏感材料 %q", forbidden[:min(24, len(forbidden))])
		}
	}
	// 答案：任何响应的任何字符串字段都不许等于它；任何输出都不许出现 answer 字段名。
	for _, body := range strings.Split(h.allBodies(), "\n") {
		for _, leaf := range jsonStringLeaves(t, body) {
			if leaf == answer {
				t.Fatalf("响应把答案作为字段值下发：%s", body)
			}
		}
	}
	if reAnswerField.MatchString(joined) {
		t.Fatalf("响应或日志里出现 answer 字段（红线 11 的字段白名单）：%s", joined)
	}
}

// reAnswerField 匹配独立的 answer 键（`answer_type` 不算：下划线是单词字符，不构成边界）。
var reAnswerField = regexp.MustCompile(`\banswer\b`)

// jsonStringLeaves 递归收集一个 JSON 文档里的全部字符串叶子值。
func jsonStringLeaves(t *testing.T, body string) []string {
	t.Helper()
	if !strings.HasPrefix(strings.TrimSpace(body), "{") {
		return nil
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("响应不是合法 JSON：%v / %q", err, body)
	}
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	return out
}

// --- 单次作答终结 ---

// TestWrongAnswerTerminatesChallenge 是阶段 3A 的核心安全性质：
// 错误答案**提交后**就终结该 challenge，正确答案紧随其后也不能重试。
func TestWrongAnswerTerminatesChallenge(t *testing.T) {
	h := newHarness(t, nil)
	site, qSecret := h.questionSite(t, "questioned", "sk-q", 2)
	_ = qSecret
	h.fillPool(t, site.Policy.Difficulty, 2)

	before := h.readyCount(t, site.Policy.Difficulty)
	r := h.challengeReply("sk-q")
	task := h.questionOf(t, r)
	answer := strconv.FormatInt(solveTaskText(t, task), 10)
	nonce := h.solvePow(t, r)

	wrong := strconv.FormatInt(solveTaskText(t, task)+1, 10)
	rr := h.redeemAnswer(r.str("token"), nonce, wrong)
	if rr.status != http.StatusBadRequest || !rr.hasCode(CodeInvalidChallenge) {
		t.Fatalf("错误答案应 invalid-challenge，got %d %s", rr.status, rr.body)
	}
	// 同一张 challenge 再用正确答案：必须仍然失败（终结已提交）
	if rr2 := h.redeemAnswer(r.str("token"), nonce, answer); rr2.status != http.StatusBadRequest ||
		!rr2.hasCode(CodeInvalidChallenge) {
		t.Fatalf("错误答案后正确答案竟可重试：%d %s", rr2.status, rr2.body)
	}
	// 题目被消耗掉且**没有回到池里**（invariant #2）
	if after := h.readyCount(t, site.Policy.Difficulty); after != before-1 {
		t.Fatalf("ready 应减少 1 且不回池：%d → %d", before, after)
	}
	// 新领一张仍可正常成功（终结只作用于那张 challenge）
	r2 := h.challengeReply("sk-q")
	task2 := h.questionOf(t, r2)
	ans2 := strconv.FormatInt(solveTaskText(t, task2), 10)
	if rr3 := h.redeemAnswer(r2.str("token"), h.solvePow(t, r2), ans2); rr3.status != http.StatusOK {
		t.Fatalf("新 challenge 应可正常兑换：%d %s", rr3.status, rr3.body)
	}
}

// TestMalformedAnswerDoesNotConsumeAttempt 定义清楚"畸形 ≠ 答错"：
// 格式不对的提交不能消耗掉唯一一次作答机会（否则垃圾输入就能把受害者的 challenge 打死）。
func TestMalformedAndMissingAnswerAreNotAttempts(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.questionSite(t, "questioned", "sk-q", 1)
	h.fillPool(t, site.Policy.Difficulty, 2)

	r := h.challengeReply("sk-q")
	task := h.questionOf(t, r)
	answer := strconv.FormatInt(solveTaskText(t, task), 10)
	nonce := h.solvePow(t, r)

	for _, bad := range []string{"", "abc", "1.5", "1,0", "١٢", "--1", " 1 2 ", strings.Repeat("9", 40)} {
		rr := h.redeemAnswer(r.str("token"), nonce, bad)
		if bad == "" {
			// 缺 answer 字段是结构错误
			rr = h.redeemAnswer(r.str("token"), nonce, answerNoField)
		}
		if !rr.hasCode(CodeInvalidRequest) {
			t.Fatalf("畸形答案 %q 应 invalid-request（不终结），got %d %s", bad, rr.status, rr.body)
		}
	}
	// 机会没被消耗：正确答案仍然可用
	if rr := h.redeemAnswer(r.str("token"), nonce, answer); rr.status != http.StatusOK {
		t.Fatalf("畸形提交不该消耗作答机会：%d %s", rr.status, rr.body)
	}
}

// TestWrongPoWIsNotAnAnswerAttempt PoW 不对不能兑换，但也不消耗作答机会。
func TestWrongPoWIsNotAnAnswerAttempt(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.questionSite(t, "questioned", "sk-q", 2)
	h.fillPool(t, site.Policy.Difficulty, 2)
	r := h.challengeReply("sk-q")
	task := h.questionOf(t, r)
	answer := strconv.FormatInt(solveTaskText(t, task), 10)

	if rr := h.redeemAnswer(r.str("token"), "1", answer); !rr.hasCode(CodeInvalidChallenge) {
		t.Fatalf("错误 PoW 不能兑换 response，got %d %s", rr.status, rr.body)
	}
	// 修正 PoW 后应能完成（说明这一步没把它当作答）
	if rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), answer); rr.status != http.StatusOK {
		t.Fatalf("PoW 修正后应可兑换：%d %s", rr.status, rr.body)
	}
}

// --- 不能降级、不能换题 ---

// TestClientCannotDowngradeOrSwapRequirement 覆盖两件事：
//  1. 请求里没有"我要走 pow_only"这种参数，也没有可改的答案字段——模式由站点声明 + 签名锁定；
//  2. 篡改 token 里的 mode/gen 一律验签失败。
func TestClientCannotDowngradeOrSwapRequirement(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.questionSite(t, "questioned", "sk-q", 2)
	h.fillPool(t, site.Policy.Difficulty, 2)
	r := h.challengeReply("sk-q")
	tok := r.str("token")
	nonce := h.solvePow(t, r)

	// 不带 answer 字段（pow_only 形状）→ 结构错误，不签发
	if rr := h.redeemAnswer(tok, nonce, answerNoField); !rr.hasCode(CodeInvalidRequest) {
		t.Fatalf("缺答案必须按结构错误处理：%d %s", rr.status, rr.body)
	}
	// 带一个"mode"字段试图声明自己走 pow_only：未知字段被拒
	req := h.postJSON("/api/redeem", map[string]any{
		"token": tok, "pow_nonce": nonce, "mode": "pow_only",
	})
	if !req.hasCode(CodeInvalidRequest) {
		t.Fatalf("未知字段应拒绝：%d %s", req.status, req.body)
	}
	// 篡改签名里的 mode/gen
	parts := strings.Split(tok, ".")
	if len(parts) != 4 {
		t.Fatal("token 形状不符")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	swapped := strings.Replace(string(raw), `"mode":"pow_question"`, `"mode":"pow_only"`, 1)
	if swapped == string(raw) {
		t.Fatal("payload 里找不到 mode 字段，签名绑定形同虚设")
	}
	forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte(swapped)) + "." + parts[3]
	if rr := h.redeemAnswer(forged, nonce, "42"); !rr.hasCode(CodeInvalidChallenge) {
		t.Fatalf("篡改 mode 的 token 必须被拒：%d %s", rr.status, rr.body)
	}
	// 换一个 generator 标识同样被拒
	raw2 := strings.Replace(string(raw), generator.ArithID, "text-warp-v1", 1)
	forged2 := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte(raw2)) + "." + parts[3]
	if rr := h.redeemAnswer(forged2, nonce, "42"); !rr.hasCode(CodeInvalidChallenge) {
		t.Fatalf("篡改 generator 的 token 必须被拒：%d %s", rr.status, rr.body)
	}
	// 原始 token 与正确答案仍可完成（上面几步都没消耗状态）
	ans := strconv.FormatInt(solveTaskText(t, h.questionOf(t, r)), 10)
	if rr := h.redeemAnswer(tok, nonce, ans); rr.status != http.StatusOK {
		t.Fatalf("未篡改的请求应仍可兑换：%d %s", rr.status, rr.body)
	}
}

// TestAnswerIsBoundToItsOwnChallenge 答案与题目一题一验证物：
// 把 A 题的答案提交给 B 题必须失败；而 B 失败后已被终结（一次作答），
// 所以正向对照用另一张新 challenge 做——这条同时钉住了"答案不是全局可复用的口令"。
//
// 前提"B 与 A 不同答案"用**有界重抽**保证，不用 `t.Skip` 蒙过去：
// 随机补题确实可能抽出同答案的两道题，但把断言跳过等于把这条安全性质的覆盖率交给概率，
// 而且 CI 里永远是绿的。d4 档最大单点答案概率实测 ≈0.0136（§8.4），因此"连续 6 次都撞上同一个答案"
// 的概率上界约 0.0136^6 ≈ 6e-12；真撞上就说明池的多样性本身已经塌了——那时**报错**才是正确的反应。
func TestAnswerIsBoundToItsOwnChallenge(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.questionSite(t, "questioned", "sk-q", 4)
	h.fillPool(t, site.Policy.Difficulty, 10)

	rA := h.challengeReply("sk-q")
	taskA := h.questionOf(t, rA)
	ansA := solveTaskText(t, taskA)

	// 领到一道**答案不同**的 B
	var rB reply
	var ansB int64
	picked := false
	for range 6 {
		rB = h.challengeReply("sk-q")
		ansB = solveTaskText(t, h.questionOf(t, rB))
		if ansB != ansA {
			picked = true
			break
		}
	}
	if !picked {
		t.Fatalf("连续 6 次领到的题都与 A 同答案（%d）：题池多样性已不成立，本测试的前提无法构造", ansA)
	}

	nonceB := h.solvePow(t, rB)
	if rr := h.redeemAnswer(rB.str("token"), nonceB, strconv.FormatInt(ansA, 10)); rr.status == http.StatusOK {
		t.Fatal("把 A 题的答案用到 B 题上竟兑换成功")
	}
	// B 已经被错误答案终结：正确答案也不再能用（一次作答的另一个侧面）
	if rr := h.redeemAnswer(rB.str("token"), h.solvePow(t, rB), strconv.FormatInt(ansB, 10)); rr.status == http.StatusOK {
		t.Fatal("错误答案之后同一张 challenge 竟可重试")
	}
	// 正向对照：新领一张，自己的答案自己的题
	rC := h.challengeReply("sk-q")
	taskC := h.questionOf(t, rC)
	ansC := strconv.FormatInt(solveTaskText(t, taskC), 10)
	if rr := h.redeemAnswer(rC.str("token"), h.solvePow(t, rC), ansC); rr.status != http.StatusOK {
		t.Fatalf("新 challenge 用自己的答案应可兑换：%d %s", rr.status, rr.body)
	}
}

// --- 池空：可重试，不降级 ---

func TestPoolEmptyReturnsRetryableNotPowOnly(t *testing.T) {
	h := newHarness(t, nil)
	_, _ = h.questionSite(t, "questioned", "sk-q", 5) // 故意不补题

	r := h.challengeReply("sk-q")
	if r.status != http.StatusServiceUnavailable || !r.hasCode(CodeChallengeUnavailable) {
		t.Fatalf("池空应 503 challenge-unavailable，got %d %s", r.status, r.body)
	}
	if r.headers.Get("Retry-After") == "" {
		t.Error("503 必须带 Retry-After")
	}
	if strings.Contains(r.body, "token") || strings.Contains(r.body, "pow") {
		t.Fatalf("池空却发出了 PoW 载荷（静默降级）：%s", r.body)
	}
	if n := h.activeChallenges(t); n != 0 {
		t.Fatalf("池空时不该留下 challenge 行：%d", n)
	}
	// 补题后可用（证明前面不是"这个站点永远坏"）
	h.fillPool(t, 5, 2)
	if r2 := h.challengeReply("sk-q"); r2.status != http.StatusOK {
		t.Fatalf("补题后应可签发：%d %s", r2.status, r2.body)
	}
}

// --- 启停与重启 ---

func TestDisableDoesNotReturnQuestionAndRestartKeepsState(t *testing.T) {
	h := newHarness(t, nil)
	site, qSecret := h.questionSite(t, "questioned", "sk-q", 2)
	h.fillPool(t, site.Policy.Difficulty, 3)
	before := h.readyCount(t, site.Policy.Difficulty)

	r := h.challengeReply("sk-q")
	nonce := h.solvePow(t, r)
	answer := strconv.FormatInt(solveTaskText(t, r.got["question"].(map[string]any)["task"].(string)), 10)

	h.disable(site.ID)
	if rr := h.redeemAnswer(r.str("token"), nonce, answer); rr.status != http.StatusBadRequest {
		t.Fatalf("停用站点后不该兑换：%d %s", rr.status, rr.body)
	}
	h.enable(site.ID)
	// 版本两次提升：停用前的 token 永久失效
	if rr := h.redeemAnswer(r.str("token"), nonce, answer); rr.status != http.StatusBadRequest {
		t.Fatalf("重新启用后停用前的 challenge 复活了")
	}
	// 但那道**没有回到池里**：ready 数不因启停而回升
	if after := h.readyCount(t, site.Policy.Difficulty); after > before-1 {
		t.Fatalf("启停把已领取的题退回池里：%d → %d", before, after)
	}

	// 未消费的 response 在重启后应仍可验证（这是 DESIGN 承诺的行为，不是缺陷）
	r2 := h.challengeReply("sk-q")
	ans2 := strconv.FormatInt(solveTaskText(t, h.questionOf(t, r2)), 10)
	resp := h.redeemAnswer(r2.str("token"), h.solvePow(t, r2), ans2)
	if resp.status != http.StatusOK {
		t.Fatalf("兑换应成功：%d %s", resp.status, resp.body)
	}
	h.restart(t)
	if v := h.verify(qSecret, resp.str("response")); !v.got["success"].(bool) {
		t.Fatalf("重启后未消费的 response 应仍可验证（一次）：%s", v.body)
	}
	if v := h.verify(qSecret, resp.str("response")); v.got["success"].(bool) {
		t.Fatal("重启后同一条 response 被验证了两次")
	}
	// 已终结的 challenge 在重启后仍不能重试
	r3 := h.challengeReply("sk-q")
	task3 := h.questionOf(t, r3)
	wrong := strconv.FormatInt(solveTaskText(t, task3)+2, 10)
	if rr := h.redeemAnswer(r3.str("token"), h.solvePow(t, r3), wrong); rr.status != http.StatusBadRequest {
		t.Fatalf("错误答案应被拒：%d %s", rr.status, rr.body)
	}
	h.fillPool(t, 2, 3)
	h.restart(t)
	r4 := h.challengeReply("sk-q")
	ans4 := strconv.FormatInt(solveTaskText(t, h.questionOf(t, r4)), 10)
	if rr := h.redeemAnswer(r4.str("token"), h.solvePow(t, r4), ans4); rr.status != http.StatusOK {
		t.Fatalf("新 challenge 在重启后应可用：%d %s", rr.status, rr.body)
	}
}

// TestFailedChallengeStaysFailedAfterRestart 终态必须活在库里，而不是活在内存里：
// 答错终结的 challenge 经过"关掉全部句柄 + 用同一个 DB 文件重建服务"之后，
// 拿**正确答案**再提交一次仍然被拒；它绑定的那道题也不得因为重启而回到 ready。
// （这条与 TestDisableDoesNotReturnQuestionAndRestartKeepsState 不同：那条走的是站点停用导致的版本失效，
// 这一条走的是"答案错误"这条终态分支——两者是不同的写入路径，各自都要钉住重启语义。）
func TestFailedChallengeStaysFailedAfterRestart(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.questionSite(t, "questioned", "sk-q", 2)
	h.fillPool(t, site.Policy.Difficulty, 3)
	before := h.readyCount(t, site.Policy.Difficulty)

	r := h.challengeReply("sk-q")
	tok := r.str("token")
	nonce := h.solvePow(t, r)
	task := h.questionOf(t, r)
	correct := strconv.FormatInt(solveTaskText(t, task), 10)

	// 第一笔有效作答：文法正确、数值错误 ⇒ 已提交的终态
	if rr := h.redeemAnswer(tok, nonce, correct+"9"); rr.status != http.StatusBadRequest {
		t.Fatalf("错误答案应被拒：%d %s", rr.status, rr.body)
	}
	h.restart(t)

	// 重启后同一张 challenge：正确答案也必须失败（终态在库里）
	if rr := h.redeemAnswer(tok, nonce, correct); rr.status != http.StatusBadRequest ||
		!rr.hasCode(CodeInvalidChallenge) {
		t.Fatalf("重启后已失败的 challenge 竟可重试：%d %s", rr.status, rr.body)
	}
	// 那道题不能因为重启而回到池里
	if after := h.readyCount(t, site.Policy.Difficulty); after != before-1 {
		t.Fatalf("重启把已终结的题目退回了池里：%d → %d（期望 %d）", before, after, before-1)
	}
	// 新 challenge 照常可用（证明失败的不是"整个站点坏了"这种假通过）
	r2 := h.challengeReply("sk-q")
	ans2 := strconv.FormatInt(solveTaskText(t, h.questionOf(t, r2)), 10)
	if rr := h.redeemAnswer(r2.str("token"), h.solvePow(t, r2), ans2); rr.status != http.StatusOK {
		t.Fatalf("重启后新 challenge 应可用：%d %s", rr.status, rr.body)
	}
}

// --- 并发：混合正确/错误答案 ---

func TestConcurrentMixedAnswersGiveOneTerminalState(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.questionSite(t, "questioned", "sk-q", 2)
	h.fillPool(t, site.Policy.Difficulty, 4)
	r := h.challengeReply("sk-q")
	tok := r.str("token")
	nonce := h.solvePow(t, r)
	task := h.questionOf(t, r)
	answer := strconv.FormatInt(solveTaskText(t, task), 10)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	wins, losers := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			a := answer
			if i%2 == 1 {
				a = strconv.FormatInt(solveTaskText(t, task)+int64(i), 10)
			}
			rr := h.postJSON("/api/redeem", map[string]any{"token": tok, "pow_nonce": nonce, "answer": a})
			mu.Lock()
			defer mu.Unlock()
			if rr.status == http.StatusOK {
				wins++
			} else {
				losers++
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins > 1 {
		t.Fatalf("%d 路并发兑换成功 %d 次", n, wins)
	}
	if wins+losers != n {
		t.Fatalf("有请求既没成功也没失败：%d+%d != %d", wins, losers, n)
	}
	t.Logf("混合答案并发：%d 成功 / %d 被拒（最多一个终态即正确）", wins, losers)
}

// --- 跨站点 ---

func TestQuestionTokenCannotBeUsedAcrossSites(t *testing.T) {
	h := newHarness(t, nil)
	siteA, secretA := h.questionSite(t, "q-a", "sk-qa", 2)
	h.fillPool(t, siteA.Policy.Difficulty, 2)

	// A 站点（pow_question）的 token 不能靠"不发答案"绕过：必须是结构错误
	rA := h.challengeReply("sk-qa")
	nonceA := h.solvePow(t, rA)
	if rr := h.redeemAnswer(rA.str("token"), nonceA, answerNoField); rr.status != http.StatusBadRequest {
		t.Fatalf("pow_question 缺答案应被拒：%d %s", rr.status, rr.body)
	}

	// B 站点（pow_only）的正常流程
	tokB, nonceB := h.challengeOK(t, "sk-bravo")
	respB := h.redeemOK(t, tokB, nonceB)

	// 先做负向：A 站点的合法 secret 不能验证 B 站点签发的 response，
	// 而且这一步不能把 respB 消费掉
	if v := h.verify(secretA, respB); v.got["success"].(bool) {
		t.Fatal("跨站点 secret 验证成功")
	}
	// 再正向：B 站点自己的 secret 仍可验证（证明上一步没消费）
	if v := h.verify(sentinelSecretB, respB); !v.got["success"].(bool) {
		t.Fatalf("本站点 secret 应可验证（跨站点尝试不得消费它）：%s", v.body)
	}
	// alpha 的 secret 同样不行
	if v := h.verify(sentinelSecret, respB); v.got["success"].(bool) {
		t.Fatal("第三个站点的 secret 验证成功")
	}
}

// --- 辅助 ---

func (h *harness) readyCount(t *testing.T, difficulty int) int {
	t.Helper()
	n, err := h.db.ReadyCount(context.Background(), store.PoolSpec{
		GeneratorID: generator.ArithID, Version: "v1", Difficulty: difficulty,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return n
}
