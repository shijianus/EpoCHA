package generator

// silhouette-assembly-v1：5 部件 / 5 槽位的离散二维装配（2026-11-12，ADR-030 Wave 2）。
//
// 部件是 3×3 栅格上的程序化 polyomino；槽位显示的是同一个形状旋转后的轮廓。用户选择部件、
// 用独立按钮旋转，再点击槽位放置；答案只提交 slot→(公开 piece ID, rotation bucket)，
// 不提交浮点坐标。生成器枚举 piece × slot × rotation，只在恰好一个完整匹配时接受 seed。
//
// 为什么槽位轮廓要旋转：如果槽位与部件都以基准方向显示，"旋转"就退化成装饰，答案空间
// 只剩 5!。把槽位按随机 0/90/180/270 显示，旋转才参与答案，同时仍保持唯一解。
// 为什么排除旋转对称部件：一个部件若有 2 个旋转与自身相同，同一个槽位就会有多个合法 rotation，
// 唯一解不成立。

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
	SilhouetteAssemblyID      = "silhouette-assembly-v1"
	silhouetteAssemblyVersion = "v1"

	silhouettePieces = 5
	silhouetteSlots  = 5

	SilhouetteWidth  = 400
	SilhouetteHeight = 260

	silhouettePieceW    = 54
	silhouettePieceH    = 54
	silhouettePieceCell = 18
	silhouettePieceY    = 18
	silhouettePieceX    = 45
	silhouettePieceGap  = 10

	silhouetteSlotW    = 64
	silhouetteSlotH    = 64
	silhouetteSlotCell = 20
	silhouetteSlotY    = 160
	silhouetteSlotX    = 20
	silhouetteSlotGap  = 10

	silhouetteMaxAnswerLen = 32
	silhouetteMaxAttempts  = 32
)

// silhouetteCatalog 是 3×3 栅格上的候选 polyomino（bit r*3+c）。名称只用于测试/文档；
// 运行时不依赖名称。NewSilhouetteAssembly 会证明它们连通、无旋转对称、两两不在 D4 下等价。
var silhouetteCatalog = [...]struct {
	name string
	mask uint16
}{
	{"T-tetromino", 1<<0 | 1<<1 | 1<<2 | 1<<4},
	{"L-tetromino", 1<<0 | 1<<3 | 1<<6 | 1<<7},
	{"S-tetromino", 1<<1 | 1<<2 | 1<<3 | 1<<4},
	{"U-pentomino", 1<<0 | 1<<2 | 1<<3 | 1<<4 | 1<<5},
	{"P-pentomino", 1<<0 | 1<<1 | 1<<3 | 1<<4 | 1<<6},
	{"F-pentomino", 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<7},
	{"W-pentomino", 1<<0 | 1<<3 | 1<<4 | 1<<7 | 1<<8},
	{"V-pentomino", 1<<0 | 1<<3 | 1<<6 | 1<<7 | 1<<8},
	{"T-pentomino", 1<<0 | 1<<1 | 1<<2 | 1<<4 | 1<<7},
}

type silhouettePieceSpec struct {
	ID    int
	Shape uint16
}

type silhouetteSlotSpec struct {
	Mask     uint16
	Piece    int
	Rotation int
}

type silhouetteSpec struct {
	Pieces [silhouettePieces]silhouettePieceSpec
	Slots  [silhouetteSlots]silhouetteSlotSpec
}

func rotateShapeMask(mask uint16, turns int) uint16 {
	turns &= 3
	var out uint16
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			if mask&(1<<uint(r*3+c)) == 0 {
				continue
			}
			nr, nc := r, c
			for t := 0; t < turns; t++ {
				nr, nc = nc, 2-nr
			}
			out |= 1 << uint(nr*3+nc)
		}
	}
	return out
}

