package store

// 阶段 2 的凭据生命周期（DESIGN §5.3、§6）。
//
// 撤销用**一个时间戳列**表达，两种轮换方式因此共用同一套判定：
//   - 立即撤销：revoked_at = now
//   - 过渡期撤销：revoked_at = now + grace（grace 有硬上限）
// 判定条件是 `revoked_at IS NULL OR revoked_at > now`，纯读库、无需后台任务，
// 而且**重启后仍然成立**——撤销状态是库里的行，不是内存里的标记。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// SiteSecret 是一把验证凭据的服务端视图：**不含明文**，明文只在创建时出现过一次。
type SiteSecret struct {
	KeyID     string
	SiteID    int64
	CreatedAt time.Time
	RevokedAt time.Time // 零值 = 永不过期（当前有效）
}

// AdminKey 是一把管理凭据的服务端视图。
type AdminKey struct {
	KeyID     string
	Label     string
	CreatedAt time.Time
	RevokedAt time.Time
}

// adminMacInput 给管理密钥摘要加域分隔：站点 secret 与 admin key 即使明文相同也不会得到相同摘要。
const adminMacInput = "epocha/v1|admin|"

// 站点 secret 的摘要算法保持阶段 1 不变（就是 SecretMac，不加域分隔串）：
// 0002 迁移要把 sites.secret_mac 的值原样搬进 site_secrets，改算法会让升级后的旧密钥全部失效。
// 管理凭据是新东西，所以它单独用域分隔串（见 AdminKeyMac）。理由见 ADR-010。

// AdminKeyMac 计算管理凭据摘要（带域分隔）。
func (d *DB) AdminKeyMac(key string) []byte {
	m := sha256.Sum256([]byte(adminMacInput + key))
	return d.SecretMac(string(m[:]))
}

// migrateStage2Data 是 0002 迁移的数据步骤：
//  1. 把阶段 1 每站点一把的 sites.secret_mac 原样搬进 site_secrets（key_id='legacy'）；
//  2. 把 sites.secret_mac 中和成 HMAC(pepper, "retired|" + 旧摘要)。
//     中和是必要的：SQLite 删不掉带 UNIQUE 约束的列（实测），留着原值就等于留第二个真相源，
//     将来任何一段误读它的代码都会让"已撤销的密钥"重新可用。
func migrateStage2Data(ctx context.Context, tx *sql.Tx, pepper []byte) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, secret_mac FROM sites`)
	if err != nil {
		return fmt.Errorf("迁移：读站点: %w", err)
	}
	type pair struct {
		id  int64
		mac string
	}
	list := []pair{}
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.mac); err != nil {
			rows.Close()
			return fmt.Errorf("迁移：扫描站点: %w", err)
		}
		list = append(list, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("迁移：遍历站点: %w", err)
	}
	rows.Close()

	for _, p := range list {
		// key_id 全局唯一，所以迁移行用"legacy-<站点 id>"这种确定性公开标识（阶段 1 每站点只有一把）。
		if _, err := tx.ExecContext(ctx, `
