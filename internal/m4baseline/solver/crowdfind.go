package solver

// crowd-find-v1 baseline：36 个角色由 6 个离散特征组合（头轮廓、眼睛、天线、嘴、
// 身体纹理、配件）。样本卡与网格用同一套绘制代码，但样本字号 58、网格 42，且若直接
// 缩放整张卡，非缩放常量（眼距 5px、纹理间距 5px 等）会让像素差失去判别力。
//
// 这里改为**逐特征分类**：用公开卡片的几何（card 尺寸 - 6 = 绘制 size）算出
// cx/cy/hy/hr/by，再在固定偏移的窗口里对 4/3 个候选模板做二值差。每个特征都在
// 相同像素偏移处绘制，所以样本与正确格子的同名特征距离接近 0。最后比较 7 元组
// （head, eyes, mouth, antenna, hat, texture, accessory），目标应是唯一精确匹配。

import (
	"fmt"
	"image"
	"math"
	"sort"
)

type bitmap struct {
	w, h int
	v    []bool
}

func newBitmap(w, h int) *bitmap { return &bitmap{w: w, h: h, v: make([]bool, w*h)} }

func (b *bitmap) get(x, y int) bool {
	if x < 0 || y < 0 || x >= b.w || y >= b.h {
		return false
	}
	return b.v[y*b.w+x]
}

func (b *bitmap) set(x, y int) {
	if x < 0 || y < 0 || x >= b.w || y >= b.h {
		return
	}
	b.v[y*b.w+x] = true
}

func (b *bitmap) fillRect(x, y, w, h int) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			b.set(xx, yy)
		}
	}
}

func (b *bitmap) fillDisc(cx, cy, r int) {
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			if x*x+y*y <= r*r {
				b.set(cx+x, cy+y)
			}
		}
	}
}

func (b *bitmap) thickLine(x0, y0, x1, y1, thickness int) {
	if thickness < 1 {
		thickness = 1
	}
	lo := -(thickness / 2)
	hi := lo + thickness - 1
	dx := absInt(x1 - x0)
	dy := -absInt(y1 - y0)
	sx, sy := -1, -1
	if x0 < x1 {
		sx = 1
	}
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		for oy := lo; oy <= hi; oy++ {
			for ox := lo; ox <= hi; ox++ {
				b.set(x0+ox, y0+oy)
			}
		}
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

type crowdFeature struct {
	head, eyes, mouth, antenna, hat, texture, accessory int
}

type crowdExtract struct {
	feat   crowdFeature
	score  float64
	valid  bool
	reason string
}

const (
	crowdInk       = uint8(100)
	crowdFillLimit = uint8(230)
)

func solveCrowdFind(publicJSON []byte) Outcome {
	d, err := loadPublic(publicJSON)
	if err != nil {
		return parseFailure(err.Error())
	}
	vals, err := requireInt(d, "grid_cols", "grid_rows", "grid_x", "grid_y",
		"grid_cell", "grid_gap", "sample_x", "sample_y", "sample_w", "sample_h")
	if err != nil {
		return parseFailure(err.Error())
	}
	cols, rows, gridX, gridY := vals[0], vals[1], vals[2], vals[3]
	cell, gap := vals[4], vals[5]
	sx, sy, sw, sh := vals[6], vals[7], vals[8], vals[9]
	if cols < 1 || rows < 1 || cols*rows > 100 || cell < 24 || sw < 24 || sh < 24 {
		return parseFailure("网格/样本几何不合法")
	}
	sample := extractCrowd(d.img, sx, sy, sw, sh)
	if !sample.valid {
		return giveUp("样本特征分类失败: " + sample.reason)
	}
	type scored struct {
		idx        int
		compatible bool
		knownEqual int
		knownDiff  int
		score      float64
		feat       crowdFeature
	}
	var matches []scored
	for i := 0; i < cols*rows; i++ {
		r, c := i/cols, i%cols
		x := gridX + c*(cell+gap)
		y := gridY + r*(cell+gap)
		ex := extractCrowd(d.img, x, y, cell, cell)
		if !ex.valid {
			continue
		}
		compat, eq, diff := crowdCompare(sample.feat, ex.feat)
		matches = append(matches, scored{
			idx: i, compatible: compat, knownEqual: eq, knownDiff: diff,
			score: ex.score, feat: ex.feat,
		})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].compatible != matches[j].compatible {
			return matches[i].compatible
		}
		if matches[i].knownEqual != matches[j].knownEqual {
			return matches[i].knownEqual > matches[j].knownEqual
		}
		return matches[i].score < matches[j].score
	})
	var compat []scored
	for _, m := range matches {
		if !m.compatible {
			break
		}
		compat = append(compat, m)
	}
	if len(compat) > 0 && compat[0].knownEqual >= 4 {
		// 同一批已知特征一致的候选里，模板得分最低者作答；未知特征（如帽子
		// 遮住的天线）本来就不可见，分数是唯一的确定性判据。
		return answer(fmt.Sprintf("v1:%d", compat[0].idx))
	}
	best := matches[0]
	return giveUp(fmt.Sprintf("没有唯一的特征匹配: sample=%+v best=%+v known_diff=%d", sample.feat, best.feat, best.knownDiff))

}

