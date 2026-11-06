package store

// 阶段 2 的站点模型（DESIGN §5.2、§10.2）。
//
// 三个概念必须分开，否则"轮换密钥"会被误写成"换站点"：
//   - id      ：稳定的内部标识，账本行外键指向它，永不复用。
//   - site_key：公开标识，出现在浏览器请求里，稳定不变。
//   - secret  ：验证凭据，可多把并存（轮换过渡期）、可撤销，明文永不落库。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/shijianus/EpoCHA/internal/pow"
)

// 策略边界。DB 的 CHECK 与这里的校验必须一致：SQL 侧是最后一道，Go 侧负责给出可解释的错误。
const (
	MaxSites             = 512 // FindSiteBySecret 是常数时间的全表扫描，站点数必须有上界
	MaxSecretsPerSite    = 4   // 同时有效的验证凭据数（过渡期新旧并存的上限）
	MaxOriginsPerSite    = 32  // 允许来源条目数
	MaxGeneratorsPerSite = 8   // 站点级候选题型上限（ADR-026）
	MaxNameLen           = 64
	MaxSiteKeyLen        = 64
	MinSecretLen         = 16 // 导入阶段 1 配置时兼容短 secret；本专案自生成的是 68 字符
	SecretPrefix         = "sk_"
	AdminKeyPrefix       = "ak_"
	MaxTTLSeconds        = 3600
	MinTTLSeconds        = 1
	MaxRateRPM           = 100000
	MaxTransitionSecond  = 86400 // 轮换过渡期硬上限（1 天）
)

// SitePolicy 是站点级可配策略。
type SitePolicy struct {
	PowBits            uint8
	ChallengeTTL       time.Duration
	ResponseTTL        time.Duration
	RateIPRPM          int
	RateSiteRPM        int
	AllowMissingOrigin bool
	// VerifyMode 是 pow_only / pow_question（阶段 3A）。它决定公开端点的响应形状，
	// 因此必须是站点的显式声明，不能由"池里刚好有题"推导。
	VerifyMode string
	// GeneratorID 与 Difficulty 只在 pow_question 下有意义：它们就是领题的分桶键。
	// GeneratorPool 为空时等价于只含 GeneratorID；非空时由调度器在池内按风险/设备抽题。
	GeneratorID   string
	GeneratorPool []string
	Difficulty    int
}

// GeneratorIDs 返回调度候选；空池回落到旧的单 generator_id。
func (p SitePolicy) GeneratorIDs() []string {
	if len(p.GeneratorPool) > 0 {
		return append([]string(nil), p.GeneratorPool...)
	}
	if p.GeneratorID != "" {
		return []string{p.GeneratorID}
	}
	return nil
}

// Site 是库中站点的完整视图（权威数据源是 SQLite，配置文件只是一次性导入入口）。
type Site struct {
	ID      int64
	Name    string
	SiteKey string
	// PowBits 只是 Policy.PowBits 的读取快捷方式，由 scanSite 单点赋值；写路径一律走 Policy。
	PowBits   uint8
	Enabled   bool
	Version   int64
	Policy    SitePolicy
	CreatedAt time.Time
}

// siteColumns 是站点读取的统一列序；不含 sites.secret_mac——该列已在 0002 迁移中被中和，
// 任何真实 secret 都匹配不上它（见 0002_stage2.sql 注释）。
const siteColumns = `id, name, site_key, pow_bits, enabled, version,
	challenge_ttl_sec, response_ttl_sec, rate_ip_rpm, rate_site_rpm, allow_missing_origin,
	verify_mode, generator_id, generator_pool, difficulty, created_at`

