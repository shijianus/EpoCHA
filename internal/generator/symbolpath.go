package generator

// symbol-path-v1：6×6 符号路径（2026-11-12，ADR-030）。
//
// 题面是一张服务端渲染的 PNG 雪碧图：顶部是长度 6–8 的目标符号序列，下面是 6×6 网格。
// 用户按目标顺序连接上下左右相邻、且不重复的格子；不允许对角线（键盘可操作）。
// 服务端先生成唯一目标路径、填入目标符号，再用 DFS 统计所有与目标序列匹配的合法路径；
// 只有 matching path count == 1 的 seed 才被接受。
//
// 公开 payload 只含网格几何、目标条几何与 36 个公开随机 cell ID；不含每格语义 symbol ID、
// 目标路径或答案。答案规范化为 v1:NN.NN...，判分走 48B 精确 HMAC。

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
	SymbolPathID      = "symbol-path-v1"
	symbolPathVersion = "v1"

	symbolPathCols      = 6
	symbolPathRows      = 6
	symbolPathCellCount = symbolPathCols * symbolPathRows

	SymbolPathWidth  = 320
	SymbolPathHeight = 360

	symbolPathMargin  = 20
	symbolPathCell    = 44
	symbolPathGap     = 3
	symbolPathGridY   = 70
	symbolPathTargetY = 12
	symbolPathTargetH = 48

	symbolPathMinLen = 6
	symbolPathMaxLen = 8

	symbolPathMaxAnswerLen = 32
	symbolPathMaxAttempts  = 96
	symbolPathNodeCap      = 1 << 18
	symbolPathMinPositions = 128

	symbolGlyphCount = 8
)

type symbolPathSpec struct {
	Grid               [symbolPathCellCount]int
	Path               []int
	Target             []int
	CellIDs            []string
	MatchingPaths      int
	PositionCandidates int
}

func symbolPathAnswer(path []int) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = fmt.Sprintf("%02d", p)
	}
	return VersionPrefix + strings.Join(parts, ".")
}

func parseSymbolPathIndices(canon string) ([]int, bool) {
	if !strings.HasPrefix(canon, VersionPrefix) {
		return nil, false
	}
	body := canon[len(VersionPrefix):]
	if body == "" {
		return nil, false
	}
	parts := strings.Split(body, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		if len(p) != 2 || p[0] < '0' || p[0] > '9' || p[1] < '0' || p[1] > '9' {
			return nil, false
		}
		out[i] = int(p[0]-'0')*10 + int(p[1]-'0')
	}
	return out, true
}

func manhattan(a, b int) int {
	ar, ac := a/symbolPathCols, a%symbolPathCols
	br, bc := b/symbolPathCols, b%symbolPathCols
	return absInt(ar-br) + absInt(ac-bc)
}

func symbolPathAnswerFromSpec(s symbolPathSpec) string { return symbolPathAnswer(s.Path) }

