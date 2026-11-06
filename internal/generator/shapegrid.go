package generator

// shape-grid-v1：3×3 图形点选（ADR-024，DESIGN §8.7.1）。
//
// **定位**：这是非纯文字输入的图形题，不是抗 AI/抗 OCR 的证明。判据（种类/填充/组合）
// 与图形都直接可见，任何能做形状分类的脚本都能解；M4 未测，管理面必须保持
// experimental:true + production_ready:false。它的价值是交互形态（点选）与题型多样性。
//
// 答案只由两个量决定：k（2 或 3）与被选中的格子集合。答案编码是 1-based 格子序号的
// 排序逗号串；verifier 与算术/文字题共用 generator.go 的精确 HMAC 路径（48B），
// 没有坐标容差，也没有新的密码学构造。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"strconv"
	"strings"
)

const (
	ShapeGridID      = "shape-grid-v1"
	shapeGridVersion = "v1"

	ShapeGridCells = 9
	shapeGridCols  = 3
	shapeGridRows  = 3

	ShapeGridWidth  = 340
	ShapeGridHeight = 340
	shapeGridMargin = 12
	shapeGridGap    = 8
	shapeGridCell   = 100

	// shapeGridMinK/MaxK：k=1 时答案空间只有 9，3 次尝试 33% ⇒ 超过 10% 红线；
	// k=2/3 的 C(9,k) 是 36/84，最坏 3 次上界 ≈8.1%。生成器只允许这两档。
	shapeGridMinK = 2
	shapeGridMaxK = 3

	shapeGridMaxAnswerLen = 32
)

type shapeGridTier struct {
	Radius      float64
	Stroke      float64
	RotationDeg float64
	Noise       int
}

var shapeGridTiers = map[int]shapeGridTier{
	1: {Radius: 30, Stroke: 4, RotationDeg: 0, Noise: 0},
	2: {Radius: 29, Stroke: 4, RotationDeg: 4, Noise: 30},
	3: {Radius: 28, Stroke: 4, RotationDeg: 8, Noise: 60},
	4: {Radius: 27, Stroke: 4, RotationDeg: 12, Noise: 100},
	5: {Radius: 26, Stroke: 3.5, RotationDeg: 16, Noise: 140},
}

func (t shapeGridTier) validate() error {
	switch {
	case t.Radius < 12 || t.Radius > 40:
		return fmt.Errorf("%w: 图形半径 %v 越界", ErrBadParams, t.Radius)
	case t.Stroke < 1 || t.Stroke > 8:
		return fmt.Errorf("%w: 描边 %v 越界", ErrBadParams, t.Stroke)
	case t.Stroke*2 >= t.Radius:
		return fmt.Errorf("%w: 描边 %v 相对半径 %v 过粗（空心会退化）", ErrBadParams, t.Stroke, t.Radius)
	case t.RotationDeg < 0 || t.RotationDeg > 30:
		return fmt.Errorf("%w: 旋转 %v° 越界", ErrBadParams, t.RotationDeg)
	case t.Noise < 0 || t.Noise > ShapeGridWidth*ShapeGridHeight/maxNoiseDivisor:
		return fmt.Errorf("%w: 噪点 %d 越界", ErrBadParams, t.Noise)
	}
	// 图形必须完整落在格子里：格 100px，半格 50px；留 2px 边距。
	if t.Radius+t.Stroke+2 > shapeGridCell/2 {
		return fmt.Errorf("%w: 半径 %v + 描边 %v 放不进格子", ErrBadParams, t.Radius, t.Stroke)
	}
	return nil
}

// ShapeGridParams 返回某个难度档的渲染参数（预览/文档用）。
func ShapeGridParams(difficulty int) (shapeGridTier, error) {
	t, ok := shapeGridTiers[difficulty]
	if !ok {
		return shapeGridTier{}, fmt.Errorf("%w: 难度 %d 没有对应的 shape-grid 档位", ErrBadParams, difficulty)
	}
	return t, nil
}

