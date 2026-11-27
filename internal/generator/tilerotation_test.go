package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func tileStream(t *testing.T, seed string) *bytes.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8192)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf)
}

func TestTileRotationDeterministicAndAnswer(t *testing.T) {
	g, err := NewTileRotation(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	seed := "tile-rotation-answer"
	q1, err := g.Generate(context.Background(), Params{Difficulty: 1}, tileStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	q2, err := g.Generate(context.Background(), Params{Difficulty: 1}, tileStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(q1.Payload)
	b2, _ := json.Marshal(q2.Payload)
	if string(b1) != string(b2) || !bytes.Equal(q1.Asset, q2.Asset) || !bytes.Equal(q1.Verifier, q2.Verifier) {
		t.Fatal("同一 seed 的 payload/asset/verifier 必须完全相同")
	}
	if len(q1.Verifier) != AnswerVerifierLen {
		t.Fatalf("verifier 长度 = %d，期望 %d", len(q1.Verifier), AnswerVerifierLen)
	}
	spec, err := buildTileRotation(tileStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	if spec.CandidateSpace < 64 {
		t.Fatalf("候选空间 = %d，要求 ≥64", spec.CandidateSpace)
	}
	if !g.Verify(q1.Verifier, tileAnswerFromSpec(spec)) {
		t.Fatal("生成器自己产生的答案必须通过 verifier")
	}
	if g.Verify(q1.Verifier, "v1:000000000") {
		t.Fatal("错误答案不应通过")
	}
}

// 10,000 个确定性 seed：初始状态不是答案、恰好一个合法最终状态、候选空间 ≥64。
func TestTileRotationTenThousandSeeds(t *testing.T) {
	const n = 10000
	for i := 0; i < n; i++ {
		seed := fmt.Sprintf("tile-%05d", i)
		spec, err := buildTileRotation(tileStream(t, seed))
		if err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
		if spec.Initial == spec.Solution {
			t.Fatalf("seed %q: 初始状态就是答案", seed)
		}
		if spec.SolutionCount != 1 {
			t.Fatalf("seed %q: 合法最终状态 = %d，期望 1", seed, spec.SolutionCount)
		}
		if spec.CandidateSpace < 64 {
			t.Fatalf("seed %q: 候选空间 = %d，要求 ≥64", seed, spec.CandidateSpace)
		}
		if !boardIsConnectedAndClosed(spec.Solution[:], tileRotationRows, tileRotationCols) {
			t.Fatalf("seed %q: 生成解不是连通闭合网络", seed)
		}
	}
}

func TestTileRotationSymmetryDoesNotPessimizeCandidateSpace(t *testing.T) {
	cases := []struct {
		mask  edgeMask
		turns int
	}{
		{0, 1}, {0x0f, 1}, {0x05, 2}, {0x0a, 2}, {0x01, 4}, {0x03, 4}, {0x07, 4},
	}
	for _, tc := range cases {
		seen := map[edgeMask]bool{}
		for i := 0; i < 4; i++ {
			m := tc.mask
			for j := 0; j < i; j++ {
				m = rotateMaskCW(m)
			}
			seen[m] = true
		}
		if len(seen) != tc.turns {
			t.Fatalf("mask %#x 的不同旋转 = %d，期望 %d", tc.mask, len(seen), tc.turns)
		}
	}
	// 对称 tile 的等价旋转不能把同一个最终状态计成多个解。
	masks := []edgeMask{0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05}
	count, _ := countTileSolutions(masks, tileRotationRows, tileRotationCols)
	if count > 1 {
		t.Fatalf("相同 mask 的重复旋转被计成 %d 个解", count)
	}
}

func TestTileRotationPayloadDoesNotLeakSolution(t *testing.T) {
	g, err := NewTileRotation(bytes.Repeat([]byte{0x72}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		q, err := g.Generate(context.Background(), Params{Difficulty: 1}, tileStream(t, fmt.Sprintf("tile-leak-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(q.Payload)
		spec, err := buildTileRotation(tileStream(t, fmt.Sprintf("tile-leak-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		answer := tileAnswerFromSpec(spec)
		if strings.Contains(string(raw), answer) {
			t.Fatalf("payload 含最终答案 %q: %s", answer, raw)
		}
		for _, forbidden := range []string{"solution", "target_masks", "expected_answer", "seed"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("payload 含禁用字段 %q: %s", forbidden, raw)
			}
		}
	}
}

func TestTileRotationAssetBudgetAndFormat(t *testing.T) {
	g, err := NewTileRotation(bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, tileStream(t, "tile-asset"))
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Asset) == 0 || len(q.Asset) > 65536 {
		t.Fatalf("asset 长度 = %d", len(q.Asset))
	}
	if q.MimeType != MimeTypePNG {
		t.Fatalf("mime = %q", q.MimeType)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(q.Asset))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != TileRotationWidth || cfg.Height != TileRotationHeight {
		t.Fatalf("asset 尺寸 = %dx%d", cfg.Width, cfg.Height)
	}
}

// Decode the rendered pixels independently: geometric variety must not change
// the edge masks or disconnect any arm from its junction.
func TestTileRotationVisibleGeometryMatchesMasks(t *testing.T) {
	offsets := [tileRotationCells]image.Point{
		{X: -12, Y: -12}, {X: 0, Y: -12}, {X: 12, Y: -12},
		{X: -12, Y: 0}, {}, {X: 12, Y: 0},
		{X: -12, Y: 12}, {X: 0, Y: 12}, {X: 12, Y: 12},
	}
	for mask := edgeMask(1); mask < 16; mask++ {
		masks := make([]edgeMask, tileRotationCells)
		for i := range masks {
			masks[i] = mask
		}
		asset, err := renderTileRotation(masks, offsets)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(asset))
		if err != nil {
			t.Fatal(err)
		}
		dark := func(p image.Point) bool { r, _, _, _ := img.At(p.X, p.Y).RGBA(); return r < 100*257 }
		for i, offset := range offsets {
			center := tileRotationCenter(i/3, i%3)
			rect := image.Rect(center.X-48, center.Y-48, center.X+48, center.Y+48)
			start := center.Add(offset)
			if !dark(start) {
				t.Fatalf("mask=%x cell=%d missing junction", mask, i)
			}
			seen := map[image.Point]bool{start: true}
			queue := []image.Point{start}
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				for _, step := range []image.Point{{X: 1}, {X: -1}, {Y: 1}, {Y: -1}} {
					n := p.Add(step)
					if n.In(rect) && !seen[n] && dark(n) {
						seen[n] = true
						queue = append(queue, n)
					}
				}
			}
			ports := []image.Point{{X: center.X, Y: rect.Min.Y}, {X: rect.Max.X - 1, Y: center.Y}, {X: center.X, Y: rect.Max.Y - 1}, {X: rect.Min.X, Y: center.Y}}
			for side, port := range ports {
				want := mask&(1<<side) != 0
				if dark(port) != want || seen[port] != want {
					t.Fatalf("mask=%x cell=%d port=%d ink/connected disagree", mask, i, side)
				}
			}
			for p := range seen {
				if (p.X == rect.Min.X || p.X == rect.Max.X-1) && abs64(int64(p.Y-center.Y)) > 3 ||
					(p.Y == rect.Min.Y || p.Y == rect.Max.Y-1) && abs64(int64(p.X-center.X)) > 3 {
					t.Fatalf("mask=%x cell=%d stray boundary crossing %v", mask, i, p)
				}
			}
		}
	}
}

func TestTileRotationEntropyFailureIsBounded(t *testing.T) {
	g, err := NewTileRotation(bytes.Repeat([]byte{0x74}, 32))
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Generate(context.Background(), Params{Difficulty: 1}, strings.NewReader(""))
	if err == nil {
		t.Fatal("空熵源必须报错")
	}
}

func TestTileRotationNormalizeAnswer(t *testing.T) {
	g, err := NewTileRotation(bytes.Repeat([]byte{0x75}, 32))
	if err != nil {
		t.Fatal(err)
	}
	valid := "v1:1a2b3c4d5"
	canon, ok := g.NormalizeAnswer("  V1:1A2B3C4D5\t")
	if !ok || canon != valid {
		t.Fatalf("规范化 = (%q,%v)", canon, ok)
	}
	for _, bad := range []string{"", "v1:", "v1:1a2b3c4d", "v1:1a2b3c4d5x", "v2:1a2b3c4d5", "v1:1a2b3c4d 5", "v1:１a2b3c4d5"} {
		if _, ok := g.NormalizeAnswer(bad); ok {
			t.Fatalf("畸形答案 %q 被接受", bad)
		}
	}
}

func FuzzTileRotationNormalize(f *testing.F) {
	for _, s := range []string{"", "v1:", "v1:1a2b3c4d5", "v1:\xff"} {
		f.Add(s)
	}
	g, _ := NewTileRotation(bytes.Repeat([]byte{0x76}, 32))
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = g.NormalizeAnswer(s)
	})
}
