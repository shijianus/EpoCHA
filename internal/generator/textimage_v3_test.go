package generator

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"image/png"
	"strings"
	"testing"
)

func newTestTextImageV3(t *testing.T) TextImage {
	t.Helper()
	g, err := NewTextImageV3(bytes.Repeat([]byte{0x44}, testMACKeyLen))
	if err != nil {
		t.Fatalf("NewTextImageV3: %v", err)
	}
	return g
}

func genOneV3(t *testing.T, g TextImage, difficulty int, seed string) (string, Question) {
	t.Helper()
	answer, err := TextImageV3AnswerFrom(detStream(t, "epocha-v3-gen-"+seed))
	if err != nil {
		t.Fatalf("TextImageV3AnswerFrom: %v", err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: difficulty}, detStream(t, "epocha-v3-gen-"+seed))
	if err != nil {
		t.Fatalf("v3 生成失败（难度 %d，seed %s）: %v", difficulty, seed, err)
	}
	return answer, q
}

func altChar(c byte) byte {
	if c == 'A' {
		return 'B'
	}
	return 'A'
}

func corruptTwo(s string) string {
	b := []byte(s)
	if len(b) < 2 {
		return s
	}
	b[0] = altChar(b[0])
	b[1] = altChar(b[1])
	return string(b)
}

func TestTextImageV3IdentityAndShape(t *testing.T) {
	g := newTestTextImageV3(t)
	if g.ID() != TextImageV3ID || g.Version() != "v3" || g.Media() != MediaImage {
		t.Fatalf("v3 身份不符: id=%q version=%q media=%q", g.ID(), g.Version(), g.Media())
	}
	for diff := 1; diff <= MaxDifficulties; diff++ {
		want, err := TextImageV3Params(diff)
		if err != nil {
			t.Fatal(err)
		}
		if err := want.validate(); err != nil {
			t.Fatalf("v3 d%d 参数不合法: %v", diff, err)
		}
		for i := 0; i < 6; i++ {
			answer, q := genOneV3(t, g, diff, seedN("v3-shape", i*7+diff))
			if len(answer) != TextImageV3Length {
				t.Fatalf("v3 答案长度 %d ≠ %d", len(answer), TextImageV3Length)
			}
			for _, r := range answer {
				if !strings.ContainsRune(TextImageV3Charset, r) {
					t.Fatalf("v3 答案含字符集外字符 %q", r)
				}
			}
			if q.MimeType != MimeTypePNG || len(q.Verifier) != verifierNonceSz+TextImageV3Length*charTagSz {
				t.Fatalf("v3 媒体/verifier 形状不符: mime=%q verifier=%d", q.MimeType, len(q.Verifier))
			}
			img, err := png.Decode(bytes.NewReader(q.Asset))
			if err != nil {
				t.Fatalf("v3 d%d 资产不是合法 PNG: %v", diff, err)
			}
			if b := img.Bounds(); b.Dx() != want.Width || b.Dy() != want.Height {
				t.Fatalf("v3 d%d 图像尺寸 %v 与档位 %dx%d 不符", diff, b, want.Width, want.Height)
			}
			if len(q.Asset) > MaxImageAssetBytes {
				t.Fatalf("v3 d%d PNG %d 字节，越界", diff, len(q.Asset))
			}
			if !g.Verify(q.Verifier, answer) {
				t.Fatalf("v3 d%d 正确答案未通过判分", diff)
			}
			if !g.Verify(q.Verifier, strings.ToLower(answer)) {
				t.Fatalf("v3 d%d 小写提交未通过判分", diff)
			}
			// 这是 v3 与 v1/v2 的关键差别：一个字符认错仍然通过。
			if !g.Verify(q.Verifier, corruptFirst(answer)) {
				t.Fatalf("v3 d%d 单字替换被拒（容差 1 未生效）", diff)
			}
			if g.Verify(q.Verifier, corruptTwo(answer)) {
				t.Fatalf("v3 d%d 两个字符错误竟通过", diff)
			}
			pub, err := g.SanitizePublic(mustJSON(t, q.Payload))
			if err != nil {
				t.Fatalf("v3 题面出网失败: %v", err)
			}
			if pub["kind"] != "text-image" || pub["length"] != TextImageV3Length || pub["tolerant"] != 1 {
				t.Fatalf("v3 公开题面形状不符: %+v", pub)
			}
		}
	}
}

