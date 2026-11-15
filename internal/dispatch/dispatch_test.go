package dispatch

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func testCandidate(id string, w float64, accessible, motion bool) Candidate {
	return Candidate{
		ID: id, BaseWeight: w, Accessible: accessible, Motion: motion,
		RiskFit:   map[Band]float64{BandLow: 1, BandMedium: 1, BandHigh: 1, BandCritical: 1},
		DeviceFit: map[Device]float64{DeviceDesktop: 1, DeviceTouch: 1, DeviceKeyboard: 1},
	}
}

func TestBandForScore(t *testing.T) {
	cases := []struct {
		score int
		want  Band
	}{{0, BandLow}, {24, BandLow}, {25, BandMedium}, {49, BandMedium}, {50, BandHigh}, {74, BandHigh}, {75, BandCritical}, {100, BandCritical}}
	for _, tc := range cases {
		if got := BandForScore(tc.score); got != tc.want {
			t.Fatalf("BandForScore(%d)=%s，期望 %s", tc.score, got, tc.want)
		}
	}
}

func TestWeightFilters(t *testing.T) {
	c := testCandidate("a", 1, false, true)
	if Weight(c, Request{Band: BandLow, Device: DeviceDesktop}) == 0 {
		t.Fatal("普通候选不应被排除")
	}
	if Weight(c, Request{Band: BandLow, Device: DeviceDesktop, Accessibility: true}) != 0 {
		t.Fatal("无障碍请求不应选非 accessible 候选")
	}
	if Weight(c, Request{Band: BandLow, Device: DeviceDesktop, NoMotion: true}) != 0 {
		t.Fatal("NoMotion 应排除运动题")
	}
	energy := Catalog()[generator.EnergyWindowID]
	if Weight(energy, Request{Band: BandMedium, Device: DeviceDesktop, NoMotion: true}) != 0 {
		t.Fatal("reduced_motion 应排除 energy-window")
	}
	pipe := Catalog()[generator.PipeConnectID]
	if Weight(pipe, Request{Band: BandMedium, Device: DeviceDesktop}) == 0 {
		t.Fatal("pipe-connect 应有正权重")
	}
	intersection := Catalog()[generator.IntersectionPointID]
	if Weight(intersection, Request{Band: BandMedium, Device: DeviceDesktop}) == 0 {
		t.Fatal("intersection-point 应有正权重")
	}
	if Weight(intersection, Request{Band: BandMedium, Device: DeviceKeyboard}) == 0 {
		t.Fatal("intersection-point 的键盘输入应可用（方向键微调）")
	}
	bad := testCandidate("b", 0, false, false)
	if Weight(bad, Request{Band: BandLow, Device: DeviceDesktop}) != 0 {
		t.Fatal("零基础权重应排除")
	}
	unknownFit := testCandidate("c", 1, false, false)
	delete(unknownFit.RiskFit, BandHigh)
	if Weight(unknownFit, Request{Band: BandHigh, Device: DeviceDesktop}) != 0 {
		t.Fatal("缺少风险带权重应排除")
	}
}

func TestPickDeterministicAndBoundaries(t *testing.T) {
	cands := []Candidate{testCandidate("a", 1, false, false), testCandidate("b", 1, false, false)}
	s := NewSchedulerWithIntn(func(n int) (int, error) {
		if n <= 0 {
			t.Fatalf("intn n=%d", n)
		}
		return 0, nil
	})
	got, err := s.Pick(cands, Request{Band: BandLow, Device: DeviceDesktop})
	if err != nil || got.ID != "a" {
		t.Fatalf("first pick = %+v, %v", got, err)
	}
	last := NewSchedulerWithIntn(func(n int) (int, error) { return n - 1, nil })
	got, err = last.Pick(cands, Request{Band: BandLow, Device: DeviceDesktop})
	if err != nil || got.ID != "b" {
		t.Fatalf("last pick = %+v, %v", got, err)
	}
	if _, err := s.Pick([]Candidate{testCandidate("a", 1, false, false)}, Request{Band: BandLow, Device: DeviceDesktop, Accessibility: true}); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("无障碍无候选应 ErrNoCandidate, got %v", err)
	}
	bad := NewSchedulerWithIntn(func(int) (int, error) { return -1, nil })
	if _, err := bad.Pick(cands, Request{Band: BandLow, Device: DeviceDesktop}); err == nil {
		t.Fatal("越界 intn 应拒绝")
	}
}

