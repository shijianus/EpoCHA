package pool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

// harness 把"真库 + 真注册表 + 可控熵源"拼在一起：
// 池的行为只有在真实 SQLite 上才有意义（水位、行数上限、状态转移都是库里的性质）。
type harness struct {
	t       *testing.T
	db      *store.DB
	reg     *generator.Registry
	w       *Worker
	cfg     Config
	entropy func() io.Reader
}

func newHarness(t *testing.T, tune func(*Config), entropy func() io.Reader) *harness {
	t.Helper()
	pepper := bytes.Repeat([]byte("p"), 32)
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "epocha.db"), pepper)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	g, err := generator.NewArithmeticBaseline(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	reg := generator.NewRegistry()
	if err := reg.Register(g); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.LowWatermark = 4
	cfg.HighWatermark = 8
	cfg.BatchPerTick = 4
	cfg.Prewarm = false
	cfg.PollInterval = 120 * time.Millisecond
	cfg.YieldPause = 0
	cfg.YieldEvery = 100
	if tune != nil {
		tune(&cfg)
	}
	if entropy == nil {
		// 指针：让状态跨调用推进（见 randStream 的注释）
		entropy = func() io.Reader { return &randStream{} }
	}
	w, err := New(db, reg, cfg, nil, entropy)
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	return &harness{t: t, db: db, reg: reg, w: w, cfg: cfg, entropy: entropy}
}

// randStream 是一个**推进的**确定性字节源：每次 Read 都产出与之前不同的字节。
// 必须是推进的：crypto/rand.Int 内部是拒绝采样，一个会重复吐出同一字节的流
// 会让它无限重读（这个坑在本阶段的测试里真实出现过，pool.maxEntropyBytesPerQuestion
// 就是生产侧的对应防护）。
// 只用于不关心具体题面内容的结构性测试；判分正确性在 generator 包里用真熵源测。
type randStream struct{ n uint64 }

func (s *randStream) Read(p []byte) (int, error) {
	for i := range p {
		s.n = s.n*6364136223846793005 + 1442695040888963407
		p[i] = byte(s.n >> 33)
	}
	return len(p), nil
}