// TestTextImageV3AcceptanceSet 直接枚举 M2 的接受域：精确答案 + 6×31 个单字替换。
func TestTextImageV3AcceptanceSet(t *testing.T) {
	g := newTestTextImageV3(t)
	answer, q := genOneV3(t, g, 3, "v3-acceptance")
	accepted := 0
	for i := 0; i < len(answer); i++ {
		for _, c := range []byte(TextImageV3Charset) {
			if c == answer[i] {
				continue
			}
			variant := []byte(answer)
			variant[i] = c
			if !g.Verify(q.Verifier, string(variant)) {
				t.Fatalf("单字替换 %d→%q 被拒: %q", i, c, variant)
			}
			accepted++
		}
	}
	if accepted != len(answer)*(len(TextImageV3Charset)-1) {
		t.Fatalf("接受域计数 %d 不符", accepted)
	}
	if g.Verify(q.Verifier, corruptTwo(answer)) {
		t.Fatal("两个字符错误不应通过")
	}
}

func TestTextImageV3AnswerFromMatchesGenerate(t *testing.T) {
	g := newTestTextImageV3(t)
	for i := 0; i < 20; i++ {
		seed := seedN("v3-match", i)
		want, err := TextImageV3AnswerFrom(detStream(t, "epocha-v3-gen-"+seed))
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := genOneV3(t, g, 3, seed)
		if answer != want {
			t.Fatalf("第 %d 次：v3 离线答案 %q 与生成答案 %q 不符", i, want, answer)
		}
	}
}

func TestTextImageV3NormalizeAnswer(t *testing.T) {
	g := newTestTextImageV3(t)
	valid := "ABCDEF"
	for _, in := range []string{valid, strings.ToLower(valid), "  " + valid + "\n"} {
		got, ok := g.NormalizeAnswer(in)
		if !ok || got != valid {
			t.Fatalf("v3 合法输入 %q 归一化失败: got=%q ok=%v", in, got, ok)
		}
	}
	bad := []string{
		"ABCDE", "ABCDEFG", // 长度不是 6
		"ABCDEI", "ABCDEO", "ABCDE0", "ABCDE1", // 字符集外
		"ABCD EF", "ＡBCDEF", // 内部空白 / 全角
	}
	for _, in := range bad {
		if got, ok := g.NormalizeAnswer(in); ok {
			t.Fatalf("v3 畸形输入 %q 被接受为 %q", in, got)
		}
	}
}

func TestTextImageV3SanitizePublicWhitelist(t *testing.T) {
	g := newTestTextImageV3(t)
	good := TextImagePublic{
		Length: TextImageV3Length, Charset: TextImageV3Charset, AnswerType: "text",
		MaxAnswerLen: TextImageV3Length, ImageWidth: 480, ImageHeight: 160, Tolerant: 1,
	}
	if _, err := g.SanitizePublic(mustJSON(t, good)); err != nil {
		t.Fatalf("v3 合法题面被拒: %v", err)
	}
	cases := map[string]string{
		"容差为 0": `{"length":6,"charset":"` + TextImageV3Charset + `","answer_type":"text","max_answer_len":6,"image_width":480,"image_height":160}`,
		"长度不符":  `{"length":8,"charset":"` + TextImageV3Charset + `","answer_type":"text","max_answer_len":8,"image_width":480,"image_height":160,"tolerant":1}`,
		"额外字段":  `{"length":6,"charset":"` + TextImageV3Charset + `","answer_type":"text","max_answer_len":6,"image_width":480,"image_height":160,"tolerant":1,"answer":"ABCDEF"}`,
		"尺寸越界":  `{"length":6,"charset":"` + TextImageV3Charset + `","answer_type":"text","max_answer_len":6,"image_width":7000,"image_height":160,"tolerant":1}`,
	}
	for name, raw := range cases {
		if _, err := g.SanitizePublic([]byte(raw)); err == nil {
			t.Fatalf("v3 非法题面 %q 未被拒", name)
		}
	}
}

