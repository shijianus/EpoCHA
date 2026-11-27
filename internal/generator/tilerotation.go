package generator

// tile-rotation-v1：3×3 路径块旋转（2026-11-12，ADR-030）。
//
// 定位：实验型交互题，不是抗 AI/抗 OCR 证明。服务端先从一棵随机生成树构造一张连通、
// 不越界的正交网络，再枚举每个 tile 的不同旋转，只在「恰好一个合法最终状态」时接受 seed。
// 客户端只提交每个格子的最终 4-bit edge mask；答案规范化为 v1:<9 hex>，判分走 48B 精确 HMAC。
//
// 为什么不用「看起来连通」的启发式判分：多解 seed 会让「所有格子看起来连上」变成错误接受。
// 唯一解由 countTileSolutions 的枚举证明；达到节点上限时拒绝该 seed，不继续无限搜索。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
)

const (
	TileRotationID      = "tile-rotation-v1"
	tileRotationVersion = "v1"

	tileRotationRows  = 3
	tileRotationCols  = 3
	tileRotationCells = tileRotationRows * tileRotationCols

	TileRotationWidth  = 320
	TileRotationHeight = 320
	tileRotationMargin = 10
	tileRotationCell   = 96
	tileRotationGap    = 4

	tileRotationMaxAnswerLen = 16
	tileRotationMaxAttempts  = 64
	tileRotationNodeCap      = 1 << 20
)

type edgeMask uint8

const (
	edgeN edgeMask = 1 << iota
	edgeE
	edgeS
	edgeW
)

func rotateMaskCW(m edgeMask) edgeMask {
	m &= 0x0f
	return ((m << 1) & 0x0f) | ((m >> 3) & 0x01)
}

func rotateMask(m edgeMask, turns uint8) edgeMask {
	for i := uint8(0); i < turns%4; i++ {
		m = rotateMaskCW(m)
	}
	return m
}

// distinctRotations 返回 mask 在 0/90/180/270 下的不同取值。对称 tile 只出现一次或两次，
// 枚举与候选空间都必须按去重后的数量算，否则会把等价旋转误当成多个解。
func distinctRotations(m edgeMask) []edgeMask {
	out := make([]edgeMask, 0, 4)
	seen := [16]bool{}
	cur := m & 0x0f
	for i := 0; i < 4; i++ {
		if !seen[cur] {
			seen[cur] = true
			out = append(out, cur)
		}
		cur = rotateMaskCW(cur)
	}
	return out
}

func tileAnswer(masks []edgeMask) string {
	var b strings.Builder
	b.WriteString(VersionPrefix)
	const hex = "0123456789abcdef"
	for _, m := range masks {
		b.WriteByte(hex[m&0x0f])
	}
	return b.String()
}

func tileAnswerFromSpec(s tileRotationSpec) string { return tileAnswer(s.Solution[:]) }

// boardIsConnectedAndClosed 是参考实现的严格版本：边界不得有出边、相邻边必须成对、
// 所有格子必须属于同一连通分量。
func boardIsConnectedAndClosed(masks []edgeMask, rows, cols int) bool {
	if rows <= 0 || cols <= 0 || len(masks) != rows*cols {
		return false
	}
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			i := r*cols + c
			m := masks[i] & 0x0f
			if r == 0 && m&edgeN != 0 {
				return false
			}
			if r == rows-1 && m&edgeS != 0 {
				return false
			}
			if c == 0 && m&edgeW != 0 {
				return false
			}
			if c == cols-1 && m&edgeE != 0 {
				return false
			}
			if c+1 < cols {
				right := masks[i+1] & 0x0f
				if (m&edgeE != 0) != (right&edgeW != 0) {
					return false
				}
			}
			if r+1 < rows {
				below := masks[i+cols] & 0x0f
				if (m&edgeS != 0) != (below&edgeN != 0) {
					return false
				}
			}
		}
	}
	seen := make([]bool, len(masks))
	queue := make([]int, 0, len(masks))
	seen[0] = true
	queue = append(queue, 0)
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		r, c := i/cols, i%cols
		visit := func(j int) {
			if j >= 0 && j < len(masks) && !seen[j] {
				seen[j] = true
				queue = append(queue, j)
			}
		}
		if masks[i]&edgeN != 0 && r > 0 {
			visit(i - cols)
		}
		if masks[i]&edgeE != 0 && c+1 < cols {
			visit(i + 1)
		}
		if masks[i]&edgeS != 0 && r+1 < rows {
			visit(i + cols)
		}
		if masks[i]&edgeW != 0 && c > 0 {
			visit(i - 1)
		}
	}
	for _, ok := range seen {
		if !ok {
			return false
		}
	}
	return true
}

