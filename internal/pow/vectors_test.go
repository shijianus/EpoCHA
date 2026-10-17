package pow

// 跨实现共享的**固定测试向量**（阶段 4A）。
//
// 为什么要有这个文件：JS 侧（web/widget/v1.worker.js 的纯 JS SHA-256 + 搜索循环）与 Go 侧
// （本包）必须对同一批输入给出**逐字节相同**的结果。只看"两端各自跑通了自己的测试"
// 证明不了一件事：它们实现的是同一个协议。
// 于是把事实钉成一个 JSON：
//   - Go 测试断言"由本实现重算 == 文件内容"（实现改动会立刻让文件变红）；
//   - Node 测试用 worker 的同一份字节断言"由 JS 实现重算 == 同一文件内容"。
// 两端各自对着同一份真值，差异就会在任一侧显形。
//
// 重新生成：EPOCHA_REGEN_POW_VECTORS=1 go test ./internal/pow/ -run TestSharedVectors
// 生成用的是**写死的种子**（不是随机），所以重跑得到的是同一份字节。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

const vectorsFile = "testdata/pow_vectors.json"

type digestVector struct {
	InputB64URL string `json:"input_b64url"`
	SHA256Hex   string `json:"sha256_hex"`
	Why         string `json:"why"`
}

type powVector struct {
	SeedB64URL string `json:"seed_b64url"`
	Bits       int    `json:"bits"`
	Nonce      string `json:"first_solution_nonce"`
	Attempts   int64  `json:"attempts"`
	ZeroBits   int    `json:"leading_zero_bits"`
}

type rejectedVector struct {
	SeedB64URL string `json:"seed_b64url"`
	Bits       int    `json:"bits"`
	Nonce      string `json:"nonce"`
	Reason     string `json:"reason"`
}

type zeroBitVector struct {
	BytesB64URL string `json:"bytes_b64url"`
	ZeroBits    int    `json:"leading_zero_bits"`
	Why         string `json:"why"`
}

type vectors struct {
	Algorithm     string           `json:"algorithm"`
	Canonical     string           `json:"canonical_form"`
	NonceFormat   string           `json:"nonce_format"`
	Notes         string           `json:"notes"`
	DigestVectors []digestVector   `json:"digest_vectors"`
	ZeroBitCases  []zeroBitVector  `json:"leading_zero_bit_vectors"`
	PoWVectors    []powVector      `json:"pow_vectors"`
	Rejected      []rejectedVector `json:"rejected_nonces"`
}

// fixedSeeds 是写死的 16 字节种子。刻意不用 crypto/rand：向量要能复现。
var fixedSeeds = [][]byte{
	{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f},
	{0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8, 0xf7, 0xf6, 0xf5, 0xf4, 0xf3, 0xf2, 0xf1, 0xf0},
	{0x2f, 0xd1, 0x8a, 0x03, 0x7c, 0xbb, 0x41, 0x9e, 0x5d, 0x6e, 0x11, 0xa0, 0xc3, 0x94, 0x7b, 0x28},
	{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01},
}

