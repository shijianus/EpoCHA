package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	sentinelSecret = "SENTINEL-SITE-SECRET-8f3c1a"
	sentinelKey    = "sentinel-master-key-material-0123456789"
	sentinelPepper = "sentinel-pepper-material-01234567890123"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func writeSites(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sites.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func validSites(t *testing.T) string {
	t.Helper()
	return writeSites(t, fmt.Sprintf(
		`[{"name":"demo","site_key":"sk-demo","secret":"%s","pow_bits":14}]`, sentinelSecret))
}

// envMap 返回 getenv 函数；未设置的键返回空串，与 os.Getenv 行为一致。
func envMap(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func baseEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		envMasterKey: b64(sentinelKey),
		envPepper:    b64(sentinelPepper),
		envDB:        filepath.Join(t.TempDir(), "epocha.db"),
		envSites:     validSites(t),
	}
}

func TestLoadValid(t *testing.T) {
	env := baseEnv(t)
	c, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != "127.0.0.1:8080" {
		t.Errorf("默认监听地址 = %q", c.Listen)
	}
	if len(c.MasterKey) < 32 || len(c.Pepper) < 32 {
		t.Error("密钥长度不足")
	}
	if c.ChallengeTTL != 180*time.Second || c.ResponseTTL != 120*time.Second {
		t.Errorf("默认 TTL 不符: %v / %v", c.ChallengeTTL, c.ResponseTTL)
	}
	if len(c.Sites) != 1 || c.Sites[0].SiteKey != "sk-demo" || c.Sites[0].PowBits != 14 {
		t.Fatalf("站点装载不符 %+v", c.Sites)
	}
	if c.Sites[0].Secret != sentinelSecret {
		t.Error("站点 secret 未装载")
	}
	if c.RateIPRPM != 60 || c.RateSiteRPM != 300 || c.RateMaxState != 8192 {
		t.Errorf("默认限流参数不符: %+v", c)
	}
}

func TestMissingKeysRefuseToStart(t *testing.T) {
	cases := []struct {
		name string
		drop string
		want error
	}{
		{"缺主密钥", envMasterKey, ErrMissingMasterKey},
		{"缺 pepper", envPepper, ErrMissingPepper},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv(t)
			delete(env, tc.drop)
			_, err := Load(nil, envMap(env))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), sentinelPepper) || strings.Contains(err.Error(), sentinelKey) {
				t.Fatalf("错误文案泄露密钥：%v", err)
			}
		})
	}
}

func TestKeyMaterialValidation(t *testing.T) {
	cases := map[string]func(map[string]string){
		"非法 base64": func(e map[string]string) { e[envMasterKey] = "not base64 !!!" },
		"过短密钥":      func(e map[string]string) { e[envMasterKey] = b64("tiny") },
		"过短 pepper": func(e map[string]string) { e[envPepper] = b64("tiny") },
		"空字符串密钥":    func(e map[string]string) { e[envMasterKey] = "   " },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			env := baseEnv(t)
			mutate(env)
			if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("应拒绝，got %v", err)
			}
		})
	}
}