func shapeConnected(mask uint16) bool {
	if mask == 0 || mask&^uint16(0x1ff) != 0 {
		return false
	}
	start := -1
	for i := 0; i < 9; i++ {
		if mask&(1<<uint(i)) != 0 {
			start = i
			break
		}
	}
	if start < 0 {
		return false
	}
	seen := uint16(1 << uint(start))
	queue := []int{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		r, c := cur/3, cur%3
		for _, d := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			nr, nc := r+d[0], c+d[1]
			if nr < 0 || nr >= 3 || nc < 0 || nc >= 3 {
				continue
			}
			ni := nr*3 + nc
			bit := uint16(1 << uint(ni))
			if mask&bit != 0 && seen&bit == 0 {
				seen |= bit
				queue = append(queue, ni)
			}
		}
	}
	return seen == mask
}

func shapeRotationallySymmetric(mask uint16) bool {
	for t := 1; t < 4; t++ {
		if rotateShapeMask(mask, t) == mask {
			return true
		}
	}
	return false
}

func validateSilhouetteCatalog() error {
	if len(silhouetteCatalog) < silhouettePieces {
		return fmt.Errorf("%w: silhouette 目录不足 %d 个形状", ErrBadParams, silhouettePieces)
	}
	for i, item := range silhouetteCatalog {
		bits := 0
		for v := item.mask; v != 0; v &= v - 1 {
			bits++
		}
		if bits < 4 || bits > 5 {
			return fmt.Errorf("%w: silhouette 形状 %q 有 %d 格", ErrBadParams, item.name, bits)
		}
		if !shapeConnected(item.mask) {
			return fmt.Errorf("%w: silhouette 形状 %q 不连通", ErrBadParams, item.name)
		}
		if shapeRotationallySymmetric(item.mask) {
			return fmt.Errorf("%w: silhouette 形状 %q 有旋转对称", ErrBadParams, item.name)
		}
		for j := i + 1; j < len(silhouetteCatalog); j++ {
			for t := 0; t < 4; t++ {
				if rotateShapeMask(item.mask, t) == silhouetteCatalog[j].mask {
					return fmt.Errorf("%w: silhouette 形状 %q 与 %q 在旋转 %d 下等价",
						ErrBadParams, item.name, silhouetteCatalog[j].name, t)
				}
			}
		}
	}
	return nil
}