func buildVectors() vectors {
	v := vectors{
		Algorithm:   "sha256-prefix",
		Canonical:   "digest = SHA256(seed || ascii_decimal_nonce)，通过条件为 digest 的前导零比特数（按字节序、高位优先）>= bits",
		NonceFormat: "nonce 是十进制字符串，长度 1..16，数值 <= 1000000000000；哈希输入用的是它的 ASCII 数字节，不是数值本身",
		Notes:       "seed 由固定常量给出（可复现）。attempts 是从 0 起算到命中为止尝试的候选个数。",
	}
	// 摘要向量：覆盖空输入、单块边界、以及**PoW 预图像的真实形状**（16 字节 seed + 十进制 nonce）。
	inputs := map[string][]byte{
		"空输入（SHA-256 的标准对照）":        {},
		"abc（标准对照）":                 []byte("abc"),
		"55 字节：填充正好落在 64 字节边界前":     bytesOf(55, 'a'),
		"56 字节：长度字段必须挤到第二块":         bytesOf(56, 'b'),
		"64 字节：正好一块，仍需追加填充块":        bytesOf(64, 'c'),
		"65 字节：两块 + 填充":             bytesOf(65, 'd'),
		"PoW 预图像 seed||\"0\"":       preimage(fixedSeeds[0], "0"),
		"PoW 预图像 seed||\"1234567\"": preimage(fixedSeeds[2], "1234567"),
	}
	for _, name := range sortedKeys(inputs) {
		in := inputs[name]
		sum := sha256.Sum256(in)
		v.DigestVectors = append(v.DigestVectors, digestVector{
			InputB64URL: base64.RawURLEncoding.EncodeToString(in),
			SHA256Hex:   hex.EncodeToString(sum[:]),
			Why:         name,
		})
	}
	// 前导零计数：JS 与 Go 必须按**同一个字节序、同一侧**数。
	// 手写期望值容易数错（本轮就错过），所以这里由实现生成、两端各自比对。
	for _, zb := range [][]byte{
		{},
		{0x00},
		{0x00, 0x00, 0x80},
		{0x0f},
		{0x00, 0x01},
		{0x00, 0x00, 0x00, 0x00, 0x10},
		{0xff},
		{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01},
	} {
		v.ZeroBitCases = append(v.ZeroBitCases, zeroBitVector{
			BytesB64URL: base64.RawURLEncoding.EncodeToString(zb),
			ZeroBits:    LeadingZeroBits(zb),
			Why:         "LeadingZeroBits(" + hex.EncodeToString(zb) + ")",
		})
	}

	for i, seed := range fixedSeeds {
		nonce, attempts, zeroBits := firstSolution(seed, 12)
		v.PoWVectors = append(v.PoWVectors, powVector{
			SeedB64URL: base64.RawURLEncoding.EncodeToString(seed),
			Bits:       12,
			Nonce:      nonce,
			Attempts:   attempts,
			ZeroBits:   zeroBits,
		})
		_ = i
	}
	// 被拒的解：格式非法与"格式对但难度不够"两类都要有，
	// 否则"校验顺序"（先格式后哈希）这一条在 JS 侧就没被测到。
	bad := []struct {
		seed   []byte
		nonce  string
		reason string
	}{
		{fixedSeeds[0], "", "empty"},
		{fixedSeeds[0], "abc", "non-decimal"},
		{fixedSeeds[0], "12.5", "non-decimal"},
		{fixedSeeds[0], "-1", "non-decimal"},
		{fixedSeeds[0], "10000000000000000", "too-long"},
		{fixedSeeds[0], "1000000000001", "over-max-value"},
		{fixedSeeds[0], "999999", "below-difficulty"},
		{fixedSeeds[3], "0000012", "leading-zeros-still-hashes"},
	}
	for _, b := range bad {
		v.Rejected = append(v.Rejected, rejectedVector{
			SeedB64URL: base64.RawURLEncoding.EncodeToString(b.seed),
			Bits:       12, Nonce: b.nonce, Reason: b.reason,
		})
	}
	return v
}

func bytesOf(n int, ch byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = ch
	}
	return out
}

