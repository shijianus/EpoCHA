package store

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPepper() []byte { return []byte(strings.Repeat("p", 32)) }

// testCaps 是测试用的宽松存量上限；封顶行为由 TestActiveChallengeCapBoundsLedger 专门验证。
var testCaps = Caps{MaxActive: 10000, MaxActivePerSite: 5000}

// newDB 在 t.TempDir() 里开一个独立库；t.Cleanup 负责关闭。
func newDB(t *testing.T) *DB {
	t.Helper()
	d, _ := newDBAt(t, filepath.Join(t.TempDir(), "epocha.db"))
	return d
}

func newDBAt(t *testing.T, path string) (*DB, string) {
	t.Helper()
	d, err := Open(t.Context(), path, testPepper())
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, path
}

func mustSite(t *testing.T, d *DB, name string) *Site {
	t.Helper()
	s := mustSiteSecret(t, d, name, "secret-"+name+"-SENTINEL")
	return s
}

// mustSiteSecret 建一个测试站点并返回它；secret 由调用方给定，便于直接拿去验证。
func mustSiteSecret(t *testing.T, d *DB, name, secret string) *Site {
	t.Helper()
	s, _, err := d.CreateSite(t.Context(), name, "sk-"+name, SitePolicy{PowBits: 14})
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if secret != "" {
		s = replaceTestSecret(t, d, s, secret)
	}
	return s
}

// replaceTestSecret 把站点的第一把凭据换成调用方指定的 secret：
// 先把新 secret 登记进去，再立即撤销旧的那把，模拟真实轮换后"只剩一把有效"的形态。
func replaceTestSecret(t *testing.T, d *DB, s *Site, secret string) *Site {
	t.Helper()
	if _, err := d.h.ExecContext(t.Context(),
		`DELETE FROM site_secrets WHERE site_id=?`, s.ID); err != nil {
		t.Fatalf("清空测试凭据: %v", err)
	}
	if _, err := d.h.ExecContext(t.Context(),
		`INSERT INTO site_secrets(key_id, site_id, secret_mac, created_at) VALUES(?, ?, ?, ?)`,
		"test-"+fmt.Sprint(s.ID), s.ID, hex.EncodeToString(d.SecretMac(secret)), formatTime(time.Now().UTC())); err != nil {
		t.Fatalf("登记测试凭据: %v", err)
	}
	return s
}

func newChallenge(t *testing.T, d *DB, siteID int64, ttl time.Duration) *Challenge {
	t.Helper()
	c := newChallengeCaps(t, d, siteID, ttl, testCaps)
	return c
}

// newChallengeCaps 用指定上限签发一条 challenge，便于测试存量封顶行为。
func newChallengeCaps(t *testing.T, d *DB, siteID int64, ttl time.Duration, caps Caps) *Challenge {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	c := &Challenge{ID: id, SiteID: siteID, PowSeed: []byte("0123456789abcdef"), PowBits: 12,
		CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := d.InsertChallenge(t.Context(), c, caps); err != nil {
		t.Fatalf("InsertChallenge: %v", err)
	}
	return c
}

func TestOpenAppliesPragmas(t *testing.T) {
	d := newDB(t)
	for q, want := range map[string]string{
		"journal_mode": "wal",
		"busy_timeout": "5000",
		"foreign_keys": "1",
		"synchronous":  "1",
		"user_version": "0",
	} {
		var got string
		if err := d.h.QueryRowContext(t.Context(), "PRAGMA "+q).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", q, err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %q, want %q", q, got, want)
		}
	}
	v, err := d.SchemaVersion(t.Context())
	if err != nil || v != schemaVersion {
		t.Fatalf("schema 版本 = %d, err %v", v, err)
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	if _, err := Open(t.Context(), "", testPepper()); !errors.Is(err, ErrBadConfig) {
		t.Errorf("空路径应拒绝，got %v", err)
	}
	if _, err := Open(t.Context(), filepath.Join(t.TempDir(), "x.db"), []byte("short")); !errors.Is(err, ErrBadConfig) {
		t.Errorf("pepper 过短应拒绝，got %v", err)
	}
}

