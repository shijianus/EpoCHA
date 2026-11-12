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

func symbolStream(t *testing.T, seed string) *bytes.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 65536)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf)
}

func TestSymbolPathDeterministicAndAnswer(t *testing.T) {
	g, err := NewSymbolPath(bytes.Repeat([]byte{0x81}, 32))
	if err != nil {
		t.Fatal(err)
	}
	seed := "symbol-path-answer"
	q1, err := g.Generate(context.Background(), Params{Difficulty: 2}, symbolStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	q2, err := g.Generate(context.Background(), Params{Difficulty: 2}, symbolStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(q1.Payload)
	b2, _ := json.Marshal(q2.Payload)
	if string(b1) != string(b2) || !bytes.Equal(q1.Asset, q2.Asset) || !bytes.Equal(q1.Verifier, q2.Verifier) {
		t.Fatal("同一 seed 的 payload/asset/verifier 必须完全相同")
	}
	spec, err := buildSymbolPath(symbolStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	if g.Verify(q1.Verifier, symbolPathAnswer(spec.Path)) == false {
		t.Fatal("生成器自己产生的路径必须通过 verifier")
	}
	if g.Verify(q1.Verifier, "v1:00.01.02.03.04.05") {
		t.Fatal("错误路径不应通过")
	}
}

func TestSymbolPathSeedsUniqueAndBounded(t *testing.T) {
	for i := 0; i < 300; i++ {
		seed := fmt.Sprintf("symbol-%04d", i)
		spec, err := buildSymbolPath(symbolStream(t, seed))
		if err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
		if spec.MatchingPaths != 1 {
			t.Fatalf("seed %q: matching paths = %d", seed, spec.MatchingPaths)
		}
		if spec.PositionCandidates < 128 {
			t.Fatalf("seed %q: position candidates = %d", seed, spec.PositionCandidates)
		}
		if len(spec.Path) < symbolPathMinLen || len(spec.Path) > symbolPathMaxLen {
			t.Fatalf("seed %q: path length = %d", seed, len(spec.Path))
		}
		for j := 1; j < len(spec.Path); j++ {
			if manhattan(spec.Path[j-1], spec.Path[j]) != 1 {
				t.Fatalf("seed %q: path 不连续", seed)
			}
		}
	}
}

func TestSymbolPathNoLeakInPayload(t *testing.T) {
	g, err := NewSymbolPath(bytes.Repeat([]byte{0x82}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		seed := fmt.Sprintf("symbol-leak-%d", i)
		q, err := g.Generate(context.Background(), Params{Difficulty: 1}, symbolStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		spec, err := buildSymbolPath(symbolStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(q.Payload)
		if strings.Contains(string(raw), symbolPathAnswer(spec.Path)) {
			t.Fatalf("payload 含路径答案: %s", raw)
		}
		for _, forbidden := range []string{"target_path", "solution", "symbol_ids", "expected_answer", "seed"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("payload 含禁用字段 %q: %s", forbidden, raw)
			}
		}
	}
}

func TestSymbolPathAssetBudget(t *testing.T) {
	g, err := NewSymbolPath(bytes.Repeat([]byte{0x83}, 32))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, symbolStream(t, "symbol-asset"))
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
	if cfg.Width != SymbolPathWidth || cfg.Height != SymbolPathHeight {
		t.Fatalf("asset 尺寸 = %dx%d", cfg.Width, cfg.Height)
	}
}

func TestSymbolPathNormalizeAnswer(t *testing.T) {
	g, err := NewSymbolPath(bytes.Repeat([]byte{0x84}, 32))
	if err != nil {
		t.Fatal(err)
	}
	// 00 -> 01 -> 02 -> 08 -> 14 -> 15 是相邻的 6 步路径。
	valid := "v1:00.01.02.08.14.15"
	canon, ok := g.NormalizeAnswer(" V1:00.01.02.08.14.15 ")
	if !ok || canon != valid {
		t.Fatalf("规范化 = (%q,%v)", canon, ok)
	}
	invalid := []string{
		"", "v1:", "v1:00.01.02.08.14", // 长度不足
		"v1:00.01.02.08.14.15.16.22.23", // 超长
		"v1:00.01.02.08.14.21",          // 不相邻
		"v1:00.01.02.08.14.14",          // 重复
		"v1:00.01.02.08.14.36",          // 越界
		"v1:00.01.02.08.14.15x",         // 尾随垃圾
		"v2:00.01.02.08.14.15",          // 非法版本
		"v1:00.01.02.08.14.１5",          // 全角
	}
	for _, bad := range invalid {
		if _, ok := g.NormalizeAnswer(bad); ok {
			t.Fatalf("畸形答案 %q 被接受", bad)
		}
	}
}

func TestSymbolGlyphsAreNotD4Equivalent(t *testing.T) {
	if err := validateSymbolGlyphs(); err != nil {
		t.Fatal(err)
	}
}

func FuzzSymbolPathNormalize(f *testing.F) {
	for _, s := range []string{"", "v1:00.07.08.14.15.21", "v1:\xff"} {
		f.Add(s)
	}
	g, _ := NewSymbolPath(bytes.Repeat([]byte{0x85}, 32))
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = g.NormalizeAnswer(s)
	})
}
