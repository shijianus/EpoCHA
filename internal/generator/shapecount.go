package generator

// shape-count-v1：两类图形计数（ADR-024，DESIGN §8.7.2）。
//
// **定位**：图形与数量都直接可见，机器视觉可以数；M4 未测，因此它不是抗 AI 证明。
// 它交付的是"看图形并给出两个数字"的交互，以及一个 36 点答案空间（6×6），
// 3 次随机尝试的上界约 8.1%（<10% 红线）。
//
// 答案只由两个数量决定；图形类别、干扰图形与位置都不参与判分，
// verifier 与算术/文字题共用精确 HMAC 路径（48B）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	ShapeCountID      = "shape-count-v1"
	shapeCountVersion = "v1"

	ShapeCountWidth  = 480
	ShapeCountHeight = 240

	// 4×4 槽位：每槽 120×60，槽内抖动 ±3px；图形半径+描边+2 必须 ≤27，才不会跨槽重叠。
	shapeCountSlotCols = 4
	shapeCountSlotRows = 4
	shapeCountSlotW    = ShapeCountWidth / shapeCountSlotCols
	shapeCountSlotH    = ShapeCountHeight / shapeCountSlotRows
	shapeCountSlots    = shapeCountSlotCols * shapeCountSlotRows

	shapeCountMinN     = 1
	shapeCountMaxN     = 6
	shapeCountMaxDecoy = 2
	shapeCountMaxItems = 2*shapeCountMaxN + shapeCountMaxDecoy

	shapeCountMaxAnswerLen = 16
)

type shapeCountTier struct {
	Radius      float64
	Stroke      float64
	RotationDeg float64
	Noise       int
}

var shapeCountTiers = map[int]shapeCountTier{
	1: {Radius: 20, Stroke: 3.5, RotationDeg: 0, Noise: 40},
	2: {Radius: 20, Stroke: 3.5, RotationDeg: 4, Noise: 70},
	3: {Radius: 19.5, Stroke: 3.5, RotationDeg: 8, Noise: 100},
	4: {Radius: 19, Stroke: 3, RotationDeg: 12, Noise: 140},
	5: {Radius: 18.5, Stroke: 3, RotationDeg: 16, Noise: 180},
}

func (t shapeCountTier) validate() error {
	switch {
	case t.Radius < 10 || t.Radius > 24:
		return fmt.Errorf("%w: 图形半径 %v 越界", ErrBadParams, t.Radius)
	case t.Stroke < 1 || t.Stroke > 8:
		return fmt.Errorf("%w: 描边 %v 越界", ErrBadParams, t.Stroke)
	case t.Stroke*2 >= t.Radius:
		return fmt.Errorf("%w: 描边 %v 相对半径 %v 过粗", ErrBadParams, t.Stroke, t.Radius)
	case t.RotationDeg < 0 || t.RotationDeg > 30:
		return fmt.Errorf("%w: 旋转 %v° 越界", ErrBadParams, t.RotationDeg)
	case t.Noise < 0 || t.Noise > ShapeCountWidth*ShapeCountHeight/maxNoiseDivisor:
		return fmt.Errorf("%w: 噪点 %d 越界", ErrBadParams, t.Noise)
	}
	// 槽高 60 ⇒ 半槽 30；抖动 ±3；图形 pad 必须 ≤27。
	if t.Radius+t.Stroke+3+2 > shapeCountSlotH/2 {
		return fmt.Errorf("%w: 半径 %v + 描边 %v 会跨槽重叠", ErrBadParams, t.Radius, t.Stroke)
	}
	return nil
}

// ShapeCountParams 返回某个难度档的渲染参数（预览/文档用）。
func ShapeCountParams(difficulty int) (shapeCountTier, error) {
	t, ok := shapeCountTiers[difficulty]
	if !ok {
		return shapeCountTier{}, fmt.Errorf("%w: 难度 %d 没有对应的 shape-count 档位", ErrBadParams, difficulty)
	}
	return t, nil
}

// ShapeCountPublic 是唯一公开题面：告诉客户端"问的是哪两类、数量范围、怎么输入"，
// 不告诉它数量本身。
type ShapeCountPublic struct {
	AnswerType   string   `json:"answer_type"`
	MaxAnswerLen int      `json:"max_answer_len"`
	ImageWidth   int      `json:"image_width"`
	ImageHeight  int      `json:"image_height"`
	LabelsZH     []string `json:"labels_zh"`
	LabelsEN     []string `json:"labels_en"`
	Min          int      `json:"min"`
	Max          int      `json:"max"`
	PromptZH     string   `json:"prompt_zh"`
	PromptEN     string   `json:"prompt_en"`
}

func (ShapeCountPublic) PublicKind() string { return "shape-count" }

// ShapeCount 实现 Generator。不可变字段，可并发调用。
type ShapeCount struct {
	macKey []byte
}