func buildSilhouetteAssembly(entropy io.Reader) (silhouetteSpec, error) {
	var lastErr error
	for attempt := 0; attempt < silhouetteMaxAttempts; attempt++ {
		spec, err := buildSilhouetteAssemblyOnce(entropy)
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
	return silhouetteSpec{}, fmt.Errorf("%w: silhouette-assembly 在 %d 次尝试后失败: %v",
		ErrGenerateExhausted, silhouetteMaxAttempts, lastErr)
}

func buildSilhouetteAssemblyOnce(entropy io.Reader) (silhouetteSpec, error) {
	var spec silhouetteSpec
	// 从目录无放回抽 5 个形状。
	perm := make([]int, len(silhouetteCatalog))
	for i := range perm {
		perm[i] = i
	}
	for i := len(perm) - 1; i > 0; i-- {
		j, err := drawInt(entropy, 0, int64(i))
		if err != nil {
			return spec, err
		}
		perm[i], perm[j] = perm[j], perm[i]
	}
	shapes := make([]uint16, silhouettePieces)
	for i := range shapes {
		shapes[i] = silhouetteCatalog[perm[i]].mask
	}
	// 公开 piece ID 是 0..4 的随机排列；它与托盘位置无关，答案必须按 ID 提交。
	ids := []int{0, 1, 2, 3, 4}
	for i := len(ids) - 1; i > 0; i-- {
		j, err := drawInt(entropy, 0, int64(i))
		if err != nil {
			return spec, err
		}
		ids[i], ids[j] = ids[j], ids[i]
	}
	for i := range spec.Pieces {
		spec.Pieces[i] = silhouettePieceSpec{ID: ids[i], Shape: shapes[i]}
	}
	// 槽位顺序独立洗牌；每个槽位的显示轮廓是源部件的随机旋转。
	slotOrder := []int{0, 1, 2, 3, 4}
	for i := len(slotOrder) - 1; i > 0; i-- {
		j, err := drawInt(entropy, 0, int64(i))
		if err != nil {
			return spec, err
		}
		slotOrder[i], slotOrder[j] = slotOrder[j], slotOrder[i]
	}
	pieceMasks := make([]uint16, silhouettePieces)
	slotMasks := make([]uint16, silhouetteSlots)
	for i, p := range spec.Pieces {
		pieceMasks[i] = p.Shape
	}
	for slot, piece := range slotOrder {
		rot, err := drawInt(entropy, 0, 3)
		if err != nil {
			return spec, err
		}
		mask := rotateShapeMask(shapes[piece], int(rot))
		slotMasks[slot] = mask
		spec.Slots[slot] = silhouetteSlotSpec{Mask: mask, Piece: piece, Rotation: int(rot)}
	}
	if countAssemblies(pieceMasks, slotMasks) != 1 {
		return spec, fmt.Errorf("%w: 装配解不唯一", ErrBadParams)
	}
	return spec, nil
}

// countAssemblies 枚举所有 piece→slot 与 rotation 的完整匹配，最多数到 2。
// 节点上限由 5! × 4^5 = 122,880 的固定结构保证；不需要额外 DFS cap。
func countAssemblies(pieces []uint16, slots []uint16) int {
	if len(pieces) != len(slots) || len(pieces) == 0 || len(pieces) > 9 {
		return 0
	}
	type cand struct {
		piece int
		rot   int
	}
	options := make([][]cand, len(slots))
	for s, slot := range slots {
		for p, piece := range pieces {
			for r := 0; r < 4; r++ {
				if rotateShapeMask(piece, r) == slot {
					options[s] = append(options[s], cand{p, r})
				}
			}
		}
		if len(options[s]) == 0 {
			return 0
		}
	}
	used := make([]bool, len(pieces))
	count := 0
	var walk func(slot int)
	walk = func(slot int) {
		if count > 1 {
			return
		}
		if slot == len(slots) {
			count++
			return
		}
		for _, c := range options[slot] {
			if used[c.piece] {
				continue
			}
			used[c.piece] = true
			walk(slot + 1)
			used[c.piece] = false
		}
	}
	walk(0)
	return count
}

func silhouetteAnswer(spec silhouetteSpec) string {
	parts := make([]string, silhouetteSlots)
	for slot, s := range spec.Slots {
		parts[slot] = strconv.Itoa(spec.Pieces[s.Piece].ID) + ":" + strconv.Itoa(s.Rotation)
	}
	return VersionPrefix + strings.Join(parts, ",")
}

type SilhouettePiecePublic struct {
	ID string `json:"id"`
	X  int    `json:"x"`
	Y  int    `json:"y"`
	W  int    `json:"w"`
	H  int    `json:"h"`
}

type SilhouetteSlotPublic struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type SilhouetteAssemblyPublic struct {
	Kind         string                  `json:"kind"`
	AnswerType   string                  `json:"answer_type"`
	MaxAnswerLen int                     `json:"max_answer_len"`
	ImageWidth   int                     `json:"image_width"`
	ImageHeight  int                     `json:"image_height"`
	Pieces       []SilhouettePiecePublic `json:"pieces"`
	Slots        []SilhouetteSlotPublic  `json:"slots"`
	InputMode    string                  `json:"input_mode"`
	PromptZH     string                  `json:"prompt_zh"`
	PromptEN     string                  `json:"prompt_en"`
}

func (SilhouetteAssemblyPublic) PublicKind() string { return "silhouette-assembly" }

type SilhouetteAssembly struct{ macKey []byte }

var _ Generator = SilhouetteAssembly{}

func NewSilhouetteAssembly(macKey []byte) (SilhouetteAssembly, error) {
	if len(macKey) < 32 {
		return SilhouetteAssembly{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	if err := validateSilhouetteCatalog(); err != nil {
		return SilhouetteAssembly{}, err
	}
	return SilhouetteAssembly{macKey: macKey}, nil
}

func (SilhouetteAssembly) ID() string      { return SilhouetteAssemblyID }
func (SilhouetteAssembly) Version() string { return silhouetteAssemblyVersion }
func (SilhouetteAssembly) Media() string   { return MediaImage }

func silhouettePieceRect(i int) image.Rectangle {
	x := silhouettePieceX + i*(silhouettePieceW+silhouettePieceGap)
	return image.Rect(x, silhouettePieceY, x+silhouettePieceW, silhouettePieceY+silhouettePieceH)
}

func silhouetteSlotRect(i int) image.Rectangle {
	x := silhouetteSlotX + i*(silhouetteSlotW+silhouetteSlotGap)
	return image.Rect(x, silhouetteSlotY, x+silhouetteSlotW, silhouetteSlotY+silhouetteSlotH)
}

func (g SilhouetteAssembly) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	spec, err := buildSilhouetteAssembly(entropy)
	if err != nil {
		return Question{}, err
	}
	nonce := make([]byte, verifierNonceSz)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	asset, err := renderSilhouetteAssembly(spec)
	if err != nil {
		return Question{}, err
	}
	pieces := make([]SilhouettePiecePublic, silhouettePieces)
	for i, p := range spec.Pieces {
		r := silhouettePieceRect(i)
		pieces[i] = SilhouettePiecePublic{
			ID: strconv.Itoa(p.ID), X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy(),
		}
	}
	slots := make([]SilhouetteSlotPublic, silhouetteSlots)
	for i := range slots {
		r := silhouetteSlotRect(i)
		slots[i] = SilhouetteSlotPublic{X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy()}
	}
	return Question{
		Payload: SilhouetteAssemblyPublic{
			Kind: "silhouette-assembly", AnswerType: AnswerTypePlacements,
			MaxAnswerLen: silhouetteMaxAnswerLen,
			ImageWidth:   SilhouetteWidth, ImageHeight: SilhouetteHeight,
			Pieces: pieces, Slots: slots, InputMode: "placements",
			PromptZH: "选择部件，用“旋转”调整方向，再点击匹配的槽位；五个部件都要放好。",
			PromptEN: "Select a piece, use Rotate to change its orientation, then click the matching slot. Place all five pieces.",
		},
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, silhouetteAnswer(spec))),
		Asset:    asset, MimeType: MimeTypePNG,
	}, nil
}

