package generator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"image/png"
	"strings"
	"testing"
)

const shapeTestKey = 0x33

func newTestShapeGrid(t *testing.T) ShapeGrid {
	t.Helper()
	g, err := NewShapeGrid(bytes.Repeat([]byte{shapeTestKey}, 32))
	if err != nil {
		t.Fatalf("NewShapeGrid: %v", err)
	}
	return g
}

func newTestShapeCount(t *testing.T) ShapeCount {
	t.Helper()
	g, err := NewShapeCount(bytes.Repeat([]byte{shapeTestKey}, 32))
	if err != nil {
		t.Fatalf("NewShapeCount: %v", err)
	}
	return g
}

func genShapeGrid(t *testing.T, g ShapeGrid, difficulty int, seed string) (string, Question) {
	t.Helper()
	answer, err := ShapeGridAnswerFrom(detStream(t, seed))
	if err != nil {
		t.Fatalf("ShapeGridAnswerFrom: %v", err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: difficulty}, detStream(t, seed))
	if err != nil {
		t.Fatalf("shape-grid 生成失败（难度 %d，seed %s）: %v", difficulty, seed, err)
	}
	return answer, q
}

func genShapeCount(t *testing.T, g ShapeCount, difficulty int, seed string) (string, Question) {
	t.Helper()
	answer, err := ShapeCountAnswerFrom(detStream(t, seed))
	if err != nil {
		t.Fatalf("ShapeCountAnswerFrom: %v", err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: difficulty}, detStream(t, seed))
	if err != nil {
		t.Fatalf("shape-count 生成失败（难度 %d，seed %s）: %v", difficulty, seed, err)
	}
	return answer, q
}

func decodePNG(t *testing.T, asset []byte) (int, int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(asset))
	if err != nil {
		t.Fatalf("资产不是合法 PNG: %v", err)
	}
	b := img.Bounds()
	return b.Dx(), b.Dy()
}

func gridAnswerParts(t *testing.T, answer string) []string {
	t.Helper()
	parts := strings.Split(answer, ",")
	if len(parts) < shapeGridMinK || len(parts) > shapeGridMaxK {
		t.Fatalf("shape-grid 答案 %q 的格子数不在 %d..%d", answer, shapeGridMinK, shapeGridMaxK)
	}
	seen := map[string]bool{}
	for _, p := range parts {
		if len(p) != 1 || p[0] < '1' || p[0] > '9' {
			t.Fatalf("shape-grid 答案 %q 含非法序号", answer)
		}
		if seen[p] {
			t.Fatalf("shape-grid 答案 %q 有重复序号", answer)
		}
		seen[p] = true
	}
	for i := 1; i < len(parts); i++ {
		if parts[i-1] >= parts[i] {
			t.Fatalf("shape-grid 答案 %q 未排序", answer)
		}
	}
	return parts
}

// TestShapeGridIdentityAndScoring 覆盖闭环：身份、资产形状、判分与规范化。
func TestShapeGridIdentityAndScoring(t *testing.T) {
	g := newTestShapeGrid(t)
	if g.ID() != ShapeGridID || g.Version() != "v1" || g.Media() != MediaImage {
		t.Fatalf("身份不符: id=%q version=%q media=%q", g.ID(), g.Version(), g.Media())
	}
	for diff := 1; diff <= MaxDifficulties; diff++ {
		for i := 0; i < 6; i++ {
			answer, q := genShapeGrid(t, g, diff, seedN("shapegrid", i*7+diff))
			gridAnswerParts(t, answer)
			if q.MimeType != MimeTypePNG || len(q.Verifier) != AnswerVerifierLen {
				t.Fatalf("媒体/verifier 形状不符: mime=%q verifier=%d", q.MimeType, len(q.Verifier))
			}
			if w, h := decodePNG(t, q.Asset); w != ShapeGridWidth || h != ShapeGridHeight {
				t.Fatalf("图像尺寸 %dx%d，期望 %dx%d", w, h, ShapeGridWidth, ShapeGridHeight)
			}
			if len(q.Asset) > MaxImageAssetBytes {
				t.Fatalf("PNG %d 字节，越界", len(q.Asset))
			}
			if !g.Verify(q.Verifier, answer) {
				t.Fatalf("正确答案未通过判分: %q", answer)
			}
			// 顺序、空格、重复都不该消耗唯一一次作答机会。
			parts := strings.Split(answer, ",")
			if len(parts) == 2 {
				if !g.Verify(q.Verifier, parts[1]+","+parts[0]) {
					t.Fatalf("乱序提交 %q 被拒（应规范化后相等）", parts[1]+","+parts[0])
				}
				if !g.Verify(q.Verifier, parts[0]+", "+parts[1]) {
					t.Fatalf("逗号后空格 %q 被拒", parts[0]+", "+parts[1])
				}
				if !g.Verify(q.Verifier, parts[0]+","+parts[0]+","+parts[1]) {
					t.Fatalf("重复序号未被去重: %q", parts[0]+","+parts[0]+","+parts[1])
				}
			}
			wrong := wrongGridAnswer(answer)
			if g.Verify(q.Verifier, wrong) {
				t.Fatalf("错误集合 %q 通过了判分（正确 %q）", wrong, answer)
			}
			pub, err := g.SanitizePublic(mustJSON(t, q.Payload))
			if err != nil {
				t.Fatalf("题面出网失败: %v", err)
			}
			if pub["kind"] != "shape-grid" || pub["answer_type"] != "cells" {
				t.Fatalf("公开题面形状不符: %+v", pub)
			}
			raw := mustJSON(t, q.Payload)
			if bytes.Contains(raw, []byte(answer)) {
				t.Fatalf("公开题面含答案串 %q", answer)
			}
			if findPrivateLeak(raw, q.Verifier) {
				t.Fatal("公开题面出现私有 verifier 字节")
			}
		}
	}
}