var _ Generator = ShapeCount{}

func NewShapeCount(macKey []byte) (ShapeCount, error) {
	if len(macKey) < 32 {
		return ShapeCount{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		t, ok := shapeCountTiers[d]
		if !ok {
			return ShapeCount{}, fmt.Errorf("%w: shape-count 缺少难度 %d 的档位", ErrBadParams, d)
		}
		if err := t.validate(); err != nil {
			return ShapeCount{}, err
		}
	}
	return ShapeCount{macKey: macKey}, nil
}

func (ShapeCount) ID() string      { return ShapeCountID }
func (ShapeCount) Version() string { return shapeCountVersion }
func (ShapeCount) Media() string   { return MediaImage }

// countSpec 是全部**答案决定量**：两类目标与两个数量。
type countSpec struct {
	catA, catB shapeCategory
	a, b       int
}

func (s countSpec) answer() string { return strconv.Itoa(s.a) + "," + strconv.Itoa(s.b) }

func (s countSpec) prompts() (zh, en string) {
	return fmt.Sprintf("数一数：图中有多少个%s？多少个%s？", s.catA.labelZH(), s.catB.labelZH()),
		fmt.Sprintf("Count the %s and the %s in the image.", s.catA.labelEN(), s.catB.labelEN())
}

// drawCountSpec 按固定顺序消费熵流；Generate 与 ShapeCountAnswerFrom 必须走同一条路径。
func drawCountSpec(entropy io.Reader) (countSpec, error) {
	var s countSpec
	cats := allCats
	i, err := drawInt(entropy, 0, int64(len(cats)-1))
	if err != nil {
		return s, err
	}
	j, err := drawInt(entropy, 0, int64(len(cats)-2))
	if err != nil {
		return s, err
	}
	if int(j) >= int(i) {
		j++
	}
	s.catA, s.catB = cats[i], cats[j]

	a, err := drawInt(entropy, shapeCountMinN, shapeCountMaxN)
	if err != nil {
		return s, err
	}
	b, err := drawInt(entropy, shapeCountMinN, shapeCountMaxN)
	if err != nil {
		return s, err
	}
	s.a, s.b = int(a), int(b)
	return s, nil
}

// ShapeCountAnswerFrom 是离线评测入口；与生产 Generate 使用同一条抽样顺序。
func ShapeCountAnswerFrom(entropy io.Reader) (string, error) {
	spec, err := drawCountSpec(entropy)
	if err != nil {
		return "", err
	}
	return spec.answer(), nil
}

// Generate 出一道计数题。答案只存在于 verifier 的 HMAC 里。
func (g ShapeCount) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	tier, ok := shapeCountTiers[p.Difficulty]
	if !ok {
		return Question{}, fmt.Errorf("%w: 难度 %d 没有对应的 shape-count 档位", ErrBadParams, p.Difficulty)
	}
	if err := tier.validate(); err != nil {
		return Question{}, err
	}

	spec, err := drawCountSpec(entropy)
	if err != nil {
		return Question{}, err
	}
	// 干扰图形：不属于两类目标，数量 0..2。它不参与判分，但会让"只数两种"这件事需要真的看题。
	d, err := drawInt(entropy, 0, shapeCountMaxDecoy)
	if err != nil {
		return Question{}, err
	}
	allowedDecoys := make([]shapeCategory, 0, len(allCats))
	for _, cat := range allCats {
		if cat != spec.catA && cat != spec.catB {
			allowedDecoys = append(allowedDecoys, cat)
		}
	}
	decoys := make([]shapeCategory, 0, int(d))
	for i := int64(0); i < d; i++ {
		idx, err := drawInt(entropy, 0, int64(len(allowedDecoys)-1))
		if err != nil {
			return Question{}, err
		}
		decoys = append(decoys, allowedDecoys[idx])
	}

	nonce := make([]byte, verifierNonceSz)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	seed := make([]byte, jitterSeedBytes)
	if _, err := io.ReadFull(entropy, seed); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	rng := newShapeJitter(seed)
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}

	items := make([]shapeDraw, 0, shapeCountMaxItems)
	add := func(cat shapeCategory, n int) {
		for i := 0; i < n; i++ {
			items = append(items, shapeDraw{Cat: cat})
		}
	}
	add(spec.catA, spec.a)
	add(spec.catB, spec.b)
	for _, cat := range decoys {
		items = append(items, shapeDraw{Cat: cat})
	}
	if len(items) > shapeCountSlots {
		// 规范上不可能发生（6+6+2=14 ≤16）；这里是防御性 fail closed，不是补位逻辑。
		return Question{}, fmt.Errorf("%w: 图形数 %d 超过槽位 %d", ErrBadParams, len(items), shapeCountSlots)
	}

	slots := make([]int, shapeCountSlots)
	for i := range slots {
		slots[i] = i
	}
	rng.Shuffle(len(slots), func(i, j int) { slots[i], slots[j] = slots[j], slots[i] })
	for i := range items {
		slot := slots[i]
		col, row := slot%shapeCountSlotCols, slot/shapeCountSlotCols
		cx := float64(col*shapeCountSlotW + shapeCountSlotW/2 + rng.Intn(7) - 3)
		cy := float64(row*shapeCountSlotH + shapeCountSlotH/2 + rng.Intn(7) - 3)
		items[i].Cx, items[i].Cy = cx, cy
		items[i].Radius = tier.Radius + rng.Float64()*3 - 1.5
		items[i].Rotation = angleRadDeg(tier.RotationDeg, rng)
		items[i].Stroke = tier.Stroke
	}
	asset, err := renderShapeScene(ShapeCountWidth, ShapeCountHeight, tier.Noise, items, rng)
	if err != nil {
		return Question{}, err
	}
	zh, en := spec.prompts()
	return Question{
		Payload: ShapeCountPublic{
			AnswerType:   "pairs",
			MaxAnswerLen: shapeCountMaxAnswerLen,
			ImageWidth:   ShapeCountWidth,
			ImageHeight:  ShapeCountHeight,
			LabelsZH:     []string{spec.catA.labelZH(), spec.catB.labelZH()},
			LabelsEN:     []string{spec.catA.labelEN(), spec.catB.labelEN()},
			Min:          shapeCountMinN,
			Max:          shapeCountMaxN,
			PromptZH:     zh,
			PromptEN:     en,
		},
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, spec.answer())),
		Asset:    asset,
		MimeType: MimeTypePNG,
	}, nil
}

