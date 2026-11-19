package solver

import (
	"fmt"
	"image"
	"sort"
)

// visual-rank-v1 baseline：每张卡是一条画在 5×4 节点栅格上的轴对齐折线，
// 线段数就是答案的排序键。payload 给了卡片包围盒，卡片内可见的节点间距是 30px
// （card_h/4）。攻击器在每条栅格边中段采样暗像素，得到一张二值边图；若边图不是
// 一条简单路径就放弃。计数只依赖公开图片的几何，不读 generator 的线段数。

const visualRankDark = 100

type rankCard struct {
	id     string
	edges  int
	valid  bool
	reason string
}

func solveVisualRank(publicJSON []byte) Outcome {
	d, err := loadPublic(publicJSON)
	if err != nil {
		return parseFailure(err.Error())
	}
	cardsRaw, ok := docObjects(d.raw, "cards")
	if !ok || len(cardsRaw) < 2 {
		return parseFailure("cards 缺失或数量不足")
	}
	cards := make([]rankCard, 0, len(cardsRaw))
	seen := map[string]bool{}
	for i, obj := range cardsRaw {
		id, ok := docString(obj, "id")
		if !ok || id == "" || seen[id] {
			return parseFailure(fmt.Sprintf("卡片 %d 的 id 不合法", i))
		}
		seen[id] = true
		x, okX := docInt(obj, "x")
		y, okY := docInt(obj, "y")
		w, okW := docInt(obj, "w")
		h, okH := docInt(obj, "h")
		if !okX || !okY || !okW || !okH || w < 30 || h < 30 {
			return parseFailure(fmt.Sprintf("卡片 %s 几何不合法", id))
		}
		n, reason := countCardSegments(d.img, image.Rect(x, y, x+w, y+h))
		cards = append(cards, rankCard{id: id, edges: n, valid: reason == "", reason: reason})
	}
	for _, c := range cards {
		if !c.valid {
			return giveUp(fmt.Sprintf("卡片 %s 线段检测失败: %s", c.id, c.reason))
		}
	}
	counts := make([]int, 0, len(cards))
	for _, c := range cards {
		counts = append(counts, c.edges)
	}
	sort.Ints(counts)
	for i := 1; i < len(counts); i++ {
		if counts[i] == counts[i-1] {
			return giveUp("线段计数出现重复，无法唯一排序")
		}
	}
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].edges == cards[j].edges {
			return cards[i].id < cards[j].id
		}
		return cards[i].edges < cards[j].edges
	})
	out := "v1:"
	for i, c := range cards {
		if i > 0 {
			out += "."
		}
		out += c.id
	}
	return answer(out)
}

// countCardSegments 返回卡片内检测到的线段数；valid=false 表示边图不是一条简单路径。
func countCardSegments(g *image.Gray, rect image.Rectangle) (int, string) {
	rect = rect.Intersect(g.Bounds())
	if rect.Empty() {
		return 0, "卡片包围盒越界"
	}
	// 节点栅格：高度方向 4 段、宽度方向 5 段。间距由卡片高度/4 推出（可见节点间距 30px）。
	spacing := rect.Dy() / 4
	if spacing < 10 {
		return 0, "卡片高度不足"
	}
	nodeCols := rect.Dx() / spacing
	nodeRows := rect.Dy() / spacing
	if nodeCols < 2 || nodeRows < 2 || nodeCols*nodeRows > 64 {
		return 0, "节点栅格不合法"
	}
	offX := rect.Min.X + (rect.Dx()-(nodeCols-1)*spacing)/2
	offY := rect.Min.Y + (rect.Dy()-(nodeRows-1)*spacing)/2
	// 边图：adj[node] = 邻接节点集合。
	adj := make([][]int, nodeCols*nodeRows)
	addEdge := func(a, b int) {
		adj[a] = append(adj[a], b)
		adj[b] = append(adj[b], a)
	}
	edges := 0
	for r := 0; r < nodeRows; r++ {
		for c := 0; c < nodeCols; c++ {
			a := r*nodeCols + c
			ax, ay := offX+c*spacing, offY+r*spacing
			if c+1 < nodeCols {
				bx, by := ax+spacing, ay
				if edgePresent(g, ax, ay, bx, by) {
					addEdge(a, a+1)
					edges++
				}
			}
			if r+1 < nodeRows {
				bx, by := ax, ay+spacing
				if edgePresent(g, ax, ay, bx, by) {
					addEdge(a, a+nodeCols)
					edges++
				}
			}
		}
	}
	if edges == 0 {
		return 0, "没有检测到线段"
	}
	// 简单路径一致性：度 ≤2、恰两个端点、所有有边节点连通、节点数 = 边数+1。
	active, ends := 0, 0
	for _, nb := range adj {
		if len(nb) == 0 {
			continue
		}
		active++
		switch len(nb) {
		case 1:
			ends++
		case 2:
		default:
			return 0, fmt.Sprintf("节点度数 %d 不是简单路径", len(nb))
		}
	}
	if ends != 2 || active != edges+1 {
		return 0, fmt.Sprintf("边图不是简单路径（active=%d ends=%d edges=%d）", active, ends, edges)
	}
	start := -1
	for i, nb := range adj {
		if len(nb) > 0 {
			start = i
			break
		}
	}
	seen := make([]bool, len(adj))
	queue := []int{start}
	seen[start] = true
	visited := 1
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nb := range adj[cur] {
			if !seen[nb] {
				seen[nb] = true
				visited++
				queue = append(queue, nb)
			}
		}
	}
	if visited != active {
		return 0, "边图不连通"
	}
	return edges, ""
}

// edgePresent 在边的中段采样暗像素；两端各 35% 不采，避开节点圆点与起终点标记。
func edgePresent(g *image.Gray, ax, ay, bx, by int) bool {
	hits := 0
	for _, t := range []float64{0.35, 0.5, 0.65} {
		x := int(float64(ax) + t*float64(bx-ax))
		y := int(float64(ay) + t*float64(by-ay))
		if darkNear(g, x, y, 1) {
			hits++
		}
	}
	return hits >= 2
}

func darkNear(g *image.Gray, x, y, radius int) bool {
	b := g.Bounds()
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			px, py := x+dx, y+dy
			if px < b.Min.X || px >= b.Max.X || py < b.Min.Y || py >= b.Max.Y {
				continue
			}
			if g.GrayAt(px, py).Y < visualRankDark {
				return true
			}
		}
	}
	return false
}
