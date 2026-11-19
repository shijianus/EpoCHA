package solver

import (
	"fmt"
	"image"
)

// shell-track-v1 baseline：payload 只给 33 帧雪碧图与 4 个容器点击区，不给交换序列。
// 攻击器做两件事：
//   1. 每轮第 0 帧在容器上方找标记（十字），标记的 x 给出目标初始容器；
//   2. 每次交换的"运动帧"与上一张"静止帧"做逐列差分。两个源容器被搬走，
//      差分列的左右边界正好是这两个容器槽位的外沿；由中心与宽度唯一确定交换对。
// 然后只按"目标在当前容器 ⇒ 换到另一格"更新位置，不需要识别哪个杯子装了什么。
//
// 这不是使用 payload 里不存在的私有交换序列：所有信息都来自公开帧像素。

type shellBox struct {
	cx int
	w  int
}

func solveShellTrack(publicJSON []byte) Outcome {
	d, err := loadPublic(publicJSON)
	if err != nil {
		return parseFailure(err.Error())
	}
	boxesRaw, ok := docObjects(d.raw, "boxes")
	if !ok || len(boxesRaw) < 2 {
		return parseFailure("boxes 缺失或数量不足")
	}
	boxes := make([]shellBox, len(boxesRaw))
	for i, obj := range boxesRaw {
		x, okX := docInt(obj, "x")
		_, okY := docInt(obj, "y")
		w, okW := docInt(obj, "w")
		h, okH := docInt(obj, "h")
		if !okX || !okY || !okW || !okH || w < 8 || h < 8 {
			return parseFailure(fmt.Sprintf("容器 %d 几何不合法", i))
		}
		boxes[i] = shellBox{cx: x + w/2, w: w}
	}
	frameW, okW := docInt(d.raw, "image_width")
	frameH, okH := docInt(d.raw, "image_height")
	frameCols, okC := docInt(d.raw, "frame_cols")
	frameRows, okR := docInt(d.raw, "frame_rows")
	frameCount, okFC := docInt(d.raw, "frame_count")
	framesPerRound, okFP := docInt(d.raw, "frames_per_round")
	rounds, okRo := docInt(d.raw, "rounds")
	if !okW || !okH || !okC || !okR || !okFC || !okFP || !okRo {
		return parseFailure("帧几何字段缺失")
	}
	if frameW < 40 || frameH < 40 || frameCols < 1 || frameRows < 1 ||
		framesPerRound < 3 || rounds < 1 || rounds > 9 ||
		frameCount != framesPerRound*rounds ||
		frameW*frameCols > d.w || frameH*frameRows > d.h {
		return parseFailure("帧几何不合法")
	}
	frameRect := func(f int) image.Rectangle {
		col, row := f%frameCols, f/frameCols
		x0, y0 := col*frameW, row*frameH
		return image.Rect(x0, y0, x0+frameW, y0+frameH)
	}
	centers := make([]int, len(boxes))
	for i, b := range boxes {
		centers[i] = b.cx
	}
	final := make([]int, rounds)
	for r := 0; r < rounds; r++ {
		base := r * framesPerRound
		start, ok := shellMarkerSlot(d.img, frameRect(base), centers)
		if !ok {
			return giveUp(fmt.Sprintf("第 %d 轮找不到目标标记", r+1))
		}
		target := start
		for k := 0; k < 4; k++ {
			prev := base + 2 + 2*k
			move := base + 3 + 2*k
			if move >= frameCount {
				return parseFailure("交换帧索引越界")
			}
			minX, maxX, ok := shellDiffBounds(d.img, frameRect(prev), frameRect(move), frameH)
			if !ok {
				return giveUp(fmt.Sprintf("第 %d 轮第 %d 次交换没有差分", r+1, k+1))
			}
			a, b, ok := identifySwap(centers, boxes, minX, maxX)
			if !ok {
				return giveUp(fmt.Sprintf("第 %d 轮第 %d 次交换无法匹配容器对", r+1, k+1))
			}
			if target == a {
				target = b
			} else if target == b {
				target = a
			}
		}
		final[r] = target
	}
	out := "v1:"
	for i, v := range final {
		if i > 0 {
			out += "."
		}
		out += fmt.Sprintf("%d", v)
	}
	return answer(out)
}

// shellMarkerSlot 在帧顶部（容器盖子上方）找标记的 x 质心。
// 标记只占 y=3..21；容器顶部从 y≈24 开始，因此这个 band 不会混入杯子像素。
func shellMarkerSlot(g *image.Gray, frame image.Rectangle, centers []int) (int, bool) {
	band := 24
	if band > frame.Dy() {
		band = frame.Dy()
	}
	sum, n := 0.0, 0
	for y := frame.Min.Y; y < frame.Min.Y+band; y++ {
		for x := frame.Min.X; x < frame.Max.X; x++ {
			if g.GrayAt(x, y).Y < 100 {
				sum += float64(x - frame.Min.X)
				n++
			}
		}
	}
	if n < 5 {
		return 0, false
	}
	cx := sum / float64(n)
	best, bestDist := -1, 1e9
	for i, c := range centers {
		dist := absFloat(cx - float64(c))
		if dist < bestDist {
			best, bestDist = i, dist
		}
	}
	if best < 0 || bestDist > 20 {
		return 0, false
	}
	return best, true
}

// shellDiffBounds 返回运动帧与上一张静止帧的差分列范围（帧内坐标）。
// 只比较 y≥20 的杯子区域：标记在上方，静止容器/地面线在两帧里完全相同。
func shellDiffBounds(g *image.Gray, prev, move image.Rectangle, frameH int) (int, int, bool) {
	minX, maxX := 1<<30, -1
	for x := 0; x < move.Dx(); x++ {
		diff := 0
		for y := 20; y < frameH; y++ {
			a := int(g.GrayAt(prev.Min.X+x, prev.Min.Y+y).Y)
			b := int(g.GrayAt(move.Min.X+x, move.Min.Y+y).Y)
			if absInt(a-b) > 30 {
				diff++
			}
		}
		if diff >= 8 {
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
		}
	}
	if maxX < 0 {
		return 0, 0, false
	}
	return minX, maxX, true
}

// identifySwap 用差分列的左右边界反推交换对。
//
// 两个源槽位的杯子被移走，差分覆盖 [min(ca,cb)-half, max(ca,cb)+half]；
// 因此 observedCenter ≈ (ca+cb)/2，observedWidth ≈ |cb-ca| + 2*half。
// 六个容器对在 (center,width) 上是可区分的（中心相同的一对宽度差很大）。
func identifySwap(centers []int, boxes []shellBox, minX, maxX int) (int, int, bool) {
	obsCenter := float64(minX+maxX) / 2
	obsWidth := float64(maxX - minX + 1)
	bestA, bestB := -1, -1
	bestScore := 1e9
	for a := 0; a < len(centers); a++ {
		for b := a + 1; b < len(centers); b++ {
			half := float64(boxes[a].w/2 - 2)
			predCenter := float64(centers[a]+centers[b]) / 2
			predWidth := float64(absInt(centers[b]-centers[a])) + 2*half
			score := absFloat(obsCenter-predCenter) + 0.5*absFloat(obsWidth-predWidth)
			if score < bestScore {
				bestScore = score
				bestA, bestB = a, b
			}
		}
	}
	if bestA < 0 || bestScore > 15 {
		return 0, 0, false
	}
	return bestA, bestB, true
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