func (g SilhouetteAssembly) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

func (SilhouetteAssembly) NormalizeAnswer(input string) (string, bool) {
	return NormalizePlacements(input, silhouetteSlots, silhouettePieces)
}

func (SilhouetteAssembly) SanitizePublic(raw []byte) (map[string]any, error) {
	var p SilhouetteAssemblyPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch {
	case p.Kind != "silhouette-assembly" || p.AnswerType != AnswerTypePlacements:
		return nil, fmt.Errorf("generator: silhouette-assembly 题面类型不合法")
	case p.MaxAnswerLen != silhouetteMaxAnswerLen:
		return nil, fmt.Errorf("generator: silhouette-assembly 答案长度不合法")
	case p.ImageWidth != SilhouetteWidth || p.ImageHeight != SilhouetteHeight:
		return nil, fmt.Errorf("generator: silhouette-assembly 图像尺寸不合法")
	case p.InputMode != "placements":
		return nil, fmt.Errorf("generator: silhouette-assembly 输入模式不合法")
	case len(p.Pieces) != silhouettePieces || len(p.Slots) != silhouetteSlots:
		return nil, fmt.Errorf("generator: silhouette-assembly 部件/槽位数量不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: silhouette-assembly 提示不合法")
	}
	seenIDs := map[string]bool{}
	for i, piece := range p.Pieces {
		if len(piece.ID) != 1 || piece.ID[0] < '0' || piece.ID[0] > '4' || seenIDs[piece.ID] {
			return nil, fmt.Errorf("generator: silhouette-assembly 部件 ID 不合法")
		}
		seenIDs[piece.ID] = true
		r := silhouettePieceRect(i)
		if piece.X != r.Min.X || piece.Y != r.Min.Y || piece.W != r.Dx() || piece.H != r.Dy() {
			return nil, fmt.Errorf("generator: silhouette-assembly 部件几何不合法")
		}
	}
	for i, slot := range p.Slots {
		r := silhouetteSlotRect(i)
		if slot.X != r.Min.X || slot.Y != r.Min.Y || slot.W != r.Dx() || slot.H != r.Dy() {
			return nil, fmt.Errorf("generator: silhouette-assembly 槽位几何不合法")
		}
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"pieces", "slots", "input_mode", "prompt_zh", "prompt_en":
		default:
			return nil, fmt.Errorf("generator: silhouette-assembly 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"pieces": p.Pieces, "slots": p.Slots, "input_mode": p.InputMode,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
	}, nil
}

func renderSilhouetteAssembly(spec silhouetteSpec) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, SilhouetteWidth, SilhouetteHeight))
	fillGrayRect(img, 0, 0, SilhouetteWidth, SilhouetteHeight, 238)
	// 托盘
	fillGrayRect(img, 10, 8, SilhouetteWidth-20, silhouettePieceH+20, 248)
	strokeGrayRect(img, image.Rect(10, 8, SilhouetteWidth-10, 8+silhouettePieceH+20), 190)
	// 槽位面板
	fillGrayRect(img, 10, silhouetteSlotY-10, SilhouetteWidth-20, silhouetteSlotH+20, 248)
	strokeGrayRect(img, image.Rect(10, silhouetteSlotY-10, SilhouetteWidth-10, silhouetteSlotY+silhouetteSlotH+10), 190)
	for i, piece := range spec.Pieces {
		drawSilhouettePiece(img, silhouettePieceRect(i), piece.Shape, true)
	}
	for i, slot := range spec.Slots {
		drawSilhouetteSlotShape(img, silhouetteSlotRect(i), slot.Mask)
	}
	return encodeGrayPNG(img)
}