func (h *harness) questionSite(name string, difficulty int) *store.Site {
	h.t.Helper()
	mode := store.ModePowQuestion
	gen := generator.ArithID
	diff := difficulty
	site, _, err := h.db.CreateSite(h.t.Context(), name, "sk-"+name, store.SitePolicy{
		PowBits: 12, VerifyMode: mode, GeneratorID: gen, Difficulty: diff,
	})
	if err != nil {
		h.t.Fatalf("CreateSite: %v", err)
	}
	// challenge TTL 用默认 180s，题目寿命默认 4200s：领题条件是"题目要比 challenge 晚过期"，
	// 反过来的配置组合会让站点永远领不到题（TestQuestionShorterThanChallenge 钉住这一点）。
	got, err := h.db.SiteByID(h.t.Context(), site.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

func (h *harness) readyCount(difficulty int) int {
	h.t.Helper()
	n, err := h.db.ReadyCount(h.t.Context(), store.PoolSpec{
		GeneratorID: generator.ArithID, Version: "v1", Difficulty: difficulty,
	}, time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// --- 水位 ---

func TestRefillBelowLowWatermark(t *testing.T) {
	h := newHarness(t, nil, nil)
	site := h.questionSite("alpha", 2)
	if got := h.readyCount(site.Policy.Difficulty); got != 0 {
		t.Fatalf("初始应有 0 题，got %d", got)
	}
	// 一轮补题：受 BatchPerTick=4 限制
	made, err := h.w.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if made != h.cfg.BatchPerTick {
		t.Fatalf("单轮应补 BatchPerTick=%d 题，got %d", h.cfg.BatchPerTick, made)
	}
	// 一批补不完时，迟滞状态必须让它继续补到高水位（否则"低触发、高停止"只是口号）
	for i := 0; i < 10; i++ {
		if _, err := h.w.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.readyCount(site.Policy.Difficulty); got != h.cfg.HighWatermark {
		t.Fatalf("应停在高水位 %d，got %d", h.cfg.HighWatermark, got)
	}
	// 消费掉几题（跌破低水位）后应再次补满
	if _, err := h.db.IssueChallengeWithQuestion(t.Context(), &store.Challenge{
		ID: "c1", SiteID: site.ID, PowSeed: []byte("0123456789abcdef"), PowBits: 12,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Minute).UTC(),
	}, store.Caps{MaxActive: 100, MaxActivePerSite: 100}, &store.PoolSpec{
		GeneratorID: generator.ArithID, Version: "v1", Difficulty: site.Policy.Difficulty,
	}); err != nil {
		t.Fatal(err)
	}
	// 已经够水位：不再生成
	before := h.w.Stats().Generated
	if made, err := h.w.Tick(t.Context()); err != nil || made != 0 {
		t.Fatalf("水位够时不应再生成：made=%d err=%v", made, err)
	}
	if h.w.Stats().Generated != before {
		t.Fatal("水位够时计数却涨了")
	}
}

// TestRefillUsesLongestEnabledChallengeTTL 短寿命 ready 题不能掩盖同桶内
// 长 TTL 站点的真实缺口。水位只统计 expires_at 严格晚于 now+maxTTL 的题。
func TestRefillUsesLongestEnabledChallengeTTL(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.LowWatermark = 2
		c.HighWatermark = 4
		c.BatchPerTick = 4
	}, nil)
	short := h.questionSite("short-ttl", 2)
	_, _, err := h.db.CreateSite(t.Context(), "long-ttl", "sk-long-ttl", store.SitePolicy{
		PowBits: 12, ChallengeTTL: time.Hour, VerifyMode: store.ModePowQuestion,
		GeneratorID: generator.ArithID, Difficulty: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	h.w.now = func() time.Time { return base }
	for i := 0; i < 4; i++ {
		id, err := store.NewID()
		if err != nil {
			t.Fatal(err)
		}
		q := &store.PoolQuestion{
			ID: id, GeneratorID: generator.ArithID, Version: "v1", Difficulty: short.Policy.Difficulty,
			Status: store.PoolReady, PayloadJSON: `{"task":"1 + 1","answer_type":"integer","max_answer_len":14}`,
			Verifier: []byte("test-verifier"), CreatedAt: base, ExpiresAt: base.Add(30 * time.Minute),
		}
		if err := h.db.InsertQuestion(t.Context(), q, store.PoolLimits{MaxRows: 100, MaxImageBytes: 1 << 20}); err != nil {
			t.Fatal(err)
		}
	}
	if made, err := h.w.Tick(t.Context()); err != nil {
		t.Fatal(err)
	} else if made != 4 {
		t.Fatalf("4 道寿命不足以覆盖最长 challenge TTL 的 ready 题应触发补足高水位，made=%d", made)
	}
	if got := h.w.Stats().Generated; got != 4 {
		t.Fatalf("长 TTL 站点对应分桶应补 4 道真正可领题，got %d", got)
	}
}

func TestRefillAgedReadyAfterDefaultTTLAdvance(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.LowWatermark, c.HighWatermark = 2, 4
		c.BatchPerTick = 4
	}, nil)
	site := h.questionSite("clock", 2)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	clock := base
	h.w.now = func() time.Time { return clock }
	if h.cfg.QuestionTTL != 4200*time.Second {
		t.Fatalf("回归须覆盖默认 4200s 题目寿命，got %v", h.cfg.QuestionTTL)
	}
	if made, err := h.w.Tick(t.Context()); err != nil || made != 4 {
		t.Fatalf("初次预热：made=%d err=%v", made, err)
	}
	clock = base.Add(h.cfg.QuestionTTL - site.Policy.ChallengeTTL - time.Nanosecond)
	if made, err := h.w.Tick(t.Context()); err != nil || made != 0 {
		t.Fatalf("过期阈值前 1ns 仍可领，不应补题：made=%d err=%v", made, err)
	}
	clock = clock.Add(time.Nanosecond)
	if made, err := h.w.Tick(t.Context()); err != nil || made != 4 {
		t.Fatalf("老化到题目与 challenge 同时过期时必须补题：made=%d err=%v", made, err)
	}
}

func TestRefillRejectsUnsatisfiableChallengeTTL(t *testing.T) {
	for _, questionTTL := range []time.Duration{time.Hour - time.Second, time.Hour} {
		t.Run(questionTTL.String(), func(t *testing.T) {
			entropyCalls := 0
			h := newHarness(t, func(c *Config) { c.QuestionTTL = questionTTL }, func() io.Reader {
				entropyCalls++
				return &randStream{}
			})
			if _, _, err := h.db.CreateSite(t.Context(), "impossible", "sk-impossible", store.SitePolicy{
				PowBits: 12, ChallengeTTL: time.Hour, VerifyMode: store.ModePowQuestion,
				GeneratorID: generator.ArithID, Difficulty: 2,
			}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				made, err := h.w.Tick(t.Context())
				if !errors.Is(err, store.ErrBadConfig) || made != 0 {
					t.Fatalf("不可满足 TTL 必须报可退避配置错误且不生成：made=%d err=%v", made, err)
				}
			}
			if entropyCalls != 0 || h.w.Stats().LastErr != "bad-config" {
				t.Fatalf("不可满足 TTL 不应消耗熵或渲染：entropy=%d stats=%+v", entropyCalls, h.w.Stats())
			}
			if n, err := h.db.PoolCount(t.Context()); err != nil || n != 0 {
				t.Fatalf("不可领题不应进入池：rows=%d err=%v", n, err)
			}
		})
	}
}

func TestRefillFullPoolStillReclaimsLaterBucket(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.LowWatermark, c.HighWatermark = 1, 2
		c.ImageLowWatermark, c.ImageHighWatermark = 1, 2
		c.BatchPerTick, c.MaxRows = 2, 2
	}, nil)
	h.questionSite("first-bucket", 1)
	site := h.questionSite("last-bucket", 2)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	h.w.now = func() time.Time { return base }
	for _, id := range []string{"aged-first", "aged-second"} {
		if err := h.db.InsertQuestion(t.Context(), &store.PoolQuestion{
			ID: id, GeneratorID: generator.ArithID, Version: "v1", Difficulty: 2,
			PayloadJSON: `{}`, Verifier: []byte("test-verifier"), CreatedAt: base,
			ExpiresAt: base.Add(site.Policy.ChallengeTTL),
		}, store.PoolLimits{MaxRows: 2, MaxImageBytes: h.cfg.ImageMaxTotalBytes}); err != nil {
			t.Fatal(err)
		}
	}
	if made, err := h.w.Tick(t.Context()); err != nil || made != 2 {
		t.Fatalf("earlier empty bucket must not prevent reclaim in later aged bucket: made=%d err=%v", made, err)
	}
}