func TestPickDistribution(t *testing.T) {
	cands := []Candidate{testCandidate("a", 3, false, false), testCandidate("b", 1, false, false)}
	rng := rand.New(rand.NewSource(9))
	s := NewSchedulerWithIntn(func(n int) (int, error) { return rng.Intn(n), nil })
	counts := map[string]int{}
	for i := 0; i < 20000; i++ {
		c, err := s.Pick(cands, Request{Band: BandLow, Device: DeviceDesktop})
		if err != nil {
			t.Fatal(err)
		}
		counts[c.ID]++
	}
	// 3:1 权重；允许统计波动，但必须明显偏向 a。
	if counts["a"] < 13500 || counts["a"] > 16500 {
		t.Fatalf("a 分布 = %d / 20000，期望约 15000", counts["a"])
	}
	if counts["b"] == 0 {
		t.Fatal("b 不应被完全饿死")
	}
}

func TestCatalogAndFallback(t *testing.T) {
	cat := Catalog()
	for _, id := range []string{
		generator.ArithID, generator.TextImageID, generator.TextImageV2ID, generator.TextImageV3ID,
		generator.ShapeGridID, generator.ShapeCountID, generator.ParallaxID, generator.BankShotID,
		generator.AccessibleChoiceID, generator.EnergyWindowID, generator.PipeConnectID,
		generator.IntersectionPointID, generator.TileRotationID, generator.SymbolPathID,
		generator.CrowdFindID, generator.VisualRankID,
		generator.ProgressiveTargetID, generator.ShellTrackID, generator.SilhouetteAssemblyID,
	} {
		if _, ok := cat[id]; !ok {
			t.Fatalf("Catalog 缺 %s", id)
		}
	}
	f := FallbackCandidate("unknown-v1")
	if f.ID != "unknown-v1" || Weight(f, Request{Band: BandCritical, Device: DeviceTouch}) == 0 {
		t.Fatalf("Fallback 权重不合法: %+v", f)
	}
}

func TestNewInteractiveTypesStayLowRisk(t *testing.T) {
	cat := Catalog()
	touchExcluded := map[string]bool{
		generator.SymbolPathID: true,
		generator.CrowdFindID:  true,
	}
	for _, id := range []string{
		generator.TileRotationID, generator.SymbolPathID, generator.CrowdFindID,
		generator.VisualRankID, generator.ProgressiveTargetID,
		generator.ShellTrackID, generator.SilhouetteAssemblyID,
	} {
		c, ok := cat[id]
		if !ok {
			t.Fatalf("Catalog 缺 %s", id)
		}
		if Weight(c, Request{Band: BandLow, Device: DeviceDesktop}) == 0 {
			t.Fatalf("%s 在 desktop low 应可用", id)
		}
		touchWeight := Weight(c, Request{Band: BandMedium, Device: DeviceTouch})
		if touchExcluded[id] {
			if touchWeight != 0 {
				t.Fatalf("%s 在移动变体完成前必须排除 touch", id)
			}
		} else if touchWeight == 0 {
			t.Fatalf("%s 在 touch medium 应可用", id)
		}
		if Weight(c, Request{Band: BandHigh, Device: DeviceDesktop}) != 0 ||
			Weight(c, Request{Band: BandCritical, Device: DeviceDesktop}) != 0 {
			t.Fatalf("%s 不应进入 high/critical（实验题型）", id)
		}
	}
	if !cat[generator.ShellTrackID].Motion {
		t.Fatal("shell-track 必须标记 Motion")
	}
	if Weight(cat[generator.ShellTrackID], Request{Band: BandLow, Device: DeviceDesktop, NoMotion: true}) != 0 {
		t.Fatal("prefers-reduced-motion 必须排除 shell-track")
	}
	if cat[generator.ProgressiveTargetID].Motion || cat[generator.SilhouetteAssemblyID].Motion {
		t.Fatal("静态题型不应标记 Motion")
	}
}
