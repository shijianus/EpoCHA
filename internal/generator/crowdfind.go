package generator

// crowd-find-v1：36 选 1 的"找相同非人形角色"（2026-11-12，ADR-030）。
//
// 每个角色由 6 个离散特征组合：头轮廓、眼睛数量/排列、天线、嘴、身体纹理、配件。
// 目标特征签名在 36 个角色中恰好出现一次；distractor 与目标在 1–2 个特征上不同。
// 不使用 Waldo、真实人脸、人物照片或品牌角色；全部由代码程序化绘制。
//
// 公开 payload 不含目标索引与特征表；只给网格几何、顶部样本包围盒与 36 个公开随机 cell ID。
// 答案规范化为 v1:<0..35>，判分走 48B 精确 HMAC。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
)

const (
	CrowdFindID      = "crowd-find-v1"
	crowdFindVersion = "v1"

	crowdFindCols  = 6
	crowdFindRows  = 6
	crowdFindCells = crowdFindCols * crowdFindRows

	CrowdFindWidth  = 340
	CrowdFindHeight = 420

	crowdFindMargin   = 12
	crowdFindGridY    = 104
	crowdFindCell     = 48
	crowdFindGap      = 3
	crowdFindSampleY  = 16
	crowdFindSampleSz = 64

	crowdFindMaxAnswerLen = 8
	crowdFindMaxAttempts  = 64
	crowdFindMaxSamples   = 256
)

// 特征值数量：头 3、眼 4、天线 4、嘴 4、纹理 4、配件 4。
var crowdFeatureValues = [6]int{3, 4, 4, 4, 4, 4}

type crowdSignature [6]int

func (s crowdSignature) key() int {
	k := 0
	for i, v := range s {
		k = k*crowdFeatureValues[i] + v
	}
	return k
}

func crowdDistance(a, b crowdSignature) int {
	d := 0
	for i := range a {
		if a[i] != b[i] {
			d++
		}
	}
	return d
}

type crowdFindSpec struct {
	Signatures  [crowdFindCells]crowdSignature
	Target      crowdSignature
	TargetIndex int
	CellIDs     []string
}

