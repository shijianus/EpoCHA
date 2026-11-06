package generator

// 图形题共用的绘制原语（ADR-024）：几何图形只由代码生成，不引入外部素材。
//
// 隔离纪律与 text-image 相同：这里只画**公开题面**，答案（哪些格子/各计数多少）
// 由调用方的 spec 决定，绝不从图形反推答案。判分仍走 generator.go 的 verifier 路径。
//
// 为什么用 golang.org/x/image/vector 而不是自己写扫描线：它已在批准依赖 x/image 内，
// 提供抗锯齿的多边形/贝塞尔光栅化；自己写一套边缘覆盖算法是重复且容易在边界上出错的活。
// 图形数量与画布都有上界（见各生成器的 validate），单次渲染是毫秒级。

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	mrand "math/rand"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/vector"
)

// shapeKind 是图形种类。集合刻意不含菱形：它与旋转后的正方形在视觉上不可区分，
// 会让"选出所有正方形"变成一道有歧义的题。
type shapeKind uint8

const (
	shapeCircle shapeKind = iota
	shapeTriangle
	shapeSquare
	shapePentagon
	shapeHexagon
)

var shapeKindOrder = [...]shapeKind{
	shapeCircle, shapeTriangle, shapeSquare, shapePentagon, shapeHexagon,
}

func (k shapeKind) labelZH() string {
	switch k {
	case shapeCircle:
		return "圆形"
	case shapeTriangle:
		return "三角形"
	case shapeSquare:
		return "正方形"
	case shapePentagon:
		return "五边形"
	case shapeHexagon:
		return "六边形"
	default:
		return "图形"
	}
}

func (k shapeKind) labelEN() string {
	switch k {
	case shapeCircle:
		return "circle"
	case shapeTriangle:
		return "triangle"
	case shapeSquare:
		return "square"
	case shapePentagon:
		return "pentagon"
	case shapeHexagon:
		return "hexagon"
	default:
		return "shape"
	}
}

// shapeCategory 是"种类 + 填充"的组合。判据按种类、按填充或按两者组合，
// 因此类别而不是单独的种类才是匹配的单位。
type shapeCategory struct {
	kind   shapeKind
	filled bool
}

func (c shapeCategory) labelZH() string {
	if c.filled {
		return "实心" + c.kind.labelZH()
	}
	return "空心" + c.kind.labelZH()
}

func (c shapeCategory) labelEN() string {
	if c.filled {
		return "filled " + c.kind.labelEN()
	}
	return "hollow " + c.kind.labelEN()
}

// allShapeCategories 是 5 种图形 × 2 种填充 = 10 个类别，顺序固定。
func allShapeCategories() []shapeCategory {
	out := make([]shapeCategory, 0, len(shapeKindOrder)*2)
	for _, k := range shapeKindOrder {
		out = append(out, shapeCategory{kind: k, filled: true})
		out = append(out, shapeCategory{kind: k, filled: false})
	}
	return out
}

// shapeDraw 是一处已经定好位置/旋转/大小的图形。
type shapeDraw struct {
	Cat      shapeCategory
	Cx, Cy   float64
	Radius   float64
	Rotation float64 // 弧度
	Stroke   float64 // 空心描边宽度（像素）；实心忽略
}

// drawShape 把一处图形画到灰度画布上。空心图形用"外轮廓 mask − 内轮廓 mask"，
// 而不是先画墨再用纸色挖洞：后者会把背景纹理/干扰点一起擦掉，在纸面不纯色时露馅。
func drawShape(dst *image.Gray, s shapeDraw, ink uint8) {
	if s.Radius <= 0 || s.Stroke < 0 {
		return
	}
	pad := s.Radius + s.Stroke + 2
	b := image.Rect(
		int(math.Floor(s.Cx-pad)), int(math.Floor(s.Cy-pad)),
		int(math.Ceil(s.Cx+pad)), int(math.Ceil(s.Cy+pad)),
	).Intersect(dst.Bounds())
	if b.Empty() {
		return
	}
	lx, ly := s.Cx-float64(b.Min.X), s.Cy-float64(b.Min.Y)
	w, h := b.Dx(), b.Dy()

	outer := rasterizeShape(w, h, s.Cat.kind, lx, ly, s.Radius, s.Rotation)
	mask := outer
	if !s.Cat.filled {
		innerR := s.Radius - s.Stroke
		if innerR > s.Radius*0.35 {
			inner := rasterizeShape(w, h, s.Cat.kind, lx, ly, innerR, s.Rotation)
			mask = ringMask(outer, inner)
		}
	}
	xdraw.DrawMask(dst, b, image.NewUniform(color.Gray{Y: ink}), image.Point{},
		mask, image.Point{}, xdraw.Over)
}