// ShapeGridPublic 是唯一公开题面：字段即白名单。
// 题目只告诉客户端"怎么点"和"网格在哪"，不告诉它哪些格子正确。
type ShapeGridPublic struct {
	AnswerType   string `json:"answer_type"`
	MaxAnswerLen int    `json:"max_answer_len"`
	ImageWidth   int    `json:"image_width"`
	ImageHeight  int    `json:"image_height"`
	GridCols     int    `json:"grid_cols"`
	GridRows     int    `json:"grid_rows"`
	GridX        int    `json:"grid_x"`
	GridY        int    `json:"grid_y"`
	GridCell     int    `json:"grid_cell"`
	GridGap      int    `json:"grid_gap"`
	PromptZH     string `json:"prompt_zh"`
	PromptEN     string `json:"prompt_en"`
}

func (ShapeGridPublic) PublicKind() string { return "shape-grid" }

// ShapeGrid 实现 Generator。不可变字段，可并发调用。
type ShapeGrid struct {
	macKey []byte
}

var _ Generator = ShapeGrid{}

func NewShapeGrid(macKey []byte) (ShapeGrid, error) {
	if len(macKey) < 32 {
		return ShapeGrid{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		t, ok := shapeGridTiers[d]
		if !ok {
			return ShapeGrid{}, fmt.Errorf("%w: shape-grid 缺少难度 %d 的档位", ErrBadParams, d)
		}
		if err := t.validate(); err != nil {
			return ShapeGrid{}, err
		}
	}
	return ShapeGrid{macKey: macKey}, nil
}

func (ShapeGrid) ID() string      { return ShapeGridID }
func (ShapeGrid) Version() string { return shapeGridVersion }
func (ShapeGrid) Media() string   { return MediaImage }

// --- 题目规格与抽样 ---

type gridCriterionMode uint8

const (
	gridByKind gridCriterionMode = iota
	gridByFill
	gridByCategory
)

// gridSpec 是全部**答案决定量**：k、判据与每个格子的类别。渲染参数不在里面。
type gridSpec struct {
	k     int
	mode  gridCriterionMode
	cat   shapeCategory // mode=kind 时只用 kind；mode=fill 时只用 filled
	cells []int         // 目标格（0-based，升序）
	cats  [ShapeGridCells]shapeCategory
}

func (s gridSpec) answer() string {
	parts := make([]string, len(s.cells))
	for i, c := range s.cells {
		parts[i] = strconv.Itoa(c + 1)
	}
	return strings.Join(parts, ",")
}

func (s gridSpec) matches(c shapeCategory) bool {
	switch s.mode {
	case gridByKind:
		return c.kind == s.cat.kind
	case gridByFill:
		return c.filled == s.cat.filled
	case gridByCategory:
		return c.kind == s.cat.kind && c.filled == s.cat.filled
	default:
		return false
	}
}

func (s gridSpec) prompts() (zh, en string) {
	switch s.mode {
	case gridByKind:
		return fmt.Sprintf("选出图中所有的%s（不论实心或空心）。", s.cat.kind.labelZH()),
			fmt.Sprintf("Select every %s, filled or hollow.", s.cat.kind.labelEN())
	case gridByFill:
		if s.cat.filled {
			return "选出图中所有实心图形（不论形状）。", "Select every filled shape, whatever its kind."
		}
		return "选出图中所有空心图形（不论形状）。", "Select every hollow shape, whatever its kind."
	default:
		return fmt.Sprintf("选出图中所有的%s。", s.cat.labelZH()),
			fmt.Sprintf("Select every %s.", s.cat.labelEN())
	}
}

// drawGridSpec 按固定顺序消费熵流。Generate 与 ShapeGridAnswerFrom 必须走同一条路径，
// 否则离线评测的 ground truth 与生产题目不是同一个答案。
func drawGridSpec(entropy io.Reader) (gridSpec, error) {
	var s gridSpec

	k, err := drawInt(entropy, shapeGridMinK, shapeGridMaxK)
	if err != nil {
		return s, err
	}
	s.k = int(k)

	mode, err := drawInt(entropy, 0, 2)
	if err != nil {
		return s, err
	}
	s.mode = gridCriterionMode(mode)

	kindIdx, err := drawInt(entropy, 0, int64(len(shapeKindOrder)-1))
	if err != nil {
		return s, err
	}
	targetKind := shapeKindOrder[kindIdx]
	filled, err := drawInt(entropy, 0, 1)
	if err != nil {
		return s, err
	}
	s.cat = shapeCategory{kind: targetKind, filled: filled == 1}

	// 目标格：对 0..8 做 Fisher–Yates，前 k 个就是答案。
	perm := [ShapeGridCells]int{}
	for i := range perm {
		perm[i] = i
	}
	for i := 0; i < ShapeGridCells; i++ {
		j, err := drawInt(entropy, int64(i), int64(ShapeGridCells-1))
		if err != nil {
			return s, err
		}
		perm[i], perm[int(j)] = perm[int(j)], perm[i]
	}
	target := make([]bool, ShapeGridCells)
	for _, c := range perm[:s.k] {
		target[c] = true
	}
	for c := 0; c < ShapeGridCells; c++ {
		if target[c] {
			s.cells = append(s.cells, c)
		}
	}

	// 每个格子的类别。目标格按判据给出确定匹配的类别；干扰格从**不可能匹配**的类别里抽。
	allowed := make([]shapeCategory, 0, len(allCats))
	for c := 0; c < ShapeGridCells; c++ {
		if target[c] {
			cat, err := s.targetCategory(entropy)
			if err != nil {
				return s, err
			}
			s.cats[c] = cat
			continue
		}
		allowed = allowed[:0]
		for _, cat := range allCats {
			if !s.matches(cat) {
				allowed = append(allowed, cat)
			}
		}
		idx, err := drawInt(entropy, 0, int64(len(allowed)-1))
		if err != nil {
			return s, err
		}
		s.cats[c] = allowed[idx]
	}
	return s, nil
}

var allCats = allShapeCategories()

// targetCategory 给目标格抽一个"满足判据"的类别。mode=kind 时填充随机（"所有三角形"
// 包含实心与空心）；mode=fill 时种类随机；mode=category 时就是那一个类别。
func (s gridSpec) targetCategory(entropy io.Reader) (shapeCategory, error) {
	switch s.mode {
	case gridByKind:
		f, err := drawInt(entropy, 0, 1)
		if err != nil {
			return shapeCategory{}, err
		}
		return shapeCategory{kind: s.cat.kind, filled: f == 1}, nil
	case gridByFill:
		k, err := drawInt(entropy, 0, int64(len(shapeKindOrder)-1))
		if err != nil {
			return shapeCategory{}, err
		}
		return shapeCategory{kind: shapeKindOrder[k], filled: s.cat.filled}, nil
	default:
		return s.cat, nil
	}
}

// ShapeGridAnswerFrom 是离线评测入口：与生产 Generate 使用同一条抽样顺序取答案。
// 它不读库、不读 verifier、不接受任何"答案查询"，只消费调用方自己的熵流。
func ShapeGridAnswerFrom(entropy io.Reader) (string, error) {
	spec, err := drawGridSpec(entropy)
	if err != nil {
		return "", err
	}
	return spec.answer(), nil
}

// Generate 出一道图形点选题。答案只存在于 verifier 的 HMAC 里。
func (g ShapeGrid) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	tier, ok := shapeGridTiers[p.Difficulty]
	if !ok {
		return Question{}, fmt.Errorf("%w: 难度 %d 没有对应的 shape-grid 档位", ErrBadParams, p.Difficulty)
	}
	if err := tier.validate(); err != nil {
		return Question{}, err
	}

	spec, err := drawGridSpec(entropy)
	if err != nil {
		return Question{}, err
	}
	nonce := make([]byte, verifierNonceSz)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	// 旋转/大小抖动只改题面，不参与判分 ⇒ 每次独立播种的 math/rand 足够（红线 10）。
	seed := make([]byte, jitterSeedBytes)
	if _, err := io.ReadFull(entropy, seed); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	rng := newShapeJitter(seed)
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}

	draws := make([]shapeDraw, 0, ShapeGridCells)
	for c := 0; c < ShapeGridCells; c++ {
		col, row := c%shapeGridCols, c/shapeGridCols
		cx := float64(shapeGridMargin + col*(shapeGridCell+shapeGridGap) + shapeGridCell/2)
		cy := float64(shapeGridMargin + row*(shapeGridCell+shapeGridGap) + shapeGridCell/2)
		draws = append(draws, shapeDraw{
			Cat:      spec.cats[c],
			Cx:       cx,
			Cy:       cy,
			Radius:   tier.Radius + (rng.Float64()*4 - 2),
			Rotation: angleRadDeg(tier.RotationDeg, rng),
			Stroke:   tier.Stroke,
		})
	}
	asset, err := renderShapeScene(ShapeGridWidth, ShapeGridHeight, tier.Noise, draws, rng)
	if err != nil {
		return Question{}, err
	}
	zh, en := spec.prompts()
	return Question{
		Payload: ShapeGridPublic{
			AnswerType:   "cells",
			MaxAnswerLen: shapeGridMaxAnswerLen,
			ImageWidth:   ShapeGridWidth,
			ImageHeight:  ShapeGridHeight,
			GridCols:     shapeGridCols,
			GridRows:     shapeGridRows,
			GridX:        shapeGridMargin,
			GridY:        shapeGridMargin,
			GridCell:     shapeGridCell,
			GridGap:      shapeGridGap,
			PromptZH:     zh,
			PromptEN:     en,
		},
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, spec.answer())),
		Asset:    asset,
		MimeType: MimeTypePNG,
	}, nil
}

