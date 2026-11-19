package solver

import (
	"fmt"
	"image"
)

// silhouette-assembly-v1 baseline：部件是 3×3 栅格上的程序化 polyomino，槽位是同一形状
// 旋转后的轮廓。payload 给了 5 个部件与 5 个槽位的包围盒；攻击器从公开图片里按 3×3
// 采样占用格，得到 pieceMask 与 slotMask，再枚举 piece×slot×0/90/180/270，
// 找唯一完整 assignment。
//
// 部件托盘按 cell=18 填满 54×54；槽位按 cell=20、inset=2 绘制，所以采样中心分别为
// x+9+18c 与 x+12+20c。两者都是公开图片上的可见几何，不依赖 generator 的私有数据。

const silhouetteCells = 3

type silhouettePiece struct {
	id   string
	mask uint16
}

func solveSilhouetteAssembly(publicJSON []byte) Outcome {
	d, err := loadPublic(publicJSON)
	if err != nil {
		return parseFailure(err.Error())
	}
	piecesRaw, ok := docObjects(d.raw, "pieces")
	if !ok || len(piecesRaw) == 0 {
		return parseFailure("pieces 缺失")
	}
	slotsRaw, ok := docObjects(d.raw, "slots")
	if !ok || len(slotsRaw) == 0 || len(slotsRaw) > len(piecesRaw) {
		return parseFailure("slots 缺失或数量不合法")
	}
	pieces := make([]silhouettePiece, len(piecesRaw))
	seenIDs := map[string]bool{}
	for i, obj := range piecesRaw {
		id, ok := docString(obj, "id")
		if !ok || len(id) != 1 || id[0] < '0' || id[0] > '9' || seenIDs[id] {
			return parseFailure(fmt.Sprintf("部件 %d 的 id 不合法", i))
		}
		seenIDs[id] = true
		x, y, w, h, ok := rectFields(obj)
		if !ok || w < 24 || h < 24 {
			return parseFailure(fmt.Sprintf("部件 %s 几何不合法", id))
		}
		mask, ok := extractPieceMask(d.img, image.Rect(x, y, x+w, y+h))
		if !ok {
			return giveUp(fmt.Sprintf("部件 %s 占用格提取失败", id))
		}
		pieces[i] = silhouettePiece{id: id, mask: mask}
	}
	slotMasks := make([]uint16, len(slotsRaw))
	for i, obj := range slotsRaw {
		x, y, w, h, ok := rectFields(obj)
		if !ok || w < 24 || h < 24 {
			return parseFailure(fmt.Sprintf("槽位 %d 几何不合法", i))
		}
		mask, ok := extractSlotMask(d.img, image.Rect(x, y, x+w, y+h))
		if !ok {
			return giveUp(fmt.Sprintf("槽位 %d 占用格提取失败", i))
		}
		slotMasks[i] = mask
	}

	type cand struct {
		piece int
		rot   int
	}
	options := make([][]cand, len(slotMasks))
	for s, slotMask := range slotMasks {
		for p, piece := range pieces {
			for r := 0; r < 4; r++ {
				if rotateSilhouette(piece.mask, r) == slotMask {
					options[s] = append(options[s], cand{piece: p, rot: r})
				}
			}
		}
		if len(options[s]) == 0 {
			return giveUp(fmt.Sprintf("槽位 %d 没有匹配的部件", s))
		}
	}
	used := make([]bool, len(pieces))
	assignment := make([]cand, len(slotMasks))
	var solutions [][]cand
	var walk func(slot int)
	walk = func(slot int) {
		if len(solutions) > 1 {
			return
		}
		if slot == len(slotMasks) {
			solutions = append(solutions, append([]cand(nil), assignment...))
			return
		}
		for _, c := range options[slot] {
			if used[c.piece] {
				continue
			}
			used[c.piece] = true
			assignment[slot] = c
			walk(slot + 1)
			used[c.piece] = false
		}
	}
	walk(0)
	if len(solutions) == 0 {
		return giveUp("没有完整装配")
	}
	if len(solutions) > 1 {
		return giveUp("装配解不唯一")
	}
	out := "v1:"
	for s, c := range solutions[0] {
		if s > 0 {
			out += ","
		}
		out += pieces[c.piece].id + ":" + fmt.Sprintf("%d", c.rot)
	}
	return answer(out)
}

func rectFields(obj map[string]any) (x, y, w, h int, ok bool) {
	x, okX := docInt(obj, "x")
	y, okY := docInt(obj, "y")
	w, okW := docInt(obj, "w")
	h, okH := docInt(obj, "h")
	return x, y, w, h, okX && okY && okW && okH
}

// extractPieceMask 从 54×54 托盘格子里采样 3×3 占用：占用格填充 96，空位是托盘 252。
func extractPieceMask(g *image.Gray, rect image.Rectangle) (uint16, bool) {
	rect = rect.Intersect(g.Bounds())
	if rect.Empty() {
		return 0, false
	}
	cell := rect.Dx() / silhouetteCells
	if cell < 8 {
		return 0, false
	}
	mx := rect.Min.X + (rect.Dx()-silhouetteCells*cell)/2
	my := rect.Min.Y + (rect.Dy()-silhouetteCells*cell)/2
	var mask uint16
	for r := 0; r < silhouetteCells; r++ {
		for c := 0; c < silhouetteCells; c++ {
			cx := mx + c*cell + cell/2
			cy := my + r*cell + cell/2
			if meanGray(g, cx-2, cy-2, 5, 5) < 180 {
				mask |= 1 << uint(r*silhouetteCells+c)
			}
		}
	}
	if mask == 0 {
		return 0, false
	}
	return mask, true
}

// extractSlotMask 从 64×64 槽位采样 3×3 占用：槽位 cell=20、inset=2，
// 占用格有 226 填充 + 44 轮廓 + 170 斜纹，空位是 246。阈值放在两者之间。
func extractSlotMask(g *image.Gray, rect image.Rectangle) (uint16, bool) {
	rect = rect.Intersect(g.Bounds())
	if rect.Empty() {
		return 0, false
	}
	cell := (rect.Dx() - 4) / silhouetteCells
	if cell < 8 {
		return 0, false
	}
	mx := rect.Min.X + 2
	my := rect.Min.Y + 2
	var mask uint16
	for r := 0; r < silhouetteCells; r++ {
		for c := 0; c < silhouetteCells; c++ {
			cx := mx + c*cell + cell/2
			cy := my + r*cell + cell/2
			if meanGray(g, cx-2, cy-2, 5, 5) < 238 {
				mask |= 1 << uint(r*silhouetteCells+c)
			}
		}
	}
	if mask == 0 {
		return 0, false
	}
	return mask, true
}

// rotateSilhouette 与生成器的 90° 顺时针旋转约定一致：(r,c) → (c,2-r)。
func rotateSilhouette(mask uint16, turns int) uint16 {
	turns &= 3
	var out uint16
	for r := 0; r < silhouetteCells; r++ {
		for c := 0; c < silhouetteCells; c++ {
			if mask&(1<<uint(r*silhouetteCells+c)) == 0 {
				continue
			}
			nr, nc := r, c
			for t := 0; t < turns; t++ {
				nr, nc = nc, 2-nr
			}
			out |= 1 << uint(nr*silhouetteCells+nc)
		}
	}
	return out
}