// TestToleranceIsProfileScoped 防止"给 v1 的旧题面加 tolerant 就换判分口径"。
func TestToleranceIsProfileScoped(t *testing.T) {
	g := newTestTextImage(t) // v1, tolerance=0
	raw := mustJSON(t, TextImagePublic{
		Length: TextImageLength, Charset: TextImageCharset, AnswerType: "text",
		MaxAnswerLen: TextImageLength, ImageWidth: 280, ImageHeight: 104, Tolerant: 1,
	})
	if _, err := g.SanitizePublic(raw); err == nil {
		t.Fatal("v1 题面带 tolerant=1 应被拒")
	}
}

func TestTextImageV3ParamsBounds(t *testing.T) {
	for d := 1; d <= MaxDifficulties; d++ {
		p, err := TextImageV3Params(d)
		if err != nil {
			t.Fatalf("d%d: %v", d, err)
		}
		if err := p.validate(); err != nil {
			t.Fatalf("d%d 参数不合法: %v", d, err)
		}
		if !p.style.abstract() || p.style.BlurPasses == 0 || p.style.WarpAmp == 0 || p.style.EraseDots == 0 {
			t.Fatalf("v3 d%d 没有启用后处理: %+v", d, p)
		}
	}
	if _, err := TextImageV3Params(0); err == nil {
		t.Fatal("v3 难度 0 应被拒")
	}
	if _, err := NewTextImageV3(bytes.Repeat([]byte{1}, 16)); !errors.Is(err, ErrBadParams) {
		t.Fatalf("v3 短密钥应被拒: %v", err)
	}
}

func TestTextImageV3PrivateLeak(t *testing.T) {
	g := newTestTextImageV3(t)
	answer, q := genOneV3(t, g, 5, "v3-leak")
	raw := mustJSON(t, q.Payload)
	if bytes.Contains(raw, []byte(answer)) {
		t.Fatal("v3 公开 payload 里出现了答案串")
	}
	if findPrivateLeak(raw, q.Verifier) {
		t.Fatal("v3 公开 payload 里出现了 verifier 字节")
	}
	if bytes.Contains(q.Asset, []byte(answer)) {
		t.Fatal("v3 PNG 原始字节里出现了答案串")
	}
	if bytes.Contains(q.Asset, []byte("PrivateVerifier")) || bytes.Contains(q.Asset, []byte("tolerant")) {
		t.Fatal("v3 PNG 原始字节里出现了私有/契约字段名")
	}
}

