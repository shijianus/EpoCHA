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

func silhouetteStream(t *testing.T, seed string) *bytes.Reader {
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

func TestSilhouetteCatalogIsUniqueUnderRotation(t *testing.T) {
	if err := validateSilhouetteCatalog(); err != nil {
		t.Fatal(err)
	}
	for _, item := range silhouetteCatalog {
		if shapeRotationallySymmetric(item.mask) {
			t.Fatalf("%s 有旋转对称", item.name)
		}
		if !shapeConnected(item.mask) {
			t.Fatalf("%s 不连通", item.name)
		}
	}
}

func TestSilhouetteAssemblyDeterministicAndAnswer(t *testing.T) {
	g, err := NewSilhouetteAssembly(bytes.Repeat([]byte{0xe1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	seed := "silhouette-answer"
	q1, err := g.Generate(context.Background(), Params{Difficulty: 2}, silhouetteStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	q2, err := g.Generate(context.Background(), Params{Difficulty: 2}, silhouetteStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(q1.Payload)
	b2, _ := json.Marshal(q2.Payload)
	if string(b1) != string(b2) || !bytes.Equal(q1.Asset, q2.Asset) || !bytes.Equal(q1.Verifier, q2.Verifier) {
		t.Fatal("同一 seed 的 payload/asset/verifier 必须完全相同")
	}
	answer, err := SilhouetteAssemblyAnswerFrom(silhouetteStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q1.Verifier, answer) {
		t.Fatalf("正确答案 %q 未通过", answer)
	}
	if g.Verify(q1.Verifier, "v1:0:0,1:0,2:0,3:0,4:0") && answer != "v1:0:0,1:0,2:0,3:0,4:0" {
		t.Fatal("错误答案不应通过")
	}
}

func TestSilhouetteAssemblyUniqueAndPiecesUsedOnce(t *testing.T) {
	for i := 0; i < 300; i++ {
		seed := fmt.Sprintf("silhouette-%04d", i)
		spec, err := buildSilhouetteAssembly(silhouetteStream(t, seed))
		if err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
		pieceMasks := make([]uint16, silhouettePieces)
		slotMasks := make([]uint16, silhouetteSlots)
		seenIDs := map[int]bool{}
		for p, piece := range spec.Pieces {
			if seenIDs[piece.ID] {
				t.Fatalf("seed %q: 公开 ID %d 重复", seed, piece.ID)
			}
			seenIDs[piece.ID] = true
			pieceMasks[p] = piece.Shape
		}
		used := map[int]bool{}
		for s, slot := range spec.Slots {
			slotMasks[s] = slot.Mask
			if used[slot.Piece] {
				t.Fatalf("seed %q: 部件 %d 被两个槽位使用", seed, slot.Piece)
			}
			used[slot.Piece] = true
			if got := rotateShapeMask(spec.Pieces[slot.Piece].Shape, slot.Rotation); got != slot.Mask {
				t.Fatalf("seed %q slot %d: rotate(piece,%d)=%#x != %#x",
					seed, s, slot.Rotation, got, slot.Mask)
			}
		}
		if len(used) != silhouettePieces {
			t.Fatalf("seed %q: 只用了 %d 个部件", seed, len(used))
		}
		if got := countAssemblies(pieceMasks, slotMasks); got != 1 {
			t.Fatalf("seed %q: 装配解 = %d", seed, got)
		}
	}
}

func TestSilhouetteAssemblyNoMappingLeakInPayload(t *testing.T) {
	g, err := NewSilhouetteAssembly(bytes.Repeat([]byte{0xe2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		seed := fmt.Sprintf("silhouette-leak-%d", i)
		q, err := g.Generate(context.Background(), Params{Difficulty: 1}, silhouetteStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		spec, err := buildSilhouetteAssembly(silhouetteStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(q.Payload)
		if strings.Contains(string(raw), silhouetteAnswer(spec)) {
			t.Fatalf("payload 含答案 %q: %s", silhouetteAnswer(spec), raw)
		}
		for _, forbidden := range []string{
			`"slot_piece":`, `"piece_index":`, `"rotation":`, `"solution":`, `"seed":`,
		} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("payload 含禁用字段 %q: %s", forbidden, raw)
			}
		}
	}
}

func TestSilhouetteAssemblyAssetBudget(t *testing.T) {
	g, err := NewSilhouetteAssembly(bytes.Repeat([]byte{0xe3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, silhouetteStream(t, "silhouette-asset"))
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
	if cfg.Width != SilhouetteWidth || cfg.Height != SilhouetteHeight {
		t.Fatalf("asset 尺寸 = %dx%d", cfg.Width, cfg.Height)
	}
}

func TestSilhouetteAssemblyNormalizeAnswer(t *testing.T) {
	g, err := NewSilhouetteAssembly(bytes.Repeat([]byte{0xe4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"v1:0:2,3:1,4:0,1:3,2:0",
		" V1:4:3,3:2,2:1,1:0,0:3 ",
	} {
		if _, ok := g.NormalizeAnswer(in); !ok {
			t.Fatalf("合法答案 %q 被拒绝", in)
		}
	}
	for _, bad := range []string{
		"", "v1:", "v1:0:2,3:1,4:0,1:3", "v1:0:2,3:1,4:0,1:3,2:0,4:0",
		"v1:0:2,0:1,4:0,1:3,2:0", "v1:5:2,3:1,4:0,1:3,2:0",
		"v1:0:4,3:1,4:0,1:3,2:0", "v1:0:2,3:1,4:0,1:3,2", "v1:0-2,3:1,4:0,1:3,2:0",
		"v2:0:2,3:1,4:0,1:3,2:0", "v1:０:2,3:1,4:0,1:3,2:0",
	} {
		if _, ok := g.NormalizeAnswer(bad); ok {
			t.Fatalf("畸形答案 %q 被接受", bad)
		}
	}
}

func FuzzSilhouetteNormalize(f *testing.F) {
	for _, s := range []string{"", "v1:0:2,3:1,4:0,1:3,2:0", "v1:\xff"} {
		f.Add(s)
	}
	g, _ := NewSilhouetteAssembly(bytes.Repeat([]byte{0xe5}, 32))
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = g.NormalizeAnswer(s)
	})
}
