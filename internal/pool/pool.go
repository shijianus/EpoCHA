// Package pool 是生成式题池的后台预生成（DESIGN §8.3、§8.4，ADR-011）。
//
// 资源纪律是这个包的第一约束，因为目标是 1 vCPU / 1GB：
//   - 生成（抽随机数、构造表达式）一律在数据库事务**之外**；
//   - 每轮补题有批量上限，每生成 N 题主动 sleep 让出 CPU；
//   - 生成或写库失败按指数退避，绝不打成忙循环；
//   - 行数有硬上限，写满就停，不无限堆积；
//   - ctx 取消即停，Wait 能等到 worker 真正退出（服务关闭不留尾巴）。
//
// 领取与终态化不在这里：那是 store 的事务方法（pool 包只管"造题"，不碰状态转移）。
package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

// Config 是池的水位与节奏参数。水位数字沿用 DESIGN §8.3 的取值，全部是 [目标]（未测量）。
type Config struct {
	// LowWatermark 低于此值触发补题；HighWatermark 补到此值停止。
	LowWatermark  int
	HighWatermark int
	// MaxRows 是池的硬上限（全局，不是每桶）：DESIGN §8.3 的 20000 [目标]。
	MaxRows int
	// Workers 是并发生成数。1 vCPU 下更多 worker 只是把同一份 CPU 抢得更快，
	// 因此默认 1；上限由 MaxWorkers 硬性封住。
	Workers int
	// QuestionTTL 是一道题的可领取时长。它必须**大于站点最大的 challenge TTL**，
	// 否则领题条件（题目要比 challenge 晚过期）永远不成立 → 站点只会拿到 503。
	// 这条不成立时 worker 启动会打警告，而不是静默。
	QuestionTTL time.Duration
	// PollInterval 是水位检查周期。
	PollInterval time.Duration
	// BatchPerTick 限制单轮补题条数，让写事务与 CPU 都被切成小段。
	BatchPerTick int
	// YieldEvery / YieldPause：每生成这么多题主动让出 CPU 这么久。
	YieldEvery int
	YieldPause time.Duration
	// 失败退避：Start 起指数增长，封顶 Max。
	BackoffStart time.Duration
	BackoffMax   time.Duration
}

// MaxWorkers 是并发生成的硬上限。它存在的意义是防止一个手滑的配置把 1 vCPU 机器打成 N 倍负载。
const MaxWorkers = 4

var ErrBadConfig = errors.New("pool: 配置不合法")

// DefaultConfig 给出阶段 3A 的默认值（DESIGN §8.3 的水位公式，标注仍是 [目标]）。
func DefaultConfig() Config {
	return Config{
		LowWatermark:  50,
		HighWatermark: 100,
		MaxRows:       20000,
		Workers:       1,
		QuestionTTL:   4200 * time.Second, // = challenge TTL 上界 3600s + 600s 余量
		PollInterval:  2 * time.Second,
		BatchPerTick:  20,
		YieldEvery:    10,
		YieldPause:    100 * time.Millisecond,
		BackoffStart:  100 * time.Millisecond,
		BackoffMax:    10 * time.Second,
	}
}

// Validate 检查参数边界；负数与零在这里全部拒绝，绝不静默变成"不限流/无限并发"。
func (c Config) Validate() error {
	switch {
	case c.LowWatermark < 1 || c.HighWatermark < c.LowWatermark:
		return fmt.Errorf("%w: 水位须满足 1 ≤ low ≤ high", ErrBadConfig)
	case c.MaxRows < c.HighWatermark:
		return fmt.Errorf("%w: max_rows(%d) 不能小于高水位(%d)", ErrBadConfig, c.MaxRows, c.HighWatermark)
	case c.Workers < 1 || c.Workers > MaxWorkers:
		return fmt.Errorf("%w: 生成并发须在 1..%d", ErrBadConfig, MaxWorkers)
	case c.QuestionTTL < time.Second || c.QuestionTTL > 24*time.Hour:
		return fmt.Errorf("%w: 题目寿命须在 1s..24h", ErrBadConfig)
	case c.PollInterval < 100*time.Millisecond:
		return fmt.Errorf("%w: 轮询间隔不得小于 100ms", ErrBadConfig)
	case c.BatchPerTick < 1:
		return fmt.Errorf("%w: 单轮批量必须为正", ErrBadConfig)
	case c.YieldEvery < 1 || c.YieldPause < 0:
		return fmt.Errorf("%w: CPU 让步参数不合法", ErrBadConfig)
	case c.BackoffStart < time.Millisecond || c.BackoffMax < c.BackoffStart:
		return fmt.Errorf("%w: 退避区间不合法", ErrBadConfig)
	}
	return nil
}

