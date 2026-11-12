package generator

// shell-track-v1：三轮四容器跟踪（2026-11-12，ADR-030 Wave 2）。
//
// 高风险实验题：每轮先短暂显示目标所在容器，再做 4 次可见交换，动画结束后用户选一个容器。
// 三轮答案一起提交成 v1:a.b.c，判分走 48B 精确 HMAC；随机猜测上界 1/4^3（三次 3/64）。
//
// 为什么动画由服务端预渲染成帧而不是下发 swap sequence：
//   - swap sequence / 初始目标索引 / 最终索引只存在于生成器内存与 verifier 的 HMAC 里；
//   - payload 只有帧几何与容器点击区，浏览器拿不到"谁和谁换了"的结构化数据。
// 为什么不用 GIF：现有题池是**单资产**（pool.asset_blob，MIME CHECK 只允许 image/png），
// 而三轮必须在每轮结束时暂停等待用户选择。单张 GIF 无法在轮次边界暂停，拆成三条资产
// 又会改 schema/DTO。帧雪碧图仍是服务端预生成、浏览器只做顺序播放，不暴露交换序列。
// GIF 路径本身在目标浏览器上的 disposal/时序一致性未实测，按 NOT_RUN/BLOCKED 记录。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"strconv"
	"strings"
)

const (
	ShellTrackID      = "shell-track-v1"
	shellTrackVersion = "v1"

	shellTrackRounds = 3
	shellTrackSlots  = 4
	shellTrackSwaps  = 4

	// 每轮：2 帧目标展示 + 1 帧隐藏 + 4 次交换 × 2 帧 = 11 帧。
	shellTrackFramesPerRound = 2 + 1 + shellTrackSwaps*2

	ShellTrackFrameW = 280
	ShellTrackFrameH = 100

	shellTrackFrameCols = 6
	shellTrackFrameRows = 6
	shellTrackFrameMS   = 170

	shellTrackMaxAnswerLen = 8
	shellTrackMaxAttempts  = 32

	shellTrackBoxY = 28
	shellTrackBoxW = 52
	shellTrackBoxH = 62
	shellTrackBase = 88
)

type shellSwap [2]int

type shellTrackRound struct {
	TargetStart int
	TargetFinal int
	Moves       int
	Swaps       [shellTrackSwaps]shellSwap
}

type shellTrackSpec struct {
	Rounds [shellTrackRounds]shellTrackRound
}

func shellTrackAnswer(spec shellTrackSpec) string {
	parts := make([]string, shellTrackRounds)
	for i := range spec.Rounds {
		parts[i] = strconv.Itoa(spec.Rounds[i].TargetFinal)
	}
	return VersionPrefix + strings.Join(parts, ".")
}

func buildShellTrack(entropy io.Reader) (shellTrackSpec, error) {
	var lastErr error
	for attempt := 0; attempt < shellTrackMaxAttempts; attempt++ {
		spec, err := buildShellTrackOnce(entropy)
		if err == nil {
			return spec, nil
		}
		lastErr = err
		if errors.Is(err, ErrOutOfEntropy) {
			return spec, err
		}
	}
	if lastErr == nil {
		lastErr = ErrGenerateExhausted
	}
	return shellTrackSpec{}, fmt.Errorf("%w: shell-track 在 %d 次尝试后失败: %v",
		ErrGenerateExhausted, shellTrackMaxAttempts, lastErr)
}

func buildShellTrackOnce(entropy io.Reader) (shellTrackSpec, error) {
	var spec shellTrackSpec
	for r := 0; r < shellTrackRounds; r++ {
		start, err := drawInt(entropy, 0, shellTrackSlots-1)
		if err != nil {
			return spec, err
		}
		spec.Rounds[r].TargetStart = int(start)
		targetSlot := int(start)
		targetCup := int(start) // 初始第 i 个容器里放的是第 i 个"内容"
		var prevA, prevB = -1, -1
		moves := 0
		for k := 0; k < shellTrackSwaps; k++ {
			var a, b int
			ok := false
			for tries := 0; tries < 16; tries++ {
				av, err := drawInt(entropy, 0, shellTrackSlots-1)
				if err != nil {
					return spec, err
				}
				bv, err := drawInt(entropy, 0, shellTrackSlots-2)
				if err != nil {
					return spec, err
				}
				a = int(av)
				b = int(bv)
				if b >= a {
					b++
				}
				if (a == prevA && b == prevB) || (a == prevB && b == prevA) {
					continue
				}
				ok = true
				break
			}
			if !ok {
				return spec, fmt.Errorf("%w: 无法选出相邻两轮不同的交换", ErrBadParams)
			}
			prevA, prevB = a, b
			spec.Rounds[r].Swaps[k] = shellSwap{a, b}
			if targetSlot == a {
				targetSlot = b
				moves++
			} else if targetSlot == b {
				targetSlot = a
				moves++
			}
			_ = targetCup
		}
		if moves < 2 {
			return spec, fmt.Errorf("%w: 目标只移动了 %d 次", ErrBadParams, moves)
		}
		spec.Rounds[r].TargetFinal = targetSlot
		spec.Rounds[r].Moves = moves
	}
	return spec, nil
}

