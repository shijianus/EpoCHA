package generator

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	mrand "math/rand"
	"strconv"
	"strings"
)

// intersection-point-v1：虚线延长线交点点击（Phase 4.5）。
//
// 复用 point 原语：640×360 图上两条虚线在随机位置相交，交点处不画任何标记，
// 用户把两条虚线延长后点击交点。**没有**遮挡块——否则"点遮挡块中心"就成了
// 不需要看图就能通过的捷径，M3 的随机猜测模型也会失真。
// 服务端把 (x,y) 量化到难度相关的网格，用 TSV 接受目标格及 8 邻格；无前端改动。
const (
	IntersectionPointID       = "intersection-point-v1"
	intersectionVersion       = "v1"
	intersectionW             = 640
	intersectionH             = 360
	intersectionMaxAnswerLen  = 8
	intersectionVerifierMagic = "IPV1"
)

type intersectionTier struct {
	Cols, Rows int
}

// 难度越高，量化网格越细（同样的像素误差会落到更多候选格），但接受域仍是 3×3 格。
var intersectionTiers = map[int]intersectionTier{
	1: {Cols: 32, Rows: 18},
	2: {Cols: 40, Rows: 24},
	3: {Cols: 48, Rows: 27},
	4: {Cols: 56, Rows: 32},
	5: {Cols: 64, Rows: 36},
}

func IntersectionPointTier(difficulty int) (int, int, error) {
	t, ok := intersectionTiers[difficulty]
	if !ok {
		return 0, 0, fmt.Errorf("%w: intersection-point 缺少难度 %d 的档位", ErrBadParams, difficulty)
	}
	return t.Cols, t.Rows, nil
}

type IntersectionPointPublic struct {
	Kind         string `json:"kind"`
	AnswerType   string `json:"answer_type"`
	MaxAnswerLen int    `json:"max_answer_len"`
	ImageWidth   int    `json:"image_width"`
	ImageHeight  int    `json:"image_height"`
	PromptZH     string `json:"prompt_zh"`
	PromptEN     string `json:"prompt_en"`
}

func (IntersectionPointPublic) PublicKind() string { return "intersection-point" }

type intersectionSpec struct {
	Cols, Rows       int
	CellX, CellY     int
	TargetX, TargetY int
	VisualSeed       int64
}

type IntersectionPoint struct{ macKey []byte }

var _ Generator = IntersectionPoint{}

