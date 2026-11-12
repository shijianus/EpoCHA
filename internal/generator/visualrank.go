package generator

// visual-rank-v1：按路径线段数量从少到多排列 5 张卡（2026-11-12，ADR-030）。
//
// 每张卡画一条粗折线，线段数量取自 5 个不同值，排序后相邻值相差 ≥2。卡片用公开随机
// 字母 ID（A–E 的随机排列）标识，答案只提交 ID 的排列，不提交视觉数值；因此不存在
// 「数值格式不同」的歧义。payload 不含 segment count。
//
// 判分是 5! 个排列中的唯一一个，走 48B 精确 HMAC；随机猜测单次 1/120。

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"sort"
	"strings"
)

const (
	VisualRankID      = "visual-rank-v1"
	visualRankVersion = "v1"

	visualRankCards = 5

	VisualRankWidth  = 500
	VisualRankHeight = 280

	visualRankCardW = 150
	visualRankCardH = 120
	visualRankGap   = 10
	visualRankMask  = 12

	visualRankCols = 5
	visualRankRows = 4
	visualRankNode = 30

	visualRankMaxAnswerLen = 16
	visualRankMaxAttempts  = 64
	visualRankNodeCap      = 1 << 16
)

type visualRankCard struct {
	ID       string
	Segments int
	Points   []image.Point
	X, Y     int
	W, H     int
}

type visualRankSpec struct {
	Cards [visualRankCards]visualRankCard
	Order []string
}

func rankAnswer(spec visualRankSpec) string {
	return VersionPrefix + strings.Join(spec.Order, ".")
}

func buildVisualRank(entropy io.Reader) (visualRankSpec, error) {
	var lastErr error
	for attempt := 0; attempt < visualRankMaxAttempts; attempt++ {
		spec, err := buildVisualRankOnce(entropy)
		if err == nil {
			return spec, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrGenerateExhausted
	}
	return visualRankSpec{}, fmt.Errorf("%w: visual-rank 在 %d 次尝试后失败: %v",
		ErrGenerateExhausted, visualRankMaxAttempts, lastErr)
}

func buildVisualRankOnce(entropy io.Reader) (visualRankSpec, error) {
	var spec visualRankSpec
	start, err := drawInt(entropy, 1, 4)
	if err != nil {
		return spec, err
	}
	// 5 个线段数：start, start+2, ..., start+8；相邻差恒为 2（≥2）。
	counts := make([]int, visualRankCards)
	for i := range counts {
		counts[i] = int(start) + 2*i
	}
	ids := []string{"A", "B", "C", "D", "E"}
	for i := len(counts) - 1; i > 0; i-- {
		j, err := drawInt(entropy, 0, int64(i))
		if err != nil {
			return spec, err
		}
		counts[i], counts[j] = counts[j], counts[i]
	}
	for i := len(ids) - 1; i > 0; i-- {
		j, err := drawInt(entropy, 0, int64(i))
		if err != nil {
			return spec, err
		}
		ids[i], ids[j] = ids[j], ids[i]
	}
	slots := []image.Rectangle{
		image.Rect(visualRankMask, visualRankMask, visualRankMask+visualRankCardW, visualRankMask+visualRankCardH),
		image.Rect(visualRankMask+visualRankCardW+visualRankGap, visualRankMask, visualRankMask+2*visualRankCardW+visualRankGap, visualRankMask+visualRankCardH),
		image.Rect(visualRankMask+2*(visualRankCardW+visualRankGap), visualRankMask, visualRankMask+3*visualRankCardW+2*visualRankGap, visualRankMask+visualRankCardH),
	}
	// 第二行两张居中。
	bottomY := visualRankMask + visualRankCardH + visualRankGap
	bottomX := (VisualRankWidth - (2*visualRankCardW + visualRankGap)) / 2
	slots = append(slots,
		image.Rect(bottomX, bottomY, bottomX+visualRankCardW, bottomY+visualRankCardH),
		image.Rect(bottomX+visualRankCardW+visualRankGap, bottomY, bottomX+2*visualRankCardW+visualRankGap, bottomY+visualRankCardH),
	)
	// 卡序随机：把 (id,count) 先洗牌，再放进固定的显示槽。
	order := make([]int, visualRankCards)
	for i := range order {
		order[i] = i
	}
	for i := len(order) - 1; i > 0; i-- {
		j, err := drawInt(entropy, 0, int64(i))
		if err != nil {
			return spec, err
		}
		order[i], order[j] = order[j], order[i]
	}
	for i := 0; i < visualRankCards; i++ {
		k := order[i]
		pts, err := drawRankPolyline(entropy, counts[k])
		if err != nil {
			return spec, err
		}
		spec.Cards[i] = visualRankCard{
			ID: ids[k], Segments: counts[k], Points: pts,
			X: slots[i].Min.X, Y: slots[i].Min.Y, W: slots[i].Dx(), H: slots[i].Dy(),
		}
	}
	sorted := append([]visualRankCard(nil), spec.Cards[:]...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Segments < sorted[j].Segments })
	for _, c := range sorted {
		spec.Order = append(spec.Order, c.ID)
	}
	return spec, nil
}

