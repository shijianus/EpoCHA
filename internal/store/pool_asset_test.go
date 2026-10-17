package store

// 阶段 3B 的存储侧测试：图片资产列的库内约束、字节预算的原子判定、
// 统计不把 BLOB 读进 Go、判分与资产解耦，以及 0003→0004 的增量迁移。

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// arithGenID 与 internal/generator.ArithID 同值：store 不 import 生成器（依赖方向是
// 生成器 → 存储 ← 池），所以测试里写字面量，与 pool_flow_test.go 保持一致。
const arithGenID = "arithmetic-baseline-v1"

// pngOf 造一段指定长度的"像 PNG 的"字节。存储层不看内容（那是生成器与 API 的事），
// 它只管"多长、什么 MIME、预算够不够"。
func pngOf(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("\x89PNG\r\n\x1a\n"))
	for i := 8; i < n; i++ {
		b[i] = byte(i % 251)
	}
	return b
}

func imageQuestion(id string, asset []byte) *PoolQuestion {
	now := time.Now().UTC()
	return &PoolQuestion{
		ID: id, GeneratorID: "text-image-v1", Version: "v1", Difficulty: 2,
		Status: PoolReady,
		PayloadJSON: `{"length":6,"charset":"ABCDEFGHJKLMNPQRSTUVWXYZ23456789",` +
			`"answer_type":"text","max_answer_len":6,"image_width":280,"image_height":104}`,
		Verifier:  []byte(strings.Repeat("v", 48)),
		Asset:     asset,
		AssetMime: MimeTypePNG,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
}

// TestAssetColumnChecksAreReal 钉住"上限写在库里，不只是写在 Go 常量里"。
// 走**裸 SQL**（绕过 InsertQuestion 的 Go 侧检查）：如果只有 Go 侧有界，
// 任何一条不经该函数的写入路径（未来的导入工具、手工修库）都能塞进一张巨图。
func TestAssetColumnChecksAreReal(t *testing.T) {
	d := newDB(t)
	questionSite(t, d, "alpha")

	raw := func(tag string, asset []byte, mime string) error {
		_, err := d.h.ExecContext(context.Background(), `
INSERT INTO pool(id, generator_id, version, difficulty, status, payload_json, verifier_blob,
                 created_at, expires_at, asset_blob, asset_mime)
VALUES(?,'text-image-v1','v1',2,'ready','{}',x'00',
       '2026-01-01T00:00:00.000000000Z','2030-01-01T00:00:00.000000000Z',?,?)`,
			tag, asset, mime)
		return err
	}
	// 恰好等于上限：必须能写进去（"越界检查"写成"≥ 即拒"会让最后一格容量凭空消失）
	if err := raw("raw-at-limit", pngOf(MaxAssetBytes), MimeTypePNG); err != nil {
		t.Fatalf("恰好 %d 字节应可写入: %v", MaxAssetBytes, err)
	}
	if err := raw("raw-over-limit", pngOf(MaxAssetBytes+1), MimeTypePNG); err == nil {
		t.Fatal("超出单题资产上限的写入必须被库内 CHECK 拒绝")
	}
	if err := raw("raw-bad-mime", pngOf(16), "image/svg+xml"); err == nil {
		t.Fatal("MIME 必须被限制在允许集合（浏览器侧 data: 类型不能由数据自证）")
	}
	if err := raw("raw-no-asset", nil, ""); err != nil {
		t.Fatalf("无资产的行应可写入（算术题就是这种形状）: %v", err)
	}
}

// TestAssetAndMimeMustPair Go 侧拒绝"只有一半"的记录。
func TestAssetAndMimeMustPair(t *testing.T) {
	d := newDB(t)
	limits := PoolLimits{MaxRows: 100, MaxImageBytes: 8 << 20}

	assetless := imageQuestion("pair-1", pngOf(64))
	assetless.AssetMime = ""
	if err := d.InsertQuestion(context.Background(), assetless, limits); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("有资产无 MIME 应被拒，got %v", err)
	}
	mimeless := imageQuestion("pair-2", nil)
	mimeless.AssetMime = MimeTypePNG
	if err := d.InsertQuestion(context.Background(), mimeless, limits); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("有 MIME 无资产应被拒，got %v", err)
	}
	big := imageQuestion("pair-3", pngOf(MaxAssetBytes+1))
	if err := d.InsertQuestion(context.Background(), big, limits); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("超过 Go 侧上限应被拒，got %v", err)
	}
}