// drawSelfAvoidingPath 用随机化回溯生成一条长度恰好 need 的自避路径。
// 有节点上限；6×6 上 need≤8 时几乎总能立即找到。
func drawSelfAvoidingPath(entropy io.Reader, need int) ([]int, error) {
	if need < 1 || need > symbolPathCellCount {
		return nil, fmt.Errorf("%w: path 长度 %d", ErrBadParams, need)
	}
	start, err := drawInt(entropy, 0, symbolPathCellCount-1)
	if err != nil {
		return nil, err
	}
	used := make([]bool, symbolPathCellCount)
	path := []int{int(start)}
	used[start] = true
	nodes := 0
	var walk func() bool
	walk = func() bool {
		if len(path) == need {
			return true
		}
		if nodes > symbolPathNodeCap {
			return false
		}
		cur := path[len(path)-1]
		r, c := cur/symbolPathCols, cur%symbolPathCols
		opts := make([]int, 0, 4)
		for _, d := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			nr, nc := r+d[0], c+d[1]
			if nr < 0 || nr >= symbolPathRows || nc < 0 || nc >= symbolPathCols {
				continue
			}
			ni := nr*symbolPathCols + nc
			if !used[ni] {
				opts = append(opts, ni)
			}
		}
		for i := 0; i < len(opts); i++ {
			j, err := drawInt(entropy, int64(i), int64(len(opts)-1))
			if err != nil {
				return false
			}
			opts[i], opts[j] = opts[j], opts[i]
		}
		for _, ni := range opts {
			nodes++
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
	if !walk() {
		return nil, fmt.Errorf("%w: 生成自避路径失败", ErrGenerateExhausted)
	}
	return path, nil
}

// countSelfAvoidingPaths 统计所有起点、所有长度 need 的自避路径数量，达到 cap 即停。
// 它是 M3 的「合法位置路径候选」下界：调用方要求至少 128。
func countSelfAvoidingPaths(need, cap int) int {
	if need < 1 || cap < 1 {
		return 0
	}
	total := 0
	used := make([]bool, symbolPathCellCount)
	var walk func(cur, depth int)
	walk = func(cur, depth int) {
		if total >= cap {
			return
		}
		if depth == need {
			total++
			return
		}
		r, c := cur/symbolPathCols, cur%symbolPathCols
		for _, d := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			nr, nc := r+d[0], c+d[1]
			if nr < 0 || nr >= symbolPathRows || nc < 0 || nc >= symbolPathCols {
				continue
			}
			ni := nr*symbolPathCols + nc
			if used[ni] {
				continue
			}
			used[ni] = true
			walk(ni, depth+1)
			used[ni] = false
		}
	}
	for start := 0; start < symbolPathCellCount && total < cap; start++ {
		used[start] = true
		walk(start, 1)
		used[start] = false
	}
	return total
}

// countMatchingPaths 统计网格中所有与 target 符号序列匹配的合法自避路径；最多数到 2。
func countMatchingPaths(grid [symbolPathCellCount]int, target []int) int {
	if len(target) == 0 {
		return 0
	}
	total := 0
	used := make([]bool, symbolPathCellCount)
	var walk func(cur, depth int)
	walk = func(cur, depth int) {
		if total > 1 {
			return
		}
		if depth == len(target) {
			total++
			return
		}
		r, c := cur/symbolPathCols, cur%symbolPathCols
		for _, d := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			nr, nc := r+d[0], c+d[1]
			if nr < 0 || nr >= symbolPathRows || nc < 0 || nc >= symbolPathCols {
				continue
			}
			ni := nr*symbolPathCols + nc
			if used[ni] || grid[ni] != target[depth] {
				continue
			}
			used[ni] = true
			walk(ni, depth+1)
			used[ni] = false
		}
	}
	for start := 0; start < symbolPathCellCount && total <= 1; start++ {
		if grid[start] != target[0] {
			continue
		}
		used[start] = true
		walk(start, 1)
		used[start] = false
	}
	return total
}

