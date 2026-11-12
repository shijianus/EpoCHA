package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"sort"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func rankStream(t *testing.T, seed string) *bytes.Reader {
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

func TestVisualRankDeterministicAndAnswer(t *testing.T) {
	g, err := NewVisualRank(bytes.Repeat([]byte{0xa1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	seed := "rank-answer"
	q1, err := g.Generate(context.Background(), Params{Difficulty: 1}, rankStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	q2, err := g.Generate(context.Background(), Params{Difficulty: 1}, rankStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(q1.Payload)
	b2, _ := json.Marshal(q2.Payload)
	if string(b1) != string(b2) || !bytes.Equal(q1.Asset, q2.Asset) || !bytes.Equal(q1.Verifier, q2.Verifier) {
		t.Fatal("同一 seed 的 payload/asset/verifier 必须完全相同")
	}
	spec, err := buildVisualRank(rankStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q1.Verifier, rankAnswer(spec)) {
		t.Fatal("正确排序必须通过")
	}
	if g.Verify(q1.Verifier, "v1:A.B.C.D.E") && rankAnswer(spec) != "v1:A.B.C.D.E" {
		t.Fatal("错误排序不应通过")
	}
}

func TestVisualRankCountsDistinctAndOrdered(t *testing.T) {
	for i := 0; i < 300; i++ {
		seed := fmt.Sprintf("rank-%04d", i)
		spec, err := buildVisualRank(rankStream(t, seed))
		if err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
		seen := map[int]bool{}
		for _, c := range spec.Cards {
			if seen[c.Segments] {
				t.Fatalf("seed %q: 线段数重复 %d", seed, c.Segments)
			}
			seen[c.Segments] = true
		}
		if len(seen) != 5 {
			t.Fatalf("seed %q: 只有 %d 个不同线段数", seed, len(seen))
		}
		vals := make([]int, 0, 5)
		for v := range seen {
			vals = append(vals, v)
		}
		sort.Ints(vals)
		for j := 1; j < len(vals); j++ {
			if vals[j]-vals[j-1] < 2 {
				t.Fatalf("seed %q: 线段数间距 %d", seed, vals[j]-vals[j-1])
			}
		}
		if len(spec.Order) != 5 {
			t.Fatalf("seed %q: order 长度 %d", seed, len(spec.Order))
		}
		for _, p := range spec.Cards {
			if len(p.Points) != p.Segments+1 {
				t.Fatalf("seed %q: 线段数 %d 但点数 %d", seed, p.Segments, len(p.Points))
			}
		}
	}
}

func TestVisualRankNoLeakInPayload(t *testing.T) {
	g, err := NewVisualRank(bytes.Repeat([]byte{0xa2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		seed := fmt.Sprintf("rank-leak-%d", i)
		q, err := g.Generate(context.Background(), Params{Difficulty: 1}, rankStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(q.Payload)
		for _, forbidden := range []string{"\"segments\":", "\"segment_count\"", "solution", "\"order\":", "expected_answer", "seed"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("payload 含禁用字段 %q: %s", forbidden, raw)
			}
		}
	}
}

func TestVisualRankAssetBudget(t *testing.T) {
	g, err := NewVisualRank(bytes.Repeat([]byte{0xa3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, rankStream(t, "rank-asset"))
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
	if cfg.Width != VisualRankWidth || cfg.Height != VisualRankHeight {
		t.Fatalf("asset 尺寸 = %dx%d", cfg.Width, cfg.Height)
	}
}

func TestVisualRankNormalizeAnswer(t *testing.T) {
	g, err := NewVisualRank(bytes.Repeat([]byte{0xa4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if canon, ok := g.NormalizeAnswer(" v1:a.b.c.d.e "); !ok || canon != "v1:A.B.C.D.E" {
		t.Fatalf("规范化 = (%q,%v)", canon, ok)
	}
	for _, bad := range []string{"", "v1:", "v1:A.B.C.D", "v1:A.B.C.D.E.F", "v1:A.B.C.D.D", "v1:A.B.C.D.1", "v2:A.B.C.D.E"} {
		if _, ok := g.NormalizeAnswer(bad); ok {
			t.Fatalf("畸形答案 %q 被接受", bad)
		}
	}
}

func FuzzVisualRankNormalize(f *testing.F) {
	for _, s := range []string{"", "v1:A.B.C.D.E", "v1:\xff"} {
		f.Add(s)
	}
	g, _ := NewVisualRank(bytes.Repeat([]byte{0xa5}, 32))
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = g.NormalizeAnswer(s)
	})
}
