package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"math"
	"strconv"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func intersectionStream(t *testing.T, seed string) *bytes.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf)
}

// intersectionCellInput 把格中心编码成规范 point；Verify 的量化必须回到同一格。
func intersectionCellInput(s intersectionSpec, cx, cy int) string {
	nx := ((cx*2 + 1) * 999) / (2 * s.Cols)
	ny := ((cy*2 + 1) * 999) / (2 * s.Rows)
	return strconv.Itoa(nx) + "," + strconv.Itoa(ny)
}

func TestIntersectionPointDeterministicAndAnswer(t *testing.T) {
	g, err := NewIntersectionPoint(bytes.Repeat([]byte{0x91}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		seed := "intersection-" + strconv.Itoa(d)
		answer, err := IntersectionPointAnswerFrom(intersectionStream(t, seed), d)
		if err != nil {
			t.Fatal(err)
		}
		q, err := g.Generate(context.Background(), Params{Difficulty: d}, intersectionStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		q2, err := g.Generate(context.Background(), Params{Difficulty: d}, intersectionStream(t, seed))
		if err != nil {
			t.Fatal(err)
		}
		p1, _ := json.Marshal(q.Payload)
		p2, _ := json.Marshal(q2.Payload)
		if !bytes.Equal(p1, p2) || !bytes.Equal(q.Verifier, q2.Verifier) || !bytes.Equal(q.Asset, q2.Asset) {
			t.Fatalf("d%d 同 seed 生成不逐字节一致", d)
		}
		if !g.Verify(q.Verifier, answer) {
			t.Fatalf("d%d 答案 %q 未通过", d, answer)
		}
		pub := q.Payload.(IntersectionPointPublic)
		if pub.AnswerType != AnswerTypePoint || pub.MaxAnswerLen != intersectionMaxAnswerLen {
			t.Fatalf("d%d 题面字段不符: %+v", d, pub)
		}
		if pub.ImageWidth != intersectionW || pub.ImageHeight != intersectionH {
			t.Fatalf("d%d 题面尺寸 = %dx%d", d, pub.ImageWidth, pub.ImageHeight)
		}
		cols, rows, err := IntersectionPointTier(d)
		if err != nil {
			t.Fatal(err)
		}
		gotCols, gotRows, tsv, ok := parseIntersectionVerifier(q.Verifier)
		if !ok || gotCols != cols || gotRows != rows {
			t.Fatalf("d%d verifier 网格 = %d×%d (%v)，期望 %d×%d", d, gotCols, gotRows, ok, cols, rows)
		}
		count, ok := TSVCount(tsv)
		if !ok || count != 9 || count > TSVMaxTags {
			t.Fatalf("d%d TSV 标签数 = %d (%v)，期望 9 且 ≤%d", d, count, ok, TSVMaxTags)
		}
		if len(q.Verifier) > 8+TSVMaxBytes {
			t.Fatalf("d%d verifier %d 字节超限", d, len(q.Verifier))
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(q.Asset))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Width != intersectionW || cfg.Height != intersectionH {
			t.Fatalf("d%d 资产尺寸 = %dx%d", d, cfg.Width, cfg.Height)
		}
		if len(q.Asset) > MaxImageAssetBytes {
			t.Fatalf("d%d 资产 %d 超限", d, len(q.Asset))
		}
		t.Logf("intersection d%d asset=%d verifier=%d", d, len(q.Asset), len(q.Verifier))
	}
}

func TestIntersectionPointTierAndM3(t *testing.T) {
	lastArea := 0
	lastP := 1.0
	for d := 1; d <= MaxDifficulties; d++ {
		cols, rows, err := IntersectionPointTier(d)
		if err != nil {
			t.Fatal(err)
		}
		area := cols * rows
		if area <= lastArea {
			t.Fatalf("d%d 网格面积 %d 未随难度增大（上一档 %d）", d, area, lastArea)
		}
		lastArea = area
		// 接受域固定 3×3 格。M3 必须按**目标格的可行范围**算（边距把目标限制在
		// 网格中央约 54%），按整张网格算会低估随机猜测通过率。
		minCX, maxCX, minCY, maxCY := intersectionCellBounds(cols, rows)
		feasible := float64((maxCX - minCX + 1) * (maxCY - minCY + 1))
		if feasible <= 0 {
			t.Fatalf("d%d 没有可行的目标格", d)
		}
		p := 9.0 / feasible
		p3 := 1 - math.Pow(1-p, 3)
		if p >= 0.10 || p3 >= 0.10 {
			t.Fatalf("d%d M3 超线: feasible=%.0f p=%.5f p3=%.5f", d, feasible, p, p3)
		}
		// 更保守的上界：3 次点击各覆盖一个 3×3 接受域，最多覆盖 27 个目标格。
		if tile := 27.0 / feasible; tile >= 0.10 {
			t.Fatalf("d%d 三次点击的覆盖上界 %.4f 超线", d, tile)
		}
		if p >= lastP {
			t.Fatalf("d%d M3 %.5f 未随难度下降（上一档 %.5f）", d, p, lastP)
		}
		lastP = p
		t.Logf("intersection d%d grid=%dx%d M3=%.4f%%/%.4f%%", d, cols, rows, p*100, p3*100)
	}
	if _, _, err := IntersectionPointTier(0); err == nil {
		t.Fatal("难度 0 应拒绝")
	}
	if _, _, err := IntersectionPointTier(MaxDifficulties + 1); err == nil {
		t.Fatal("超出难度上限应拒绝")
	}
}

func TestIntersectionPointAcceptanceNeighborhood(t *testing.T) {
	g, _ := NewIntersectionPoint(bytes.Repeat([]byte{0x92}, 32))
	seed := "intersection-neighborhood"
	spec, nonce, err := buildIntersectionPoint(intersectionStream(t, seed), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != 16 {
		t.Fatalf("nonce 长度 = %d", len(nonce))
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 3}, intersectionStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q.Verifier, intersectionCellInput(spec, spec.CellX, spec.CellY)) {
		t.Fatal("目标格未通过")
	}
	for _, d := range [][2]int{{-1, -1}, {-1, 0}, {-1, 1}, {0, -1}, {0, 1}, {1, -1}, {1, 0}, {1, 1}} {
		cx, cy := spec.CellX+d[0], spec.CellY+d[1]
		if !g.Verify(q.Verifier, intersectionCellInput(spec, cx, cy)) {
			t.Fatalf("3×3 邻格 (%d,%d) 未通过", cx, cy)
		}
	}
	for _, d := range [][2]int{{-2, 0}, {2, 0}, {0, -2}, {0, 2}, {-2, -2}, {2, 2}} {
		cx, cy := spec.CellX+d[0], spec.CellY+d[1]
		if cx < 0 || cy < 0 || cx >= spec.Cols || cy >= spec.Rows {
			continue
		}
		if g.Verify(q.Verifier, intersectionCellInput(spec, cx, cy)) {
			t.Fatalf("3×3 之外的格 (%d,%d) 不应通过", cx, cy)
		}
	}
	// 角点（0,999）离目标远，必然不在接受域内。
	for _, bad := range []string{"0,0", "999,999", "0,999", "999,0"} {
		if g.Verify(q.Verifier, bad) {
			t.Fatalf("远离目标的点 %q 不应通过", bad)
		}
	}
}

func TestIntersectionPointNormalize(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"120,340", "120,340", true},
		{" 120 , 340 ", "120,340", true},
		{"0,0", "0,0", true},
		{"999,999", "999,999", true},
		{"", "", false},
		{"120", "", false},
		{"120,340,560", "", false},
		{"1000,0", "", false},
		{"-1,2", "", false},
		{"1.5,2", "", false},
		{"a,b", "", false},
		{"1, 2 3", "", false},
	}
	for _, tc := range cases {
		got, ok := (IntersectionPoint{}).NormalizeAnswer(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("NormalizePoint(%q)=(%q,%v)，期望 (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestIntersectionPointMalformedVerifier(t *testing.T) {
	g, _ := NewIntersectionPoint(bytes.Repeat([]byte{0x93}, 32))
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, intersectionStream(t, "intersection-verifier-shape"))
	if err != nil {
		t.Fatal(err)
	}
	if g.Verify(nil, "100,100") || g.Verify(PrivateVerifier("IPV1"), "100,100") {
		t.Fatal("空/截断 verifier 应拒绝")
	}
	badMagic := append([]byte(nil), q.Verifier...)
	copy(badMagic[0:4], "XXXX")
	if g.Verify(badMagic, "100,100") {
		t.Fatal("错误 magic 应拒绝")
	}
	zeroGrid := append([]byte(nil), q.Verifier...)
	zeroGrid[4], zeroGrid[5], zeroGrid[6], zeroGrid[7] = 0, 0, 0, 0
	if g.Verify(zeroGrid, "100,100") {
		t.Fatal("0×0 网格应拒绝")
	}
	oversized := append([]byte(nil), q.Verifier...)
	oversized[4], oversized[5], oversized[6], oversized[7] = 0x10, 0x00, 0x10, 0x00 // 4096×4096
	if g.Verify(oversized, "100,100") {
		t.Fatal("超过格数上限的网格应拒绝")
	}
	truncated := q.Verifier[:len(q.Verifier)-1]
	if g.Verify(truncated, "100,100") {
		t.Fatal("截断的 TSV 应拒绝")
	}
}

func TestIntersectionPointSanitize(t *testing.T) {
	g, _ := NewIntersectionPoint(bytes.Repeat([]byte{0x94}, 32))
	q, err := g.Generate(context.Background(), Params{Difficulty: 2}, intersectionStream(t, "intersection-shape"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(q.Payload)
	if _, err := g.SanitizePublic(raw); err != nil {
		t.Fatalf("合法题面被拒: %v", err)
	}
	for _, tc := range []struct {
		name string
		key  string
		val  any
	}{
		{"额外 answer", "answer", "100,100"},
		{"额外目标格", "grid_cols", 32},
		{"错误 kind", "kind", "shape-grid"},
		{"错误 answer_type", "answer_type", "cells"},
		{"错误图像宽度", "image_width", 320},
		{"错误图像高度", "image_height", 180},
		{"错误答案长度", "max_answer_len", 16},
		{"空中文提示", "prompt_zh", ""},
	} {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		m[tc.key] = tc.val
		bad, _ := json.Marshal(m)
		if _, err := g.SanitizePublic(bad); err == nil {
			t.Fatalf("%s 应拒绝", tc.name)
		}
	}
	if _, err := g.SanitizePublic([]byte("not json")); err == nil {
		t.Fatal("非 JSON 应拒绝")
	}
}

func TestIntersectionPointRenderHasNoAnswerMarker(t *testing.T) {
	seed := "intersection-render"
	spec, _, err := buildIntersectionPoint(intersectionStream(t, seed), 2)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := renderIntersection(spec)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(asset))
	if err != nil {
		t.Fatal(err)
	}
	gimg, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("解码类型 = %T，期望 *image.Gray", img)
	}
	if gimg.Bounds().Dx() != intersectionW || gimg.Bounds().Dy() != intersectionH {
		t.Fatalf("渲染尺寸 = %v", gimg.Bounds())
	}
	// 交点处必须留空：没有遮挡块、没有十字、没有高亮。否则"点某个可见标记"
	// 就成了不看图也能通过的捷径（M3 的随机猜测模型也就不成立）。
	if got := gimg.GrayAt(spec.TargetX, spec.TargetY).Y; got < 200 {
		t.Fatalf("交点像素 = %d，应为背景（交点不得被画出来）", got)
	}
	for dy := -10; dy <= 10; dy++ {
		for dx := -10; dx <= 10; dx++ {
			if got := gimg.GrayAt(spec.TargetX+dx, spec.TargetY+dy).Y; got < 200 {
				t.Fatalf("交点附近 (%d,%d) 像素 = %d，应留空", dx, dy, got)
			}
		}
	}
	m1 := 0.55 + float64((spec.VisualSeed>>2)&3)*0.12
	m2 := -0.55 - float64((spec.VisualSeed>>4)&3)*0.12
	for _, m := range []float64{m1, m2} {
		p0, p1 := intersectionLineEndpoints(spec, m)
		for _, p := range []image.Point{p0, p1} {
			if p.X < 10 || p.X > intersectionW-11 || p.Y < 10 || p.Y > intersectionH-11 {
				t.Fatalf("端点 %v 不在画布内边距内", p)
			}
			if got := gimg.GrayAt(p.X, p.Y).Y; got != 20 {
				t.Fatalf("端点 %v 的标记像素 = %d，期望 20", p, got)
			}
		}
		// 线必须是虚的：同一条线上既有深色像素，也有背景像素。
		dark, light := 0, 0
		steps := max(absInt(p1.X-p0.X), absInt(p1.Y-p0.Y))
		for i := 0; i <= steps; i++ {
			tf := float64(i) / float64(steps)
			x := int(math.Round(float64(p0.X) + tf*float64(p1.X-p0.X)))
			y := int(math.Round(float64(p0.Y) + tf*float64(p1.Y-p0.Y)))
			dx, dy := x-spec.TargetX, y-spec.TargetY
			if dx*dx+dy*dy <= 20*20 {
				continue
			}
			if gimg.GrayAt(x, y).Y < 60 {
				dark++
			} else if gimg.GrayAt(x, y).Y > 200 {
				light++
			}
		}
		if dark < 40 || light < 40 {
			t.Fatalf("斜率 %.2f 的线不像虚线: dark=%d light=%d", m, dark, light)
		}
	}
}
