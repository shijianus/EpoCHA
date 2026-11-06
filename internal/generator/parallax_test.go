package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func parallaxStream(t *testing.T, seed string) io.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParallaxGenerateDeterministic(t *testing.T) {
	g, err := NewParallax(bytes.Repeat([]byte{0x21}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		seed := "parallax-determinism-" + string(rune('0'+d))
		q1, err := g.Generate(context.Background(), Params{Difficulty: d}, parallaxStream(t, seed))
		if err != nil {
			t.Fatalf("d%d Generate: %v", d, err)
		}
		q2, err := g.Generate(context.Background(), Params{Difficulty: d}, parallaxStream(t, seed))
		if err != nil {
			t.Fatalf("d%d Generate(2): %v", d, err)
		}
		b1, _ := json.Marshal(q1.Payload)
		b2, _ := json.Marshal(q2.Payload)
		if !bytes.Equal(b1, b2) || !bytes.Equal(q1.Verifier, q2.Verifier) || !bytes.Equal(q1.Asset, q2.Asset) {
			t.Fatalf("d%d 同 seed 生成不逐字节一致", d)
		}
		if len(q1.Asset) > MaxImageAssetBytes {
			t.Fatalf("d%d 资产 %d 超过 %d", d, len(q1.Asset), MaxImageAssetBytes)
		}
		t.Logf("parallax d%d asset=%d bytes frames=%d", d, len(q1.Asset), q1.Payload.(ParallaxPublic).FrameCount)
	}
}

func TestParallaxAnswerFromMatchesGenerate(t *testing.T) {
	g, err := NewParallax(bytes.Repeat([]byte{0x22}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		seed := "parallax-answer-" + string(rune('0'+d))
		answer, err := ParallaxAnswerFrom(parallaxStream(t, seed), d)
		if err != nil {
			t.Fatalf("d%d AnswerFrom: %v", d, err)
		}
		q, err := g.Generate(context.Background(), Params{Difficulty: d}, parallaxStream(t, seed))
		if err != nil {
			t.Fatalf("d%d Generate: %v", d, err)
		}
		if !g.Verify(q.Verifier, answer) {
			t.Fatalf("d%d 答案 %q 与 verifier 不匹配", d, answer)
		}
		if len(answer) != ParallaxCodeLength {
			t.Fatalf("答案长度 = %d", len(answer))
		}
		for i := 0; i < len(answer); i++ {
			if answer[i] < '0' || answer[i] > '9' {
				t.Fatalf("答案含非数字: %q", answer)
			}
		}
		pub := q.Payload.(ParallaxPublic)
		if pub.RenderMode != RenderModeSpriteSheet || pub.FrameCount < MinFrameCount {
			t.Fatalf("公开题面缺帧参数: %+v", pub)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(q.Asset))
		if err != nil {
			t.Fatalf("PNG 解码失败: %v", err)
		}
		if cfg.Width != pub.ImageWidth*pub.FrameCols || cfg.Height != pub.ImageHeight*pub.FrameRows {
			t.Fatalf("雪碧图尺寸 = %dx%d，期望 %dx%d", cfg.Width, cfg.Height,
				pub.ImageWidth*pub.FrameCols, pub.ImageHeight*pub.FrameRows)
		}
	}
}

func TestParallaxNormalizeAnswer(t *testing.T) {
	g := Parallax{}
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"1234", "1234", true},
		{" 1234 ", "1234", true},
		{"0000", "0000", true},
		{"123", "", false},
		{"12345", "", false},
		{"12 34", "", false},
		{"12a4", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := g.NormalizeAnswer(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("NormalizeAnswer(%q)=(%q,%v)，期望 (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParallaxWrongCodeRejected(t *testing.T) {
	g, _ := NewParallax(bytes.Repeat([]byte{0x23}, 32))
	answer, err := ParallaxAnswerFrom(parallaxStream(t, "parallax-wrong"), 3)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 3}, parallaxStream(t, "parallax-wrong"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := "0000"
	if answer == wrong {
		wrong = "1111"
	}
	if g.Verify(q.Verifier, wrong) {
		t.Fatalf("错误答案 %q 不应通过（正确 %q）", wrong, answer)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatal("正确答案应通过")
	}
}

func TestParallaxSanitizePublic(t *testing.T) {
	g, _ := NewParallax(bytes.Repeat([]byte{0x24}, 32))
	q, err := g.Generate(context.Background(), Params{Difficulty: 2}, parallaxStream(t, "parallax-shape"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(q.Payload)
	if _, err := g.SanitizePublic(raw); err != nil {
		t.Fatalf("合法题面被拒绝: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["answer"] = "1234"
	bad, _ := json.Marshal(m)
	if _, err := g.SanitizePublic(bad); err == nil {
		t.Fatal("额外 answer 字段应被拒绝")
	}
	m = map[string]any{}
	_ = json.Unmarshal(raw, &m)
	m["frame_loop"] = false
	bad, _ = json.Marshal(m)
	if _, err := g.SanitizePublic(bad); err == nil {
		t.Fatal("frame_loop=false 应被拒绝")
	}
	if !strings.Contains(ParallaxCharset, "0") {
		t.Fatal("字符集应包含 0")
	}
}

func TestParallaxM3Space(t *testing.T) {
	if space := len(ParallaxCharset); space != 10 {
		t.Fatalf("字符集大小 = %d", space)
	}
	p := 1.0 / 10000.0
	if p <= 0 || p >= 0.1 {
		t.Fatalf("单次盲猜 = %v，必须远低于 10%%", p)
	}
}
