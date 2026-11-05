// Command evalgen 产出**离线**评测样本：一批图片 + 一份 ground truth + 一份复现清单。
//
// 这是开发工具，不在服务的依赖图里（stage3b.sh 用 go list 钉住 cmd/server 不链接
// internal/eval）。生产 HTTP API 没有任何答案出口，ground truth 只落本地目录，
// 而那个目录在 .gitignore 里。
//
// 样本集分两类，用途不能混：
//   - dev：调参用。看完结果改渲染参数，再跑一次，这是正常工程。
//   - holdout：**固定验收集**。看过它的结果之后再回头调参数并复用同一集当"独立效果证明"，
//     测的就不是泛化能力而是"我还记得答案"。所以 holdout 的 seed 写死在这里，
//     调参期间不要拿它跑；要调就回 dev。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shijianus/EpoCHA/internal/eval"
	"github.com/shijianus/EpoCHA/internal/generator"
)

const (
	maxSamples = 300
	// holdoutSeed 是固定验收集的种子。**不许改**：改了它就不是同一集，
	// 之前看过的结果也就不能拿来对照了。
	holdoutSeed   = "epocha-3b-holdout-v1-do-not-retune"
	holdoutSeedV2 = "epocha-3b-holdout-v2-do-not-retune"
	// devSeedHint 只是默认值；dev 集鼓励换 seed（换 seed = 换一批新样本）。
	devSeedHint   = "epocha-3b-dev-v1"
	devSeedHintV2 = "epocha-3b-dev-v2"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "evalgen:", err)
		os.Exit(1)
	}
}

type options struct {
	outDir     string
	set        string
	count      int
	difficulty int
	generator  string
	seed       string
	now        func() time.Time
}

func run(args []string) error {
	fs := flag.NewFlagSet("evalgen", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.outDir, "out", "eval-out", "输出目录（评测产物目录，不入库）")
	fs.StringVar(&o.set, "set", "", "样本集：dev（调参集）或 holdout（固定验收集）")
	fs.IntVar(&o.count, "count", 40, "样本数量（1..300）")
	fs.IntVar(&o.difficulty, "difficulty", 3, "难度档 1..5")
	fs.StringVar(&o.generator, "generator", generator.TextImageID, "题型：text-image-v1 或 text-image-v2")
	fs.StringVar(&o.seed, "seed", "", "运行种子；留空则 dev 用默认值、holdout 用固定值")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("命令行参数: %w", err)
	}
	o.now = time.Now
	return generate(&o)
}