func scanSite(sc interface {
	Scan(dest ...any) error
}) (*Site, error) {
	var (
		s                  = &Site{}
		enabled, allowMiss int64
		chalTTL, respTTL   int64
		created            string
		poolRaw            string
		err                error
	)
	if err = sc.Scan(&s.ID, &s.Name, &s.SiteKey, &s.PowBits, &enabled, &s.Version,
		&chalTTL, &respTTL, &s.Policy.RateIPRPM, &s.Policy.RateSiteRPM, &allowMiss,
		&s.Policy.VerifyMode, &s.Policy.GeneratorID, &poolRaw, &s.Policy.Difficulty, &created); err != nil {
		return nil, err
	}
	if s.Policy.GeneratorPool, err = parseGeneratorPool(poolRaw); err != nil {
		return nil, err
	}
	s.Policy.PowBits = s.PowBits
	s.Enabled, s.Policy.AllowMissingOrigin = enabled == 1, allowMiss == 1
	s.Policy.ChallengeTTL = time.Duration(chalTTL) * time.Second
	s.Policy.ResponseTTL = time.Duration(respTTL) * time.Second
	if s.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return s, nil
}

// Validate 检查策略边界；CreateSite 与 UpdateSite 共用，避免两处漂移。
func (p SitePolicy) Validate() error {
	switch {
	case p.PowBits < pow.MinBits || p.PowBits > pow.MaxBits:
		return fmt.Errorf("%w: pow_bits=%d 须在 %d..%d", ErrBadConfig, p.PowBits, pow.MinBits, pow.MaxBits)
	case int64(p.ChallengeTTL/time.Second) < MinTTLSeconds || int64(p.ChallengeTTL/time.Second) > MaxTTLSeconds:
		return fmt.Errorf("%w: challenge TTL 须在 %d..%d 秒", ErrBadConfig, MinTTLSeconds, MaxTTLSeconds)
	case int64(p.ResponseTTL/time.Second) < MinTTLSeconds || int64(p.ResponseTTL/time.Second) > MaxTTLSeconds:
		return fmt.Errorf("%w: response TTL 须在 %d..%d 秒", ErrBadConfig, MinTTLSeconds, MaxTTLSeconds)
	case p.RateIPRPM < 1 || p.RateIPRPM > MaxRateRPM:
		return fmt.Errorf("%w: rate_ip_rpm=%d 须在 1..%d", ErrBadConfig, p.RateIPRPM, MaxRateRPM)
	case p.RateSiteRPM < 1 || p.RateSiteRPM > MaxRateRPM:
		return fmt.Errorf("%w: rate_site_rpm=%d 须在 1..%d", ErrBadConfig, p.RateSiteRPM, MaxRateRPM)
	case p.VerifyMode != ModePowOnly && p.VerifyMode != ModePowQuestion:
		return fmt.Errorf("%w: 未知验证模式 %q", ErrBadConfig, p.VerifyMode)
	case p.VerifyMode == ModePowQuestion && p.GeneratorID == "":
		// pow_question 没配生成器 = 领题时只能整批失败；在写入前就拒绝，别留到运行时
		return fmt.Errorf("%w: pow_question 站点必须指定 generator_id", ErrBadConfig)
	case p.VerifyMode == ModePowQuestion && (p.Difficulty < MinDifficulty || p.Difficulty > MaxDifficulty):
		return fmt.Errorf("%w: 难度档须在 %d..%d", ErrBadConfig, MinDifficulty, MaxDifficulty)
	}
	if _, err := normalizeGeneratorPool(p.GeneratorPool); err != nil {
		return err
	}
	return nil
}

// DefaultPolicy 给出与阶段 1 一致的中性默认值（导入配置、创建站点时作为基线）。
func DefaultPolicy() SitePolicy {
	return SitePolicy{
		PowBits:      18,
		ChallengeTTL: 180 * time.Second,
		ResponseTTL:  120 * time.Second,
		RateIPRPM:    60,
		RateSiteRPM:  300,
		VerifyMode:   ModePowOnly, // 默认与阶段 1/2 行为一致：只发 PoW
		Difficulty:   MinDifficulty,
	}
}

func validateNameSiteKey(name, siteKey string) error {
	if err := validateName(name); err != nil {
		return err
	}
	switch {
	case !printableOK(siteKey) || len(siteKey) > MaxSiteKeyLen:
		return fmt.Errorf("%w: site_key 须为 1..%d 个可见字符", ErrBadConfig, MaxSiteKeyLen)
	}
	return nil
}

