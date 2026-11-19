package m4baseline

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/m4baseline/solver"
)

// DefaultGenerators 是 Gate B 覆盖的 7 个题型。顺序固定，便于跨轮比较。
var DefaultGenerators = []string{
	generator.TileRotationID,
	generator.SymbolPathID,
	generator.CrowdFindID,
	generator.VisualRankID,
	generator.ProgressiveTargetID,
	generator.ShellTrackID,
	generator.SilhouetteAssemblyID,
}

type Config struct {
	Challenges            int
	Difficulty            int
	Lang                  string
	Generators            []string
	MaxGenerationAttempts int
	Baseline              BaselineInfo
}

func (c Config) normalized() Config {
	if c.Challenges <= 0 {
		c.Challenges = 1000
	}
	if c.Difficulty < 1 || c.Difficulty > generator.MaxDifficulties {
		c.Difficulty = 3
	}
	if c.Lang == "" {
		c.Lang = "zh-CN"
	}
	if c.Generators == nil {
		c.Generators = append([]string(nil), DefaultGenerators...)
	}
	if c.MaxGenerationAttempts <= 0 {
		c.MaxGenerationAttempts = 20
	}
	return c
}

type BaselineInfo struct {
	OID    string `json:"oid"`
	Branch string `json:"branch"`
	Status string `json:"status"`
}

type ChallengeRecord struct {
	Generator          string
	Index              int
	Solved             bool
	ParseFailed        bool
	GaveUp             bool
	SolverCPUMs        float64
	SolverWallMs       float64
	RSSBytes           uint64
	AssetBytes         int
	PayloadBytes       int
	GenerationAttempts int
	Note               string
	Status             string
	PublicSHA256       string
	AssetSHA256        string
	OSCPUStatus        string
	OSUserMs           float64
	OSSystemMs         float64
	RuntimeEstimateMs  float64
	GenerationWallMs   float64
	PreparationWallMs  float64
	GradingWallMs      float64
	HarnessWallMs      float64
	GCCount            uint32
	GCPauseMs          float64
}

type Criteria struct {
	FailSuccessRate     float64 `json:"fail_success_rate"`
	FailP95CPUMs        float64 `json:"fail_p95_cpu_ms"`
	RedesignMinRate     float64 `json:"redesign_min_rate"`
	ChanceSignificance  string  `json:"chance_significance"`
	AnswerSpaceRule     string  `json:"answer_space_rule"`
	TimingRule          string  `json:"timing_rule"`
	NoProductionPromote string  `json:"no_production_promotion"`
}

type GeneratorResult struct {
	Generator            string   `json:"generator"`
	Total                int      `json:"total"`
	Solved               int      `json:"solved"`
	SuccessRate          float64  `json:"success_rate"`
	CI95Low              float64  `json:"ci95_low"`
	CI95High             float64  `json:"ci95_high"`
	MedianCPUMs          float64  `json:"median_cpu_ms"`
	P95CPUMs             float64  `json:"p95_cpu_ms"`
	MedianWallMs         float64  `json:"median_wall_ms"`
	P95WallMs            float64  `json:"p95_wall_ms"`
	P95TimingBasis       string   `json:"p95_timing_basis"`
	MedianRSSBytes       uint64   `json:"median_rss_bytes"`
	P95RSSBytes          uint64   `json:"p95_rss_bytes"`
	MeanCPUPerSuccessMs  float64  `json:"mean_cpu_ms_per_success"`
	MeanWallPerSuccessMs float64  `json:"mean_wall_ms_per_success"`
	ParseFailureRate     float64  `json:"parse_failure_rate"`
	GiveUpRate           float64  `json:"giveup_rate"`
	MedianAssetBytes     int      `json:"median_asset_bytes"`
	P95AssetBytes        int      `json:"p95_asset_bytes"`
	MinAssetBytes        int      `json:"min_asset_bytes"`
	MaxAssetBytes        int      `json:"max_asset_bytes"`
	MedianPayloadBytes   int      `json:"median_payload_bytes"`
	RandomGuessSingle    float64  `json:"random_guess_single"`
	RandomGuessThree     float64  `json:"random_guess_three"`
	RandomGuessNote      string   `json:"random_guess_note"`
	SignificantAbove     bool     `json:"significant_above_chance"`
	Verdict              string   `json:"verdict"`
	KeepExperimental     bool     `json:"keep_experimental"`
	ProductionReady      bool     `json:"production_ready"`
	Recommendation       string   `json:"recommendation"`
	Notes                []string `json:"notes,omitempty"`
}