// Stats 是运维观测用的计数。**内存计数，重启清零**——它是可观测性，不是权威状态：
// 池的权威状态永远是库里的行。
type Stats struct {
	Generated int64
	Failed    int64  // 生成阶段的失败（熵源、重试耗尽、题面非法）
	Rejected  int64  // 写库被拒（体积上限、行数上限）
	LastErr   string // 最近一次错误的**类别**，不含题目内容
	LastTick  string // 最近一轮补题时间（RFC3339）
	// RestartResets 恒为 true：这些计数在内存里，重启归零。
	// 池的权威状态永远是库里的行，不是这里的计数（DESIGN §5.4）。
	RestartResets bool
}

// Worker 是后台补题器。
type Worker struct {
	db  *store.DB
	reg *generator.Registry
	cfg Config
	log *slog.Logger
	now func() time.Time
	// entropy 每次调用返回一个 CSPRNG 流。生产是 crypto/rand.Reader；
	// 测试注入确定性流，但**注入点只在构造参数上**——运行路径没有"固定种子"回落分支。
	entropy func() io.Reader

	mu    sync.Mutex
	stats Stats
	// refilling 是**每个分桶**的补题迟滞状态：ready 跌破 low 才置位，补到 high 才清除。
	// 没有它，"低水位触发、高水位停止"在跨轮次时无法成立（一批补不完就停在 low 以下）；
	// 有了它，也不会退化成"永远钉在高水位"的持续生成。
	// 这是内存状态：重启后清空，最坏情况是多补或晚补一轮，不影响任何正确性不变量。
	refilling map[string]bool

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// New 构造 worker。entropy 为 nil 时使用 crypto/rand.Reader。
func New(db *store.DB, reg *generator.Registry, cfg Config, log *slog.Logger, entropy func() io.Reader) (*Worker, error) {
	if db == nil || reg == nil {
		return nil, fmt.Errorf("%w: 缺少库句柄或生成器注册表", ErrBadConfig)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if entropy == nil {
		entropy = func() io.Reader { return rand.Reader }
	}
	return &Worker{db: db, reg: reg, cfg: cfg, log: log, now: time.Now, entropy: entropy,
		refilling: map[string]bool{}}, nil
}

// Start 启动 Workers 个补题 goroutine，直到返回的 ctx 取消。
func (w *Worker) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	for i := 0; i < w.cfg.Workers; i++ {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.loop(ctx)
		}()
	}
}

// StopCancel 请求停止；Wait 等到 goroutine 真正退出。
func (w *Worker) StopCancel() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *Worker) Wait() { w.wg.Wait() }

func (w *Worker) Stats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.stats
	s.RestartResets = true
	return s
}

func (w *Worker) noteGenerated() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.Generated++
	w.stats.LastTick = w.now().UTC().Format(time.RFC3339)
}

// noteFailed / noteRejected 只记录错误**类别**：错误文本可能含题目内容或驱动细节，
// 那些都不能进管理响应，也不能进日志（红线 11 的按值扫描会把它抓出来）。
func (w *Worker) noteFailed(err error) {
	w.countFailure("failed", err)
}

func (w *Worker) noteRejected(err error) {
	w.countFailure("rejected", err)
}