// countTileSolutions 枚举 initial 每格的不同旋转，统计不同最终 mask 向量中满足
// 「闭合 + 连通」的数量。返回 ok=false 表示节点上限耗尽（调用方必须拒绝该 seed）。
func countTileSolutions(initial []edgeMask, rows, cols int) (count int, ok bool) {
	cells := rows * cols
	if len(initial) != cells {
		return 0, false
	}
	options := make([][]edgeMask, cells)
	for i, m := range initial {
		options[i] = distinctRotations(m)
	}
	cur := make([]edgeMask, cells)
	seen := map[string]struct{}{}
	nodes := 0
	var walk func(i int) bool
	walk = func(i int) bool {
		if nodes > tileRotationNodeCap {
			return false
		}
		if i == cells {
			if !boardIsConnectedAndClosed(cur, rows, cols) {
				return true
			}
			key := tileAnswer(cur)
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				count++
				if count > 1 {
					return false
				}
			}
			return true
		}
		r, c := i/cols, i%cols
		for _, m := range options[i] {
			nodes++
			if r == 0 && m&edgeN != 0 {
				continue
			}
			if r == rows-1 && m&edgeS != 0 {
				continue
			}
			if c == 0 && m&edgeW != 0 {
				continue
			}
			if c == cols-1 && m&edgeE != 0 {
				continue
			}
			if c > 0 {
				left := cur[i-1] & 0x0f
				if (m&edgeW != 0) != (left&edgeE != 0) {
					continue
				}
			}
			if r > 0 {
				above := cur[i-cols] & 0x0f
				if (m&edgeN != 0) != (above&edgeS != 0) {
					continue
				}
			}
			cur[i] = m
			if !walk(i + 1) {
				return false
			}
		}
		return true
	}
	if !walk(0) {
		return count, false
	}
	return count, true
}

type tileRotationSpec struct {
	Solution       [tileRotationCells]edgeMask
	Initial        [tileRotationCells]edgeMask
	CandidateSpace int
	SolutionCount  int
}