// crowdCompare 逐特征比较；-1 表示分类器对该特征不确定（例如帽子遮住天线），
// 未知特征不参与相等/不相等判定。生成器保证 36 个签名两两不同，但**可见特征**
// 可能无法区分两个只在被遮挡特征上不同的角色；这种情况下不猜。
func crowdCompare(a, b crowdFeature) (compatible bool, knownEqual, knownDiff int) {
	pairs := [][2]int{
		{a.head, b.head}, {a.eyes, b.eyes}, {a.mouth, b.mouth},
		{a.antenna, b.antenna}, {a.hat, b.hat}, {a.texture, b.texture}, {a.accessory, b.accessory},
	}
	for _, p := range pairs {
		if p[0] < 0 || p[1] < 0 {
			continue
		}
		if p[0] == p[1] {
			knownEqual++
		} else {
			knownDiff++
		}
	}
	return knownDiff == 0, knownEqual, knownDiff
}

func extractCrowd(img *image.Gray, x, y, w, h int) crowdExtract {
	size := w - 6
	if h-6 < size {
		size = h - 6
	}
	if size < 18 {
		return crowdExtract{}
	}
	cx, cy := x+w/2, y+h/2
	hy := cy - size/6
	hr := size / 4
	by := cy + size/8
	bw, bh := size*3/5, size/3
	var ex crowdExtract
	var ok bool
	ex.feat.head, ex.score, _ = classifyHead(img, cx, hy, hr)
	eyes, d, ok := classifyEyes(img, cx, hy, hr)
	if !ok {
		eyes, d = -1, 1.0
	}
	ex.feat.eyes = eyes
	ex.score += d
	mouth, d, ok := classifyMouth(img, cx, hy, hr)
	if !ok {
		mouth, d = -1, 1.0
	}
	ex.feat.mouth = mouth
	ex.score += d
	antenna, hat, d, ok := classifyAntennaHat(img, cx, hy, hr)
	if !ok {
		antenna, hat, d = -1, -1, 1.0
	}
	ex.feat.antenna, ex.feat.hat = antenna, hat
	ex.score += d
	texture, d, ok := classifyTexture(img, cx-bw/2, by, bw, bh)
	if !ok {
		texture, d = -1, 1.0
	}
	ex.feat.texture = texture
	ex.score += d
	accessory, d, ok := classifyAccessory(img, cx, by)
	if !ok {
		accessory, d = -1, 1.0
	}
	ex.feat.accessory = accessory
	ex.score += d
	ex.valid = true
	return ex
}

func meanGrayC(img *image.Gray, x, y, r int) float64 {
	sum, n := 0, 0
	b := img.Bounds()
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			px, py := x+dx, y+dy
			if !image.Pt(px, py).In(b) {
				continue
			}
			sum += int(img.GrayAt(px, py).Y)
			n++
		}
	}
	if n == 0 {
		return 255
	}
	return float64(sum) / float64(n)
}

func classifyHead(img *image.Gray, cx, hy, hr int) (int, float64, bool) {
	if hr < 5 {
		return 0, 0, false
	}
	// 头形用三个固定相对点判断，避开固定像素大小的眼睛/嘴巴：
	//   - 头顶内侧两点（x=±hr/3, y=hy-hr+3）：圆/方是填充 214，六边形是背景 252；
	//   - 上方两角（±0.8hr, -0.8hr）：方形仍在填充内，圆已出圆外。
	off := hr / 3
	y := hy - hr + 3
	a := meanGrayC(img, cx+off, y, 0)
	b := meanGrayC(img, cx-off, y, 0)
	if a >= float64(crowdFillLimit) && b >= float64(crowdFillLimit) {
		return 2, 0.2, true // 六边形
	}
	co := int(math.Round(0.8 * float64(hr)))
	c1 := meanGrayC(img, cx-co, hy-co, 0)
	c2 := meanGrayC(img, cx+co, hy-co, 0)
	if c1 < float64(crowdFillLimit) && c2 < float64(crowdFillLimit) {
		return 1, 0.1, true // 方形
	}
	return 0, 0.1, true // 圆形
}