// wrongGridAnswer 构造一个合法但不同的集合：把第一个序号换成另一个不同的序号。
func wrongGridAnswer(answer string) string {
	parts := strings.Split(answer, ",")
	bad := "1"
	if parts[0] == "1" {
		bad = "2"
	}
	return strings.Join(append([]string{bad}, parts[1:]...), ",")
}

func TestShapeGridNormalizeAnswer(t *testing.T) {
	g := newTestShapeGrid(t)
	valid := map[string]string{
		"1,2":     "1,2",
		" 2,1 ":   "1,2",
		"2, 1":    "1,2",
		"01,02":   "1,2",
		"2,2,1":   "1,2",
		"9,8,7":   "7,8,9",
		"1,2,3,4": "1,2,3,4",
	}
	for in, want := range valid {
		got, ok := g.NormalizeAnswer(in)
		if !ok || got != want {
			t.Fatalf("合法输入 %q 归一化为 %q ok=%v，期望 %q", in, got, ok, want)
		}
	}
	bad := []string{
		"", " ", "0", "10", "1,0", "1,,2", "1,", ",1", "1.5", "-1", "+1",
		"1 2", "1;2", "1,2,3,4,5,6,7,8,9,1", "１,２",
	}
	for _, in := range bad {
		if got, ok := g.NormalizeAnswer(in); ok {
			t.Fatalf("畸形输入 %q 被接受为 %q", in, got)
		}
	}
}

func TestShapeGridSanitizePublicWhitelist(t *testing.T) {
	g := newTestShapeGrid(t)
	good := ShapeGridPublic{
		AnswerType: "cells", MaxAnswerLen: shapeGridMaxAnswerLen,
		ImageWidth: ShapeGridWidth, ImageHeight: ShapeGridHeight,
		GridCols: shapeGridCols, GridRows: shapeGridRows,
		GridX: shapeGridMargin, GridY: shapeGridMargin,
		GridCell: shapeGridCell, GridGap: shapeGridGap,
		PromptZH: "选出图中所有的三角形。", PromptEN: "Select every triangle.",
	}
	if _, err := g.SanitizePublic(mustJSON(t, good)); err != nil {
		t.Fatalf("合法题面被拒: %v", err)
	}
	cases := map[string]string{
		"答案类型": `{"answer_type":"text","max_answer_len":32,"image_width":340,"image_height":340,"grid_cols":3,"grid_rows":3,"grid_x":12,"grid_y":12,"grid_cell":100,"grid_gap":8,"prompt_zh":"x","prompt_en":"x"}`,
		"尺寸":   `{"answer_type":"cells","max_answer_len":32,"image_width":400,"image_height":340,"grid_cols":3,"grid_rows":3,"grid_x":12,"grid_y":12,"grid_cell":100,"grid_gap":8,"prompt_zh":"x","prompt_en":"x"}`,
		"网格":   `{"answer_type":"cells","max_answer_len":32,"image_width":340,"image_height":340,"grid_cols":2,"grid_rows":3,"grid_x":12,"grid_y":12,"grid_cell":100,"grid_gap":8,"prompt_zh":"x","prompt_en":"x"}`,
		"额外字段": `{"answer_type":"cells","max_answer_len":32,"image_width":340,"image_height":340,"grid_cols":3,"grid_rows":3,"grid_x":12,"grid_y":12,"grid_cell":100,"grid_gap":8,"prompt_zh":"x","prompt_en":"x","answer":"1,2"}`,
		"空提示":  `{"answer_type":"cells","max_answer_len":32,"image_width":340,"image_height":340,"grid_cols":3,"grid_rows":3,"grid_x":12,"grid_y":12,"grid_cell":100,"grid_gap":8,"prompt_zh":"","prompt_en":"x"}`,
	}
	for name, raw := range cases {
		if _, err := g.SanitizePublic([]byte(raw)); err == nil {
			t.Fatalf("非法题面 %q 未被拒", name)
		}
	}
}

