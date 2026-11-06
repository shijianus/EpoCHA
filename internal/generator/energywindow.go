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
	"strconv"
)

// energy-window-v1：瞬态时机捕捉（Phase 4）。
//
// 复用 sprite_sheet + timing：前端只播放帧并记录相位，不新增输入控件。
// 服务端用 TSV 校验量化后的时间桶；目标相位只在中间 40..439 桶里抽样，
// 所以 M3 的分母是 400 个可行桶（不是 480）：d1 13/400=3.25%，3 次 ≈9.4%（<10%）。
// M4、人工可读性、辅助技术均 NOT_RUN。
const (
	EnergyWindowID      = "energy-window-v1"
	energyWindowVersion = "v1"
	energyFrameW        = 320
	energyFrameH        = 100
	energyFrames        = 36
	energyCols          = 6
	energyRows          = 6
	energyFrameMS       = 80
	energyCycleMS       = energyFrames * energyFrameMS
	energyQuantumMS     = 6
	energyBuckets       = energyCycleMS / energyQuantumMS // 480
	energyMaxAnswerLen  = 5

	// 目标相位只在中间区间抽样：两侧各留 40 桶，保证整个容差窗口都不落进
	// <200ms 的零思考拒绝区，也不越过周期边界。M3 的分母因此是 400 个可行桶。
	energyTargetMinBucket = 40
	energyTargetMaxBucket = energyBuckets - 41
	energyFeasibleBuckets = energyTargetMaxBucket - energyTargetMinBucket + 1
)

// 难度越高，接受窗口越窄；d1 13 桶（9.44% 三次上界），d5 5 桶（3.7%）。
// 分母固定为可行目标桶数 400（见 buildEnergyWindow 的抽样区间）。
var energyTolerances = map[int]int{1: 6, 2: 5, 3: 4, 4: 3, 5: 2}

func EnergyWindowTolerance(difficulty int) (int, error) {
	t, ok := energyTolerances[difficulty]
	if !ok {
		return 0, fmt.Errorf("%w: energy-window 缺少难度 %d 的档位", ErrBadParams, difficulty)
	}
	return t, nil
}

