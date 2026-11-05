package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"strings"
	"testing"
)

func newTestTextImageV2(t *testing.T) TextImage {
	t.Helper()
	g, err := NewTextImageV2(bytes.Repeat([]byte{0x22}, testMACKeyLen))
	if err != nil {
		t.Fatalf("NewTextImageV2: %v", err)
	}
	return g
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func genOneV2(t *testing.T, g TextImage, difficulty int, seed string) (string, Question) {
	t.Helper()
	answer, q, err := g.generateWithAnswer(context.Background(), Params{Difficulty: difficulty}, detStream(t, "epocha-v2-gen-"+seed))
	if err != nil {
		t.Fatalf("v2 生成失败（难度 %d，seed %s）: %v", difficulty, seed, err)
	}
	return answer, q
}

func TestTextImageV2IdentityAndShape(t *testing.T) {
	g := newTestTextImageV2(t)
	if g.ID() != TextImageV2ID || g.Version() != "v2" || g.Media() != MediaImage {
		t.Fatalf("v2 身份不符: id=%q version=%q media=%q", g.ID(), g.Version(), g.Media())
	}
	for diff := 1; diff <= MaxDifficulties; diff++ {
		want, err := TextImageV2Params(diff)
		if err != nil {
			t.Fatal(err)
		}
		if err := want.validate(); err != nil {
			t.Fatalf("v2 d%d 参数不合法: %v", diff, err)
		}
		for i := 0; i < 8; i++ {
			answer, q := genOneV2(t, g, diff, seedN("v2-shape", i*7+diff))
			if len(answer) != TextImageV2Length {
				t.Fatalf("v2 答案长度 %d ≠ %d", len(answer), TextImageV2Length)
			}
			for _, r := range answer {
				if !strings.ContainsRune(TextImageV2Charset, r) {
					t.Fatalf("v2 答案含字符集外字符 %q", r)
				}
			}
			if q.MimeType != MimeTypePNG || len(q.Verifier) != AnswerVerifierLen {
				t.Fatalf("v2 媒体/verifier 形状不符: mime=%q verifier=%d", q.MimeType, len(q.Verifier))
			}
			img, err := png.Decode(bytes.NewReader(q.Asset))
			if err != nil {
				t.Fatalf("v2 d%d 资产不是合法 PNG: %v", diff, err)
			}
			if b := img.Bounds(); b.Dx() != want.Width || b.Dy() != want.Height {
				t.Fatalf("v2 d%d 图像尺寸 %v 与档位 %dx%d 不符", diff, b, want.Width, want.Height)
			}
			if len(q.Asset) > MaxImageAssetBytes {
				t.Fatalf("v2 d%d PNG %d 字节，越界", diff, len(q.Asset))
			}
			if !g.Verify(q.Verifier, answer) {
				t.Fatalf("v2 d%d 正确答案未通过判分", diff)
			}
			if !g.Verify(q.Verifier, strings.ToLower(answer)) {
				t.Fatalf("v2 d%d 小写提交未通过判分", diff)
			}
			if g.Verify(q.Verifier, corruptFirst(answer)) {
				t.Fatalf("v2 d%d 改了一个字符竟通过判分", diff)
			}
			pub, err := g.SanitizePublic(mustJSON(t, q.Payload))
			if err != nil {
				t.Fatalf("v2 题面出网失败: %v", err)
			}
			if pub["kind"] != "text-image" || pub["length"] != TextImageV2Length {
				t.Fatalf("v2 公开题面形状不符: %+v", pub)
			}
		}
	}
}

func TestTextImageV2AnswerFromMatchesGenerate(t *testing.T) {
	g := newTestTextImageV2(t)
	for i := 0; i < 20; i++ {
		seed := seedN("v2-match", i)
		want, err := TextImageV2AnswerFrom(detStream(t, "epocha-v2-gen-"+seed))
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := genOneV2(t, g, 3, seed)
		if answer != want {
			t.Fatalf("第 %d 次：v2 离线答案 %q 与生成答案 %q 不符", i, want, answer)
		}
	}
}

func TestTextImageV2NormalizeAnswer(t *testing.T) {
	g := newTestTextImageV2(t)
	valid := "ABCDEFGH"
	for _, in := range []string{valid, strings.ToLower(valid), "  " + valid + "\n"} {
		got, ok := g.NormalizeAnswer(in)
		if !ok || got != valid {
			t.Fatalf("v2 合法输入 %q 归一化失败: got=%q ok=%v", in, got, ok)
		}
	}
	bad := []string{
		"ABCDEFG", "ABCDEFGHJ", // 长度不是 8
		"ABCDEFGI", "ABCDEFGO", "ABCDEFG0", "ABCDEFG1", // 字符集外
		"ABCD EFG", "ＡBCDEFGH", // 内部空白 / 全角
	}
	for _, in := range bad {
		if got, ok := g.NormalizeAnswer(in); ok {
			t.Fatalf("v2 畸形输入 %q 被接受为 %q", in, got)
		}
	}
}

func TestTextImageV2SanitizePublicWhitelist(t *testing.T) {
	g := newTestTextImageV2(t)
	good := TextImagePublic{
		Length: TextImageV2Length, Charset: TextImageV2Charset, AnswerType: "text",
		MaxAnswerLen: TextImageV2Length, ImageWidth: 480, ImageHeight: 160,
	}
	if _, err := g.SanitizePublic(mustJSON(t, good)); err != nil {
		t.Fatalf("v2 合法题面被拒: %v", err)
	}
	cases := map[string]string{
		"长度不符":   `{"length":6,"charset":"` + TextImageV2Charset + `","answer_type":"text","max_answer_len":8,"image_width":480,"image_height":160}`,
		"字符集不符":  `{"length":8,"charset":"ABC","answer_type":"text","max_answer_len":8,"image_width":480,"image_height":160}`,
		"答案类型不符": `{"length":8,"charset":"` + TextImageV2Charset + `","answer_type":"integer","max_answer_len":8,"image_width":480,"image_height":160}`,
		"额外字段":   `{"length":8,"charset":"` + TextImageV2Charset + `","answer_type":"text","max_answer_len":8,"image_width":480,"image_height":160,"answer":"ABCDEFGH"}`,
		"尺寸越界":   `{"length":8,"charset":"` + TextImageV2Charset + `","answer_type":"text","max_answer_len":8,"image_width":7000,"image_height":160}`,
	}
	for name, raw := range cases {
		if _, err := g.SanitizePublic([]byte(raw)); err == nil {
			t.Fatalf("v2 非法题面 %q 未被拒", name)
		}
	}
}

func TestTextImageV2AssetHasNoAnswerChunk(t *testing.T) {
	g := newTestTextImageV2(t)
	answer, q := genOneV2(t, g, 5, "v2-asset")
	if bytes.Contains(q.Asset, []byte(answer)) {
		t.Fatal("v2 PNG 原始字节里出现了答案串")
	}
	if bytes.Contains(q.Asset, []byte("PrivateVerifier")) || bytes.Contains(q.Asset, []byte("answer")) {
		t.Fatal("v2 PNG 原始字节里出现了私有字段名")
	}
	pub, err := json.Marshal(q.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(pub, []byte(answer)) {
		t.Fatal("v2 公开 payload 里出现了答案串")
	}
}

func TestTextImageV2ParamsBounds(t *testing.T) {
	for d := 1; d <= MaxDifficulties; d++ {
		p, err := TextImageV2Params(d)
		if err != nil {
			t.Fatal(err)
		}
		if p.Width != 480 || p.Height != 160 || p.style.RotationDeg <= 0 || p.style.Stroke <= 0 {
			t.Fatalf("v2 d%d 档位不符合强形变契约: %+v", d, p)
		}
	}
	if _, err := TextImageV2Params(0); err == nil {
		t.Fatal("v2 难度 0 应被拒")
	}
	if _, err := TextImageV2Params(MaxDifficulties + 1); err == nil {
		t.Fatal("v2 越界难度应被拒")
	}
}
