// Command m4baseline 是 M4 Qualification Gate B 的开发期攻击基线入口。
//
// 它是**开发工具**：不在 cmd/server 的依赖图里；生成的 challenge 只存在于内存，
// 不写数据库、不读数据库、不下发 verifier。solver 包只收到浏览器能拿到的公开 DTO。
//
// 退出码沿用仓库的三态约定：
//
//	0 = 全部题型都完成了要求的 challenge 数（判定结果可能是 FAIL，那是测量结论）
//	1 = 工具自身错误（读写文件、生成器构造失败）
//	3 = BLOCKED（某题型无法生成足够的 challenge，或环境不可用）
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/shijianus/EpoCHA/internal/m4baseline"
)

func main() {
	code := run(os.Args[1:])
	os.Exit(code)
}

func run(args []string) int {
	fs := flag.NewFlagSet("m4baseline", flag.ContinueOnError)
	var (
		challenges = fs.Int("challenges", 1000, "每个题型的独立 challenge 数")
		difficulty = fs.Int("difficulty", 3, "难度档 1..5")
		lang       = fs.String("lang", "zh-CN", "语言标记（题型忽略它，但保持与生产 Params 一致）")
		gensFlag   = fs.String("generators", "", "逗号分隔的题型白名单；留空=全部 7 个")
		outDir     = fs.String("out-dir", "", "全新 CSV/JSON 输出目录；默认模块外唯一临时目录")
		csvPath    = fs.String("csv", "", "CSV 路径（默认 <out-dir>/m4-results.csv）")
		jsonPath   = fs.String("json", "", "JSON 路径（默认 <out-dir>/m4-summary.json）")
		oidFlag    = fs.String("baseline-oid", "", "基线 OID；留空则读 git rev-parse HEAD")
		branchFlag = fs.String("baseline-branch", "", "基线分支；留空则读 git branch --show-current")
	)
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *challenges < 1 || *challenges > 100000 {
		fmt.Fprintln(os.Stderr, "m4baseline: --challenges 必须在 1..100000")
		return 1
	}
	gensProvided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "generators" {
			gensProvided = true
		}
	})
	if fs.NArg() != 0 || *difficulty < 1 || *difficulty > 5 || (*lang != "zh-CN" && *lang != "en") || *csvPath != "" || *jsonPath != "" {
		fmt.Fprintln(os.Stderr, "m4baseline: invalid parameters; use one new --out-dir for the complete bundle")
		return 1
	}
	gens := append([]string(nil), m4baseline.DefaultGenerators...)
	if gensProvided {
		gens = nil
		seen := map[string]bool{}
		for _, g := range strings.Split(*gensFlag, ",") {
			g = strings.TrimSpace(g)
			if g == "" || !knownGenerator(g) || seen[g] {
				fmt.Fprintln(os.Stderr, "m4baseline: empty, duplicate or unknown generator")
				return 1
			}
			seen[g] = true
			gens = append(gens, g)
		}
	}
	rootBytes, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "m4baseline: source identity unavailable")
		return 1
	}
	root := strings.TrimSpace(string(rootBytes))
	identity, err := m4baseline.CaptureSource(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "m4baseline: source identity unavailable")
		return 1
	}
	var target string
	if *outDir == "" {
		target, err = os.MkdirTemp("", "epocha-m4-")
	} else {
		target, err = filepath.Abs(*outDir)
		if err == nil {
			err = os.Mkdir(target, 0700)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "m4baseline: output must be a new directory")
		return 1
	}
	base := m4baseline.BaselineInfo{OID: identity.Commit, Branch: identity.Branch, Status: "clean"}
	if identity.Dirty {
		base.Status = "dirty"
	}
	cfg := m4baseline.Config{
		Challenges: *challenges,
		Difficulty: *difficulty,
		Lang:       *lang,
		Generators: gens,
		Baseline:   base,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Fprintf(os.Stderr, "m4baseline: baseline=%s branch=%s status=%s challenges/type=%d\n",
		base.OID, base.Branch, base.Status, *challenges)
	sum, records, err := m4baseline.Run(ctx, cfg, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	})
	code := 0
	if err != nil {
		code = 1
	}
	for _, result := range sum.Results {
		if result.Verdict == "BLOCKED" || result.Verdict == "NOT_RUN" {
			if code == 0 {
				code = 3
			}
		}
	}
	endIdentity, sourceErr := m4baseline.CaptureSource(root)
	if sourceErr != nil || endIdentity.SHA256 != identity.SHA256 {
		fmt.Fprintln(os.Stderr, "m4baseline: source changed during measurement; no complete evidence")
		return 1
	}
	executable, hashErr := os.Executable()
	if hashErr != nil {
		return 1
	}
	binary, hashErr := os.Open(executable)
	if hashErr != nil {
		return 1
	}
	hash := sha256.New()
	_, hashErr = io.Copy(hash, binary)
	closeErr := binary.Close()
	if hashErr != nil || closeErr != nil {
		return 1
	}
	buildInfo, _ := debug.ReadBuildInfo()
	metadata := map[string]any{"source": identity, "source_stable": true, "binary_sha256": hex.EncodeToString(hash.Sum(nil)), "build_info": buildInfo,
		"argv": args, "baseline_label": map[string]string{"oid": *oidFlag, "branch": *branchFlag}, "source_binary_binding": "UNVERIFIED: requires independently captured build receipt",
		"raw_exit_code": code, "measurement_error": err != nil}
	if publishErr := m4baseline.PublishEvidence(target, sum, records, metadata, code); publishErr != nil {
		fmt.Fprintf(os.Stderr, "m4baseline: evidence not complete: %v\n", publishErr)
		return 1
	}
	fmt.Fprintf(os.Stderr, "m4baseline: complete bundle %s; raw_exit_code=%d\n", target, code)
	printSummary(sum)
	return code
}