func TestErrorMessagesDoNotLeakSecrets(t *testing.T) {
	// pow_bits 越界会让加载失败；错误里绝不能带上站点 secret 或密钥材料
	env := baseEnv(t)
	env[envSites] = writeSites(t, fmt.Sprintf(
		`[{"name":"bad","site_key":"sk-bad","secret":"%s","pow_bits":99}]`, sentinelSecret))
	_, err := Load(nil, envMap(env))
	if err == nil {
		t.Fatal("越界 pow_bits 应拒绝")
	}
	for _, leak := range []string{sentinelSecret, sentinelKey, sentinelPepper} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误泄露 %q：%v", leak, err)
		}
	}
	// SafeSummary 同样不得含密钥
	env2 := baseEnv(t)
	c, err := Load(nil, envMap(env2))
	if err != nil {
		t.Fatal(err)
	}
	sum := c.SafeSummary()
	for _, leak := range []string{sentinelSecret, sentinelKey, sentinelPepper} {
		if strings.Contains(sum, leak) {
			t.Fatalf("SafeSummary 泄露 %q：%s", leak, sum)
		}
	}
	if !strings.Contains(sum, "site_imports=") || !strings.Contains(sum, "admin_listen=") {
		t.Fatalf("SafeSummary 缺少站点导入数或管理监听地址：%s", sum)
	}
	if strings.Contains(sum, "admin_key_present=true") {
		t.Fatal("SafeSummary 不该带出凭据内容")
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	env := baseEnv(t)
	env[envListen] = "0.0.0.0:1"
	env[envDB] = "/tmp/from-env.db"
	c, err := Load([]string{"--listen", "127.0.0.1:9999", "--db", "/tmp/from-flag.db"}, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9999" || c.DBPath != "/tmp/from-flag.db" {
		t.Fatalf("flag 应覆盖 env：%+v", c)
	}
	// sites flag 覆盖 env
	flagSites := writeSites(t, `[{"name":"a","site_key":"k-a","secret":"aaaaaaaaaaaaaaaaaaaa"}]`)
	c2, err := Load([]string{"--sites", flagSites}, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if c2.Sites[0].Name != "a" || c2.Sites[0].PowBits != 18 {
		t.Fatalf("--sites 未生效：%+v", c2.Sites)
	}
}

func TestMissingDBAndSites(t *testing.T) {
	env := baseEnv(t)
	delete(env, envDB)
	if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
		t.Error("缺 DB 路径应拒绝")
	}
	// 阶段 2：站点配置只是可选的一次性导入入口，SQLite 才是权威数据源，
	// 所以"没有配置文件"必须能正常启动（站点全从库里读）。
	env = baseEnv(t)
	delete(env, envSites)
	c, err := Load(nil, envMap(env))
	if err != nil {
		t.Errorf("缺站点配置不应拒绝：%v", err)
	} else if len(c.Sites) != 0 {
		t.Errorf("无配置文件时站点导入列表应为空：%+v", c.Sites)
	}
	// 但**指定了**文件却读不到，仍然是配置错误（静默忽略会让运维以为导入发生了）
	if _, err := Load([]string{"--sites", filepath.Join(t.TempDir(), "nope.json")}, envMap(baseEnv(t))); !errors.Is(err, ErrInvalid) {
		t.Error("显式指定的站点文件不存在应拒绝")
	}
	if _, err := Load([]string{"--unknown-flag", "1"}, envMap(baseEnv(t))); !errors.Is(err, ErrInvalid) {
		t.Error("未知 flag 应拒绝")
	}
}

func TestSitesValidation(t *testing.T) {
	cases := map[string]string{
		"空数组":         `[]`,
		"非数组":         `{"name":"x"}`,
		"未知字段":        `[{"name":"a","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa","extra":1}]`,
		"坏 JSON":      `[{"name":`,
		"缺 secret":    `[{"name":"a","site_key":"k"}]`,
		"secret 过短":   `[{"name":"a","site_key":"k","secret":"short"}]`,
		"site_key 重复": `[{"name":"a","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa"},{"name":"b","site_key":"k","secret":"bbbbbbbbbbbbbbbbbbbb"}]`,
		"站点名重复":       `[{"name":"a","site_key":"k1","secret":"aaaaaaaaaaaaaaaaaaaa"},{"name":"a","site_key":"k2","secret":"bbbbbbbbbbbbbbbbbbbb"}]`,
		"pow_bits 低":  `[{"name":"a","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa","pow_bits":11}]`,
		"pow_bits 高":  `[{"name":"a","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa","pow_bits":25}]`,
		"name 空":      `[{"name":"","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa"}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			env := baseEnv(t)
			env[envSites] = writeSites(t, body)
			if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("应拒绝，got %v", err)
			}
		})
	}
}

func TestSitesFileOversize(t *testing.T) {
	env := baseEnv(t)
	pad := strings.Repeat("x", sitesFileMaxBytes+10)
	env[envSites] = writeSites(t, fmt.Sprintf(`[{"name":"%s","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa"}]`, pad))
	if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
		t.Fatal("超大站点文件应拒绝")
	}
}

func TestTooManySites(t *testing.T) {
	env := baseEnv(t)
	var b strings.Builder
	b.WriteString("[")
	for i := range maxSites + 1 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":"n%d","site_key":"k%d","secret":"aaaaaaaaaaaaaaaaaaaa"}`, i, i)
	}
	b.WriteString("]")
	env[envSites] = writeSites(t, b.String())
	if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
		t.Fatal("站点数超上限应拒绝")
	}
}

