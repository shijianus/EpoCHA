package generator

// progressive-target-v1：三轮渐进定位（2026-11-12，ADR-030 Wave 2）。
//
// 每轮是一张独立的 3×3 board，顶部样本给出当轮目标字形，board 里恰好一格是它。
// 用户选择后客户端**不判断对错**，只记录索引并进入下一轮；三轮选择一起提交成
// v1:a.b.c，判分走 48B 精确 HMAC。随机猜测上界是 1/9^3（三次尝试 3/729）。
//
// 为什么要等服务端渲染整张雪碧图：客户端如果自己按 seed 画图，就必须拿到每轮的
// 目标字形/目标格；那等于把答案下发。这里的公开 payload 只有几何，没有目标索引。

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
	ProgressiveTargetID      = "progressive-target-v1"
	progressiveTargetVersion = "v1"

	progressiveTargetRounds = 3
	progressiveTargetCols   = 3
	progressiveTargetRows   = 3
	progressiveTargetCells  = progressiveTargetCols * progressiveTargetRows

	// 三块 320×150 的轮次面板叠成一张图。所有几何都是公开常量，不含目标位置。
	ProgressiveTargetWidth  = 320
	ProgressiveTargetHeight = 466

	progressiveTargetRoundY   = 8
	progressiveTargetRoundH   = 150
	progressiveTargetSampleX  = 12
	progressiveTargetSampleY  = 47
	progressiveTargetSampleW  = 56
	progressiveTargetSampleH  = 56
	progressiveTargetGridX    = 168
	progressiveTargetGridY    = 5
	progressiveTargetGridCell = 44
	progressiveTargetGridGap  = 4

	progressiveTargetMaxAnswerLen = 8
	progressiveTargetMaxAttempts  = 16
)

type progressiveRoundSpec struct {
	TargetIndex int
	TargetGlyph int
	Glyphs      [progressiveTargetCells]int
}

type progressiveTargetSpec struct {
	Rounds [progressiveTargetRounds]progressiveRoundSpec
}

func progressiveTargetAnswer(spec progressiveTargetSpec) string {
	parts := make([]string, progressiveTargetRounds)
	for i := range spec.Rounds {
		parts[i] = strconv.Itoa(spec.Rounds[i].TargetIndex)
	}
	return VersionPrefix + strings.Join(parts, ".")
}

func parseProgressiveAnswer(spec string) ([progressiveTargetRounds]int, bool) {
	var out [progressiveTargetRounds]int
	canon, ok := NormalizeSequence(spec, progressiveTargetRounds, progressiveTargetCells-1)
	if !ok {
		return out, false
	}
	body := canon[len(VersionPrefix):]
	parts := strings.Split(body, ".")
	if len(parts) != progressiveTargetRounds {
		return out, false
	}
	for i, p := range parts {
		out[i] = int(p[0] - '0')
	}
	return out, true
}

