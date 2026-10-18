// Command evalocr 是**外部 OCR 适配器**：把一批样本图片交给一个本地 OCR 可执行文件，
// 再用与题型无关的评分器算出通过率。
//
// 三条纪律：
//  1. 不联网、不上传样本。要接外部多模态 API 必须另行获得授权（DESIGN §8.5、AGENTS 停止条款）。
//  2. 工具不存在 / 不可执行 ⇒ **明确报告"未测"并以退出码 3 退出**，
//     而不是打印一个 0% 通过率：0% 会被读成"题目很难"，而真实含义是"根本没测"。
//  3. 工具失败与答错分开计数（internal/eval 的 Score 负责这条），两个分母都报出来。
//
// 调用方式不含 shell：命令与参数由 flag 给出，`{image}` 占位符替换成图片路径，
// 走 exec.Command 的直接 argv 形式（避免把文件名变成可执行片段）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shijianus/EpoCHA/internal/eval"
	"github.com/shijianus/EpoCHA/internal/generator"
)

// exitBlocked 是"评测没能执行"的专用退出码：与 1（跑出错）区分开，
// 这样脚本与人都能看出"未测"不是"测了但很差"。
const exitBlocked = 3

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "evalocr:", err)
	}
	os.Exit(code)
}

type options struct {
	groundTruth string
	out         string
	ocrCmd      string
	ocrArgs     []string
	timeout     time.Duration
	limit       int
	normalize   string
	imageDir    string
}

func run(args []string) (int, error) {
	fs := flag.NewFlagSet("evalocr", flag.ContinueOnError)
	var o options
	var timeoutSec int
	fs.StringVar(&o.groundTruth, "groundtruth", "", "ground-truth.jsonl 路径（必填）")
	fs.StringVar(&o.imageDir, "images", "", "图片目录（默认取 ground truth 所在目录）")
	fs.StringVar(&o.out, "out", "", "预测输出路径 predictions.jsonl（留空则只打印指标）")
	fs.StringVar(&o.ocrCmd, "ocr-cmd", "tesseract", "本地 OCR 可执行文件名或路径")
	fs.Var(&stringList{dst: &o.ocrArgs}, "ocr-arg", "OCR 参数，可重复；{image} 会替换成图片路径")
	fs.IntVar(&timeoutSec, "timeout-sec", 20, "单张超时（秒，1..120）")
	fs.IntVar(&o.limit, "limit", 0, "只跑前 N 张（0 = 全部）")
	fs.StringVar(&o.normalize, "normalize", "textimage", "归一化实现：textimage（= 生产 generator.TextImage.NormalizeAnswer，默认）| none（逐字节比较）")
	if err := fs.Parse(args); err != nil {
		return 1, fmt.Errorf("命令行参数: %w", err)
	}
	o.timeout = time.Duration(timeoutSec) * time.Second
	if o.groundTruth == "" {
		return 1, errors.New("必须给 --groundtruth")
	}
	if timeoutSec < 1 || timeoutSec > 120 {
		return 1, errors.New("单张超时须在 1..120 秒")
	}
	if len(o.ocrArgs) == 0 {
		// tesseract 的最小可用形态：输入图、输出到 stdout、英文、单页。
		// 其它 OCR 工具请自己用 --ocr-arg 描述参数。
		o.ocrArgs = []string{"{image}", "-", "--oem", "3", "-l", "eng", "stdout"}
	}
	if o.imageDir == "" {
		o.imageDir = filepath.Dir(o.groundTruth)
	}
	return measure(&o)
}

type stringList struct{ dst *[]string }

func (s *stringList) String() string { return strings.Join(*s.dst, " ") }
func (s *stringList) Set(v string) error {
	*s.dst = append(*s.dst, v)
	return nil
}

func measure(o *options) (int, error) {
	ground, err := loadGround(o.groundTruth)
	if err != nil {
		return 1, err
	}
	if o.limit > 0 && o.limit < len(ground) {
		ground = ground[:o.limit]
	}
	// 先确认工具存在：这一步失败就是"未测"，不能伪装成一次 0% 的测量
	bin, err := exec.LookPath(o.ocrCmd)
	if err != nil {
		fmt.Printf("OCR 实测：未执行（阻塞）\n")
		fmt.Printf("  原因：找不到可执行文件 %q（PATH=%s）\n", o.ocrCmd, os.Getenv("PATH"))
		fmt.Printf("  已交付：适配器 + 输入输出格式 + 评分器（internal/eval，有测试）\n")
		fmt.Printf("  要跑起来：装一个离线 OCR 后执行\n"+
			"    go run ./cmd/evalocr --groundtruth %s --ocr-cmd <工具> --ocr-arg '{image}' ...\n", o.groundTruth)
		fmt.Printf("  样本量 %d 条已就绪；**不得**用缺测得出任何抗机器结论（ADR-002）\n", len(ground))
		return exitBlocked, nil
	}
	fmt.Printf("OCR 适配器：%s（%s/%s）\n", bin, runtime.GOOS, runtime.GOARCH)

	preds := make([]eval.Prediction, 0, len(ground))
	for i, s := range ground {
		path := filepath.Join(o.imageDir, s.File)
		// 目录穿越防线：ground truth 里的文件名必须落在图片目录内
		absDir, err := filepath.Abs(o.imageDir)
		if err != nil {
			return 1, err
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return 1, err
		}
		if !strings.HasPrefix(absPath, absDir+string(os.PathSeparator)) {
			preds = append(preds, eval.Prediction{File: s.File, Err: "path-outside-image-dir"})
			continue
		}
		if _, err := os.Stat(absPath); err != nil {
			preds = append(preds, eval.Prediction{File: s.File, Err: "image-missing"})
			continue
		}
		text, ms, ferr := runOCR(bin, o, absPath)
		p := eval.Prediction{File: s.File, Text: text, MS: ms}
		if ferr != "" {
			p.Err = ferr
		}
		preds = append(preds, p)
		if (i+1)%25 == 0 {
			fmt.Printf("  已处理 %d/%d\n", i+1, len(ground))
		}
	}
	if o.out != "" {
		raw, err := json.MarshalIndent(preds, "", "  ")
		if err != nil {
			return 1, err
		}
		if err := os.WriteFile(o.out, append(raw, '\n'), 0o600); err != nil {
			return 1, fmt.Errorf("写预测文件: %w", err)
		}
		fmt.Printf("预测已写入 %s\n", o.out)
	}
	norm, err := normalizer(o.normalize)
	if err != nil {
		return 1, err
	}
	m, err := eval.Score(ground, preds, norm)
	if err != nil {
		return 1, err
	}
	report(m, o, bin)
	return 0, nil
}