// 迁移必须幂等：重复 Open 不得报错、不得丢数据、版本不变。
func TestMigrationIdempotentAndDataSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "epocha.db")
	d1, _ := newDBAt(t, path)
	s := mustSite(t, d1, "alpha")
	c := newChallenge(t, d1, s.ID, time.Minute)
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(t.Context(), path, testPepper())
	if err != nil {
		t.Fatalf("二次 Open: %v", err)
	}
	if v, _ := d2.SchemaVersion(t.Context()); v != schemaVersion {
		t.Fatalf("重开后版本 = %d", v)
	}
	if _, err := d2.Challenge(t.Context(), c.ID); err != nil {
		t.Fatalf("重开后 challenge 丢失: %v", err)
	}
	got, err := d2.SiteByKey(t.Context(), "sk-alpha")
	if err != nil || got.ID != s.ID {
		t.Fatalf("重开后站点丢失: %v %+v", err, got)
	}
	for i := 1; i <= 3; i++ {
		dn, err := Open(t.Context(), path, testPepper())
		if err != nil {
			t.Fatalf("第 %d 次重复 Open: %v", i, err)
		}
		_ = dn.Close()
	}
}

func TestCreateSiteValidates(t *testing.T) {
	d := newDB(t)
	if _, _, err := d.CreateSite(t.Context(), "x", "k", SitePolicy{PowBits: 11}); !errors.Is(err, ErrBadConfig) {
		t.Error("bits 下界外应拒绝")
	}
	if _, _, err := d.CreateSite(t.Context(), "x", "k", SitePolicy{PowBits: 25}); !errors.Is(err, ErrBadConfig) {
		t.Error("bits 上界外应拒绝")
	}
	if _, _, err := d.CreateSite(t.Context(), "x", "k", SitePolicy{PowBits: 14, ChallengeTTL: 2 * time.Hour}); !errors.Is(err, ErrBadConfig) {
		t.Error("TTL 越界应拒绝")
	}
	if _, _, err := d.CreateSite(t.Context(), " x ", "k", SitePolicy{PowBits: 14}); !errors.Is(err, ErrBadConfig) {
		t.Error("带空白的站点名应拒绝（审计与日志要打印它）")
	}
	if _, _, err := d.CreateSite(t.Context(), "x", "k", SitePolicy{PowBits: 14, RateSiteRPM: -1}); !errors.Is(err, ErrBadConfig) {
		t.Error("负 rpm 应拒绝")
	}
	site, secret, err := d.CreateSite(t.Context(), "x", "k", SitePolicy{PowBits: 14})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, SecretPrefix) || len(secret) < 32 {
		t.Fatalf("生成的 secret 形状不符: len=%d", len(secret))
	}
	if _, _, err := d.CreateSite(t.Context(), "y", "k", SitePolicy{PowBits: 14}); !errors.Is(err, ErrConflict) {
		t.Errorf("site_key 重复应 ErrConflict，got %v", err)
	}
	if _, _, err := d.CreateSite(t.Context(), "x", "z", SitePolicy{PowBits: 14}); !errors.Is(err, ErrConflict) {
		t.Errorf("站点名重复应 ErrConflict，got %v", err)
	}
	// 创建即有一把有效凭据：否则会出现"站点存在但永远无法验证"的半状态
	if _, err := d.FindSiteBySecret(t.Context(), secret, time.Now()); err != nil {
		t.Fatalf("新建站点的凭据不可用: %v", err)
	}
	if got, _ := d.ListSecrets(t.Context(), site.ID); len(got) != 1 {
		t.Fatalf("新建站点应有 1 把凭据，got %d", len(got))
	}
}