func buildProgressiveTarget(entropy io.Reader) (progressiveTargetSpec, error) {
	var lastErr error
	for attempt := 0; attempt < progressiveTargetMaxAttempts; attempt++ {
		spec, err := buildProgressiveTargetOnce(entropy)
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
	return progressiveTargetSpec{}, fmt.Errorf("%w: progressive-target 在 %d 次尝试后失败: %v",
		ErrGenerateExhausted, progressiveTargetMaxAttempts, lastErr)
}

func buildProgressiveTargetOnce(entropy io.Reader) (progressiveTargetSpec, error) {
	var spec progressiveTargetSpec
	for r := 0; r < progressiveTargetRounds; r++ {
		glyph, err := drawInt(entropy, 0, symbolGlyphCount-1)
		if err != nil {
			return spec, err
		}
		idx, err := drawInt(entropy, 0, progressiveTargetCells-1)
		if err != nil {
			return spec, err
		}
		spec.Rounds[r].TargetGlyph = int(glyph)
		spec.Rounds[r].TargetIndex = int(idx)
		// 干扰字形永远不等于目标字形；允许其他字形重复，因为"恰好一个目标"由
		// 目标格单独指定，不依赖干扰字形的唯一性。
		for c := 0; c < progressiveTargetCells; c++ {
			if c == spec.Rounds[r].TargetIndex {
				spec.Rounds[r].Glyphs[c] = int(glyph)
				continue
			}
			v, err := drawInt(entropy, 0, symbolGlyphCount-2)
			if err != nil {
				return spec, err
			}
			if int(v) >= int(glyph) {
				v++
			}
			spec.Rounds[r].Glyphs[c] = int(v)
		}
	}
	return spec, nil
}

type ProgressiveTargetPublic struct {
	Kind         string `json:"kind"`
	AnswerType   string `json:"answer_type"`
	MaxAnswerLen int    `json:"max_answer_len"`
	ImageWidth   int    `json:"image_width"`
	ImageHeight  int    `json:"image_height"`
	GridCols     int    `json:"grid_cols"`
	GridRows     int    `json:"grid_rows"`
	RoundCount   int    `json:"round_count"`
	RoundY       int    `json:"round_y"`
	RoundH       int    `json:"round_h"`
	SampleX      int    `json:"sample_x"`
	SampleY      int    `json:"sample_y"`
	SampleW      int    `json:"sample_w"`
	SampleH      int    `json:"sample_h"`
	GridX        int    `json:"grid_x"`
	GridY        int    `json:"grid_y"`
	GridCell     int    `json:"grid_cell"`
	GridGap      int    `json:"grid_gap"`
	InputMode    string `json:"input_mode"`
	PromptZH     string `json:"prompt_zh"`
	PromptEN     string `json:"prompt_en"`
}

func (ProgressiveTargetPublic) PublicKind() string { return "progressive-target" }

type ProgressiveTarget struct{ macKey []byte }

var _ Generator = ProgressiveTarget{}

func NewProgressiveTarget(macKey []byte) (ProgressiveTarget, error) {
	if len(macKey) < 32 {
		return ProgressiveTarget{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	if err := validateSymbolGlyphs(); err != nil {
		return ProgressiveTarget{}, err
	}
	return ProgressiveTarget{macKey: macKey}, nil
}

func (ProgressiveTarget) ID() string      { return ProgressiveTargetID }
func (ProgressiveTarget) Version() string { return progressiveTargetVersion }
func (ProgressiveTarget) Media() string   { return MediaImage }

func (g ProgressiveTarget) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	spec, err := buildProgressiveTarget(entropy)
	if err != nil {
		return Question{}, err
	}
	nonce := make([]byte, verifierNonceSz)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	asset, err := renderProgressiveTarget(spec)
	if err != nil {
		return Question{}, err
	}
	return Question{
		Payload: ProgressiveTargetPublic{
			Kind: "progressive-target", AnswerType: AnswerTypeSequence,
			MaxAnswerLen: progressiveTargetMaxAnswerLen,
			ImageWidth:   ProgressiveTargetWidth, ImageHeight: ProgressiveTargetHeight,
			GridCols: progressiveTargetCols, GridRows: progressiveTargetRows,
			RoundCount: progressiveTargetRounds,
			RoundY:     progressiveTargetRoundY, RoundH: progressiveTargetRoundH,
			SampleX: progressiveTargetSampleX, SampleY: progressiveTargetSampleY,
			SampleW: progressiveTargetSampleW, SampleH: progressiveTargetSampleH,
			GridX: progressiveTargetGridX, GridY: progressiveTargetGridY,
			GridCell: progressiveTargetGridCell, GridGap: progressiveTargetGridGap,
			InputMode: "progressive",
			PromptZH:  "按顺序完成三轮：每轮点击与顶部样本完全相同的图形。",
			PromptEN:  "Complete three rounds in order: click the shape that exactly matches the sample at the top of each round.",
		},
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, progressiveTargetAnswer(spec))),
		Asset:    asset, MimeType: MimeTypePNG,
	}, nil
}

func (g ProgressiveTarget) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

func (ProgressiveTarget) NormalizeAnswer(input string) (string, bool) {
	return NormalizeSequence(input, progressiveTargetRounds, progressiveTargetCells-1)
}