INSERT INTO site_secrets(key_id, site_id, secret_mac, created_at)
SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM site_secrets WHERE key_id=?)`,
			"legacy-"+fmt.Sprint(p.id), p.id, p.mac, formatTime(time.Now().UTC()), "legacy-"+fmt.Sprint(p.id)); err != nil {
			return fmt.Errorf("迁移：登记旧凭据: %w", err)
		}
		mac, err := hex.DecodeString(p.mac)
		if err != nil {
			return fmt.Errorf("迁移：旧摘要不是 hex（站点 %d）", p.id)
		}
		h := macWithKey(pepper, retiredPrefix+hex.EncodeToString(mac))
		if _, err := tx.ExecContext(ctx,
			`UPDATE sites SET secret_mac=? WHERE id=?`, hex.EncodeToString(h), p.id); err != nil {
			return fmt.Errorf("迁移：中和旧列: %w", err)
		}
	}
	return nil
}

// macWithKey 是 HMAC-SHA256(pepper, msg)，供包内需要"带 pepper 的摘要"但不属于
// DB 实例语义的地方使用（迁移步骤里 tx 与 pepper 都是参数，拿不到 d）。
func macWithKey(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// --- 验证凭据 ---

// AddSecret 为站点新增一把验证凭据，返回明文（仅此一次）。
// 同时有效的凭据数有上限：过渡期本该是短暂的，无限并存等于把撤销做成不可达的操作。
func (d *DB) AddSecret(ctx context.Context, siteID int64) (*SiteSecret, string, error) {
	if _, err := d.SiteByID(ctx, siteID); err != nil {
		return nil, "", err
	}
	var active int
	if err := d.h.QueryRowContext(ctx,
		`SELECT count(*) FROM site_secrets WHERE site_id=? AND revoked_at IS NULL`, siteID).Scan(&active); err != nil {
		return nil, "", fmt.Errorf("store: 统计凭据: %w", err)
	}
	if active >= MaxSecretsPerSite {
		return nil, "", fmt.Errorf("%w: 同时有效的凭据数上限为 %d", ErrBadConfig, MaxSecretsPerSite)
	}
	secret, err := NewSecret()
	if err != nil {
		return nil, "", err
	}
	keyID, err := NewKeyID()
	if err != nil {
		return nil, "", err
	}
	if _, err := d.h.ExecContext(ctx,
		`INSERT INTO site_secrets(key_id, site_id, secret_mac, created_at) VALUES(?,?,?,?)`,
		keyID, siteID, hex.EncodeToString(d.SecretMac(secret)), formatTime(time.Now().UTC())); err != nil {
		if isUniqueErr(err) {
			return nil, "", fmt.Errorf("%w: key_id 或摘要冲突", ErrConflict)
		}
		return nil, "", fmt.Errorf("store: 登记凭据: %w", err)
	}
	got, err := d.SecretByKey(ctx, siteID, keyID)
	if err != nil {
		return nil, "", err
	}
	return got, secret, nil
}

// SecretByKey 读一把凭据的元数据（不含任何摘要或明文）。
func (d *DB) SecretByKey(ctx context.Context, siteID int64, keyID string) (*SiteSecret, error) {
	var (
		s          = &SiteSecret{SiteID: siteID, KeyID: keyID}
		created    string
		revoked    sql.NullString
		revokedStr string
		err        error
	)
	err = d.h.QueryRowContext(ctx,
		`SELECT created_at, revoked_at FROM site_secrets WHERE key_id=? AND site_id=?`, keyID, siteID).
		Scan(&created, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读凭据: %w", err)
	}
	if s.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if revoked.Valid {
		if revokedStr, err = revoked.String, error(nil); err != nil {
			return nil, err
		}
		if s.RevokedAt, err = parseTime(revokedStr); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ListSecrets 列出站点的全部凭据元数据。
func (d *DB) ListSecrets(ctx context.Context, siteID int64) ([]*SiteSecret, error) {
	rows, err := d.h.QueryContext(ctx, `
SELECT key_id, created_at, revoked_at FROM site_secrets WHERE site_id=? ORDER BY created_at, key_id`, siteID)
	if err != nil {
		return nil, fmt.Errorf("store: 列凭据: %w", err)
	}
	defer rows.Close()
	out := []*SiteSecret{}
	for rows.Next() {
		var (
			keyID, created string
			revoked        sql.NullString
			s              = &SiteSecret{SiteID: siteID}
		)
		if err := rows.Scan(&keyID, &created, &revoked); err != nil {
			return nil, fmt.Errorf("store: 扫描凭据: %w", err)
		}
		s.KeyID = keyID
		if s.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if revoked.Valid {
			if s.RevokedAt, err = parseTime(revoked.String); err != nil {
				return nil, err
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeSecret 撤销一把凭据：at<=now 即立即失效，at>now 即过渡期（到点自动失效）。
// 已撤销的凭据重复撤销返回当前视图，不报错——轮换脚本可安全重跑。
func (d *DB) RevokeSecret(ctx context.Context, siteID int64, keyID string, at time.Time) (*SiteSecret, error) {
	if _, err := d.SecretByKey(ctx, siteID, keyID); err != nil {
		return nil, err
	}
	res, err := d.h.ExecContext(ctx,
		`UPDATE site_secrets SET revoked_at=? WHERE key_id=? AND site_id=?`,
		formatTime(at.UTC()), keyID, siteID)
	if err != nil {
		return nil, fmt.Errorf("store: 撤销凭据: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrNotFound
	}
	return d.SecretByKey(ctx, siteID, keyID)
}

// RotateSecret 一次完成"新建 + 处理旧密钥"：transition=0 表示立即撤销旧密钥，
// 否则旧密钥在过渡期内仍可用（上限 MaxTransitionSecond）。
// 撤销只针对**当前有效**的凭据，不动已经失效的历史行。
func (d *DB) RotateSecret(ctx context.Context, siteID int64, transition time.Duration, now time.Time) (*SiteSecret, string, []*SiteSecret, error) {
	if transition < 0 || transition > MaxTransitionSecond*time.Second {
		return nil, "", nil, fmt.Errorf("%w: 过渡期须在 0..%d 秒", ErrBadConfig, MaxTransitionSecond)
	}
	fresh, secret, err := d.AddSecret(ctx, siteID)
	if err != nil {
		return nil, "", nil, err
	}
	current, err := d.ListSecrets(ctx, siteID)
	if err != nil {
		return nil, "", nil, err
	}
	until := now.UTC()
	if transition > 0 {
		until = now.UTC().Add(transition)
	}
	revoked := []*SiteSecret{}
	for _, s := range current {
		if s.KeyID == fresh.KeyID || !s.RevokedAt.IsZero() {
			continue
		}
		got, err := d.RevokeSecret(ctx, siteID, s.KeyID, until)
		if err != nil {
			return nil, "", nil, err
		}
		revoked = append(revoked, got)
	}
	return fresh, secret, revoked, nil
}

// FindSiteBySecret 由 secret 定位站点：先鉴凭据、后碰状态（DESIGN §10.1）。
// 扫描该表全部行而不是提前返回，避免通过响应时间区分"命中第几行"；
// 已撤销/到点的行**不参与匹配**，因此错误或被撤销的 secret 无法消费任何合法 response。
// FindSiteBySecret 由 secret 定位站点：先鉴凭据、后碰状态（DESIGN §10.1）。
// 扫描整张表而不是提前返回，避免通过响应时间区分"命中第几行"；
// 已撤销或已到点的行**不参与匹配**，因此错误 secret 与被撤销的 secret 都无法消费合法 response。
func (d *DB) FindSiteBySecret(ctx context.Context, secret string, now time.Time) (*Site, error) {
	rows, err := d.h.QueryContext(ctx, `
