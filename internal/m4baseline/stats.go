package m4baseline

import (
	"math"
	"sort"
)

// percentile 使用线性插值（与常见监控口径一致）；values 必须已排序或在内部排序副本。
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if p <= 0 {
		return values[0]
	}
	if p >= 100 {
		return values[len(values)-1]
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func median(values []float64) float64 { return percentile(values, 50) }

// wilson95 返回二项比例的 Wilson 95% 置信区间。n=0 时返回 (0,1)，表示"没有信息"，
// 不是"0% 成功"。
func wilson95(success, n int) (float64, float64) {
	if n <= 0 {
		return 0, 1
	}
	const z = 1.959963984540054
	p := float64(success) / float64(n)
	denom := 1 + z*z/float64(n)
	center := (p + z*z/(2*float64(n))) / denom
	half := z * math.Sqrt(p*(1-p)/float64(n)+z*z/(4*float64(n)*float64(n))) / denom
	lo, hi := center-half, center+half
	if lo < 0 {
		lo = 0
	}
	if hi > 1 {
		hi = 1
	}
	return lo, hi
}

// randomGuessThree 是"三次独立尝试至少中一次"的概率。生产只有一次作答机会，
// 这里同时给单次与三次，是为了对齐 DESIGN §8.4 的 M3 口径。
func randomGuessThree(p float64) float64 {
	if p <= 0 {
		return 0
	}
	if p >= 1 {
		return 1
	}
	return 1 - math.Pow(1-p, 3)
}
