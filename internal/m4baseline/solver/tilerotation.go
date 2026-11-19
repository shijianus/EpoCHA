package solver

// tile-rotation-v1 baseline：公开 payload 直接给了每格的 initial_masks（4-bit 边掩码）。
// 攻击器不需要看图：在每格的 4 个旋转里做带回溯的约束搜索，只保留边界不出边、
// 相邻边成对、最终 9 格连通的解。生成器保证唯一解，所以搜索一旦找到唯一解就结束。
//
// 这不是"识别图像"，而是**公开结构化字段直接泄漏了题目状态**：服务端为了让客户端
// 能渲染旋转交互，把初始掩码放进了 payload，而答案只是它的一个合法旋转组合。

const (
	tileCells = 9
	tileRows  = 3
	tileCols  = 3

	edgeN = 1 << 0
	edgeE = 1 << 1
	edgeS = 1 << 2
	edgeW = 1 << 3

	tileNodeCap = 1 << 20
)

func solveTileRotation(publicJSON []byte) Outcome {
	d, err := loadPublic(publicJSON)
	if err != nil {
		return parseFailure(err.Error())
	}
	maskStr, ok := docString(d.raw, "initial_masks")
	if !ok || len(maskStr) != tileCells {
		return parseFailure("initial_masks 缺失或长度不为 9")
	}
	initial := make([]uint8, tileCells)
	for i := 0; i < tileCells; i++ {
		v, ok := hexNibble(maskStr[i])
		if !ok {
			return parseFailure("initial_masks 含非十六进制字符")
		}
		initial[i] = v & 0x0f
	}
	solutions, exhausted := searchTileBoards(initial)
	if exhausted {
		return giveUp("tile 搜索达到节点上限")
	}
	switch len(solutions) {
	case 1:
		return answer(tileAnswerString(solutions[0]))
	case 0:
		return giveUp("没有闭合连通的旋转组合")
	default:
		return giveUp("解不唯一")
	}
}

func hexNibble(c byte) (uint8, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

func rotateMaskCW(m uint8) uint8 {
	m &= 0x0f
	return ((m << 1) & 0x0f) | ((m >> 3) & 0x01)
}

func distinctRotations(m uint8) []uint8 {
	out := make([]uint8, 0, 4)
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

func searchTileBoards(initial []uint8) ([][]uint8, bool) {
	options := make([][]uint8, tileCells)
	for i, m := range initial {
		options[i] = distinctRotations(m)
	}
	cur := make([]uint8, tileCells)
	var out [][]uint8
	nodes := 0
	var walk func(i int) bool
	walk = func(i int) bool {
		if nodes > tileNodeCap {
			return false
		}
		if i == tileCells {
			if tileBoardClosedAndConnected(cur) {
				sol := append([]uint8(nil), cur...)
				out = append(out, sol)
			}
			return len(out) < 2
		}
		r, c := i/tileCols, i%tileCols
		for _, m := range options[i] {
			nodes++
			if r == 0 && m&edgeN != 0 {
				continue
			}
			if r == tileRows-1 && m&edgeS != 0 {
				continue
			}
			if c == 0 && m&edgeW != 0 {
				continue
			}
			if c == tileCols-1 && m&edgeE != 0 {
				continue
			}
			if c > 0 {
				left := cur[i-1]
				if (m&edgeW != 0) != (left&edgeE != 0) {
					continue
				}
			}
			if r > 0 {
				above := cur[i-tileCols]
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
		return out, true
	}
	return out, false
}

func tileBoardClosedAndConnected(masks []uint8) bool {
	if len(masks) != tileCells {
		return false
	}
	for r := 0; r < tileRows; r++ {
		for c := 0; c < tileCols; c++ {
			i := r*tileCols + c
			m := masks[i] & 0x0f
			if r == 0 && m&edgeN != 0 {
				return false
			}
			if r == tileRows-1 && m&edgeS != 0 {
				return false
			}
			if c == 0 && m&edgeW != 0 {
				return false
			}
			if c == tileCols-1 && m&edgeE != 0 {
				return false
			}
			if c+1 < tileCols {
				right := masks[i+1] & 0x0f
				if (m&edgeE != 0) != (right&edgeW != 0) {
					return false
				}
			}
			if r+1 < tileRows {
				below := masks[i+tileCols] & 0x0f
				if (m&edgeS != 0) != (below&edgeN != 0) {
					return false
				}
			}
		}
	}
	seen := make([]bool, tileCells)
	queue := []int{0}
	seen[0] = true
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		r, c := i/tileCols, i%tileCols
		visit := func(j int) {
			if j >= 0 && j < tileCells && !seen[j] {
				seen[j] = true
				queue = append(queue, j)
			}
		}
		if masks[i]&edgeN != 0 && r > 0 {
			visit(i - tileCols)
		}
		if masks[i]&edgeE != 0 && c+1 < tileCols {
			visit(i + 1)
		}
		if masks[i]&edgeS != 0 && r+1 < tileRows {
			visit(i + tileCols)
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

func tileAnswerString(masks []uint8) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 3+tileCells)
	out = append(out, 'v', '1', ':')
	for _, m := range masks {
		out = append(out, hex[m&0x0f])
	}
	return string(out)
}
