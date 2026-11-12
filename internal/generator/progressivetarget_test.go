package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func progressiveStream(t *testing.T, seed string) *bytes.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32768)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf)
}

func TestProgressiveTargetDeterministicAndAnswer(t *testing.T) {
	g, err := NewProgressiveTarget(bytes.Repeat([]byte{0xc1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	seed := "progressive-answer"
	q1, err := g.Generate(context.Background(), Params{Difficulty: 2}, progressiveStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	q2, err := g.Generate(context.Background(), Params{Difficulty: 2}, progressiveStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(q1.Payload)
	b2, _ := json.Marshal(q2.Payload)
	if string(b1) != string(b2) || !bytes.Equal(q1.Asset, q2.Asset) || !bytes.Equal(q1.Verifier, q2.Verifier) {
		t.Fatal("同一 seed 的 payload/asset/verifier 必须完全相同")
	}
	if len(q1.Verifier) != AnswerVerifierLen {
		t.Fatalf("verifier 长度 = %d", len(q1.Verifier))
	}
	answer, err := ProgressiveTargetAnswerFrom(progressiveStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q1.Verifier, answer) {
		t.Fatalf("正确答案 %q 未通过", answer)
	}
	if g.Verify(q1.Verifier, "v1:0.0.1") && answer != "v1:0.0.1" {
		t.Fatal("错误答案不应通过")
	}
}

func TestProgressiveTargetRoundsHaveExactlyOneTarget(t *testing.T) {
	for i := 0; i < 300; i++ {
		seed := fmt.Sprintf("progressive-%04d", i)
		spec, err := buildProgressiveTarget(progressiveStream(t, seed))
		if err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
		for r := 0; r < progressiveTargetRounds; r++ {
			rd := spec.Rounds[r]
			if rd.TargetIndex < 0 || rd.TargetIndex >= progressiveTargetCells {
				t.Fatalf("seed %q round %d: 目标索引 %d 越界", seed, r, rd.TargetIndex)
			}
			count := 0
			for c := 0; c < progressiveTargetCells; c++ {
				if rd.Glyphs[c] == rd.TargetGlyph {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("seed %q round %d: 目标字形出现 %d 次", seed, r, count)
			}
			if rd.Glyphs[rd.TargetIndex] != rd.TargetGlyph {
				t.Fatalf("seed %q round %d: 目标索引处的字形不是目标", seed, r)
			}
		}
	}
}

func TestProgressiveTargetNoLeakInPayload(t *testing.T) {
	g, err := NewProgressiveTarget(bytes.Repeat([]byte{0xc2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		seed := fmt.Sprintf("progressive-leak-%d", i)
		q, err := g.Generate(context.Background(), Params{Difficulty: 1}, progressiveStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		spec, err := buildProgressiveTarget(progressiveStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(q.Payload)
		if strings.Contains(string(raw), progressiveTargetAnswer(spec)) {
			t.Fatalf("payload 含答案 %q: %s", progressiveTargetAnswer(spec), raw)
		}
		for _, forbidden := range []string{
			"target_index", "target_glyph", "target_positions", "answers", "solution", "seed",
		} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("payload 含禁用字段 %q: %s", forbidden, raw)
			}
		}
	}
}

func TestProgressiveTargetAssetBudgetAndFormat(t *testing.T) {
	g, err := NewProgressiveTarget(bytes.Repeat([]byte{0xc3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, progressiveStream(t, "progressive-asset"))
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Asset) == 0 || len(q.Asset) > 65536 {
		t.Fatalf("asset 长度 = %d", len(q.Asset))
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(q.Asset))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != ProgressiveTargetWidth || cfg.Height != ProgressiveTargetHeight {
		t.Fatalf("asset 尺寸 = %dx%d", cfg.Width, cfg.Height)
	}
}

func TestProgressiveTargetNormalizeAnswer(t *testing.T) {
	g, err := NewProgressiveTarget(bytes.Repeat([]byte{0xc4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"v1:0.4.8", " V1:3.3.0 ", "v1:8.0.8"} {
		if _, ok := g.NormalizeAnswer(in); !ok {
			t.Fatalf("合法答案 %q 被拒绝", in)
		}
	}
	for _, bad := range []string{
		"", "v1:", "v1:0.4", "v1:0.4.8.1", "v1:0..8", "v1:0.4.",
		"v1:.0.4", "v1:9.4.8", "v1:0.4.10", "v1:-1.4.8", "v1:0,4,8",
		"v2:0.4.8", "v1:０.4.8", "v1:0.4.8x",
	} {
		if _, ok := g.NormalizeAnswer(bad); ok {
			t.Fatalf("畸形答案 %q 被接受", bad)
		}
	}
}

func FuzzProgressiveTargetNormalize(f *testing.F) {
	for _, s := range []string{"", "v1:0.4.8", "v1:\xff"} {
		f.Add(s)
	}
	g, _ := NewProgressiveTarget(bytes.Repeat([]byte{0xc5}, 32))
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = g.NormalizeAnswer(s)
	})
}