type OverallResult struct {
	Total             int     `json:"total"`
	Solved            int     `json:"solved"`
	SuccessRate       float64 `json:"success_rate"`
	CI95Low           float64 `json:"ci95_low"`
	CI95High          float64 `json:"ci95_high"`
	FailCount         int     `json:"fail_count"`
	FailRedesignCount int     `json:"fail_redesign_count"`
	PassCount         int     `json:"pass_count"`
	BlockedCount      int     `json:"blocked_count"`
	NotRunCount       int     `json:"not_run_count"`
	NoPromotion       bool    `json:"no_production_promotion"`
}

type Summary struct {
	SchemaVersion int               `json:"schema_version"`
	Gate          string            `json:"gate"`
	Baseline      BaselineInfo      `json:"baseline"`
	StartedAt     string            `json:"started_at"`
	FinishedAt    string            `json:"finished_at"`
	DurationSec   float64           `json:"duration_seconds"`
	Host          HostInfo          `json:"host"`
	Config        map[string]any    `json:"config"`
	Criteria      Criteria          `json:"criteria"`
	Results       []GeneratorResult `json:"results"`
	Overall       OverallResult     `json:"overall"`
}

// Run 生成 cfg.Challenges 个独立 challenge/题型，逐个调用 solver，最后用生产
// Verify 判分。关键边界：solver.Solve 只收到 publicJSON（浏览器 DTO），
// q.Verifier 留在本函数里，只在 solver 返回之后用于判分。
func Run(ctx context.Context, cfg Config, logf func(string, ...any)) (Summary, []ChallengeRecord, error) {
	cfg = cfg.normalized()
	if len(cfg.Generators) == 0 {
		return Summary{}, nil, errors.New("empty generator whitelist")
	}
	seen := make(map[string]bool)
	for _, id := range cfg.Generators {
		if seen[id] {
			return Summary{}, nil, errors.New("duplicate generator")
		}
		seen[id] = true
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	started := time.Now().UTC()
	host := currentHost()
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return Summary{}, nil, fmt.Errorf("生成内存 mac key: %w", err)
	}
	var records []ChallengeRecord
	results := make([]GeneratorResult, 0, len(cfg.Generators))
	var runError error
	for _, id := range cfg.Generators {
		g, err := newGenerator(id, key)
		if err != nil {
			return Summary{}, nil, fmt.Errorf("构造 %s: %w", id, err)
		}
		if g.Media() != generator.MediaImage {
			return Summary{}, nil, fmt.Errorf("%s 不是图片题型，Gate B 只覆盖图片 DTO", id)
		}
		res := GeneratorResult{
			Generator:        id,
			KeepExperimental: true,
			ProductionReady:  false,
		}
		res.RandomGuessSingle, res.RandomGuessNote = guessSingle(id)
		res.RandomGuessThree = randomGuessThree(res.RandomGuessSingle)
		var genRecords []ChallengeRecord
		blocked := false
		for len(genRecords) < cfg.Challenges {
			if err := ctx.Err(); err != nil {
				runError = err
				res.Verdict = "BLOCKED"
				break
			}
			sampleStarted := time.Now()
			generationStarted := time.Now()
			var q generator.Question
			attempts := 0
			for {
				attempts++
				q, err = g.Generate(ctx, generator.Params{Difficulty: cfg.Difficulty, Lang: cfg.Lang}, rand.Reader)
				if err == nil {
					break
				}
				if attempts >= cfg.MaxGenerationAttempts {
					res.Notes = append(res.Notes, fmt.Sprintf("生成失败（%d 次）: %v", attempts, err))
					res.Verdict = "BLOCKED"
					blocked = true
					break
				}
			}
			generationMs := float64(time.Since(generationStarted).Nanoseconds()) / 1e6
			if blocked {
				rec := ChallengeRecord{Generator: id, Index: len(genRecords), Status: "generation_error", GenerationAttempts: attempts, GenerationWallMs: generationMs, Note: "generation_error", OSCPUStatus: "not_run"}
				genRecords = append(genRecords, rec)
				records = append(records, rec)
				break
			}
			prepStarted := time.Now()
			publicJSON, payloadBytes, err := publicDTO(g, q)
			prepMs := float64(time.Since(prepStarted).Nanoseconds()) / 1e6
			if err != nil {
				res.Notes = append(res.Notes, "公开 DTO 构造失败")
				res.Verdict = "BLOCKED"
				blocked = true
				rec := ChallengeRecord{Generator: id, Index: len(genRecords), Status: "dto_error", GenerationAttempts: attempts, GenerationWallMs: generationMs, PreparationWallMs: prepMs, Note: "dto_error", OSCPUStatus: "not_run"}
				genRecords = append(genRecords, rec)
				records = append(records, rec)
				break
			}
			rss0 := rssBytes()
			var gc0, gc1 runtime.MemStats
			runtime.ReadMemStats(&gc0)
			estimate0 := runtimeUserEstimateSeconds()
			user0, system0, cpuErr0 := processCPUSeconds()
			t0 := time.Now()
			out := solver.Solve(id, publicJSON)
			wall := time.Since(t0)
			user1, system1, cpuErr1 := processCPUSeconds()
			estimate1 := runtimeUserEstimateSeconds()
			runtime.ReadMemStats(&gc1)
			rss1 := rssBytes()
			cpuStatus := "ok"
			userMs, systemMs := (user1-user0)*1000, (system1-system0)*1000
			if cpuErr0 != nil || cpuErr1 != nil {
				cpuStatus = "unavailable"
			} else if userMs < 0 || systemMs < 0 || math.IsNaN(userMs+systemMs) {
				cpuStatus = "invalid_delta"
			} else if userMs+systemMs == 0 {
				cpuStatus = "below_resolution"
			}
			if cpuStatus == "unavailable" || cpuStatus == "invalid_delta" {
				userMs, systemMs = 0, 0
			}
			cpuMs := userMs + systemMs
			wallMs := float64(wall.Nanoseconds()) / 1e6
			if wallMs < 0 || math.IsNaN(wallMs) {
				wallMs = 0
			}
			rss := rss1
			if rss < rss0 {
				rss = rss0
			}
			solved := false
			gradeStarted := time.Now()
			status := "wrong"
			note := out.Note
			if out.ParseFailed {
				status = "parse_failed"
			} else if out.GaveUp || out.Answer == "" {
				status = "gave_up"
			}
			if !out.ParseFailed && !out.GaveUp && out.Answer != "" {
				if canon, ok := g.NormalizeAnswer(out.Answer); !ok {
					note = "invalid-answer-format"
					status = "invalid_answer"
				} else {
					solved = g.Verify(q.Verifier, canon)
					if solved {
						status = "solved"
					}
				}
			}
			gradingMs := float64(time.Since(gradeStarted).Nanoseconds()) / 1e6
			publicHash, assetHash := sha256.Sum256(publicJSON), sha256.Sum256(q.Asset)
			rec := ChallengeRecord{
				Generator:          id,
				Index:              len(genRecords),
				Solved:             solved,
				ParseFailed:        out.ParseFailed,
				GaveUp:             out.GaveUp,
				SolverCPUMs:        cpuMs,
				SolverWallMs:       wallMs,
				RSSBytes:           rss,
				AssetBytes:         len(q.Asset),
				PayloadBytes:       payloadBytes,
				GenerationAttempts: attempts,
				Note:               note,
				Status:             status, PublicSHA256: hex.EncodeToString(publicHash[:]), AssetSHA256: hex.EncodeToString(assetHash[:]),
				OSCPUStatus: cpuStatus, OSUserMs: userMs, OSSystemMs: systemMs,
				RuntimeEstimateMs: (estimate1 - estimate0) * 1000,
				GenerationWallMs:  generationMs, PreparationWallMs: prepMs, GradingWallMs: gradingMs,
				HarnessWallMs: math.Max(0, float64(time.Since(sampleStarted).Nanoseconds())/1e6-generationMs-prepMs-wallMs-gradingMs),
				GCCount:       gc1.NumGC - gc0.NumGC, GCPauseMs: float64(gc1.PauseTotalNs-gc0.PauseTotalNs) / 1e6,
			}
			genRecords = append(genRecords, rec)
			records = append(records, rec)
			if len(genRecords)%100 == 0 {
				logf("%s: %d/%d solved=%d last_wall=%.3fms", id, len(genRecords), cfg.Challenges, countSolved(genRecords), wallMs)
			}
		}
		res.Total = len(genRecords)
		res.Solved = countSolved(genRecords)
		res = summarizeGenerator(res, genRecords)
		results = append(results, res)
		logf("%s: verdict=%s rate=%.4f p95=%s(%.3fms)", id, res.Verdict, res.SuccessRate, res.P95TimingBasis, res.P95CPUMs)
	}
	finished := time.Now().UTC()
	sum := Summary{
		SchemaVersion: 2,
		Gate:          "M4 Qualification Gate B",
		Baseline:      cfg.Baseline,
		StartedAt:     started.Format(time.RFC3339),
		FinishedAt:    finished.Format(time.RFC3339),
		DurationSec:   finished.Sub(started).Seconds(),
		Host:          host,
		Config: map[string]any{
			"challenges_per_generator": cfg.Challenges,
			"difficulty":               cfg.Difficulty,
			"lang":                     cfg.Lang,
			"generators":               cfg.Generators,
			"entropy":                  "crypto/rand.Reader (fresh per challenge)",
			"solver_input":             "public /api/challenge question DTO only (whitelist + base64 PNG)",
			"solver_forbidden":         "verifier, DB answers, generator private seed, admin API",
			"measurement_method":       "os-process-cpu-v2; wall-proxy when any sample has insufficient resolution",
			"cpu_unit_seconds":         "Windows FILETIME 100ns / Linux timeval 1us; unit is not effective resolution",
			"cpu_effective_resolution": "not calibrated; per-sample zero/error marked and CPU p95 not used",
			"requested":                cfg.Challenges * len(cfg.Generators), "completed": len(records),
			"interrupted": runError != nil,
		},
		Criteria: Criteria{
			FailSuccessRate:     0.80,
			FailP95CPUMs:        1000,
			RedesignMinRate:     0.20,
			ChanceSignificance:  "Wilson 95% lower bound > random_guess_single and solved >= 10",
			AnswerSpaceRule:     "random guess over the public answer space; conservative (attacker-favourable) bounds",
			TimingRule:          "OS user+system CPU includes concurrent runtime/GC; per-question p95 requires every sample valid and nonzero, otherwise wall-proxy; zero is not zero cost",
			NoProductionPromote: "production_ready, weights, difficulty and defaults are not changed by this tool",
		},
		Results: results,
	}
	sum.Overall = summarizeOverall(records, results)
	return sum, records, runError
}