// NormalizeAnswer：逗号分隔的 1-based 格子序号；排序、去重，不扩大接受域。
// 空串/0/越界/非十进制一律**畸形**（不消耗作答机会）。
func (ShapeGrid) NormalizeAnswer(input string) (string, bool) {
	return NormalizeCellSet(input, ShapeGridCells, shapeGridMaxAnswerLen)
}

func (g ShapeGrid) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

// SanitizePublic 是出网唯一通道：白名单之外任何键都报错。
func (ShapeGrid) SanitizePublic(raw []byte) (map[string]any, error) {
	var p ShapeGridPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch {
	case p.AnswerType != "cells":
		return nil, fmt.Errorf("generator: shape-grid 的 answer_type=%q 不合法", p.AnswerType)
	case p.MaxAnswerLen < 1 || p.MaxAnswerLen > 64:
		return nil, fmt.Errorf("generator: shape-grid 的 max_answer_len=%d 越界", p.MaxAnswerLen)
	case p.ImageWidth != ShapeGridWidth || p.ImageHeight != ShapeGridHeight:
		return nil, fmt.Errorf("generator: shape-grid 的图像尺寸 %dx%d 与档位不符", p.ImageWidth, p.ImageHeight)
	case p.GridCols != shapeGridCols || p.GridRows != shapeGridRows:
		return nil, fmt.Errorf("generator: shape-grid 的网格 %dx%d 与档位不符", p.GridCols, p.GridRows)
	case p.GridX != shapeGridMargin || p.GridY != shapeGridMargin:
		return nil, fmt.Errorf("generator: shape-grid 的网格原点 (%d,%d) 与档位不符", p.GridX, p.GridY)
	case p.GridCell != shapeGridCell || p.GridGap != shapeGridGap:
		return nil, fmt.Errorf("generator: shape-grid 的格/间距 (%d,%d) 与档位不符", p.GridCell, p.GridGap)
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, errors.New("generator: shape-grid 的提示文本不合法")
	}
	for k := range generic {
		switch k {
		case "answer_type", "max_answer_len", "image_width", "image_height",
			"grid_cols", "grid_rows", "grid_x", "grid_y", "grid_cell", "grid_gap",
			"prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: shape-grid 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind":           p.PublicKind(),
		"answer_type":    p.AnswerType,
		"max_answer_len": p.MaxAnswerLen,
		"image_width":    p.ImageWidth,
		"image_height":   p.ImageHeight,
		"grid_cols":      p.GridCols,
		"grid_rows":      p.GridRows,
		"grid_x":         p.GridX,
		"grid_y":         p.GridY,
		"grid_cell":      p.GridCell,
		"grid_gap":       p.GridGap,
		"prompt_zh":      p.PromptZH,
		"prompt_en":      p.PromptEN,
	}, nil
}

// newShapeJitter 用 CSPRNG 种子建一个独立的 math/rand 实例（不共享）。
// 图形抖动不参与判分，因此这里用 math/rand 是红线 10 允许的那一类。
func newShapeJitter(seed []byte) *mrand.Rand {
	var v uint64
	for i := 0; i < len(seed) && i < 8; i++ {
		v = v<<8 | uint64(seed[i])
	}
	return mrand.New(mrand.NewSource(int64(v)))
}