SELECT site_id, secret_mac FROM site_secrets
WHERE revoked_at IS NULL OR revoked_at > ?`, formatTime(now.UTC()))
	if err != nil {
		return nil, fmt.Errorf("store: 遍历凭据: %w", err)
	}
	defer rows.Close()

	want := d.SecretMac(secret)
	siteID := int64(0)
	for rows.Next() {
		var (
			id     int64
			macHex string
		)
		if err := rows.Scan(&id, &macHex); err != nil {
			return nil, fmt.Errorf("store: 扫描凭据: %w", err)
		}
		mac, err := hex.DecodeString(macHex)
		if err != nil {
			return nil, fmt.Errorf("store: 凭据摘要损坏: %w", err)
		}
		if subtle.ConstantTimeCompare(want, mac) == 1 && siteID == 0 {
			siteID = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历凭据: %w", err)
	}
	if siteID == 0 {
		return nil, ErrNotFound
	}
	return d.SiteByID(ctx, siteID)
}

// --- 管理凭据 ---

// BootstrapAdminKey 在库中**没有任何管理密钥记录时**登记 env 带来的那把（DESIGN §6）。
// 有记录后返回 already=true，调用方必须忽略环境变量——否则运维清空 env 的意图会被重启撤销。
func (d *DB) BootstrapAdminKey(ctx context.Context, key, label string) (*AdminKey, bool, error) {
	if len(key) < MinSecretLen {
		return nil, false, fmt.Errorf("%w: 管理凭据短于 %d 字符", ErrBadConfig, MinSecretLen)
	}
	var n int
	if err := d.h.QueryRowContext(ctx, `SELECT count(*) FROM admin_keys`).Scan(&n); err != nil {
		return nil, false, fmt.Errorf("store: 统计管理凭据: %w", err)
	}
	if n > 0 {
		return nil, true, nil
	}
	id, err := d.AddAdminKey(ctx, label, key)
	if err != nil {
		return nil, false, err
	}
	return id, false, nil
}

// AddAdminKey 登记一把管理凭据（明文只由调用方在创建响应里展示一次）。
func (d *DB) AddAdminKey(ctx context.Context, label, key string) (*AdminKey, error) {
	if !printableOK(label) || len(label) > MaxNameLen {
		return nil, fmt.Errorf("%w: 管理凭据标签须为 1..%d 个可见字符", ErrBadConfig, MaxNameLen)
	}
	if len(key) < MinSecretLen {
		return nil, fmt.Errorf("%w: 管理凭据短于 %d 字符", ErrBadConfig, MinSecretLen)
	}
	keyID, err := NewKeyID()
	if err != nil {
		return nil, err
	}
	if _, err := d.h.ExecContext(ctx,
		`INSERT INTO admin_keys(key_id, label, key_mac, created_at) VALUES(?,?,?,?)`,
		keyID, label, hex.EncodeToString(d.AdminKeyMac(key)), formatTime(time.Now().UTC())); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("%w: 管理凭据标签已存在", ErrConflict)
		}
		return nil, fmt.Errorf("store: 登记管理凭据: %w", err)
	}
	return d.AdminKeyByKey(ctx, keyID)
}

// NewAdminKeyWithID 生成一把管理凭据并登记，返回明文（只在创建时可见）。
func (d *DB) NewAdminKeyWithID(ctx context.Context, label string) (*AdminKey, string, error) {
	key, err := NewAdminKey()
	if err != nil {
		return nil, "", err
	}
	id, err := d.AddAdminKey(ctx, label, key)
	if err != nil {
		return nil, "", err
	}
	return id, key, nil
}

func (d *DB) AdminKeyByKey(ctx context.Context, keyID string) (*AdminKey, error) {
	var (
		a       = &AdminKey{KeyID: keyID}
		created string
		revoked sql.NullString
		label   string
		err     error
	)
	err = d.h.QueryRowContext(ctx,
		`SELECT label, created_at, revoked_at FROM admin_keys WHERE key_id=?`, keyID).
		Scan(&label, &created, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读管理凭据: %w", err)
	}
	a.Label = label
	if a.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if revoked.Valid {
		if a.RevokedAt, err = parseTime(revoked.String); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// ListAdminKeys 只返回元数据：标签、时间与撤销时刻。不含摘要、更不含明文。
func (d *DB) ListAdminKeys(ctx context.Context) ([]*AdminKey, error) {
	rows, err := d.h.QueryContext(ctx,
		`SELECT key_id, label, created_at, revoked_at FROM admin_keys ORDER BY created_at, key_id`)
	if err != nil {
		return nil, fmt.Errorf("store: 列管理凭据: %w", err)
	}
	defer rows.Close()
	out := []*AdminKey{}
	for rows.Next() {
		var (
			keyID, label, created string
			revoked               sql.NullString
			a                     = &AdminKey{}
		)
		if err := rows.Scan(&keyID, &label, &created, &revoked); err != nil {
			return nil, fmt.Errorf("store: 扫描管理凭据: %w", err)
		}
		a.KeyID, a.Label = keyID, label
		if a.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if revoked.Valid {
			if a.RevokedAt, err = parseTime(revoked.String); err != nil {
				return nil, err
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RevokeAdminKey 撤销（at<=now 立即失效，at>now 为过渡期）。
func (d *DB) RevokeAdminKey(ctx context.Context, keyID string, at time.Time) (*AdminKey, error) {
	if _, err := d.AdminKeyByKey(ctx, keyID); err != nil {
		return nil, err
	}
	res, err := d.h.ExecContext(ctx,
		`UPDATE admin_keys SET revoked_at=? WHERE key_id=?`, formatTime(at.UTC()), keyID)
	if err != nil {
		return nil, fmt.Errorf("store: 撤销管理凭据: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrNotFound
	}
	return d.AdminKeyByKey(ctx, keyID)
}

// MatchAdminKey 常数时间比对管理凭据，返回命中的 key_id（用于审计 actor）。
// 遍历全部行、不因命中而提前返回，避免用时序定位"是第几条记录"。
func (d *DB) MatchAdminKey(ctx context.Context, key string, now time.Time) (string, error) {
	rows, err := d.h.QueryContext(ctx, `
SELECT key_id, key_mac FROM admin_keys WHERE revoked_at IS NULL OR revoked_at > ?`, formatTime(now.UTC()))
	if err != nil {
		return "", fmt.Errorf("store: 遍历管理凭据: %w", err)
	}
	defer rows.Close()
	want := d.AdminKeyMac(key)
	hit := ""
	for rows.Next() {
		var keyID, macHex string
		if err := rows.Scan(&keyID, &macHex); err != nil {
			return "", fmt.Errorf("store: 扫描管理凭据: %w", err)
		}
		mac, err := hex.DecodeString(macHex)
		if err != nil {
			return "", fmt.Errorf("store: 管理凭据摘要损坏: %w", err)
		}
		if subtle.ConstantTimeCompare(want, mac) == 1 {
			hit = keyID
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("store: 遍历管理凭据: %w", err)
	}
	if hit == "" {
		return "", ErrNotFound
	}
	return hit, nil
}
