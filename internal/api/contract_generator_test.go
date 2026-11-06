package api

// 契约测试生成器：只在测试进程内登记，用来证明 angle/point/timing 的
// API → verifier → redeem 路径。它们不是生产题型，也不进入 cmd/server 的注册表。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"strconv"
	"strings"

	"github.com/shijianus/EpoCHA/internal/generator"
)

const (
	contractAngleID      = "contract-angle-v1"
	contractPointID      = "contract-point-v1"
	contractTimingID     = "contract-timing-v1"
	contractAngleAnswer  = "900"
	contractPointAnswer  = "500,500"
	contractTimingAnswer = "1200"
)

type contractPublic struct {
	Kind         string `json:"kind"`
	AnswerType   string `json:"answer_type"`
	MaxAnswerLen int    `json:"max_answer_len"`
	ImageWidth   int    `json:"image_width"`
	ImageHeight  int    `json:"image_height"`
	PromptZH     string `json:"prompt_zh"`
	PromptEN     string `json:"prompt_en"`

	AngleMin  int `json:"angle_min,omitempty"`
	AngleMax  int `json:"angle_max,omitempty"`
	AngleStep int `json:"angle_step,omitempty"`

	RenderMode string `json:"render_mode,omitempty"`
	FrameCount int    `json:"frame_count,omitempty"`
	FrameCols  int    `json:"frame_cols,omitempty"`
	FrameRows  int    `json:"frame_rows,omitempty"`
	FrameMS    int    `json:"frame_ms,omitempty"`
	FrameLoop  bool   `json:"frame_loop,omitempty"`
}

func (p contractPublic) PublicKind() string { return p.Kind }

type contractGen struct {
	id         string
	kind       string
	answerType string
	canonical  string
	key        []byte
}

func newContractAngle(key []byte) (contractGen, error) {
	return newContractGen(key, contractAngleID, "angle-contract", generator.AnswerTypeAngle, contractAngleAnswer)
}

func newContractPoint(key []byte) (contractGen, error) {
	return newContractGen(key, contractPointID, "point-contract", generator.AnswerTypePoint, contractPointAnswer)
}

func newContractTiming(key []byte) (contractGen, error) {
	return newContractGen(key, contractTimingID, "timing-contract", generator.AnswerTypeTiming, contractTimingAnswer)
}

func newContractGen(key []byte, id, kind, answerType, canonical string) (contractGen, error) {
	if len(key) < 32 {
		return contractGen{}, generator.ErrBadParams
	}
	return contractGen{id: id, kind: kind, answerType: answerType, canonical: canonical, key: append([]byte(nil), key...)}, nil
}

func (g contractGen) ID() string      { return g.id }
func (g contractGen) Version() string { return "v1" }
func (g contractGen) Media() string   { return generator.MediaImage }

func (g contractGen) Generate(ctx context.Context, p generator.Params, entropy io.Reader) (generator.Question, error) {
	if err := ctx.Err(); err != nil {
		return generator.Question{}, err
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return generator.Question{}, fmt.Errorf("%w: %v", generator.ErrOutOfEntropy, err)
	}
	value, err := g.tagValue(g.canonical)
	if err != nil {
		return generator.Question{}, err
	}
	b, err := generator.NewTSVBuilder(g.key, nonce)
	if err != nil {
		return generator.Question{}, err
	}
	if err := b.Add(0, value); err != nil {
		return generator.Question{}, err
	}
	ver, err := b.Build()
	if err != nil {
		return generator.Question{}, err
	}
	pub := contractPublic{
		Kind: g.kind, AnswerType: g.answerType, MaxAnswerLen: 8,
		ImageWidth: 320, ImageHeight: 180,
		PromptZH: "契约测试题", PromptEN: "contract fixture",
	}
	switch g.answerType {
	case generator.AnswerTypeAngle:
		pub.AngleMin, pub.AngleMax, pub.AngleStep = 0, 1800, 1
	case generator.AnswerTypeTiming:
		pub.RenderMode = generator.RenderModeSpriteSheet
		pub.FrameCount, pub.FrameCols, pub.FrameRows, pub.FrameMS, pub.FrameLoop = 4, 2, 2, 100, true
	}
	assetW, assetH := 320, 180
	if g.answerType == generator.AnswerTypeTiming {
		assetW, assetH = 640, 360
	}
	return generator.Question{
		Payload:  pub,
		Verifier: generator.PrivateVerifier(ver),
		Asset:    contractPNG(assetW, assetH),
		MimeType: "image/png",
	}, nil
}