func TestShapeGridParamsBounds(t *testing.T) {
	for d := 1; d <= MaxDifficulties; d++ {
		p, err := ShapeGridParams(d)
		if err != nil {
			t.Fatalf("d%d: %v", d, err)
		}
		if err := p.validate(); err != nil {
			t.Fatalf("d%d 参数不合法: %v", d, err)
		}
	}
	if _, err := ShapeGridParams(0); err == nil {
		t.Fatal("难度 0 应被拒")
	}
	if _, err := ShapeGridParams(MaxDifficulties + 1); err == nil {
		t.Fatal("越界难度应被拒")
	}
	if _, err := NewShapeGrid(bytes.Repeat([]byte{1}, 16)); !errors.Is(err, ErrBadParams) {
		t.Fatalf("短密钥应被拒: %v", err)
	}
}

// TestShapeGridAnswerSpace 量一下实际答案集合：k 必须落在 2..3，且答案不塌缩。
func TestShapeGridAnswerSpace(t *testing.T) {
	g := newTestShapeGrid(t)
	seen := map[string]bool{}
	k2, k3 := 0, 0
	for i := 0; i < 60; i++ {
		answer, err := ShapeGridAnswerFrom(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		seen[answer] = true
		switch len(strings.Split(answer, ",")) {
		case 2:
			k2++
		case 3:
			k3++
		default:
			t.Fatalf("答案 %q 的 k 不在 2..3", answer)
		}
	}
	if k2 == 0 || k3 == 0 {
		t.Fatalf("k=2/3 的分布塌缩: k2=%d k3=%d", k2, k3)
	}
	if len(seen) < 20 {
		t.Fatalf("60 次只出现 %d 个不同答案，答案空间疑似塌缩", len(seen))
	}
	_ = g
}

func TestShapeGridNotBlankAndNoClipping(t *testing.T) {
	g := newTestShapeGrid(t)
	for d := 1; d <= MaxDifficulties; d++ {
		_, q := genShapeGrid(t, g, d, seedN("shapegrid-ink", d))
		img, err := png.Decode(bytes.NewReader(q.Asset))
		if err != nil {
			t.Fatal(err)
		}
		dark := 0
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				r, _, _, _ := img.At(x, y).RGBA()
				if r < 0x8000 {
					dark++
				}
				// 边框只应有纸面/噪点；图形被边距挡在里面。噪点最暗 paper-60。
				if x == b.Min.X || y == b.Min.Y || x == b.Max.X-1 || y == b.Max.Y-1 {
					if r < 150*0x101 {
						t.Fatalf("d%d 边框像素过暗（%d）：图形可能被裁切", d, r>>8)
					}
				}
			}
		}
		if dark < 500 {
			t.Fatalf("d%d 图像几乎没有墨迹（dark=%d）", d, dark)
		}
	}
}

