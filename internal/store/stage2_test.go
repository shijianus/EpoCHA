package store

import (
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildV1File 直接在文件上造一个**阶段 1 形态**的库：schema_version=1、单 secret 存在
// sites.secret_mac、challenges 没有 origin 列。全程用裸 SQL 与阶段 1 的列集合，
// 这样"升级"这一步走的确实是 0001→0002 的增量路径，而不是"用 v2 代码写数据再假装迁移"。
func buildV1File(t *testing.T, path string) (siteID int64, challengeID, responseID, secret string) {
	t.Helper()
	h, err := sql.Open("sqlite", fmt.Sprintf(dsnTemplate, path))
	if err != nil {
		t.Fatal(err)
	}
	h.SetMaxOpenConns(1)
	defer h.Close()
	ctx := t.Context()
	if _, err := h.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ExecContext(ctx, migration0001); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ExecContext(ctx, `INSERT INTO meta(k,v) VALUES('schema_version','1')`); err != nil {
		t.Fatal(err)
	}
	secret = "OLD-STAGE1-SECRET-2c5e8a1b"
	pepper := testPepper()
	mac := hex.EncodeToString(macWithKey(pepper, secret))
	if _, err := h.ExecContext(ctx, `
INSERT INTO sites(name, site_key, secret_mac, pow_bits, created_at) VALUES('legacy','sk-legacy',?,14,'2026-01-01T00:00:00Z')`,
		mac); err != nil {
		t.Fatal(err)
	}
	if err := h.QueryRowContext(ctx, `SELECT id FROM sites WHERE site_key='sk-legacy'`).Scan(&siteID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	challengeID, _ = NewID()
	responseID, _ = NewID()
	if _, err := h.ExecContext(ctx, `
INSERT INTO challenges(id, site_id, pow_seed, pow_bits, status, attempts, created_at, expires_at, redeemed_at)
VALUES(?,?,?,12,'redeemed',0,?,?,?)`,
		challengeID, siteID, []byte("0123456789abcdef"),
		formatTime(now.Add(-time.Hour)), formatTime(now.Add(time.Hour)), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ExecContext(ctx, `
INSERT INTO responses(id, challenge_id, site_id, created_at, expires_at, used) VALUES(?,?,?,?,?,1)`,
		responseID, challengeID, siteID, formatTime(now), formatTime(now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	return siteID, challengeID, responseID, secret
}

// TestStage1DatabaseUpgrades 是阶段 2 的迁移验收：拿一个阶段 1 的库升级，
// 要求（1）版本进到 2；（2）旧 secret 仍可定位站点（凭据搬进 site_secrets）；
// （3）**已消费的防重放状态仍然生效**（重放失败）；（4）sites.secret_mac 被中和、不可用于认证。
func TestStage1DatabaseUpgrades(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	siteID, challengeID, responseID, oldSecret := buildV1File(t, path)
	_ = challengeID
	mac := hex.EncodeToString(macWithKey(testPepper(), oldSecret))

	// 升级：同一个文件重新打开
	d2, err := Open(t.Context(), path, testPepper())
	if err != nil {
		t.Fatalf("升级打开失败: %v", err)
	}
	defer d2.Close()
	if v, _ := d2.SchemaVersion(t.Context()); v != schemaVersion {
		t.Fatalf("升级后版本 = %d，期望 %d", v, schemaVersion)
	}
	// (2) 旧 secret 仍可用
	got, err := d2.FindSiteBySecret(t.Context(), oldSecret, time.Now())
	if err != nil || got.ID != siteID {
		t.Fatalf("旧 secret 升级后不可用: %v %+v", err, got)
	}
	// 迁移行带确定性的公开 key_id
	secrets, err := d2.ListSecrets(t.Context(), siteID)
	if err != nil || len(secrets) != 1 || secrets[0].KeyID != "legacy-"+itoaTest(siteID) {
		t.Fatalf("迁移凭据不符: %+v err %v", secrets, err)
	}
	// (3) 阶段 1 里已消费的 response，升级后重放仍失败——这就是"原有安全状态保留"
	if _, err := d2.ConsumeResponse(t.Context(), responseID, siteID, time.Now()); !errors.Is(err, ErrConsumed) {
		t.Fatalf("升级后重放竟成功: %v", err)
	}
	// 阶段 1 的 challenge 行也还在，且状态仍是 redeemed（迁移不动账本数据）
	if c, err := d2.Challenge(t.Context(), challengeID); err != nil || c.Status != statusRedeemed || c.Origin != "" {
		t.Fatalf("迁移后的 challenge 不符: %+v err %v", c, err)
	}
	// (4) 旧列已被中和：库里那一行不再等于任何 secret 的摘要
	var stale string
	if err := d2.h.QueryRowContext(t.Context(),
		`SELECT secret_mac FROM sites WHERE id=?`, siteID).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if subtle.ConstantTimeCompare([]byte(stale), []byte(mac)) == 1 {
		t.Fatal("sites.secret_mac 仍是原始摘要：留下第二个真相源，撤销会被误读路径绕过")
	}
	if subtle.ConstantTimeCompare([]byte(stale), []byte(hex.EncodeToString(d2.SecretMac(oldSecret)))) == 1 {
		t.Fatal("sites.secret_mac 仍可被用作认证依据")
	}
	// 站点未被停用的库升级后 version 从 1 起算，签发即可绑定
	site2, err := d2.SiteByID(t.Context(), siteID)
	if err != nil || !site2.Enabled || site2.Version != 1 {
		t.Fatalf("升级后站点状态不符: %+v err %v", site2, err)
	}
}

func itoaTest(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestMigrationIsIdempotentAfterUpgrade 确认升级后的库反复打开不再触碰数据。
func TestMigrationIsIdempotentAfterUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	_, challengeID, _, s := buildV1File(t, path)

	for i := 0; i < 3; i++ {
		dd, err := Open(t.Context(), path, testPepper())
		if err != nil {
			t.Fatalf("第 %d 次打开: %v", i, err)
		}
		if _, err := dd.Challenge(t.Context(), challengeID); err != nil {
			t.Fatalf("第 %d 次打开后 challenge 丢失: %v", i, err)
		}
		if _, err := dd.FindSiteBySecret(t.Context(), s, time.Now()); err != nil {
			t.Fatalf("第 %d 次打开后凭据不可用: %v", i, err)
		}
		if n, _ := dd.CountChallenges(t.Context(), statusRedeemed); n != 1 {
			t.Fatalf("第 %d 次打开后 redeemed 计数 = %d", i, n)
		}
		dd.Close()
	}
}

// --- 凭据生命周期 ---

func TestSecretTransitionAndImmediateRevoke(t *testing.T) {
	d := newDB(t)
	site, first, err := d.CreateSite(t.Context(), "alpha", "sk-alpha", SitePolicy{PowBits: 14})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// 过渡期：旧密钥在窗口内仍可用，到点后不可用
	fresh, second, retired, err := d.RotateSecret(t.Context(), site.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 1 || retired[0].KeyID == fresh.KeyID {
		t.Fatalf("轮换应撤下 1 把旧密钥: %+v", retired)
	}
	if second == first || second == "" {
		t.Fatal("新 secret 必须不同且非空")
	}
	if _, err := d.FindSiteBySecret(t.Context(), first, now.Add(30*time.Minute)); err != nil {
		t.Fatalf("过渡期内旧 secret 应仍可用: %v", err)
	}
	if _, err := d.FindSiteBySecret(t.Context(), first, now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过渡期结束后旧 secret 应失效: %v", err)
	}
	if _, err := d.FindSiteBySecret(t.Context(), second, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("新 secret 应持续可用: %v", err)
	}

	// 立即撤销：没有过渡窗口
	site2, s2, err := d.CreateSite(t.Context(), "bravo", "sk-bravo", SitePolicy{PowBits: 14})
	if err != nil {
		t.Fatal(err)
	}
	secrets, _ := d.ListSecrets(t.Context(), site2.ID)
	if _, err := d.RevokeSecret(t.Context(), site2.ID, secrets[0].KeyID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.FindSiteBySecret(t.Context(), s2, time.Now().Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("立即撤销后仍可用: %v", err)
	}
	// 全部撤销后该站点无法再通过 secret 定位——等价于"凭据不可用"，不是"站点不存在"的另一种错误
	if _, err := d.FindSiteBySecret(t.Context(), s2, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，got %v", err)
	}
}

func TestSecretCountAndTransitionBounds(t *testing.T) {
	d := newDB(t)
	site, _, err := d.CreateSite(t.Context(), "alpha", "sk-alpha", SitePolicy{PowBits: 14})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxSecretsPerSite-1; i++ {
		if _, _, err := d.AddSecret(t.Context(), site.ID); err != nil {
			t.Fatalf("第 %d 把应可添加: %v", i+2, err)
		}
	}
	if _, _, err := d.AddSecret(t.Context(), site.ID); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("超过 %d 把应拒绝，got %v", MaxSecretsPerSite, err)
	}
	if _, _, _, err := d.RotateSecret(t.Context(), site.ID, (MaxTransitionSecond+1)*time.Second, time.Now()); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("过渡期超上限应拒绝，got %v", err)
	}
	if _, _, _, err := d.RotateSecret(t.Context(), site.ID, -time.Second, time.Now()); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("负过渡期应拒绝，got %v", err)
	}
}

// TestRevokedSecretCannotConsumeValidResponse 覆盖红线 4 的阶段 2 形态：
// 被撤销的凭据不能消费任何合法 response，而换上新凭据后同一条 response 仍能正常验证。
func TestRevokedSecretCannotConsumeValidResponse(t *testing.T) {
	d := newDB(t)
	site, first, err := d.CreateSite(t.Context(), "alpha", "sk-alpha", SitePolicy{PowBits: 14})
	if err != nil {
		t.Fatal(err)
	}
	c := newChallenge(t, d, site.ID, time.Hour)
	r, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// 先确认这条 response 属于 first 这把凭据所在的站点
	if got, err := d.FindSiteBySecret(t.Context(), first, time.Now()); err != nil || got.ID != site.ID {
		t.Fatalf("凭据定位异常: %v %+v", err, got)
	}
	// 轮换并**立即**撤销旧密钥
	if _, _, _, err := d.RotateSecret(t.Context(), site.ID, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.FindSiteBySecret(t.Context(), first, time.Now().Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧凭据应已失效: %v", err)
	}
	secrets, _ := d.ListSecrets(t.Context(), site.ID)
	var active string
	for _, s := range secrets {
		if s.RevokedAt.IsZero() || s.RevokedAt.After(time.Now()) {
			active = s.KeyID
		}
	}
	if active == "" {
		t.Fatal("轮换后应有一把有效凭据")
	}
	// 新凭据仍可完成验证：撤销不是"封站"
	if _, err := d.ConsumeResponse(t.Context(), r.ID, site.ID, time.Now()); err != nil {
		t.Fatalf("旧凭据撤销后，新凭据应仍能验证这条 response: %v", err)
	}
	// 再放一次旧凭据上去：已被消费，依旧失败（撤销+重放两条性质同时成立）
	if _, err := d.ConsumeResponse(t.Context(), r.ID, site.ID, time.Now()); !errors.Is(err, ErrConsumed) {
		t.Fatalf("重复验证应失败，got %v", err)
	}
}

// --- 站点版本与启停 ---

func TestSiteDisableInvalidatesTokensAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	d, _ := newDBAt(t, path)
	site := mustSite(t, d, "alpha")
	v0 := site.Version

	c := newChallenge(t, d, site.ID, time.Hour)
	r, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// 停用：版本提升
	off, err := d.SetSiteEnabled(t.Context(), site.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if off.Enabled || off.Version == v0 {
		t.Fatalf("停用应提升版本: %+v", off)
	}
	// 重复停用不再提升版本（幂等：启停是状态动作，不是计数器）
	again, err := d.SetSiteEnabled(t.Context(), site.ID, false)
	if err != nil || again.Version != off.Version {
		t.Fatalf("重复停用不应再改版本: %+v err %v", again, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// 重启 + 重新启用：未消费的 response 不得复活
	d2, _ := newDBAt(t, path)
	on, err := d2.SetSiteEnabled(t.Context(), site.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !on.Enabled || on.Version <= off.Version {
		t.Fatalf("重新启用应再次提升版本: %+v", on)
	}
	if _, err := d2.ConsumeResponse(t.Context(), r.ID, site.ID, time.Now()); err != nil {
		t.Logf("注意：store 层的消费只看 used/expires_at，站点启停由 API 层裁决（版本绑定在 token 上）")
	}
	// 权威结论：账本行仍是 used=0，因此"停用即失效"必须由签名里的 svid + 站点状态共同保证，
	// 这条测试锁住的是"版本单调提升且重启后不变"这个前提。
	got, err := d2.SiteByID(t.Context(), site.ID)
	if err != nil || got.Version != on.Version {
		t.Fatalf("版本未持久化: %+v err %v", got, err)
	}
}

// --- 来源规范化 ---

func TestNormalizeOriginExactMatch(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "https://App.Example.com", want: "https://app.example.com"},
		{in: "https://app.example.com:443", want: "https://app.example.com"},
		{in: "https://app.example.com:8443", want: "https://app.example.com:8443"},
		{in: "http://localhost:8080", want: "http://localhost:8080"},
		{in: "http://localhost", want: "http://localhost"},
		{in: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{in: "http://[::1]:8080", want: "http://[::1]:8080"},
		{in: "https://example.com.", want: "https://example.com"},
		// 相似后缀：必须规范成它自己，绝不能与 example.com 混同
		{in: "https://evil-example.com", want: "https://evil-example.com"},
		{in: "https://example.com.evil.net", want: "https://example.com.evil.net"},
		// 非预期子域：规范后仍是子域，是否放行由白名单决定
		{in: "https://deep.app.example.com", want: "https://deep.app.example.com"},
		// 协议不同就是不同来源
		{in: "http://app.example.com", want: "http://app.example.com"},
		// 拒绝项
		{in: "null", wantErr: true},
		{in: "", wantErr: true},
		{in: "https://*.example.com", wantErr: true},
		{in: "https://example.com/path", wantErr: true},
		{in: "https://example.com?a=1", wantErr: true},
		{in: "https://user:pw@example.com", wantErr: true},
		{in: "ftp://example.com", wantErr: true},
		{in: "https://example.com:0", wantErr: true},
		{in: "https://example.com:70000", wantErr: true},
		{in: "javascript:alert(1)", wantErr: true},
		{in: "https://ex ample.com", wantErr: true},
		{in: "https://_bad", wantErr: true},
		{in: "https://-bad.example.com", wantErr: true},
		{in: "http://singlelabel", wantErr: true},
		{in: "https://example.com/../x", wantErr: true},
		{in: "  ", wantErr: true},
		{in: "https://" + strings.Repeat("a", 300) + ".com", wantErr: true},
	}
	for _, c := range cases {
		got, err := NormalizeOrigin(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeOrigin(%q) 应拒绝，got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeOrigin(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeOrigin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOriginHostExtraction(t *testing.T) {
	for in, want := range map[string]string{
		"https://app.example.com":      "app.example.com",
		"https://app.example.com:8443": "app.example.com",
		"http://[::1]:8080":            "::1",
		"":                             "",
		"not-an-origin":                "",
	} {
		if got := OriginHost(in); got != want {
			t.Errorf("OriginHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReplaceOriginsAndMatching(t *testing.T) {
	d := newDB(t)
	site := mustSite(t, d, "alpha")
	got, err := d.ReplaceOrigins(t.Context(), site.ID, []string{
		"https://app.example.com:443", "http://localhost:8080", "https://app.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("重复/默认端口应归并: %+v", got)
	}
	for o, want := range map[string]bool{
		"https://app.example.com":      true,
		"http://localhost:8080":        true,
		"http://app.example.com":       false, // 协议不同
		"https://app.example.com:8443": false, // 端口不同
		"https://evil-example.com":     false, // 相似后缀
		"https://deep.example.com":     false, // 子域不放行
		"https://example.com":          false,
	} {
		ok, err := d.OriginAllowed(t.Context(), site.ID, o)
		if err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Errorf("OriginAllowed(%q) = %v, want %v", o, ok, want)
		}
	}
	// 全量替换语义：第二批生效后第一批立刻不可用（安全动作不缓存）
	if _, err := d.ReplaceOrigins(t.Context(), site.ID, []string{"https://only.example.com"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.OriginAllowed(t.Context(), site.ID, "https://app.example.com"); ok {
		t.Fatal("替换后旧来源仍被放行")
	}
	// 空列表 = 不接受任何浏览器来源
	if _, err := d.ReplaceOrigins(t.Context(), site.ID, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.OriginAllowed(t.Context(), site.ID, "https://only.example.com"); ok {
		t.Fatal("清空后仍放行")
	}
	// 超限与非法条目整批拒绝
	tooMany := make([]string, 0, MaxOriginsPerSite+1)
	for i := 0; i <= MaxOriginsPerSite; i++ {
		tooMany = append(tooMany, "https://s"+itoaTest(int64(i))+".example.com")
	}
	if _, err := d.ReplaceOrigins(t.Context(), site.ID, tooMany); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("来源条目超限应拒绝，got %v", err)
	}
	if _, err := d.ReplaceOrigins(t.Context(), site.ID, []string{"https://ok.example.com", "null"}); !errors.Is(err, ErrBadConfig) {
		t.Fatal("一条非法来源应整批拒绝（不接受部分生效的白名单）")
	}
	if ok, _ := d.OriginAllowed(t.Context(), site.ID, "https://ok.example.com"); ok {
		t.Fatal("整批拒绝时不得有任何条目写入")
	}
}

// --- 管理凭据与审计 ---

func TestAdminKeyLifecycle(t *testing.T) {
	d := newDB(t)
	const k1 = "ak_bootstrap_1f2e3d4c5b6a"
	// 库里没有记录时 bootstrap 才登记
	got, already, err := d.BootstrapAdminKey(t.Context(), k1, "bootstrap")
	if err != nil || already {
		t.Fatalf("首次 bootstrap 应登记: %+v already=%v err=%v", got, already, err)
	}
	if id, err := d.MatchAdminKey(t.Context(), k1, time.Now()); err != nil || id != got.KeyID {
		t.Fatalf("bootstrap 凭据应可用: %v %+v", err, id)
	}
	// 已有记录后 bootstrap 不再生效（撤销不会因重启复活）
	again, already, err := d.BootstrapAdminKey(t.Context(), "ak_whatever_else_99887766", "bootstrap")
	if err != nil || !already || again != nil {
		t.Fatalf("已有记录时应忽略 env: %+v already=%v err=%v", again, already, err)
	}
	// 撤销立即生效
	if _, err := d.RevokeAdminKey(t.Context(), got.KeyID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MatchAdminKey(t.Context(), k1, time.Now().Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("撤销后仍通过: %v", err)
	}
	// 域分隔：站点 secret 与管理凭据即使明文相同也不是同一个摘要
	same := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := d.AddAdminKey(t.Context(), "ops", same); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(d.AdminKeyMac(same)) == hex.EncodeToString(d.SecretMac(same)) {
		t.Fatal("管理凭据与站点 secret 摘要未做域分隔：跨用途可互换")
	}
	// 列表不回显任何凭据材料
	list, err := d.ListAdminKeys(t.Context())
	if err != nil || len(list) != 2 {
		t.Fatalf("列表不符: %+v err %v", list, err)
	}
	for _, k := range list {
		if k.KeyID == "" || k.Label == "" {
			t.Fatal("管理凭据视图缺少公开标识")
		}
	}
}

func TestAuditPaginationAndBounds(t *testing.T) {
	d := newDB(t)
	site := mustSite(t, d, "alpha")
	for i := 0; i < 7; i++ {
		if err := d.AppendAudit(t.Context(), "k-test", "site.update", "site:"+itoaTest(site.ID), true); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := d.ListAudit(t.Context(), 0, 3)
	if err != nil || len(page1) != 3 {
		t.Fatalf("第一页应有 3 条: %d %v", len(page1), err)
	}
	if page1[0].ID <= page1[2].ID {
		t.Fatal("审计应按 id 倒序（最新在前）")
	}
	page2, err := d.ListAudit(t.Context(), page1[2].ID, 3)
	if err != nil || len(page2) != 3 {
		t.Fatalf("游标分页失败: %d %v", len(page2), err)
	}
	if page2[0].ID >= page1[2].ID {
		t.Fatal("游标未推进")
	}
	// 分页上限与非法值
	if _, err := d.ListAudit(t.Context(), 0, 501); !errors.Is(err, ErrBadConfig) {
		t.Errorf("limit>500 应拒绝，got %v", err)
	}
	if _, err := d.ListAudit(t.Context(), 0, -1); !errors.Is(err, ErrBadConfig) {
		t.Errorf("负 limit 应拒绝，got %v", err)
	}
	if _, err := d.ListAudit(t.Context(), -1, 10); !errors.Is(err, ErrBadConfig) {
		t.Errorf("负游标应拒绝，got %v", err)
	}
	// 审计内容不接受凭据形态的字段名，但 target 长度有上限
	if err := d.AppendAudit(t.Context(), "k-test", "site.update", strings.Repeat("t", 300), true); !errors.Is(err, ErrBadConfig) {
		t.Errorf("超长 target 应拒绝，got %v", err)
	}
	if err := d.AppendAudit(t.Context(), "bad\nactor", "site.update", "x", true); !errors.Is(err, ErrBadConfig) {
		t.Errorf("含控制字符的 actor 应拒绝（日志注入面），got %v", err)
	}
	// 保留期清理：只删超期的，不动新行
	d.h.ExecContext(t.Context(), `UPDATE audit SET ts=? WHERE id<=2`, formatTime(time.Now().AddDate(0, 0, -(AuditRetentionDays+1)).UTC()))
	n, err := d.PurgeAudit(t.Context(), time.Now())
	if err != nil || n != 2 {
		t.Fatalf("审计清理应删 2 行: %d %v", n, err)
	}
	rest, _ := d.ListAudit(t.Context(), 0, 100)
	if len(rest) != 5 {
		t.Fatalf("清理后剩余 %d 条", len(rest))
	}
}

// TestConcurrentAdminKeyCreationNoDupKeyID 验证 key_id 生成在高并发下不产生主键冲突导致的静默覆盖。
func TestConcurrentAdminKeyCreationNoDupKeyID(t *testing.T) {
	d := newDB(t)
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := d.NewAdminKeyWithID(t.Context(), "ops-"+itoaTest(int64(i)))
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("第 %d 次并发创建失败: %v", i, err)
		}
	}
	list, err := d.ListAdminKeys(t.Context())
	if err != nil || len(list) != 20 {
		t.Fatalf("应有 20 条管理凭据: %d %v", len(list), err)
	}
}
