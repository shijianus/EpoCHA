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