func generate(o *options) error {
	switch o.set {
	case "dev", "holdout":
	case "":
		return errors.New("必须指定 --set dev 或 --set holdout（两类样本的用途不同，不能混用）")
	default:
		return fmt.Errorf("未知样本集 %q（只支持 dev / holdout）", o.set)
	}
	if o.count < 1 || o.count > maxSamples {
		return fmt.Errorf("样本数须在 1..%d", maxSamples)
	}
	if o.difficulty < 1 || o.difficulty > generator.MaxDifficulties {
		return fmt.Errorf("难度档须在 1..%d", generator.MaxDifficulties)
	}
	seed := o.seed
	baseHoldout, baseDev := holdoutSeed, devSeedHint
	switch o.generator {
	case generator.TextImageID:
	case generator.TextImageV2ID:
		baseHoldout, baseDev = holdoutSeedV2, devSeedHintV2
	default:
		return fmt.Errorf("未知题型 %q（只支持 %s / %s）", o.generator, generator.TextImageID, generator.TextImageV2ID)
	}
	switch o.set {
	case "holdout":
		// holdout 的 seed 不接受覆盖：否则"同一固定集"这句话就没有意义了
		if seed != "" && seed != baseHoldout {
			return errors.New("holdout 集必须用固定 seed（调参请回 dev 集，否则验收集不再独立）")
		}
		seed = baseHoldout
	case "dev":
		if seed == "" {
			seed = baseDev
		}
	}

	var g generator.Generator
	var answerFrom func(io.Reader) (string, error)
	var params generator.ImageParams
	switch o.generator {
	case generator.TextImageID:
		img, err := generator.NewTextImage(devKey())
		if err != nil {
			return fmt.Errorf("构造生成器: %w", err)
		}
		g, answerFrom = img, generator.TextImageAnswerFrom
		params, err = generator.TextImageParams(o.difficulty)
		if err != nil {
			return err
		}
	case generator.TextImageV2ID:
		img, err := generator.NewTextImageV2(devKey())
		if err != nil {
			return fmt.Errorf("构造生成器: %w", err)
		}
		g, answerFrom = img, generator.TextImageV2AnswerFrom
		params, err = generator.TextImageV2Params(o.difficulty)
		if err != nil {
			return err
		}
	}
	dir := filepath.Join(o.outDir, fmt.Sprintf("%s-d%d-n%d-%s", o.set, o.difficulty, o.count,
		o.now().UTC().Format("20060102T150405Z")))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建输出目录: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".keep"), nil, 0o600); err != nil {
		return err
	}

	gtPath := filepath.Join(dir, "ground-truth.jsonl")
	gtFile, err := os.OpenFile(gtPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建 ground truth: %w", err)
	}
	defer gtFile.Close()
	enc := json.NewEncoder(gtFile)

	type manifest struct {
		GeneratorID  string `json:"generator_id"`
		GeneratorVer string `json:"generator_version"`
		Set          string `json:"set"`
		Difficulty   int    `json:"difficulty"`
		Count        int    `json:"count"`
		Seed         string `json:"seed"`
		// 复现信息：档位参数 + 运行时。字节级复现在同一 Go 版本上成立；
		// 换工具链后 math/rand 与 crypto/rand.Int 的内部算法都不是稳定性承诺，
		// 因此**已生成的图片文件才是样本**，seed 只是"为什么是这些文件"的说明。
		Render     generator.ImageParams `json:"render_params"`
		GoVersion  string                `json:"go_version"`
		GOOSARCH   string                `json:"goos_goarch"`
		CreatedAt  string                `json:"created_at_utc"`
		AnswerFile string                `json:"answer_file"`
		Warning    string                `json:"warning"`
	}
	written := 0
	var totalBytes int
	for i := 1; i <= o.count; i++ {
		// 每个样本一条独立流：seed 里带上序号，于是"第 i 张图"完全由运行 seed 决定
		sampleSeed := fmt.Sprintf("%s/%s/%06d", seed, o.set, i)
		stream, err := eval.NewDeterministic([]byte(sampleSeed))
		if err != nil {
			return err
		}
		answer, err := answerFrom(stream)
		if err != nil {
			return fmt.Errorf("取答案: %w", err)
		}
		stream2, err := eval.NewDeterministic([]byte(sampleSeed))
		if err != nil {
			return err
		}
		q, err := g.Generate(context.Background(), generator.Params{Difficulty: o.difficulty}, stream2)
		if err != nil {
			return fmt.Errorf("生成第 %d 张: %w", i, err)
		}
		// 配对自检：答案与图片必须来自同一条流位置，否则整批 ground truth 无意义
		if !g.Verify(q.Verifier, answer) {
			return fmt.Errorf("第 %d 张的 ground truth 与图片不匹配（熵流位置不一致）", i)
		}
		// 文件名只带序号：不能从文件名读出答案，也不能与答案建立可逆映射
		name := fmt.Sprintf("sample-%04d.png", i)
		if err := os.WriteFile(filepath.Join(dir, name), q.Asset, 0o600); err != nil {
			return err
		}
		totalBytes += len(q.Asset)
		if err := enc.Encode(eval.Sample{
			File: name, Answer: answer,
			Generator: g.ID(), Version: g.Version(), Difficulty: o.difficulty,
			Seed: sampleSeed,
		}); err != nil {
			return err
		}
		written++
	}
	m := manifest{
		GeneratorID: g.ID(), GeneratorVer: g.Version(), Set: o.set,
		Difficulty: o.difficulty, Count: written, Seed: seed, Render: params,
		GoVersion: runtime.Version(), GOOSARCH: runtime.GOOS + "/" + runtime.GOARCH,
		CreatedAt:  o.now().UTC().Format(time.RFC3339),
		AnswerFile: "ground-truth.jsonl",
		Warning: "本目录含答案表，属开发产物：不得提交入库、不得作为公开运行接口的一部分、" +
			"不得上传到任何外部多模态 API（除非另行获得授权）",
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("样本集 %s：%d 张图（难度 %d，档位 %dx%d），图片共 %d 字节（均值 %d）\n",
		o.set, written, o.difficulty, params.Width, params.Height,
		totalBytes, totalBytes/max(written, 1))
	fmt.Printf("目录：%s\n清单：manifest.json\n答案表：ground-truth.jsonl（不要提交、不要外发）\n", dir)
	if o.set == "holdout" {
		fmt.Println("提醒：holdout 是固定验收集。看过它的结果之后请回 dev 集调参，" +
			"不要反复用同一集调参再拿它当独立效果证明。")
	}
	return nil
}

// devKey 是评测样本的 verifier 密钥：一把**固定常量**，只为让 g.Verify 能做配对自检。
// 它与生产主密钥无关，也不参与任何服务判定。
func devKey() []byte { return []byte(strings.Repeat("e", 32)) }
