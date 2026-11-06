package generator

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	"io"
	mrand "math/rand"
	"strings"
)

// parallax-code-v1：时序运动视差题（ADR-025/ADR-028）。
//
// 单帧只包含每个数字的一部分像素块，数字要靠连续帧的视觉暂留拼合；
// 背景两层以不同速度移动，单帧截图看到的是"噪点 + 部分笔画"，不是完整答案。
// 这是**形状层面的成本提升**，不是抗 AI 证明：M4 仍为 NOT_RUN。
const (
	ParallaxID      = "parallax-code-v1"
	parallaxVersion = "v1"

	ParallaxCodeLength = 4
	ParallaxCharset    = "0123456789"
	parallaxFrameW     = 320
	parallaxFrameH     = 120
	parallaxGlyphScale = 7
	parallaxGlyphGap   = 10
	parallaxGroups     = 4
)

// ParallaxPublic 是唯一公开题面：字段即白名单；答案不在里面。
type ParallaxPublic struct {
	Kind         string `json:"kind"`
	AnswerType   string `json:"answer_type"`
	MaxAnswerLen int    `json:"max_answer_len"`
	Length       int    `json:"length"`
	Charset      string `json:"charset"`
	ImageWidth   int    `json:"image_width"`
	ImageHeight  int    `json:"image_height"`
	RenderMode   string `json:"render_mode"`
	FrameCount   int    `json:"frame_count"`
	FrameCols    int    `json:"frame_cols"`
	FrameRows    int    `json:"frame_rows"`
	FrameMS      int    `json:"frame_ms"`
	FrameLoop    bool   `json:"frame_loop"`
	PromptZH     string `json:"prompt_zh"`
	PromptEN     string `json:"prompt_en"`
}

func (ParallaxPublic) PublicKind() string { return "parallax-code" }

type parallaxTier struct {
	frames, cols, rows, frameMS int
	speedA, speedB              int
}

var parallaxTiers = map[int]parallaxTier{
	1: {frames: 8, cols: 4, rows: 2, frameMS: 130, speedA: 7, speedB: 13},
	2: {frames: 8, cols: 4, rows: 2, frameMS: 120, speedA: 9, speedB: 16},
	3: {frames: 10, cols: 5, rows: 2, frameMS: 105, speedA: 11, speedB: 19},
	4: {frames: 12, cols: 4, rows: 3, frameMS: 92, speedA: 13, speedB: 22},
	5: {frames: 12, cols: 4, rows: 3, frameMS: 82, speedA: 15, speedB: 25},
}

// Parallax 实现 Generator；无可变状态，可并发调用。
type Parallax struct {
	macKey []byte
}

var _ Generator = Parallax{}

