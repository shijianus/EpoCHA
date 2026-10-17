// Package store 提供 SQLite 防重放账本：challenge 与 response 的一次性状态转移（DESIGN §5.1、ADR-004）。
package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/0001_stage1.sql
var migration0001 string

// schemaVersion 是 migration0001 对应的版本号，写入 meta 表。
const schemaVersion = 1

// MaxFailedAttempts 是一个 challenge 允许的失败兑换次数；达到即作废（DESIGN §3 失败策略）。
const MaxFailedAttempts = 3

// purgeBatchRows 限制单轮清理的行数：清理跑在单写连接上，一次全表删除会在 1 vCPU 上
// 长时间占住写入，"有界、可停止"比"一轮清干净"更重要。剩余行由下一轮处理。
const purgeBatchRows = 500

// Caps 是签发侧的存量上限。它与限流器是两个量：限流控制速率，Caps 控制数据库里同时
// 存在的活动 challenge 行数——缺了后者，攻击者用足够多的源地址仍能持续扩大账本。
type Caps struct {
	MaxActive        int // 全局 issued 且未过期的 challenge 上限
	MaxActivePerSite int // 单站点同上
}

const (
	statusIssued   = "issued"
	statusRedeemed = "redeemed"
	statusInvalid  = "invalid"
)

// 驱动实测事实（ADR-007）：驱动名 "sqlite"，DSN 的 _pragma 参数在连接上生效。
const dsnTemplate = "file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

var (
	ErrNotFound       = errors.New("store: 记录不存在")
	ErrNotRedeemable  = errors.New("store: challenge 不可兑换")
	ErrConsumed       = errors.New("store: response 已被消费或过期")
	ErrBadConfig      = errors.New("store: 参数不合法")
	ErrSchemaConflict = errors.New("store: 数据库版本比程序新")
	ErrCapExceeded    = errors.New("store: 活动 challenge 已达上限")

	// timeLayout 用固定 9 位小数（不裁剪尾零）：账本里的过期判定是 SQL 内的字符串比较，
	// 只有定宽格式才能让字典序与时间序一致。time.RFC3339Nano 会裁掉尾随零，
	// 使 "…:03Z" 与 "…:03.5Z" 的比较落到 'Z' vs '.' 上。
	timeLayout = "2006-01-02T15:04:05.000000000Z07:00"
)

// DB 是账本句柄。
type DB struct {
	h      *sql.DB
	pepper []byte
}

// Site 是一个已配置站点。secret 明文只存在于调用方，库里只有带 pepper 的摘要。
type Site struct {
	ID        int64
	Name      string
	SiteKey   string
	SecretMac []byte
	PowBits   uint8
}

// Challenge 是一次 challenge 的权威记录：PoW 参数以此行为准，不接受客户端自报。
type Challenge struct {
	ID         string
	SiteID     int64
	PowSeed    []byte
	PowBits    uint8
	Status     string
	Attempts   int
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RedeemedAt time.Time
}