// drawRankPolyline 在 5×4 节点栅格上找一条恰好 segments 段的简单折线（不重复节点）。
func drawRankPolyline(entropy io.Reader, segments int) ([]image.Point, error) {
	if segments < 1 || segments > visualRankCols*visualRankRows-1 {
		return nil, fmt.Errorf("%w: 线段数 %d", ErrBadParams, segments)
	}
	nodes := visualRankCols * visualRankRows
	for attempt := 0; attempt < 32; attempt++ {
		start, err := drawInt(entropy, 0, int64(nodes-1))
		if err != nil {
			return nil, err
		}
		used := make([]bool, nodes)
		path := []int{int(start)}
		used[start] = true
		budget := visualRankNodeCap
		var walk func() bool
		walk = func() bool {
			if len(path) == segments+1 {
				return true
			}
			if budget <= 0 {
				return false
			}
			cur := path[len(path)-1]
			r, c := cur/visualRankCols, cur%visualRankCols
			opts := make([]int, 0, 4)
			for _, d := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
				nr, nc := r+d[0], c+d[1]
				if nr < 0 || nr >= visualRankRows || nc < 0 || nc >= visualRankCols {
					continue
				}
				ni := nr*visualRankCols + nc
				if !used[ni] {
					opts = append(opts, ni)
				}
			}
			for i := 0; i < len(opts); i++ {
				j, err := drawInt(entropy, int64(i), int64(len(opts)-1))
				if err != nil {
					budget = 0
					return false
				}
				opts[i], opts[j] = opts[j], opts[i]
			}
			for _, ni := range opts {
				budget--
				used[ni] = true
				path = append(path, ni)
				if walk() {
					return true
				}
				path = path[:len(path)-1]
				used[ni] = false
			}
			return false
		}
		if walk() {
			pts := make([]image.Point, len(path))
			for i, n := range path {
				r, c := n/visualRankCols, n%visualRankCols
				pts[i] = image.Pt(c*visualRankNode, r*visualRankNode)
			}
			return pts, nil
		}
	}
	return nil, fmt.Errorf("%w: 无法生成 %d 段折线", ErrGenerateExhausted, segments)
}

type visualRankCardPublic struct {
	ID string `json:"id"`
	X  int    `json:"x"`
	Y  int    `json:"y"`
	W  int    `json:"w"`
	H  int    `json:"h"`
}

type VisualRankPublic struct {
	Kind         string                 `json:"kind"`
	AnswerType   string                 `json:"answer_type"`
	MaxAnswerLen int                    `json:"max_answer_len"`
	ImageWidth   int                    `json:"image_width"`
	ImageHeight  int                    `json:"image_height"`
	Cards        []visualRankCardPublic `json:"cards"`
	InputMode    string                 `json:"input_mode"`
	PromptZH     string                 `json:"prompt_zh"`
	PromptEN     string                 `json:"prompt_en"`
}

func (VisualRankPublic) PublicKind() string { return "visual-rank" }

type VisualRank struct{ macKey []byte }

var _ Generator = VisualRank{}

func NewVisualRank(macKey []byte) (VisualRank, error) {
	if len(macKey) < 32 {
		return VisualRank{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	return VisualRank{macKey: macKey}, nil
}

func (VisualRank) ID() string      { return VisualRankID }
func (VisualRank) Version() string { return visualRankVersion }
func (VisualRank) Media() string   { return MediaImage }

func (g VisualRank) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	spec, err := buildVisualRank(entropy)
	if err != nil {
		return Question{}, err
	}
	nonce := make([]byte, verifierNonceSz)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	asset, err := renderVisualRank(spec)
	if err != nil {
		return Question{}, err
	}
	cards := make([]visualRankCardPublic, visualRankCards)
	for i, c := range spec.Cards {
		cards[i] = visualRankCardPublic{ID: c.ID, X: c.X, Y: c.Y, W: c.W, H: c.H}
	}
	return Question{
		Payload: VisualRankPublic{
			Kind: "visual-rank", AnswerType: AnswerTypeOrder, MaxAnswerLen: visualRankMaxAnswerLen,
			ImageWidth: VisualRankWidth, ImageHeight: VisualRankHeight,
			Cards: cards, InputMode: "rank",
			PromptZH: "按路径包含的线段数量，从少到多依次选择五张卡片。",
			PromptEN: "Select the five cards in order from the fewest to the most line segments.",
		},
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, rankAnswer(spec))),
		Asset:    asset, MimeType: MimeTypePNG,
	}, nil
}

