package main

import (
	"strconv"
	"testing"

	"github.com/shijianus/EpoCHA/internal/pow"
)

// 负起点会让求解器提交 "-123" 这类服务端判非法的 nonce，曾导致端到端脚本偶发失败。
func TestRandStartAlwaysNonNegative(t *testing.T) {
	for range 5000 {
		n, err := randStart()
		if err != nil {
			t.Fatal(err)
		}
		if n < 0 || n > nonceCeil {
			t.Fatalf("起点越界：%d", n)
		}
	}
}

func TestSearchNonceProducesServerAcceptedSolution(t *testing.T) {
	for _, bits := range []uint8{pow.MinBits, pow.MinBits + 2} {
		params, err := pow.NewParams(bits)
		if err != nil {
			t.Fatal(err)
		}
		nonce, iterations, err := searchNonce(params, 20_000_000)
		if err != nil {
			t.Fatalf("bits=%d: %v", bits, err)
		}
		if iterations < 1 {
			t.Fatalf("bits=%d: 迭代计数 = %d", bits, iterations)
		}
		// 候选必须是纯十进制且落在服务端上限内
		if _, err := strconv.ParseInt(nonce, 10, 64); err != nil {
			t.Fatalf("nonce %q 不是合法整数", nonce)
		}
		for _, r := range nonce {
			if r < '0' || r > '9' {
				t.Fatalf("nonce %q 含非数字字符", nonce)
			}
		}
		ok, err := params.Verify(nonce)
		if err != nil || !ok {
			t.Fatalf("bits=%d nonce=%q 未通过服务端校验 ok=%v err=%v", bits, nonce, ok, err)
		}
	}
}

func TestSearchNonceHonoursIterationCap(t *testing.T) {
	params, err := pow.NewParams(pow.MaxBits) // 24 位：上限 1 次迭代必然找不到
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := searchNonce(params, 1); err == nil {
		t.Fatal("迭代上限过低时应报错，而不是返回空解")
	}
}

func TestVerifyRespCodes(t *testing.T) {
	if got := (verifyResp{}).codes(); got != "-" {
		t.Errorf("空错误码应为 -，got %q", got)
	}
	got := (verifyResp{ErrorCodes: []string{"a", "b"}}).codes()
	if got != "a,b" {
		t.Errorf("codes = %q", got)
	}
}

// evalExpr 必须真的会算题：它答的是脚本从服务端拿到的表达式，读不到库里的答案。
// 这些用例同时是"括号语义"的回归——(9 - 3) × 5 与 9 - 3 × 5 是两个不同的值。
func TestEvalExprAnswersTasks(t *testing.T) {
	cases := map[string]int64{
		"12 + 7":        19,
		"3 - 20":        -17,
		"9 × 8":         72,
		"56 ÷ 8":        7,
		"(9 - 3) × 5":   30,
		"(2 + 3) ÷ 5":   1,
		"(12 - 18) × 2": -12,
		"2 × 3":         6,
	}
	for task, want := range cases {
		got, err := evalExpr(task)
		if err != nil {
			t.Errorf("evalExpr(%q): %v", task, err)
			continue
		}
		if got != want {
			t.Errorf("evalExpr(%q) = %d，期望 %d", task, got, want)
		}
	}
	for _, bad := range []string{"", "1 +", "(1 + 2", "1 ? 2", "1 ÷ 0", "7 ÷ 2", "a × b", "1 + 2 + 3"} {
		if v, err := evalExpr(bad); err == nil {
			t.Errorf("非法题面 %q 竟算出 %d", bad, v)
		}
	}
}

// challengeResp.answer 决定 devclient 走 pow_only 还是 pow_question 形状：
// 带题面就必须带答案，不带题面就不能凭空造一个 answer 字段（阶段 1/2 的契约不能变形）。
func TestAnswerFieldFollowsMode(t *testing.T) {
	var powOnly challengeResp
	if a, err := powOnly.answer(); err != nil || a != "" {
		t.Fatalf("pow_only 不该有答案：%q %v", a, err)
	}
	withQ := challengeResp{Question: map[string]any{"task": "7 × 8"}}
	a, err := withQ.answer()
	if err != nil || a != "56" {
		t.Fatalf("题面求值不符：%q err %v", a, err)
	}
	if _, err := (challengeResp{Question: map[string]any{"task": ""}}).answer(); err == nil {
		t.Error("question 缺 task 应报错，不能静默当成 pow_only")
	}
}