func TestRefillAgedReadyReleasesCapacity(t *testing.T) {
	for _, image := range []bool{false, true} {
		name := "rows"
		if image {
			name = "image-bytes"
		}
		t.Run(name, func(t *testing.T) {
			h := newImageHarness(t, func(c *Config) {
				c.LowWatermark, c.HighWatermark = 1, 2
				c.ImageLowWatermark, c.ImageHighWatermark = 1, 2
				c.BatchPerTick = 2
				c.MaxRows = 2
				c.ImageMaxTotalBytes = 2 * store.MaxAssetBytes
			})
			var site *store.Site
			if image {
				site = h.imageSite("aged", 2)
			} else {
				site = h.questionSite("aged", 2)
			}
			base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
			h.w.now = func() time.Time { return base }
			spec := store.PoolSpec{GeneratorID: site.Policy.GeneratorID, Version: "v1", Difficulty: 2}
			for i := 0; i < 2; i++ {
				id, err := store.NewID()
				if err != nil {
					t.Fatal(err)
				}
				q := &store.PoolQuestion{
					ID: id, GeneratorID: spec.GeneratorID, Version: spec.Version, Difficulty: spec.Difficulty,
					PayloadJSON: `{}`, Verifier: []byte("test-verifier"), CreatedAt: base.Add(-time.Hour),
					ExpiresAt: base.Add(site.Policy.ChallengeTTL),
				}
				if image {
					q.Asset, q.AssetMime = make([]byte, store.MaxAssetBytes), store.MimeTypePNG
				}
				if err := h.db.InsertQuestion(t.Context(), q, store.PoolLimits{MaxRows: 2, MaxImageBytes: h.cfg.ImageMaxTotalBytes}); err != nil {
					t.Fatal(err)
				}
			}
			if made, err := h.w.Tick(t.Context()); err != nil || made != 2 {
				t.Fatalf("寿命恰好不足的 ready 占满容量后必须回收并补足：made=%d err=%v", made, err)
			}
			if n, err := h.db.ReadyCount(t.Context(), spec, base.Add(site.Policy.ChallengeTTL)); err != nil || n != 2 {
				t.Fatalf("补题后应有两道覆盖完整 challenge TTL 的题：ready=%d err=%v", n, err)
			}
			if n, err := h.db.PoolCount(t.Context()); err != nil || n != 2 {
				t.Fatalf("回收不应突破全局行数上限：rows=%d err=%v", n, err)
			}
		})
	}
}