func NewIntersectionPoint(macKey []byte) (IntersectionPoint, error) {
	if len(macKey) < 32 {
		return IntersectionPoint{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		if _, _, err := IntersectionPointTier(d); err != nil {
			return IntersectionPoint{}, err
		}
	}
	return IntersectionPoint{macKey: macKey}, nil
}

func (IntersectionPoint) ID() string      { return IntersectionPointID }
func (IntersectionPoint) Version() string { return intersectionVersion }
func (IntersectionPoint) Media() string   { return MediaImage }

// intersectionMarginPx 是目标格到画布边缘的最小像素距离。交点太靠边时，
// 一条线的可见部分会短到无法外推；这个边距同时把目标区域限制在网格中央约 54%，
// 因此 M3 要按"目标格的可行范围"算，而不是按整张网格算。
const intersectionMarginPx = 50

// intersectionCellBounds 返回格中心落在内边距内的闭区间。
func intersectionCellBounds(cols, rows int) (minCX, maxCX, minCY, maxCY int) {
	centerOK := func(c, n, size int) bool {
		p := (c*size + size/2) / n
		return p >= intersectionMarginPx && p <= size-1-intersectionMarginPx
	}
	minCX, maxCX = 0, cols-1
	for minCX < cols && !centerOK(minCX, cols, intersectionW) {
		minCX++
	}
	for maxCX > 0 && !centerOK(maxCX, cols, intersectionW) {
		maxCX--
	}
	minCY, maxCY = 0, rows-1
	for minCY < rows && !centerOK(minCY, rows, intersectionH) {
		minCY++
	}
	for maxCY > 0 && !centerOK(maxCY, rows, intersectionH) {
		maxCY--
	}
	return minCX, maxCX, minCY, maxCY
}

func buildIntersectionPoint(entropy io.Reader, difficulty int) (intersectionSpec, []byte, error) {
	cols, rows, err := IntersectionPointTier(difficulty)
	if err != nil {
		return intersectionSpec{}, nil, err
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return intersectionSpec{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	minCX, maxCX, minCY, maxCY := intersectionCellBounds(cols, rows)
	if minCX > maxCX || minCY > maxCY {
		return intersectionSpec{}, nil, fmt.Errorf("%w: intersection-point 边距吃掉了全部目标格", ErrBadParams)
	}
	cx, err := drawInt(entropy, int64(minCX), int64(maxCX))
	if err != nil {
		return intersectionSpec{}, nil, err
	}
	cy, err := drawInt(entropy, int64(minCY), int64(maxCY))
	if err != nil {
		return intersectionSpec{}, nil, err
	}
	seedBytes := make([]byte, 8)
	if _, err := io.ReadFull(entropy, seedBytes); err != nil {
		return intersectionSpec{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	var seed int64
	for _, b := range seedBytes {
		seed = seed<<8 | int64(b)
	}
	// 目标点是格子中心，保证规范化后的 point 再量化回同一格。
	tx := (int(cx)*intersectionW + intersectionW/2) / cols
	ty := (int(cy)*intersectionH + intersectionH/2) / rows
	return intersectionSpec{
		Cols: cols, Rows: rows, CellX: int(cx), CellY: int(cy),
		TargetX: tx, TargetY: ty, VisualSeed: seed,
	}, nonce, nil
}

func intersectionAnswer(spec intersectionSpec) string {
	// 取格中心对应的 0..999 坐标；Verify 用 floor(x*cols/1000) 能回到同一格。
	nx := ((spec.CellX*2 + 1) * 999) / (2 * spec.Cols)
	ny := ((spec.CellY*2 + 1) * 999) / (2 * spec.Rows)
	return strconv.Itoa(nx) + "," + strconv.Itoa(ny)
}

// IntersectionPointAnswerFrom 按与 Generate 相同的熵顺序取目标点（规范 point）。
func IntersectionPointAnswerFrom(entropy io.Reader, difficulty int) (string, error) {
	spec, _, err := buildIntersectionPoint(entropy, difficulty)
	if err != nil {
		return "", err
	}
	return intersectionAnswer(spec), nil
}

func (g IntersectionPoint) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	spec, nonce, err := buildIntersectionPoint(entropy, p.Difficulty)
	if err != nil {
		return Question{}, err
	}
	tags, err := NewTSVBuilder(g.macKey, nonce)
	if err != nil {
		return Question{}, err
	}
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			x, y := spec.CellX+dx, spec.CellY+dy
			if x < 0 || y < 0 || x >= spec.Cols || y >= spec.Rows {
				continue
			}
			if err := tags.Add(0, uint32(y*spec.Cols+x)); err != nil {
				return Question{}, err
			}
		}
	}
	tsv, err := tags.Build()
	if err != nil {
		return Question{}, err
	}
	ver := make([]byte, 8, 8+len(tsv))
	copy(ver[0:4], intersectionVerifierMagic)
	binary.BigEndian.PutUint16(ver[4:6], uint16(spec.Cols))
	binary.BigEndian.PutUint16(ver[6:8], uint16(spec.Rows))
	ver = append(ver, tsv...)
	asset, err := renderIntersection(spec)
	if err != nil {
		return Question{}, err
	}
	pub := IntersectionPointPublic{
		Kind: "intersection-point", AnswerType: AnswerTypePoint, MaxAnswerLen: intersectionMaxAnswerLen,
		ImageWidth: intersectionW, ImageHeight: intersectionH,
		PromptZH: "延长两条虚线，点击它们的交点",
		PromptEN: "Extend the two dashed lines and click where they meet",
	}
	return Question{Payload: pub, Verifier: PrivateVerifier(ver), Asset: asset, MimeType: MimeTypePNG}, nil
}

func (g IntersectionPoint) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	cols, rows, tsv, ok := parseIntersectionVerifier(v)
	if !ok {
		return false
	}
	parts := strings.Split(canon, ",")
	if len(parts) != 2 {
		return false
	}
	x, _ := strconv.Atoi(parts[0])
	y, _ := strconv.Atoi(parts[1])
	cx, cy := x*cols/1000, y*rows/1000
	if cx >= cols {
		cx = cols - 1
	}
	if cy >= rows {
		cy = rows - 1
	}
	return TSVContains(tsv, g.macKey, 0, uint32(cy*cols+cx))
}

func (IntersectionPoint) NormalizeAnswer(input string) (string, bool) {
	return NormalizePoint(input)
}

func (IntersectionPoint) SanitizePublic(raw []byte) (map[string]any, error) {
	var p IntersectionPointPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("generator: intersection-point 题面 JSON: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("generator: intersection-point 题面 JSON: %w", err)
	}
	switch {
	case p.Kind != "intersection-point" || p.AnswerType != AnswerTypePoint:
		return nil, fmt.Errorf("generator: intersection-point 题面类型不合法")
	case p.MaxAnswerLen != intersectionMaxAnswerLen:
		return nil, fmt.Errorf("generator: intersection-point 答案长度不合法")
	case p.ImageWidth != intersectionW || p.ImageHeight != intersectionH:
		return nil, fmt.Errorf("generator: intersection-point 图像尺寸不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: intersection-point 提示不合法")
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height", "prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: intersection-point 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

func parseIntersectionVerifier(v PrivateVerifier) (int, int, []byte, bool) {
	if len(v) < 8 || string(v[0:4]) != intersectionVerifierMagic {
		return 0, 0, nil, false
	}
	cols := int(binary.BigEndian.Uint16(v[4:6]))
	rows := int(binary.BigEndian.Uint16(v[6:8]))
	if cols < 1 || rows < 1 || cols*rows > 4096 {
		return 0, 0, nil, false
	}
	return cols, rows, v[8:], true
}

// intersectionLineEndpoints 把过交点的直线裁剪到画布内边距内：
// 端点因此总在画布内（不会被 setGrayBounded 悄悄丢掉，也不会让标记画到界外）。
func intersectionLineEndpoints(s intersectionSpec, m float64) (image.Point, image.Point) {
	const margin = 10
	tMin := float64(margin - s.TargetX)
	tMax := float64(intersectionW - 1 - margin - s.TargetX)
	ya := float64(margin-s.TargetY) / m
	yb := float64(intersectionH-1-margin-s.TargetY) / m
	if ya > yb {
		ya, yb = yb, ya
	}
	if ya > tMin {
		tMin = ya
	}
	if yb < tMax {
		tMax = yb
	}
	p0 := image.Pt(s.TargetX+int(math.Round(tMin)), s.TargetY+int(math.Round(m*tMin)))
	p1 := image.Pt(s.TargetX+int(math.Round(tMax)), s.TargetY+int(math.Round(m*tMax)))
	return p0, p1
}

// drawDashedLine 只画部分采样点。交点附近留空，避免把答案直接画出来；
// 虚线本身的间隙与留空区不可区分，因此画面不会指示交点位置。
func drawDashedLine(img *image.Gray, s intersectionSpec, p0, p1 image.Point, period, phase int) {
	steps := max(absInt(p1.X-p0.X), absInt(p1.Y-p0.Y))
	if steps == 0 {
		return
	}
	const dashLen = 18
	const clearR = 14
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x := int(math.Round(float64(p0.X) + t*float64(p1.X-p0.X)))
		y := int(math.Round(float64(p0.Y) + t*float64(p1.Y-p0.Y)))
		if (x+phase)%period >= dashLen {
			continue
		}
		dx, dy := x-s.TargetX, y-s.TargetY
		if dx*dx+dy*dy <= clearR*clearR {
			continue
		}
		drawSegment(img, image.Pt(x, y), image.Pt(x, y), 46)
	}
}

func renderIntersection(s intersectionSpec) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, intersectionW, intersectionH))
	fillGrayRect(img, 0, 0, intersectionW, intersectionH, 235)
	// 两条不同斜率的虚线；交点附近留空，用户必须外推。
	m1 := 0.55 + float64((s.VisualSeed>>2)&3)*0.12
	m2 := -0.55 - float64((s.VisualSeed>>4)&3)*0.12
	var ends [2][2]image.Point
	periods := [2]int{34, 46}
	phases := [2]int{int((s.VisualSeed >> 6) & 31), int((s.VisualSeed >> 11) & 31)}
	for i, m := range []float64{m1, m2} {
		p0, p1 := intersectionLineEndpoints(s, m)
		ends[i] = [2]image.Point{p0, p1}
		drawDashedLine(img, s, p0, p1, periods[i], phases[i])
	}
	// 端点小标记，强调这是一条“延长线”，不是自由曲线。
	for _, e := range ends {
		fillCircle(img, e[0].X, e[0].Y, 4, 20)
		fillCircle(img, e[1].X, e[1].Y, 4, 20)
	}
	rng := mrand.New(mrand.NewSource(s.VisualSeed))
	for i := 0; i < 24; i++ {
		x, y := rng.Intn(intersectionW), rng.Intn(intersectionH)
		if img.GrayAt(x, y).Y > 220 {
			img.SetGray(x, y, color.Gray{Y: uint8(225 + rng.Intn(8))})
		}
	}
	return encodeGrayPNG(img)
}
