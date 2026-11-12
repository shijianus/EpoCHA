package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func pipeStream(t *testing.T, seed string) *bytes.Reader {
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

func TestPipeConnectDeterministicAndAnswer(t *testing.T) {
	g, err := NewPipeConnect(bytes.Repeat([]byte{0x81}, 32))
	if err != nil {
		t.Fatal(err)
	}
	seed := "pipe-answer"
	answer, err := PipeConnectAnswerFrom(pipeStream(t, seed), 2)
	if err != nil {
		t.Fatal(err)
	}
	q1, err := g.Generate(context.Background(), Params{Difficulty: 2}, pipeStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	q2, err := g.Generate(context.Background(), Params{Difficulty: 2}, pipeStream(t, seed))
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(q1.Payload)
	b2, _ := json.Marshal(q2.Payload)
	if !bytes.Equal(b1, b2) || !bytes.Equal(q1.Verifier, q2.Verifier) || !bytes.Equal(q1.Asset, q2.Asset) {
		t.Fatal("同 seed 生成不逐字节一致")
	}
	if !g.Verify(q1.Verifier, answer) {
		t.Fatalf("答案 %q 未通过", answer)
	}
	parts := strings.Split(answer, ",")
	if len(parts) != pipeKeyCount {
		t.Fatalf("关键节点数 = %d，期望 %d", len(parts), pipeKeyCount)
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > pipeCells {
			t.Fatalf("关键节点越界: %q", p)
		}
	}
	pub := q1.Payload.(PipeConnectPublic)
	if pub.GridCols != 4 || pub.GridRows != 4 || pub.GridCell != pipeGridCell {
		t.Fatalf("网格字段不符: %+v", pub)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(q1.Asset))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != pipeSize || cfg.Height != pipeSize {
		t.Fatalf("资产尺寸 = %dx%d", cfg.Width, cfg.Height)
	}
	if len(q1.Asset) > MaxImageAssetBytes {
		t.Fatalf("资产 %d 超限", len(q1.Asset))
	}
	t.Logf("pipe asset=%d", len(q1.Asset))
}

func TestPipeConnectDifficultyIsMonotonic(t *testing.T) {
	last := 0
	g, _ := NewPipeConnect(bytes.Repeat([]byte{0x84}, 32))
	for d := 1; d <= MaxDifficulties; d++ {
		lo, hi, err := PipeConnectDecoyRange(d)
		if err != nil {
			t.Fatal(err)
		}
		if lo < last || hi < lo {
			t.Fatalf("d%d 死路范围 %d..%d 非单调（上一档 %d）", d, lo, hi, last)
		}
		last = hi
		spec, _, err := buildPipeConnect(pipeStream(t, "pipe-tier-"+strconv.Itoa(d)), d)
		if err != nil {
			t.Fatal(err)
		}
		if len(spec.Decoys) < lo || len(spec.Decoys) > hi {
			t.Fatalf("d%d 死路数 %d 不在 %d..%d", d, len(spec.Decoys), lo, hi)
		}
		answer, err := PipeConnectAnswerFrom(pipeStream(t, "pipe-tier-answer-"+strconv.Itoa(d)), d)
		if err != nil {
			t.Fatal(err)
		}
		q, err := g.Generate(context.Background(), Params{Difficulty: d}, pipeStream(t, "pipe-tier-answer-"+strconv.Itoa(d)))
		if err != nil {
			t.Fatal(err)
		}
		if !g.Verify(q.Verifier, answer) {
			t.Fatalf("d%d 答案未通过", d)
		}
		// 关键断点与死路都被擦空，盲猜者能把范围缩到 E=死路数+2 个候选格。
		// M3 必须按 C(E,2) 算，且取该档**最少**死路数（E 最小、通过率最高）。
		minDecoys, _, _ := PipeConnectDecoyRange(d)
		e := minDecoys + pipeKeyCount
		p := 1.0 / (float64(e) * float64(e-1) / 2)
		if p3 := 1 - math.Pow(1-p, 3); p >= 0.10 || p3 >= 0.10 {
			t.Fatalf("d%d M3 超线: E=%d p=%.4f p3=%.4f", d, e, p, p3)
		}
	}
}

func TestPipeConnectNormalizeAndWrongSet(t *testing.T) {
	g, _ := NewPipeConnect(bytes.Repeat([]byte{0x82}, 32))
	answer, err := PipeConnectAnswerFrom(pipeStream(t, "pipe-wrong"), 2)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 2}, pipeStream(t, "pipe-wrong"))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q.Verifier, " "+strings.ReplaceAll(answer, ",", " , ")+" ") {
		t.Fatal("空格/顺序规范化后应通过")
	}
	wrong := "1,2"
	if answer == wrong {
		wrong = "15,16"
	}
	if g.Verify(q.Verifier, wrong) {
		t.Fatalf("错误集合 %q 不应通过（正确 %q）", wrong, answer)
	}
	for _, bad := range []string{"", "0", "17", "1,", "1,2,3", "a,b"} {
		if g.Verify(q.Verifier, bad) {
			t.Fatalf("畸形答案 %q 不应通过", bad)
		}
	}
	// 同难度下候选格数固定为 E=死路数+2；这里用最保守的 d1 下限做检查。
	lo, _, _ := PipeConnectDecoyRange(1)
	p := 1.0 / (float64(lo+pipeKeyCount) * float64(lo+pipeKeyCount-1) / 2)
	if p3 := 1 - math.Pow(1-p, 3); p >= 0.10 || p3 >= 0.10 {
		t.Fatalf("M3 超线 p=%.4f p3=%.4f", p, p3)
	}
}