func (w *Worker) countFailure(kind string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if kind == "failed" {
		w.stats.Failed++
	} else {
		w.stats.Rejected++
	}
	if err != nil {
		w.stats.LastErr = errClass(err)
	}
	w.stats.LastTick = w.now().UTC().Format(time.RFC3339)
}

// errClass 把错误折叠成有限类别，避免把库/驱动文本带进管理响应。
func errClass(err error) string {
	switch {
	case errors.Is(err, store.ErrPoolFull):
		return "pool-full"
	case errors.Is(err, generator.ErrOutOfEntropy):
		return "entropy"
	case errors.Is(err, generator.ErrGenerateExhausted):
		return "generate-exhausted"
	case errors.Is(err, generator.ErrBadParams):
		return "bad-params"
	case errors.Is(err, generator.ErrUnknownGenerator):
		return "unknown-generator"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "unspecified"
	}
}

func (w *Worker) loop(ctx context.Context) {
	// 节奏：有进展就立刻再来一轮（每轮仍受 BatchPerTick 限制），
	// 水位够就等下一次 tick，失败就按指数退避。三者都不会变成忙循环。
	backoff := time.Duration(0)
	for {
		if backoff > 0 {
			if err := sleepCtx(ctx, backoff); err != nil {
				return
			}
		}
		generated, failedErr := w.Tick(ctx)
		if ctx.Err() != nil {
			return
		}
		if failedErr != nil {
			backoff = nextBackoff(backoff, w.cfg)
			w.log.Warn("题池补题失败，退避", "backoff_ms", backoff.Milliseconds(), "err", errClass(failedErr))
			continue
		}
		if generated > 0 {
			backoff = 0
			continue
		}
		backoff = 0
		if err := sleepCtx(ctx, w.cfg.PollInterval); err != nil {
			return
		}
	}
}