func TestUpdateSiteName(t *testing.T) {
	d := newDB(t)
	site, _, err := d.CreateSite(t.Context(), "alpha", "pk_alpha", SitePolicy{PowBits: 14})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.CreateSite(t.Context(), "beta", "pk_beta", SitePolicy{PowBits: 14}); err != nil {
		t.Fatal(err)
	}
	before := site.Version

	// nil = 不改：省略 Name 的策略更新不能把名字清空或改掉。
	seven := 7
	got, err := d.UpdateSite(t.Context(), site.ID, SiteUpdate{PowBits: uint8p(16), RateSiteRPM: &seven})
	if err != nil {
		t.Fatalf("只改策略字段: %v", err)
	}
	if got.Name != "alpha" {
		t.Errorf("未提供 name 时不应改动显示名，got %q", got.Name)
	}

	got, err = d.UpdateSite(t.Context(), site.ID, SiteUpdate{Name: strp("alpha-renamed")})
	if err != nil {
		t.Fatalf("改名字: %v", err)
	}
	if got.Name != "alpha-renamed" {
		t.Fatalf("名字未生效，got %q", got.Name)
	}
	if got.PowBits != 16 || got.Policy.RateSiteRPM != 7 {
		t.Errorf("改名字不能顺带重置其他字段：%+v", got.Policy)
	}
	// 显示名不参与任何判定，改它不是"立即生效的安全动作"，因此不提升站点版本。
	if got.Version != before {
		t.Errorf("改名字不应提升站点版本：before=%d after=%d", before, got.Version)
	}

	// 重启后仍在（写的是库，不是内存）。
	path := d.path
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(t.Context(), path, testPepper())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d2.Close() })
	if s2, err := d2.SiteByKey(t.Context(), "pk_alpha"); err != nil || s2.Name != "alpha-renamed" {
		t.Fatalf("重启后名字丢失：%v %+v", err, s2)
	}

	// 与创建路径同一套边界：空白、空串、超长都拒。
	for _, bad := range []string{"  x  ", "", strings.Repeat("n", MaxNameLen+1), "带\n换行"} {
		if _, err := d2.UpdateSite(t.Context(), site.ID, SiteUpdate{Name: strp(bad)}); !errors.Is(err, ErrBadConfig) {
			t.Errorf("非法名字 %q 应 ErrBadConfig，got %v", bad, err)
		}
	}
	// 撞名是 UNIQUE 冲突：报 ErrConflict，且原站点保持不动（这条 UPDATE 要么整体生效要么不生效）。
	beta, err := d2.SiteByKey(t.Context(), "pk_beta")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d2.UpdateSite(t.Context(), beta.ID, SiteUpdate{Name: strp("alpha-renamed")}); !errors.Is(err, ErrConflict) {
		t.Errorf("重复站点名应 ErrConflict，got %v", err)
	}
	if again, err := d2.SiteByID(t.Context(), site.ID); err != nil || again.Name != "alpha-renamed" {
		t.Fatalf("失败的改名不应影响对方：%v %+v", err, again)
	}
	if again, err := d2.SiteByID(t.Context(), beta.ID); err != nil || again.Name != "beta" {
		t.Fatalf("撞名后自身应保持原值：%v %+v", err, again)
	}
}

func strp(s string) *string { return &s }
func uint8p(v uint8) *uint8 { return &v }

func TestSecretLookup(t *testing.T) {
	d := newDB(t)
	s := mustSiteSecret(t, d, "demo", "top-secret-value")
	now := time.Now()
	if got, err := d.FindSiteBySecret(t.Context(), "top-secret-value", now); err != nil || got.ID != s.ID {
		t.Fatalf("按 secret 定位站点失败: %v %+v", err, got)
	}
	if _, err := d.FindSiteBySecret(t.Context(), "Top-Secret-Value", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("大小写不同不应匹配，got %v", err)
	}
	if _, err := d.FindSiteBySecret(t.Context(), "nope", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知 secret 应 ErrNotFound，got %v", err)
	}
	// 定位到的站点必须带上策略字段——/siteverify 与签发都要靠它
	got, err := d.FindSiteBySecret(t.Context(), "top-secret-value", now)
	if err != nil || got.PowBits != 14 || got.Policy.ChallengeTTL == 0 {
		t.Fatalf("站点策略未随定位返回: %+v err %v", got, err)
	}
}

