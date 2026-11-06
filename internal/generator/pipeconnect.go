package generator

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	mrand "math/rand"
	"sort"
	"strconv"
	"strings"
)

// pipe-connect-v1：空间拓扑轻量连通（Phase 4）。
//
// 复用 cells 原语：4×4 网格上只有主路径的 2 个断点需要点选；其余为空/死路。
// M2 答案空间是 C(16,2)=120，但关键断点只可能出现在被擦空的候选格里——死路也被擦空，
// 所以攻击者不需要看路径就能把范围缩到 E 个候选格。M3 因此按 C(E,2) 算：
// d1 至少 7 个死路（E=9）时 C(9,2)=36，单次 2.78%、3 次 8.1%（<10%）。
// 这仍是**盲猜**上界；能顺着主路径找出两个断点属于 M4，未测。
const (
	PipeConnectID      = "pipe-connect-v1"
	pipeConnectVersion = "v1"
	pipeSize           = 480
	pipeGridCols       = 4
	pipeGridRows       = 4
	pipeGridX          = 48
	pipeGridY          = 48
	pipeGridCell       = 84
	pipeGridGap        = 12
	pipeCells          = pipeGridCols * pipeGridRows
	pipeMaxAnswerLen   = 8
	pipeKeyCount       = 2
)