func publicDTO(g generator.Generator, q generator.Question) ([]byte, int, error) {
	rawPayload, err := json.Marshal(q.Payload)
	if err != nil {
		return nil, 0, err
	}
	fields, err := g.SanitizePublic(rawPayload)
	if err != nil {
		return nil, 0, err
	}
	fields["generator"] = g.ID()
	fields["generator_version"] = g.Version()
	if len(q.Asset) == 0 || q.MimeType != "image/png" {
		return nil, 0, fmt.Errorf("图片题缺少 PNG 资产（asset=%d mime=%q）", len(q.Asset), q.MimeType)
	}
	fields["media"] = generator.MediaImage
	fields["image_mime"] = q.MimeType
	fields["image_base64"] = base64.StdEncoding.EncodeToString(q.Asset)
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, 0, err
	}
	return out, len(rawPayload), nil
}

func newGenerator(id string, key []byte) (generator.Generator, error) {
	switch id {
	case generator.TileRotationID:
		return generator.NewTileRotation(key)
	case generator.SymbolPathID:
		return generator.NewSymbolPath(key)
	case generator.CrowdFindID:
		return generator.NewCrowdFind(key)
	case generator.VisualRankID:
		return generator.NewVisualRank(key)
	case generator.ProgressiveTargetID:
		return generator.NewProgressiveTarget(key)
	case generator.ShellTrackID:
		return generator.NewShellTrack(key)
	case generator.SilhouetteAssemblyID:
		return generator.NewSilhouetteAssembly(key)
	default:
		return nil, fmt.Errorf("未登记的 Gate B 题型 %q", id)
	}
}