// NormalizeAnswer：恰好两个 ASCII 十进制数（各 1..2 位），逗号分隔，剥前导零。
// 负数、小数、空字段、多余字段一律**畸形**。
func (ShapeCount) NormalizeAnswer(input string) (string, bool) {
	s := trimASCII(input)
	if s == "" || len(s) > shapeCountMaxAnswerLen {
		return "", false
	}
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return "", false
	}
	out := make([]string, 2)
	for i, raw := range parts {
		p := trimASCII(raw)
		if p == "" || len(p) > 2 {
			return "", false
		}
		n := 0
		for j := 0; j < len(p); j++ {
			if p[j] < '0' || p[j] > '9' {
				return "", false
			}
			n = n*10 + int(p[j]-'0')
		}
		if n > 99 {
			return "", false
		}
		out[i] = strconv.Itoa(n)
	}
	return out[0] + "," + out[1], true
}

func (g ShapeCount) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

// SanitizePublic 是出网唯一通道：白名单之外任何键都报错。
func (ShapeCount) SanitizePublic(raw []byte) (map[string]any, error) {
	var p ShapeCountPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch {
	case p.AnswerType != "pairs":
		return nil, fmt.Errorf("generator: shape-count 的 answer_type=%q 不合法", p.AnswerType)
	case p.MaxAnswerLen < 1 || p.MaxAnswerLen > 64:
		return nil, fmt.Errorf("generator: shape-count 的 max_answer_len=%d 越界", p.MaxAnswerLen)
	case p.ImageWidth != ShapeCountWidth || p.ImageHeight != ShapeCountHeight:
		return nil, fmt.Errorf("generator: shape-count 的图像尺寸 %dx%d 与档位不符", p.ImageWidth, p.ImageHeight)
	case len(p.LabelsZH) != 2 || len(p.LabelsEN) != 2:
		return nil, errors.New("generator: shape-count 必须恰好两个标签")
	case p.Min < 0 || p.Max > 99 || p.Min >= p.Max:
		return nil, fmt.Errorf("generator: shape-count 的数量范围 %d..%d 不合法", p.Min, p.Max)
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, errors.New("generator: shape-count 的提示文本不合法")
	}
	for _, label := range append(append([]string{}, p.LabelsZH...), p.LabelsEN...) {
		if label == "" || len(label) > 64 {
			return nil, errors.New("generator: shape-count 的标签不合法")
		}
	}
	for k := range generic {
		switch k {
		case "answer_type", "max_answer_len", "image_width", "image_height",
			"labels_zh", "labels_en", "min", "max", "prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: shape-count 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind":           p.PublicKind(),
		"answer_type":    p.AnswerType,
		"max_answer_len": p.MaxAnswerLen,
		"image_width":    p.ImageWidth,
		"image_height":   p.ImageHeight,
		"labels_zh":      p.LabelsZH,
		"labels_en":      p.LabelsEN,
		"min":            p.Min,
		"max":            p.Max,
		"prompt_zh":      p.PromptZH,
		"prompt_en":      p.PromptEN,
	}, nil
}
