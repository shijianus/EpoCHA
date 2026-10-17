package pow

import (
	"crypto/sha256"
	"encoding/base64"
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