// guessSingle 返回单次随机猜测通过率。口径是**对攻击者有利**的上界：
// 取公开答案空间中最大的一个（而不是最有利防守方的那个）。
func guessSingle(id string) (float64, string) {
	switch id {
	case generator.TileRotationID:
		// generator 要求 candidateSpace >= 64；对称 tile 会让实际空间更小，
		// 所以 1/64 是随机猜测的上界。
		return 1.0 / 64.0, "1/64: generator minimum candidate space; actual space is usually larger"
	case generator.SymbolPathID:
		// 6×6 上长度 6 的自避路径共 4008 条（长度 7 为 8752、8 为 19312）；
		// 取最小分母 4008 作为攻击者最有利的猜测空间。
		return 1.0 / 4008.0, "1/4008: minimum self-avoiding path count on 6x6 (L=6..8)"
	case generator.CrowdFindID:
		return 1.0 / 36.0, "1/36"
	case generator.VisualRankID:
		return 1.0 / 120.0, "1/5!"
	case generator.ProgressiveTargetID:
		return 1.0 / 729.0, "1/9^3"
	case generator.ShellTrackID:
		return 1.0 / 64.0, "1/4^3"
	case generator.SilhouetteAssemblyID:
		// 旋转可由可见槽位轮廓确定；对攻击者最有利的口径是只猜 5! 排列。
		return 1.0 / 120.0, "1/5!: piece assignment; rotations are visually determined"
	default:
		return 0, "unknown"
	}
}

