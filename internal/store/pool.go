package store

// 阶段 3A 的题池与"题目 ↔ challenge"绑定（DESIGN §5.4、§8.4，ADR-011）。
//
// 三条不变量决定了这里的形状：
//  1. 一条题池记录只能分配给一个 challenge —— pool.challenge_id 上的 partial UNIQUE 索引 + 条件更新。
//  2. 已分配的题目**永不回到 ready**（断线、超时、答案错误都不回池）：回池等于让同一题面面对多个挑战者。
//  3. 题目分配与 challenge 行创建在**同一事务**提交：否则会出现"题被占走但没有对应 challenge"的孤儿绑定。
//     生成（渲染、熵消费）一律在事务外完成，写事务里只有 SQL。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 题池状态。ready → assigned → terminal 是**单向**的，terminal 不可回退。
const (
	PoolReady    = "ready"
	PoolAssigned = "assigned"
	PoolTerminal = "terminal"
)

// claimAttempts 是一次领取里"选候选 → 条件更新"的重试上限。
// 它的作用是**让这个循环有界**，不是正确性依赖（正确性来自"同一事务内的条件更新 + 影响行数判定"）。
// 两种并发形态的实际结果不同，都已实测（TestOneQuestionPerChallengeTwoHandles /
// TestStaleSnapshotClaimFailsClosed）：
//   - 同一进程：SetMaxOpenConns(1) 让事务在唯一连接上串行化，RowsAffected==0 分支实际走不到；
//   - 两个独立 *sql.DB（多进程，非部署形态）：读快照过期的那一方在 UPDATE 上拿到
//     SQLITE_BUSY("database is locked")，整个事务回滚 ⇒ fail closed。既不会静默覆盖别人的绑定，
//     也不会留下"题已 assigned 但没有 challenge"的孤儿；调用方得到 500（可重试），
//     绝不会退化成"那就发一张不带题的 challenge"。
const claimAttempts = 5

// ErrPoolEmpty 是"池里没有满足本站策略的题"。它是**可重试**的资源状态，
// 绝不能被处理成"那就只发 PoW"——降级会让站点声明的策略静默失效（ADR-011 #7）。
var ErrPoolEmpty = errors.New("store: 题池无可用题目")

// PoolSpec 决定一条题能不能发给某个站点：按 (generator, version, difficulty) 分桶，
// 而不是按站点分桶——题目内容与站点无关，站点差异只在策略选择上。
type PoolSpec struct {
	GeneratorID string
	Version     string
	Difficulty  int
}

// ValidateSpec 是对外暴露的分桶键校验（池 worker 与站点配置都用它）。
func (s PoolSpec) ValidateSpec() error { return s.validate() }

func (s PoolSpec) validate() error {
	switch {
	case s.GeneratorID == "" || len(s.GeneratorID) > 64:
		return fmt.Errorf("%w: generator_id 不合法", ErrBadConfig)
	case s.Version == "" || len(s.Version) > 32:
		return fmt.Errorf("%w: generator version 不合法", ErrBadConfig)
	case s.Difficulty < MinDifficulty || s.Difficulty > MaxDifficulty:
		return fmt.Errorf("%w: difficulty=%d 须在 %d..%d", ErrBadConfig, s.Difficulty, MinDifficulty, MaxDifficulty)
	}
	return nil
}

// PoolQuestion 是一条题池记录。PayloadJSON 是可以下发的公开题面；
// Verifier 是服务端私有判分数据，**永不下发、永不写日志**（红线 5）。
type PoolQuestion struct {
	ID          string
	GeneratorID string
	Version     string
	Difficulty  int
	Status      string
	PayloadJSON string
	Verifier    []byte
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ChallengeID string
	AssignedAt  time.Time
	TerminalAt  time.Time
}

// InsertQuestion 写入一条 ready 题。上限判定与插入在同一条语句里完成（与 challenge 存量上限同一手法）：
// 池的行数上限是资源边界，不能靠"先查再插"的近似。
func (d *DB) InsertQuestion(ctx context.Context, q *PoolQuestion, maxRows int) error {
	if q == nil || q.ID == "" || q.PayloadJSON == "" || len(q.Verifier) == 0 || q.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: 题目字段不完整", ErrBadConfig)
	}
	if err := q.Spec().validate(); err != nil {
		return err
	}
	if maxRows <= 0 {
		return fmt.Errorf("%w: 题池上限必须为正", ErrBadConfig)
	}
	if len(q.PayloadJSON) > MaxPayloadBytes || len(q.Verifier) > MaxVerifierBytes {
		return fmt.Errorf("%w: 题目过大（payload=%d verifier=%d 字节，上限 %d/%d）",
			ErrBadConfig, len(q.PayloadJSON), len(q.Verifier), MaxPayloadBytes, MaxVerifierBytes)
	}
	res, err := d.h.ExecContext(ctx, `
INSERT INTO pool(id, generator_id, version, difficulty, status, payload_json, verifier_blob, created_at, expires_at)
SELECT ?,?,?,?,?,?,?,?,?
WHERE (SELECT count(*) FROM pool) < ?`,
		q.ID, q.GeneratorID, q.Version, q.Difficulty, PoolReady, q.PayloadJSON, q.Verifier,
		formatTime(q.CreatedAt.UTC()), formatTime(q.ExpiresAt.UTC()), maxRows)
	if err != nil {
		return fmt.Errorf("store: 写入题目: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrPoolFull
	}
	return nil
}