func TestTTLAndRateParsing(t *testing.T) {
	env := baseEnv(t)
	env[envChalTTL] = "60"
	env[envRespTTL] = "30"
	env[envRateIP] = "10"
	env[envRateSite] = "50"
	env[envRateStates] = "256"
	c, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.ChallengeTTL != 60*time.Second || c.ResponseTTL != 30*time.Second {
		t.Errorf("TTL 未生效: %v / %v", c.ChallengeTTL, c.ResponseTTL)
	}
	if c.RateIPRPM != 10 || c.RateSiteRPM != 50 || c.RateMaxState != 256 {
		t.Errorf("限流参数未生效: %+v", c)
	}

	for _, bad := range []string{"abc", "-5", "1.5"} {
		env[envChalTTL] = bad
		if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
			t.Errorf("TTL=%q 应拒绝，got %v", bad, err)
		}
	}
	env[envChalTTL] = "0"
	if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
		t.Error("TTL=0 应拒绝（下限 1s）")
	}
	env = baseEnv(t)
	env[envRateIP] = "-1"
	if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
		t.Error("负 rpm 应拒绝")
	}
}

func TestTrustProxyParsing(t *testing.T) {
	env := baseEnv(t)
	env[envTrustProxy] = "true"
	env[envTrustedNets] = "198.51.100.0/24, 203.0.113.0/24"
	c, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if !c.TrustProxy || len(c.TrustedProxies) != 2 {
		t.Fatalf("代理配置未生效: %+v", c)
	}
	env[envTrustProxy] = "yes"
	if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
		t.Error("非法布尔值应拒绝")
	}
	// trust_proxy=true 但未给网段：由 risk.NewSource 负责拒绝，config 只如实传递
	env[envTrustProxy] = "1"
	env[envTrustedNets] = ""
	c2, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if !c2.TrustProxy || len(c2.TrustedProxies) != 0 {
		t.Fatalf("空网段应传下去由下游校验: %+v", c2)
	}
}

// TestTTLUpperBoundAndActiveCaps 覆盖阶段 1 复核补上的两项边界：
// TTL 上界（有效期长度直接决定防重放账本必须保留多久）与活动 challenge 存量上限。
func TestTTLUpperBoundAndActiveCaps(t *testing.T) {
	env := baseEnv(t)
	env[envChalTTL] = "86400"
	if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
		t.Fatal("challenge TTL 超过上界（1h）应拒绝：否则一条配置就能让账本永久增长")
	}

	env = baseEnv(t)
	c, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxActiveChallenges <= 0 || c.MaxActivePerSite <= 0 ||
		c.MaxActivePerSite > c.MaxActiveChallenges {
		t.Fatalf("默认存量上限不合法: %+v", c)
	}

	env = baseEnv(t)
	env[envMaxActive] = "50"
	env[envMaxActiveSite] = "20"
	c, err = Load(nil, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxActiveChallenges != 50 || c.MaxActivePerSite != 20 {
		t.Fatalf("存量上限未生效: %d/%d", c.MaxActiveChallenges, c.MaxActivePerSite)
	}

	for _, bad := range []struct{ global, site string }{
		{"0", "10"}, {"-1", "10"}, {"10", "0"}, {"10", "20"},
		{"2000000", "10"}, {"abc", "10"},
	} {
		env := baseEnv(t)
		env[envMaxActive] = bad.global
		env[envMaxActiveSite] = bad.site
		if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
			t.Errorf("全局=%q 单站=%q 应拒绝，got %v", bad.global, bad.site, err)
		}
	}
}