type PipeConnectPublic struct {
	Kind         string `json:"kind"`
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

func (PipeConnectPublic) PublicKind() string { return "pipe-connect" }

type pipeSpec struct {
	Rows       [pipeGridCols]int
	KeyCells   []int
	Decoys     []int
	VisualSeed int64
}

type pipeTier struct {
	minDecoys, maxDecoys int
	maxRowStep           int
}

// 难度梯度：d1 扁平路径 + 9 个空候选格；d5 行位波动 + 14 个空候选格。
// 死路下限由 M3 的 C(E,2) 反推：d1 必须有 7 个死路（E=9，3 次 8.1% < 10%）。
var pipeTiers = map[int]pipeTier{
	1: {minDecoys: 7, maxDecoys: 8, maxRowStep: 0},
	2: {minDecoys: 8, maxDecoys: 9, maxRowStep: 1},
	3: {minDecoys: 9, maxDecoys: 10, maxRowStep: 1},
	4: {minDecoys: 10, maxDecoys: 11, maxRowStep: 1},
	5: {minDecoys: 11, maxDecoys: 12, maxRowStep: 1},
}

func PipeConnectDecoyRange(difficulty int) (int, int, error) {
	t, ok := pipeTiers[difficulty]
	if !ok {
		return 0, 0, fmt.Errorf("%w: pipe-connect 缺少难度 %d 的档位", ErrBadParams, difficulty)
	}
	return t.minDecoys, t.maxDecoys, nil
}

type PipeConnect struct{ macKey []byte }

var _ Generator = PipeConnect{}

func NewPipeConnect(macKey []byte) (PipeConnect, error) {
	if len(macKey) < 32 {
		return PipeConnect{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		if _, _, err := PipeConnectDecoyRange(d); err != nil {
			return PipeConnect{}, err
		}
	}
	return PipeConnect{macKey: macKey}, nil
}

func (PipeConnect) ID() string      { return PipeConnectID }
func (PipeConnect) Version() string { return pipeConnectVersion }
func (PipeConnect) Media() string   { return MediaImage }

func buildPipeConnect(entropy io.Reader, difficulty int) (pipeSpec, []byte, error) {
	tier, ok := pipeTiers[difficulty]
	if !ok {
		return pipeSpec{}, nil, fmt.Errorf("%w: pipe-connect 缺少难度 %d 的档位", ErrBadParams, difficulty)
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return pipeSpec{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	var spec pipeSpec
	start, err := drawInt(entropy, 0, pipeGridRows-1)
	if err != nil {
		return pipeSpec{}, nil, err
	}
	spec.Rows[0] = int(start)
	for c := 1; c < pipeGridCols; c++ {
		step := int64(0)
		if tier.maxRowStep > 0 {
			step, err = drawInt(entropy, -1, 1)
			if err != nil {
				return pipeSpec{}, nil, err
			}
		}
		row := spec.Rows[c-1] + int(step)
		if row < 0 {
			row = 0
		}
		if row >= pipeGridRows {
			row = pipeGridRows - 1
		}
		spec.Rows[c] = row
	}
	path := make(map[int]bool, pipeGridCols)
	for c, r := range spec.Rows {
		path[r*pipeGridCols+c+1] = true
	}
	// 主路径第 2、3 列是断点；用户点选它们才能连通。
	spec.KeyCells = []int{
		spec.Rows[1]*pipeGridCols + 2,
		spec.Rows[2]*pipeGridCols + 3,
	}
	sort.Ints(spec.KeyCells)

	// 死路候选：优先选与主路径相邻的非路径格，形成视觉干扰支路。
	var adj []int
	seen := map[int]bool{}
	for c, r := range spec.Rows {
		for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			nc, nr := c+d[0], r+d[1]
			if nc < 0 || nc >= pipeGridCols || nr < 0 || nr >= pipeGridRows {
				continue
			}
			idx := nr*pipeGridCols + nc + 1
			if path[idx] || seen[idx] {
				continue
			}
			seen[idx] = true
			adj = append(adj, idx)
		}
	}
	if len(adj) < tier.minDecoys {
		for idx := 1; idx <= pipeCells; idx++ {
			if !path[idx] && !seen[idx] {
				seen[idx] = true
				adj = append(adj, idx)
			}
		}
	}
	maxDecoys := tier.maxDecoys
	if len(adj) > 0 {
		lo := tier.minDecoys
		if lo > len(adj) {
			lo = len(adj)
		}
		hi := tier.maxDecoys
		if hi > len(adj) {
			hi = len(adj)
		}
		if hi < lo {
			hi = lo
		}
		n, err := drawInt(entropy, int64(lo), int64(hi))
		if err != nil {
			return pipeSpec{}, nil, err
		}
		maxDecoys = int(n)
	}
	for i := 0; i < maxDecoys && len(adj) > 0; i++ {
		n, err := drawInt(entropy, 0, int64(len(adj)-1))
		if err != nil {
			return pipeSpec{}, nil, err
		}
		idx := adj[n]
		adj = append(adj[:n], adj[n+1:]...)
		spec.Decoys = append(spec.Decoys, idx)
	}
	seedBytes := make([]byte, 8)
	if _, err := io.ReadFull(entropy, seedBytes); err != nil {
		return pipeSpec{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	for _, b := range seedBytes {
		spec.VisualSeed = spec.VisualSeed<<8 | int64(b)
	}
	return spec, nonce, nil
}

// PipeConnectAnswerFrom 按与 Generate 相同的熵顺序取关键节点集合。
func PipeConnectAnswerFrom(entropy io.Reader, difficulty int) (string, error) {
	spec, _, err := buildPipeConnect(entropy, difficulty)
	if err != nil {
		return "", err
	}
	return pipeAnswer(spec), nil
}

func pipeAnswer(spec pipeSpec) string {
	keys := append([]int(nil), spec.KeyCells...)
	sort.Ints(keys)
	parts := make([]string, len(keys))
	for i, n := range keys {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

func (g PipeConnect) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	spec, nonce, err := buildPipeConnect(entropy, p.Difficulty)
	if err != nil {
		return Question{}, err
	}
	asset, err := renderPipeConnect(spec)
	if err != nil {
		return Question{}, err
	}
	pub := PipeConnectPublic{
		Kind: "pipe-connect", AnswerType: AnswerTypeCells, MaxAnswerLen: pipeMaxAnswerLen,
		ImageWidth: pipeSize, ImageHeight: pipeSize,
		GridCols: pipeGridCols, GridRows: pipeGridRows,
		GridX: pipeGridX, GridY: pipeGridY, GridCell: pipeGridCell, GridGap: pipeGridGap,
		PromptZH: "点选主路径上两个断开的节点，使起点与终点连通",
		PromptEN: "Select the two broken nodes on the main path to connect start and end",
	}
	return Question{
		Payload: pub, Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, pipeAnswer(spec))),
		Asset: asset, MimeType: MimeTypePNG,
	}, nil
}

func (g PipeConnect) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

func (PipeConnect) NormalizeAnswer(input string) (string, bool) {
	return NormalizeCellSet(input, pipeCells, pipeMaxAnswerLen)
}

func (PipeConnect) SanitizePublic(raw []byte) (map[string]any, error) {
	var p PipeConnectPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("generator: pipe-connect 题面 JSON: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("generator: pipe-connect 题面 JSON: %w", err)
	}
	switch {
	case p.Kind != "pipe-connect" || p.AnswerType != AnswerTypeCells:
		return nil, fmt.Errorf("generator: pipe-connect 题面类型不合法")
	case p.MaxAnswerLen != pipeMaxAnswerLen:
		return nil, fmt.Errorf("generator: pipe-connect 答案长度不合法")
	case p.ImageWidth != pipeSize || p.ImageHeight != pipeSize:
		return nil, fmt.Errorf("generator: pipe-connect 图像尺寸不合法")
	case p.GridCols != pipeGridCols || p.GridRows != pipeGridRows ||
		p.GridX != pipeGridX || p.GridY != pipeGridY ||
		p.GridCell != pipeGridCell || p.GridGap != pipeGridGap:
		return nil, fmt.Errorf("generator: pipe-connect 网格几何不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: pipe-connect 提示不合法")
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"grid_cols", "grid_rows", "grid_x", "grid_y", "grid_cell", "grid_gap",
			"prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: pipe-connect 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"grid_cols": p.GridCols, "grid_rows": p.GridRows,
		"grid_x": p.GridX, "grid_y": p.GridY, "grid_cell": p.GridCell, "grid_gap": p.GridGap,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

func pipeCenter(row, col int) image.Point {
	return image.Pt(pipeGridX+col*(pipeGridCell+pipeGridGap)+pipeGridCell/2,
		pipeGridY+row*(pipeGridCell+pipeGridGap)+pipeGridCell/2)
}

func renderPipeConnect(spec pipeSpec) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, pipeSize, pipeSize))
	fillGrayRect(img, 0, 0, pipeSize, pipeSize, 236)
	// 淡网格；断点与死路都落在格子里。
	for r := 0; r < pipeGridRows; r++ {
		for c := 0; c < pipeGridCols; c++ {
			rr := image.Rect(pipeGridX+c*(pipeGridCell+pipeGridGap), pipeGridY+r*(pipeGridCell+pipeGridGap),
				pipeGridX+c*(pipeGridCell+pipeGridGap)+pipeGridCell,
				pipeGridY+r*(pipeGridCell+pipeGridGap)+pipeGridCell)
			strokeGrayRect(img, rr, 214)
		}
	}
	pathPts := make([]image.Point, pipeGridCols)
	for c, r := range spec.Rows {
		pathPts[c] = pipeCenter(r, c)
	}
	for c := 0; c+1 < len(pathPts); c++ {
		drawSegment(img, pathPts[c], pathPts[c+1], 46)
	}
	// 死路支路：从相邻主路径节点拉一条浅色线，再擦成断点。
	for _, idx := range spec.Decoys {
		n := idx - 1
		r, c := n/pipeGridCols, n%pipeGridCols
		p := pipeCenter(r, c)
		var from image.Point
		found := false
		for pc, pr := range spec.Rows {
			if (absInt(pc-c) == 1 && pr == r) || (absInt(pr-r) == 1 && pc == c) {
				from, found = pipeCenter(pr, pc), true
				break
			}
		}
		if found {
			drawSegment(img, from, p, 120)
		}
		fillGrayRect(img, p.X-pipeGridCell/2+2, p.Y-pipeGridCell/2+2, pipeGridCell-4, pipeGridCell-4, 236)
		strokeGrayRect(img, image.Rect(p.X-pipeGridCell/2+2, p.Y-pipeGridCell/2+2,
			p.X+pipeGridCell/2-2, p.Y+pipeGridCell/2-2), 128)
	}
	// 主路径节点；关键断点擦空，保留浅色边框。
	for c, p := range pathPts {
		idx := spec.Rows[c]*pipeGridCols + c + 1
		if containsInt(spec.KeyCells, idx) {
			fillGrayRect(img, p.X-pipeGridCell/2+2, p.Y-pipeGridCell/2+2, pipeGridCell-4, pipeGridCell-4, 236)
			// 断点与死路必须**长得一样**：如果断点用更深的边框，答案就写在图上，
			// 盲猜模型（C(E,2)）和人工题目都失去意义。
			strokeGrayRect(img, image.Rect(p.X-pipeGridCell/2+2, p.Y-pipeGridCell/2+2,
				p.X+pipeGridCell/2-2, p.Y+pipeGridCell/2-2), 128)
			continue
		}
		fillCircle(img, p.X, p.Y, 7, 46)
	}
	// 起点/终点标记：左侧实心、右侧空心，避免文字依赖。
	fillCircle(img, pathPts[0].X, pathPts[0].Y, 11, 230)
	strokeCircle(img, pathPts[0].X, pathPts[0].Y, 11, 20)
	fillCircle(img, pathPts[len(pathPts)-1].X, pathPts[len(pathPts)-1].Y, 11, 236)
	strokeCircle(img, pathPts[len(pathPts)-1].X, pathPts[len(pathPts)-1].Y, 11, 20)
	strokeCircle(img, pathPts[len(pathPts)-1].X, pathPts[len(pathPts)-1].Y, 7, 20)
	rng := mrand.New(mrand.NewSource(spec.VisualSeed))
	for i := 0; i < 30; i++ {
		x, y := rng.Intn(pipeSize), rng.Intn(pipeSize)
		if img.GrayAt(x, y).Y > 220 {
			img.SetGray(x, y, color.Gray{Y: uint8(224 + rng.Intn(8))})
		}
	}
	return encodeGrayPNG(img)
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