func preimage(seed []byte, nonce string) []byte {
	out := append([]byte{}, seed...)
	return append(out, []byte(nonce)...)
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 排序保证 JSON 里的顺序稳定（否则 map 顺序会让"重新生成"产生无意义 diff）。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// firstSolution 从 0 开始暴力找第一个满足难度的 nonce。
func firstSolution(seed []byte, bits int) (string, int64, int) {
	p := Params{Algo: Algo, Bits: uint8(bits), Seed: seed}
	for n := int64(0); n < 1_000_000; n++ {
		cand := strconv.FormatInt(n, 10)
		ok, err := p.Verify(cand)
		if err != nil {
			panic(err)
		}
		if ok {
			sum := sha256.Sum256(preimage(seed, cand))
			return cand, n + 1, LeadingZeroBits(sum[:])
		}
	}
	panic("固定种子应在 100 万次内找到 12 位的解")
}

func TestSharedVectorsMatchImplementation(t *testing.T) {
	want := buildVectors()
	path := filepath.Join(".", vectorsFile)
	if os.Getenv("EPOCHA_REGEN_POW_VECTORS") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("已重新生成", path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("缺少共享向量 %s（用 EPOCHA_REGEN_POW_VECTORS=1 go test ./internal/pow/ -run TestSharedVectors 生成）: %v", vectorsFile, err)
	}
	var got vectors
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Algorithm != want.Algorithm || len(got.DigestVectors) == 0 || len(got.PoWVectors) == 0 {
		t.Fatalf("向量文件形态不对：%+v", got)
	}

	// 1) 摘要：文件里的 hex 必须等于本实现对同一输入的计算结果。
	for _, dv := range got.DigestVectors {
		in, err := base64.RawURLEncoding.DecodeString(dv.InputB64URL)
		if err != nil {
			t.Fatalf("输入 base64url 解不开（%s）：%v", dv.Why, err)
		}
		sum := sha256.Sum256(in)
		if hex.EncodeToString(sum[:]) != dv.SHA256Hex {
			t.Fatalf("摘要向量不一致（%s）：实现=%s 文件=%s —— 文件已过期或实现被改动", dv.Why, hex.EncodeToString(sum[:]), dv.SHA256Hex)
		}
	}

	// 1b) 前导零计数：文件里的期望值必须等于本实现的结果。
	for _, zb := range got.ZeroBitCases {
		raw, err := base64.RawURLEncoding.DecodeString(zb.BytesB64URL)
		if err != nil {
			t.Fatalf("前导零向量的字节解不开（%s）：%v", zb.Why, err)
		}
		if n := LeadingZeroBits(raw); n != zb.ZeroBits {
			t.Fatalf("前导零计数不一致：%s 实现=%d 文件=%d", zb.Why, n, zb.ZeroBits)
		}
	}

	// 2) PoW：文件里的 first_solution_nonce 必须是**第一个**解，且真的满足难度。
	for _, pv := range got.PoWVectors {
		seed, err := base64.RawURLEncoding.DecodeString(pv.SeedB64URL)
		if err != nil {
			t.Fatal(err)
		}
		p := Params{Algo: Algo, Bits: uint8(pv.Bits), Seed: seed}
		ok, err := p.Verify(pv.Nonce)
		if err != nil {
			t.Fatalf("文件里的解在本实现看来格式非法（nonce=%q）：%v", pv.Nonce, err)
		}
		if !ok {
			t.Fatalf("文件里的解不满足难度：%s bits=%d", pv.Nonce, pv.Bits)
		}
		nonce, attempts, zeroBits := firstSolution(seed, pv.Bits)
		if nonce != pv.Nonce || attempts != pv.Attempts || zeroBits != pv.ZeroBits {
			t.Fatalf("第一个解不一致：实现=(%s,%d,%dbits) 文件=(%s,%d,%dbits)",
				nonce, attempts, zeroBits, pv.Nonce, pv.Attempts, pv.ZeroBits)
		}
	}

	// 3) 被拒的解：要么格式非法（返回 error），要么难度不够（返回 false）。
	for _, rv := range got.Rejected {
		seed, err := base64.RawURLEncoding.DecodeString(rv.SeedB64URL)
		if err != nil {
			t.Fatal(err)
		}
		p := Params{Algo: Algo, Bits: uint8(rv.Bits), Seed: seed}
		ok, err := p.Verify(rv.Nonce)
		switch {
		case err != nil && (rv.Reason == "non-decimal" || rv.Reason == "too-long" ||
			rv.Reason == "over-max-value" || rv.Reason == "empty"):
			// 预期：格式校验先于哈希。
		case err != nil:
			t.Errorf("被拒向量 %q（%s）意外返回错误：%v", rv.Nonce, rv.Reason, err)
		case rv.Reason == "leading-zeros-still-hashes":
			// 前导零是合法十进制串，但哈希结果与原数值不同 ⇒ 只要求"不报错"。
			_ = ok
		case ok:
			t.Errorf("被拒向量 %q（%s）竟然通过了", rv.Nonce, rv.Reason)
		}
	}
}
