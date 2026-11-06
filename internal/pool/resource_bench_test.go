package pool

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

// measureFill 在独立临时库里把池从空补到 n 题，返回耗时、行数、库大小与各均值。
// 库大小给两个数：写入进行中（含 -wal/-shm）与关闭后（WAL 已合并回主文件）。
// 只用 store 的公开 API：不为了测量给生产代码开后门。
func measureFill(b *testing.B, n int) (time.Duration, int64, int64, int64, int64) {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "epocha.db")
	db, err := store.Open(context.Background(), path, bytes.Repeat([]byte("p"), 32))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }() // 下面会显式关闭以量 checkpoint 后的大小，这里只兜住提前 Fatal 的分支
	g, err := generator.NewArithmeticBaseline(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		b.Fatal(err)
	}
	reg := generator.NewRegistry()
	if err := reg.Register(g); err != nil {
		b.Fatal(err)
	}
	if _, _, err := db.CreateSite(context.Background(), "bench", "sk-bench", store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion, GeneratorID: generator.ArithID, Difficulty: 3,
	}); err != nil {
		b.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Prewarm = false
	cfg.LowWatermark = n
	cfg.HighWatermark = n
	cfg.BatchPerTick = n // 一次性补满，测"有限批次"的整批成本
	cfg.YieldPause = 100 * time.Millisecond
	w, err := New(db, reg, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		b.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := w.Tick(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	rows, err := db.PoolCount(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	// WAL 模式下数据先落在 -wal 里：只 stat 主文件会把"刚写完"测成 4KB，
	// 只 stat 三个文件又会把未合并的日志算成净占用。两个都报。
	sizeHot := dirBytes(b, dir)
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	sizeQuiet := dirBytes(b, dir)
	return elapsed, int64(rows), sizeHot, sizeQuiet, payloadAndVerifierBytes(b, g)
}

// dirBytes 汇总临时目录里所有文件大小（含 -wal/-shm）。
func dirBytes(b *testing.B, dir string) int64 {
	b.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		b.Fatal(err)
	}
	var total int64
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		st, err := e.Info()
		if err != nil {
			b.Fatal(err)
		}
		total += st.Size()
	}
	return total
}

// payloadAndVerifierBytes 直接量单题的两块数据大小（与入库的字节同源）。
func payloadAndVerifierBytes(b *testing.B, g generator.Generator) int64 {
	b.Helper()
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: 3, Lang: "zh-CN"}, cryptoRandReader)
	if err != nil {
		b.Fatal(err)
	}
	raw, err := json.Marshal(q.Payload)
	if err != nil {
		b.Fatal(err)
	}
	if len(raw) > len(q.Verifier) {
		return int64(len(raw))
	}
	return int64(len(q.Verifier))
}

// BenchmarkPoolFill 是资源记录的实测入口（需显式 -bench 跑，不进默认测试路径）。
// RSS 读 /proc/self/status 的 VmRSS：Go 的堆指标不等于进程 RSS，不能用它代替。
func BenchmarkPoolFill(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(itoaBench(n), func(b *testing.B) {
			var total time.Duration
			var rows, hot, quiet, maxItem int64
			for i := 0; i < b.N; i++ {
				d, r, h, q, m := measureFill(b, n)
				total, rows, hot, quiet, maxItem = d, r, h, q, m
			}
			b.ReportMetric(float64(total.Microseconds())/float64(maxBench(1, b.N))/1000, "ms/批")
			b.ReportMetric(float64(maxItem), "B/题(较大块)")
			b.ReportMetric(float64(hot), "B/库(写入中)")
			b.ReportMetric(float64(quiet), "B/库(checkpoint后)")
			// 让出 CPU 的 sleep 是**故意**花掉的（1 vCPU 机器的主要约束），必须从耗时里分开报，
			// 否则"10ms/题"会被误读成生成成本，而真实生成+写入只有它的几十分之一。
			yielded := time.Duration(n/maxBench(1, cfgYieldEvery)) * cfgYieldPause
			pure := total - yielded
			if pure < 0 {
				pure = 0
			}
			b.Logf("补 %d 题：rows=%d 总耗时=%v（其中主动让出 CPU=%v）纯生成+写入=%v，单题纯成本=%.2fms",
				n, rows, total.Round(time.Millisecond), yielded.Round(time.Millisecond),
				pure.Round(time.Millisecond), float64(pure.Microseconds())/float64(maxBench(1, int(rows)))/1000)
			b.Logf("单题较大块=%d 字节（上限 %d/%d），库=写入中 %d 字节 / checkpoint 后 %d 字节（每行净 %.0f 字节），进程 VmRSS=%s（含测试框架，不是服务进程）",
				maxItem, store.MaxPayloadBytes, store.MaxVerifierBytes, hot, quiet,
				float64(quiet)/float64(maxBench(1, int(rows))), vmRSSBench())
		})
	}
}

var cryptoRandReader = crand.Reader

// cfgYieldEvery / cfgYieldPause 与 DefaultConfig 一致，单独抽出来是为了能把"故意睡的时间"算准。
const (
	cfgYieldEvery = 10
	cfgYieldPause = 100 * time.Millisecond
)

func vmRSSBench() string {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "不可读：" + err.Error()
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "VmRSS:"))
		}
	}
	return "内核未提供 VmRSS"
}

func maxBench(a, c int) int {
	if a > c {
		return a
	}
	return c
}

func itoaBench(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
