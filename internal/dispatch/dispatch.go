package dispatch

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/shijianus/EpoCHA/internal/generator"
)

// Package dispatch 是风险带/设备画像的纯选型器（ADR-026）。
//
// 它只做"从候选里按权重抽一个"，不读库、不持有状态、不做判定；候选与权重由调用方给出。
// 这样调度逻辑可以单测，而 API 仍负责把选中的 ID 绑定进 challenge。

type Band string

const (
	BandLow      Band = "low"
	BandMedium   Band = "medium"
	BandHigh     Band = "high"
	BandCritical Band = "critical"
)

type Device string

const (
	DeviceDesktop  Device = "desktop"
	DeviceTouch    Device = "touch"
	DeviceKeyboard Device = "keyboard"
)

var (
	ErrNoCandidate = errors.New("dispatch: 没有符合条件的题型")
	ErrBadConfig   = errors.New("dispatch: 调度配置不合法")
)

// Candidate 是调度器可见的题型画像。RiskFit/DeviceFit 为 0 表示该组合不可用。
type Candidate struct {
	ID         string
	Family     string
	Accessible bool
	Motion     bool
	BaseWeight float64
	RiskFit    map[Band]float64
	DeviceFit  map[Device]float64
}

// Request 是本次签发的过滤条件。设备是客户端声明，只用于可用性，不是信任信号。
type Request struct {
	Band          Band
	Device        Device
	Accessibility bool
	NoMotion      bool
}

// Scheduler 使用可注入的 intn 便于确定性测试；默认是 crypto/rand。
type Scheduler struct {
	intn func(n int) (int, error)
}

func NewScheduler() *Scheduler {
	return &Scheduler{intn: cryptoIntn}
}

func NewSchedulerWithIntn(fn func(int) (int, error)) *Scheduler {
	if fn == nil {
		fn = cryptoIntn
	}
	return &Scheduler{intn: fn}
}