// Response 是一次成功兑换签发的短期令牌记录。
type Response struct {
	ID          string
	ChallengeID string
	SiteID      int64
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// Verification 是 siteverify 成功后回读的事实。
type Verification struct {
	SiteID             int64
	ChallengeID        string
	ChallengeCreatedAt time.Time
}

// Open 打开（必要时创建）数据库并应用迁移。pepper 必须 ≥32 字节。
func Open(ctx context.Context, path string, pepper []byte) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: 缺少数据库路径", ErrBadConfig)
	}
	if len(pepper) < 32 {
		return nil, fmt.Errorf("%w: pepper 长度不足 32 字节", ErrBadConfig)
	}
	h, err := sql.Open("sqlite", fmt.Sprintf(dsnTemplate, path))
	if err != nil {
		return nil, fmt.Errorf("store: 打开: %w", err)
	}
	// 单连接是第一版规避 SQLITE_BUSY 的简化选择，不是安全边界（ADR-004）。
	h.SetMaxOpenConns(1)
	h.SetMaxIdleConns(1)
	if err := h.PingContext(ctx); err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("store: 连接: %w", err)
	}
	d := &DB{h: h, pepper: pepper}
	if err := d.migrate(ctx); err != nil {
		_ = h.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error { return d.h.Close() }

// migrate 幂等应用迁移：版本相同则跳过，库版本更新则拒绝启动。
func (d *DB) migrate(ctx context.Context) error {
	tx, err := d.h.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 迁移事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("store: meta 表: %w", err)
	}
	cur, err := schemaVersionOf(ctx, tx)
	if err != nil {
		return err
	}
	if cur > schemaVersion {
		return fmt.Errorf("%w: 库为 v%d，程序支持 v%d", ErrSchemaConflict, cur, schemaVersion)
	}
	if cur == schemaVersion {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, migration0001); err != nil {
		return fmt.Errorf("store: 应用迁移: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(k, v) VALUES('schema_version', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		fmt.Sprint(schemaVersion)); err != nil {
		return fmt.Errorf("store: 记录版本: %w", err)
	}
	return tx.Commit()
}

// SchemaVersion 返回当前库版本。
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullString
	if err := d.h.QueryRowContext(ctx, `SELECT v FROM meta WHERE k='schema_version'`).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: 读版本: %w", err)
	}
	var n int
	if _, err := fmt.Sscanf(v.String, "%d", &n); err != nil {
		return 0, fmt.Errorf("store: 解析版本 %q: %w", v.String, err)
	}
	return n, nil
}

func schemaVersionOf(ctx context.Context, tx *sql.Tx) (int, error) {
	var v sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT v FROM meta WHERE k='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: 读版本: %w", err)
	}
	var n int
	if _, err := fmt.Sscanf(v.String, "%d", &n); err != nil {
		return 0, fmt.Errorf("store: 解析版本 %q: %w", v.String, err)
	}
	return n, nil
}

// SecretMac 计算站点密钥摘要。pepper 不出包，调用方拿不到明文密钥。
func (d *DB) SecretMac(secret string) []byte {
	m := hmac.New(sha256.New, d.pepper)
	m.Write([]byte(secret))
	return m.Sum(nil)
}

// PutSite 幂等登记配置里的站点（阶段 1 站点来自配置文件，管理 API 属阶段 2）。
func (d *DB) PutSite(ctx context.Context, name, siteKey, secret string, bits uint8) (*Site, error) {
	if name == "" || siteKey == "" || secret == "" {
		return nil, fmt.Errorf("%w: 站点字段不完整", ErrBadConfig)
	}
	if bits < 12 || bits > 24 {
		return nil, fmt.Errorf("%w: pow_bits=%d 越界", ErrBadConfig, bits)
	}
	mac := d.SecretMac(secret)
	_, err := d.h.ExecContext(ctx, `
INSERT INTO sites(name, site_key, secret_mac, pow_bits, created_at) VALUES(?,?,?,?,?)
ON CONFLICT(site_key) DO UPDATE SET name=excluded.name, secret_mac=excluded.secret_mac, pow_bits=excluded.pow_bits`,
		name, siteKey, hex.EncodeToString(mac), int(bits), formatTime(time.Now().UTC()))
	if err != nil {
		return nil, fmt.Errorf("store: 登记站点: %w", err)
	}
	// site_key 上有唯一约束，回读得到稳定 id；不使用 RETURNING（不假设驱动捆绑的 SQLite 版本支持它）。
	got, err := d.SiteByKey(ctx, siteKey)
	if err != nil {
		return nil, err
	}
	return got, nil
}

// SiteByKey 按公开 site_key 查站点。
func (d *DB) SiteByKey(ctx context.Context, siteKey string) (*Site, error) {
	return d.querySite(ctx, `SELECT id, name, site_key, secret_mac, pow_bits FROM sites WHERE site_key=?`, siteKey)
}

// SiteByID 按内部 id 查站点。
func (d *DB) SiteByID(ctx context.Context, id int64) (*Site, error) {
	return d.querySite(ctx, `SELECT id, name, site_key, secret_mac, pow_bits FROM sites WHERE id=?`, id)
}