func drawSilhouettePiece(img *image.Gray, rect image.Rectangle, shape uint16, filled bool) {
	fillGrayRect(img, rect.Min.X, rect.Min.Y, rect.Dx(), rect.Dy(), 252)
	strokeGrayRect(img, rect, 180)
	drawSilhouetteShape(img, rect, shape, filled, 0)
}

func drawSilhouetteSlotShape(img *image.Gray, rect image.Rectangle, shape uint16) {
	fillGrayRect(img, rect.Min.X, rect.Min.Y, rect.Dx(), rect.Dy(), 246)
	strokeGrayRect(img, rect, 180)
	drawSilhouetteShape(img, rect, shape, false, 2)
}

func drawSilhouetteShape(img *image.Gray, rect image.Rectangle, shape uint16, filled bool, inset int) {
	cell := silhouettePieceCell
	if rect.Dx() != silhouettePieceW {
		cell = silhouetteSlotCell
	}
	mx := rect.Min.X + (rect.Dx()-3*cell)/2
	my := rect.Min.Y + (rect.Dy()-3*cell)/2
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			if shape&(1<<uint(r*3+c)) == 0 {
				continue
			}
			x := mx + c*cell + inset
			y := my + r*cell + inset
			w := cell - 2*inset
			if filled {
				fillGrayRect(img, x, y, w, w, 96)
				strokeGrayRect(img, image.Rect(x, y, x+w, y+w), 28)
			} else {
				fillGrayRect(img, x, y, w, w, 226)
				strokeGrayRect(img, image.Rect(x, y, x+w, y+w), 44)
				// 斜纹：槽位不是只靠颜色与部件区分，同时有轮廓和纹理。
				for k := 3; k < w; k += 5 {
					drawThickSegment(img, image.Pt(x+k, y+1), image.Pt(x+1, y+k), 1, 170)
				}
			}
		}
	}
}

func SilhouetteAssemblyAnswerFrom(entropy io.Reader) (string, error) {
	spec, err := buildSilhouetteAssembly(entropy)
	if err != nil {
		return "", err
	}
	return silhouetteAnswer(spec), nil
}