func countSolved(recs []ChallengeRecord) int {
	n := 0
	for _, r := range recs {
		if r.Solved {
			n++
		}
	}
	return n
}

func summarizeGenerator(res GeneratorResult, recs []ChallengeRecord) GeneratorResult {
	n := len(recs)
	if n == 0 {
		res.CI95Low, res.CI95High = 0, 1
		if res.Verdict == "" {
			res.Verdict = "BLOCKED"
		}
		res.Recommendation = "NONE"
		return res
	}
	cpu := make([]float64, 0, n)
	wall := make([]float64, 0, n)
	rss := make([]float64, 0, n)
	assets := make([]float64, 0, n)
	payloads := make([]float64, 0, n)
	parse, giveup := 0, 0
	var cpuSum, wallSum float64
	for _, r := range recs {
		cpu = append(cpu, r.SolverCPUMs)
		wall = append(wall, r.SolverWallMs)
		rss = append(rss, float64(r.RSSBytes))
		assets = append(assets, float64(r.AssetBytes))
		payloads = append(payloads, float64(r.PayloadBytes))
		cpuSum += r.SolverCPUMs
		wallSum += r.SolverWallMs
		if r.ParseFailed {
			parse++
		}
		if r.GaveUp {
			giveup++
		}
	}
	res.Total = n
	res.SuccessRate = float64(res.Solved) / float64(n)
	res.CI95Low, res.CI95High = wilson95(res.Solved, n)
	res.MedianCPUMs = median(cpu)
	res.P95CPUMs = percentile(cpu, 95)
	res.MedianWallMs = median(wall)
	res.P95WallMs = percentile(wall, 95)
	res.MedianRSSBytes = uint64(median(rss))
	res.P95RSSBytes = uint64(percentile(rss, 95))
	res.ParseFailureRate = float64(parse) / float64(n)
	res.GiveUpRate = float64(giveup) / float64(n)
	res.MedianAssetBytes = int(median(assets))
	res.P95AssetBytes = int(percentile(assets, 95))
	sort.Float64s(assets)
	res.MinAssetBytes = int(assets[0])
	res.MaxAssetBytes = int(assets[len(assets)-1])
	res.MedianPayloadBytes = int(median(payloads))
	if res.Solved > 0 {
		res.MeanCPUPerSuccessMs = cpuSum / float64(res.Solved)
		res.MeanWallPerSuccessMs = wallSum / float64(res.Solved)
	}
	validCPU := true
	for _, r := range recs {
		if r.OSCPUStatus != "ok" || r.SolverCPUMs <= 0 {
			validCPU = false
		}
	}
	res.P95TimingBasis = "wall-proxy"
	if validCPU {
		res.P95TimingBasis = "os-process-cpu"
	}
	timingP95 := res.P95CPUMs
	if res.P95TimingBasis == "wall-proxy" {
		timingP95 = res.P95WallMs
	}
	res.SignificantAbove = res.Solved >= 10 && res.CI95Low > res.RandomGuessSingle
	switch {
	case res.Verdict == "BLOCKED":
		res.Recommendation = "NONE"
	case res.SuccessRate >= 0.80 && timingP95 <= 1000:
		res.Verdict = "FAIL"
		res.Recommendation = "REDESIGN"
	case res.SuccessRate >= 0.20:
		res.Verdict = "FAIL_REDESIGN"
		res.Recommendation = "REDESIGN"
	case res.SignificantAbove:
		res.Verdict = "FAIL_REDESIGN"
		res.Recommendation = "REDESIGN"
	default:
		res.Verdict = "PASS"
		res.Recommendation = "KEEP_EXPERIMENTAL"
	}
	return res
}