func (d *DB) querySite(ctx context.Context, q string, arg any) (*Site, error) {
	var (
		id, bits int64
		macHex   string
		s        = &Site{}
	)
	err := d.h.QueryRowContext(ctx, q, arg).Scan(&id, &s.Name, &s.SiteKey, &macHex, &bits)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查站点: %w", err)
	}
	mac, err := hex.DecodeString(macHex)
	if err != nil {
		return nil, fmt.Errorf("store: 站点摘要损坏: %w", err)
	}
	s.ID, s.SecretMac, s.PowBits = id, mac, uint8(bits)
	return s, nil
}

// MatchSecret 常数时间比较调用方 secret 与站点摘要。
// 阶段 1 站点数量来自配置（很小），逐个比较的代价可忽略；阶段 2 站点规模变大时需重新评估此处的时序面。
func (d *DB) MatchSecret(site *Site, secret string) bool {
	return subtle.ConstantTimeCompare(d.SecretMac(secret), site.SecretMac) == 1
}

// FindSiteBySecret 用于 /siteverify：先由 secret 定位站点，再做任何状态消费。
// 找不到即返回 ErrNotFound，调用方不得据此触碰 token 状态（DESIGN §10.1）。
func (d *DB) FindSiteBySecret(ctx context.Context, secret string) (*Site, error) {
	rows, err := d.h.QueryContext(ctx, `SELECT id, name, site_key, secret_mac, pow_bits FROM sites`)
	if err != nil {
		return nil, fmt.Errorf("store: 遍历站点: %w", err)
	}
	defer rows.Close()
	want := d.SecretMac(secret)
	for rows.Next() {
		var (
			id, bits int64
			macHex   string
			s        = &Site{}
		)
		if err := rows.Scan(&id, &s.Name, &s.SiteKey, &macHex, &bits); err != nil {
			return nil, fmt.Errorf("store: 扫描站点: %w", err)
		}
		mac, err := hex.DecodeString(macHex)
		if err != nil {
			return nil, fmt.Errorf("store: 站点摘要损坏: %w", err)
		}
		// 遍历所有行而非提前返回，避免通过响应时间区分"命中第几个站点"。
		if subtle.ConstantTimeCompare(want, mac) == 1 {
			s.ID, s.SecretMac, s.PowBits = id, mac, uint8(bits)
			return s, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历站点: %w", err)
	}
	return nil, ErrNotFound
}

// NewID 生成 32 字符小写 hex 标识（challenge id / response id）。
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: 生成 id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// InsertChallenge 以 status=issued 落库。PoW 参数由此成为服务端权威记录。
// 写入与"活动 challenge 是否超限"在同一条语句内完成：这是签发侧的硬上限，
// 限流器只控制速率、不控制存量，两者都不能单独保证 SQLite 活动记录有界。
func (d *DB) InsertChallenge(ctx context.Context, c *Challenge, caps Caps) error {
	if c == nil || c.ID == "" || c.SiteID <= 0 || len(c.PowSeed) == 0 || c.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: challenge 字段不完整", ErrBadConfig)
	}
	if caps.MaxActive <= 0 || caps.MaxActivePerSite <= 0 {
		return fmt.Errorf("%w: challenge 上限必须为正（全局=%d 单站=%d）",
			ErrBadConfig, caps.MaxActive, caps.MaxActivePerSite)
	}
	res, err := d.h.ExecContext(ctx, `
INSERT INTO challenges(id, site_id, pow_seed, pow_bits, status, attempts, created_at, expires_at)
SELECT ?,?,?,?,?,?,?,?
WHERE (SELECT count(*) FROM challenges WHERE status=? AND expires_at>?) < ?
  AND (SELECT count(*) FROM challenges WHERE site_id=? AND status=? AND expires_at>?) < ?`,
		c.ID, c.SiteID, c.PowSeed, int(c.PowBits), statusIssued, 0,
		formatTime(c.CreatedAt.UTC()), formatTime(c.ExpiresAt.UTC()),
		statusIssued, formatTime(c.CreatedAt.UTC()), caps.MaxActive,
		c.SiteID, statusIssued, formatTime(c.CreatedAt.UTC()), caps.MaxActivePerSite)
	if err != nil {
		return fmt.Errorf("store: 写入 challenge: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// 全局或单站活动量已达上限：拒绝继续扩大账本，对外表现为限流。
		return ErrCapExceeded
	}
	return nil
}

// Challenge 读取 challenge 权威记录。
func (d *DB) Challenge(ctx context.Context, id string) (*Challenge, error) {
	var (
		c        = &Challenge{}
		created  string
		expires  string
		redeemed sql.NullString
		status   string
		siteID   int64
	)
	err := d.h.QueryRowContext(ctx, `
SELECT id, site_id, pow_seed, pow_bits, status, attempts, created_at, expires_at, redeemed_at
FROM challenges WHERE id=?`, id).
		Scan(&c.ID, &siteID, &c.PowSeed, &c.PowBits, &status, &c.Attempts, &created, &expires, &redeemed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读 challenge: %w", err)
	}
	c.SiteID, c.Status = siteID, status
	if c.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if c.ExpiresAt, err = parseTime(expires); err != nil {
		return nil, err
	}
	if redeemed.Valid {
		if c.RedeemedAt, err = parseTime(redeemed.String); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Redeem 原子地把 issued 且未过期的 challenge 置为 redeemed，并在同一事务内签发 response。
// 满足 ADR-004 的四条保证：条件更新、同事务、检查影响行数、由测试证明并发与重启语义。
func (d *DB) Redeem(ctx context.Context, challengeID string, now time.Time, ttl time.Duration) (*Response, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: response TTL 必须为正", ErrBadConfig)
	}
	tx, err := d.h.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 兑换事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
UPDATE challenges SET status=?, redeemed_at=?
WHERE id=? AND status=? AND expires_at>?`,
		statusRedeemed, formatTime(now.UTC()), challengeID, statusIssued, formatTime(now.UTC()))
	if err != nil {
		return nil, fmt.Errorf("store: 条件更新 challenge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: 读影响行数: %w", err)
	}
	if n != 1 {
		// 不存在、已兑换、已作废或已过期——对外一律同一结果，不给探测信号。
		return nil, ErrNotRedeemable
	}
	rid, err := NewID()
	if err != nil {
		return nil, err
	}
	created := now.UTC()
	expires := created.Add(ttl)
	var siteID int64
	err = tx.QueryRowContext(ctx, `SELECT site_id FROM challenges WHERE id=?`, challengeID).Scan(&siteID)
	if err != nil {
		return nil, fmt.Errorf("store: 回读站点: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO responses(id, challenge_id, site_id, created_at, expires_at, used) VALUES(?,?,?,?,?,0)`,
		rid, challengeID, siteID, formatTime(created), formatTime(expires)); err != nil {
		return nil, fmt.Errorf("store: 签发 response: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交兑换: %w", err)
	}
	return &Response{ID: rid, ChallengeID: challengeID, SiteID: siteID, CreatedAt: created, ExpiresAt: expires}, nil
}

// FailAttempt 记录一次失败兑换：达到上限即作废该 challenge。
// 与 Redeem 不同，这里不产生对外成功结果，因此允许非事务的单条条件更新。
func (d *DB) FailAttempt(ctx context.Context, challengeID string) (attempts int, status string, err error) {
	res, err := d.h.ExecContext(ctx, `
UPDATE challenges
SET attempts = attempts + 1,
    status = CASE WHEN attempts + 1 >= ? THEN ? ELSE status END
WHERE id=? AND status=?`, MaxFailedAttempts, statusInvalid, challengeID, statusIssued)
	if err != nil {
		return 0, "", fmt.Errorf("store: 记录失败尝试: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, "", ErrNotFound
	}
	var cur sql.NullInt64
	err = d.h.QueryRowContext(ctx, `SELECT attempts, status FROM challenges WHERE id=?`, challengeID).Scan(&cur, &status)
	if err != nil {
		return 0, "", fmt.Errorf("store: 回读尝试次数: %w", err)
	}
	return int(cur.Int64), status, nil
}

// ConsumeResponse 原子地把未过期的 response 置为已使用并回读事实。
// siteID 一致性问题在这里就拦下：错误 secret 定位到的站点无法消费别的站点的 token。
func (d *DB) ConsumeResponse(ctx context.Context, responseID string, siteID int64, now time.Time) (*Verification, error) {
	tx, err := d.h.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 验证事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
UPDATE responses SET used=1
WHERE id=? AND site_id=? AND used=0 AND expires_at>?`,
		responseID, siteID, formatTime(now.UTC()))
	if err != nil {
		return nil, fmt.Errorf("store: 条件更新 response: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrConsumed
	}
	v := &Verification{SiteID: siteID}
	var created string
	err = tx.QueryRowContext(ctx, `
SELECT r.challenge_id, c.created_at
FROM responses r JOIN challenges c ON c.id = r.challenge_id
WHERE r.id=? AND r.site_id=?`, responseID, siteID).Scan(&v.ChallengeID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConsumed
	}
	if err != nil {
		return nil, fmt.Errorf("store: 回读 challenge: %w", err)
	}
	if v.ChallengeCreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交验证: %w", err)
	}
	return v, nil
}

// Purge 删除已消费或早已失效的账本行。防重放语义不依赖它——它只限制库体积。
// 删除条件必须与外键相容：responses.challenge_id 引用 challenges.id，而"被兑换但尚未验证"
// 的 response 其有效期可以晚于所属 challenge，此时删掉 challenge 会被外键挡下（阶段 1 实测报错）。
// 因此 challenge 只在**没有任何 response 行再引用它**时删除；仍有效的 response 行一律保留。
// 每批行数有上限：清理跑在单写连接上，1 vCPU 机器不能被一次全表删除长时间占住。
func (d *DB) Purge(ctx context.Context, now time.Time) (responses, challenges int64, err error) {
	limit := strconv.Itoa(purgeBatchRows)
	ts := formatTime(now.UTC())
	tx, err := d.h.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("store: 清理事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	r1, err := tx.ExecContext(ctx, `
DELETE FROM responses WHERE id IN (
  SELECT id FROM responses WHERE used=1 OR expires_at < ? LIMIT ?)`, ts, limit)
	if err != nil {
		return 0, 0, fmt.Errorf("store: 清理 responses: %w", err)
	}
	responses, _ = r1.RowsAffected()

	r2, err := tx.ExecContext(ctx, `
DELETE FROM challenges WHERE id IN (
  SELECT id FROM challenges
  WHERE status<>? AND expires_at < ?
    AND NOT EXISTS (SELECT 1 FROM responses r WHERE r.challenge_id = challenges.id)
  LIMIT ?)`, statusIssued, ts, limit)
	if err != nil {
		return responses, 0, fmt.Errorf("store: 清理 challenges: %w", err)
	}
	challenges, _ = r2.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("store: 提交清理: %w", err)
	}
	return responses, challenges, nil
}

// CountChallenges 供验收脚本与测试断言状态分布。
func (d *DB) CountChallenges(ctx context.Context, status string) (int, error) {
	var n int
	err := d.h.QueryRowContext(ctx, `SELECT count(*) FROM challenges WHERE status=?`, status).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: 统计 challenge: %w", err)
	}
	return n, nil
}

func formatTime(t time.Time) string { return t.Format(timeLayout) }

// parseTime 兼容两种写法：本版本定宽写出，阶段 1 的行可能是 RFC3339Nano 裁剪过尾零的。
// 只回退一次格式、不做"猜时间"的宽容解析——字符串损坏仍必须报错而不是当成零点。
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339Nano, s); err2 == nil {
			return t2.UTC(), nil
		}
		return time.Time{}, fmt.Errorf("store: 时间格式损坏 %q: %w", s, err)
	}
	return t.UTC(), nil
}