func TestRefillStopsAtMaxRows(t *testing.T) {
	limit := 6
	h := newHarness(t, func(c *Config) {
		c.LowWatermark = 1
		c.HighWatermark = 5
		c.MaxRows = limit
		c.BatchPerTick = 20
		// 图片水位也必须落在这个极小的行数上限内（Validate 会挡"永远补不到高水位"的配置）
		c.ImageLowWatermark, c.ImageHighWatermark = 1, 2
	}, nil)
	// 两个分桶共用同一个**全局**上限：第二个桶才会撞上
	h.questionSite("alpha", 1)
	h.questionSite("bravo", 3)
	total := 0
	for i := 0; i < 20; i++ {
		made, _ := h.w.Tick(t.Context())
		total += made
		if made == 0 {
			break
		}
	}
	if n, err := h.db.PoolCount(t.Context()); err != nil || n > limit {
		t.Fatalf("行数必须停在硬上限 %d，got %d (err %v)", limit, n, err)
	}
	if st := h.w.Stats(); st.Rejected == 0 {
		t.Fatal("撞到硬上限必须记一次 rejected，否则运维看不到池被打满")
	}
}

func TestWatermarkIsPerBucket(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LowWatermark = 2; c.HighWatermark = 4; c.BatchPerTick = 8 }, nil)
	h.questionSite("d1", 1)
	h.questionSite("d3", 3)
	for i := 0; i < 6; i++ {
		if _, err := h.w.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	// 两个难度档各自补满，不互相挤占
	if n := h.readyCount(1); n != 4 {
		t.Fatalf("difficulty=1 桶应有 4 题，got %d", n)
	}
	if n := h.readyCount(3); n != 4 {
		t.Fatalf("difficulty=3 桶应有 4 题，got %d", n)
	}
}

func TestDisabledSiteNeedsNoQuestions(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LowWatermark = 2; c.HighWatermark = 4; c.BatchPerTick = 8 }, nil)
	site := h.questionSite("alpha", 1)
	key, _, err := h.db.NewAdminKeyWithID(t.Context(), "test-pool-disable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SetSiteEnabled(t.Context(), site.ID, false, key.KeyID); err != nil {
		t.Fatal(err)
	}
	if made, err := h.w.Tick(t.Context()); err != nil || made != 0 {
		t.Fatalf("停用站点不该备题：made=%d err=%v", made, err)
	}
}

// --- 配置边界 ---