type shellTrackBox struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type ShellTrackPublic struct {
	Kind           string          `json:"kind"`
	AnswerType     string          `json:"answer_type"`
	MaxAnswerLen   int             `json:"max_answer_len"`
	ImageWidth     int             `json:"image_width"`
	ImageHeight    int             `json:"image_height"`
	RenderMode     string          `json:"render_mode"`
	FrameCount     int             `json:"frame_count"`
	FrameCols      int             `json:"frame_cols"`
	FrameRows      int             `json:"frame_rows"`
	FrameMS        int             `json:"frame_ms"`
	FrameLoop      bool            `json:"frame_loop"`
	Rounds         int             `json:"rounds"`
	FramesPerRound int             `json:"frames_per_round"`
	Boxes          []shellTrackBox `json:"boxes"`
	InputMode      string          `json:"input_mode"`
	PromptZH       string          `json:"prompt_zh"`
	PromptEN       string          `json:"prompt_en"`
}

func (ShellTrackPublic) PublicKind() string { return "shell-track" }

type ShellTrack struct{ macKey []byte }

var _ Generator = ShellTrack{}

func NewShellTrack(macKey []byte) (ShellTrack, error) {
	if len(macKey) < 32 {
		return ShellTrack{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	if shellTrackFramesPerRound*shellTrackRounds > shellTrackFrameCols*shellTrackFrameRows {
		return ShellTrack{}, fmt.Errorf("%w: shell-track 帧数超过雪碧图容量", ErrBadParams)
	}
	if ShellTrackFrameW*shellTrackFrameCols > 2048 || ShellTrackFrameH*shellTrackFrameRows > 2048 {
		return ShellTrack{}, fmt.Errorf("%w: shell-track 雪碧图超过客户端上限", ErrBadParams)
	}
	return ShellTrack{macKey: macKey}, nil
}

func (ShellTrack) ID() string      { return ShellTrackID }
func (ShellTrack) Version() string { return shellTrackVersion }
func (ShellTrack) Media() string   { return MediaImage }

func shellTrackBoxes() []shellTrackBox {
	out := make([]shellTrackBox, shellTrackSlots)
	for i := range out {
		cx := shellTrackSlotCenter(i)
		out[i] = shellTrackBox{X: cx - shellTrackBoxW/2, Y: shellTrackBoxY, W: shellTrackBoxW, H: shellTrackBoxH}
	}
	return out
}

func shellTrackSlotCenter(slot int) int {
	return 40 + slot*70
}

func (g ShellTrack) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	spec, err := buildShellTrack(entropy)
	if err != nil {
		return Question{}, err
	}
	nonce := make([]byte, verifierNonceSz)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	asset, err := renderShellTrack(spec)
	if err != nil {
		return Question{}, err
	}
	return Question{
		Payload: ShellTrackPublic{
			Kind: "shell-track", AnswerType: AnswerTypeSequence,
			MaxAnswerLen: shellTrackMaxAnswerLen,
			ImageWidth:   ShellTrackFrameW, ImageHeight: ShellTrackFrameH,
			RenderMode: RenderModeSpriteSheet,
			FrameCount: shellTrackRounds * shellTrackFramesPerRound,
			FrameCols:  shellTrackFrameCols, FrameRows: shellTrackFrameRows,
			FrameMS: shellTrackFrameMS, FrameLoop: false,
			Rounds: shellTrackRounds, FramesPerRound: shellTrackFramesPerRound,
			Boxes: shellTrackBoxes(), InputMode: "shell",
			PromptZH: "每轮先记住目标所在容器，再跟踪交换；动画结束后点击目标容器。共三轮。",
			PromptEN: "Remember which container holds the target, track the swaps, then click the target container after each round. Three rounds.",
		},
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, shellTrackAnswer(spec))),
		Asset:    asset, MimeType: MimeTypePNG,
	}, nil
}

