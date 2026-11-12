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

func crowdStream(t *testing.T, seed string) *bytes.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16384)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf)
}

func TestCrowdFindDeterministicAndAnswer(t *testing.T) {
	g, err := NewCrowdFind(bytes.Repeat([]byte{0x91}, 32))
	if err != nil {
		t.Fatal(err)
	}
	seed := "crowd-answer"
	q1, err := g.Generate(context.Background(), Params{Difficulty: 1}, crowdStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	q2, err := g.Generate(context.Background(), Params{Difficulty: 1}, crowdStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(q1.Payload)
	b2, _ := json.Marshal(q2.Payload)
	if string(b1) != string(b2) || !bytes.Equal(q1.Asset, q2.Asset) || !bytes.Equal(q1.Verifier, q2.Verifier) {
		t.Fatal("同一 seed 的 payload/asset/verifier 必须完全相同")
	}
	spec, err := buildCrowdFind(crowdStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q1.Verifier, fmt.Sprintf("v1:%d", spec.TargetIndex)) {
		t.Fatal("正确索引必须通过")
	}
	wrong := (spec.TargetIndex + 1) % crowdFindCells
	if g.Verify(q1.Verifier, fmt.Sprintf("v1:%d", wrong)) {
		t.Fatal("错误索引不应通过")
	}
}

func TestCrowdFindTargetUniqueAndDistractors(t *testing.T) {
	for i := 0; i < 200; i++ {
		seed := fmt.Sprintf("crowd-%04d", i)
		spec, err := buildCrowdFind(crowdStream(t, seed))
		if err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
		count := 0
		for _, sig := range spec.Signatures {
			if sig == spec.Target {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("seed %q: 目标签名出现 %d 次", seed, count)
		}
		for idx, sig := range spec.Signatures {
			if idx == spec.TargetIndex {
				continue
			}
			d := crowdDistance(sig, spec.Target)
			if d != 1 && d != 2 {
				t.Fatalf("seed %q: distractor 与目标差 %d 个特征", seed, d)
			}
		}
	}
}

func TestCrowdFindNoLeakInPayload(t *testing.T) {
	g, err := NewCrowdFind(bytes.Repeat([]byte{0x92}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		seed := fmt.Sprintf("crowd-leak-%d", i)
		q, err := g.Generate(context.Background(), Params{Difficulty: 1}, crowdStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(q.Payload)
		for _, forbidden := range []string{"target_index", "target_signature", "feature", "solution", "expected_answer", "seed"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("payload 含禁用字段 %q: %s", forbidden, raw)
			}
		}
	}
}

func TestCrowdFindAssetBudget(t *testing.T) {
	g, err := NewCrowdFind(bytes.Repeat([]byte{0x93}, 32))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, crowdStream(t, "crowd-asset"))
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
	if cfg.Width != CrowdFindWidth || cfg.Height != CrowdFindHeight {
		t.Fatalf("asset 尺寸 = %dx%d", cfg.Width, cfg.Height)
	}
}

func TestCrowdFindNormalizeAnswer(t *testing.T) {
	g, err := NewCrowdFind(bytes.Repeat([]byte{0x94}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"v1:0", "v1:35", " V1:7 "} {
		if _, ok := g.NormalizeAnswer(in); !ok {
			t.Fatalf("合法答案 %q 被拒绝", in)
		}
	}
	for _, bad := range []string{"", "v1:", "v1:36", "v1:-1", "v1:01", "v1:1x", "v2:1", "v1:１"} {
		if _, ok := g.NormalizeAnswer(bad); ok {
			t.Fatalf("畸形答案 %q 被接受", bad)
		}
	}
}

func FuzzCrowdFindNormalize(f *testing.F) {
	for _, s := range []string{"", "v1:0", "v1:35", "v1:\xff"} {
		f.Add(s)
	}
	g, _ := NewCrowdFind(bytes.Repeat([]byte{0x95}, 32))
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = g.NormalizeAnswer(s)
	})
}