// TestImageBudgetIsJudgedAtomically 字节预算与行数上限是**两条独立的**判定，
// 且都必须在写入的同一条 SQL 里完成（与 ADR-009 对存量的处理同一手法）。
func TestImageBudgetIsJudgedAtomically(t *testing.T) {
	d := newDB(t)
	questionSite(t, d, "alpha")
	small := pngOf(1000)
	budget := int64(2*len(small) + 500) // 只够两条

	insert := func(id string, asset []byte) error {
		q := imageQuestion(id, asset)
		if asset == nil {
			q.AssetMime = ""
		}
		return d.InsertQuestion(context.Background(), q, PoolLimits{MaxRows: 1000, MaxImageBytes: budget})
	}
	if err := insert("b-1", small); err != nil {
		t.Fatal(err)
	}
	if err := insert("b-2", small); err != nil {
		t.Fatal(err)
	}
	err := insert("b-3", small)
	if !errors.Is(err, ErrImageBudget) {
		t.Fatalf("第三条应触发图片预算，got %v", err)
	}
	// 行数上限远未触及：证明报错类别来自预算而不是行数
	if n, _ := d.PoolCount(context.Background()); n != 2 {
		t.Fatalf("池行数 %d，期望 2（预算拒绝不得留下半成品行）", n)
	}
	// 文本题不受图片预算影响：预算为 0 也照样能写没有资产的行
	if err := insert("b-text", nil); err != nil {
		t.Fatalf("无资产题目不应受图片预算限制: %v", err)
	}
	used, err := d.ImageAssetBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(2 * len(small)); used != want {
		t.Fatalf("ImageAssetBytes = %d，期望 %d", used, want)
	}
}

