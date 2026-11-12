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

func shellStream(t *testing.T, seed string) *bytes.Reader {
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

func TestShellTrackDeterministicAndAnswer(t *testing.T) {
	g, err := NewShellTrack(bytes.Repeat([]byte{0xd1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	seed := "shell-answer"
	q1, err := g.Generate(context.Background(), Params{Difficulty: 2}, shellStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	q2, err := g.Generate(context.Background(), Params{Difficulty: 2}, shellStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(q1.Payload)
	b2, _ := json.Marshal(q2.Payload)
	if string(b1) != string(b2) || !bytes.Equal(q1.Asset, q2.Asset) || !bytes.Equal(q1.Verifier, q2.Verifier) {
		t.Fatal("同一 seed 的 payload/asset/verifier 必须完全相同")
	}
	answer, err := ShellTrackAnswerFrom(shellStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q1.Verifier, answer) {
		t.Fatalf("正确答案 %q 未通过", answer)
	}
	if g.Verify(q1.Verifier, "v1:0.0.0") && answer != "v1:0.0.0" {
		t.Fatal("错误答案不应通过")
	}
}

func TestShellTrackSwapsMoveTargetAndFinalIsConsistent(t *testing.T) {
	for i := 0; i < 300; i++ {
		seed := fmt.Sprintf("shell-%04d", i)
		spec, err := buildShellTrack(shellStream(t, seed))
		if err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
		for r := 0; r < shellTrackRounds; r++ {
			rd := spec.Rounds[r]
			if rd.Moves < 2 {
				t.Fatalf("seed %q round %d: 目标只移动 %d 次", seed, r, rd.Moves)
			}
			slots := [shellTrackSlots]int{0, 1, 2, 3}
			targetSlot := rd.TargetStart
			for _, sw := range rd.Swaps {
				if sw[0] < 0 || sw[0] >= shellTrackSlots || sw[1] < 0 || sw[1] >= shellTrackSlots || sw[0] == sw[1] {
					t.Fatalf("seed %q round %d: 非法交换 %v", seed, r, sw)
				}
				slots[sw[0]], slots[sw[1]] = slots[sw[1]], slots[sw[0]]
				if targetSlot == sw[0] {
					targetSlot = sw[1]
				} else if targetSlot == sw[1] {
					targetSlot = sw[0]
				}
			}
			if targetSlot != rd.TargetFinal {
				t.Fatalf("seed %q round %d: 跟踪终点 %d != 记录 %d", seed, r, targetSlot, rd.TargetFinal)
			}
			if rd.TargetStart < 0 || rd.TargetStart >= shellTrackSlots ||
				rd.TargetFinal < 0 || rd.TargetFinal >= shellTrackSlots {
				t.Fatalf("seed %q round %d: 目标索引越界", seed, r)
			}
		}
	}
}

func TestShellTrackNoSwapLeakInPayload(t *testing.T) {
	g, err := NewShellTrack(bytes.Repeat([]byte{0xd2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		seed := fmt.Sprintf("shell-leak-%d", i)
		q, err := g.Generate(context.Background(), Params{Difficulty: 1}, shellStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		spec, err := buildShellTrack(shellStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(q.Payload)
		if strings.Contains(string(raw), shellTrackAnswer(spec)) {
			t.Fatalf("payload 含答案 %q: %s", shellTrackAnswer(spec), raw)
		}
		for _, forbidden := range []string{
			`"target_index":`, `"target_start":`, `"target_final":`, `"swaps":`,
			`"solution":`, `"secret":`, `"seed":`,
		} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("payload 含禁用字段 %q: %s", forbidden, raw)
			}
		}
	}
}

func TestShellTrackAssetBudgetAndSpriteGeometry(t *testing.T) {
	g, err := NewShellTrack(bytes.Repeat([]byte{0xd3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, shellStream(t, "shell-asset"))
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
	if cfg.Width != ShellTrackFrameW*shellTrackFrameCols ||
		cfg.Height != ShellTrackFrameH*shellTrackFrameRows {
		t.Fatalf("asset 尺寸 = %dx%d", cfg.Width, cfg.Height)
	}
}

func TestShellTrackNormalizeAnswer(t *testing.T) {
	g, err := NewShellTrack(bytes.Repeat([]byte{0xd4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"v1:0.3.1", " V1:2.2.0 ", "v1:3.3.3"} {
		if _, ok := g.NormalizeAnswer(in); !ok {
			t.Fatalf("合法答案 %q 被拒绝", in)
		}
	}
	for _, bad := range []string{
		"", "v1:", "v1:0.3", "v1:0.3.1.2", "v1:0..1", "v1:0.3.",
		"v1:.0.1", "v1:4.0.0", "v1:-1.0.0", "v1:0,1,2", "v2:0.1.2", "v1:０.1.2", "v1:0.1.2x",
	} {
		if _, ok := g.NormalizeAnswer(bad); ok {
			t.Fatalf("畸形答案 %q 被接受", bad)
		}
	}
}

func FuzzShellTrackNormalize(f *testing.F) {
	for _, s := range []string{"", "v1:0.3.1", "v1:\xff"} {
		f.Add(s)
	}
	g, _ := NewShellTrack(bytes.Repeat([]byte{0xd5}, 32))
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = g.NormalizeAnswer(s)
	})
}
