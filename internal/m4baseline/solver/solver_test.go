package solver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os/exec"
	"strings"
	"testing"
)

func newTestGray(w, h int, v uint8) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for i := range g.Pix {
		g.Pix[i] = v
	}
	return g
}

func testFillRect(g *image.Gray, x, y, w, h int, v uint8) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			if image.Pt(xx, yy).In(g.Bounds()) {
				g.SetGray(xx, yy, color.Gray{Y: v})
			}
		}
	}
}

func testThickLine(g *image.Gray, x0, y0, x1, y1, thickness int, v uint8) {
	if thickness < 1 {
		thickness = 1
	}
	lo := -(thickness / 2)
	hi := lo + thickness - 1
	dx, dy := x1-x0, y1-y0
	steps := absInt(dx)
	if absInt(dy) > steps {
		steps = absInt(dy)
	}
	if steps == 0 {
		steps = 1
	}
	for i := 0; i <= steps; i++ {
		x := x0 + dx*i/steps
		y := y0 + dy*i/steps
		for oy := lo; oy <= hi; oy++ {
			for ox := lo; ox <= hi; ox++ {
				if image.Pt(x+ox, y+oy).In(g.Bounds()) {
					g.SetGray(x+ox, y+oy, color.Gray{Y: v})
				}
			}
		}
	}
}

func testDisc(g *image.Gray, cx, cy, r int, v uint8) {
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			if x*x+y*y <= r*r && image.Pt(cx+x, cy+y).In(g.Bounds()) {
				g.SetGray(cx+x, cy+y, color.Gray{Y: v})
			}
		}
	}
}