func TestLegacySitesColumnCannotAuthenticate(t *testing.T) {
	// 0002 迁移把 sites.secret_mac 中和掉；阶段 2 新建的站点该列只是占位。
	// 若哪段代码误读它，就会把"已撤销/不存在的凭据"当成有效——这条测试把它挡在编译期之外。
	d := newDB(t)
	s := mustSiteSecret(t, d, "alpha", "top-secret-value")
	var mac string
	if err := d.h.QueryRowContext(t.Context(), `SELECT secret_mac FROM sites WHERE id=?`, s.ID).Scan(&mac); err != nil {
		t.Fatal(err)
	}
	if subtle.ConstantTimeCompare([]byte(mac), []byte(hex.EncodeToString(d.SecretMac("top-secret-value")))) == 1 {
		t.Fatal("sites.secret_mac 仍等于可用摘要：迁移/新建路径没有中和它")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSecretNotStoredInPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	d, _ := newDBAt(t, path)
	const sentinel = "SENTINEL-SECRET-9f3c8a1e7b"
	mustSiteSecret(t, d, "s1", sentinel)
	if _, _, err := d.CreateSite(t.Context(), "s2", "sk-s2", SitePolicy{PowBits: 14}); err != nil {
		t.Fatal(err)
	}
	const adminSentinel = "SENTINEL-ADMINKEY-4d1f9c2a8b7e"
	if _, err := d.AddAdminKey(t.Context(), "ops", adminSentinel); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{sentinel, adminSentinel} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("DB 文件里出现凭据明文（%d 字节文件，哨兵长度 %d）", len(raw), len(secret))
		}
	}
}

func TestNewIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 32 {
			t.Fatalf("id 长度 %d", len(id))
		}
		if seen[id] {
			t.Fatal("id 重复")
		}
		seen[id] = true
	}
}

func TestChallengeValidationAndNotFound(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "alpha")
	if err := d.InsertChallenge(t.Context(), &Challenge{ID: "x"}, testCaps); !errors.Is(err, ErrBadConfig) {
		t.Error("字段不完整应拒绝")
	}
	if _, err := d.Challenge(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Error("不存在应 ErrNotFound")
	}
	if _, err := d.SiteByKey(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Error("站点不存在应 ErrNotFound")
	}
	c := newChallenge(t, d, s.ID, time.Minute)
	got, err := d.Challenge(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusIssued || got.Attempts != 0 || got.PowBits != 12 || string(got.PowSeed) != string(c.PowSeed) {
		t.Fatalf("回读不符 %+v", got)
	}
}

// 并发兑换：同一 challenge 只能有一个成功（DESIGN §17 阶段 1 ③）。
func TestConcurrentRedeemExactlyOneWins(t *testing.T) {
	for _, concurrency := range []int{2, 50, 100} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			d := newDB(t)
			s := mustSite(t, d, "alpha")
			c := newChallenge(t, d, s.ID, time.Minute)
			ok := redeemRace(t, d, c.ID, concurrency)
			if ok != 1 {
				t.Fatalf("%d 路并发成功 %d 次，应为 1", concurrency, ok)
			}
			if n, _ := d.CountChallenges(t.Context(), statusRedeemed); n != 1 {
				t.Fatalf("redeemed 行 = %d", n)
			}
		})
	}
}

func redeemRace(t *testing.T, d *DB, cid string, n int) int {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := d.Redeem(t.Context(), cid, time.Now(), time.Minute); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	return wins
}

// 两个独立 *sql.DB 句柄争抢同一文件：连接池形态不是安全边界，必须由库层条件更新兜住。
func TestTwoIndependentHandlesSameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	a, _ := newDBAt(t, path)
	s := mustSite(t, a, "alpha")
	c := newChallenge(t, a, s.ID, time.Minute)

	b, err := Open(t.Context(), path, testPepper())
	if err != nil {
		t.Fatalf("第二个句柄: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	race := func(d *DB) {
		defer wg.Done()
		if _, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Minute); err == nil {
			mu.Lock()
			wins++
			mu.Unlock()
		}
	}
	wg.Add(2)
	go race(a)
	go race(b)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("两个独立句柄并发成功 %d 次，应为 1", wins)
	}
	// 交叉复核：另一个句柄读到的状态一致
	got, err := b.Challenge(t.Context(), c.ID)
	if err != nil || got.Status != statusRedeemed {
		t.Fatalf("跨句柄状态不一致: %v %+v", err, got)
	}
}

