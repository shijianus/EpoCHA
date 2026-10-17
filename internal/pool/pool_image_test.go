package pool

// 阶段 3B 的池侧测试：图片桶的水位、字节预算、让步节奏与"媒体声明与资产必须成对"。
//
// 这一批用例的存在理由是同一句话：**图片题的资源形状与文本题差两个量级**，
// 沿用算术题的水位与行数量级就等于让 1 vCPU 机器持续渲染。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

// newImageHarness 在注册表里同时登记算术与视觉题型，并建一个用视觉题型的站点。
func newImageHarness(t *testing.T, tune func(*Config)) *harness {
	t.Helper()
	pepper := bytes.Repeat([]byte("p"), 32)
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "epocha.db"), pepper)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	key := bytes.Repeat([]byte{7}, 32)
	arith, err := generator.NewArithmeticBaseline(key)
	if err != nil {
		t.Fatal(err)
	}
	img, err := generator.NewTextImage(key)
	if err != nil {
		t.Fatal(err)
	}
	reg := generator.NewRegistry()
	if err := reg.Register(arith); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(img); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.LowWatermark = 4
	cfg.HighWatermark = 8
	cfg.BatchPerTick = 8
	cfg.PollInterval = 120 * time.Millisecond
	cfg.YieldPause = 0
	cfg.YieldEvery = 100
	cfg.ImageLowWatermark = 2
	cfg.ImageHighWatermark = 4
	if tune != nil {
		tune(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("配置不合法: %v", err)
	}
	w, err := New(db, reg, cfg, nil, func() io.Reader { return &randStream{} })
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, db: db, reg: reg, w: w, cfg: cfg}
}

func (h *harness) imageSite(name string, difficulty int) *store.Site {
	h.t.Helper()
	site, _, err := h.db.CreateSite(h.t.Context(), name, "sk-"+name, store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion,
		GeneratorID: generator.TextImageID, Difficulty: difficulty,
	})
	if err != nil {
		h.t.Fatalf("CreateSite: %v", err)
	}
	got, err := h.db.SiteByID(h.t.Context(), site.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

func (h *harness) readyOf(genID string, difficulty int) int {
	h.t.Helper()
	g, err := h.reg.Lookup(genID)
	if err != nil {
		h.t.Fatal(err)
	}
	spec := store.PoolSpec{GeneratorID: genID, Version: g.Version(), Difficulty: difficulty}
	n, err := h.db.ReadyCount(h.t.Context(), spec, time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// TestImageBucketUsesOwnWatermarks 图片桶补到自己的高水位就停，**不**继承算术桶的 50/100。
func TestImageBucketUsesOwnWatermarks(t *testing.T) {
	h := newImageHarness(t, func(c *Config) {
		c.LowWatermark = 40
		c.HighWatermark = 60
		c.ImageLowWatermark = 2
		c.ImageHighWatermark = 4
	})
	site := h.imageSite("visual", 2)
	for i := 0; i < 6; i++ {
		if _, err := h.w.Tick(context.Background()); err != nil {
			t.Fatalf("补题失败: %v", err)
		}
	}
	got := h.readyOf(generator.TextImageID, site.Policy.Difficulty)
	if got != 4 {
		t.Fatalf("图片桶 ready = %d，期望停在图片高水位 4（算术水位 60 不得继承）", got)
	}
	total, err := h.db.PoolCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("池里总行数 %d ≠ 4：图片桶越过了自己的水位", total)
	}
}

// TestImageByteBudgetStopsGenerationWithoutWastingRender 预算到顶时：
// 预检查必须让 worker **跳过生成**（不是"生成完再被写库拒绝"），并如实报出 image-budget。
func TestImageByteBudgetStopsGenerationWithoutWastingRender(t *testing.T) {
	// 预算刚好容得下一条最坏情况的图：第一条能写进去，之后每一轮都应被预检查挡下
	h := newImageHarness(t, func(c *Config) {
		c.ImageMaxTotalBytes = store.MaxAssetBytes
		c.ImageHighWatermark = 4
		c.ImageLowWatermark = 3
	})
	h.imageSite("visual", 1)

	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("第一轮应能补题: %v", err)
	}
	used, err := h.db.ImageAssetBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if used == 0 || used > store.MaxAssetBytes {
		t.Fatalf("图片字节 %d 不在预算内", used)
	}
	genAfterFirst := h.w.Stats().Generated

	for i := 0; i < 4; i++ {
		if _, err := h.w.Tick(context.Background()); err != nil {
			t.Fatalf("预算到顶不是错误，Tick 不应报错: %v", err)
		}
	}
	st := h.w.Stats()
	if st.Generated != genAfterFirst {
		t.Fatalf("预算已用尽却仍在生成（%d → %d）：白烧 CPU 在 1 vCPU 上就是可被利用的放大面",
			genAfterFirst, st.Generated)
	}
	if st.LastErr != "image-budget" {
		t.Fatalf("LastErr = %q，期望 image-budget", st.LastErr)
	}
	if st.Rejected == 0 {
		t.Fatal("预算拒绝必须计入 Rejected（它是资源状态，不是生成故障）")
	}
	// 预算随清理释放：过期行被 Purge 删掉后能继续补题
	// （走真实清理路径而不是手写 DELETE，顺带钉住"图片行也在清理范围内"）
	if _, err := h.db.Purge(context.Background(), time.Now().Add(3*time.Hour)); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.w.Stats().Generated == genAfterFirst {
		t.Fatal("预算释放后应恢复补题")
	}
}

// TestImageQuestionWithoutAssetIsRejected 声明 image 却没产出资产 ⇒ 写库前就拒绝。
// 这条挡的是"下发一道看不见的题"：那种行一旦进池，站点领到的是空题面。
func TestImageQuestionWithoutAssetIsRejected(t *testing.T) {
	h := newImageHarness(t, nil)
	lying := assetlessImageGenerator{inner: mustTextImage(t)}
	if err := h.reg.Register(lying); err != nil {
		t.Fatal(err)
	}
	// 站点指向这个"说谎"的题型：它声明 image，但 Generate 不给图
	if _, _, err := h.db.CreateSite(context.Background(), "liar", "sk-liar", store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion, GeneratorID: lying.ID(), Difficulty: 1,
	}); err != nil {
		t.Fatal(err)
	}
	before := h.w.Stats().Generated
	if _, err := h.w.Tick(context.Background()); err == nil {
		t.Fatal("资产缺失应让本轮失败（进而退避），不能静默写进池")
	}
	if h.w.Stats().Generated != before {
		t.Fatal("生成失败却计了成功数")
	}
	n, err := h.db.PoolCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("池里出现了 %d 条没有图片的图片题", n)
	}
	if got := h.w.Stats().LastErr; got != "bad-config" {
		t.Fatalf("LastErr = %q，期望 bad-config（媒体与资产不配对属于实现/配置错误）", got)
	}
}