func TestConfigValidation(t *testing.T) {
	good := DefaultConfig()
	if err := good.Validate(); err != nil {
		t.Fatalf("默认配置应合法: %v", err)
	}
	bad := []struct {
		name   string
		mutate func(*Config)
	}{
		{"低水位为 0", func(c *Config) { c.LowWatermark = 0 }},
		{"低水位高于高水位", func(c *Config) { c.LowWatermark = 200 }},
		{"上限小于高水位", func(c *Config) { c.MaxRows = 1 }},
		{"并发为 0", func(c *Config) { c.Workers = 0 }},
		{"并发超硬上限", func(c *Config) { c.Workers = MaxWorkers + 1 }},
		{"题目寿命为 0", func(c *Config) { c.QuestionTTL = 0 }},
		{"轮询过快", func(c *Config) { c.PollInterval = time.Millisecond }},
		{"批量为 0", func(c *Config) { c.BatchPerTick = 0 }},
		{"不让出 CPU", func(c *Config) { c.YieldEvery = 0 }},
		{"退避倒挂", func(c *Config) { c.BackoffMax = time.Millisecond }},
	}
	for _, tc := range bad {
		c := good
		tc.mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s：应拒绝", tc.name)
		}
	}
}

func TestNewRejectsMissingDeps(t *testing.T) {
	if _, err := New(nil, nil, DefaultConfig(), nil, nil); err == nil {
		t.Error("缺少依赖应拒绝")
	}
}

// --- 失败退避与恢复 ---

// brokenReader 让生成阶段一直失败：worker 必须退避而不是忙循环。
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestBackoffOnEntropyFailure(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.PollInterval = 100 * time.Millisecond
		c.BackoffStart = 200 * time.Millisecond
		c.BackoffMax = 400 * time.Millisecond
	}, func() io.Reader { return brokenReader{} })
	h.questionSite("alpha", 1)

	ctx, cancel := context.WithCancel(t.Context())
	h.w.Start(ctx)
	time.Sleep(450 * time.Millisecond)
	st := h.w.Stats()
	cancel()
	h.w.Wait()

	if st.Failed == 0 {
		t.Fatal("熵源坏掉必须记失败")
	}
	if st.Generated != 0 {
		t.Fatalf("坏熵源不该产出题目：%d", st.Generated)
	}
	// 忙循环检查：450ms 内、退避 200..400ms，失败次数不该是成千上万
	if st.Failed > 200 {
		t.Fatalf("失败次数 %d 说明退避没生效（忙循环）", st.Failed)
	}
}

func TestRecoversAfterFailure(t *testing.T) {
	var mu sync.Mutex
	failNext := true
	entropy := func() io.Reader {
		mu.Lock()
		defer mu.Unlock()
		if failNext {
			failNext = false
			return brokenReader{}
		}
		return &randStream{}
	}
	h := newHarness(t, func(c *Config) { c.LowWatermark = 2; c.HighWatermark = 4; c.BatchPerTick = 4 }, entropy)
	h.questionSite("alpha", 1)
	if _, err := h.w.Tick(t.Context()); err == nil {
		t.Fatal("首轮应因熵源失败而报错")
	}
	if got := h.w.Stats().Failed; got != 1 {
		t.Fatalf("失败计数 = %d", got)
	}
	// 下一轮恢复正常：必须能继续补题（退避不是"永久停摆"）
	for i := 0; i < 5; i++ {
		if _, err := h.w.Tick(t.Context()); err != nil {
			t.Fatalf("第 %d 轮恢复失败: %v", i, err)
		}
	}
	if n := h.readyCount(1); n == 0 {
		t.Fatal("恢复后仍没有题")
	}
}

// --- 关闭与取消 ---

func TestStopAndWait(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LowWatermark = 2; c.HighWatermark = 4 }, nil)
	h.questionSite("alpha", 1)
	ctx, cancel := context.WithCancel(t.Context())
	h.w.Start(ctx)
	time.Sleep(150 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() { h.w.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消 ctx 后 worker 未退出")
	}
}

func TestStopCancelExplicit(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.questionSite("alpha", 1)
	h.w.Start(t.Context())
	h.w.StopCancel()
	done := make(chan struct{})
	go func() { h.w.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopCancel 后 worker 未退出")
	}
}