func validateName(name string) error {
	if !printableOK(name) || len(name) > MaxNameLen {
		return fmt.Errorf("%w: 站点名须为 1..%d 个可见字符", ErrBadConfig, MaxNameLen)
	}
	return nil
}

// printableOK 拒绝空白与控制字符：审计与启动日志会打印这些标识，混入控制字符等于注入日志行。
func printableOK(s string) bool {
	if strings.TrimSpace(s) != s || s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// CreateSite 新建站点并同时生成第一把验证凭据；secret 明文只在此返回一次。
func (d *DB) CreateSite(ctx context.Context, name, siteKey string, p SitePolicy) (*Site, string, error) {
	return d.createSite(ctx, name, siteKey, p, "")
}

// CreateSiteAudited 将站点、首把凭据、来源与成功审计同事务提交。
// 提交后响应仍可能丢失；此接口只保证审计失败不会留下未返回的凭据（ADR-021）。
func (d *DB) CreateSiteAudited(ctx context.Context, name, siteKey string, p SitePolicy, origins []string, actor string) (*Site, string, error) {
	clean, err := normalizeOrigins(origins)
	if err != nil {
		return nil, "", err
	}
	return d.createSiteWithOrigins(ctx, name, siteKey, p, "", clean, &actor)
}

// createSite 落一行站点与一把凭据。secretOverride 非空时使用调用方给定的 secret
// （配置一次性导入的路径），否则随机生成。返回的 secret 就是要一次性展示给运维的明文。
func (d *DB) createSite(ctx context.Context, name, siteKey string, p SitePolicy, secretOverride string) (*Site, string, error) {
	return d.createSiteWithOrigins(ctx, name, siteKey, p, secretOverride, nil, nil)
}

func (d *DB) createSiteWithOrigins(ctx context.Context, name, siteKey string, p SitePolicy, secretOverride string, origins []string, auditActor *string) (*Site, string, error) {
	if err := validateNameSiteKey(name, siteKey); err != nil {
		return nil, "", err
	}
	p = withDefaults(p)
	if err := p.Validate(); err != nil {
		return nil, "", err
	}
	var n int
	if err := d.h.QueryRowContext(ctx, `SELECT count(*) FROM sites`).Scan(&n); err != nil {
		return nil, "", fmt.Errorf("store: 统计站点: %w", err)
	}
	if n >= MaxSites {
		return nil, "", fmt.Errorf("%w: 站点数已达上限 %d", ErrBadConfig, MaxSites)
	}
	secret := secretOverride
	if secret == "" {
		var err error
		if secret, err = NewSecret(); err != nil {
			return nil, "", err
		}
	}
	if len(secret) < MinSecretLen {
		return nil, "", fmt.Errorf("%w: secret 短于 %d 字符", ErrBadConfig, MinSecretLen)
	}
	keyID, err := NewKeyID()
	if err != nil {
		return nil, "", err
	}
	now := formatTime(time.Now().UTC())
	poolJSON, err := encodeGeneratorPool(p.GeneratorPool)
	if err != nil {
		return nil, "", err
	}
	tx, err := d.h.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("store: 建站点事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if auditActor != nil {
		if err := guardAuditedActor(ctx, tx, *auditActor); err != nil {
			return nil, "", err
		}
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO sites(name, site_key, secret_mac, pow_bits, created_at, enabled, version,
                  challenge_ttl_sec, response_ttl_sec, rate_ip_rpm, rate_site_rpm, allow_missing_origin,
                  verify_mode, generator_id, generator_pool, difficulty)
VALUES(?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?)`,
		name, siteKey, retiredMacFor(siteKey), int(p.PowBits), now,
		1, int64(p.ChallengeTTL/time.Second), int64(p.ResponseTTL/time.Second),
		p.RateIPRPM, p.RateSiteRPM, b2i(p.AllowMissingOrigin),
		p.VerifyMode, p.GeneratorID, poolJSON, p.Difficulty)
	if err != nil {
		if isUniqueErr(err) {
			return nil, "", fmt.Errorf("%w: 站点名或 site_key 已存在", ErrConflict)
		}
		return nil, "", fmt.Errorf("store: 写入站点: %w", err)
	}
	siteID, err := res.LastInsertId()
	if err != nil {
		return nil, "", fmt.Errorf("store: 回读站点 id: %w", err)
	}
	// 站点行与凭据行必须同一事务提交：否则会出现"站点存在但没有可用凭据"的半状态，
	// 那种状态下 /siteverify 永远失败而 /api/challenge 照常签发。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO site_secrets(key_id, site_id, secret_mac, created_at) VALUES(?,?,?,?)`,
		keyID, siteID, hex.EncodeToString(d.SecretMac(secret)), now); err != nil {
		if isUniqueErr(err) {
			return nil, "", fmt.Errorf("%w: secret 摘要冲突（撞库，重试即可）", ErrConflict)
		}
		return nil, "", fmt.Errorf("store: 登记凭据: %w", err)
	}
	if err := insertOrigins(ctx, tx, siteID, origins, now); err != nil {
		return nil, "", err
	}
	if auditActor != nil {
		if err := appendAudit(ctx, tx, *auditActor, "site.create", "site:"+strconv.FormatInt(siteID, 10), true); err != nil {
			return nil, "", err
		}
	}
	// 所有可能失败的回读在提交前完成，避免已生效后因回读失败丢掉一次性明文。
	site, err := scanSite(tx.QueryRowContext(ctx, `SELECT `+siteColumns+` FROM sites WHERE id=?`, siteID))
	if err != nil {
		return nil, "", fmt.Errorf("store: 回读新站点: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("store: 提交建站点: %w", err)
	}
	return site, secret, nil
}

// ImportSite 是配置文件的**一次性导入**入口：site_key 已存在时原样返回库中的站点、不做任何覆盖。
// 这是"重启不得把旧配置写回权威数据源"的落点——配置文件里过期的 secret/难度/白名单
// 既不能复活已撤销的密钥，也不能重新启用已停用的站点。返回 imported 表示本轮是否真的建了站点。
func (d *DB) ImportSite(ctx context.Context, name, siteKey, secret string, p SitePolicy) (*Site, bool, error) {
	if existing, err := d.SiteByKey(ctx, siteKey); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	site, _, err := d.createSite(ctx, name, siteKey, p, secret)
	if err != nil {
		return nil, false, err
	}
	return site, true, nil
}

// SiteUpdate 是显式的部分更新：nil = 不改。
// 用指针而不是"零值当哨兵"，因为布尔量没有安全的零值——把 AllowMissingOrigin 的 false
// 解释成"不改"会让"改成拒绝"永远无法表达，而解释成"要改"又会让只想调难度的请求静默收紧来源策略。
type SiteUpdate struct {
	Name *string
	// PowBits 等策略字段：nil = 不改（见类型注释）。
	PowBits            *uint8
	ChallengeTTL       *time.Duration
	ResponseTTL        *time.Duration
	RateIPRPM          *int
	RateSiteRPM        *int
	AllowMissingOrigin *bool
	VerifyMode         *string
	GeneratorID        *string
	GeneratorPool      *[]string
	Difficulty         *int
}

// UpdateSite 改策略字段。**不**提升站点版本：已签发的 token 继续按签发快照生效（DESIGN §5.3）。
// 立即生效的安全动作只有两类：停用站点与撤销凭据，它们各自走自己的路径。
//
// Name 走同一条 UPDATE：显示名不参与任何判定（账本按 site_id 外键、下发按 site_key），
// 改它不会让任何已签发东西失效，也不需要提升站点版本。
func (d *DB) UpdateSite(ctx context.Context, id int64, u SiteUpdate) (*Site, error) {
	cur, err := d.SiteByID(ctx, id)
	if err != nil {
		return nil, err
	}
	next := applyUpdate(cur.Policy, u)
	if err := next.Validate(); err != nil {
		return nil, err
	}
	name := cur.Name
	if u.Name != nil {
		if err := validateName(*u.Name); err != nil {
			return nil, err
		}
		name = *u.Name
	}
	poolJSON, err := encodeGeneratorPool(next.GeneratorPool)
	if err != nil {
		return nil, err
	}
	res, err := d.h.ExecContext(ctx, `
UPDATE sites SET name=?, pow_bits=?, challenge_ttl_sec=?, response_ttl_sec=?, rate_ip_rpm=?, rate_site_rpm=?,
                 allow_missing_origin=?, verify_mode=?, generator_id=?, generator_pool=?, difficulty=?
WHERE id=?`,
		name, int(next.PowBits), int64(next.ChallengeTTL/time.Second), int64(next.ResponseTTL/time.Second),
		next.RateIPRPM, next.RateSiteRPM, b2i(next.AllowMissingOrigin),
		next.VerifyMode, next.GeneratorID, poolJSON, next.Difficulty, id)
	if err != nil {
		// name 与 site_key 都是 UNIQUE：撞名是"另一个站点已经用了这个名字"，
		// 不是内部错误，也不回滚任何东西（这条 UPDATE 要么整体生效要么整体不生效）。
		if isUniqueErr(err) {
			return nil, fmt.Errorf("%w: 站点名或 site_key 已存在", ErrConflict)
		}
		return nil, fmt.Errorf("store: 更新站点: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrNotFound
	}
	return d.SiteByID(ctx, id)
}

// SetSiteEnabled 启停站点。每次**状态变化**都提升站点版本：
// 停用使未消费的 token 立即失效，重新启用也不会让它们复活。
func (d *DB) SetSiteEnabled(ctx context.Context, id int64, enabled bool) (*Site, error) {
	cur, err := d.SiteByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.Enabled == enabled {
		return cur, nil
	}
	res, err := d.h.ExecContext(ctx, `
UPDATE sites SET enabled=?, version=version+1 WHERE id=?`, b2i(enabled), id)
	if err != nil {
		return nil, fmt.Errorf("store: 启停站点: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrNotFound
	}
	return d.SiteByID(ctx, id)
}

// ListSites 按 id 游标分页（limit ≤500）。
func (d *DB) ListSites(ctx context.Context, afterID int64, limit int) ([]*Site, error) {
	limit, err := pageSize(limit, 50, 500)
	if err != nil {
		return nil, err
	}
	rows, err := d.h.QueryContext(ctx, `SELECT `+siteColumns+` FROM sites WHERE id>? ORDER BY id LIMIT ?`,
		afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 列站点: %w", err)
	}
	defer rows.Close()
	out := []*Site{}
	for rows.Next() {
		s, err := scanSite(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描站点: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SiteByKey 按公开 site_key 查站点。
func (d *DB) SiteByKey(ctx context.Context, siteKey string) (*Site, error) {
	return d.querySite(ctx, `SELECT `+siteColumns+` FROM sites WHERE site_key=?`, siteKey)
}

// SiteByID 按内部 id 查站点。
func (d *DB) SiteByID(ctx context.Context, id int64) (*Site, error) {
	return d.querySite(ctx, `SELECT `+siteColumns+` FROM sites WHERE id=?`, id)
}

func (d *DB) querySite(ctx context.Context, q string, arg any) (*Site, error) {
	s, err := scanSite(d.h.QueryRowContext(ctx, q, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查站点: %w", err)
	}
	return s, nil
}

// --- 生成器池 ---

// normalizeGeneratorPool 去空/去重并做边界校验；返回副本，调用方可安全持有。
func normalizeGeneratorPool(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxGeneratorsPerSite {
		return nil, fmt.Errorf("%w: 生成器池最多 %d 项", ErrBadConfig, MaxGeneratorsPerSite)
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if !printableOK(id) || len(id) > 64 {
			return nil, fmt.Errorf("%w: 生成器 id 须为 1..64 个可见字符", ErrBadConfig)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: 生成器池含重复 id %q", ErrBadConfig, id)
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

func encodeGeneratorPool(in []string) (string, error) {
	clean, err := normalizeGeneratorPool(in)
	if err != nil {
		return "", err
	}
	if len(clean) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return "", fmt.Errorf("store: 编码生成器池: %w", err)
	}
	return string(raw), nil
}

func parseGeneratorPool(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("store: 解析生成器池: %w", err)
	}
	return normalizeGeneratorPool(out)
}

// --- 辅助 ---

// retiredMacFor 给已弃用的 sites.secret_mac 列生成占位值。
// 阶段 2 的凭据权威在 site_secrets 表；但 SQLite 删不掉带 UNIQUE 约束的旧列（实测报
// "cannot drop UNIQUE column"），所以旧列留一个由 site_key 派生的定宽摘要：
// 它等于 HMAC(pepper, 某个真实 secret) 的概率与伪造摘要本身同级，即实际不可用，
// 同时因为 site_key 已 UNIQUE 而天然满足该列的唯一约束。
func retiredMacFor(siteKey string) string {
	sum := sha256.Sum256([]byte(retiredPrefix + siteKey))
	return hex.EncodeToString(sum[:])
}

func withDefaults(p SitePolicy) SitePolicy {
	def := DefaultPolicy()
	if p.PowBits == 0 {
		p.PowBits = def.PowBits
	}
	if p.ChallengeTTL == 0 {
		p.ChallengeTTL = def.ChallengeTTL
	}
	if p.ResponseTTL == 0 {
		p.ResponseTTL = def.ResponseTTL
	}
	if p.RateIPRPM == 0 {
		p.RateIPRPM = def.RateIPRPM
	}
	if p.RateSiteRPM == 0 {
		p.RateSiteRPM = def.RateSiteRPM
	}
	if p.VerifyMode == "" {
		p.VerifyMode = def.VerifyMode
	}
	if p.Difficulty == 0 {
		p.Difficulty = def.Difficulty
	}
	return p
}

// applyUpdate 把 nil 视为"保持现状"。
func applyUpdate(cur SitePolicy, u SiteUpdate) SitePolicy {
	out := cur
	if u.PowBits != nil {
		out.PowBits = *u.PowBits
	}
	if u.ChallengeTTL != nil {
		out.ChallengeTTL = *u.ChallengeTTL
	}
	if u.ResponseTTL != nil {
		out.ResponseTTL = *u.ResponseTTL
	}
	if u.RateIPRPM != nil {
		out.RateIPRPM = *u.RateIPRPM
	}
	if u.RateSiteRPM != nil {
		out.RateSiteRPM = *u.RateSiteRPM
	}
	if u.AllowMissingOrigin != nil {
		out.AllowMissingOrigin = *u.AllowMissingOrigin
	}
	if u.VerifyMode != nil {
		out.VerifyMode = *u.VerifyMode
	}
	if u.GeneratorID != nil {
		out.GeneratorID = *u.GeneratorID
	}
	if u.GeneratorPool != nil {
		out.GeneratorPool = append([]string(nil), (*u.GeneratorPool)...)
	}
	if u.Difficulty != nil {
		out.Difficulty = *u.Difficulty
	}
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func pageSize(want, def, max int) (int, error) {
	switch {
	case want < 0:
		return 0, fmt.Errorf("%w: 分页参数为负", ErrBadConfig)
	case want == 0:
		return def, nil
	case want > max:
		return 0, fmt.Errorf("%w: 分页上限为 %d", ErrBadConfig, max)
	}
	return want, nil
}

func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

// NewSecret 生成一把高熵验证凭据：前缀只是可读标识，熵全部来自 crypto/rand（256 bit）。
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: 生成 secret: %w", err)
	}
	return SecretPrefix + hex.EncodeToString(b), nil
}

// NewAdminKey 生成一把管理凭据。
func NewAdminKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: 生成 admin key: %w", err)
	}
	return AdminKeyPrefix + hex.EncodeToString(b), nil
}

// NewSiteKey 生成公开站点标识。它不是凭据（写在浏览器里），但也不能可预测——
// 可预测的 site_key 会让"探测别的租户"变成一次枚举。
func NewSiteKey() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: 生成 site_key: %w", err)
	}
	return "pk_" + hex.EncodeToString(b), nil
}

// NewKeyID 生成公开密钥标识（与 secret 的随机部分彼此独立）。
func NewKeyID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: 生成 key id: %w", err)
	}
	return "k" + hex.EncodeToString(b), nil
}