// TestImageYieldPaceIsSlowerThanText 视觉补题的让步节奏独立且更长。
// 断言用**下界**（耗时 ≥ 让步时长），不做上界断言：调度抖动会让上界变成偶发失败。
func TestImageYieldPaceIsSlowerThanText(t *testing.T) {
	h := newImageHarness(t, func(c *Config) {
		c.ImageLowWatermark = 1
		c.ImageHighWatermark = 4
		c.YieldEvery = 2
		c.YieldPause = 0
		c.ImageYieldPause = 60 * time.Millisecond
	})
	h.imageSite("visual", 1)
	start := time.Now()
	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	// 4 条题、每 2 条让出 60ms ⇒ 至少一次让步
	if elapsed < 60*time.Millisecond {
		t.Fatalf("图片补题耗时 %v：让步没有生效（YieldEvery=2、ImageYieldPause=60ms）", elapsed)
	}
}

// TestImageConstantsMatchStore 生成器与存储两侧的字节上限/MIME 必须是同一个值。
// 两侧各自写一遍数字而无人检查，是"改一处忘一处"的标准形态。
func TestImageConstantsMatchStore(t *testing.T) {
	if generator.MimeTypePNG != store.MimeTypePNG {
		t.Fatalf("MIME 两侧不一致：%q vs %q", generator.MimeTypePNG, store.MimeTypePNG)
	}
	if generator.MaxImageAssetBytes != store.MaxAssetBytes {
		t.Fatalf("单题资产上限两侧不一致：生成器 %d vs 存储 %d",
			generator.MaxImageAssetBytes, store.MaxAssetBytes)
	}
	if store.MaxAssetBytes > DefaultConfig().ImageMaxTotalBytes {
		t.Fatal("默认图片字节预算连一条题都放不下：这种默认值会让图片池永远为空")
	}
}

// TestImageConfigValidation 图片水位/预算的配置边界：负数与"永远达不到"的组合一律拒绝。
func TestImageConfigValidation(t *testing.T) {
	base := DefaultConfig()
	rejects := map[string]func(*Config){
		"低水位为零":     func(c *Config) { c.ImageLowWatermark = 0 },
		"高水位低于低水位":  func(c *Config) { c.ImageHighWatermark = 3; c.ImageLowWatermark = 5 },
		"预算小于单题上限":  func(c *Config) { c.ImageMaxTotalBytes = store.MaxAssetBytes - 1 },
		"预算无上界":     func(c *Config) { c.ImageMaxTotalBytes = MaxImageBudgetBytes + 1 },
		"行数控不住图片水位": func(c *Config) { c.MaxRows = 10; c.ImageHighWatermark = 20 },
		"让步为负":      func(c *Config) { c.ImageYieldPause = -time.Second },
	}
	for name, mutate := range rejects {
		c := base
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("%s：应被配置校验拒绝", name)
		} else if !errors.Is(err, ErrBadConfig) {
			t.Fatalf("%s：错误类别不对: %v", name, err)
		}
	}
	// 合法的最小组合必须通过：预算恰好等于一条题的上限
	c := base
	c.ImageMaxTotalBytes = store.MaxAssetBytes
	if err := c.Validate(); err != nil {
		t.Fatalf("最小合法预算被拒: %v", err)
	}
}

// --- 夹具：一个"声明 image 但不产出资产"的生成器 ---

type assetlessImageGenerator struct {
	inner generator.TextImage
}

func (a assetlessImageGenerator) ID() string      { return "assetless-image-test-v1" }
func (a assetlessImageGenerator) Version() string { return "v1" }
func (a assetlessImageGenerator) Media() string   { return generator.MediaImage }
func (a assetlessImageGenerator) NormalizeAnswer(s string) (string, bool) {
	return a.inner.NormalizeAnswer(s)
}
func (a assetlessImageGenerator) Verify(v generator.PrivateVerifier, s string) bool {
	return a.inner.Verify(v, s)
}
func (a assetlessImageGenerator) SanitizePublic(raw []byte) (map[string]any, error) {
	return a.inner.SanitizePublic(raw)
}
func (a assetlessImageGenerator) Generate(ctx context.Context, p generator.Params, r io.Reader) (generator.Question, error) {
	q, err := a.inner.Generate(ctx, p, r)
	q.Asset = nil // 故意留空：模拟一个"渲染失败却返回成功"的实现
	return q, err
}

func mustTextImage(t *testing.T) generator.TextImage {
	t.Helper()
	g, err := generator.NewTextImage(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return g
}