// buildTileRotation 按固定熵顺序构造一个唯一解 seed。失败是**可重试的生成失败**，
// 绝不降级成固定题；调用方（题池 worker）按退避处理。
func buildTileRotation(entropy io.Reader) (tileRotationSpec, error) {
	var lastErr error
	for attempt := 0; attempt < tileRotationMaxAttempts; attempt++ {
		spec, err := buildTileRotationOnce(entropy)
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
	return tileRotationSpec{}, fmt.Errorf("%w: tile-rotation 在 %d 次尝试后没有找到唯一解 seed: %v",
		ErrGenerateExhausted, tileRotationMaxAttempts, lastErr)
}

func buildTileRotationOnce(entropy io.Reader) (tileRotationSpec, error) {
	var spec tileRotationSpec
	solution := make([]edgeMask, tileRotationCells)

	// 随机生成树：随机起点 + 随机化 DFS。树保证 9 格全部连通且每格至少有一条边。
	var visited [tileRotationCells]bool
	start, err := drawInt(entropy, 0, tileRotationCells-1)
	if err != nil {
		return spec, err
	}
	stack := []int{int(start)}
	visited[start] = true
	edges := map[[2]int]bool{}
	dirs := [][3]int{{-1, 0, int(edgeS)}, {1, 0, int(edgeN)}, {0, -1, int(edgeE)}, {0, 1, int(edgeW)}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		r, c := cur/tileRotationCols, cur%tileRotationCols
		cands := make([]int, 0, 4)
		for _, d := range dirs {
			nr, nc := r+d[0], c+d[1]
			if nr < 0 || nr >= tileRotationRows || nc < 0 || nc >= tileRotationCols {
				continue
			}
			ni := nr*tileRotationCols + nc
			if !visited[ni] {
				cands = append(cands, ni)
			}
		}
		if len(cands) == 0 {
			stack = stack[:len(stack)-1]
			continue
		}
		idx, err := drawInt(entropy, 0, int64(len(cands)-1))
		if err != nil {
			return spec, err
		}
		ni := cands[idx]
		edges[[2]int{cur, ni}] = true
		visited[ni] = true
		stack = append(stack, ni)
	}

	// 0..2 条额外边：保留树的可连通性，同时制造少量环，避免所有题都是树。
	extra, err := drawInt(entropy, 0, 2)
	if err != nil {
		return spec, err
	}
	for k := 0; k < int(extra); k++ {
		a, err := drawInt(entropy, 0, tileRotationCells-1)
		if err != nil {
			return spec, err
		}
		ar, ac := int(a)/tileRotationCols, int(a)%tileRotationCols
		var opts []int
		for _, d := range dirs {
			nr, nc := ar+d[0], ac+d[1]
			if nr < 0 || nr >= tileRotationRows || nc < 0 || nc >= tileRotationCols {
				continue
			}
			ni := nr*tileRotationCols + nc
			if !edges[[2]int{int(a), ni}] && !edges[[2]int{ni, int(a)}] {
				opts = append(opts, ni)
			}
		}
		if len(opts) == 0 {
			continue
		}
		pick, err := drawInt(entropy, 0, int64(len(opts)-1))
		if err != nil {
			return spec, err
		}
		edges[[2]int{int(a), opts[pick]}] = true
	}
	for e := range edges {
		a, b := e[0], e[1]
		ar, ac := a/tileRotationCols, a%tileRotationCols
		br, bc := b/tileRotationCols, b%tileRotationCols
		switch {
		case br == ar-1 && bc == ac:
			solution[a] |= edgeN
			solution[b] |= edgeS
		case br == ar+1 && bc == ac:
			solution[a] |= edgeS
			solution[b] |= edgeN
		case bc == ac-1 && br == ar:
			solution[a] |= edgeW
			solution[b] |= edgeE
		case bc == ac+1 && br == ar:
			solution[a] |= edgeE
			solution[b] |= edgeW
		}
	}

	// 初始旋转：每格从自己的不同旋转里抽一个。若整体恰好还是解，强制至少一格换一个
	// 非等价旋转；若所有格子都只有一种旋转，则这个 seed 没有可玩的旋转，重试。
	candidateSpace := 1
	rotOpts := make([][]edgeMask, tileRotationCells)
	for i := range solution {
		rotOpts[i] = distinctRotations(solution[i])
		candidateSpace *= len(rotOpts[i])
	}
	if candidateSpace < 64 {
		return spec, fmt.Errorf("%w: 候选空间 %d", ErrBadParams, candidateSpace)
	}
	initial := make([]edgeMask, tileRotationCells)
	allSolved := true
	for i := range solution {
		idx, err := drawInt(entropy, 0, int64(len(rotOpts[i])-1))
		if err != nil {
			return spec, err
		}
		initial[i] = rotOpts[i][idx]
		if initial[i] != solution[i] {
			allSolved = false
		}
	}
	if allSolved {
		for i := range solution {
			if len(rotOpts[i]) > 1 {
				initial[i] = rotOpts[i][1]
				allSolved = false
				break
			}
		}
		if allSolved {
			return spec, fmt.Errorf("%w: 没有可旋转的 tile", ErrBadParams)
		}
	}

	count, ok := countTileSolutions(initial, tileRotationRows, tileRotationCols)
	if !ok || count != 1 {
		return spec, fmt.Errorf("%w: 解数 %d ok=%v", ErrBadParams, count, ok)
	}
	copy(spec.Solution[:], solution)
	copy(spec.Initial[:], initial)
	spec.CandidateSpace = candidateSpace
	spec.SolutionCount = count
	return spec, nil
}

type TileRotationPublic struct {
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
	InitialMasks string `json:"initial_masks"`
	Style        string `json:"style"`
	InputMode    string `json:"input_mode"`
	PromptZH     string `json:"prompt_zh"`
	PromptEN     string `json:"prompt_en"`
}

func (TileRotationPublic) PublicKind() string { return "tile-rotation" }

type TileRotation struct{ macKey []byte }

var _ Generator = TileRotation{}

func NewTileRotation(macKey []byte) (TileRotation, error) {
	if len(macKey) < 32 {
		return TileRotation{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	return TileRotation{macKey: macKey}, nil
}

func (TileRotation) ID() string      { return TileRotationID }
func (TileRotation) Version() string { return tileRotationVersion }
func (TileRotation) Media() string   { return MediaImage }

func (g TileRotation) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	spec, err := buildTileRotation(entropy)
	if err != nil {
		return Question{}, err
	}
	// Vary the visible route geometry while keeping rotation ports at the edge
	// midpoints. These points affect every drawn path, never the answer masks.
	var junctions [tileRotationCells]image.Point
	for i := range junctions {
		x, err := drawInt(entropy, -12, 12)
		if err != nil {
			return Question{}, err
		}
		y, err := drawInt(entropy, -12, 12)
		if err != nil {
			return Question{}, err
		}
		junctions[i] = image.Pt(int(x), int(y))
	}
	nonce := make([]byte, verifierNonceSz)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	asset, err := renderTileRotation(spec.Initial[:], junctions)
	if err != nil {
		return Question{}, err
	}
	return Question{
		Payload: TileRotationPublic{
			Kind: "tile-rotation", AnswerType: AnswerTypeMasks, MaxAnswerLen: tileRotationMaxAnswerLen,
			ImageWidth: TileRotationWidth, ImageHeight: TileRotationHeight,
			GridCols: tileRotationCols, GridRows: tileRotationRows,
			GridX: tileRotationMargin, GridY: tileRotationMargin,
			GridCell: tileRotationCell, GridGap: tileRotationGap,
			InitialMasks: tileInitialMasks(spec.Initial[:]),
			Style:        "network-v1", InputMode: "rotate",
			PromptZH: "旋转每一个路径块，使所有路径连成一张完整的网络，且没有边伸出边界。",
			PromptEN: "Rotate every tile so all paths form one connected network with no edge on the outer border.",
		},
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, tileAnswerFromSpec(spec))),
		Asset:    asset, MimeType: MimeTypePNG,
	}, nil
}