func cryptoIntn(n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("%w: n=%d", ErrBadConfig, n)
	}
	v, err := crand.Int(crand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

// BandForScore 使用 ADR-026 的四档边界。
func BandForScore(score int) Band {
	switch {
	case score < 25:
		return BandLow
	case score < 50:
		return BandMedium
	case score < 75:
		return BandHigh
	default:
		return BandCritical
	}
}

// Weight 返回候选在当前请求下的权重；0 表示被硬约束排除。
func Weight(c Candidate, req Request) float64 {
	// 无障碍候选只能由显式 accessibility 请求选中；普通请求也不得误选它。
	if c.Accessible != req.Accessibility {
		return 0
	}
	if req.NoMotion && c.Motion {
		return 0
	}
	rf, ok := c.RiskFit[req.Band]
	if !ok {
		return 0
	}
	df, ok := c.DeviceFit[req.Device]
	if !ok {
		return 0
	}
	w := c.BaseWeight * rf * df
	if math.IsNaN(w) || math.IsInf(w, 0) || w <= 0 {
		return 0
	}
	return w
}

// Pick 按归一化权重抽取一个候选。所有候选权重为 0 时返回 ErrNoCandidate；
// 不提供"随便挑一个"的回落（那会把风控策略静默降级）。
func (s *Scheduler) Pick(cands []Candidate, req Request) (Candidate, error) {
	if s == nil || s.intn == nil {
		return Candidate{}, fmt.Errorf("%w: nil scheduler", ErrBadConfig)
	}
	type weighted struct {
		c Candidate
		w int
	}
	items := make([]weighted, 0, len(cands))
	total := 0
	for _, c := range cands {
		w := Weight(c, req)
		if w <= 0 {
			continue
		}
		// 量化到 1..N 的整数权重；极小但非零的候选仍保留 1，避免被浮点截断成"永不选中"。
		n := int(math.Round(w * 1000))
		if n < 1 {
			n = 1
		}
		items = append(items, weighted{c: c, w: n})
		total += n
	}
	if len(items) == 0 || total <= 0 {
		return Candidate{}, ErrNoCandidate
	}
	draw, err := s.intn(total)
	if err != nil {
		return Candidate{}, fmt.Errorf("dispatch: 随机抽样: %w", err)
	}
	if draw < 0 || draw >= total {
		return Candidate{}, fmt.Errorf("%w: intn=%d total=%d", ErrBadConfig, draw, total)
	}
	for _, it := range items {
		if draw < it.w {
			return it.c, nil
		}
		draw -= it.w
	}
	return Candidate{}, ErrNoCandidate
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

// FallbackCandidate 给"已注册但未在 Catalog 登记"的题型一个中性权重；
// 它不推断题型性质，访问性/运动能力都按 false 处理（保守）。
func FallbackCandidate(id string) Candidate {
	return Candidate{
		ID: id, BaseWeight: 1,
		RiskFit:   map[Band]float64{BandLow: 1, BandMedium: 1, BandHigh: 1, BandCritical: 1},
		DeviceFit: map[Device]float64{DeviceDesktop: 1, DeviceTouch: 1, DeviceKeyboard: 1},
	}
}

// Catalog 是 Phase 3 的静态画像表。未知 ID 由 API 以保守权重兜底，不在本包静默造数据。
func Catalog() map[string]Candidate {
	fit := func(pairs ...any) map[Band]float64 {
		m := map[Band]float64{}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[pairs[i].(Band)] = asFloat(pairs[i+1])
		}
		return m
	}
	dev := func(pairs ...any) map[Device]float64 {
		m := map[Device]float64{}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[pairs[i].(Device)] = asFloat(pairs[i+1])
		}
		return m
	}
	return map[string]Candidate{
		generator.ArithID: {
			ID: generator.ArithID, Family: "baseline", BaseWeight: 0.2,
			RiskFit:   fit(BandLow, 0.5, BandMedium, 0.2, BandHigh, 0.05, BandCritical, 0),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 1),
		},
		generator.TextImageID: {
			ID: generator.TextImageID, Family: "visual-text", BaseWeight: 1,
			RiskFit:   fit(BandLow, 0.8, BandMedium, 1.2, BandHigh, 0.7, BandCritical, 0.3),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 0.2),
		},
		generator.TextImageV2ID: {
			ID: generator.TextImageV2ID, Family: "visual-text", BaseWeight: 1,
			RiskFit:   fit(BandLow, 0.7, BandMedium, 1.2, BandHigh, 0.9, BandCritical, 0.4),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 0.2),
		},
		generator.TextImageV3ID: {
			ID: generator.TextImageV3ID, Family: "visual-text", BaseWeight: 1,
			RiskFit:   fit(BandLow, 0.6, BandMedium, 1.2, BandHigh, 1, BandCritical, 0.5),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 0.2),
		},
		generator.ShapeGridID: {
			ID: generator.ShapeGridID, Family: "shape", BaseWeight: 1,
			RiskFit:   fit(BandLow, 1.2, BandMedium, 1, BandHigh, 0.4, BandCritical, 0.1),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1.2, DeviceKeyboard, 0.8),
		},
		generator.ShapeCountID: {
			ID: generator.ShapeCountID, Family: "shape", BaseWeight: 1,
			RiskFit:   fit(BandLow, 1.2, BandMedium, 1, BandHigh, 0.4, BandCritical, 0.1),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1.2, DeviceKeyboard, 0.8),
		},
		generator.ParallaxID: {
			ID: generator.ParallaxID, Family: "temporal", BaseWeight: 0.9, Motion: true,
			RiskFit:   fit(BandLow, 0.2, BandMedium, 1, BandHigh, 1.5, BandCritical, 1.5),
			DeviceFit: dev(DeviceDesktop, 1.2, DeviceTouch, 0.7, DeviceKeyboard, 0),
		},
		generator.BankShotID: {
			ID: generator.BankShotID, Family: "physics", BaseWeight: 1,
			RiskFit:   fit(BandLow, 0.4, BandMedium, 1.2, BandHigh, 1.5, BandCritical, 1.2),
			DeviceFit: dev(DeviceDesktop, 1.2, DeviceTouch, 1, DeviceKeyboard, 0.5),
		},
		generator.EnergyWindowID: {
			ID: generator.EnergyWindowID, Family: "timing", BaseWeight: 0.9, Motion: true,
			RiskFit:   fit(BandLow, 0.6, BandMedium, 1.2, BandHigh, 1.4, BandCritical, 1),
			DeviceFit: dev(DeviceDesktop, 1.2, DeviceTouch, 0.9, DeviceKeyboard, 0.2),
		},
		generator.PipeConnectID: {
			ID: generator.PipeConnectID, Family: "spatial-topology", BaseWeight: 1.1,
			RiskFit:   fit(BandLow, 1.2, BandMedium, 1.1, BandHigh, 0.7, BandCritical, 0.2),
			DeviceFit: dev(DeviceDesktop, 1.1, DeviceTouch, 1.1, DeviceKeyboard, 0.6),
		},
		generator.IntersectionPointID: {
			ID: generator.IntersectionPointID, Family: "geometry", BaseWeight: 1,
			RiskFit:   fit(BandLow, 1.1, BandMedium, 1.2, BandHigh, 0.9, BandCritical, 0.3),
			DeviceFit: dev(DeviceDesktop, 1.2, DeviceTouch, 0.9, DeviceKeyboard, 0.6),
		},
		generator.AccessibleChoiceID: {
			ID: generator.AccessibleChoiceID, Family: "accessible", BaseWeight: 1, Accessible: true,
			RiskFit:   fit(BandLow, 1, BandMedium, 0.6, BandHigh, 0, BandCritical, 0),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 1),
		},
		// 2026-11-12 新题型：BaseWeight 刻意很低，且没有高风险档的 RiskFit。
		// 它们不会进入任何现有站点（站点必须显式配置 generator_id/generator_pool）；
		// 这里非零只是让"运营者显式选择后"能领到题，不是生产可用声明。
		generator.TileRotationID: {
			ID: generator.TileRotationID, Family: "spatial-topology", BaseWeight: 0.2,
			RiskFit:   fit(BandLow, 0.4, BandMedium, 0.3, BandHigh, 0, BandCritical, 0),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 1),
		},
		generator.SymbolPathID: {
			ID: generator.SymbolPathID, Family: "spatial-path", BaseWeight: 0.2,
			RiskFit:   fit(BandLow, 0.4, BandMedium, 0.3, BandHigh, 0, BandCritical, 0),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 1),
		},
		generator.CrowdFindID: {
			ID: generator.CrowdFindID, Family: "visual-search", BaseWeight: 0.2,
			RiskFit:   fit(BandLow, 0.4, BandMedium, 0.3, BandHigh, 0, BandCritical, 0),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 0.8),
		},
		generator.VisualRankID: {
			ID: generator.VisualRankID, Family: "visual-order", BaseWeight: 0.2,
			RiskFit:   fit(BandLow, 0.4, BandMedium, 0.3, BandHigh, 0, BandCritical, 0),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 1),
		},
		generator.ProgressiveTargetID: {
			ID: generator.ProgressiveTargetID, Family: "visual-search", BaseWeight: 0.2,
			RiskFit:   fit(BandLow, 0.4, BandMedium, 0.3, BandHigh, 0, BandCritical, 0),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 1),
		},
		generator.ShellTrackID: {
			ID: generator.ShellTrackID, Family: "motion-tracking", BaseWeight: 0.15, Motion: true,
			RiskFit:   fit(BandLow, 0.3, BandMedium, 0.2, BandHigh, 0, BandCritical, 0),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 1),
		},
		generator.SilhouetteAssemblyID: {
			ID: generator.SilhouetteAssemblyID, Family: "spatial-assembly", BaseWeight: 0.2,
			RiskFit:   fit(BandLow, 0.4, BandMedium, 0.3, BandHigh, 0, BandCritical, 0),
			DeviceFit: dev(DeviceDesktop, 1, DeviceTouch, 1, DeviceKeyboard, 1),
		},
	}
}