// TestAdminBootstrapConfig 覆盖管理引导凭据的三条边界：缺失合法、过短/含空白拒绝、
// 以及错误文案不回显凭据本身。
func TestAdminBootstrapConfig(t *testing.T) {
	env := baseEnv(t)
	c, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AdminKey) != 0 {
		t.Fatalf("未设置 %s 时不应有引导凭据", envAdminKey)
	}
	const adminKey = "ak_0123456789abcdef0123456789abcdef"
	env[envAdminKey] = adminKey
	c, err = Load(nil, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if string(c.AdminKey) != adminKey {
		t.Fatal("引导凭据应与环境变量逐字节相同（不做 base64 变换，登记形式与发送形式必须一致）")
	}
	for name, v := range map[string]string{
		"过短":    "short",
		"含空白":   "ak with spaces inside it here!!",
		"含控制字符": "ak_" + string(rune(7)) + "bcdefghijklmnopqrstuvwxyz0123456",
	} {
		env := baseEnv(t)
		env[envAdminKey] = v
		if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s 的引导凭据应拒绝", name)
		} else if strings.Contains(err.Error(), v) {
			t.Errorf("错误文案回显了凭据：%v", err)
		}
	}
	// 管理监听地址：默认 loopback，flag 可覆盖
	c, err = Load(nil, envMap(baseEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.AdminListen, "127.0.0.1:") {
		t.Fatalf("管理监听默认应只在 loopback，got %s", c.AdminListen)
	}
	c, err = Load([]string{"--admin-listen", "127.0.0.1:9999"}, envMap(baseEnv(t)))
	if err != nil || c.AdminListen != "127.0.0.1:9999" {
		t.Fatalf("--admin-listen 未生效：%v %+v", err, c)
	}
}

// TestSitesFileOriginsValidation 确认配置里的来源条目在装载阶段就规范化并被严格校验。
func TestSitesFileOriginsValidation(t *testing.T) {
	cases := map[string]string{
		"合法":     `[{"name":"a","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa","origins":["https://app.example.com"]}]`,
		"通配符":    `[{"name":"a","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa","origins":["https://*.example.com"]}]`,
		"null":   `[{"name":"a","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa","origins":["null"]}]`,
		"相似后缀":   `[{"name":"a","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa","origins":["https://evil-example.com"]}]`,
		"非 http": `[{"name":"a","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa","origins":["ftp://example.com"]}]`,
	}
	for name, body := range cases {
		path := writeSites(t, body)
		if name == "合法" || name == "相似后缀" {
			if _, err := Load([]string{"--sites", path}, envMap(baseEnv(t))); err != nil {
				t.Fatalf("%s 应通过：%v", name, err)
			}
			continue
		}
		if _, err := Load([]string{"--sites", path}, envMap(baseEnv(t))); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s 应被拒绝，got %v", name, err)
		}
	}
	// 合法条目要带上规范化结果与显式放行标志
	path := writeSites(t, `[{"name":"a","site_key":"k","secret":"aaaaaaaaaaaaaaaaaaaa","origins":["https://app.example.com:443"],"allow_missing_origin":true}]`)
	c, err := Load([]string{"--sites", path}, envMap(baseEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sites[0].Origins) != 1 || c.Sites[0].Origins[0] != "https://app.example.com" {
		t.Fatalf("默认端口未规范化：%+v", c.Sites[0].Origins)
	}
	if !c.Sites[0].AllowMissingOrigin {
		t.Fatal("allow_missing_origin 未生效")
	}
}