func summarizeOverall(recs []ChallengeRecord, results []GeneratorResult) OverallResult {
	out := OverallResult{Total: len(recs), NoPromotion: true}
	out.Solved = countSolved(recs)
	if out.Total > 0 {
		out.SuccessRate = float64(out.Solved) / float64(out.Total)
	}
	out.CI95Low, out.CI95High = wilson95(out.Solved, out.Total)
	for _, r := range results {
		switch r.Verdict {
		case "FAIL":
			out.FailCount++
		case "FAIL_REDESIGN":
			out.FailRedesignCount++
		case "PASS":
			out.PassCount++
		case "BLOCKED":
			out.BlockedCount++
		case "NOT_RUN":
			out.NotRunCount++
		}
	}
	return out
}

func WriteCSV(w io.Writer, records []ChallengeRecord) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"generator", "index", "solved", "parse_failed", "gave_up",
		"solver_cpu_ms", "solver_wall_ms", "rss_bytes", "asset_bytes",
		"payload_bytes", "generation_attempts", "note", "status", "public_sha256", "asset_sha256",
		"os_cpu_status", "os_user_ms", "os_system_ms", "runtime_estimate_user_ms", "generation_wall_ms",
		"preparation_wall_ms", "grading_wall_ms", "harness_wall_ms", "gc_count", "gc_pause_ms",
	}); err != nil {
		return err
	}
	for _, r := range records {
		row := []string{
			r.Generator,
			strconv.Itoa(r.Index),
			strconv.FormatBool(r.Solved),
			strconv.FormatBool(r.ParseFailed),
			strconv.FormatBool(r.GaveUp),
			strconv.FormatFloat(r.SolverCPUMs, 'g', -1, 64),
			strconv.FormatFloat(r.SolverWallMs, 'g', -1, 64),
			strconv.FormatUint(r.RSSBytes, 10),
			strconv.Itoa(r.AssetBytes),
			strconv.Itoa(r.PayloadBytes),
			strconv.Itoa(r.GenerationAttempts),
			r.Note, r.Status, r.PublicSHA256, r.AssetSHA256, r.OSCPUStatus,
			strconv.FormatFloat(r.OSUserMs, 'g', -1, 64), strconv.FormatFloat(r.OSSystemMs, 'g', -1, 64),
			strconv.FormatFloat(r.RuntimeEstimateMs, 'g', -1, 64), strconv.FormatFloat(r.GenerationWallMs, 'g', -1, 64),
			strconv.FormatFloat(r.PreparationWallMs, 'g', -1, 64), strconv.FormatFloat(r.GradingWallMs, 'g', -1, 64),
			strconv.FormatFloat(r.HarnessWallMs, 'g', -1, 64), strconv.FormatUint(uint64(r.GCCount), 10),
			strconv.FormatFloat(r.GCPauseMs, 'g', -1, 64),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func WriteJSON(w io.Writer, sum Summary) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(sum)
}