func TestPipeConnectSanitize(t *testing.T) {
	g, _ := NewPipeConnect(bytes.Repeat([]byte{0x83}, 32))
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, pipeStream(t, "pipe-shape"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(q.Payload)
	if _, err := g.SanitizePublic(raw); err != nil {
		t.Fatalf("合法题面被拒: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["answer"] = "1,2"
	bad, _ := json.Marshal(m)
	if _, err := g.SanitizePublic(bad); err == nil {
		t.Fatal("额外 answer 应拒绝")
	}
	_ = json.Unmarshal(raw, &m)
	m["grid_cell"] = 99
	bad, _ = json.Marshal(m)
	if _, err := g.SanitizePublic(bad); err == nil {
		t.Fatal("错误网格几何应拒绝")
	}
}

func TestNormalizeCellSet(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"3,7", "3,7", true},
		{"7,3,3", "3,7", true},
		{" 3 , 7 ", "3,7", true},
		{"", "", false},
		{"0", "", false},
		{"17", "", false},
		{"1,", "", false},
		{"1,2,3", "1,2,3", true},
	}
	for _, tc := range cases {
		got, ok := NormalizeCellSet(tc.in, 16, 8)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("NormalizeCellSet(%q)=(%q,%v)，期望 (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPipeConnectBrokenCellsLookAlike(t *testing.T) {
	spec, _, err := buildPipeConnect(pipeStream(t, "pipe-look-alike"), 5)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := renderPipeConnect(spec)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(asset))
	if err != nil {
		t.Fatal(err)
	}
	gimg, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("解码类型 = %T", img)
	}
	// 断点与死路的边框/内部必须完全一致；任何颜色差都会把答案直接标出来。
	sample := func(cell int) (border, inside uint8) {
		n := cell - 1
		p := pipeCenter(n/pipeGridCols, n%pipeGridCols)
		x, y := p.X-pipeGridCell/2+2, p.Y-pipeGridCell/2+2
		return gimg.GrayAt(x, y).Y, gimg.GrayAt(p.X, p.Y).Y
	}
	for _, key := range spec.KeyCells {
		for _, decoy := range spec.Decoys {
			kb, ki := sample(key)
			db, di := sample(decoy)
			if kb != db || ki != di {
				t.Fatalf("断点 %d (%d,%d) 与死路 %d (%d,%d) 外观不同", key, kb, ki, decoy, db, di)
			}
			if kb != 128 || ki != 236 {
				t.Fatalf("断点/死路应统一为浅边框+空内部: border=%d inside=%d", kb, ki)
			}
		}
	}
}

// TestPipeConnectRemainsLegacyCellSelection 钉住现状边界：pipe-connect-v1 的 verifier
// 只表达"两个断点格"的 cells 集合，不能表达每格 edge mask / 路径覆盖 / 端点配对。
// 这不是完整管道拼图；整改必须新开 ID/version（见审计文档 §9），不得原地改变已签发语义。
func TestPipeConnectRemainsLegacyCellSelection(t *testing.T) {
	key := bytes.Repeat([]byte{0x6a}, 32)
	g, err := NewPipeConnect(key)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 2}, pipeStream(t, "pipe-legacy-shape"))
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := q.Payload.(PipeConnectPublic)
	if !ok {
		t.Fatalf("pipe-connect-v1 payload 类型 = %T", q.Payload)
	}
	if pub.AnswerType != AnswerTypeCells {
		t.Fatalf("pipe-connect-v1 answer_type = %q，期望 cells", pub.AnswerType)
	}
	if len(q.Verifier) != AnswerVerifierLen {
		t.Fatalf("pipe-connect-v1 verifier 长度 = %d，期望精确 HMAC %d", len(q.Verifier), AnswerVerifierLen)
	}
	answer, err := PipeConnectAnswerFrom(pipeStream(t, "pipe-legacy-shape"), 2)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(answer, ",")
	if len(parts) != 2 {
		t.Fatalf("pipe-connect-v1 答案 = %q，应恰好两个断点格", answer)
	}
	raw, _ := json.Marshal(q.Payload)
	for _, forbidden := range []string{`"masks":`, `"edges":`, `"solution_path":`, `"endpoint_pairs":`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("pipe-connect-v1 不应声称完整管道状态，payload 含 %s: %s", forbidden, raw)
		}
	}
}
