package solver

import (
	"errors"
	"image"
	"image/color"
	"math"
)

var errNoInk = errors.New("裁剪区域内没有笔迹")

// toGray 把任意解码结果转成 Gray。题目资产本来就是 image.Gray（PNG 灰度），
// 这里仍保留转换路径，避免把"解码器恰好返回 Gray"当成契约。
func toGray(img image.Image) *image.Gray {
	if g, ok := img.(*image.Gray); ok {
		return g
	}
	b := img.Bounds()
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			g.SetGray(x, y, color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray))
		}
	}
	return g
}

func crop(g *image.Gray, r image.Rectangle) (*image.Gray, bool) {
	r = r.Intersect(g.Bounds())
	if r.Empty() {
		return nil, false
	}
	sub, ok := g.SubImage(r).(*image.Gray)
	if !ok {
		return nil, false
	}
	return sub, true
}

func meanGray(g *image.Gray, x0, y0, w, h int) float64 {
	if w <= 0 || h <= 0 {
		return 0
	}
	var sum, n int
	b := g.Bounds()
	for y := y0; y < y0+h; y++ {
		if y < b.Min.Y || y >= b.Max.Y {
			continue
		}
		for x := x0; x < x0+w; x++ {
			if x < b.Min.X || x >= b.Max.X {
				continue
			}
			sum += int(g.GrayAt(x, y).Y)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}

func minGray(g *image.Gray, x0, y0, w, h int) uint8 {
	v := uint8(255)
	b := g.Bounds()
	for y := y0; y < y0+h; y++ {
		if y < b.Min.Y || y >= b.Max.Y {
			continue
		}
		for x := x0; x < x0+w; x++ {
			if x < b.Min.X || x >= b.Max.X {
				continue
			}
			if p := g.GrayAt(x, y).Y; p < v {
				v = p
			}
		}
	}
	return v
}

// thresholdMask 返回矩形内的二值笔迹图（灰度 < threshold）。
func thresholdMask(g *image.Gray, r image.Rectangle, threshold uint8) ([]bool, int, int, bool) {
	c, ok := crop(g, r)
	if !ok {
		return nil, 0, 0, false
	}
	w, h := c.Bounds().Dx(), c.Bounds().Dy()
	out := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out[y*w+x] = c.GrayAt(c.Bounds().Min.X+x, c.Bounds().Min.Y+y).Y < threshold
		}
	}
	return out, w, h, true
}

// inkBBox 返回笔迹的紧包围盒（相对 mask 左上角）。
func inkBBox(mask []bool, w, h int) (image.Rectangle, bool) {
	minX, minY, maxX, maxY := w, h, -1, -1
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if !mask[y*w+x] {
				continue
			}
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
		}
	}
	if maxX < 0 {
		return image.Rectangle{}, false
	}
	return image.Rect(minX, minY, maxX+1, maxY+1), true
}

// normalizedInk 把笔迹按紧包围盒归一化到 nw×nh 的密度图。
//
// 密度而不是二值：目标条与网格里的同一字形用不同字号绘制（32 vs 34 / 42 vs 34），
// 笔迹包围盒的绝对尺寸不同；按包围盒重采样后比较密度，才能在不读取渲染参数的前提下
// 对齐同一字形。密度图的每个格是"该格覆盖的源像素中笔迹占比"，因此对整体缩放不敏感。
func normalizedInk(g *image.Gray, r image.Rectangle, threshold uint8, nw, nh int) ([]float64, error) {
	if nw < 1 || nh < 1 {
		return nil, errNoInk
	}
	mask, w, h, ok := thresholdMask(g, r, threshold)
	if !ok {
		return nil, errNoInk
	}
	bbox, ok := inkBBox(mask, w, h)
	if !ok {
		return nil, errNoInk
	}
	bw, bh := bbox.Dx(), bbox.Dy()
	ink := make([]float64, nw*nh)
	total := make([]float64, nw*nh)
	for y := 0; y < bh; y++ {
		ny := y * nh / bh
		if ny >= nh {
			ny = nh - 1
		}
		for x := 0; x < bw; x++ {
			nx := x * nw / bw
			if nx >= nw {
				nx = nw - 1
			}
			i := ny*nw + nx
			total[i]++
			if mask[(bbox.Min.Y+y)*w+(bbox.Min.X+x)] {
				ink[i]++
			}
		}
	}
	for i := range ink {
		if total[i] > 0 {
			ink[i] /= total[i]
		}
	}
	return ink, nil
}

// inkDistance 是两张密度图的平均绝对差。范围 0..1。
func inkDistance(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return math.Inf(1)
	}
	var sum float64
	for i := range a {
		sum += math.Abs(a[i] - b[i])
	}
	return sum / float64(len(a))
}

// resampleGray 用双线性插值把矩形重采样到 dw×dh，返回 0..255 的灰度值。
// 目标样本与网格单元的绘制字号不同，必须先缩放到同一坐标系再比较。
func resampleGray(g *image.Gray, r image.Rectangle, dw, dh int) []float64 {
	out := make([]float64, dw*dh)
	if dw < 1 || dh < 1 || r.Empty() {
		return out
	}
	scaleX := float64(r.Dx()) / float64(dw)
	scaleY := float64(r.Dy()) / float64(dh)
	for y := 0; y < dh; y++ {
		sy := float64(r.Min.Y) + (float64(y)+0.5)*scaleY - 0.5
		for x := 0; x < dw; x++ {
			sx := float64(r.Min.X) + (float64(x)+0.5)*scaleX - 0.5
			out[y*dw+x] = bilinear(g, sx, sy)
		}
	}
	return out
}

func bilinear(g *image.Gray, x, y float64) float64 {
	b := g.Bounds()
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	x1, y1 := x0+1, y0+1
	fx, fy := x-float64(x0), y-float64(y0)
	at := func(px, py int) float64 {
		if px < b.Min.X {
			px = b.Min.X
		}
		if px >= b.Max.X {
			px = b.Max.X - 1
		}
		if py < b.Min.Y {
			py = b.Min.Y
		}
		if py >= b.Max.Y {
			py = b.Max.Y - 1
		}
		return float64(g.GrayAt(px, py).Y)
	}
	v00, v10 := at(x0, y0), at(x1, y0)
	v01, v11 := at(x0, y1), at(x1, y1)
	return v00*(1-fx)*(1-fy) + v10*fx*(1-fy) + v01*(1-fx)*fy + v11*fx*fy
}

// ncc 是零均值归一化互相关。相同字形只差重采样时它的峰值接近 1；
// 光照/整体亮度差不会改变它。den=0（常量图）时返回 0。
func ncc(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var ma, mb float64
	for i := range a {
		ma += a[i]
		mb += b[i]
	}
	ma /= float64(len(a))
	mb /= float64(len(b))
	var num, da, db float64
	for i := range a {
		va := a[i] - ma
		vb := b[i] - mb
		num += va * vb
		da += va * va
		db += vb * vb
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}

// meanAbsDiff 是平均绝对差，范围 0..255。
func meanAbsDiff(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return math.Inf(1)
	}
	var sum float64
	for i := range a {
		sum += math.Abs(a[i] - b[i])
	}
	return sum / float64(len(a))
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