func classifyEyes(img *image.Gray, cx, hy, hr int) (int, float64, bool) {
	tpls := eyeTemplates()
	ox, oy := -9, -8
	best, second := -1, math.Inf(1)
	bestD := math.Inf(1)
	for i, t := range tpls {
		d := templateDistance(img, cx+ox, hy+oy, t, nil, cx, hy, hr-2)
		if d < bestD {
			second, bestD = bestD, d
			best = i
		} else if d < second {
			second = d
		}
	}
	if best < 0 || math.IsInf(bestD, 1) {
		return 0, bestD, false
	}
	if bestD > 0.30 {
		return -1, bestD, true
	}
	if !math.IsInf(second, 1) && second-bestD < 0.02 {
		return -1, bestD, true
	}
	return best, bestD, true
}

func classifyMouth(img *image.Gray, cx, hy, hr int) (int, float64, bool) {
	tpls := mouthTemplates()
	ox, oy := -7, 2
	best, second := -1, math.Inf(1)
	bestD := math.Inf(1)
	for i, t := range tpls {
		d := templateDistance(img, cx+ox, hy+oy, t, nil, cx, hy, hr-2)
		if d < bestD {
			second, bestD = bestD, d
			best = i
		} else if d < second {
			second = d
		}
	}
	if best < 0 || math.IsInf(bestD, 1) {
		return 0, bestD, false
	}
	if bestD > 0.35 {
		return -1, bestD, true
	}
	if !math.IsInf(second, 1) && second-bestD < 0.02 {
		return -1, bestD, true
	}
	return best, bestD, true
}

func classifyAntennaHat(img *image.Gray, cx, hy, hr int) (int, int, float64, bool) {
	type combo struct {
		ant, hat int
		tpl      *bitmap
	}
	var combos []combo
	for a := 0; a < 4; a++ {
		for h := 0; h < 2; h++ {
			combos = append(combos, combo{ant: a, hat: h, tpl: antennaTemplate(a, h)})
		}
	}
	best, second := -1, math.Inf(1)
	bestD := math.Inf(1)
	for i, c := range combos {
		d := templateDistance(img, cx-10, hy-hr-14, c.tpl, nil, 0, 0, 0)
		if d < bestD {
			second, bestD = bestD, d
			best = i
		} else if d < second {
			second = d
		}
	}
	if best < 0 || math.IsInf(bestD, 1) {
		return 0, 0, bestD, false
	}
	if bestD > 0.40 {
		return -1, -1, bestD, true
	}
	if !math.IsInf(second, 1) && second-bestD < 0.01 {
		return -1, -1, bestD, true
	}
	return combos[best].ant, combos[best].hat, bestD, true
}

func classifyTexture(img *image.Gray, bx, by, bw, bh int) (int, float64, bool) {
	if bw < 8 || bh < 6 {
		return 0, 0, false
	}
	best, second := -1, math.Inf(1)
	bestD := math.Inf(1)
	valid := func(lx, ly int) bool {
		return lx >= 2 && lx < bw-2 && ly >= 2 && ly < bh-2
	}
	for k := 0; k < 4; k++ {
		t := textureTemplate(k, bw, bh)
		d := templateDistance(img, bx, by, t, valid, 0, 0, 0)
		if d < bestD {
			second, bestD = bestD, d
			best = k
		} else if d < second {
			second = d
		}
	}
	if best < 0 || math.IsInf(bestD, 1) {
		return 0, bestD, false
	}
	if bestD > 0.35 {
		return -1, bestD, true
	}
	if !math.IsInf(second, 1) && second-bestD < 0.02 {
		return -1, bestD, true
	}
	return best, bestD, true
}

func classifyAccessory(img *image.Gray, cx, by int) (int, float64, bool) {
	valid := func(lx, ly int) bool {
		// bitmap 局部坐标 (0,0) 对应绘制局部 (-11,-7)。
		dlx, dly := lx-11, ly-7
		return dly <= -1 || (dlx >= 6 && dly >= 2)
	}
	best, second := -1, math.Inf(1)
	bestD := math.Inf(1)
	for _, k := range []int{0, 2, 3} {
		t := accessoryTemplate(k)
		d := templateDistance(img, cx-11, by-7, t, valid, 0, 0, 0)
		if d < bestD {
			second, bestD = bestD, d
			best = k
		} else if d < second {
			second = d
		}
	}
	if best < 0 || math.IsInf(bestD, 1) {
		return 0, bestD, false
	}
	if bestD > 0.35 {
		return -1, bestD, true
	}
	if !math.IsInf(second, 1) && second-bestD < 0.02 {
		return -1, bestD, true
	}
	return best, bestD, true
}

