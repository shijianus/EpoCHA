package solver

import (
	"fmt"
	"image"
)

// progressive-target-v1 baseline：每轮一张 3×3 board，顶部样本给出当轮目标字形。
// 这与 symbol-path 共用同一套 8 个字形，只是样本字号 42、网格字号 34；按包围盒归一化后
// 逐格模板匹配，取最近的一格。三轮独立，客户端不判断对错，所以攻击器也不需要。

const progressiveScoreMargin = 0.005
const progressiveMaxDistance = 0.35

func solveProgressiveTarget(publicJSON []byte) Outcome {
	d, err := loadPublic(publicJSON)
	if err != nil {
		return parseFailure(err.Error())
	}
	vals, err := requireInt(d, "grid_cols", "grid_rows", "round_count", "round_y", "round_h",
		"sample_x", "sample_y", "sample_w", "sample_h", "grid_x", "grid_y", "grid_cell", "grid_gap")
	if err != nil {
		return parseFailure(err.Error())
	}
	cols, rows, rounds, roundY, roundH := vals[0], vals[1], vals[2], vals[3], vals[4]
	sampleX, sampleY, sampleW, sampleH := vals[5], vals[6], vals[7], vals[8]
	gridX, gridY, cell, gap := vals[9], vals[10], vals[11], vals[12]
	if cols < 1 || rows < 1 || cols*rows > 81 || rounds < 1 || rounds > 9 || cell < 8 {
		return parseFailure("渐进定位几何不合法")
	}
	answerCells := make([]int, rounds)
	for r := 0; r < rounds; r++ {
		baseY := roundY + r*roundH
		sampleRect := image.Rect(sampleX, baseY+sampleY, sampleX+sampleW, baseY+sampleY+sampleH)
		target, err := glyphTemplate(d.img, sampleRect, 1)
		if err != nil {
			return giveUp(fmt.Sprintf("第 %d 轮样本字形提取失败: %v", r+1, err))
		}
		best, second := -1, -1
		bestScore, secondScore := 0.0, 0.0
		for i := 0; i < cols*rows; i++ {
			cr, cc := i/cols, i%cols
			rect := image.Rect(
				gridX+cc*(cell+gap), baseY+gridY+cr*(cell+gap),
				gridX+cc*(cell+gap)+cell, baseY+gridY+cr*(cell+gap)+cell,
			)
			cellGlyph, err := glyphTemplate(d.img, rect, 1)
			if err != nil {
				return giveUp(fmt.Sprintf("第 %d 轮格子 %d 提取字形失败: %v", r+1, i, err))
			}
			score := inkDistance(target, cellGlyph)
			if best < 0 || score < bestScore {
				second, secondScore = best, bestScore
				best, bestScore = i, score
			} else if second < 0 || score < secondScore {
				second, secondScore = i, score
			}
		}
		if best < 0 {
			return giveUp(fmt.Sprintf("第 %d 轮没有可比较的格子", r+1))
		}
		if bestScore > progressiveMaxDistance {
			return giveUp(fmt.Sprintf("第 %d 轮最佳距离过高 %.3f", r+1, bestScore))
		}
		if second >= 0 && secondScore-bestScore < progressiveScoreMargin {
			return giveUp(fmt.Sprintf("第 %d 轮最佳与次佳过近 %.4f/%.4f", r+1, bestScore, secondScore))
		}
		answerCells[r] = best
	}
	out := "v1:"
	for i, c := range answerCells {
		if i > 0 {
			out += "."
		}
		out += fmt.Sprintf("%d", c)
	}
	return answer(out)
}