func TestShapeCountIdentityAndScoring(t *testing.T) {
	g := newTestShapeCount(t)
	if g.ID() != ShapeCountID || g.Version() != "v1" || g.Media() != MediaImage {
		t.Fatalf("身份不符: id=%q version=%q media=%q", g.ID(), g.Version(), g.Media())
	}
	for diff := 1; diff <= MaxDifficulties; diff++ {
		for i := 0; i < 6; i++ {
			answer, q := genShapeCount(t, g, diff, seedN("shapecount", i*7+diff))
			parts := strings.Split(answer, ",")
			if len(parts) != 2 {
				t.Fatalf("答案 %q 不是两个数", answer)
			}
			for _, p := range parts {
				if p < "1" || p > "6" {
					t.Fatalf("答案 %q 的数量越界", answer)
				}
			}
			if q.MimeType != MimeTypePNG || len(q.Verifier) != AnswerVerifierLen {
				t.Fatalf("媒体/verifier 形状不符: mime=%q verifier=%d", q.MimeType, len(q.Verifier))
			}
			if w, h := decodePNG(t, q.Asset); w != ShapeCountWidth || h != ShapeCountHeight {
				t.Fatalf("图像尺寸 %dx%d，期望 %dx%d", w, h, ShapeCountWidth, ShapeCountHeight)
			}
			if len(q.Asset) > MaxImageAssetBytes {
				t.Fatalf("PNG %d 字节，越界", len(q.Asset))
			}
			if !g.Verify(q.Verifier, answer) {
				t.Fatalf("正确答案未通过判分: %q", answer)
			}
			if !g.Verify(q.Verifier, parts[0]+", "+parts[1]) {
				t.Fatalf("逗号后空格 %q 被拒", answer)
			}
			if !g.Verify(q.Verifier, "0"+parts[0]+",0"+parts[1]) {
				t.Fatalf("前导零 %q 未被规范化", answer)
			}
			wrong := "1,1"
			if answer == wrong {
				wrong = "2,2"
			}
			if g.Verify(q.Verifier, wrong) {
				t.Fatalf("错误计数 %q 通过了判分（正确 %q）", wrong, answer)
			}
			pub, err := g.SanitizePublic(mustJSON(t, q.Payload))
			if err != nil {
				t.Fatalf("题面出网失败: %v", err)
			}
			if pub["kind"] != "shape-count" || pub["answer_type"] != "pairs" {
				t.Fatalf("公开题面形状不符: %+v", pub)
			}
			labels, ok := pub["labels_zh"].([]string)
			if !ok || len(labels) != 2 {
				t.Fatalf("labels_zh 形状不符: %#v", pub["labels_zh"])
			}
			raw := mustJSON(t, q.Payload)
			if findPrivateLeak(raw, q.Verifier) {
				t.Fatal("公开题面出现私有 verifier 字节")
			}
		}
	}
}

func TestShapeCountNormalizeAnswer(t *testing.T) {
	g := newTestShapeCount(t)
	valid := map[string]string{
		"1,2":   "1,2",
		" 2, 1": "2,1",
		"01,02": "1,2",
		"0,0":   "0,0",
		"9,9":   "9,9",
	}
	for in, want := range valid {
		got, ok := g.NormalizeAnswer(in)
		if !ok || got != want {
			t.Fatalf("合法输入 %q 归一化为 %q ok=%v，期望 %q", in, got, ok, want)
		}
	}
	bad := []string{
		"", "1", "1,", ",1", "1,2,3", "-1,2", "1.5,2", "1 2", "１,２", "100,2", "1,a",
	}
	for _, in := range bad {
		if got, ok := g.NormalizeAnswer(in); ok {
			t.Fatalf("畸形输入 %q 被接受为 %q", in, got)
		}
	}
}