func (g VisualRank) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

func (VisualRank) NormalizeAnswer(input string) (string, bool) {
	return NormalizeOrder(input, visualRankCards)
}

func (VisualRank) SanitizePublic(raw []byte) (map[string]any, error) {
	var p VisualRankPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Kind != "visual-rank" || p.AnswerType != AnswerTypeOrder || p.MaxAnswerLen != visualRankMaxAnswerLen {
		return nil, fmt.Errorf("generator: visual-rank 题面类型不合法")
	}
	if p.ImageWidth != VisualRankWidth || p.ImageHeight != VisualRankHeight {
		return nil, fmt.Errorf("generator: visual-rank 图像尺寸不合法")
	}
	if p.InputMode != "rank" || len(p.Cards) != visualRankCards {
		return nil, fmt.Errorf("generator: visual-rank 输入模式/卡数不合法")
	}
	if p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200 {
		return nil, fmt.Errorf("generator: visual-rank 提示不合法")
	}
	seen := map[string]bool{}
	for _, c := range p.Cards {
		if len(c.ID) != 1 || c.ID[0] < 'A' || c.ID[0] > 'E' || seen[c.ID] {
			return nil, fmt.Errorf("generator: visual-rank 卡片 ID 不合法")
		}
		seen[c.ID] = true
		if c.W != visualRankCardW || c.H != visualRankCardH {
			return nil, fmt.Errorf("generator: visual-rank 卡片尺寸不合法")
		}
		if c.X < 0 || c.Y < 0 || c.X+c.W > VisualRankWidth || c.Y+c.H > VisualRankHeight {
			return nil, fmt.Errorf("generator: visual-rank 卡片包围盒越界")
		}
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"cards", "input_mode", "prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: visual-rank 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"cards": p.Cards, "input_mode": p.InputMode,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

func renderVisualRank(spec visualRankSpec) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, VisualRankWidth, VisualRankHeight))
	fillGrayRect(img, 0, 0, VisualRankWidth, VisualRankHeight, 242)
	for _, c := range spec.Cards {
		fillGrayRect(img, c.X, c.Y, c.W, c.H, 252)
		strokeGrayRect(img, image.Rect(c.X, c.Y, c.X+c.W, c.Y+c.H), 170)
		if len(c.Points) < 2 {
			return nil, fmt.Errorf("%w: visual-rank 卡片没有折线", ErrBadParams)
		}
		// 折线局部坐标 → 卡片坐标：水平居中、垂直居中。
		offX := c.X + (c.W-(visualRankCols-1)*visualRankNode)/2
		offY := c.Y + (c.H-(visualRankRows-1)*visualRankNode)/2
		for i := 0; i+1 < len(c.Points); i++ {
			a := image.Pt(offX+c.Points[i].X, offY+c.Points[i].Y)
			b := image.Pt(offX+c.Points[i+1].X, offY+c.Points[i+1].Y)
			drawThickSegment(img, a, b, 5, 34)
		}
		for _, p := range c.Points {
			fillCircle(img, offX+p.X, offY+p.Y, 4, 34)
		}
		// 起点用空心圆标记，终点用实心方块，方便辨认方向；不编码线段数。
		start := image.Pt(offX+c.Points[0].X, offY+c.Points[0].Y)
		end := image.Pt(offX+c.Points[len(c.Points)-1].X, offY+c.Points[len(c.Points)-1].Y)
		fillCircle(img, start.X, start.Y, 7, 252)
		strokeCircle(img, start.X, start.Y, 7, 34)
		fillGrayRect(img, end.X-5, end.Y-5, 10, 10, 34)
	}
	return encodeGrayPNG(img)
}

func VisualRankAnswerFrom(entropy io.Reader) (string, error) {
	spec, err := buildVisualRank(entropy)
	if err != nil {
		return "", err
	}
	return rankAnswer(spec), nil
}
