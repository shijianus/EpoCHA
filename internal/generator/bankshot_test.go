package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"math"
	"strconv"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func bankShotStream(t *testing.T, seed string) io.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBankShotGenerateDeterministic(t *testing.T) {
	g, err := NewBankShot(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		seed := "bankshot-determinism-" + strconv.Itoa(d)
		q1, err := g.Generate(context.Background(), Params{Difficulty: d}, bankShotStream(t, seed))
		if err != nil {
			t.Fatalf("d%d Generate: %v", d, err)
		}
		q2, err := g.Generate(context.Background(), Params{Difficulty: d}, bankShotStream(t, seed))
		if err != nil {
			t.Fatalf("d%d Generate(2): %v", d, err)
		}
		b1, _ := json.Marshal(q1.Payload)
		b2, _ := json.Marshal(q2.Payload)
		if !bytes.Equal(b1, b2) || !bytes.Equal(q1.Verifier, q2.Verifier) || !bytes.Equal(q1.Asset, q2.Asset) {
			t.Fatalf("d%d 同 seed 生成不逐字节一致", d)
		}
		if len(q1.Asset) > MaxImageAssetBytes {
			t.Fatalf("d%d 资产 %d 超过 %d", d, len(q1.Asset), MaxImageAssetBytes)
		}
		t.Logf("bank-shot d%d asset=%d bytes", d, len(q1.Asset))
	}
}

func TestBankShotAnswerFromMatchesGenerate(t *testing.T) {
	g, _ := NewBankShot(bytes.Repeat([]byte{0x32}, 32))
	for d := 1; d <= MaxDifficulties; d++ {
		seed := "bankshot-answer-" + strconv.Itoa(d)
		answer, err := BankShotAnswerFrom(bankShotStream(t, seed), d)
		if err != nil {
			t.Fatalf("d%d AnswerFrom: %v", d, err)
		}
		q, err := g.Generate(context.Background(), Params{Difficulty: d}, bankShotStream(t, seed))
		if err != nil {
			t.Fatalf("d%d Generate: %v", d, err)
		}
		if !g.Verify(q.Verifier, answer) {
			t.Fatalf("d%d 答案 %q 与 verifier 不匹配", d, answer)
		}
		tol, _ := BankShotTolerance(d)
		n, ok := TSVCount(q.Verifier)
		if !ok || n != 2*tol+1 {
			t.Fatalf("d%d TSV 标签数 = %d (ok=%v)，期望 %d", d, n, ok, 2*tol+1)
		}
		pub := q.Payload.(BankShotPublic)
		if pub.AnswerType != AnswerTypeAngle || pub.MinBounces != 1 {
			t.Fatalf("公开题面字段不符: %+v", pub)
		}
		if pub.CueX < 0 || pub.CueX >= pub.ImageWidth || pub.CueY < 0 || pub.CueY >= pub.ImageHeight {
			t.Fatalf("母球位置越界: %+v", pub)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(q.Asset))
		if err != nil {
			t.Fatalf("PNG 解码失败: %v", err)
		}
		if cfg.Width != bankShotWidth || cfg.Height != bankShotHeight {
			t.Fatalf("资产尺寸 = %dx%d", cfg.Width, cfg.Height)
		}
	}
}

func TestBankShotToleranceBoundaries(t *testing.T) {
	g, _ := NewBankShot(bytes.Repeat([]byte{0x33}, 32))
	answer, err := BankShotAnswerFrom(bankShotStream(t, "bankshot-boundary"), 1)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 1}, bankShotStream(t, "bankshot-boundary"))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := strconv.Atoi(answer)
	tol, _ := BankShotTolerance(1)
	for _, delta := range []int{-tol, 0, tol} {
		angle := (a + delta + 3600) % 3600
		if !g.Verify(q.Verifier, strconv.Itoa(angle)) {
			t.Fatalf("容差内 %d (delta=%d) 应通过", angle, delta)
		}
	}
	for _, delta := range []int{-tol - 1, tol + 1, 1800} {
		angle := (a + delta + 3600) % 3600
		if g.Verify(q.Verifier, strconv.Itoa(angle)) {
			t.Fatalf("容差外 %d (delta=%d) 不应通过", angle, delta)
		}
	}
	if g.Verify(q.Verifier, "3600") {
		t.Fatal("越界角度 3600 不应通过")
	}
}

func TestBankShotM3Budget(t *testing.T) {
	for d := 1; d <= MaxDifficulties; d++ {
		tol, _ := BankShotTolerance(d)
		p := float64(2*tol+1) / 3600.0
		p3 := 1 - math.Pow(1-p, 3)
		if p3 >= 0.10 {
			t.Fatalf("d%d 单次 p=%.6f，3 次 P3=%.4f 超过 10%%", d, p, p3)
		}
	}
}

func TestBankShotNormalizeAndWrongAnswer(t *testing.T) {
	g, _ := NewBankShot(bytes.Repeat([]byte{0x34}, 32))
	answer, err := BankShotAnswerFrom(bankShotStream(t, "bankshot-normalize"), 3)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 3}, bankShotStream(t, "bankshot-normalize"))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := strconv.Atoi(answer)
	if !g.Verify(q.Verifier, " "+answer+" ") {
		t.Fatal("前导零/外围空白应规范化后通过")
	}
	wrong := (a + 900) % 3600
	if g.Verify(q.Verifier, strconv.Itoa(wrong)) {
		t.Fatalf("错误角度 %d 不应通过", wrong)
	}
	for _, bad := range []string{"", "3600", "-1", "12.3", "1 2", "abc"} {
		if g.Verify(q.Verifier, bad) {
			t.Fatalf("畸形/越界答案 %q 不应通过", bad)
		}
	}
}

func TestBankShotSanitizePublic(t *testing.T) {
	g, _ := NewBankShot(bytes.Repeat([]byte{0x35}, 32))
	q, err := g.Generate(context.Background(), Params{Difficulty: 2}, bankShotStream(t, "bankshot-shape"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(q.Payload)
	if _, err := g.SanitizePublic(raw); err != nil {
		t.Fatalf("合法题面被拒绝: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["obstacle"] = "secret"
	bad, _ := json.Marshal(m)
	if _, err := g.SanitizePublic(bad); err == nil {
		t.Fatal("额外 obstacle 字段应被拒绝")
	}
	_ = json.Unmarshal(raw, &m)
	m["min_bounces"] = 2
	bad, _ = json.Marshal(m)
	if _, err := g.SanitizePublic(bad); err == nil {
		t.Fatal("min_bounces=2 应被拒绝")
	}
}
