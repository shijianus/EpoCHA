package store

// 最小审计（DESIGN §5.3、§10.2）。
//
// 记录的是"谁在什么时候对哪个对象做了什么、成不成功"，**不是**请求内容：
// 不写 secret、不写 Bearer token、不写答案、不写完整请求体。actor 是管理密钥的公开 key_id。
// 保留期由清理任务按天裁剪，避免审计自己变成另一个无界增长源。

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// AuditRetentionDays 是审计行保留天数 [目标]：够一次事后追查，又不至于让库无限膨胀。
const AuditRetentionDays = 90

// AuditEntry 是一条审计记录。
type AuditEntry struct {
	ID        int64
	Timestamp time.Time
	Actor     string
	Action    string
	Target    string
	Success   bool
}

// AppendAudit 独立写入审计，供失败记录与已生效的撤销/停用收尾使用。
// 新建一次性凭据的成功审计必须走对应 Audited 方法，与业务同事务提交（ADR-021）。
func (d *DB) AppendAudit(ctx context.Context, actor, action, target string, success bool) error {
	return appendAudit(ctx, d.h, actor, action, target, success)
}

// guardAuditedActor takes the SQLite write slot before reading actor validity.
// The no-op meta update makes revoke and credential creation serialize at the
// same write boundary: whichever transaction gets the slot first determines
// whether the actor is still current. A handler-level authentication snapshot
// cannot provide that ordering once a request body was in flight.
func guardAuditedActor(ctx context.Context, tx *sql.Tx, actor string) error {
	if !printableOK(actor) || len(actor) > MaxNameLen {
		return fmt.Errorf("%w: 审计 actor 不合法", ErrBadConfig)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET v=v WHERE k='schema_version'`); err != nil {
		return fmt.Errorf("store: 锁定 actor 校验写事务: %w", err)
	}
	var present int
	if err := tx.QueryRowContext(ctx, `
SELECT 1 FROM admin_keys
WHERE key_id=? AND (revoked_at IS NULL OR revoked_at > ?)
`, actor, formatTime(time.Now().UTC())).Scan(&present); err != nil {
		if err == sql.ErrNoRows {
			return ErrRevoked
		}
		return fmt.Errorf("store: 校验管理 actor: %w", err)
	}
	return nil
}

func appendAudit(ctx context.Context, h interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, actor, action, target string, success bool) error {
	if !printableOK(actor) || len(actor) > MaxNameLen {
		return fmt.Errorf("%w: 审计 actor 不合法", ErrBadConfig)
	}
	if !printableOK(action) || len(action) > MaxNameLen {
		return fmt.Errorf("%w: 审计 action 不合法", ErrBadConfig)
	}
	if !printableOK(target) || len(target) > 256 {
		return fmt.Errorf("%w: 审计 target 不合法", ErrBadConfig)
	}
	if _, err := h.ExecContext(ctx,
		`INSERT INTO audit(ts, actor, action, target, success) VALUES(?,?,?,?,?)`,
		formatTime(time.Now().UTC()), actor, action, target, b2i(success)); err != nil {
		return fmt.Errorf("store: 写审计: %w", err)
	}
	return nil
}

// ListAudit 以 id 倒序游标分页（最新在前）。beforeID=0 表示从最新开始。
// limit 上限 500：分页参数是公开可影响的读放大面，必须有硬边界。
func (d *DB) ListAudit(ctx context.Context, beforeID int64, limit int) ([]*AuditEntry, error) {
	limit, err := pageSize(limit, 50, 500)
	if err != nil {
		return nil, err
	}
	if beforeID < 0 {
		return nil, fmt.Errorf("%w: 游标为负", ErrBadConfig)
	}
	// before_id=0 表示"从最新开始"，用一个足够大的游标代替（而不是把 0 当上界，那会返回空页）
	cursor := beforeID
	if cursor == 0 {
		cursor = int64(1) << 62
	}
	rows, err := d.h.QueryContext(ctx, `
SELECT id, ts, actor, action, target, success FROM audit
WHERE id < ? ORDER BY id DESC LIMIT ?`, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 列审计: %w", err)
	}
	defer rows.Close()
	out := []*AuditEntry{}
	for rows.Next() {
		var (
			e       = &AuditEntry{}
			ts      string
			success int64
		)
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.Target, &success); err != nil {
			return nil, fmt.Errorf("store: 扫描审计: %w", err)
		}
		e.Success = success == 1
		if e.Timestamp, err = parseTime(ts); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeAudit 删除保留期之外的审计行，每批行数与账本清理同界。
func (d *DB) PurgeAudit(ctx context.Context, now time.Time) (int64, error) {
	cutoff := formatTime(now.UTC().AddDate(0, 0, -AuditRetentionDays))
	res, err := d.h.ExecContext(ctx, `
DELETE FROM audit WHERE id IN (SELECT id FROM audit WHERE ts < ? LIMIT ?)`,
		cutoff, strconv.Itoa(purgeBatchRows))
	if err != nil {
		return 0, fmt.Errorf("store: 清理审计: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
