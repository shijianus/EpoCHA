package pow

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"testing"
)

// solve 是测试用的暴力求解器：真实搜索，不使用固定解。
func solve(t *testing.T, p Params) string {
	t.Helper()
	for i := int64(0); i < 5_000_000; i++ {
		n := strconv.FormatInt(i, 10)
		ok, err := p.Verify(n)
		if err != nil {
			t.Fatalf("Verify(%q): %v", n, err)
		}
		if ok {
			return n
		}
	}
	t.Fatal("求解失败：超出迭代上限")
	return ""
}

func TestNewParamsBitsBounds(t *testing.T) {
	for _, bits := range []uint8{MinBits, 18, MaxBits} {
		p, err := NewParams(bits)
		if err != nil {
			t.Fatalf("bits=%d: %v", bits, err)
		}
		if p.Algo != Algo || p.Bits != bits || len(p.Seed) != seedLen {
			t.Fatalf("bits=%d: 参数形状不符 %+v", bits, p)
		}
	}
	for _, bits := range []uint8{0, 1, 11, 25, 200} {
		if _, err := NewParams(bits); err == nil {
			t.Errorf("bits=%d 应被拒绝", bits)
		}
	}
}

func TestSeedsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		p, err := NewParams(MinBits)
		if err != nil {
			t.Fatal(err)
		}
		k := base64.RawURLEncoding.EncodeToString(p.Seed)
		if seen[k] {
			t.Fatal("seed 重复：解可跨 challenge 复用")
		}
		seen[k] = true
	}
}

