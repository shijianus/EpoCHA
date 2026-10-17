// Package config 把 flag 与环境变量装配成运行时配置（DESIGN §12、ADR-005）。
//
// 密钥只从环境变量或密钥文件读取，不走命令行参数：argv 在同机 `ps` 与容器 inspect 里是可见的。
package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shijianus/EpoCHA/internal/store"
)

const (
	minKeyBytes       = 32
	minSecretLen      = 16
	minAdminKeyLen    = 32
	maxSites          = 64
	sitesFileMaxBytes = 1 << 16

	envMasterKey     = "EPOCHA_MASTER_KEY"
	envPrevMasterKey = "EPOCHA_MASTER_KEY_PREVIOUS"
	envPepper        = "EPOCHA_PEPPER"
	envListen        = "EPOCHA_LISTEN"
	envDB            = "EPOCHA_DB"
	envSites         = "EPOCHA_SITES_FILE"
	envChalTTL       = "EPOCHA_CHALLENGE_TTL_SEC"
	envRespTTL       = "EPOCHA_RESPONSE_TTL_SEC"
	envTrustProxy    = "EPOCHA_TRUST_PROXY"
	envTrustedNets   = "EPOCHA_TRUSTED_PROXIES"
	envRateIP        = "EPOCHA_RATE_IP_RPM"
	envRateSite      = "EPOCHA_RATE_SITE_RPM"
	envRateStates    = "EPOCHA_RATE_MAX_STATES"
	envMaxActive     = "EPOCHA_MAX_ACTIVE_CHALLENGES"
	envMaxActiveSite = "EPOCHA_MAX_ACTIVE_CHALLENGES_PER_SITE"
	envAdminListen   = "EPOCHA_ADMIN_LISTEN"
	envAdminKey      = "EPOCHA_ADMIN_KEY"
)

// TTL 上下界。上界不是形式主义：challenge/response 的有效期直接决定防重放账本里
// "必须保留以防重放"的窗口长度，配成几天等于把库变成永久增长器。
const (
	minTTL     = time.Second
	maxTTL     = time.Hour
	minMaxRows = 1
	maxMaxRows = 1_000_000
)

var (
	ErrInvalid = errors.New("config: 配置不合法")

	// 缺密钥时的错误文案：绝不包含任何密钥材料。
	ErrMissingMasterKey = fmt.Errorf("%w: 缺少 %s", ErrInvalid, envMasterKey)
	ErrMissingPepper    = fmt.Errorf("%w: 缺少 %s", ErrInvalid, envPepper)
)

// Site 是配置文件里的一个站点条目。**只用于一次性导入**：site_key 已存在于库中时
// 本条完全不生效（不覆盖 secret、不重新启用停用站点），见 store.ImportSite 与 ADR-010。
type Site struct {
	Name    string `json:"name"`
	SiteKey string `json:"site_key"`
	Secret  string `json:"secret"`
	PowBits uint8  `json:"pow_bits"`
	// Origins 是允许来源列表（规范化后精确匹配）。空列表 = 不接受任何浏览器来源。
	Origins []string `json:"origins"`
	// AllowMissingOrigin 放行"没有 Origin 的直接 API 调用"。
	// 默认拒绝；演示/验收客户端必须在这一条上显式写 true，不对所有站点静默放开。
	AllowMissingOrigin bool `json:"allow_missing_origin"`
}

// Config 是服务运行参数。
type Config struct {
	// ShowVersion 由 --version 置位。版本查询不要求密钥与数据库，Load 在该情况下提前返回。
	ShowVersion bool

	Listen      string
	AdminListen string
	DBPath      string
	MasterKey   []byte
	PrevMaster  []byte
	Pepper      []byte
	// AdminKey 是**可选**的引导凭据：仅当库里没有任何管理密钥记录时登记一次
	// （store.BootstrapAdminKey）。之后运维应在管理 API 里管理，并可以清空这个环境变量。
	AdminKey     []byte
	Sites        []Site
	ChallengeTTL time.Duration
	ResponseTTL  time.Duration

	TrustProxy     bool
	TrustedProxies []string

	RateIPRPM    int
	RateSiteRPM  int
	RateMaxState int

	// MaxActiveChallenges 与 MaxActivePerSite 是**存量**上限，与限流（速率）正交：
	// 攻击者只要有足够多的源地址就能绕过速率限制，但绕不过"库里同时只允许这么多活动
	// challenge"。缺了这一条，/api/challenge 可以无限扩大 SQLite（DESIGN §14-6/§14-9）。
	// 数值是未实测的 [目标]，取舍依据是 1 vCPU/50GB 机器上账本行的体积与扫描成本。
	MaxActiveChallenges int
	MaxActivePerSite    int
}