// Spec 返回这道题所属的分桶键。
func (q *PoolQuestion) Spec() PoolSpec {
	return PoolSpec{GeneratorID: q.GeneratorID, Version: q.Version, Difficulty: q.Difficulty}
}

// ErrPoolFull 是"题池行数已达硬上限"。
var ErrPoolFull = errors.New("store: 题池已达上限")

// PoolCount 是池的总行数（含三种状态），用于全局容量判定。
func (d *DB) PoolCount(ctx context.Context) (int, error) {
	var n int
	if err := d.h.QueryRowContext(ctx, `SELECT count(*) FROM pool`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计题池: %w", err)
	}
	return n, nil
}

// QuestionBuckets 返回需要备题的分桶：只看在用的站点、且只取 pow_question 的显式配置。
// 分桶键是 (generator_id, difficulty)——version 由注册表解析，避免库里存两份版本号。
func (d *DB) QuestionBuckets(ctx context.Context) ([]PoolSpec, error) {
	rows, err := d.h.QueryContext(ctx, `
SELECT DISTINCT generator_id, difficulty FROM sites
WHERE enabled=1 AND verify_mode=? AND generator_id <> ''
ORDER BY generator_id, difficulty`, ModePowQuestion)
	if err != nil {
		return nil, fmt.Errorf("store: 读备题分桶: %w", err)
	}
	defer rows.Close()
	out := []PoolSpec{}
	for rows.Next() {
		var (
			id   string
			diff int
		)
		if err := rows.Scan(&id, &diff); err != nil {
			return nil, fmt.Errorf("store: 扫描备题分桶: %w", err)
		}
		out = append(out, PoolSpec{GeneratorID: id, Difficulty: diff})
	}
	return out, rows.Err()
}

// ReadyCount 返回某个分桶里可用的题数。
func (d *DB) ReadyCount(ctx context.Context, spec PoolSpec, now time.Time) (int, error) {
	var n int
	err := d.h.QueryRowContext(ctx, `
SELECT count(*) FROM pool
WHERE status=? AND generator_id=? AND version=? AND difficulty=? AND expires_at>?`,
		PoolReady, spec.GeneratorID, spec.Version, spec.Difficulty, formatTime(now.UTC())).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: 统计可用题目: %w", err)
	}
	return n, nil
}

// PoolRowCounts 是管理面的只读水位统计（ready/assigned/terminal 与总数）。
type PoolRowCounts struct {
	GeneratorID string
	Version     string
	Difficulty  int
	Ready       int
	Assigned    int
	Terminal    int
	Total       int
	OldestReady string // 最早过期的 ready 题（判断池是否在退化，非敏感）
}

