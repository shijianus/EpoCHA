package main

// evalocr 的测试：**不需要真的 OCR 工具**。
//
// 这里测的是适配器与计数逻辑本身（评分器在 internal/eval 里另有测试）：
// 一个假 OCR 脚本可以精确制造"答对 / 答错 / 非零退出 / 超时 / 空输出 / 文件缺失"六种情况，
// 于是"工具失败与答错分开计数、两个分母都报"这条纪律是被证明的，不是被声称的。
// 缺工具时**必须**返回专用的"未测"退出码而不是 0% 通过率——这条也有用例。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/eval"
)

// fakeOCR 写一个可执行脚本：按文件名尾号决定输出，用于制造各种结果。
func fakeOCR(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakeocr.sh")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeGround(t *testing.T, dir string, samples ...eval.Sample) string {
	t.Helper()
	var b strings.Builder
	for _, s := range samples {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	p := filepath.Join(dir, "ground-truth.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMeasureCountsCorrectWrongAndToolFailures(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.png", "b.png", "c.png", "d.png"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("\x89PNG fake"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gt := writeGround(t, dir,
		eval.Sample{File: "a.png", Answer: "ABCD23"},
		eval.Sample{File: "b.png", Answer: "ABCD23"},
		eval.Sample{File: "c.png", Answer: "ABCD23"},
		eval.Sample{File: "d.png", Answer: "ABCD23"},
	)
	// 假 OCR：a 答对、b 答错、c 非零退出、d 输出空
	script := fakeOCR(t, `case "$1" in
  *a.png) echo "abcd23" ;;
  *b.png) echo "ZZZZZZ" ;;
  *c.png) echo "boom" >&2; exit 7 ;;
  *d.png) printf "" ;;
esac
exit 0`)

	o := &options{
		groundTruth: gt, imageDir: dir, ocrCmd: script,
		ocrArgs: []string{"{image}"}, timeout: 5 * time.Second, normalize: "upper",
	}
	code, err := measure(o)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if code != 0 {
		t.Fatalf("退出码 %d，期望 0", code)
	}
	// 独立复算一遍：证明报告里的数字不是自说自话
	preds := []eval.Prediction{
		{File: "a.png", Text: "abcd23"},
		{File: "b.png", Text: "ZZZZZZ"},
		{File: "c.png", Err: "exec-failed"},
		{File: "d.png", Err: "empty-output"},
	}
	m, err := eval.Score(loadGroundMust(t, gt), preds, normalizer("upper"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Exact != 1 || m.Mismatched != 1 || m.ToolFailures != 2 {
		t.Fatalf("复算结果不符：%+v", m)
	}
	if m.StrictAccuracy != 0.25 {
		t.Fatalf("严格通过率应 1/4：%v", m.StrictAccuracy)
	}
	if m.ScoredAccuracy != 0.5 {
		t.Fatalf("有效样本通过率应 1/2（两次工具失败不进分母）：%v", m.ScoredAccuracy)
	}
}

func TestMeasureWritesPredictionsAndRespectsLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	gt := writeGround(t, dir,
		eval.Sample{File: "a.png", Answer: "ABCD23"},
		eval.Sample{File: "b.png", Answer: "ABCD23"})
	script := fakeOCR(t, `echo "ABCD23"`)
	out := filepath.Join(t.TempDir(), "predictions.jsonl")
	code, err := measure(&options{
		groundTruth: gt, imageDir: dir, ocrCmd: script, ocrArgs: []string{"{image}"},
		timeout: 5 * time.Second, normalize: "upper", limit: 1, out: out,
	})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"file"`) != 1 {
		t.Fatalf("--limit 没生效：%s", raw)
	}
	if !strings.Contains(string(raw), `"ms"`) {
		t.Fatalf("预测应含耗时字段：%s", raw)
	}
}

// TestMeasureBlockedWhenToolMissing 是本轮最重要的一条：
// 缺工具必须报"未测"（专用退出码），而不是给出一个看起来像结论的 0%。
func TestMeasureBlockedWhenToolMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	gt := writeGround(t, dir, eval.Sample{File: "a.png", Answer: "ABCD23"})
	code, err := measure(&options{
		groundTruth: gt, imageDir: dir, ocrCmd: "/nonexistent/ocr-tool-here",
		ocrArgs: []string{"{image}"}, timeout: time.Second, normalize: "upper",
	})
	if err != nil {
		t.Fatalf("阻塞不是错误：%v", err)
	}
	if code != exitBlocked {
		t.Fatalf("退出码 %d，期望 %d（未测与测出 0%% 必须可区分）", code, exitBlocked)
	}
}

// TestGroundTruthCannotEscapeImageDir ground truth 是外部数据：
// 里面的文件名不能把适配器变成"读任意路径"的能力。
func TestGroundTruthCannotEscapeImageDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "images")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outside.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	gt := writeGround(t, sub, eval.Sample{File: "../outside.png", Answer: "ABCD23"})
	script := fakeOCR(t, `echo "ABCD23"`)
	predsPath := filepath.Join(t.TempDir(), "p.json")
	if code, err := measure(&options{
		groundTruth: gt, imageDir: sub, ocrCmd: script, ocrArgs: []string{"{image}"},
		timeout: time.Second, normalize: "upper", out: predsPath,
	}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	raw, err := os.ReadFile(predsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "path-outside-image-dir") {
		t.Fatalf("越界文件名应被拒：%s", raw)
	}
}

func TestNormalizerPolicy(t *testing.T) {
	n := normalizer("upper")
	if got := n(" ab cd23\n"); got != "ABCD23" {
		t.Fatalf("upper 归一化结果 %q", got)
	}
	// 刻意**不做** 0↔O / 1↔I 替换：那种宽容会系统性抬高通过率
	if got := n("ABCD0O"); got != "ABCD0O" {
		t.Fatalf("不应做易混字符替换：%q", got)
	}
	if normalizer("none") != nil {
		t.Fatal("none 应返回 nil，由评分器逐字节比较")
	}
}

func loadGroundMust(t *testing.T, path string) []eval.Sample {
	t.Helper()
	out, err := loadGround(path)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