func TestShapeCountSanitizePublicWhitelist(t *testing.T) {
	g := newTestShapeCount(t)
	good := ShapeCountPublic{
		AnswerType: "pairs", MaxAnswerLen: shapeCountMaxAnswerLen,
		ImageWidth: ShapeCountWidth, ImageHeight: ShapeCountHeight,
		LabelsZH: []string{"实心三角形", "空心圆形"}, LabelsEN: []string{"filled triangles", "hollow circles"},
		Min: shapeCountMinN, Max: shapeCountMaxN,
		PromptZH: "数一数。", PromptEN: "Count.",
	}
	if _, err := g.SanitizePublic(mustJSON(t, good)); err != nil {
		t.Fatalf("合法题面被拒: %v", err)
	}
	cases := map[string]string{
		"答案类型": `{"answer_type":"cells","max_answer_len":16,"image_width":480,"image_height":240,"labels_zh":["a","b"],"labels_en":["a","b"],"min":1,"max":6,"prompt_zh":"x","prompt_en":"x"}`,
		"一个标签": `{"answer_type":"pairs","max_answer_len":16,"image_width":480,"image_height":240,"labels_zh":["a"],"labels_en":["a"],"min":1,"max":6,"prompt_zh":"x","prompt_en":"x"}`,
		"范围倒置": `{"answer_type":"pairs","max_answer_len":16,"image_width":480,"image_height":240,"labels_zh":["a","b"],"labels_en":["a","b"],"min":6,"max":1,"prompt_zh":"x","prompt_en":"x"}`,
		"额外字段": `{"answer_type":"pairs","max_answer_len":16,"image_width":480,"image_height":240,"labels_zh":["a","b"],"labels_en":["a","b"],"min":1,"max":6,"prompt_zh":"x","prompt_en":"x","answer":"1,2"}`,
		"尺寸":   `{"answer_type":"pairs","max_answer_len":16,"image_width":500,"image_height":240,"labels_zh":["a","b"],"labels_en":["a","b"],"min":1,"max":6,"prompt_zh":"x","prompt_en":"x"}`,
	}
	for name, raw := range cases {
		if _, err := g.SanitizePublic([]byte(raw)); err == nil {
			t.Fatalf("非法题面 %q 未被拒", name)
		}
	}
}

func TestShapeCountParamsBounds(t *testing.T) {
	for d := 1; d <= MaxDifficulties; d++ {
		p, err := ShapeCountParams(d)
		if err != nil {
			t.Fatalf("d%d: %v", d, err)
		}
		if err := p.validate(); err != nil {
			t.Fatalf("d%d 参数不合法: %v", d, err)
		}
	}
	if _, err := ShapeCountParams(0); err == nil {
		t.Fatal("难度 0 应被拒")
	}
	if _, err := ShapeCountParams(MaxDifficulties + 1); err == nil {
		t.Fatal("越界难度应被拒")
	}
	if _, err := NewShapeCount(bytes.Repeat([]byte{1}, 16)); !errors.Is(err, ErrBadParams) {
		t.Fatalf("短密钥应被拒: %v", err)
	}
}

func TestShapeCountAnswerSpace(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		answer, err := ShapeCountAnswerFrom(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		seen[answer] = true
		parts := strings.Split(answer, ",")
		for _, p := range parts {
			if p < "1" || p > "6" {
				t.Fatalf("答案 %q 越界", answer)
			}
		}
	}
	if len(seen) < 12 {
		t.Fatalf("60 次只出现 %d 个不同答案，36 点答案空间疑似塌缩", len(seen))
	}
}

func TestShapeCountNotBlankAndNoClipping(t *testing.T) {
	g := newTestShapeCount(t)
	for d := 1; d <= MaxDifficulties; d++ {
		_, q := genShapeCount(t, g, d, seedN("shapecount-ink", d))
		img, err := png.Decode(bytes.NewReader(q.Asset))
		if err != nil {
			t.Fatal(err)
		}
		dark := 0
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				r, _, _, _ := img.At(x, y).RGBA()
				if r < 0x8000 {
					dark++
				}
				if x == b.Min.X || y == b.Min.Y || x == b.Max.X-1 || y == b.Max.Y-1 {
					if r < 150*0x101 {
						t.Fatalf("d%d 边框像素过暗（%d）：图形可能被裁切", d, r>>8)
					}
				}
			}
		}
		if dark < 300 {
			t.Fatalf("d%d 图像几乎没有墨迹（dark=%d）", d, dark)
		}
	}
}

// TestShapeAnswersDoNotLeakAcrossGenerators 是类型隔离的静态检查：
// 两种图形题的公开题面都不含对方的字段，也不含 verifier。
func TestShapeAnswersDoNotLeakAcrossGenerators(t *testing.T) {
	grid := newTestShapeGrid(t)
	count := newTestShapeCount(t)
	_, gq := genShapeGrid(t, grid, 2, "leak-grid")
	_, cq := genShapeCount(t, count, 2, "leak-count")
	graw, _ := json.Marshal(gq.Payload)
	craw, _ := json.Marshal(cq.Payload)
	if bytes.Contains(graw, []byte("labels_zh")) || bytes.Contains(craw, []byte("grid_cols")) {
		t.Fatal("两种图形题的公开字段发生串扰")
	}
	if findPrivateLeak(graw, gq.Verifier) || findPrivateLeak(craw, cq.Verifier) {
		t.Fatal("公开题面出现私有 verifier")
	}
}