func report(m eval.Metrics, o *options, bin string) {
	fmt.Println("--- OCR 基线结果 ---")
	fmt.Printf("工具            : %s（参数 %v）\n", bin, o.ocrArgs)
	fmt.Printf("样本量          : %d（有输出 %d，缺测 %d，未知文件 %d）\n",
		m.Total, m.Predicted, m.Missing, m.Unknown)
	fmt.Printf("工具失败        : %d\n", m.ToolFailures)
	fmt.Printf("整串正确        : %d（答错 %d，畸形输出 %d）\n", m.Exact, m.Mismatched, m.Malformed)
	fmt.Printf("严格通过率      : %.2f%%（= 正确 / 全部样本；缺测与失败都算不通过）\n", m.StrictAccuracy*100)
	fmt.Printf("有效样本通过率  : %.2f%%（= 正确 / 成功跑完的样本；剔除工具失败与未知文件）\n", m.ScoredAccuracy*100)
	fmt.Printf("字符级正确率    : %.2f%%（只统计与答案等长的输出）\n", m.CharAccuracy*100)
	if m.MSCount > 0 {
		fmt.Printf("单张耗时        : min %dms / median %dms / mean %dms / max %dms（n=%d）\n",
			m.MSMin, m.MSMedian, m.MSMean, m.MSMax, m.MSCount)
	}
	fmt.Println("注意：以上是**本地 OCR 基线**，不是「抗 AI」结论；多模态模型与打码服务未测（DESIGN §8.5）。")
}

// runOCR 执行一次外部命令。返回输出文本、耗时（毫秒）与失败标记（空串 = 成功）。
func runOCR(bin string, o *options, image string) (string, int64, string) {
	args := make([]string, 0, len(o.ocrArgs))
	for _, a := range o.ocrArgs {
		args = append(args, strings.ReplaceAll(a, "{image}", image))
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	ms := time.Since(start).Milliseconds()
	if ctx.Err() != nil {
		return "", ms, "timeout"
	}
	if err != nil {
		// 不带回 stderr 原文：OCR 工具的报错可能含本机路径，那些不该进评测产物
		return "", ms, "exec-failed"
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return "", ms, "empty-output"
	}
	return out, ms, ""
}

// normalizer 决定"什么算一个可比较的答案"。**默认走生产实现**
// （generator.TextImage.NormalizeAnswer：裁首尾空白、含内部空白即畸形、长度必须恰好 6、
// 字符必须在字符集内、大小写折叠），而不是这里再写一份"看起来差不多"的归一化。
//
// 为什么这是本轮修掉的一个真实缺陷：旧的 `upper` 档会把 OCR 常见的字符间空格**删掉**，
// 于是 "A B C D E F" 被算成正确；而服务端对同一个串会直接 400 invalid-request。
// 用宽容版归一化测出来的通过率是**虚高**的，且虚高多少完全取决于 OCR 的输出习惯——
// 那正是"为了让指标好看而放宽口径"。`none` 保留给逐字节对照实验。
func normalizer(kind string) (eval.Normalizer, error) {
	switch kind {
	case "", "textimage":
		var g generator.TextImage
		return g.NormalizeAnswer, nil
	case "none":
		return nil, nil // 逐字节比较（eval.Score 的 nil 分支）
	default:
		return nil, fmt.Errorf("--normalize 只支持 textimage | none，实得 %q", kind)
	}
}

func loadGround(path string) ([]eval.Sample, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 ground truth: %w", err)
	}
	var out []eval.Sample
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s eval.Sample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, fmt.Errorf("ground truth 第 %d 行不可解析: %w", len(out)+1, err)
		}
		if s.File == "" || s.Answer == "" {
			return nil, fmt.Errorf("ground truth 第 %d 行缺 file/answer", len(out)+1)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("ground truth 为空")
	}
	return out, nil
}