func nextBackoff(cur time.Duration, cfg Config) time.Duration {
	if cur <= 0 {
		return cfg.BackoffStart
	}
	next := cur * 2
	if next > cfg.BackoffMax || next <= 0 { // <=0 处理溢出
		return cfg.BackoffMax
	}
	return next
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Tick 执行一轮补题，返回本轮生成的题数与第一个致命错误。
// 单独导出是为了测试可以直接驱动它（不必和时钟赛跑），也为了启动预热复用。
func (w *Worker) Tick(ctx context.Context) (int, error) {
	buckets, err := w.db.QuestionBuckets(ctx)
	if err != nil {
		return 0, err
	}
	if len(buckets) == 0 {
		return 0, nil
	}
	total, err := w.db.PoolCount(ctx)
	if err != nil {
		return 0, err
	}
	now := w.now()
	made := 0
	var firstErr error

	for _, spec := range buckets {
		if ctx.Err() != nil {
			return made, ctx.Err()
		}
		g, err := w.reg.Lookup(spec.GeneratorID)
		if err != nil {
			// 站点配了一个本程序没有登记的题型：跳过并记错，**绝不回落到别的题型**——
			// 那会把站点声明的策略换成另一个策略，静默改变业务方拿到的题。
			w.noteFailed(err)
			firstErr = err
			continue
		}
		spec.Version = g.Version()
		if err := spec.ValidateSpec(); err != nil {
			w.noteFailed(err)
			continue
		}
		ready, err := w.db.ReadyCount(ctx, spec, now)
		if err != nil {
			w.noteFailed(err)
			firstErr = err
			continue
		}
		key := spec.GeneratorID + "|" + spec.Version + "|" + strconv.Itoa(spec.Difficulty)
		w.mu.Lock()
		behind := w.refilling[key] || ready < w.cfg.LowWatermark
		if behind && ready >= w.cfg.HighWatermark {
			w.refilling[key] = false
			behind = ready < w.cfg.LowWatermark // 已补到高水位：重新武装触发条件
		}
		if behind {
			w.refilling[key] = true
		}
		w.mu.Unlock()
		if !behind {
			continue
		}
		need := w.cfg.HighWatermark - ready
		if need > w.cfg.BatchPerTick {
			need = w.cfg.BatchPerTick
		}
		// bucketMade 是本桶这一轮的补题数，必须与跨桶累计的 made 分开：
		// 迟滞撤防条件是"这一桶补到高水位"，拿累计数判定会让第一个桶的产量
		// 把第二个桶提前撤防（ready 仍然低于低水位，却要等下一轮才补）。
		bucketMade := 0
		for i := 0; i < need; i++ {
			if total >= w.cfg.MaxRows {
				// 全局上限：这一轮到此为止，不重试（下一轮再看）
				w.noteRejected(store.ErrPoolFull)
				return made, nil
			}
			if err := w.provision(ctx, g, spec, now); err != nil {
				if errors.Is(err, store.ErrPoolFull) || errors.Is(err, store.ErrBadConfig) {
					w.noteRejected(err)
				} else {
					w.noteFailed(err)
				}
				if firstErr == nil {
					firstErr = err
				}
				if errors.Is(err, context.Canceled) {
					return made, err
				}
				// 单条失败就退出这一轮：让上层退避，而不是在内层继续撞同一个坏熵源
				return made, firstErr
			}
			total++
			made++
			bucketMade++
			w.noteGenerated()
			if ready+bucketMade >= w.cfg.HighWatermark {
				// 补满即撤防：下一次要等 ready 再度跌破 low 才重新触发
				w.mu.Lock()
				w.refilling[key] = false
				w.mu.Unlock()
			}
			if made%w.cfg.YieldEvery == 0 {
				if err := sleepCtx(ctx, w.cfg.YieldPause); err != nil {
					return made, err
				}
			}
		}
	}
	return made, firstErr
}

// maxEntropyBytesPerQuestion 是单题允许消耗的熵字节上限。
// 理由：crypto/rand.Int 内部是拒绝采样——如果熵源是个会重复吐出同一字节序列的东西
// （坏掉的注入源、被截断的设备），它会不停地重读、永不返回。给一个硬上限，
// 让这种熵源变成**有界失败**（ErrOutOfEntropy → 退避），而不是 worker 卡死。
const maxEntropyBytesPerQuestion = 4096

// provision 生成一道题并写入池。生成在事务外，写库是单条条件插入（带全局行数上限）。
func (w *Worker) provision(ctx context.Context, g generator.Generator, spec store.PoolSpec, now time.Time) error {
	id, err := newQuestionID()
	if err != nil {
		return err
	}
	q, err := g.Generate(ctx, generator.Params{Difficulty: spec.Difficulty, Lang: "zh-CN"},
		io.LimitReader(w.entropy(), maxEntropyBytesPerQuestion))
	if err != nil {
		return err
	}
	payload, err := json.Marshal(q.Payload)
	if err != nil {
		return err
	}
	// 出网前用白名单过一遍：这里就拒绝非法题面，而不是等到签发请求时才发现
	if _, err := g.SanitizePublic(payload); err != nil {
		return fmt.Errorf("pool: 题面不合法: %w", err)
	}
	if len(payload) > store.MaxPayloadBytes {
		return fmt.Errorf("%w: 题面超过 %d 字节", store.ErrBadConfig, store.MaxPayloadBytes)
	}
	row := &store.PoolQuestion{
		ID:          id,
		GeneratorID: spec.GeneratorID,
		Version:     spec.Version,
		Difficulty:  spec.Difficulty,
		Status:      store.PoolReady,
		PayloadJSON: string(payload),
		Verifier:    []byte(q.Verifier),
		CreatedAt:   now.UTC(),
		ExpiresAt:   now.UTC().Add(w.cfg.QuestionTTL),
	}
	return w.db.InsertQuestion(ctx, row, w.cfg.MaxRows)
}

// newQuestionID 生成 16 字节随机 hex 的池行 id。
// 它不是安全凭据，但必须不可预测：可预测的 id 会让"枚举池行"成为可能。
func newQuestionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("pool: 生成题目 id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
