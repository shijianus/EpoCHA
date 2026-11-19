package solver

import (
	"fmt"
	"image"
)

// glyphNormDim 是字形密度图的边长。8 个符号字形由粗笔画构成（3–6px），
// 24×24 足以区分它们，又不会被目标条与网格 2px 的字号差放大成噪声。
const glyphNormDim = 24

// glyphInkThreshold 把字形笔迹（灰度 26–30）从背景（246–252）和格子边框（170–200）分开。
const glyphInkThreshold = 160

type targetCard struct {
	rect    image.Rectangle
	clipped bool
}

// glyphTemplate 从公开图片的一个格子/样本卡里提取字形密度图。
// inset 用来避开卡片边框；字形本身的紧包围盒由 normalizedInk 重新计算。
func glyphTemplate(g *image.Gray, rect image.Rectangle, inset int) ([]float64, error) {
	r := rect.Inset(inset)
	if r.Empty() {
		return nil, fmt.Errorf("字形裁剪为空")
	}
	return normalizedInk(g, r, glyphInkThreshold, glyphNormDim, glyphNormDim)
}

// matchGlyphs 返回每个单元格对每个模板的距离矩阵：dist[cell][template]。
func matchGlyphs(cells [][]float64, templates [][]float64) [][]float64 {
	dist := make([][]float64, len(cells))
	for i, c := range cells {
		dist[i] = make([]float64, len(templates))
		for j, t := range templates {
			dist[i][j] = inkDistance(c, t)
		}
	}
	return dist
}

// targetStripCards 从公开图片里定位目标条上的 L 个样本卡。
//
// 关键事实（公开可见的渲染几何）：target_len=8 时卡片总宽 8*40+7*4=348 > 图片宽 320，
// 两端样本卡会被图片边界裁掉。payload 没有样本卡宽度/间距，所以这里从列内的非背景
// 像素推宽度与间距；边缘的窄 run 标记为 clipped，调用方用"目标序列是 8 个字形的一个
// 排列"来补缺失符号，而不是假装那半张卡可读。
func targetStripCards(g *image.Gray, targetY, targetH, targetLen int) ([]targetCard, bool) {
	if targetLen < 1 || targetH < 8 {
		return nil, false
	}
	y0 := targetY + 2
	y1 := targetY + targetH - 2
	if y0 < g.Bounds().Min.Y {
		y0 = g.Bounds().Min.Y
	}
	if y1 > g.Bounds().Max.Y {
		y1 = g.Bounds().Max.Y
	}
	if y1 <= y0 {
		return nil, false
	}
	counts := make([]int, g.Bounds().Dx())
	for x := g.Bounds().Min.X; x < g.Bounds().Max.X; x++ {
		n := 0
		for y := y0; y < y1; y++ {
			v := g.GrayAt(x, y).Y
			if v > 244 || v < 220 {
				n++
			}
		}
		counts[x-g.Bounds().Min.X] = n
	}
	threshold := (y1 - y0) / 4
	if threshold < 3 {
		threshold = 3
	}
	type run struct{ lo, hi int }
	var runs []run
	inRun := false
	for i, n := range counts {
		if n >= threshold {
			if !inRun {
				runs = append(runs, run{lo: i, hi: i})
				inRun = true
			} else {
				runs[len(runs)-1].hi = i
			}
		} else {
			inRun = false
		}
	}
	var merged []run
	for _, r := range runs {
		if len(merged) > 0 && r.lo-merged[len(merged)-1].hi <= 1 {
			merged[len(merged)-1].hi = r.hi
		} else {
			merged = append(merged, r)
		}
	}
	if len(merged) == 0 {
		return nil, false
	}
	widths := make([]int, len(merged))
	maxW := 0
	for i, r := range merged {
		widths[i] = r.hi - r.lo + 1
		if widths[i] > maxW {
			maxW = widths[i]
		}
	}
	if maxW < 24 {
		return nil, false
	}
	// 间距取内部相邻 run 的空隙中位数；只有一张卡时无法推断，按 0 处理。
	var gaps []int
	for i := 1; i < len(merged); i++ {
		gaps = append(gaps, merged[i].lo-merged[i-1].hi-1)
	}
	gap := 0
	if len(gaps) > 0 {
		for _, g := range gaps {
			gap += g
		}
		gap /= len(gaps)
		if gap < 0 {
			gap = 0
		}
	}
	if len(merged) != targetLen {
		return nil, false
	}
	// 第一张卡若是窄 run 且贴着左边界，说明它被裁切；反推完整卡片的起点。
	x0 := merged[0].lo
	if merged[0].lo == 0 && widths[0] < maxW-2 {
		x0 = merged[0].hi + 1 - maxW
	}
	cards := make([]targetCard, targetLen)
	for i := 0; i < targetLen; i++ {
		x := x0 + i*(maxW+gap)
		r := image.Rect(x, targetY, x+maxW, targetY+targetH)
		clipped := r.Min.X < g.Bounds().Min.X || r.Max.X > g.Bounds().Max.X
		cards[i] = targetCard{rect: r, clipped: clipped}
	}
	// 对内部完整卡做一次对齐核对：run 的左右边界必须与重建的卡片在 2px 内。
	for i := 1; i < targetLen-1; i++ {
		if cards[i].clipped {
			continue
		}
		if absInt(merged[i].lo-cards[i].rect.Min.X) > 2 ||
			absInt(merged[i].hi+1-cards[i].rect.Max.X) > 2 {
			return nil, false
		}
	}
	return cards, true
}