func (ProgressiveTarget) SanitizePublic(raw []byte) (map[string]any, error) {
	var p ProgressiveTargetPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch {
	case p.Kind != "progressive-target" || p.AnswerType != AnswerTypeSequence:
		return nil, fmt.Errorf("generator: progressive-target 题面类型不合法")
	case p.MaxAnswerLen != progressiveTargetMaxAnswerLen:
		return nil, fmt.Errorf("generator: progressive-target 答案长度不合法")
	case p.ImageWidth != ProgressiveTargetWidth || p.ImageHeight != ProgressiveTargetHeight:
		return nil, fmt.Errorf("generator: progressive-target 图像尺寸不合法")
	case p.GridCols != progressiveTargetCols || p.GridRows != progressiveTargetRows:
		return nil, fmt.Errorf("generator: progressive-target 网格不合法")
	case p.RoundCount != progressiveTargetRounds || p.RoundY != progressiveTargetRoundY ||
		p.RoundH != progressiveTargetRoundH:
		return nil, fmt.Errorf("generator: progressive-target 轮次几何不合法")
	case p.SampleX != progressiveTargetSampleX || p.SampleY != progressiveTargetSampleY ||
		p.SampleW != progressiveTargetSampleW || p.SampleH != progressiveTargetSampleH:
		return nil, fmt.Errorf("generator: progressive-target 样本几何不合法")
	case p.GridX != progressiveTargetGridX || p.GridY != progressiveTargetGridY ||
		p.GridCell != progressiveTargetGridCell || p.GridGap != progressiveTargetGridGap:
		return nil, fmt.Errorf("generator: progressive-target 网格几何不合法")
	case p.InputMode != "progressive":
		return nil, fmt.Errorf("generator: progressive-target 输入模式不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: progressive-target 提示不合法")
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"grid_cols", "grid_rows", "round_count", "round_y", "round_h",
			"sample_x", "sample_y", "sample_w", "sample_h",
			"grid_x", "grid_y", "grid_cell", "grid_gap", "input_mode",
			"prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: progressive-target 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"grid_cols": p.GridCols, "grid_rows": p.GridRows,
		"round_count": p.RoundCount, "round_y": p.RoundY, "round_h": p.RoundH,
		"sample_x": p.SampleX, "sample_y": p.SampleY, "sample_w": p.SampleW, "sample_h": p.SampleH,
		"grid_x": p.GridX, "grid_y": p.GridY, "grid_cell": p.GridCell, "grid_gap": p.GridGap,
		"input_mode": p.InputMode, "prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

func renderProgressiveTarget(spec progressiveTargetSpec) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, ProgressiveTargetWidth, ProgressiveTargetHeight))
	fillGrayRect(img, 0, 0, ProgressiveTargetWidth, ProgressiveTargetHeight, 238)
	for r := 0; r < progressiveTargetRounds; r++ {
		roundY := progressiveTargetRoundY + r*progressiveTargetRoundH
		fillGrayRect(img, 0, roundY, ProgressiveTargetWidth, progressiveTargetRoundH, 246)
		strokeGrayRect(img, image.Rect(0, roundY, ProgressiveTargetWidth, roundY+progressiveTargetRoundH), 200)

		sx := progressiveTargetSampleX
		sy := roundY + progressiveTargetSampleY
		fillGrayRect(img, sx, sy, progressiveTargetSampleW, progressiveTargetSampleH, 252)
		strokeGrayRect(img, image.Rect(sx, sy, sx+progressiveTargetSampleW, sy+progressiveTargetSampleH), 170)
		drawSymbolGlyph(img, spec.Rounds[r].TargetGlyph,
			sx+progressiveTargetSampleW/2, sy+progressiveTargetSampleH/2, 42, 26)

		for c := 0; c < progressiveTargetCells; c++ {
			col, row := c%progressiveTargetCols, c/progressiveTargetCols
			x := progressiveTargetGridX + col*(progressiveTargetGridCell+progressiveTargetGridGap)
			y := roundY + progressiveTargetGridY + row*(progressiveTargetGridCell+progressiveTargetGridGap)
			fillGrayRect(img, x, y, progressiveTargetGridCell, progressiveTargetGridCell, 252)
			strokeGrayRect(img, image.Rect(x, y, x+progressiveTargetGridCell, y+progressiveTargetGridCell), 196)
			drawSymbolGlyph(img, spec.Rounds[r].Glyphs[c],
				x+progressiveTargetGridCell/2, y+progressiveTargetGridCell/2, 34, 28)
		}
	}
	return encodeGrayPNG(img)
}

func ProgressiveTargetAnswerFrom(entropy io.Reader) (string, error) {
	spec, err := buildProgressiveTarget(entropy)
	if err != nil {
		return "", err
	}
	return progressiveTargetAnswer(spec), nil
}