func testPNG(t *testing.T, g *image.Gray) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testPublicJSON(t *testing.T, w, h int, asset []byte, extra map[string]any) []byte {
	t.Helper()
	doc := map[string]any{
		"media":        "image",
		"image_mime":   "image/png",
		"image_width":  w,
		"image_height": h,
		"image_base64": base64.StdEncoding.EncodeToString(asset),
		"generator":    "test",
	}
	for k, v := range extra {
		doc[k] = v
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTileRotationSolverKnownBoard(t *testing.T) {
	// 一条覆盖 9 格的蛇形路径：0-1-2-5-4-3-6-7-8。
	// 掩码顺序（N=1,E=2,S=4,W=8）：2,a,c,6,a,9,3,a,8。
	asset := testPNG(t, newTestGray(320, 320, 236))
	payload := testPublicJSON(t, 320, 320, asset, map[string]any{
		"initial_masks": "2ac6a93a8",
	})
	out := Solve("tile-rotation-v1", payload)
	if out.ParseFailed || out.GaveUp {
		t.Fatalf("unexpected failure: %+v", out)
	}
	if out.Answer != "v1:2ac6a93a8" {
		t.Fatalf("answer=%q, want v1:2ac6a93a8", out.Answer)
	}
}

func TestVisualRankSolverSynthetic(t *testing.T) {
	const W, H = 500, 280
	g := newTestGray(W, H, 242)
	slots := []image.Rectangle{
		image.Rect(12, 12, 162, 132),
		image.Rect(172, 12, 322, 132),
		image.Rect(332, 12, 482, 132),
		image.Rect(95, 142, 245, 262),
		image.Rect(255, 142, 405, 262),
	}
	ids := []string{"A", "B", "C", "D", "E"}
	counts := []int{3, 9, 1, 7, 5}
	// 5×4 栅格上的固定蛇形路径，足以覆盖 1..9 段。
	serp := [][2]int{
		{0, 0}, {1, 0}, {2, 0}, {3, 0}, {4, 0}, {4, 1}, {3, 1}, {2, 1}, {1, 1}, {0, 1},
		{0, 2}, {1, 2}, {2, 2}, {3, 2}, {4, 2},
	}
	cards := make([]map[string]any, 5)
	for i, r := range slots {
		testFillRect(g, r.Min.X, r.Min.Y, r.Dx(), r.Dy(), 252)
		offX := r.Min.X + (r.Dx()-4*30)/2
		offY := r.Min.Y + (r.Dy()-3*30)/2
		for k := 0; k < counts[i]; k++ {
			a, b := serp[k], serp[k+1]
			testThickLine(g, offX+a[0]*30, offY+a[1]*30, offX+b[0]*30, offY+b[1]*30, 5, 34)
		}
		for k := 0; k <= counts[i]; k++ {
			testDisc(g, offX+serp[k][0]*30, offY+serp[k][1]*30, 4, 34)
		}
		cards[i] = map[string]any{
			"id": ids[i], "x": r.Min.X, "y": r.Min.Y, "w": r.Dx(), "h": r.Dy(),
		}
	}
	payload := testPublicJSON(t, W, H, testPNG(t, g), map[string]any{"cards": cards})
	out := Solve("visual-rank-v1", payload)
	if out.ParseFailed || out.GaveUp {
		t.Fatalf("unexpected failure: %+v", out)
	}
	want := "v1:C.A.E.D.B" // counts 1,3,5,7,9
	if out.Answer != want {
		t.Fatalf("answer=%q, want %q", out.Answer, want)
	}
}

func TestSilhouetteSolverSynthetic(t *testing.T) {
	// 目录里的 5 个形状（与生成器同为 3×3 bitmask）。
	shapes := []uint16{
		1<<0 | 1<<1 | 1<<2 | 1<<4,        // T
		1<<0 | 1<<3 | 1<<6 | 1<<7,        // L
		1<<1 | 1<<2 | 1<<3 | 1<<4,        // S
		1<<0 | 1<<2 | 1<<3 | 1<<4 | 1<<5, // U
		1<<0 | 1<<1 | 1<<3 | 1<<4 | 1<<6, // P
	}
	const W, H = 400, 260
	g := newTestGray(W, H, 238)
	pieces := make([]map[string]any, 5)
	for i, shape := range shapes {
		x, y := 45+i*64, 18
		testFillRect(g, x, y, 54, 54, 252)
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				if shape&(1<<uint(r*3+c)) != 0 {
					testFillRect(g, x+c*18, y+r*18, 18, 18, 96)
				}
			}
		}
		pieces[i] = map[string]any{"id": string(rune('0' + i)), "x": x, "y": y, "w": 54, "h": 54}
	}
	rots := []int{0, 1, 2, 3, 0}
	slots := make([]map[string]any, 5)
	for i, shape := range shapes {
		x, y := 20+i*74, 160
		testFillRect(g, x, y, 64, 64, 246)
		mask := rotTest(shape, rots[i])
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				if mask&(1<<uint(r*3+c)) != 0 {
					testFillRect(g, x+2+c*20+2, y+2+r*20+2, 16, 16, 226)
				}
			}
		}
		slots[i] = map[string]any{"x": x, "y": y, "w": 64, "h": 64}
	}
	payload := testPublicJSON(t, W, H, testPNG(t, g), map[string]any{
		"pieces": pieces, "slots": slots,
	})
	out := Solve("silhouette-assembly-v1", payload)
	if out.ParseFailed || out.GaveUp {
		t.Fatalf("unexpected failure: %+v", out)
	}
	want := "v1:0:0,1:1,2:2,3:3,4:0"
	if out.Answer != want {
		t.Fatalf("answer=%q, want %q", out.Answer, want)
	}
}

func rotTest(mask uint16, turns int) uint16 {
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

func TestParseFailures(t *testing.T) {
	if out := Solve("tile-rotation-v1", []byte(`{}`)); !out.ParseFailed {
		t.Fatalf("empty DTO should be a parse failure: %+v", out)
	}
	if out := Solve("no-such-generator", []byte(`{}`)); !out.ParseFailed {
		t.Fatalf("unknown generator should be a parse failure: %+v", out)
	}
}

// TestSolverDoesNotImportGenerator 钉住"攻击器不知道 verifier/生成器私有结构"：
// solver 包只能依赖标准库，不能 import internal/generator。
func TestSolverDoesNotImportGenerator(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not in PATH")
	}
	cmd := exec.Command(goBin, "list", "-deps", ".")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := string(out)
	if strings.Contains(deps, "github.com/shijianus/EpoCHA/internal/generator") {
		t.Fatalf("solver imports internal/generator:\n%s", deps)
	}
}
