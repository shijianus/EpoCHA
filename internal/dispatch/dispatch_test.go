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
		generator.AccessibleChoiceID,
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
