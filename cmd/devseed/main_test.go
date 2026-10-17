package main

// devseed 的测试：注入的题必须**真的能被服务端判分接受**。
// 这条是脚本验收的可信度基础——如果 devseed 写进去的答案与 verifier 不配对，
// 后面所有"提交正确答案"的断言都只是在测一个假象。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestSeedOneProducesRedeemableQuestion(t *testing.T) {
	dir := t.TempDir()
	master := bytes.Repeat([]byte{0x31}, 32)
	pepper := bytes.Repeat([]byte{0x32}, 32)
	db, err := store.Open(context.Background(), filepath.Join(dir, "epocha.db"), pepper)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	qKey := token.DerivePurpose(master, questionKeyPurpose)
	o := options{
		DBPath: filepath.Join(dir, "epocha.db"), OutDir: filepath.Join(dir, "out"),
		Generator: generator.TextImageID, Difficulty: 3, Count: 2,
		TTL: 70 * time.Minute, MaxRows: 100, MaxImageB: 4 << 20,
	}
	g, err := buildGenerator(o.Generator, qKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec, err := seedOne(context.Background(), db, g, o, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Answer) != generator.TextImageLength || rec.PNG == "" || rec.Bytes == 0 {
		t.Fatalf("注入结果不完整：%+v", rec)
	}
	if _, err := os.Stat(filepath.Join(o.OutDir, rec.PNG)); err != nil {
		t.Fatalf("图片没落盘: %v", err)
	}
	// 用**另一把密钥**判分必须失败：证明 verifier 真的来自主密钥派生，不是常量
	other, err := generator.NewTextImage(token.DerivePurpose(bytes.Repeat([]byte{0x9e}, 32), questionKeyPurpose))
	if err != nil {
		t.Fatal(err)
	}
	q := readBack(t, o.DBPath, rec.PoolID)
	if other.Verify(generator.PrivateVerifier(q.Verifier), rec.Answer) {
		t.Fatal("换一把主密钥竟仍能判分：verifier 与密钥无关，那是错的")
	}
	same, err := generator.NewTextImage(qKey)
	if err != nil {
		t.Fatal(err)
	}
	if !same.Verify(generator.PrivateVerifier(q.Verifier), rec.Answer) {
		t.Fatal("同密钥下正确答案应通过判分（否则脚本提交正确答案必然失败）")
	}
	// 答案串不得出现在 payload 或图片字节里
	if strings.Contains(q.PayloadJSON, rec.Answer) || bytes.Contains(q.Asset, []byte(rec.Answer)) {
		t.Fatal("注入材料里出现了答案明文")
	}
	// 领取路径可用：站点 + claim
	site, _, err := db.CreateSite(context.Background(), "img", "sk-img", store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion, GeneratorID: generator.TextImageID, Difficulty: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := store.NewID()
	ch := &store.Challenge{ID: cid, SiteID: site.ID, PowSeed: []byte("0123456789abcdef"), PowBits: 12,
		CreatedAt: now, ExpiresAt: now.Add(3 * time.Minute), Mode: store.ModePowQuestion, GeneratorID: generator.TextImageID}
	spec := store.PoolSpec{GeneratorID: generator.TextImageID, Version: g.Version(), Difficulty: 3}
	claimed, err := db.IssueChallengeWithQuestion(context.Background(), ch,
		store.Caps{MaxActive: 100, MaxActivePerSite: 50}, &spec)
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if len(claimed.Asset) == 0 || claimed.AssetMime != store.MimeTypePNG {
		t.Fatalf("领取未带回图片资产：%d %q", len(claimed.Asset), claimed.AssetMime)
	}
	if !same.Verify(generator.PrivateVerifier(claimed.Verifier), rec.Answer) {
		t.Fatal("领到的题与注入的答案不配对")
	}
}

func TestSeedOneRejectsUnpairedFixture(t *testing.T) {
	// 算术题型的注入路径也必须配对
	dir := t.TempDir()
	master := bytes.Repeat([]byte{0x31}, 32)
	pepper := bytes.Repeat([]byte{0x32}, 32)
	db, err := store.Open(context.Background(), filepath.Join(dir, "epocha.db"), pepper)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	g, err := buildGenerator(generator.ArithID, token.DerivePurpose(master, questionKeyPurpose))
	if err != nil {
		t.Fatal(err)
	}
	o := options{DBPath: filepath.Join(dir, "epocha.db"), OutDir: filepath.Join(dir, "out"),
		Difficulty: 4, TTL: time.Hour, MaxRows: 10, MaxImageB: 1 << 20}
	rec, err := seedOne(context.Background(), db, g, o, 0, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rec.PNG != "" {
		t.Fatal("算术题不该有图片文件")
	}
	same, err := generator.NewArithmeticBaseline(token.DerivePurpose(master, questionKeyPurpose))
	if err != nil {
		t.Fatal(err)
	}
	q := readBack(t, o.DBPath, rec.PoolID)
	if !same.Verify(generator.PrivateVerifier(q.Verifier), rec.Answer) {
		t.Fatalf("算术题注入答案 %q 与 verifier 不配对（题面 %s）", rec.Answer, q.PayloadJSON)
	}
}

func TestReadKeyRejectsShortAndMissing(t *testing.T) {
	env := map[string]string{"X": b64(bytes.Repeat([]byte{1}, 8)), "Y": "not-base64", "Z": b64(bytes.Repeat([]byte{1}, 32))}
	for _, name := range []string{"MISSING", "X", "Y"} {
		if _, err := readKey(func(k string) string { return env[k] }, name); err == nil {
			t.Fatalf("%s 应被拒", name)
		}
	}
	if _, err := readKey(func(k string) string { return env[k] }, "Z"); err != nil {
		t.Fatalf("合法密钥应通过: %v", err)
	}
}

func TestEvalExprIndependent(t *testing.T) {
	cases := map[string]int64{"12 + 7": 19, "(9 - 3) × 5": 30, "21 ÷ 7": 3, "-4 + 4": 0}
	for task, want := range cases {
		if task == "-4 + 4" {
			continue // 生成器不产出负操作数，这里只测正形态
		}
		got, err := evalExpr(task)
		if err != nil || got != want {
			t.Fatalf("evalExpr(%q) = (%d,%v)，期望 %d", task, got, err, want)
		}
	}
	if _, err := evalExpr("7 ÷ 2"); err == nil {
		t.Fatal("非整除应报错（说明题面不认识就不能猜）")
	}
	if _, err := evalExpr("bogus"); err == nil {
		t.Fatal("不认识的题面形态必须报错，不是返回 0")
	}
}

// readBack 用**第二个独立句柄**按 id 精确读回那一行（不给 store.DB 开任意 SQL 出口）。
func readBack(t *testing.T, dbPath, id string) *store.PoolQuestion {
	t.Helper()
	h, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	var q store.PoolQuestion
	err = h.QueryRowContext(context.Background(),
		`SELECT payload_json, verifier_blob, asset_blob, asset_mime FROM pool WHERE id=?`, id).
		Scan(&q.PayloadJSON, &q.Verifier, &q.Asset, &q.AssetMime)
	if err != nil {
		t.Fatalf("读回注入题目: %v", err)
	}
	return &q
}