func buildCrowdFind(entropy io.Reader) (crowdFindSpec, error) {
	var lastErr error
	for attempt := 0; attempt < crowdFindMaxAttempts; attempt++ {
		spec, err := buildCrowdFindOnce(entropy)
		if err == nil {
			return spec, nil
		}
		if errors.Is(err, ErrOutOfEntropy) {
			return spec, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrGenerateExhausted
	}
	return crowdFindSpec{}, fmt.Errorf("%w: crowd-find 在 %d 次尝试后失败: %v",
		ErrGenerateExhausted, crowdFindMaxAttempts, lastErr)
}

func buildCrowdFindOnce(entropy io.Reader) (crowdFindSpec, error) {
	var spec crowdFindSpec
	for i := 0; i < len(spec.Target); i++ {
		v, err := drawInt(entropy, 0, int64(crowdFeatureValues[i]-1))
		if err != nil {
			return spec, err
		}
		spec.Target[i] = int(v)
	}
	ti, err := drawInt(entropy, 0, crowdFindCells-1)
	if err != nil {
		return spec, err
	}
	spec.TargetIndex = int(ti)
	used := map[int]bool{spec.Target.key(): true}
	for idx := 0; idx < crowdFindCells; idx++ {
		if idx == spec.TargetIndex {
			spec.Signatures[idx] = spec.Target
			continue
		}
		ok := false
		for tries := 0; tries < crowdFindMaxSamples; tries++ {
			sig, err := drawCrowdDistractor(entropy, spec.Target)
			if err != nil {
				return spec, err
			}
			if used[sig.key()] {
				continue
			}
			used[sig.key()] = true
			spec.Signatures[idx] = sig
			ok = true
			break
		}
		if !ok {
			return spec, fmt.Errorf("%w: 无法生成足够多的不同 distractor", ErrBadParams)
		}
	}
	seen := map[string]bool{}
	for len(spec.CellIDs) < crowdFindCells {
		v, err := drawInt(entropy, 0, 0xffff)
		if err != nil {
			return spec, err
		}
		id := fmt.Sprintf("%04x", v)
		if seen[id] {
			continue
		}
		seen[id] = true
		spec.CellIDs = append(spec.CellIDs, id)
	}
	return spec, nil
}

// drawCrowdDistractor 从目标出发改 1–2 个特征；每个被改的特征都必须换成不同的值。
func drawCrowdDistractor(entropy io.Reader, target crowdSignature) (crowdSignature, error) {
	sig := target
	d, err := drawInt(entropy, 1, 2)
	if err != nil {
		return sig, err
	}
	order := []int{0, 1, 2, 3, 4, 5}
	for i := len(order) - 1; i > 0; i-- {
		j, err := drawInt(entropy, 0, int64(i))
		if err != nil {
			return sig, err
		}
		order[i], order[j] = order[j], order[i]
	}
	for k := 0; k < int(d); k++ {
		f := order[k]
		n := crowdFeatureValues[f] - 1
		v, err := drawInt(entropy, 0, int64(n-1))
		if err != nil {
			return sig, err
		}
		if int(v) >= target[f] {
			v++
		}
		sig[f] = int(v)
	}
	return sig, nil
}

type CrowdFindPublic struct {
	Kind         string   `json:"kind"`
	AnswerType   string   `json:"answer_type"`
	MaxAnswerLen int      `json:"max_answer_len"`
	ImageWidth   int      `json:"image_width"`
	ImageHeight  int      `json:"image_height"`
	GridCols     int      `json:"grid_cols"`
	GridRows     int      `json:"grid_rows"`
	GridX        int      `json:"grid_x"`
	GridY        int      `json:"grid_y"`
	GridCell     int      `json:"grid_cell"`
	GridGap      int      `json:"grid_gap"`
	SampleX      int      `json:"sample_x"`
	SampleY      int      `json:"sample_y"`
	SampleW      int      `json:"sample_w"`
	SampleH      int      `json:"sample_h"`
	CellIDs      []string `json:"cell_ids"`
	InputMode    string   `json:"input_mode"`
	PromptZH     string   `json:"prompt_zh"`
	PromptEN     string   `json:"prompt_en"`
}

func (CrowdFindPublic) PublicKind() string { return "crowd-find" }

type CrowdFind struct{ macKey []byte }

var _ Generator = CrowdFind{}

func NewCrowdFind(macKey []byte) (CrowdFind, error) {
	if len(macKey) < 32 {
		return CrowdFind{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	return CrowdFind{macKey: macKey}, nil
}

func (CrowdFind) ID() string      { return CrowdFindID }
func (CrowdFind) Version() string { return crowdFindVersion }
func (CrowdFind) Media() string   { return MediaImage }

func (g CrowdFind) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	spec, err := buildCrowdFind(entropy)
	if err != nil {
		return Question{}, err
	}
	nonce := make([]byte, verifierNonceSz)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	asset, err := renderCrowdFind(spec)
	if err != nil {
		return Question{}, err
	}
	gridX := (CrowdFindWidth - (crowdFindCols*crowdFindCell + (crowdFindCols-1)*crowdFindGap)) / 2
	return Question{
		Payload: CrowdFindPublic{
			Kind: "crowd-find", AnswerType: AnswerTypeSelect, MaxAnswerLen: crowdFindMaxAnswerLen,
			ImageWidth: CrowdFindWidth, ImageHeight: CrowdFindHeight,
			GridCols: crowdFindCols, GridRows: crowdFindRows,
			GridX: gridX, GridY: crowdFindGridY,
			GridCell: crowdFindCell, GridGap: crowdFindGap,
			SampleX: (CrowdFindWidth - crowdFindSampleSz) / 2, SampleY: crowdFindSampleY,
			SampleW: crowdFindSampleSz, SampleH: crowdFindSampleSz,
			CellIDs: append([]string(nil), spec.CellIDs...), InputMode: "select",
			PromptZH: "找出与顶部样本完全相同的角色，并点击它。",
			PromptEN: "Click the character that exactly matches the sample at the top.",
		},
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, fmt.Sprintf("v1:%d", spec.TargetIndex))),
		Asset:    asset, MimeType: MimeTypePNG,
	}, nil
}

func (g CrowdFind) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

func (CrowdFind) NormalizeAnswer(input string) (string, bool) {
	return NormalizeSelect(input, crowdFindCells)
}