type EnergyWindowPublic struct {
	Kind         string `json:"kind"`
	AnswerType   string `json:"answer_type"`
	MaxAnswerLen int    `json:"max_answer_len"`
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

func (EnergyWindowPublic) PublicKind() string { return "energy-window" }

type energySpec struct {
	TargetBucket int
	TargetMS     int
	Tolerance    int
	VisualSeed   int64
}

type EnergyWindow struct{ macKey []byte }

var _ Generator = EnergyWindow{}

func NewEnergyWindow(macKey []byte) (EnergyWindow, error) {
	if len(macKey) < 32 {
		return EnergyWindow{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		if _, err := EnergyWindowTolerance(d); err != nil {
			return EnergyWindow{}, err
		}
	}
	spec := FrameSpec{Mode: RenderModeSpriteSheet, Count: energyFrames, Cols: energyCols, Rows: energyRows, FrameMS: energyFrameMS, Loop: true}
	if err := spec.Validate(); err != nil {
		return EnergyWindow{}, err
	}
	if _, _, err := spec.SheetSize(energyFrameW, energyFrameH); err != nil {
		return EnergyWindow{}, err
	}
	return EnergyWindow{macKey: macKey}, nil
}

func (EnergyWindow) ID() string      { return EnergyWindowID }
func (EnergyWindow) Version() string { return energyWindowVersion }
func (EnergyWindow) Media() string   { return MediaImage }

func buildEnergyWindow(entropy io.Reader, difficulty int) (energySpec, []byte, error) {
	tolerance, err := EnergyWindowTolerance(difficulty)
	if err != nil {
		return energySpec{}, nil, err
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return energySpec{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	b, err := drawInt(entropy, energyTargetMinBucket, energyTargetMaxBucket)
	if err != nil {
		return energySpec{}, nil, err
	}
	seedBytes := make([]byte, 8)
	if _, err := io.ReadFull(entropy, seedBytes); err != nil {
		return energySpec{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	var seed int64
	for _, c := range seedBytes {
		seed = seed<<8 | int64(c)
	}
	bucket := int(b)
	return energySpec{TargetBucket: bucket, TargetMS: bucket * energyQuantumMS, Tolerance: tolerance, VisualSeed: seed}, nonce, nil
}

// EnergyWindowAnswerFrom 按与 Generate 相同的熵顺序取目标相位（毫秒）。
func EnergyWindowAnswerFrom(entropy io.Reader, difficulty int) (string, error) {
	spec, _, err := buildEnergyWindow(entropy, difficulty)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(spec.TargetMS), nil
}

func (g EnergyWindow) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	spec, nonce, err := buildEnergyWindow(entropy, p.Difficulty)
	if err != nil {
		return Question{}, err
	}
	tags, err := NewTSVBuilder(g.macKey, nonce)
	if err != nil {
		return Question{}, err
	}
	for d := -spec.Tolerance; d <= spec.Tolerance; d++ {
		bucket := (spec.TargetBucket + d + energyBuckets) % energyBuckets
		if err := tags.Add(0, uint32(bucket)); err != nil {
			return Question{}, err
		}
	}
	ver, err := tags.Build()
	if err != nil {
		return Question{}, err
	}
	asset, err := renderEnergyWindow(spec)
	if err != nil {
		return Question{}, err
	}
	pub := EnergyWindowPublic{
		Kind: "energy-window", AnswerType: AnswerTypeTiming, MaxAnswerLen: energyMaxAnswerLen,
		ImageWidth: energyFrameW, ImageHeight: energyFrameH,
		RenderMode: RenderModeSpriteSheet,
		FrameCount: energyFrames, FrameCols: energyCols, FrameRows: energyRows,
		FrameMS: energyFrameMS, FrameLoop: true,
		PromptZH: "在游标进入标记区间时点击“记录时机”",
		PromptEN: "Click “Record timing” when the cursor enters the marked window",
	}
	return Question{Payload: pub, Verifier: PrivateVerifier(ver), Asset: asset, MimeType: MimeTypePNG}, nil
}

func (g EnergyWindow) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	ms, err := strconv.Atoi(canon)
	if err != nil || ms < 0 {
		return false
	}
	bucket := (ms / energyQuantumMS) % energyBuckets
	return TSVContains(v, g.macKey, 0, uint32(bucket))
}

func (EnergyWindow) NormalizeAnswer(input string) (string, bool) {
	canon, ok := NormalizeTiming(input)
	if !ok {
		return "", false
	}
	ms, err := strconv.Atoi(canon)
	if err != nil || ms < 200 {
		// 首圈前 200ms 视为零思考机械抢答；超长值由 NormalizeTiming 拒绝。
		return "", false
	}
	return canon, true
}

func (EnergyWindow) SanitizePublic(raw []byte) (map[string]any, error) {
	var p EnergyWindowPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("generator: energy-window 题面 JSON: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("generator: energy-window 题面 JSON: %w", err)
	}
	switch {
	case p.Kind != "energy-window" || p.AnswerType != AnswerTypeTiming:
		return nil, fmt.Errorf("generator: energy-window 题面类型不合法")
	case p.MaxAnswerLen != energyMaxAnswerLen:
		return nil, fmt.Errorf("generator: energy-window 答案长度不合法")
	case p.ImageWidth != energyFrameW || p.ImageHeight != energyFrameH:
		return nil, fmt.Errorf("generator: energy-window 帧尺寸不合法")
	case p.RenderMode != RenderModeSpriteSheet || !p.FrameLoop:
		return nil, fmt.Errorf("generator: energy-window 渲染模式不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: energy-window 提示不合法")
	}
	spec := FrameSpec{Mode: p.RenderMode, Count: p.FrameCount, Cols: p.FrameCols, Rows: p.FrameRows, FrameMS: p.FrameMS, Loop: p.FrameLoop}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if _, _, err := spec.SheetSize(energyFrameW, energyFrameH); err != nil {
		return nil, err
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"render_mode", "frame_count", "frame_cols", "frame_rows", "frame_ms", "frame_loop",
			"prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: energy-window 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"render_mode": p.RenderMode, "frame_count": p.FrameCount,
		"frame_cols": p.FrameCols, "frame_rows": p.FrameRows,
		"frame_ms": p.FrameMS, "frame_loop": p.FrameLoop,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

func renderEnergyWindow(spec energySpec) ([]byte, error) {
	sheetW, sheetH := energyFrameW*energyCols, energyFrameH*energyRows
	sheet := image.NewGray(image.Rect(0, 0, sheetW, sheetH))
	// 目标区在轨道上的固定位置；接受域是 13 个 6ms 桶（约 3.25% 周期）。
	trackX0, trackX1 := 30, energyFrameW-30
	trackW := trackX1 - trackX0
	zoneX := trackX0 + int(float64(trackW)*float64(spec.TargetBucket)/float64(energyBuckets))
	zoneW := int(float64(trackW) * float64(2*spec.Tolerance+1) / float64(energyBuckets))
	if zoneW < 1 {
		zoneW = 1
	}
	for f := 0; f < energyFrames; f++ {
		frame := image.NewGray(image.Rect(0, 0, energyFrameW, energyFrameH))
		fillGrayRect(frame, 0, 0, energyFrameW, energyFrameH, 18)
		fillGrayRect(frame, trackX0, 48, trackW, 6, 70)
		for x := trackX0; x <= trackX1; x += 20 {
			fillGrayRect(frame, x, 42, 1, 18, 42)
		}
		fillGrayRect(frame, zoneX, 38, zoneW, 26, 225)
		// 目标区括号，帮助人眼定位；不暴露精确时间桶。
		fillGrayRect(frame, zoneX, 28, zoneW, 2, 190)
		cursorX := trackX0 + int(float64(trackW)*float64(f)/float64(energyFrames))
		fillGrayRect(frame, cursorX, 32, 3, 38, 245)
		fillCircle(frame, cursorX, 51, 5, 245)
		rng := mrand.New(mrand.NewSource(spec.VisualSeed + int64(f)*37))
		for i := 0; i < 10; i++ {
			x, y := rng.Intn(energyFrameW), rng.Intn(energyFrameH)
			if frame.GrayAt(x, y).Y <= 40 {
				frame.SetGray(x, y, color.Gray{Y: uint8(22 + rng.Intn(12))})
			}
		}
		col, row := f%energyCols, f/energyCols
		stddraw.Draw(sheet, image.Rect(col*energyFrameW, row*energyFrameH,
			(col+1)*energyFrameW, (row+1)*energyFrameH), frame, image.Point{}, stddraw.Src)
	}
	return encodeGrayPNG(sheet)
}
