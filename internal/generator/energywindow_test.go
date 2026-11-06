package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"math"
	"strconv"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func energyStream(t *testing.T, seed string) *bytes.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic reader is infinite; read a bounded prefix so the test helper can be reused.
	buf := make([]byte, 4096)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf)
}

func TestEnergyWindowDeterministicAndAnswer(t *testing.T) {
	g, err := NewEnergyWindow(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		seed := "energy-" + string(rune('0'+d))
		answer, err := EnergyWindowAnswerFrom(energyStream(t, seed), d)
		if err != nil {
			t.Fatal(err)
		}
		q, err := g.Generate(context.Background(), Params{Difficulty: d}, energyStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		q2, err := g.Generate(context.Background(), Params{Difficulty: d}, energyStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		p1, _ := json.Marshal(q.Payload)
		p2, _ := json.Marshal(q2.Payload)
		if !bytes.Equal(p1, p2) || !bytes.Equal(q.Verifier, q2.Verifier) || !bytes.Equal(q.Asset, q2.Asset) {
			t.Fatalf("d%d 同 seed 生成不逐字节一致", d)
		}
		if !g.Verify(q.Verifier, answer) {
			t.Fatalf("d%d 答案 %q 未通过", d, answer)
		}
		pub := q.Payload.(EnergyWindowPublic)
		if pub.AnswerType != AnswerTypeTiming || pub.FrameCount != energyFrames || pub.FrameMS != energyFrameMS {
			t.Fatalf("题面字段不符: %+v", pub)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(q.Asset))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Width != energyFrameW*energyCols || cfg.Height != energyFrameH*energyRows {
			t.Fatalf("雪碧图尺寸 = %dx%d", cfg.Width, cfg.Height)
		}
		if len(q.Asset) > MaxImageAssetBytes {
			t.Fatalf("资产 %d 超限", len(q.Asset))
		}
		t.Logf("energy d%d asset=%d", d, len(q.Asset))
	}
}

func TestEnergyWindowToleranceAndM3(t *testing.T) {
	g, _ := NewEnergyWindow(bytes.Repeat([]byte{0x72}, 32))
	answer, err := EnergyWindowAnswerFrom(energyStream(t, "energy-boundary"), 2)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 2}, energyStream(t, "energy-boundary"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := strconv.Atoi(answer)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{-30, 0, 30} {
		if target+delta < 0 || target+delta >= energyCycleMS {
			continue
		}
		if !g.Verify(q.Verifier, strconv.Itoa(target+delta)) {
			t.Fatalf("容差内 delta=%d 未通过", delta)
		}
	}
	for _, delta := range []int{-66, 66} {
		if target+delta < 0 || target+delta >= energyCycleMS {
			continue
		}
		if g.Verify(q.Verifier, strconv.Itoa(target+delta)) {
			t.Fatalf("容差外 delta=%d 不应通过", delta)
		}
	}
	tol, _ := EnergyWindowTolerance(2)
	p := float64(2*tol+1) / float64(energyFeasibleBuckets)
	p3 := 1 - math.Pow(1-p, 3)
	if p >= 0.10 || p3 >= 0.10 {
		t.Fatalf("M3 超线: p=%.4f p3=%.4f", p, p3)
	}
}

func TestEnergyWindowMultiCycleAndEarlyClick(t *testing.T) {
	g, _ := NewEnergyWindow(bytes.Repeat([]byte{0x74}, 32))
	answer, err := EnergyWindowAnswerFrom(energyStream(t, "energy-cycle"), 1)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, energyStream(t, "energy-cycle"))
	if err != nil {
		t.Fatal(err)
	}
	target, _ := strconv.Atoi(answer)
	for _, ms := range []int{target + energyCycleMS, target + 2*energyCycleMS} {
		if ms > 60000 {
			continue
		}
		if !g.Verify(q.Verifier, strconv.Itoa(ms)) {
			t.Fatalf("跨周期 %dms 应通过", ms)
		}
	}
	if g.Verify(q.Verifier, strconv.Itoa(target+energyCycleMS+60)) {
		t.Fatal("同周期目标窗外应拒绝")
	}
	if g.Verify(q.Verifier, "100") {
		t.Fatal("<200ms 的零思考抢答应拒绝")
	}
}

func TestEnergyWindowDifficultyIsMonotonic(t *testing.T) {
	var last int
	for d := 1; d <= MaxDifficulties; d++ {
		tol, err := EnergyWindowTolerance(d)
		if err != nil {
			t.Fatal(err)
		}
		if d > 1 && tol >= last {
			t.Fatalf("d%d 容差 %d 未随难度收窄（上一档 %d）", d, tol, last)
		}
		last = tol
		p := float64(2*tol+1) / float64(energyFeasibleBuckets)
		p3 := 1 - math.Pow(1-p, 3)
		if p >= 0.10 || p3 >= 0.10 {
			t.Fatalf("d%d M3 超线: p=%.4f p3=%.4f", d, p, p3)
		}
		// 更保守的 3 次覆盖上界：三次点击各覆盖一个 13/11/… 桶的接受域。
		if tile := float64(3*(2*tol+1)) / float64(energyFeasibleBuckets); tile >= 0.10 {
			t.Fatalf("d%d 三次覆盖上界 %.4f 超线", d, tile)
		}
	}
}

func TestEnergyWindowSanitizeAndWrongAnswer(t *testing.T) {
	g, _ := NewEnergyWindow(bytes.Repeat([]byte{0x73}, 32))
	q, err := g.Generate(context.Background(), Params{Difficulty: 3}, energyStream(t, "energy-shape"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(q.Payload)
	if _, err := g.SanitizePublic(raw); err != nil {
		t.Fatalf("合法题面被拒: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["answer"] = "1200"
	bad, _ := json.Marshal(m)
	if _, err := g.SanitizePublic(bad); err == nil {
		t.Fatal("额外 answer 应拒绝")
	}
	_ = json.Unmarshal(raw, &m)
	m["frame_loop"] = false
	bad, _ = json.Marshal(m)
	if _, err := g.SanitizePublic(bad); err == nil {
		t.Fatal("frame_loop=false 应拒绝")
	}
	if g.Verify(q.Verifier, "abc") || g.Verify(q.Verifier, "60001") {
		t.Fatal("畸形/越界时间不应通过")
	}
}