func TestRedeemRejectsExpiredAndNonIssued(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "alpha")

	expired := newChallenge(t, d, s.ID, -time.Minute)
	if _, err := d.Redeem(t.Context(), expired.ID, time.Now(), time.Minute); !errors.Is(err, ErrNotRedeemable) {
		t.Errorf("过期 challenge 应不可兑换，got %v", err)
	}
	c := newChallenge(t, d, s.ID, time.Hour)
	r, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r.SiteID != s.ID || r.ChallengeID != c.ID || !r.ExpiresAt.After(r.CreatedAt) {
		t.Fatalf("response 记录不符 %+v", r)
	}
	if _, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Minute); !errors.Is(err, ErrNotRedeemable) {
		t.Errorf("重复兑换应失败，got %v", err)
	}
	if _, err := d.Redeem(t.Context(), "does-not-exist", time.Now(), time.Minute); !errors.Is(err, ErrNotRedeemable) {
		t.Errorf("不存在的 id 应统一失败，got %v", err)
	}
	if _, err := d.Redeem(t.Context(), c.ID, time.Now(), 0); !errors.Is(err, ErrBadConfig) {
		t.Errorf("TTL<=0 应拒绝，got %v", err)
	}
}

func TestFailAttemptExhaustsChallenge(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "alpha")
	c := newChallenge(t, d, s.ID, time.Minute)
	for i := 1; i <= MaxFailedAttempts; i++ {
		attempts, status, err := d.FailAttempt(t.Context(), c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if attempts != i {
			t.Fatalf("attempts = %d, want %d", attempts, i)
		}
		want := statusIssued
		if i == MaxFailedAttempts {
			want = statusInvalid
		}
		if status != want {
			t.Fatalf("第 %d 次失败后 status = %s, want %s", i, status, want)
		}
	}
	if _, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Minute); !errors.Is(err, ErrNotRedeemable) {
		t.Fatal("作废后的 challenge 不得再兑换")
	}
	if _, _, err := d.FailAttempt(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Error("不存在的 id 应 ErrNotFound")
	}
}

func TestConsumeResponseAtomicity(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "alpha")
	c := newChallenge(t, d, s.ID, time.Minute)
	r, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.ConsumeResponse(t.Context(), r.ID, s.ID, time.Now())
	if err != nil {
		t.Fatalf("首次消费: %v", err)
	}
	if v.ChallengeID != c.ID || !v.ChallengeCreatedAt.Equal(c.CreatedAt.Truncate(time.Nanosecond)) {
		t.Fatalf("回读事实不符 %+v vs %+v", v, c)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, s.ID, time.Now()); !errors.Is(err, ErrConsumed) {
		t.Fatalf("二次消费应失败，got %v", err)
	}
}

// 错误 secret 定位到的站点不得消费别的站点的 token，也不得改变其状态。
func TestConsumeResponseSiteBinding(t *testing.T) {
	d := newDB(t)
	a := mustSite(t, d, "alpha")
	b := mustSite(t, d, "bravo")
	c := newChallenge(t, d, a.ID, time.Minute)
	r, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, b.ID, time.Now()); !errors.Is(err, ErrConsumed) {
		t.Fatalf("跨站点消费必须失败，got %v", err)
	}
	// 站点 a 仍可正常消费：证明失败的尝试没有改动状态
	if _, err := d.ConsumeResponse(t.Context(), r.ID, a.ID, time.Now()); err != nil {
		t.Fatalf("原站点消费失败：%v", err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, a.ID, time.Now()); !errors.Is(err, ErrConsumed) {
		t.Fatal("第三次消费应失败")
	}
}