func tileInitialMasks(masks []edgeMask) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(masks))
	for i, m := range masks {
		out[i] = hex[m&0x0f]
	}
	return string(out)
}

// TileRotationAnswerFrom 按与 Generate 相同的熵顺序取答案（离线评测/夹具用）。
func TileRotationAnswerFrom(entropy io.Reader) (string, error) {
	spec, err := buildTileRotation(entropy)
	if err != nil {
		return "", err
	}
	return tileAnswerFromSpec(spec), nil
}

func (g TileRotation) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

func (TileRotation) NormalizeAnswer(input string) (string, bool) {
	return NormalizeMasks(input, tileRotationCells)
}

func (TileRotation) SanitizePublic(raw []byte) (map[string]any, error) {
	var p TileRotationPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch {
	case p.Kind != "tile-rotation" || p.AnswerType != AnswerTypeMasks:
		return nil, fmt.Errorf("generator: tile-rotation 题面类型不合法")
	case p.MaxAnswerLen != tileRotationMaxAnswerLen:
		return nil, fmt.Errorf("generator: tile-rotation 答案长度不合法")
	case p.ImageWidth != TileRotationWidth || p.ImageHeight != TileRotationHeight:
		return nil, fmt.Errorf("generator: tile-rotation 图像尺寸不合法")
	case p.GridCols != tileRotationCols || p.GridRows != tileRotationRows ||
		p.GridX != tileRotationMargin || p.GridY != tileRotationMargin ||
		p.GridCell != tileRotationCell || p.GridGap != tileRotationGap:
		return nil, fmt.Errorf("generator: tile-rotation 网格几何不合法")
	case len(p.InitialMasks) != tileRotationCells:
		return nil, fmt.Errorf("generator: tile-rotation 初始 masks 长度不合法")
	case p.Style != "network-v1" || p.InputMode != "rotate":
		return nil, fmt.Errorf("generator: tile-rotation 样式/输入模式不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: tile-rotation 提示不合法")
	}
	for i := 0; i < len(p.InitialMasks); i++ {
		if _, ok := asciiHexNibble(p.InitialMasks[i]); !ok {
			return nil, fmt.Errorf("generator: tile-rotation 初始 masks 含非法字符")
		}
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"grid_cols", "grid_rows", "grid_x", "grid_y", "grid_cell", "grid_gap",
			"initial_masks", "style", "input_mode", "prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: tile-rotation 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"grid_cols": p.GridCols, "grid_rows": p.GridRows,
		"grid_x": p.GridX, "grid_y": p.GridY, "grid_cell": p.GridCell, "grid_gap": p.GridGap,
		"initial_masks": p.InitialMasks, "style": p.Style, "input_mode": p.InputMode,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

func tileRotationCenter(r, c int) image.Point {
	return image.Pt(
		tileRotationMargin+c*(tileRotationCell+tileRotationGap)+tileRotationCell/2,
		tileRotationMargin+r*(tileRotationCell+tileRotationGap)+tileRotationCell/2,
	)
}

func renderTileRotation(masks []edgeMask, junctions [tileRotationCells]image.Point) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, TileRotationWidth, TileRotationHeight))
	fillGrayRect(img, 0, 0, TileRotationWidth, TileRotationHeight, 236)
	for r := 0; r < tileRotationRows; r++ {
		for c := 0; c < tileRotationCols; c++ {
			idx := r*tileRotationCols + c
			p := tileRotationCenter(r, c)
			junction := p.Add(junctions[idx])
			rect := image.Rect(p.X-tileRotationCell/2, p.Y-tileRotationCell/2,
				p.X+tileRotationCell/2, p.Y+tileRotationCell/2)
			fillGrayRect(img, rect.Min.X, rect.Min.Y, tileRotationCell, tileRotationCell, 228)
			strokeGrayRect(img, rect, 196)
			m := masks[idx] & 0x0f
			half := tileRotationCell / 2
			if m&edgeN != 0 {
				drawThickSegment(img, junction, image.Pt(p.X, p.Y-half), 4, 46)
			}
			if m&edgeE != 0 {
				drawThickSegment(img, junction, image.Pt(p.X+half, p.Y), 4, 46)
			}
			if m&edgeS != 0 {
				drawThickSegment(img, junction, image.Pt(p.X, p.Y+half), 4, 46)
			}
			if m&edgeW != 0 {
				drawThickSegment(img, junction, image.Pt(p.X-half, p.Y), 4, 46)
			}
			// 方向标记：靠 N 侧的小点，旋转后位置改变，对称路径块也能看出朝向。
			fillCircle(img, p.X, p.Y-22, 3, 40)
		}
	}
	return encodeGrayPNG(img)
}

func drawThickSegment(img *image.Gray, a, b image.Point, thickness int, v uint8) {
	if thickness < 1 {
		thickness = 1
	}
	lo := -(thickness / 2)
	hi := lo + thickness - 1
	for dx := lo; dx <= hi; dx++ {
		for dy := lo; dy <= hi; dy++ {
			drawSegment(img, image.Pt(a.X+dx, a.Y+dy), image.Pt(b.X+dx, b.Y+dy), v)
		}
	}
}