// PoolStats 返回按分桶聚合的水位。行数有上限（分桶组合本身就是有界的）。
func (d *DB) PoolStats(ctx context.Context, now time.Time) ([]*PoolRowCounts, error) {
	rows, err := d.h.QueryContext(ctx, `
SELECT generator_id, version, difficulty,
       sum(CASE WHEN status=? THEN 1 ELSE 0 END),
       sum(CASE WHEN status=? THEN 1 ELSE 0 END),
       sum(CASE WHEN status=? THEN 1 ELSE 0 END),
       count(*),
       min(CASE WHEN status=? THEN expires_at END)
FROM pool
GROUP BY generator_id, version, difficulty
ORDER BY generator_id, version, difficulty`,
		PoolReady, PoolAssigned, PoolTerminal, PoolReady)
	if err != nil {
		return nil, fmt.Errorf("store: 池统计: %w", err)
	}
	defer rows.Close()
	out := []*PoolRowCounts{}
	for rows.Next() {
		var (
			s        = &PoolRowCounts{}
			oldest   sql.NullString
			total    int64
			r, a, tm int64
		)
		if err := rows.Scan(&s.GeneratorID, &s.Version, &s.Difficulty, &r, &a, &tm, &total, &oldest); err != nil {
			return nil, fmt.Errorf("store: 扫描池统计: %w", err)
		}
		s.Ready, s.Assigned, s.Terminal, s.Total = int(r), int(a), int(tm), int(total)
		if oldest.Valid {
			if t, err := parseTime(oldest.String); err == nil {
				s.OldestReady = t.UTC().Format(time.RFC3339)
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// IssueChallengeWithQuestion 在**同一个事务**里创建 challenge 行并领取一道题。
// spec 为 nil 表示站点是 pow_only：只建 challenge，不碰池。
//
// 领取用"选候选 → 条件更新 → 检查影响行数"，重试上限 claimAttempts；
// 任一步失败整个事务回滚，因此不会出现"题被标 assigned 但没有 challenge"的孤立绑定。
func (d *DB) IssueChallengeWithQuestion(ctx context.Context, c *Challenge, caps Caps, spec *PoolSpec) (*PoolQuestion, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: challenge 为空", ErrBadConfig)
	}
	tx, err := d.h.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 签发事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// challenge 的模式/生成器是**签发快照**：之后站点改配置不会影响已发出的这张挑战。
	mode, gen := ModePowOnly, ""
	if spec != nil {
		mode, gen = ModePowQuestion, spec.GeneratorID
	}
	if err := insertChallengeTx(ctx, tx, c, caps, mode, gen); err != nil {
		return nil, err
	}
	if spec == nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store: 提交签发: %w", err)
		}
		return nil, nil
	}
	now := formatTime(c.CreatedAt.UTC())
	// 题目必须比 challenge 更晚过期，否则会出现"challenge 还有效但题已过期"的死角
	needAfter := formatTime(c.ExpiresAt.UTC())
	var claimed *PoolQuestion
	for range claimAttempts {
		var id string
		err := tx.QueryRowContext(ctx, `
SELECT id FROM pool
WHERE status=? AND generator_id=? AND version=? AND difficulty=? AND expires_at>?
ORDER BY expires_at LIMIT ?`,
			PoolReady, spec.GeneratorID, spec.Version, spec.Difficulty, needAfter, 1).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			break // 桶里没有满足条件的题
		}
		if err != nil {
			return nil, fmt.Errorf("store: 选题: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
UPDATE pool SET status=?, challenge_id=?, assigned_at=?
WHERE id=? AND status=? AND expires_at>?`,
			PoolAssigned, c.ID, now, id, PoolReady, needAfter)
		if err != nil {
			return nil, fmt.Errorf("store: 领取条件更新: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			// 同一事务内改状态的唯一写者就是本事务，所以这一行只在"跨进程共享同一库文件"时可达；
			// 那里更常见的形态是上面 ExecContext 直接拿到 SQLITE_BUSY 而整笔回滚（见 claimAttempts 注释）。
			// 保留这一分支是为了让"看到但没改到"变成一次有界重试，而不是一句"领到了"。
			continue
		}
		claimed, err = readQuestionTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		break
	}
	if claimed == nil {
		// 池空：整个事务回滚，challenge 也不会留下——不静默降级成 pow_only（ADR-011 #7）
		return nil, ErrPoolEmpty
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交签发+领题: %w", err)
	}
	return claimed, nil
}

func readQuestionTx(ctx context.Context, tx *sql.Tx, id string) (*PoolQuestion, error) {
	var (
		q           = &PoolQuestion{ID: id}
		created     string
		expires     string
		assigned    sql.NullString
		challengeID sql.NullString
		err         error
	)
	err = tx.QueryRowContext(ctx, `
SELECT generator_id, version, difficulty, status, payload_json, verifier_blob,
       created_at, expires_at, challenge_id, assigned_at
FROM pool WHERE id=?`, id).
		Scan(&q.GeneratorID, &q.Version, &q.Difficulty, &q.Status, &q.PayloadJSON, &q.Verifier,
			&created, &expires, &challengeID, &assigned)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读题目: %w", err)
	}
	if q.ChallengeID = ""; challengeID.Valid {
		q.ChallengeID = challengeID.String
	}
	if q.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if q.ExpiresAt, err = parseTime(expires); err != nil {
		return nil, err
	}
	if assigned.Valid {
		if q.AssignedAt, err = parseTime(assigned.String); err != nil {
			return nil, err
		}
	}
	return q, nil
}

// QuestionForChallenge 返回绑定到这张 challenge 的题（没有绑定则 ErrNotFound）。
// redeem 阶段读它拿私有 verifier；这一步只读，不进写事务。
func (d *DB) QuestionForChallenge(ctx context.Context, challengeID string) (*PoolQuestion, error) {
	var (
		q        = &PoolQuestion{}
		created  string
		expires  string
		assigned sql.NullString
		terminal sql.NullString
		bound    sql.NullString
		status   string
		err      error
	)
	err = d.h.QueryRowContext(ctx, `
SELECT id, generator_id, version, difficulty, status, payload_json, verifier_blob,
       created_at, expires_at, challenge_id, assigned_at, terminal_at
FROM pool WHERE challenge_id=?`, challengeID).
		Scan(&q.ID, &q.GeneratorID, &q.Version, &q.Difficulty, &status, &q.PayloadJSON, &q.Verifier,
			&created, &expires, &bound, &assigned, &terminal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查绑定题目: %w", err)
	}
	q.Status = status
	if bound.Valid {
		q.ChallengeID = bound.String
	}
	if q.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if q.ExpiresAt, err = parseTime(expires); err != nil {
		return nil, err
	}
	if assigned.Valid {
		if q.AssignedAt, err = parseTime(assigned.String); err != nil {
			return nil, err
		}
	}
	if terminal.Valid {
		if q.TerminalAt, err = parseTime(terminal.String); err != nil {
			return nil, err
		}
	}
	return q, nil
}
