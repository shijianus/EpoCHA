package solver

import (
	"fmt"
	"image"
	"math"
)

// symbol-path-v1 baseline：目标条与 6×6 网格都用同一套 8 个字形绘制。
//
// 网格里的同一字形是**逐像素相同**的（同字号、整格对齐），所以先把 36 个格子按
// 24×24 密度图聚类（相同字形距离≈0），得到 6–8 个字形类；目标条上的样本卡按密度
// 匹配到这些类。目标条在 target_len=8 时总宽 348 > 320，两端卡片被图片边界裁掉；
// 攻击器不假装半张卡可读，而是利用"目标序列是 8 个字形的一个排列"把未出现的类补到
// 被裁切的位置，再在网格上做类序列 DFS。
//
// 这不是"识别得多聪明"，而是题目本身把目标序列和同一批字形都公开画在图上。

const (
	symbolClusterThreshold = 0.002
	symbolTargetMaxDist    = 0.35
	symbolSecondMargin     = 0.05
	symbolComboCap         = 4096
)

type visibleTarget struct {
	pos      int
	best     int
	second   int
	bestDist float64
	secDist  float64
}

func solveSymbolPath(publicJSON []byte) Outcome {
	d, err := loadPublic(publicJSON)
	if err != nil {
		return parseFailure(err.Error())
	}
	vals, err := requireInt(d, "grid_cols", "grid_rows", "grid_x", "grid_y",
		"grid_cell", "grid_gap", "target_len", "target_y", "target_h")
	if err != nil {
		return parseFailure(err.Error())
	}
	cols, rows, gridX, gridY := vals[0], vals[1], vals[2], vals[3]
	cell, gap, targetLen := vals[4], vals[5], vals[6]
	targetY, targetH := vals[7], vals[8]
	if cols < 2 || rows < 2 || cols*rows > 100 || cell < 8 || targetLen < 2 || targetLen > 9 {
		return parseFailure("网格/目标条几何不合法")
	}
	cards, ok := targetStripCards(d.img, targetY, targetH, targetLen)
	if !ok {
		return giveUp("无法从公开图片定位目标条上的样本卡")
	}
	// 网格字形聚类。
	cellTemplates := make([][]float64, cols*rows)
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			rect := image.Rect(
				gridX+c*(cell+gap), gridY+r*(cell+gap),
				gridX+c*(cell+gap)+cell, gridY+r*(cell+gap)+cell,
			)
			t, err := glyphTemplate(d.img, rect, 1)
			if err != nil {
				return giveUp(fmt.Sprintf("网格单元 %d 提取字形失败: %v", r*cols+c, err))
			}
			cellTemplates[r*cols+c] = t
		}
	}
	protos, labels, ok := clusterGlyphs(cellTemplates)
	if !ok || len(protos) < targetLen {
		return giveUp(fmt.Sprintf("网格字形聚类失败（%d 类，目标 %d 个）", len(protos), targetLen))
	}
	// 目标条可见卡匹配到类。
	visible := make([]visibleTarget, 0, targetLen)
	clippedPos := make([]int, 0, 2)
	for pos, card := range cards {
		if card.clipped {
			clippedPos = append(clippedPos, pos)
			continue
		}
		t, err := glyphTemplate(d.img, card.rect, 1)
		if err != nil {
			return giveUp(fmt.Sprintf("目标卡 %d 提取字形失败: %v", pos, err))
		}
		best, second := -1, -1
		bestDist, secDist := math.Inf(1), math.Inf(1)
		for i, p := range protos {
			dist := inkDistance(t, p)
			if dist < bestDist {
				second, secDist = best, bestDist
				best, bestDist = i, dist
			} else if dist < secDist {
				second, secDist = i, dist
			}
		}
		if best < 0 || bestDist > symbolTargetMaxDist {
			return giveUp(fmt.Sprintf("目标卡 %d 无可靠字形匹配（%.3f）", pos, bestDist))
		}
		visible = append(visible, visibleTarget{
			pos: pos, best: best, second: second, bestDist: bestDist, secDist: secDist,
		})
	}
	// 枚举可见卡的候选（best，必要时 best+second），补足被裁切位置。
	seenSolutions := map[string]bool{}
	var solutions [][]int
	combos := 0
	var assign func(i int, used map[int]bool, seq []int)
	assign = func(i int, used map[int]bool, seq []int) {
		if len(solutions) > 1 || combos > symbolComboCap {
			return
		}
		if i == len(visible) {
			combos++
			missing := make([]int, 0, len(protos))
			for c := 0; c < len(protos); c++ {
				if !used[c] {
					missing = append(missing, c)
				}
			}
			if len(clippedPos) == 0 {
				// 没有裁切：可见卡已经覆盖全部目标位置；网格里多出来的字形类
				// 是 distractor，不影响序列。
				if !distinctLabels(seq) {
					return
				}
				addSymbolSolutions(&solutions, seenSolutions, countSymbolPaths(labels, cols, rows, seq, 2))
				return
			}
			if len(missing) != len(clippedPos) {
				return
			}
			permuteInts(missing, 0, func(order []int) {
				if len(solutions) > 1 {
					return
				}
				full := append([]int(nil), seq...)
				for k, pos := range clippedPos {
					full[pos] = order[k]
				}
				if !distinctLabels(full) {
					return
				}
				addSymbolSolutions(&solutions, seenSolutions, countSymbolPaths(labels, cols, rows, full, 2))
			})
			return
		}
		v := visible[i]
		try := func(cluster int) {
			if cluster < 0 || used[cluster] {
				return
			}
			used[cluster] = true
			seq[v.pos] = cluster
			assign(i+1, used, seq)
			delete(used, cluster)
		}
		try(v.best)
		if v.second >= 0 && v.secDist <= v.bestDist+symbolSecondMargin {
			try(v.second)
		}
	}
	seq := make([]int, targetLen)
	assign(0, map[int]bool{}, seq)
	if combos > symbolComboCap {
		return giveUp("符号路径候选组合达到上限")
	}
	if len(solutions) == 0 {
		return giveUp("没有与目标序列匹配的路径")
	}
	if len(solutions) > 1 {
		return giveUp("匹配路径不唯一")
	}
	return answer(pathAnswer(solutions[0]))
}