func NewParallax(macKey []byte) (Parallax, error) {
	if len(macKey) < 32 {
		return Parallax{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		t, ok := parallaxTiers[d]
		if !ok {
			return Parallax{}, fmt.Errorf("%w: parallax 缺少难度 %d 的档位", ErrBadParams, d)
		}
		spec := FrameSpec{
			Mode: RenderModeSpriteSheet, Count: t.frames,
			Cols: t.cols, Rows: t.rows, FrameMS: t.frameMS, Loop: true,
		}
		if err := spec.Validate(); err != nil {
			return Parallax{}, err
		}
		if _, _, err := spec.SheetSize(parallaxFrameW, parallaxFrameH); err != nil {
			return Parallax{}, err
		}
	}
	return Parallax{macKey: macKey}, nil
}

func (Parallax) ID() string      { return ParallaxID }
func (Parallax) Version() string { return parallaxVersion }
func (Parallax) Media() string   { return MediaImage }

type parallaxSpec struct {
	Code       string
	Tier       parallaxTier
	VisualSeed int64
}

// buildParallax 是 Generate 与 ParallaxAnswerFrom 的**唯一**抽样路径；
// 两处必须消费完全相同的熵字节，否则 devseed 的"答案与图配对"会静默错位。
func buildParallax(entropy io.Reader, difficulty int) (parallaxSpec, []byte, error) {
	if difficulty < 1 || difficulty > MaxDifficulties {
		return parallaxSpec{}, nil, fmt.Errorf("%w: difficulty=%d", ErrBadParams, difficulty)
	}
	tier, ok := parallaxTiers[difficulty]
	if !ok {
		return parallaxSpec{}, nil, fmt.Errorf("%w: difficulty=%d", ErrBadParams, difficulty)
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return parallaxSpec{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	var code strings.Builder
	for i := 0; i < ParallaxCodeLength; i++ {
		n, err := drawInt(entropy, 0, int64(len(ParallaxCharset)-1))
		if err != nil {
			return parallaxSpec{}, nil, err
		}
		code.WriteByte(ParallaxCharset[n])
	}
	seedBytes := make([]byte, 8)
	if _, err := io.ReadFull(entropy, seedBytes); err != nil {
		return parallaxSpec{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	var seed int64
	for _, b := range seedBytes {
		seed = seed<<8 | int64(b)
	}
	return parallaxSpec{Code: code.String(), Tier: tier, VisualSeed: seed}, nonce, nil
}

// ParallaxAnswerFrom 按与 Generate 相同的熵顺序取出答案，供 devseed/离线评测使用。
// 它不读库、不下发答案；生产 API 没有这条路径。
func ParallaxAnswerFrom(entropy io.Reader, difficulty int) (string, error) {
	spec, _, err := buildParallax(entropy, difficulty)
	if err != nil {
		return "", err
	}
	return spec.Code, nil
}

func (g Parallax) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	spec, nonce, err := buildParallax(entropy, p.Difficulty)
	if err != nil {
		return Question{}, err
	}
	asset, err := renderParallaxSheet(spec)
	if err != nil {
		return Question{}, err
	}
	pub := ParallaxPublic{
		Kind: "parallax-code", AnswerType: AnswerTypeText, MaxAnswerLen: ParallaxCodeLength,
		Length: ParallaxCodeLength, Charset: ParallaxCharset,
		ImageWidth: parallaxFrameW, ImageHeight: parallaxFrameH,
		RenderMode: RenderModeSpriteSheet,
		FrameCount: spec.Tier.frames, FrameCols: spec.Tier.cols, FrameRows: spec.Tier.rows,
		FrameMS: spec.Tier.frameMS, FrameLoop: true,
		PromptZH: "看运动中的数字，输入 4 位数字",
		PromptEN: "Read the 4 digits that appear in motion",
	}
	return Question{
		Payload:  pub,
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, spec.Code)),
		Asset:    asset,
		MimeType: MimeTypePNG,
	}, nil
}

func (g Parallax) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

func (Parallax) NormalizeAnswer(input string) (string, bool) {
	s := trimASCII(input)
	if len(s) != ParallaxCodeLength {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", false
		}
	}
	return s, true
}

func (Parallax) SanitizePublic(raw []byte) (map[string]any, error) {
	var p ParallaxPublic
	var generic map[string]any
	if err := jsonUnmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := jsonUnmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch {
	case p.Kind != "parallax-code" || p.AnswerType != AnswerTypeText:
		return nil, fmt.Errorf("generator: parallax 题面类型不合法")
	case p.MaxAnswerLen != ParallaxCodeLength || p.Length != ParallaxCodeLength:
		return nil, fmt.Errorf("generator: parallax 答案长度不合法")
	case p.Charset != ParallaxCharset:
		return nil, fmt.Errorf("generator: parallax 字符集不合法")
	case p.ImageWidth != parallaxFrameW || p.ImageHeight != parallaxFrameH:
		return nil, fmt.Errorf("generator: parallax 帧尺寸不合法")
	case p.RenderMode != RenderModeSpriteSheet || !p.FrameLoop:
		return nil, fmt.Errorf("generator: parallax 渲染模式不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: parallax 提示不合法")
	}
	spec := FrameSpec{Mode: p.RenderMode, Count: p.FrameCount, Cols: p.FrameCols, Rows: p.FrameRows, FrameMS: p.FrameMS, Loop: p.FrameLoop}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if _, _, err := spec.SheetSize(parallaxFrameW, parallaxFrameH); err != nil {
		return nil, err
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "length", "charset",
			"image_width", "image_height", "render_mode", "frame_count",
			"frame_cols", "frame_rows", "frame_ms", "frame_loop", "prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: parallax 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"length": p.Length, "charset": p.Charset,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"render_mode": p.RenderMode, "frame_count": p.FrameCount,
		"frame_cols": p.FrameCols, "frame_rows": p.FrameRows,
		"frame_ms": p.FrameMS, "frame_loop": p.FrameLoop,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

// jsonUnmarshal 是 json.Unmarshal 的薄包装，集中错误文案；避免每个 SanitizePublic
// 重复写同一句 "generator: 题面 JSON"。
func jsonUnmarshal(raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("generator: 题面 JSON: %w", err)
	}
	return nil
}

var parallaxDigits = [10][7]string{
	{"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	{"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	{"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	{"11111", "00010", "00100", "00010", "00001", "10001", "01110"},
	{"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	{"11111", "10000", "11110", "00001", "00001", "10001", "01110"},
	{"00110", "01000", "10000", "11110", "10001", "10001", "01110"},
	{"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	{"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	{"01110", "10001", "10001", "01111", "00001", "00010", "01100"},
}

func renderParallaxSheet(spec parallaxSpec) ([]byte, error) {
	t := spec.Tier
	sheetW, sheetH := parallaxFrameW*t.cols, parallaxFrameH*t.rows
	sheet := image.NewGray(image.Rect(0, 0, sheetW, sheetH))
	for f := 0; f < t.frames; f++ {
		frame := image.NewGray(image.Rect(0, 0, parallaxFrameW, parallaxFrameH))
		// 背景：两层不同速度的竖线 + 静态横线。它们是"视差"的来源，不参与答案。
		fillGrayRect(frame, 0, 0, parallaxFrameW, parallaxFrameH, 18)
		for i := 0; i < 8; i++ {
			x := (i*40 + f*t.speedA) % parallaxFrameW
			fillGrayRect(frame, x, 0, 1, parallaxFrameH, 52)
		}
		for i := 0; i < 5; i++ {
			x := (i*64 - f*t.speedB + parallaxFrameW*8) % parallaxFrameW
			fillGrayRect(frame, x, 0, 2, parallaxFrameH, 78)
		}
		for y := 20; y < parallaxFrameH; y += 40 {
			fillGrayRect(frame, 0, y, parallaxFrameW, 1, 38)
		}
		// 每帧固定种子的轻噪点：让单帧更像背景，不进入答案像素。
		rng := mrand.New(mrand.NewSource(spec.VisualSeed + int64(f)*7919))
		for i := 0; i < 36; i++ {
			x, y := rng.Intn(parallaxFrameW), rng.Intn(parallaxFrameH)
			v := 30 + rng.Intn(30)
			frame.SetGray(x, y, color.Gray{Y: uint8(v)})
		}
		drawParallaxCode(frame, spec.Code, f)
		dst := image.Rect((f%t.cols)*parallaxFrameW, (f/t.cols)*parallaxFrameH,
			(f%t.cols+1)*parallaxFrameW, (f/t.cols+1)*parallaxFrameH)
		stddraw.Draw(sheet, dst, frame, image.Point{}, stddraw.Src)
	}
	return encodeGrayPNG(sheet)
}

func drawParallaxCode(dst *image.Gray, code string, frame int) {
	glyphW, glyphH := 5*parallaxGlyphScale, 7*parallaxGlyphScale
	totalW := len(code)*glyphW + (len(code)-1)*parallaxGlyphGap
	baseX := (parallaxFrameW - totalW) / 2
	baseY := (parallaxFrameH - glyphH) / 2
	for di := 0; di < len(code); di++ {
		d := int(code[di] - '0')
		if d < 0 || d > 9 {
			continue
		}
		for row := 0; row < 7; row++ {
			line := parallaxDigits[d][row]
			for col := 0; col < 5; col++ {
				if line[col] != '1' {
					continue
				}
				// 每个像素块只在一个相位组出现一次；一帧最多看到 1/4 的笔画。
				group := (di*11 + row*5 + col*3 + row*col) % parallaxGroups
				if group != frame%parallaxGroups {
					continue
				}
				x := baseX + di*(glyphW+parallaxGlyphGap) + col*parallaxGlyphScale
				y := baseY + row*parallaxGlyphScale
				fillGrayRect(dst, x, y, parallaxGlyphScale, parallaxGlyphScale, 235)
			}
		}
	}
}

func fillGrayRect(dst *image.Gray, x, y, w, h int, v uint8) {
	r := image.Rect(x, y, x+w, y+h).Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	stddraw.Draw(dst, r, image.NewUniform(color.Gray{Y: v}), image.Point{}, stddraw.Src)
}