func (g ShellTrack) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

func (ShellTrack) NormalizeAnswer(input string) (string, bool) {
	return NormalizeSequence(input, shellTrackRounds, shellTrackSlots-1)
}

func (ShellTrack) SanitizePublic(raw []byte) (map[string]any, error) {
	var p ShellTrackPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	boxes := shellTrackBoxes()
	switch {
	case p.Kind != "shell-track" || p.AnswerType != AnswerTypeSequence:
		return nil, fmt.Errorf("generator: shell-track 题面类型不合法")
	case p.MaxAnswerLen != shellTrackMaxAnswerLen:
		return nil, fmt.Errorf("generator: shell-track 答案长度不合法")
	case p.ImageWidth != ShellTrackFrameW || p.ImageHeight != ShellTrackFrameH:
		return nil, fmt.Errorf("generator: shell-track 帧尺寸不合法")
	case p.RenderMode != RenderModeSpriteSheet:
		return nil, fmt.Errorf("generator: shell-track render_mode 不合法")
	case p.FrameCount != shellTrackRounds*shellTrackFramesPerRound:
		return nil, fmt.Errorf("generator: shell-track 帧数不合法")
	case p.FrameCols != shellTrackFrameCols || p.FrameRows != shellTrackFrameRows:
		return nil, fmt.Errorf("generator: shell-track 帧网格不合法")
	case p.FrameMS != shellTrackFrameMS || p.FrameLoop:
		return nil, fmt.Errorf("generator: shell-track 帧时序不合法")
	case p.Rounds != shellTrackRounds || p.FramesPerRound != shellTrackFramesPerRound:
		return nil, fmt.Errorf("generator: shell-track 轮次不合法")
	case p.InputMode != "shell":
		return nil, fmt.Errorf("generator: shell-track 输入模式不合法")
	case len(p.Boxes) != shellTrackSlots:
		return nil, fmt.Errorf("generator: shell-track 容器数量不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: shell-track 提示不合法")
	}
	for i, b := range p.Boxes {
		if b != boxes[i] {
			return nil, fmt.Errorf("generator: shell-track 容器几何不合法")
		}
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"render_mode", "frame_count", "frame_cols", "frame_rows", "frame_ms", "frame_loop",
			"rounds", "frames_per_round", "boxes", "input_mode", "prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: shell-track 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"render_mode": p.RenderMode, "frame_count": p.FrameCount,
		"frame_cols": p.FrameCols, "frame_rows": p.FrameRows,
		"frame_ms": p.FrameMS, "frame_loop": p.FrameLoop,
		"rounds": p.Rounds, "frames_per_round": p.FramesPerRound,
		"boxes": p.Boxes, "input_mode": p.InputMode,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

// shellFrameSpec 是一帧的视觉状态。它只存在于渲染器内，不序列化。
type shellFrameSpec struct {
	Slots      [shellTrackSlots]int
	MarkerSlot int
	Move       bool
	MoveA      int
	MoveB      int
}

func shellTrackFrames(round shellTrackRound) []shellFrameSpec {
	frames := make([]shellFrameSpec, 0, shellTrackFramesPerRound)
	slots := [shellTrackSlots]int{0, 1, 2, 3}
	frames = append(frames,
		shellFrameSpec{Slots: slots, MarkerSlot: round.TargetStart},
		shellFrameSpec{Slots: slots, MarkerSlot: round.TargetStart},
		shellFrameSpec{Slots: slots, MarkerSlot: -1},
	)
	for _, sw := range round.Swaps {
		frames = append(frames, shellFrameSpec{
			Slots: slots, MarkerSlot: -1, Move: true, MoveA: sw[0], MoveB: sw[1],
		})
		slots[sw[0]], slots[sw[1]] = slots[sw[1]], slots[sw[0]]
		frames = append(frames, shellFrameSpec{Slots: slots, MarkerSlot: -1})
	}
	return frames
}

func renderShellTrack(spec shellTrackSpec) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, ShellTrackFrameW*shellTrackFrameCols, ShellTrackFrameH*shellTrackFrameRows))
	fillGrayRect(img, 0, 0, img.Bounds().Dx(), img.Bounds().Dy(), 238)
	frame := 0
	for r := 0; r < shellTrackRounds; r++ {
		frames := shellTrackFrames(spec.Rounds[r])
		if len(frames) != shellTrackFramesPerRound {
			return nil, fmt.Errorf("%w: shell-track 第 %d 轮帧数 %d", ErrBadParams, r, len(frames))
		}
		for _, f := range frames {
			if frame >= shellTrackFrameCols*shellTrackFrameRows {
				return nil, fmt.Errorf("%w: shell-track 帧数超过雪碧图容量", ErrBadParams)
			}
			x0 := (frame % shellTrackFrameCols) * ShellTrackFrameW
			y0 := (frame / shellTrackFrameCols) * ShellTrackFrameH
			drawShellFrame(img, x0, y0, f)
			frame++
		}
	}
	return encodeGrayPNG(img)
}