// TestPoolStatsAggregatesBytesWithoutLoadingBlobs 统计只回整数。
// 这条断言的是"聚合值正确"；不加载 BLOB 这一点由 SQL 形态保证（sum(length(...))），
// 若有人改成 SELECT asset_blob 再在 Go 里累加，本用例仍绿——所以同时留了一条源码检查。
func TestPoolStatsAggregatesBytesWithoutLoadingBlobs(t *testing.T) {
	d := newDB(t)
	questionSite(t, d, "alpha")
	limits := PoolLimits{MaxRows: 1000, MaxImageBytes: 8 << 20}
	var imageTotal int
	for i := 0; i < 3; i++ {
		b := pngOf(500 + i*111)
		imageTotal += len(b)
		if err := d.InsertQuestion(context.Background(), imageQuestion(fmt.Sprintf("st-%d", i), b), limits); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		q := imageQuestion(fmt.Sprintf("st-t%d", i), nil)
		q.AssetMime = ""
		q.GeneratorID = arithGenID
		if err := d.InsertQuestion(context.Background(), q, limits); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := d.PoolStats(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var gotImage, gotText *PoolRowCounts
	for _, r := range rows {
		if r.GeneratorID == arithGenID {
			gotText = r
		} else {
			gotImage = r
		}
	}
	if gotImage == nil || gotText == nil {
		t.Fatal("两个分桶都应出现在统计里")
	}
	if gotImage.Total != 3 || gotImage.Ready != 3 {
		t.Fatalf("图片桶计数 %+v", gotImage)
	}
	if gotImage.TotalAssetBytes != int64(imageTotal) {
		t.Fatalf("图片桶字节 %d ≠ 实际 %d", gotImage.TotalAssetBytes, imageTotal)
	}
	if gotImage.ReadyAssetBytes != int64(imageTotal) {
		t.Fatalf("ready 字节 %d ≠ %d", gotImage.ReadyAssetBytes, imageTotal)
	}
	if gotText.Total != 2 || gotText.TotalAssetBytes != 0 || gotText.ReadyAssetBytes != 0 {
		t.Fatalf("算术桶不该带任何图片字节：%+v", gotText)
	}
}

// TestVerifierReadPathDoesNotNeedAsset 判分只读 verifier：
// 图片资产被清空（未来的资产清理任务、或库文件被部分损坏）时，正确答案仍然通过。
// 这条把"资产不参与验证"从口头承诺变成回归测试。
func TestVerifierReadPathDoesNotNeedAsset(t *testing.T) {
	d := newDB(t)
	s := questionSite(t, d, "alpha")
	limits := PoolLimits{MaxRows: 100, MaxImageBytes: 8 << 20}
	q := imageQuestion("vr-1", pngOf(2048))
	if err := d.InsertQuestion(context.Background(), q, limits); err != nil {
		t.Fatal(err)
	}
	// challenge 必须比题目早过期（领题条件要求题目比 challenge 晚过期）
	c := newChallengeRow(s.ID, 10*time.Minute)
	spec := PoolSpec{GeneratorID: q.GeneratorID, Version: q.Version, Difficulty: q.Difficulty}
	claimed, err := d.IssueChallengeWithQuestion(context.Background(), c, Caps{MaxActive: 100, MaxActivePerSite: 100}, &spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed.Asset) != 2048 || claimed.AssetMime != MimeTypePNG {
		t.Fatalf("领取路径应带回那一题的资产：%d 字节 mime=%q", len(claimed.Asset), claimed.AssetMime)
	}
	v, err := d.QuestionVerifierForChallenge(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != len(q.Verifier) || !bytes.Equal(v, q.Verifier) {
		t.Fatal("判分读路径返回的 verifier 与写入值不符")
	}
	// 资产从库里消失（只留 verifier）：兑换照常
	if _, err := d.h.ExecContext(context.Background(),
		`UPDATE pool SET asset_blob=NULL, asset_mime='' WHERE id=?`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	v2, err := d.QuestionVerifierForChallenge(context.Background(), c.ID)
	if err != nil || !bytes.Equal(v, v2) {
		t.Fatalf("资产被清空后 verifier 应不变：%v err=%v", err != nil, err)
	}
	res, err := d.RedeemAttempt(context.Background(), c.ID, time.Now().UTC(), time.Minute, true)
	if err != nil || res.Outcome != OutcomeIssued {
		t.Fatalf("没有图片也应能完成兑换（判分不依赖资产）: %v %+v", err, res)
	}
	if _, err := d.QuestionVerifierForChallenge(context.Background(), "no-such-challenge"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未绑定的 challenge 应返回 ErrNotFound，got %v", err)
	}
}

// TestPurgeReleasesImageBytes 图片字节随池行一起被清理：
// 否则"预算"会变成永久占用的死账，图片池迟早永远补不进题。
func TestPurgeReleasesImageBytes(t *testing.T) {
	d := newDB(t)
	questionSite(t, d, "alpha")
	limits := PoolLimits{MaxRows: 100, MaxImageBytes: 8 << 20}
	if err := d.InsertQuestion(context.Background(), imageQuestion("pg-1", pngOf(4096)), limits); err != nil {
		t.Fatal(err)
	}
	before, _ := d.ImageAssetBytes(context.Background())
	if before != 4096 {
		t.Fatalf("前置条件：已用字节 %d", before)
	}
	// 过期行（ready 且 expires_at < now）应被清掉
	if _, err := d.Purge(context.Background(), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	after, _ := d.ImageAssetBytes(context.Background())
	if after != 0 {
		t.Fatalf("清理后仍占 %d 字节图片预算", after)
	}
}

// TestMigration0004FromStage3ADatabase 拿一个**只有 0001..0003** 的库让当前程序打开：
// 0004 必须是纯增量（旧行 asset 为 NULL、旧流程不变），而不是"重建表"。
func TestMigration0004FromStage3ADatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stage3a.db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	h, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{migration0001, migration0002, migration0003} {
		if _, err := h.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("应用阶段迁移: %v", err)
		}
	}
	ctx := context.Background()
	// meta 表由 store.migrate 创建（不在 0001 里），所以手工搭 3A 库时也要建它
	if _, err := h.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
		 INSERT INTO meta(k, v) VALUES('schema_version', '3')`); err != nil {
		t.Fatal(err)
	}
	// 一条 3A 形状的题（没有资产列的取值）与一个站点
	if _, err := h.ExecContext(ctx, `
INSERT INTO pool(id, generator_id, version, difficulty, status, payload_json, verifier_blob, created_at, expires_at)
VALUES('legacy-1','arithmetic-baseline-v1','v1',3,'ready','{"task":"1 + 1","answer_type":"integer","max_answer_len":14}',
       x'0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20',
       '2026-01-01T00:00:00.000000000Z','2030-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(ctx, path, testPepper())
	if err != nil {
		t.Fatalf("打开阶段 3A 的库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if v, _ := d.SchemaVersion(ctx); v != 4 {
		t.Fatalf("升级后版本 = %d，期望 4", v)
	}
	// 旧行完整保留，且新列是 NULL/''（不静默改变既有行的语义）
	var (
		assetNull bool
		mime      string
		status    string
	)
	err = d.h.QueryRowContext(ctx,
		`SELECT asset_blob IS NULL, asset_mime, status FROM pool WHERE id='legacy-1'`).
		Scan(&assetNull, &mime, &status)
	if err != nil {
		t.Fatalf("旧题目行丢失: %v", err)
	}
	if !assetNull || mime != "" || status != PoolReady {
		t.Fatalf("旧行被改写：assetNULL=%t mime=%q status=%q", assetNull, mime, status)
	}
	// 旧行仍可被领取并终结（幂等升级不改变任何安全语义）
	s, _, err := d.CreateSite(ctx, "alpha", "sk-alpha", SitePolicy{PowBits: 14, VerifyMode: ModePowQuestion, GeneratorID: arithGenID, Difficulty: 3})
	if err != nil {
		t.Fatal(err)
	}
	c := newChallengeRow(s.ID, 10*time.Minute)
	spec := PoolSpec{GeneratorID: arithGenID, Version: "v1", Difficulty: 3}
	claimed, err := d.IssueChallengeWithQuestion(ctx, c, Caps{MaxActive: 100, MaxActivePerSite: 100}, &spec)
	if err != nil {
		t.Fatalf("升级后的旧题应仍可领取: %v", err)
	}
	if claimed.ID != "legacy-1" || len(claimed.Asset) != 0 {
		t.Fatalf("领取结果不符：%s asset=%d", claimed.ID, len(claimed.Asset))
	}
	if res, err := d.RedeemAttempt(ctx, c.ID, time.Now().UTC(), time.Minute, true); err != nil || res.Outcome != OutcomeIssued {
		t.Fatalf("旧题兑换失败: %v %+v", err, res)
	}
	// 二次打开不再改动版本（迁移幂等）
	if _, err := Open(ctx, path, testPepper()); err != nil {
		t.Fatalf("重复打开失败: %v", err)
	}
}

// TestInsertLimitsValidated 上限参数本身也要有界：
// MaxRows=0 或负的预算不能被解释成"不限"，那会把资源边界静默关掉。
func TestInsertLimitsValidated(t *testing.T) {
	d := newDB(t)
	questionSite(t, d, "alpha")
	q := imageQuestion("lim-1", nil)
	q.AssetMime = ""
	if err := d.InsertQuestion(context.Background(), q, PoolLimits{MaxRows: 0, MaxImageBytes: 1 << 20}); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("MaxRows=0 应被拒，got %v", err)
	}
	if err := d.InsertQuestion(context.Background(), q, PoolLimits{MaxRows: 10, MaxImageBytes: -1}); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("负预算应被拒，got %v", err)
	}
	// 预算为 0 的含义是"禁止任何图片资产"，不是"不限"
	if err := d.InsertQuestion(context.Background(), imageQuestion("lim-2", pngOf(64)),
		PoolLimits{MaxRows: 10, MaxImageBytes: 0}); !errors.Is(err, ErrImageBudget) && !errors.Is(err, ErrBadConfig) {
		t.Fatalf("预算 0 应拒绝图片行，got %v", err)
	}
}