func TestConsumeResponseExpiry(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "alpha")
	c := newChallenge(t, d, s.ID, time.Minute)
	r, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, s.ID, time.Now().Add(2*time.Second)); !errors.Is(err, ErrConsumed) {
		t.Fatalf("过期 response 应失败，got %v", err)
	}
	if _, err := d.ConsumeResponse(t.Context(), "missing", s.ID, time.Now()); !errors.Is(err, ErrConsumed) {
		t.Fatalf("未知 rid 应统一 ErrConsumed，got %v", err)
	}
}

// 重启后不可重放：这是"状态在 SQLite 而非内存"的证明（DESIGN §17 阶段 1）。
func TestReplayAfterReopenFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	d, _ := newDBAt(t, path)
	s := mustSite(t, d, "alpha")
	c := newChallenge(t, d, s.ID, time.Hour)
	r, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, s.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d2, err := Open(t.Context(), path, testPepper())
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if _, err := d2.ConsumeResponse(t.Context(), r.ID, s.ID, time.Now()); !errors.Is(err, ErrConsumed) {
		t.Fatalf("重启后重放应失败，got %v", err)
	}
	if _, err := d2.Redeem(t.Context(), c.ID, time.Now(), time.Minute); !errors.Is(err, ErrNotRedeemable) {
		t.Fatalf("重启后重复兑换应失败，got %v", err)
	}
	// 未消费的 challenge 重开后仍可兑换（不是"全部拒绝"式的假安全）
	c2 := newChallenge(t, d2, s.ID, time.Hour)
	if _, err := d2.Redeem(t.Context(), c2.ID, time.Now(), time.Minute); err != nil {
		t.Fatalf("重开后未消费的 challenge 应可兑换: %v", err)
	}
}

// 两个独立句柄并发验证同一 response：恰好一个成功。
func TestTwoHandlesConsumeSameResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	a, _ := newDBAt(t, path)
	s := mustSite(t, a, "alpha")
	c := newChallenge(t, a, s.ID, time.Hour)
	r, err := a.Redeem(t.Context(), c.ID, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(t.Context(), path, testPepper())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	errs := []error{}
	run := func(d *DB) {
		defer wg.Done()
		_, err := d.ConsumeResponse(t.Context(), r.ID, s.ID, time.Now())
		mu.Lock()
		if err == nil {
			wins++
		} else {
			errs = append(errs, err)
		}
		mu.Unlock()
	}
	wg.Add(2)
	go run(a)
	go run(b)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("两个句柄并发消费成功 %d 次，应为 1", wins)
	}
	// 失败方必须拿到受控哨兵，而不是裸的驱动/约束错误文本：调用方据此决定对外错误码。
	for _, err := range errs {
		if !errors.Is(err, ErrConsumed) {
			t.Fatalf("竞争失败方应得到 ErrConsumed，got %v", err)
		}
	}
}

func TestOneResponsePerChallenge(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "alpha")
	c := newChallenge(t, d, s.ID, time.Hour)
	if _, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	// 手工再插一条同一 challenge 的 response，应被 UNIQUE 约束拒绝
	_, err := d.h.ExecContext(t.Context(), `
INSERT INTO responses(id, challenge_id, site_id, created_at, expires_at, used) VALUES(?,?,?,?,?,0)`,
		"dup-id", c.ID, s.ID, formatTime(time.Now().UTC()), formatTime(time.Now().Add(time.Minute).UTC()))
	if err == nil {
		t.Fatal("一个 challenge 不得签发两条 response")
	}
	if n, _ := d.CountChallenges(t.Context(), statusRedeemed); n != 1 {
		t.Fatalf("redeemed 计数 = %d", n)
	}
}