func drawShellFrame(img *image.Gray, x0, y0 int, f shellFrameSpec) {
	fillGrayRect(img, x0, y0, ShellTrackFrameW, ShellTrackFrameH, 238)
	fillGrayRect(img, x0+10, y0+shellTrackBase, ShellTrackFrameW-20, 2, 180)
	// 先画静止容器，再画交换中的两个，最后画标记；相同容器形状不携带身份信息。
	for slot := 0; slot < shellTrackSlots; slot++ {
		if f.Move && (slot == f.MoveA || slot == f.MoveB) {
			continue
		}
		drawShellCup(img, x0+shellTrackSlotCenter(slot), y0+shellTrackBase, f.Slots[slot], 0)
	}
	if f.Move {
		ca, cb := shellTrackSlotCenter(f.MoveA), shellTrackSlotCenter(f.MoveB)
		// 两个容器相向而行：一个走到 35%，另一个走到 65%，在中点附近错开。
		drawShellCup(img, x0+lerpInt(ca, cb, 35), y0+shellTrackBase-5, f.Slots[f.MoveA], 0)
		drawShellCup(img, x0+lerpInt(cb, ca, 35), y0+shellTrackBase+5, f.Slots[f.MoveB], 0)
	}
	if f.MarkerSlot >= 0 && f.MarkerSlot < shellTrackSlots {
		drawShellMarker(img, x0+shellTrackSlotCenter(f.MarkerSlot), y0+12)
	}
}

func lerpInt(a, b, pct int) int { return a + (b-a)*pct/100 }

func drawShellCup(img *image.Gray, cx, baseY, cup, dy int) {
	baseY += dy
	const half = 22
	const bodyH = 42
	const domeR = 20
	// 深色外轮廓 + 浅色杯体，再加一条竖向反光条：容器之间没有任何颜色/纹理差异，
	// 目标身份只来自画面上的短暂标记。
	fillCircle(img, cx, baseY-bodyH, domeR+2, 30)
	fillGrayRect(img, cx-half-2, baseY-bodyH, 2*half+4, bodyH, 30)
	fillCircle(img, cx, baseY-bodyH, domeR, 218)
	fillGrayRect(img, cx-half, baseY-bodyH, 2*half, bodyH, 218)
	fillGrayRect(img, cx-4, baseY-bodyH+1, 8, bodyH-4, 196)
	fillGrayRect(img, cx-half+4, baseY-6, 2*half-8, 4, 170)
	_ = cup // 容器外观不编码内容；参数保留给帧状态阅读者
}

func drawShellMarker(img *image.Gray, cx, cy int) {
	fillCircle(img, cx, cy, 5, 40)
	drawThickSegment(img, image.Pt(cx-9, cy), image.Pt(cx+9, cy), 2, 40)
	drawThickSegment(img, image.Pt(cx, cy-9), image.Pt(cx, cy+9), 2, 40)
	drawThickSegment(img, image.Pt(cx-6, cy-6), image.Pt(cx+6, cy+6), 2, 40)
	drawThickSegment(img, image.Pt(cx-6, cy+6), image.Pt(cx+6, cy-6), 2, 40)
}

func ShellTrackAnswerFrom(entropy io.Reader) (string, error) {
	spec, err := buildShellTrack(entropy)
	if err != nil {
		return "", err
	}
	return shellTrackAnswer(spec), nil
}