func knownGenerator(id string) bool {
	for _, g := range m4baseline.DefaultGenerators {
		if g == id {
			return true
		}
	}
	return false
}

func gitOutput(args ...string) string {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitStatus() string {
	out := gitOutput("status", "--porcelain")
	if out == "" {
		return "clean"
	}
	if strings.Contains(out, "\n") {
		return "dirty(multiple)"
	}
	return "dirty"
}

func printSummary(sum m4baseline.Summary) {
	fmt.Println("generator,total,solved,rate,ci95_low,ci95_high,median_cpu_ms,p95_cpu_ms,median_wall_ms,p95_wall_ms,median_rss,p95_rss,parse_fail,giveup,verdict")
	for _, r := range sum.Results {
		fmt.Printf("%s,%d,%d,%.4f,%.4f,%.4f,%.3f,%.3f,%.3f,%.3f,%d,%d,%.4f,%.4f,%s\n",
			r.Generator, r.Total, r.Solved, r.SuccessRate, r.CI95Low, r.CI95High,
			r.MedianCPUMs, r.P95CPUMs, r.MedianWallMs, r.P95WallMs,
			r.MedianRSSBytes, r.P95RSSBytes, r.ParseFailureRate, r.GiveUpRate, r.Verdict)
	}
	fmt.Printf("overall,total=%d,solved=%d,rate=%.4f,ci95=[%.4f,%.4f],fail=%d,fail_redesign=%d,pass=%d,blocked=%d,not_run=%d\n",
		sum.Overall.Total, sum.Overall.Solved, sum.Overall.SuccessRate,
		sum.Overall.CI95Low, sum.Overall.CI95High,
		sum.Overall.FailCount, sum.Overall.FailRedesignCount, sum.Overall.PassCount,
		sum.Overall.BlockedCount, sum.Overall.NotRunCount)
	fmt.Printf("finished_at=%s duration=%.1fs\n", sum.FinishedAt, sum.DurationSec)
}