func TestPurgeBoundsLedgerSize(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "alpha")

	// 一条已被消费的账本：redeem → consume，Purge 应清掉
	used := newChallenge(t, d, s.ID, time.Hour)
	r, err := d.Redeem(t.Context(), used.ID, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, s.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	// 一条尝试次数耗尽而作废、且已过期的 challenge
	dead := newChallenge(t, d, s.ID, time.Hour)
	for range MaxFailedAttempts {
		if _, _, err := d.FailAttempt(t.Context(), dead.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.h.ExecContext(t.Context(), `UPDATE challenges SET expires_at=? WHERE id=?`,
		formatTime(time.Now().Add(-time.Hour).UTC()), dead.ID); err != nil {
		t.Fatal(err)
	}
	live := newChallenge(t, d, s.ID, time.Hour)

	c, err := d.Purge(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c.Responses < 1 || c.Challenges < 1 {
		t.Fatalf("清理行数 responses=%d challenges=%d", c.Responses, c.Challenges)
	}
	if _, err := d.Challenge(t.Context(), live.ID); err != nil {
		t.Fatalf("未过期的 issued challenge 不该被清理: %v", err)
	}
	if _, err := d.Challenge(t.Context(), dead.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("作废且过期的 challenge 应被清理，got %v", err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, s.ID, time.Now()); !errors.Is(err, ErrConsumed) {
		t.Fatal("清理后重放必须失败")
	}
}

// TestPurgeKeepsUnconsumedResponse 覆盖一条真实时间线：challenge 在被兑换时已接近过期，
// 其签发的 response TTL 长于 challenge 剩余寿命。此时清理任务若先删 challenge，
// 要么被外键挡下（Purge 直接失败），要么让仍然有效的 response 无法完成验证。
// 期望：清理不报错，且未消费的 response 之后仍可成功验证。
func TestPurgeKeepsUnconsumedResponse(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "alpha")

	base := time.Now().UTC()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	c := &Challenge{ID: id, SiteID: s.ID, PowSeed: []byte("0123456789abcdef"), PowBits: 12,
		CreatedAt: base, ExpiresAt: base.Add(10 * time.Second)}
	if err := d.InsertChallenge(t.Context(), c, testCaps); err != nil {
		t.Fatal(err)
	}
	r, err := d.Redeem(t.Context(), id, base.Add(5*time.Second), time.Hour)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	// now 落在 challenge 已过期、response 仍有效的窗口内
	if _, err := d.Purge(t.Context(), base.Add(time.Minute)); err != nil {
		t.Fatalf("Purge 在 response 仍有效时失败: %v", err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, s.ID, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("清理后仍然有效的 response 验证失败: %v", err)
	}
}

// TestActiveChallengeCapBoundsLedger 验证签发侧的存量封顶：活动（issued 且未过期）行数
// 达到上限后写入被拒，而过期行不再占用额度、作废行不计入。
// 这条上限是"challenge 签发不能无限扩大 SQLite"的保证——限流器只控制速率，不控制存量。
func TestActiveChallengeCapBoundsLedger(t *testing.T) {
	d := newDB(t)
	a := mustSite(t, d, "alpha")
	b := mustSite(t, d, "bravo")

	caps := Caps{MaxActive: 3, MaxActivePerSite: 2}
	for range 2 {
		newChallengeCaps(t, d, a.ID, time.Hour, caps)
	}
	if err := d.InsertChallenge(t.Context(), &Challenge{
		ID: "over-site", SiteID: a.ID, PowSeed: []byte("0123456789abcdef"), PowBits: 12,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}, caps); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("单站活动量超上限应 ErrCapExceeded，got %v", err)
	}
	// 另一个站点还能签 1 条，签满全局 3 条后同样被拒
	newChallengeCaps(t, d, b.ID, time.Hour, caps)
	if err := d.InsertChallenge(t.Context(), &Challenge{
		ID: "over-global", SiteID: b.ID, PowSeed: []byte("0123456789abcdef"), PowBits: 12,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}, caps); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("全局活动量超上限应 ErrCapExceeded，got %v", err)
	}

	// 过期与作废的行不占额度：把上面 2 条 alpha challenge 置为已过期后即可继续签发
	if _, err := d.h.ExecContext(t.Context(),
		`UPDATE challenges SET expires_at=? WHERE site_id=? AND expires_at>?`,
		formatTime(time.Now().Add(-time.Minute).UTC()), a.ID, formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	ch := &Challenge{ID: "after-expire", SiteID: a.ID, PowSeed: []byte("0123456789abcdef"), PowBits: 12,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC()}
	if err := d.InsertChallenge(t.Context(), ch, caps); err != nil {
		t.Fatalf("过期行不应继续占用额度: %v", err)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	d := newDB(t)
	_, err := d.h.ExecContext(t.Context(), `
INSERT INTO challenges(id, site_id, pow_seed, pow_bits, status, attempts, created_at, expires_at)
VALUES('orphan', 99999, x'00', 12, 'issued', 0, '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')`)
	if err == nil {
		t.Fatal("foreign_keys 未生效：悬挂引用被接受")
	}
}

func TestCheckConstraints(t *testing.T) {
	d := newDB(t)
	site := mustSite(t, d, "alpha")
	const (
		created = "2026-01-01T00:00:00Z"
		expires = "2026-01-02T00:00:00Z"
	)
	insertChallenge := `INSERT INTO challenges(id, site_id, pow_seed, pow_bits, status, attempts, created_at, expires_at)
		VALUES(?,?,?,?,?,?,?,?)`
	cases := []struct {
		name string
		args []any
	}{
		{"bits 越界", []any{"c1", site.ID, []byte{0}, 99, statusIssued, 0, created, expires}},
		{"未知状态", []any{"c2", site.ID, []byte{0}, 12, "bogus", 0, created, expires}},
		{"attempts 越界", []any{"c3", site.ID, []byte{0}, 12, statusIssued, 99, created, expires}},
	}
	for _, c := range cases {
		if _, err := d.h.ExecContext(t.Context(), insertChallenge, c.args...); err == nil {
			t.Errorf("CHECK 约束未拦住：%s", c.name)
		}
	}
	if _, err := d.h.ExecContext(t.Context(),
		`INSERT INTO responses(id, challenge_id, site_id, created_at, expires_at, used) VALUES(?,?,?,?,?,?)`,
		"r1", "no-such-challenge", site.ID, created, expires, 7); err == nil {
		t.Error("responses.used 非法值或悬挂引用应被拒绝")
	}
}

// readFile 用于按值扫描 DB 文件（路径始终来自 t.TempDir()，不含外部输入）。
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// TestPurgeFreesPagesNotFileShrink 把一条容易被误解的事实钉在测试里：
// 清理删除的是**行**，SQLite 只把页放进空闲链供后续复用，文件不会因此自动变小。
// 声称"删除行 ⇒ 数据库文件缩小"是错的；真正收缩需要 VACUUM——本专案不在清理任务里做它，
// 那会让单写连接长时间独占，而 50GB 磁盘预算下复用空闲页已经足够。
func TestPurgeFreesPagesNotFileShrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	d, err := Open(t.Context(), path, testPepper())
	if err != nil {
		t.Fatal(err)
	}
	s := mustSite(t, d, "alpha")
	for range 400 {
		c := newChallenge(t, d, s.ID, -time.Minute)
		if _, err := d.h.ExecContext(t.Context(),
			`UPDATE challenges SET status=? WHERE id=?`, statusRedeemed, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		if _, err := d.Purge(t.Context(), time.Now()); err != nil {
			t.Fatalf("第 %d 轮清理失败: %v", i, err)
		}
	}
	if n, err := d.CountChallenges(t.Context(), statusRedeemed); err != nil || n != 0 {
		t.Fatalf("清理后仍剩 %d 条 redeemed challenge (err %v)", n, err)
	}
	if _, err := d.h.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	emptied, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	fresh := filepath.Join(t.TempDir(), "fresh.db")
	fd, err := Open(t.Context(), fresh, testPepper())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fd.h.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	fs, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	// 断言的是"没有缩小回空库"：这正是"删行不等于缩文件"的表现。
	if emptied.Size() <= fs.Size() {
		t.Fatalf("删空账本后文件缩回了空库大小（%d ≤ %d）：说明实现里发生了本不该有的收缩/重建",
			emptied.Size(), fs.Size())
	}
	t.Logf("行已删空：文件 %d 字节，空库 %d 字节——差异是待复用的空闲页", emptied.Size(), fs.Size())
}