// Load 解析命令行参数与环境变量。getenv 为 os.Getenv，测试里可注入。
func Load(args []string, getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // 参数错误由我们包装，不打印用法（避免把内部参数名暴露给外部）
	showVersion := fs.Bool("version", false, "打印版本信息后退出")
	listen := fs.String("listen", "", "监听地址（覆盖 "+envListen+"）")
	db := fs.String("db", "", "SQLite 文件路径（覆盖 "+envDB+"）")
	sites := fs.String("sites", "", "站点配置 JSON 文件，仅作一次性导入（覆盖 "+envSites+"）")
	adminListen := fs.String("admin-listen", "", "管理 API 监听地址，默认仅 loopback（覆盖 "+envAdminListen+"）")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("%w: 命令行参数", ErrInvalid)
	}
	// 版本查询不需要密钥与数据库，提前返回，避免"缺密钥拒绝启动"波及 --version。
	if *showVersion {
		return &Config{ShowVersion: true}, nil
	}

	c := &Config{
		Listen:       firstNonEmpty(*listen, getenv(envListen), "127.0.0.1:8080"),
		AdminListen:  firstNonEmpty(*adminListen, getenv(envAdminListen), "127.0.0.1:8081"),
		DBPath:       firstNonEmpty(*db, getenv(envDB)),
		ChallengeTTL: 180 * time.Second,
		ResponseTTL:  120 * time.Second,
		RateIPRPM:    60,
		RateSiteRPM:  300,
		RateMaxState: 8192,

		MaxActiveChallenges: 10000,
		MaxActivePerSite:    2000,
	}
	var err error
	if c.MasterKey, err = readKey(getenv, envMasterKey); err != nil {
		return nil, err
	}
	if c.Pepper, err = readKey(getenv, envPepper); err != nil {
		return nil, err
	}
	if v := getenv(envPrevMasterKey); v != "" {
		if c.PrevMaster, err = readKey(getenv, envPrevMasterKey); err != nil {
			return nil, err
		}
	}
	if c.DBPath == "" {
		return nil, fmt.Errorf("%w: 缺少数据库路径（--db 或 %s）", ErrInvalid, envDB)
	}
	// 显式设置值与"未设置"必须区分：把 EPOCHA_*_TTL_SEC=0 悄悄回落到默认值，
	// 会让运维以为改了配置而实际没改。
	if d, ok, err := parseDuration(getenv(envChalTTL)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, envChalTTL)
	} else if ok {
		c.ChallengeTTL = d
	}
	if d, ok, err := parseDuration(getenv(envRespTTL)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, envRespTTL)
	} else if ok {
		c.ResponseTTL = d
	}
	if c.ChallengeTTL < minTTL || c.ResponseTTL < minTTL {
		return nil, fmt.Errorf("%w: TTL 必须 ≥%s", ErrInvalid, minTTL)
	}
	if c.ChallengeTTL > maxTTL || c.ResponseTTL > maxTTL {
		return nil, fmt.Errorf("%w: TTL 必须 ≤%s（有效期越长，防重放账本必须保留的窗口越长）", ErrInvalid, maxTTL)
	}

	switch strings.ToLower(getenv(envTrustProxy)) {
	case "", "false", "0":
	case "true", "1":
		c.TrustProxy = true
	default:
		return nil, fmt.Errorf("%w: %s 必须是 true/false", ErrInvalid, envTrustProxy)
	}
	for _, net := range strings.Split(getenv(envTrustedNets), ",") {
		if net = strings.TrimSpace(net); net != "" {
			c.TrustedProxies = append(c.TrustedProxies, net)
		}
	}

	if n, ok, err := parseInt(getenv(envRateIP)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, envRateIP)
	} else if ok {
		c.RateIPRPM = n
	}
	if n, ok, err := parseInt(getenv(envRateSite)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, envRateSite)
	} else if ok {
		c.RateSiteRPM = n
	}
	if n, ok, err := parseInt(getenv(envRateStates)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, envRateStates)
	} else if ok {
		c.RateMaxState = n
	}
	if c.RateIPRPM <= 0 || c.RateSiteRPM <= 0 || c.RateMaxState <= 0 {
		return nil, fmt.Errorf("%w: 限流参数必须为正", ErrInvalid)
	}
	if n, ok, err := parseInt(getenv(envMaxActive)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, envMaxActive)
	} else if ok {
		c.MaxActiveChallenges = n
	}
	if n, ok, err := parseInt(getenv(envMaxActiveSite)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, envMaxActiveSite)
	} else if ok {
		c.MaxActivePerSite = n
	}
	if c.MaxActiveChallenges < minMaxRows || c.MaxActivePerSite < minMaxRows ||
		c.MaxActiveChallenges > maxMaxRows || c.MaxActivePerSite > maxMaxRows {
		return nil, fmt.Errorf("%w: 活动 challenge 上限须在 %d..%d", ErrInvalid, minMaxRows, maxMaxRows)
	}
	if c.MaxActivePerSite > c.MaxActiveChallenges {
		return nil, fmt.Errorf("%w: 单站上限不得高于全局上限", ErrInvalid)
	}

	// 站点配置是**可选的导入入口**：阶段 2 起 SQLite 才是权威数据源。
	// 没有配置文件也能正常启动（站点全部来自库）；有则只导入库里尚不存在的 site_key。
	if sitesPath := firstNonEmpty(*sites, getenv(envSites)); sitesPath != "" {
		if c.Sites, err = loadSites(sitesPath); err != nil {
			return nil, err
		}
	}
	// 管理引导凭据是可选的：库里已有管理密钥记录时它完全不生效（见 store.BootstrapAdminKey）。
	if v := getenv(envAdminKey); v != "" {
		if c.AdminKey, err = readAdminKey(v); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// loadSites 读取站点 JSON。secret 字段是明文密钥材料，只在本函数内短暂存在。
func loadSites(path string) ([]Site, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: 无法读取站点文件", ErrInvalid)
	}
	defer f.Close()

	body, err := io.ReadAll(io.LimitReader(f, sitesFileMaxBytes+1))
	if err != nil || len(body) > sitesFileMaxBytes {
		return nil, fmt.Errorf("%w: 站点文件过大或不可读", ErrInvalid)
	}
	var raw []struct {
		Name               string   `json:"name"`
		SiteKey            string   `json:"site_key"`
		Secret             string   `json:"secret"`
		PowBits            *int     `json:"pow_bits"`
		Origins            []string `json:"origins"`
		AllowMissingOrigin bool     `json:"allow_missing_origin"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: 站点文件不是合法的 JSON 数组", ErrInvalid)
	}
	if len(raw) == 0 || len(raw) > maxSites {
		return nil, fmt.Errorf("%w: 站点数量须在 1..%d", ErrInvalid, maxSites)
	}
	out := make([]Site, 0, len(raw))
	seenKey := map[string]bool{}
	seenName := map[string]bool{}
	for _, r := range raw {
		s := Site{Name: strings.TrimSpace(r.Name), SiteKey: strings.TrimSpace(r.SiteKey), Secret: r.Secret}
		switch {
		case s.Name == "" || s.SiteKey == "" || s.Secret == "":
			return nil, fmt.Errorf("%w: 站点 name/site_key/secret 不可为空", ErrInvalid)
		case len(s.Secret) < minSecretLen:
			// 文案里不回显 secret 本身
			return nil, fmt.Errorf("%w: 站点 %q 的 secret 短于 %d 字符", ErrInvalid, s.Name, minSecretLen)
		case seenKey[s.SiteKey]:
			return nil, fmt.Errorf("%w: site_key 重复", ErrInvalid)
		case seenName[s.Name]:
			return nil, fmt.Errorf("%w: 站点名重复", ErrInvalid)
		}
		// 未指定时给一个保守默认值，而不是让 0 穿过 DB 的 CHECK 约束。
		bits := 18
		if r.PowBits != nil {
			bits = *r.PowBits
		}
		if bits < 12 || bits > 24 {
			return nil, fmt.Errorf("%w: 站点 %q 的 pow_bits=%d 越界", ErrInvalid, s.Name, bits)
		}
		s.PowBits = uint8(bits)
		s.AllowMissingOrigin = r.AllowMissingOrigin
		s.Origins = make([]string, 0, len(r.Origins))
		if len(r.Origins) > 32 {
			return nil, fmt.Errorf("%w: 站点 %q 的来源条目超过 32", ErrInvalid, s.Name)
		}
		// 配置里的来源必须是可规范化的完整 Origin：在这里就拒绝，不把问题留到导入之后。
		for _, o := range r.Origins {
			n, err := store.NormalizeOrigin(o)
			if err != nil {
				return nil, fmt.Errorf("%w: 站点 %q 的来源 %q 不合法", ErrInvalid, s.Name, o)
			}
			s.Origins = append(s.Origins, n)
		}
		seenKey[s.SiteKey], seenName[s.Name] = true, true
		out = append(out, s)
	}
	return out, nil
}

// readAdminKey 读取管理引导凭据。它与运维实际发出的 Bearer 值**逐字节相同**——
// 不做 base64 解码：把"库中登记的形式"和"线上发送的形式"分开会制造两套真相。
func readAdminKey(v string) ([]byte, error) {
	v = strings.TrimSpace(v)
	if len(v) < minAdminKeyLen {
		// 不回显原值
		return nil, fmt.Errorf("%w: %s 短于 %d 字符", ErrInvalid, envAdminKey, minAdminKeyLen)
	}
	for _, r := range v {
		if r < 0x21 || r > 0x7e {
			return nil, fmt.Errorf("%w: %s 含空白或控制字符", ErrInvalid, envAdminKey)
		}
	}
	return []byte(v), nil
}

// readKey 读取 base64 编码的密钥材料，要求解码后 ≥32 字节。
func readKey(getenv func(string) string, name string) ([]byte, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		if name == envPepper {
			return nil, ErrMissingPepper
		}
		return nil, ErrMissingMasterKey
	}
	k, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		// 不回显原值，也不回显解码错误细节
		return nil, fmt.Errorf("%w: %s 不是合法 base64", ErrInvalid, name)
	}
	if len(k) < minKeyBytes {
		return nil, fmt.Errorf("%w: %s 解码后短于 %d 字节", ErrInvalid, name, minKeyBytes)
	}
	return k, nil
}

// SafeSummary 是可安全写入启动输出与日志的一行摘要。
func (c *Config) SafeSummary() string {
	return fmt.Sprintf("listen=%s admin_listen=%s db=%s site_imports=%d challenge_ttl=%s response_ttl=%s trust_proxy=%t ip_rpm=%d site_rpm=%d max_active=%d max_active_per_site=%d admin_key_present=%t",
		c.Listen, c.AdminListen, c.DBPath, len(c.Sites), c.ChallengeTTL, c.ResponseTTL, c.TrustProxy,
		c.RateIPRPM, c.RateSiteRPM, c.MaxActiveChallenges, c.MaxActivePerSite, len(c.AdminKey) > 0)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseDuration 返回 (值, 是否显式设置, 错误)。空串表示未设置。
func parseDuration(v string) (time.Duration, bool, error) {
	if strings.TrimSpace(v) == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, true, errors.New("invalid")
	}
	return time.Duration(n) * time.Second, true, nil
}

// parseInt 同 parseDuration，用于计数型配置。
func parseInt(v string) (int, bool, error) {
	if strings.TrimSpace(v) == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, true, errors.New("invalid")
	}
	return n, true, nil
}