func rasterizeShape(w, h int, kind shapeKind, cx, cy, r, rot float64) *image.Alpha {
	mask := image.NewAlpha(image.Rect(0, 0, w, h))
	if w <= 0 || h <= 0 || r <= 0 {
		return mask
	}
	z := vector.NewRasterizer(w, h)
	shapePath(z, kind, cx, cy, r, rot)
	z.Draw(mask, mask.Bounds(), image.NewUniform(color.Alpha{A: 255}), image.Point{})
	return mask
}

func ringMask(outer, inner *image.Alpha) *image.Alpha {
	out := image.NewAlpha(outer.Bounds())
	for i := range out.Pix {
		o, n := uint16(outer.Pix[i]), uint16(inner.Pix[i])
		if o > n {
			out.Pix[i] = uint8(o - n)
		}
	}
	return out
}

// shapePath 把一种图形写进光栅化器。坐标是**局部**坐标（画布左上为原点）。
func shapePath(z *vector.Rasterizer, kind shapeKind, cx, cy, r, rot float64) {
	switch kind {
	case shapeCircle:
		circlePath(z, cx, cy, r)
	case shapeTriangle:
		polygonPath(z, cx, cy, r, 3, -math.Pi/2+rot)
	case shapeSquare:
		// 从 -45° 开始 ⇒ 轴对齐的正方形；全程只有小角度旋转，不会被看成菱形。
		polygonPath(z, cx, cy, r, 4, -math.Pi/4+rot)
	case shapePentagon:
		polygonPath(z, cx, cy, r, 5, -math.Pi/2+rot)
	case shapeHexagon:
		polygonPath(z, cx, cy, r, 6, -math.Pi/2+rot)
	default:
		circlePath(z, cx, cy, r)
	}
}

func polygonPath(z *vector.Rasterizer, cx, cy, r float64, sides int, start float64) {
	for i := 0; i < sides; i++ {
		a := start + 2*math.Pi*float64(i)/float64(sides)
		x, y := float32(cx+r*math.Cos(a)), float32(cy+r*math.Sin(a))
		if i == 0 {
			z.MoveTo(x, y)
		} else {
			z.LineTo(x, y)
		}
	}
	z.ClosePath()
}

// circlePath 用四段三次贝塞尔近似圆。控制点常数是 4/3·tan(π/8)，
// 近似误差远小于一个像素（半径 ≤96 的图形上不可见）。
func circlePath(z *vector.Rasterizer, cx, cy, r float64) {
	const k = 0.5522847498307936
	x, y := float32(cx), float32(cy)
	rf := float32(r)
	kf := float32(r * k)
	z.MoveTo(x+rf, y)
	z.CubeTo(x+rf, y+kf, x+kf, y+rf, x, y+rf)
	z.CubeTo(x-kf, y+rf, x-rf, y+kf, x-rf, y)
	z.CubeTo(x-rf, y-kf, x-kf, y-rf, x, y-rf)
	z.CubeTo(x+kf, y-rf, x+rf, y-kf, x+rf, y)
	z.ClosePath()
}

// renderShapeScene 画纸面、轻噪点与图形，返回 PNG 字节。
//
// 噪点画在图形**之前**：图形本身保持干净，判据（种类/填充/数量）不被噪点干扰；
// 噪点只降低背景的"纯色感"。它的上界与 text-image 的 5% 同一口径。
func renderShapeScene(width, height, noise int, shapes []shapeDraw, rng *mrand.Rand) ([]byte, error) {
	switch {
	case width < 40 || width > maxImageWidth:
		return nil, fmt.Errorf("%w: 画布宽 %d 越界", ErrBadParams, width)
	case height < 40 || height > maxImageWidth:
		return nil, fmt.Errorf("%w: 画布高 %d 越界", ErrBadParams, height)
	case width*height > maxImagePixels:
		return nil, fmt.Errorf("%w: 像素总数 %d 越界", ErrBadParams, width*height)
	case noise < 0 || noise > width*height/maxNoiseDivisor:
		return nil, fmt.Errorf("%w: 噪点 %d 越界", ErrBadParams, noise)
	}

	img := image.NewGray(image.Rect(0, 0, width, height))
	xdraw.Draw(img, img.Bounds(), image.NewUniform(color.Gray{Y: paperGray}), image.Point{}, xdraw.Src)
	for i := 0; i < noise; i++ {
		x, y := rng.Intn(width), rng.Intn(height)
		v := int(paperGray) - rng.Intn(noiseGraySpread+1)
		img.SetGray(x, y, color.Gray{Y: uint8(clampInt(v, 0, 255))})
	}
	for _, s := range shapes {
		drawShape(img, s, inkGray)
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("generator: PNG 编码: %w", err)
	}
	out := buf.Bytes()
	if len(out) > MaxImageAssetBytes {
		return nil, fmt.Errorf("%w: %d 字节（上限 %d）", ErrRenderTooLarge, len(out), MaxImageAssetBytes)
	}
	return out, nil
}