func TestTickHonoursContextCancel(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LowWatermark = 2; c.HighWatermark = 40; c.BatchPerTick = 40 }, nil)
	h.questionSite("alpha", 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.w.Tick(ctx); err == nil {
		t.Fatal("已取消的 context 必须让 Tick 停下")
	}
}

// --- 未登记题型 ---

func TestUnknownGeneratorIsNotReplaced(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LowWatermark = 2; c.HighWatermark = 4; c.BatchPerTick = 4 }, nil)
	// 直接把站点的 generator_id 指到一个没登记的题型（管理面会挡，这里模拟数据被外部改动的情况）
	site, _, err := h.db.CreateSite(t.Context(), "ghost", "sk-ghost", store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion, GeneratorID: "text-warp-v1", Difficulty: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = site
	made, err := h.w.Tick(t.Context())
	if err == nil {
		t.Fatal("未登记题型必须报错")
	}
	if made != 0 {
		t.Fatalf("未登记题型时不得产出别的题：%d", made)
	}
	if st := h.w.Stats(); st.LastErr != "unknown-generator" {
		t.Fatalf("错误类别应为 unknown-generator，got %q", st.LastErr)
	}
}

// --- 题目形状与体积 ---

func TestProvisionedRowsAreWellFormed(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LowWatermark = 2; c.HighWatermark = 3; c.BatchPerTick = 3 }, nil)
	h.questionSite("alpha", 4)
	if _, err := h.w.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.PoolStats(t.Context(), time.Now())
	if err != nil || len(rows) != 1 {
		t.Fatalf("分桶统计不符: %+v err %v", rows, err)
	}
	if rows[0].Ready != 3 || rows[0].Assigned != 0 || rows[0].Terminal != 0 {
		t.Fatalf("水位状态不符: %+v", rows[0])
	}
	if rows[0].GeneratorID != generator.ArithID || rows[0].Difficulty != 4 {
		t.Fatalf("分桶键不符: %+v", rows[0])
	}
}

func TestStatsRestartResets(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LowWatermark = 1; c.HighWatermark = 2; c.BatchPerTick = 2 }, nil)
	h.questionSite("alpha", 1)
	if _, err := h.w.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	st := h.w.Stats()
	if !st.RestartResets {
		t.Fatal("计数是内存态，必须对外标明重启清零")
	}
	if st.LastTick == "" || strings.HasSuffix(st.LastTick, "0001-01-01T00:00:00Z") {
		t.Fatalf("LastTick 未记录: %q", st.LastTick)
	}
}

// --- 退避计算 ---

func TestNextBackoffGrowsAndCaps(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BackoffStart = 100 * time.Millisecond
	cfg.BackoffMax = 800 * time.Millisecond
	seq := []time.Duration{}
	cur := time.Duration(0)
	for i := 0; i < 6; i++ {
		cur = nextBackoff(cur, cfg)
		seq = append(seq, cur)
	}
	want := []time.Duration{100, 200, 400, 800, 800, 800}
	for i, w := range want {
		if seq[i] != w*time.Millisecond {
			t.Fatalf("第 %d 次退避 = %v，期望 %v", i+1, seq[i], w*time.Millisecond)
		}
	}
	// 溢出保护：极大值翻倍后不能变成负数
	cur = time.Duration(1) << 62
	if got := nextBackoff(cur, cfg); got != cfg.BackoffMax {
		t.Fatalf("溢出时应取上限，got %v", got)
	}
}

func TestErrClassCoversExpectedClasses(t *testing.T) {
	for err, want := range map[error]string{
		store.ErrPoolFull:              "pool-full",
		generator.ErrOutOfEntropy:      "entropy",
		generator.ErrGenerateExhausted: "generate-exhausted",
		generator.ErrBadParams:         "bad-params",
		generator.ErrUnknownGenerator:  "unknown-generator",
		context.Canceled:               "canceled",
		errors.New("别的"):               "unspecified",
	} {
		if got := errClass(err); got != want {
			t.Errorf("errClass(%v) = %q，期望 %q", err, got, want)
		}
	}
}