func (g contractGen) Verify(v generator.PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	value, err := g.tagValue(canon)
	if err != nil {
		return false
	}
	return generator.TSVContains(v, g.key, 0, value)
}

func (g contractGen) NormalizeAnswer(input string) (string, bool) {
	return generator.NormalizePrimitiveAnswer(g.answerType, input)
}

func (g contractGen) SanitizePublic(raw []byte) (map[string]any, error) {
	var p contractPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Kind != g.kind || p.AnswerType != g.answerType || p.MaxAnswerLen < 1 || p.MaxAnswerLen > 64 {
		return nil, fmt.Errorf("generator: contract 题面头不合法")
	}
	if p.ImageWidth < 1 || p.ImageWidth > 640 || p.ImageHeight < 1 || p.ImageHeight > 640 {
		return nil, fmt.Errorf("generator: contract 图像尺寸不合法")
	}
	if p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200 {
		return nil, fmt.Errorf("generator: contract 提示不合法")
	}
	switch g.answerType {
	case generator.AnswerTypeAngle:
		if p.AngleMin < 0 || p.AngleMax > generator.AngleMax || p.AngleMin >= p.AngleMax || p.AngleStep < 1 {
			return nil, fmt.Errorf("generator: contract angle 参数不合法")
		}
	case generator.AnswerTypeTiming:
		spec := generator.FrameSpec{Mode: p.RenderMode, Count: p.FrameCount, Cols: p.FrameCols, Rows: p.FrameRows, FrameMS: p.FrameMS, Loop: p.FrameLoop}
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		if _, _, err := spec.SheetSize(p.ImageWidth, p.ImageHeight); err != nil {
			return nil, err
		}
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"prompt_zh", "prompt_en", "angle_min", "angle_max", "angle_step",
			"render_mode", "frame_count", "frame_cols", "frame_rows", "frame_ms", "frame_loop":
		default:
			return nil, fmt.Errorf("generator: contract 题面含非白名单字段 %q", k)
		}
	}
	out := map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}
	if g.answerType == generator.AnswerTypeAngle {
		out["angle_min"], out["angle_max"], out["angle_step"] = p.AngleMin, p.AngleMax, p.AngleStep
	}
	if g.answerType == generator.AnswerTypeTiming {
		out["render_mode"], out["frame_count"], out["frame_cols"] = p.RenderMode, p.FrameCount, p.FrameCols
		out["frame_rows"], out["frame_ms"], out["frame_loop"] = p.FrameRows, p.FrameMS, p.FrameLoop
	}
	return out, nil
}

func (g contractGen) tagValue(canonical string) (uint32, error) {
	switch g.answerType {
	case generator.AnswerTypeAngle, generator.AnswerTypeTiming:
		n, err := strconv.Atoi(canonical)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("contract: 数值不合法")
		}
		return uint32(n), nil
	case generator.AnswerTypePoint:
		parts := strings.Split(canonical, ",")
		if len(parts) != 2 {
			return 0, fmt.Errorf("contract: 点不合法")
		}
		x, err1 := strconv.Atoi(parts[0])
		y, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || x < 0 || y < 0 || x > 999 || y > 999 {
			return 0, fmt.Errorf("contract: 点不合法")
		}
		return uint32(x)<<16 | uint32(y), nil
	default:
		return 0, fmt.Errorf("contract: 未知类型")
	}
}

func contractPNG(w, h int) []byte {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}
