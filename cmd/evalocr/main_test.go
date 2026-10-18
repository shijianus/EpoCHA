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
	"github.com/shijianus/EpoCHA/internal/generator"
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
		ocrArgs: []string{"{image}"}, timeout: 5 * time.Second, normalize: "textimage",
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
	norm, err := normalizer("textimage")
	if err != nil {
		t.Fatal(err)
	}
	m, err := eval.Score(loadGroundMust(t, gt), preds, norm)
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
		timeout: 5 * time.Second, normalize: "textimage", limit: 1, out: out,
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
		ocrArgs: []string{"{image}"}, timeout: time.Second, normalize: "textimage",
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
		timeout: time.Second, normalize: "textimage", out: predsPath,
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
	// 默认档必须**就是**生产实现。这条断言是整个评测口径的地基：
	// 评分器与判分器一旦用两套归一化，测出来的通过率就不再是"服务端会接受多少"。
	n, err := normalizer("textimage")
	if err != nil {
		t.Fatal(err)
	}
	if n == nil {
		t.Fatal("textimage 档不应回落到逐字节比较")
	}
	var g generator.TextImage
	for _, in := range []string{"abcd23", " ABCD23 ", "ABCD23", "AB\tCD23"} {
		want, wantOK := g.NormalizeAnswer(in)
		got, gotOK := n(in)
		if got != want || gotOK != wantOK {
			t.Fatalf("与生产 NormalizeAnswer 不一致 %q：eval=(%q,%v) prod=(%q,%v)", in, got, gotOK, want, wantOK)
		}
	}
	// 大小写折叠是"同一答案的另一种写法"，OCR 常见，服务端也接受。
	if got, ok := n("abcd23"); !ok || got != "ABCD23" {
		t.Fatalf("大小写折叠结果不对：%q %v", got, ok)
	}
	// 关键回归点：**含内部空白**的输出在生产路径是畸形（400），不是"删掉空格就算对"。
	// 旧的 upper 档会把它归一化成正确答案，从而把通过率抬高——那正是被禁止的口径放宽。
	if got, ok := n("AB CD23"); ok {
		t.Fatalf("含内部空白的输出不应被判为合法输入（旧 upper 档就是错在这里）：%q", got)
	}
	// 易混字符替换同样不允许：0↔O、1↔I 的宽容会系统性抬高通过率
	if got, ok := n("ABCD0O"); ok {
		t.Fatalf("0/O 不在字符集内，不应合法：%q", got)
	}
	// 长度必须恰好 6：OCR 少读一个字符就是错，不该被"归一化"救回来
	if _, ok := n("ABCD2"); ok {
		t.Fatal("长度不足 6 不应判为合法")
	}
	if _, err := normalizer("upper"); err == nil {
		t.Fatal("upper 档已废除：它会比服务端更宽容，测出来的通过率不可信")
	}
	if n2, err := normalizer("none"); err != nil || n2 != nil {
		t.Fatal("none 应返回 nil，由评分器逐字节比较")
	}
}

// 畸形输出必须单独可见：把它混进"答错"会让人以为 OCR 读出了内容只是读歪了。
func TestScoreCountsMalformedSeparately(t *testing.T) {
	n, err := normalizer("textimage")
	if err != nil {
		t.Fatal(err)
	}
	ground := []eval.Sample{{File: "a.png", Answer: "ABCD23"}, {File: "b.png", Answer: "ABCD23"}}
	preds := []eval.Prediction{
		{File: "a.png", Text: "A B C D 2 3"}, // OCR 插了空格：服务端会 400，而不是接受
		{File: "b.png", Text: "ABCD24"},      // 格式合法但答错
	}
	m, err := eval.Score(ground, preds, n)
	if err != nil {
		t.Fatal(err)
	}
	if m.Exact != 0 || m.Malformed != 1 || m.Mismatched != 1 {
		t.Fatalf("畸形与答错应分开计数：%+v", m)
	}
	if m.StrictAccuracy != 0 {
		t.Fatalf("通过率应为 0：%v", m.StrictAccuracy)
	}
	// 分母仍然是全部样本：畸形输出不能从口径里被删掉。
	if m.ScoredAccuracy != 0 || m.Predicted != 2 {
		t.Fatalf("畸形不应被剔出分母：%+v", m)
	}
}

// ground truth 自己畸形 = 样本集坏了，必须炸而不是静默少测。
func TestScoreRejectsMalformedGroundTruth(t *testing.T) {
	n, err := normalizer("textimage")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eval.Score([]eval.Sample{{File: "a.png", Answer: "AB CD"}}, nil, n); err == nil {
		t.Fatal("ground truth 畸形应当报错")
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