// templateDistance 比较窗口内的暗像素模板；ignoreR>0 时以 (icx,icy) 为圆心、
// 半径 ignoreR 之外/之内的像素不参与（用于遮掉不同尺寸头的轮廓）。
// 返回 0..1 的失配比例。
func templateDistance(img *image.Gray, ox, oy int, tpl *bitmap, valid func(lx, ly int) bool,
	icx, icy, ignoreR int) float64 {
	n, bad := 0, 0
	b := img.Bounds()
	for ly := 0; ly < tpl.h; ly++ {
		for lx := 0; lx < tpl.w; lx++ {
			if valid != nil && !valid(lx, ly) {
				continue
			}
			ax, ay := ox+lx, oy+ly
			if !image.Pt(ax, ay).In(b) {
				continue
			}
			if ignoreR > 0 {
				dx, dy := ax-icx, ay-icy
				if dx*dx+dy*dy > ignoreR*ignoreR {
					continue
				}
			}
			n++
			obs := img.GrayAt(ax, ay).Y < crowdInk
			if obs != tpl.get(lx, ly) {
				bad++
			}
		}
	}
	if n == 0 {
		return math.Inf(1)
	}
	return float64(bad) / float64(n)
}

// --- 模板（都相对公开绘制锚点，局部坐标）---

func eyeTemplates() []*bitmap {
	layouts := [][][2]int{
		{{0, 0}},
		{{-5, 0}, {5, 0}},
		{{0, -5}, {0, 5}},
		{{-7, 0}, {0, -5}, {7, 0}},
	}
	out := make([]*bitmap, 0, 4)
	for _, pts := range layouts {
		b := newBitmap(19, 11)
		for _, p := range pts {
			b.fillDisc(9+p[0], 8+p[1], 2)
		}
		out = append(out, b)
	}
	return out
}

func mouthTemplates() []*bitmap {
	// 局部窗口 x=-7..7, y=+2..9；bitmap 坐标 = 绘制局部 - (x0=-7, y0=+2)。
	mk := func(draw func(b *bitmap)) *bitmap {
		b := newBitmap(15, 8)
		draw(b)
		return b
	}
	return []*bitmap{
		mk(func(b *bitmap) { b.thickLine(2, 3, 12, 3, 2) }), // 0 横线 (-5,+5)-(5,+5)
		mk(func(b *bitmap) { // 1 U 形
			b.thickLine(2, 2, 5, 5, 2)
			b.thickLine(5, 5, 9, 5, 2)
			b.thickLine(9, 5, 12, 2, 2)
		}),
		mk(func(b *bitmap) { b.fillDisc(7, 3, 2) }), // 2 点 (0,+5)
		mk(func(b *bitmap) { // 3 锯齿
			b.thickLine(1, 3, 5, 1, 2)
			b.thickLine(5, 1, 9, 5, 2)
			b.thickLine(9, 5, 13, 3, 2)
		}),
	}
}

func antennaTemplate(ant, hat int) *bitmap {
	// 局部窗口 x=-10..10, y=-14..-1；坐标先平移到 (10,14)。
	b := newBitmap(21, 14)
	X := func(x int) int { return 10 + x }
	Y := func(y int) int { return 14 + y }
	switch ant {
	case 1:
		b.thickLine(X(0), Y(0), X(0), Y(-8), 2)
	case 2:
		b.thickLine(X(0), Y(0), X(-7), Y(-8), 2)
		b.thickLine(X(0), Y(0), X(7), Y(-8), 2)
	case 3:
		b.thickLine(X(0), Y(0), X(4), Y(-5), 2)
		b.thickLine(X(4), Y(-5), X(-2), Y(-9), 2)
		b.thickLine(X(-2), Y(-9), X(3), Y(-13), 2)
	}
	if hat == 1 {
		b.fillRect(X(-9), Y(-6), 18, 5)
		b.fillRect(X(-5), Y(-12), 10, 6)
	}
	return b
}

func textureTemplate(kind, bw, bh int) *bitmap {
	b := newBitmap(bw, bh)
	switch kind {
	case 0:
	case 1:
		for y := 3; y < bh-2; y += 5 {
			for x := 3; x < bw-2; x += 5 {
				b.fillDisc(x, y, 1)
			}
		}
	case 2:
		for x := 4; x < bw-2; x += 5 {
			b.thickLine(x, 2, x, bh-3, 1)
		}
	case 3:
		for y := 2; y < bh-2; y += 4 {
			for x := 2; x < bw-2; x += 4 {
				if ((x/4)+(y/4))%2 == 0 {
					b.fillRect(x, y, 2, 2)
				}
			}
		}
	}
	return b
}

func accessoryTemplate(kind int) *bitmap {
	// 局部窗口 x=-11..11, y=-7..9；坐标先平移到 (11,7)。
	b := newBitmap(23, 17)
	X := func(x int) int { return 11 + x }
	Y := func(y int) int { return 7 + y }
	switch kind {
	case 2:
		b.thickLine(X(0), Y(-1), X(-8), Y(-5), 3)
		b.thickLine(X(0), Y(-1), X(8), Y(-5), 3)
	case 3:
		b.fillRect(X(-10), Y(-2), 20, 4)
		b.thickLine(X(7), Y(2), X(10), Y(8), 3)
	}
	return b
}