// clusterGlyphs 按密度距离贪心聚类。网格里同一字形的绘制逐像素相同，
// 因此 0.002 的阈值只会把不同字形分开，不会把同字形拆开。
func clusterGlyphs(cells [][]float64) ([][]float64, []int, bool) {
	if len(cells) == 0 {
		return nil, nil, false
	}
	var protos [][]float64
	var sums [][]float64
	var counts []int
	labels := make([]int, len(cells))
	for i, c := range cells {
		best, bestDist := -1, math.Inf(1)
		for j, p := range protos {
			d := inkDistance(c, p)
			if d < bestDist {
				best, bestDist = j, d
			}
		}
		if best >= 0 && bestDist <= symbolClusterThreshold {
			labels[i] = best
			counts[best]++
			for k := range sums[best] {
				sums[best][k] += c[k]
			}
			for k := range protos[best] {
				protos[best][k] = sums[best][k] / float64(counts[best])
			}
			continue
		}
		labels[i] = len(protos)
		protos = append(protos, append([]float64(nil), c...))
		sums = append(sums, append([]float64(nil), c...))
		counts = append(counts, 1)
	}
	if len(protos) > 8 {
		return nil, nil, false
	}
	return protos, labels, true
}

func distinctLabels(seq []int) bool {
	seen := map[int]bool{}
	for _, v := range seq {
		if seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}

func addSymbolSolutions(dst *[][]int, seen map[string]bool, paths [][]int) {
	for _, p := range paths {
		key := fmt.Sprintf("%v", p)
		if seen[key] {
			continue
		}
		seen[key] = true
		*dst = append(*dst, p)
	}
}

func countSymbolPaths(labels []int, cols, rows int, seq []int, cap int) [][]int {
	var out [][]int
	used := make([]bool, len(labels))
	path := make([]int, 0, len(seq))
	var walk func(cur, depth int)
	walk = func(cur, depth int) {
		if len(out) >= cap {
			return
		}
		if depth == len(seq) {
			out = append(out, append([]int(nil), path...))
			return
		}
		r, c := cur/cols, cur%cols
		for _, dxy := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			nr, nc := r+dxy[0], c+dxy[1]
			if nr < 0 || nr >= rows || nc < 0 || nc >= cols {
				continue
			}
			ni := nr*cols + nc
			if used[ni] || labels[ni] != seq[depth] {
				continue
			}
			used[ni] = true
			path = append(path, ni)
			walk(ni, depth+1)
			path = path[:len(path)-1]
			used[ni] = false
		}
	}
	for start := 0; start < len(labels) && len(out) < cap; start++ {
		if labels[start] != seq[0] {
			continue
		}
		used[start] = true
		path = append(path, start)
		walk(start, 1)
		path = path[:len(path)-1]
		used[start] = false
	}
	return out
}

func permuteInts(a []int, i int, f func([]int)) {
	if i == len(a) {
		f(a)
		return
	}
	for j := i; j < len(a); j++ {
		a[i], a[j] = a[j], a[i]
		permuteInts(a, i+1, f)
		a[i], a[j] = a[j], a[i]
	}
}

func pathAnswer(cells []int) string {
	out := "v1:"
	for i, c := range cells {
		if i > 0 {
			out += "."
		}
		out += fmt.Sprintf("%02d", c)
	}
	return out
}