func TestTextImageV3CrossKeyFails(t *testing.T) {
	g := newTestTextImageV3(t)
	other, err := NewTextImageV3(bytes.Repeat([]byte{0x55}, testMACKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	answer, q := genOneV3(t, g, 2, "v3-cross-key")
	if !g.Verify(q.Verifier, answer) {
		t.Fatal("原密钥下自验必须通过")
	}
	if other.Verify(q.Verifier, answer) {
		t.Fatal("换密钥后仍接受正确答案（判分密钥可替换）")
	}
}

func TestCharVerifierRejectsMalformed(t *testing.T) {
	key := bytes.Repeat([]byte{0x66}, 32)
	nonce := bytes.Repeat([]byte{0x77}, verifierNonceSz)
	canon := "ABCDEF"
	v := charVerifier(key, nonce, canon)
	if len(v) != verifierNonceSz+len(canon)*charTagSz {
		t.Fatalf("逐字 verifier 长度 %d 不符", len(v))
	}
	if !verifyChars(key, v, canon, 1) {
		t.Fatal("精确答案应通过")
	}
	for _, bad := range []PrivateVerifier{nil, {}, PrivateVerifier("short"), v[:len(v)-1], append(append([]byte{}, v...), 0)} {
		if verifyChars(key, bad, canon, 1) {
			t.Fatalf("畸形 verifier（%d 字节）竟判真", len(bad))
		}
	}
	if verifyChars(key, v, canon, -1) {
		t.Fatal("负容差应判假")
	}
	if verifyChars(key, v, "ABCDE", 1) {
		t.Fatal("长度不一致的答案应判假")
	}
}

func TestCharTagSeparatesPositionAndChar(t *testing.T) {
	key := bytes.Repeat([]byte{0x88}, 32)
	nonce := bytes.Repeat([]byte{0x99}, verifierNonceSz)
	if bytes.Equal(charTag(key, nonce, 1, 'A'), charTag(key, nonce, 2, 'A')) {
		t.Fatal("不同位置的同字符标签相同（位置未分隔）")
	}
	if bytes.Equal(charTag(key, nonce, 1, 'A'), charTag(key, nonce, 1, 'B')) {
		t.Fatal("同位置不同字符标签相同（字符未分隔）")
	}
}

// TestV3VerifierIsNotExactVerifier 钉住"v3 不走 v1/v2 的 48B 路径"。
func TestV3VerifierIsNotExactVerifier(t *testing.T) {
	g := newTestTextImageV3(t)
	_, q := genOneV3(t, g, 1, "v3-shape-check")
	if len(q.Verifier) == AnswerVerifierLen {
		t.Fatal("v3 用了 v1/v2 的 48B verifier")
	}
	// 同一段 nonce/答案，逐字构造与精确构造必须不同（域分隔 + 结构都不同）。
	key := bytes.Repeat([]byte{0x44}, testMACKeyLen)
	nonce := bytes.Repeat([]byte{0x01}, verifierNonceSz)
	if bytes.Equal(charVerifier(key, nonce, "ABCDEF"), answerVerifier(key, nonce, "ABCDEF")) {
		t.Fatal("逐字 verifier 与精确 verifier 撞了")
	}
}

// 用 crypto/rand 生成题目验证"生产熵源路径"也能闭环（不依赖测试确定性流）。
func TestTextImageV3ProductionEntropy(t *testing.T) {
	g := newTestTextImageV3(t)
	for i := 0; i < 4; i++ {
		q, err := g.Generate(context.Background(), Params{Difficulty: 2}, rand.Reader)
		if err != nil {
			t.Fatalf("crypto/rand 路径生成失败: %v", err)
		}
		if len(q.Verifier) != verifierNonceSz+TextImageV3Length*charTagSz {
			t.Fatalf("verifier 长度不符: %d", len(q.Verifier))
		}
		if _, err := json.Marshal(q.Payload); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCharVerifierWireFormatIndependent 在测试侧独立重算逐字线格式。
// 生产构造函数被改动时这里必须一起变红：库里的 verifier 字节是服务端唯一能解释的判分材料。
func TestCharVerifierWireFormatIndependent(t *testing.T) {
	key := bytes.Repeat([]byte{0x44}, testMACKeyLen)
	nonce := bytes.Repeat([]byte{0x01}, verifierNonceSz)
	answer := "ABCDEF"
	want := append([]byte{}, nonce...)
	for i := 0; i < len(answer); i++ {
		m := hmac.New(sha256.New, key)
		m.Write([]byte("epocha/v1|qver-char|"))
		m.Write(nonce)
		m.Write([]byte{0})
		m.Write([]byte{byte(i)})
		m.Write([]byte{0})
		m.Write([]byte{answer[i]})
		want = append(want, m.Sum(nil)...)
	}
	got := charVerifier(key, nonce, answer)
	if !bytes.Equal(got, want) {
		t.Fatalf("逐字 verifier 线格式漂移：got %d 字节，want %d 字节", len(got), len(want))
	}
	if len(got) != verifierNonceSz+len(answer)*charTagSz {
		t.Fatalf("逐字 verifier 长度 %d 不符", len(got))
	}
}