func (CrowdFind) SanitizePublic(raw []byte) (map[string]any, error) {
	var p CrowdFindPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	gridX := (CrowdFindWidth - (crowdFindCols*crowdFindCell + (crowdFindCols-1)*crowdFindGap)) / 2
	switch {
	case p.Kind != "crowd-find" || p.AnswerType != AnswerTypeSelect:
		return nil, fmt.Errorf("generator: crowd-find 题面类型不合法")
	case p.MaxAnswerLen != crowdFindMaxAnswerLen:
		return nil, fmt.Errorf("generator: crowd-find 答案长度不合法")
	case p.ImageWidth != CrowdFindWidth || p.ImageHeight != CrowdFindHeight:
		return nil, fmt.Errorf("generator: crowd-find 图像尺寸不合法")
	case p.GridCols != crowdFindCols || p.GridRows != crowdFindRows ||
		p.GridX != gridX || p.GridY != crowdFindGridY ||
		p.GridCell != crowdFindCell || p.GridGap != crowdFindGap:
		return nil, fmt.Errorf("generator: crowd-find 网格几何不合法")
	case p.SampleX < 0 || p.SampleY < 0 || p.SampleW != crowdFindSampleSz || p.SampleH != crowdFindSampleSz ||
		p.SampleX+p.SampleW > CrowdFindWidth || p.SampleY+p.SampleH > CrowdFindHeight:
		return nil, fmt.Errorf("generator: crowd-find 样本包围盒不合法")
	case p.InputMode != "select":
		return nil, fmt.Errorf("generator: crowd-find 输入模式不合法")
	case len(p.CellIDs) != crowdFindCells:
		return nil, fmt.Errorf("generator: crowd-find cell ID 数量不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: crowd-find 提示不合法")
	}
	seen := map[string]bool{}
	for _, id := range p.CellIDs {
		if len(id) != 4 || seen[id] {
			return nil, fmt.Errorf("generator: crowd-find cell ID 不合法")
		}
		for i := 0; i < len(id); i++ {
			if _, ok := asciiHexNibble(id[i]); !ok {
				return nil, fmt.Errorf("generator: crowd-find cell ID 含非法字符")
			}
		}
		seen[id] = true
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"grid_cols", "grid_rows", "grid_x", "grid_y", "grid_cell", "grid_gap",
			"sample_x", "sample_y", "sample_w", "sample_h", "cell_ids", "input_mode",
			"prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: crowd-find 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"grid_cols": p.GridCols, "grid_rows": p.GridRows,
		"grid_x": p.GridX, "grid_y": p.GridY, "grid_cell": p.GridCell, "grid_gap": p.GridGap,
		"sample_x": p.SampleX, "sample_y": p.SampleY, "sample_w": p.SampleW, "sample_h": p.SampleH,
		"cell_ids": p.CellIDs, "input_mode": p.InputMode,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

func renderCrowdFind(spec crowdFindSpec) ([]byte, error) {
	if spec.TargetIndex < 0 || spec.TargetIndex >= crowdFindCells || len(spec.CellIDs) != crowdFindCells {
		return nil, fmt.Errorf("%w: crowd-find spec 不完整", ErrBadParams)
	}
	img := image.NewGray(image.Rect(0, 0, CrowdFindWidth, CrowdFindHeight))
	fillGrayRect(img, 0, 0, CrowdFindWidth, CrowdFindHeight, 240)
	// 顶部样本卡
	sx := (CrowdFindWidth - crowdFindSampleSz) / 2
	fillGrayRect(img, sx, crowdFindSampleY, crowdFindSampleSz, crowdFindSampleSz, 252)
	strokeGrayRect(img, image.Rect(sx, crowdFindSampleY, sx+crowdFindSampleSz, crowdFindSampleY+crowdFindSampleSz), 150)
	drawCrowdCharacter(img, spec.Target, sx+crowdFindSampleSz/2, crowdFindSampleY+crowdFindSampleSz/2, crowdFindSampleSz-6)
	// 6×6 网格
	gridX := (CrowdFindWidth - (crowdFindCols*crowdFindCell + (crowdFindCols-1)*crowdFindGap)) / 2
	for r := 0; r < crowdFindRows; r++ {
		for c := 0; c < crowdFindCols; c++ {
			x := gridX + c*(crowdFindCell+crowdFindGap)
			y := crowdFindGridY + r*(crowdFindCell+crowdFindGap)
			fillGrayRect(img, x, y, crowdFindCell, crowdFindCell, 252)
			strokeGrayRect(img, image.Rect(x, y, x+crowdFindCell, y+crowdFindCell), 190)
			drawCrowdCharacter(img, spec.Signatures[r*crowdFindCols+c],
				x+crowdFindCell/2, y+crowdFindCell/2, crowdFindCell-6)
		}
	}
	return encodeGrayPNG(img)
}

func drawCrowdCharacter(img *image.Gray, sig crowdSignature, cx, cy, size int) {
	const ink = uint8(24)
	if size < 32 {
		size = 32
	}
	hy := cy - size/6
	hr := size / 4
	// 头轮廓
	switch sig[0] % 3 {
	case 0:
		fillCircle(img, cx, hy, hr, 214)
		strokeCircle(img, cx, hy, hr, ink)
	case 1:
		fillGrayRect(img, cx-hr, hy-hr, 2*hr, 2*hr, 214)
		strokeGrayRect(img, image.Rect(cx-hr, hy-hr, cx+hr, hy+hr), ink)
	case 2:
		pts := []image.Point{
			{cx - hr, hy}, {cx - hr/2, hy - hr}, {cx + hr/2, hy - hr},
			{cx + hr, hy}, {cx + hr/2, hy + hr}, {cx - hr/2, hy + hr},
		}
		for i := 0; i < len(pts); i++ {
			drawThickSegment(img, pts[i], pts[(i+1)%len(pts)], 3, ink)
		}
	}
	// 眼睛
	eyes := []image.Point{{cx, hy}}
	switch sig[1] % 4 {
	case 0:
		eyes = []image.Point{{cx, hy}}
	case 1:
		eyes = []image.Point{{cx - 5, hy}, {cx + 5, hy}}
	case 2:
		eyes = []image.Point{{cx, hy - 5}, {cx, hy + 5}}
	case 3:
		eyes = []image.Point{{cx - 7, hy}, {cx, hy - 5}, {cx + 7, hy}}
	}
	for _, e := range eyes {
		fillCircle(img, e.X, e.Y, 2, ink)
	}
	// 天线
	switch sig[2] % 4 {
	case 1:
		drawThickSegment(img, image.Pt(cx, hy-hr), image.Pt(cx, hy-hr-8), 2, ink)
	case 2:
		drawThickSegment(img, image.Pt(cx, hy-hr), image.Pt(cx-7, hy-hr-8), 2, ink)
		drawThickSegment(img, image.Pt(cx, hy-hr), image.Pt(cx+7, hy-hr-8), 2, ink)
	case 3:
		drawThickSegment(img, image.Pt(cx, hy-hr), image.Pt(cx+4, hy-hr-5), 2, ink)
		drawThickSegment(img, image.Pt(cx+4, hy-hr-5), image.Pt(cx-2, hy-hr-9), 2, ink)
		drawThickSegment(img, image.Pt(cx-2, hy-hr-9), image.Pt(cx+3, hy-hr-13), 2, ink)
	}
	// 嘴
	switch sig[3] % 4 {
	case 0:
		drawThickSegment(img, image.Pt(cx-5, hy+5), image.Pt(cx+5, hy+5), 2, ink)
	case 1:
		drawThickSegment(img, image.Pt(cx-5, hy+4), image.Pt(cx-2, hy+7), 2, ink)
		drawThickSegment(img, image.Pt(cx-2, hy+7), image.Pt(cx+2, hy+7), 2, ink)
		drawThickSegment(img, image.Pt(cx+2, hy+7), image.Pt(cx+5, hy+4), 2, ink)
	case 2:
		fillCircle(img, cx, hy+5, 2, ink)
	case 3:
		drawThickSegment(img, image.Pt(cx-6, hy+5), image.Pt(cx-2, hy+3), 2, ink)
		drawThickSegment(img, image.Pt(cx-2, hy+3), image.Pt(cx+2, hy+7), 2, ink)
		drawThickSegment(img, image.Pt(cx+2, hy+7), image.Pt(cx+6, hy+5), 2, ink)
	}
	// 身体
	bw, bh := size*3/5, size/3
	bx, by := cx-bw/2, cy+size/8
	fillGrayRect(img, bx, by, bw, bh, 212)
	strokeGrayRect(img, image.Rect(bx, by, bx+bw, by+bh), ink)
	switch sig[4] % 4 {
	case 1:
		for y := by + 3; y < by+bh-2; y += 5 {
			for x := bx + 3; x < bx+bw-2; x += 5 {
				fillCircle(img, x, y, 1, ink)
			}
		}
	case 2:
		for x := bx + 4; x < bx+bw-2; x += 5 {
			drawThickSegment(img, image.Pt(x, by+2), image.Pt(x, by+bh-2), 1, ink)
		}
	case 3:
		for y := by + 2; y < by+bh-2; y += 4 {
			for x := bx + 2; x < bx+bw-2; x += 4 {
				if ((x-bx)/4+(y-by)/4)%2 == 0 {
					fillGrayRect(img, x, y, 2, 2, ink)
				}
			}
		}
	}
	// 配件
	switch sig[5] % 4 {
	case 1: // 帽子
		fillGrayRect(img, cx-9, hy-hr-6, 18, 5, ink)
		fillGrayRect(img, cx-5, hy-hr-12, 10, 6, ink)
	case 2: // 领结
		drawThickSegment(img, image.Pt(cx, by-1), image.Pt(cx-8, by-5), 3, ink)
		drawThickSegment(img, image.Pt(cx, by-1), image.Pt(cx+8, by-5), 3, ink)
	case 3: // 围巾
		fillGrayRect(img, cx-10, by-2, 20, 4, ink)
		drawThickSegment(img, image.Pt(cx+7, by+2), image.Pt(cx+10, by+8), 3, ink)
	}
}

func CrowdFindAnswerFrom(entropy io.Reader) (string, error) {
	spec, err := buildCrowdFind(entropy)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("v1:%d", spec.TargetIndex), nil
}