func buildSymbolPath(entropy io.Reader) (symbolPathSpec, error) {
	var lastErr error
	for attempt := 0; attempt < symbolPathMaxAttempts; attempt++ {
		spec, err := buildSymbolPathOnce(entropy)
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
	return symbolPathSpec{}, fmt.Errorf("%w: symbol-path 在 %d 次尝试后没有找到唯一路径: %v",
		ErrGenerateExhausted, symbolPathMaxAttempts, lastErr)
}

func buildSymbolPathOnce(entropy io.Reader) (symbolPathSpec, error) {
	var spec symbolPathSpec
	length, err := drawInt(entropy, symbolPathMinLen, symbolPathMaxLen)
	if err != nil {
		return spec, err
	}
	positions := countSelfAvoidingPaths(int(length), symbolPathMinPositions)
	if positions < symbolPathMinPositions {
		return spec, fmt.Errorf("%w: 位置候选只有 %d", ErrBadParams, positions)
	}
	path, err := drawSelfAvoidingPath(entropy, int(length))
	if err != nil {
		return spec, err
	}
	var glyphs [symbolGlyphCount]int
	for i := range glyphs {
		glyphs[i] = i
	}
	for i := len(glyphs) - 1; i > 0; i-- {
		j, err := drawInt(entropy, 0, int64(i))
		if err != nil {
			return spec, err
		}
		glyphs[i], glyphs[j] = glyphs[j], glyphs[i]
	}
	target := make([]int, len(path))
	for i := range path {
		target[i] = glyphs[i]
	}
	for i := 0; i < symbolPathCellCount; i++ {
		spec.Grid[i] = -1
	}
	for i, cell := range path {
		spec.Grid[cell] = target[i]
	}
	for i := 0; i < symbolPathCellCount; i++ {
		if spec.Grid[i] >= 0 {
			continue
		}
		v, err := drawInt(entropy, 0, symbolGlyphCount-1)
		if err != nil {
			return spec, err
		}
		spec.Grid[i] = int(v)
	}
	matching := countMatchingPaths(spec.Grid, target)
	if matching != 1 {
		return spec, fmt.Errorf("%w: matching paths=%d", ErrBadParams, matching)
	}
	spec.Path = path
	spec.Target = target
	spec.MatchingPaths = matching
	spec.PositionCandidates = positions
	seen := map[string]bool{}
	for len(spec.CellIDs) < symbolPathCellCount {
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

type SymbolPathPublic struct {
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
	TargetLen    int      `json:"target_len"`
	TargetY      int      `json:"target_y"`
	TargetH      int      `json:"target_h"`
	CellIDs      []string `json:"cell_ids"`
	InputMode    string   `json:"input_mode"`
	PromptZH     string   `json:"prompt_zh"`
	PromptEN     string   `json:"prompt_en"`
}

func (SymbolPathPublic) PublicKind() string { return "symbol-path" }

type SymbolPath struct{ macKey []byte }

var _ Generator = SymbolPath{}

func NewSymbolPath(macKey []byte) (SymbolPath, error) {
	if len(macKey) < 32 {
		return SymbolPath{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	if err := validateSymbolGlyphs(); err != nil {
		return SymbolPath{}, err
	}
	return SymbolPath{macKey: macKey}, nil
}

func (SymbolPath) ID() string      { return SymbolPathID }
func (SymbolPath) Version() string { return symbolPathVersion }
func (SymbolPath) Media() string   { return MediaImage }

func (g SymbolPath) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	spec, err := buildSymbolPath(entropy)
	if err != nil {
		return Question{}, err
	}
	nonce := make([]byte, verifierNonceSz)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	asset, err := renderSymbolPath(spec)
	if err != nil {
		return Question{}, err
	}
	gridX := (SymbolPathWidth - (symbolPathCols*symbolPathCell + (symbolPathCols-1)*symbolPathGap)) / 2
	return Question{
		Payload: SymbolPathPublic{
			Kind: "symbol-path", AnswerType: AnswerTypePath, MaxAnswerLen: symbolPathMaxAnswerLen,
			ImageWidth: SymbolPathWidth, ImageHeight: SymbolPathHeight,
			GridCols: symbolPathCols, GridRows: symbolPathRows,
			GridX: gridX, GridY: symbolPathGridY,
			GridCell: symbolPathCell, GridGap: symbolPathGap,
			TargetLen: len(spec.Target), TargetY: symbolPathTargetY, TargetH: symbolPathTargetH,
			CellIDs: append([]string(nil), spec.CellIDs...), InputMode: "path",
			PromptZH: "按顶部目标符号的顺序，依次连接相邻的格子；不能重复，也不能走对角线。",
			PromptEN: "Trace the target symbol sequence through adjacent cells in order; no repeats or diagonals.",
		},
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, symbolPathAnswerFromSpec(spec))),
		Asset:    asset, MimeType: MimeTypePNG,
	}, nil
}

func (g SymbolPath) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

func (g SymbolPath) NormalizeAnswer(input string) (string, bool) {
	canon, ok := NormalizePath(input, symbolPathCellCount, symbolPathMinLen, symbolPathMaxLen)
	if !ok {
		return "", false
	}
	idx, ok := parseSymbolPathIndices(canon)
	if !ok {
		return "", false
	}
	for i := 1; i < len(idx); i++ {
		if manhattan(idx[i-1], idx[i]) != 1 {
			return "", false
		}
	}
	return canon, true
}

func (SymbolPath) SanitizePublic(raw []byte) (map[string]any, error) {
	var p SymbolPathPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	gridX := (SymbolPathWidth - (symbolPathCols*symbolPathCell + (symbolPathCols-1)*symbolPathGap)) / 2
	switch {
	case p.Kind != "symbol-path" || p.AnswerType != AnswerTypePath:
		return nil, fmt.Errorf("generator: symbol-path 题面类型不合法")
	case p.MaxAnswerLen != symbolPathMaxAnswerLen:
		return nil, fmt.Errorf("generator: symbol-path 答案长度不合法")
	case p.ImageWidth != SymbolPathWidth || p.ImageHeight != SymbolPathHeight:
		return nil, fmt.Errorf("generator: symbol-path 图像尺寸不合法")
	case p.GridCols != symbolPathCols || p.GridRows != symbolPathRows ||
		p.GridX != gridX || p.GridY != symbolPathGridY ||
		p.GridCell != symbolPathCell || p.GridGap != symbolPathGap:
		return nil, fmt.Errorf("generator: symbol-path 网格几何不合法")
	case p.TargetLen < symbolPathMinLen || p.TargetLen > symbolPathMaxLen:
		return nil, fmt.Errorf("generator: symbol-path 目标长度不合法")
	case p.TargetY != symbolPathTargetY || p.TargetH != symbolPathTargetH:
		return nil, fmt.Errorf("generator: symbol-path 目标条几何不合法")
	case p.InputMode != "path":
		return nil, fmt.Errorf("generator: symbol-path 输入模式不合法")
	case len(p.CellIDs) != symbolPathCellCount:
		return nil, fmt.Errorf("generator: symbol-path cell ID 数量不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: symbol-path 提示不合法")
	}
	seen := map[string]bool{}
	for _, id := range p.CellIDs {
		if len(id) != 4 || seen[id] {
			return nil, fmt.Errorf("generator: symbol-path cell ID 不合法")
		}
		for i := 0; i < len(id); i++ {
			if _, ok := asciiHexNibble(id[i]); !ok {
				return nil, fmt.Errorf("generator: symbol-path cell ID 含非法字符")
			}
		}
		seen[id] = true
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"grid_cols", "grid_rows", "grid_x", "grid_y", "grid_cell", "grid_gap",
			"target_len", "target_y", "target_h", "cell_ids", "input_mode",
			"prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: symbol-path 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"grid_cols": p.GridCols, "grid_rows": p.GridRows,
		"grid_x": p.GridX, "grid_y": p.GridY, "grid_cell": p.GridCell, "grid_gap": p.GridGap,
		"target_len": p.TargetLen, "target_y": p.TargetY, "target_h": p.TargetH,
		"cell_ids": p.CellIDs, "input_mode": p.InputMode,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

// --- 符号字形 ---

func drawSymbolGlyph(img *image.Gray, kind, cx, cy, size int, ink uint8) {
	if size < 16 {
		size = 16
	}
	half := size / 2
	switch kind % symbolGlyphCount {
	case 0: // 圆环 + 三点
		strokeCircle(img, cx, cy, half-6, ink)
		strokeCircle(img, cx, cy, half-3, ink)
		fillCircle(img, cx-7, cy, 2, ink)
		fillCircle(img, cx, cy, 2, ink)
		fillCircle(img, cx+7, cy, 2, ink)
	case 1: // 三角形轮廓 + 横纹
		drawThickSegment(img, image.Pt(cx, cy-half+4), image.Pt(cx+half-4, cy+half-4), 3, ink)
		drawThickSegment(img, image.Pt(cx+half-4, cy+half-4), image.Pt(cx-half+4, cy+half-4), 3, ink)
		drawThickSegment(img, image.Pt(cx-half+4, cy+half-4), image.Pt(cx, cy-half+4), 3, ink)
		for i := -1; i <= 1; i++ {
			drawThickSegment(img, image.Pt(cx-6, cy+i*5), image.Pt(cx+6, cy+i*5), 2, ink)
		}
	case 2: // 方形轮廓 + 斜纹
		drawThickSegment(img, image.Pt(cx-half+4, cy-half+4), image.Pt(cx+half-4, cy-half+4), 3, ink)
		drawThickSegment(img, image.Pt(cx+half-4, cy-half+4), image.Pt(cx+half-4, cy+half-4), 3, ink)
		drawThickSegment(img, image.Pt(cx+half-4, cy+half-4), image.Pt(cx-half+4, cy+half-4), 3, ink)
		drawThickSegment(img, image.Pt(cx-half+4, cy+half-4), image.Pt(cx-half+4, cy-half+4), 3, ink)
		for i := -1; i <= 1; i++ {
			drawThickSegment(img, image.Pt(cx-half+7, cy+i*6), image.Pt(cx+half-7, cy+i*6-6), 2, ink)
		}
	case 3: // 十字 + 中心点
		drawThickSegment(img, image.Pt(cx, cy-half+4), image.Pt(cx, cy+half-4), 6, ink)
		drawThickSegment(img, image.Pt(cx-half+4, cy), image.Pt(cx+half-4, cy), 6, ink)
		fillCircle(img, cx, cy, 3, 236)
	case 4: // 菱形轮廓 + 竖纹
		drawThickSegment(img, image.Pt(cx, cy-half+3), image.Pt(cx+half-3, cy), 3, ink)
		drawThickSegment(img, image.Pt(cx+half-3, cy), image.Pt(cx, cy+half-3), 3, ink)
		drawThickSegment(img, image.Pt(cx, cy+half-3), image.Pt(cx-half+3, cy), 3, ink)
		drawThickSegment(img, image.Pt(cx-half+3, cy), image.Pt(cx, cy-half+3), 3, ink)
		for i := -1; i <= 1; i++ {
			drawThickSegment(img, image.Pt(cx+i*6, cy-7), image.Pt(cx+i*6, cy+7), 2, ink)
		}
	case 5: // L 形 + 两点
		drawThickSegment(img, image.Pt(cx-half+5, cy-half+4), image.Pt(cx-half+5, cy+half-4), 6, ink)
		drawThickSegment(img, image.Pt(cx-half+5, cy+half-4), image.Pt(cx+half-4, cy+half-4), 6, ink)
		fillCircle(img, cx+6, cy-6, 2, ink)
		fillCircle(img, cx+10, cy-1, 2, ink)
	case 6: // 六边形轮廓 + 内环
		pts := []image.Point{
			{cx - half + 5, cy}, {cx - half/2, cy - half + 4},
			{cx + half/2, cy - half + 4}, {cx + half - 5, cy},
			{cx + half/2, cy + half - 4}, {cx - half/2, cy + half - 4},
		}
		for i := 0; i < len(pts); i++ {
			drawThickSegment(img, pts[i], pts[(i+1)%len(pts)], 3, ink)
		}
		strokeCircle(img, cx, cy, 6, ink)
	case 7: // T 形 + 棋盘点
		drawThickSegment(img, image.Pt(cx-half+4, cy-half+5), image.Pt(cx+half-4, cy-half+5), 6, ink)
		drawThickSegment(img, image.Pt(cx, cy-half+5), image.Pt(cx, cy+half-4), 6, ink)
		for i := -1; i <= 1; i++ {
			for j := -1; j <= 1; j += 2 {
				fillCircle(img, cx+i*7, cy+6+j*4, 2, ink)
			}
		}
	}
	// 左上角方向缺口：让任何图形在 90°/180°/270° 旋转或镜像下都不再与自身（或其他字形）
	// 视觉等价。它是字形的一部分，不是答案提示。
	fillGrayRect(img, cx-half+2, cy-half+2, 5, 5, ink)
}

func glyphBits(kind, size int) []bool {
	img := image.NewGray(image.Rect(0, 0, size, size))
	fillGrayRect(img, 0, 0, size, size, 255)
	drawSymbolGlyph(img, kind, size/2, size/2, size-4, 0)
	out := make([]bool, size*size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			out[y*size+x] = img.GrayAt(x, y).Y < 128
		}
	}
	return out
}

func transformBits(bits []bool, size, transform int) []bool {
	out := make([]bool, size*size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var nx, ny int
			switch transform {
			case 0:
				nx, ny = x, y
			case 1:
				nx, ny = size-1-y, x
			case 2:
				nx, ny = size-1-x, size-1-y
			case 3:
				nx, ny = y, size-1-x
			case 4:
				nx, ny = size-1-x, y
			case 5:
				nx, ny = x, size-1-y
			case 6:
				nx, ny = y, x
			case 7:
				nx, ny = size-1-y, size-1-x
			}
			out[ny*size+nx] = bits[y*size+x]
		}
	}
	return out
}

func sameBits(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// validateSymbolGlyphs 在启动时证明 8 个字形两两不在 D4（旋转/镜像）下等价。
// 这是构造性检查，不是每 seed 的随机检查；失败时服务拒绝启动。
func validateSymbolGlyphs() error {
	const size = 34
	bits := make([][]bool, symbolGlyphCount)
	for i := 0; i < symbolGlyphCount; i++ {
		bits[i] = glyphBits(i, size)
		ink := 0
		for _, b := range bits[i] {
			if b {
				ink++
			}
		}
		if ink < 8 {
			return fmt.Errorf("%w: symbol glyph %d 墨迹过少", ErrBadParams, i)
		}
	}
	for i := 0; i < symbolGlyphCount; i++ {
		for j := i + 1; j < symbolGlyphCount; j++ {
			for t := 0; t < 8; t++ {
				if sameBits(bits[i], transformBits(bits[j], size, t)) {
					return fmt.Errorf("%w: symbol glyph %d 与 %d 在 D4 变换 %d 下等价",
						ErrBadParams, i, j, t)
				}
			}
		}
	}
	return nil
}

func renderSymbolPath(spec symbolPathSpec) ([]byte, error) {
	if len(spec.Path) == 0 || len(spec.Target) != len(spec.Path) {
		return nil, fmt.Errorf("%w: symbol-path spec 不完整", ErrBadParams)
	}
	img := image.NewGray(image.Rect(0, 0, SymbolPathWidth, SymbolPathHeight))
	fillGrayRect(img, 0, 0, SymbolPathWidth, SymbolPathHeight, 238)
	gridX := (SymbolPathWidth - (symbolPathCols*symbolPathCell + (symbolPathCols-1)*symbolPathGap)) / 2
	// 目标条
	targetCell := 36
	targetGap := 4
	total := len(spec.Target)*targetCell + (len(spec.Target)-1)*targetGap
	tx := (SymbolPathWidth - total) / 2
	for i, g := range spec.Target {
		x := tx + i*(targetCell+targetGap)
		fillGrayRect(img, x, symbolPathTargetY, targetCell, targetCell, 250)
		strokeGrayRect(img, image.Rect(x, symbolPathTargetY, x+targetCell, symbolPathTargetY+targetCell), 190)
		drawSymbolGlyph(img, g, x+targetCell/2, symbolPathTargetY+targetCell/2, 32, 30)
	}
	// 网格
	for r := 0; r < symbolPathRows; r++ {
		for c := 0; c < symbolPathCols; c++ {
			x := gridX + c*(symbolPathCell+symbolPathGap)
			y := symbolPathGridY + r*(symbolPathCell+symbolPathGap)
			fillGrayRect(img, x, y, symbolPathCell, symbolPathCell, 248)
			strokeGrayRect(img, image.Rect(x, y, x+symbolPathCell, y+symbolPathCell), 200)
			drawSymbolGlyph(img, spec.Grid[r*symbolPathCols+c], x+symbolPathCell/2, y+symbolPathCell/2, 34, 28)
		}
	}
	return encodeGrayPNG(img)
}

// 让测试/评测可以按同一熵顺序取答案。
func SymbolPathAnswerFrom(entropy io.Reader) (string, error) {
	spec, err := buildSymbolPath(entropy)
	if err != nil {
		return "", err
	}
	return symbolPathAnswer(spec.Path), nil
}