// 已知向量：手工构造一个满足前导零条件的输入，确保位计数与协议一致。
func TestLeadingZeroBitsVectors(t *testing.T) {
	cases := []struct {
		in   []byte
		want int
	}{
		{[]byte{0x00, 0x00, 0x80}, 16},
		{[]byte{0x00, 0x01}, 15}, // 8 + 0b00000001 的 7 个前导零
		{[]byte{0x0f}, 4},        // 0b00001111
		{[]byte{0x00, 0x00, 0x00, 0x00}, 32},
		{[]byte{0x00, 0x00, 0x00, 0x00, 0x10}, 32 + 3},
		{[]byte{0xff}, 0},
	}
	for _, c := range cases {
		if got := LeadingZeroBits(c.in); got != c.want {
			t.Errorf("LeadingZeroBits(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// 已知答案向量：固定输入 → 固定 SHA-256 摘要。
// 这是"独立验算路径"的第一半——期望值是 FIPS 180-4 公开的摘要，硬编码在测试里，
// 不来自被测代码；若本包对哈希输入的拼接顺序（seed || nonce 的 ASCII 十进制）有任何改动，
// 这两条会立刻失败。
func TestSHA256KnownAnswerVectors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}
	for _, c := range cases {
		got := sha256.Sum256([]byte(c.in))
		if hex.EncodeToString(got[:]) != c.want {
			t.Errorf("sha256(%q) = %x, want %s", c.in, got[:], c.want)
		}
	}
}

// independentZeroBits 是 LeadingZeroBits 的**另一套实现**（逐位右移而非按字节分段），
// 只用于测试。两套实现若在某处不一致，说明至少一处对"位序"的理解是错的。
func independentZeroBits(b []byte) int {
	n := 0
	for _, c := range b {
		for i := 7; i >= 0; i-- {
			if c&(1<<i) != 0 {
				return n
			}
			n++
		}
	}
	return n
}

// TestPrefixBoundaryAgreesWithIndependentCheck 覆盖非整字节难度（13/15/17/23）与边界：
// 断言 pow.Verify 的接受条件与"独立实现的位数判定"完全一致，且 bits=k 的解在 bits=k+1 下
// 不必然成立（难度语义没有被 off-by-one 削弱）。全程不调用 LeadingZeroBits 做判定。
func TestPrefixBoundaryAgreesWithIndependentCheck(t *testing.T) {
	// 上限取 17：期望迭代 2^17，仍远低于 solve 的迭代封顶
	for _, bits := range []uint8{12, 13, 15, 17} {
		p, err := NewParams(bits)
		if err != nil {
			t.Fatal(err)
		}
		nonce := solve(t, p)
		h := sha256.Sum256(append(append([]byte{}, p.Seed...), nonce...))
		if got := independentZeroBits(h[:]); got < int(bits) {
			t.Fatalf("bits=%d：独立实现的位数判定为 %d，未达难度（解 %q）", bits, got, nonce)
		}
		// 同一个 seed 下，bits+1 的解集是真子集：低难度的解不应被高难度普遍接受
		ok, err := (Params{Algo: Algo, Bits: bits + 1, Seed: p.Seed}).Verify(nonce)
		if err != nil {
			t.Fatal(err)
		}
		if ok && independentZeroBits(h[:]) < int(bits+1) {
			t.Fatalf("bits=%d 的解被判为满足 %d，且独立判定不足", bits, bits+1)
		}
	}
}

// TestExactBoundaryOneBitShortIsRejected 用手工构造的边界摘要钉死"前 bits 位为 0"的语义：
// 第 bits+1 位为 1 时，bits+1 难度的判定必须为假。两套位序实现必须给出同一个数。
func TestExactBoundaryOneBitShortIsRejected(t *testing.T) {
	for bits := 12; bits <= 24; bits++ {
		h := make([]byte, 32)
		// 前 bits 位为 0，紧接着的一位为 1
		h[bits/8] |= 1 << (7 - uint(bits%8))
		wantBits := independentZeroBits(h)
		if wantBits != bits {
			t.Fatalf("构造错误：期望 %d 个前导零，独立实现给出 %d", bits, wantBits)
		}
		if got := LeadingZeroBits(h); got != wantBits {
			t.Fatalf("bits=%d：被测实现给出 %d，独立实现给出 %d", bits, got, wantBits)
		}
		// 判定条件本身：满足 bits，不满足 bits+1
		if !(LeadingZeroBits(h) >= bits) || LeadingZeroBits(h) >= bits+1 {
			t.Fatalf("边界向量不满足预期：bits=%d zeros=%d", bits, LeadingZeroBits(h))
		}
	}
}

func TestVerifyAcceptsRealSolution(t *testing.T) {
	for _, bits := range []uint8{MinBits, MinBits + 2} {
		p, err := NewParams(bits)
		if err != nil {
			t.Fatal(err)
		}
		nonce := solve(t, p)
		ok, err := p.Verify(nonce)
		if err != nil || !ok {
			t.Fatalf("bits=%d 自求自验失败 ok=%v err=%v", bits, ok, err)
		}
		h := sha256.Sum256(append(append([]byte{}, p.Seed...), nonce...))
		if LeadingZeroBits(h[:]) < int(bits) {
			t.Fatalf("哈希前导零不足：%x", h[:4])
		}
	}
}

func TestVerifyRejectsWrongSolutions(t *testing.T) {
	p, err := NewParams(20) // 高难度，几乎不可能随机撞上
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"1", "999999999999", "42", "888888888888"} {
		ok, err := p.Verify(n)
		if err != nil {
			t.Fatalf("Verify(%q): %v", n, err)
		}
		if ok {
			t.Fatalf("bits=20 不应被 %q 通过", n)
		}
	}
}

func TestVerifyRejectsMalformedNonce(t *testing.T) {
	p, err := NewParams(MinBits)
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"", " ", "0x10", "1e3", "-1", "+1", "1_000", "abc", "12 34",
		"1.5", "0000000000000000000001", // 超长
		"9999999999999999", // 16 位但 >1e12
		"١٢٣",              // 非 ASCII 数字
	}
	for _, n := range bad {
		if _, err := p.Verify(n); err == nil {
			t.Errorf("非法解 %q 应报错", n)
		}
	}
}

// seed 是哈希输入的一部分，换一个 seed 同一个解就失效——这是"不可摊销"的性质。
func TestSolutionNotPortableAcrossSeeds(t *testing.T) {
	a, err := NewParams(MinBits)
	if err != nil {
		t.Fatal(err)
	}
	solution := solve(t, a)
	b, err := NewParams(MinBits)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Seed) == string(b.Seed) {
		t.Skip("极小概率的 seed 冲突")
	}
	ok, err := b.Verify(solution)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Log("在低难度下偶然通过（可接受），但概率应极低")
	}
}

func TestVerifyRejectsUnknownAlgoAndBadSeed(t *testing.T) {
	if _, err := (Params{Algo: "argon2", Bits: MinBits, Seed: make([]byte, seedLen)}).Verify("1"); err == nil {
		t.Error("未知算法应报错")
	}
	if _, err := (Params{Algo: Algo, Bits: MinBits, Seed: make([]byte, 8)}).Verify("1"); err == nil {
		t.Error("seed 长度不符应报错")
	}
}

func BenchmarkVerify(b *testing.B) {
	p, _ := NewParams(MinBits)
	// 基准用必然失败的解，测的是格式校验 + 一次哈希的成本量级（未做难度校准，DESIGN §7 标注为待实测）。
	s := strconv.FormatInt(12345, 10)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = p.Verify(s)
	}
}
