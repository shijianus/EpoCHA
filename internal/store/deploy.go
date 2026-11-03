package store

// 阶段 5A：备份、恢复与就绪探针。
//
// 为什么用 VACUUM INTO 而不是"直接 cp 主文件"，也不是 sqlite3 CLI：
//   1. 库跑在 WAL 模式下（dsnTemplate 钉住），最新的那部分数据在 -wal 里，
//      **只 cp 主 .db 会得到一个"存在但落后"的数据库**——这不是理论风险，DESIGN §13 里
//      就记着"写入进行中主文件 4,096B + WAL 2,855,192B"这个实测形状。
//   2. VACUUM INTO 是 SQLite 官方的一致性快照：它把源库（含 WAL 内容）重写成目标文件，
//      产物是单个、无 -wal 的库，天然可校验、可搬运。
//   3. sqlite3 是外部二进制，本仓库的约束是"服务与运维能力都用纯 Go 实现"（AGENTS 硬性技术约束）。
//      备份工具属于交付面的一部分，不能引入一个宿主上必须装好的命令。

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// BackupFormat 写进 manifest：让"哪一代工具该能读这份备份"成为可检查的事实，
	// 而不是靠人记住三年前的目录布局。
	BackupFormat = "epocha-backup/v1"

	manifestSuffix   = ".manifest.json"
	partialSuffix    = ".partial"
	maxManifestBytes = 1 << 20

	// pepperFingerprintPurpose 是域分隔串（DESIGN §6 的派生纪律：同一把密钥不复用到不同用途）。
	pepperFingerprintPurpose = "epocha/v1|pepper-fp|"

	// restoreAdminLabel 标记"这把管理凭据是恢复流程本地装入的"，让管理面列表能区分它和常规签发。
	restoreAdminLabel = "restore"

	// recoverLabelPrefix 是本地管理访问恢复的标签前缀。label 在库里是 UNIQUE 的，
	// 所以真实标签带上 key_id 后缀——同一台机器做过多次恢复时不能因冲突而失败。
	recoverLabelPrefix = "local-recover"

	// maxRecoverRevoked 只给"撤销了多少把"这个报告项一个上界，不是安全边界。
	maxRecoverRevoked = 1 << 20

	// readOnlyDSN 用 mode=ro 打开源库：备份动作本身**绝不**写源文件。
	// 这一条把"我不会手滑改到活库"从承诺变成驱动层强制。
	readOnlyDSN = "file:%s?mode=ro&_pragma=busy_timeout(5000)"

	readyMaxWait = 2 * time.Second
)

var (
	// ErrNotReady 是就绪探针的统一失败族：调用方（/readyz）只关心"是/否"，原因不外泄。
	ErrNotReady      = errors.New("store: 不就绪")
	ErrBackupInvalid = errors.New("store: 备份不可信")

	// ErrPepperMismatch 单独成族：恢复流程要能区分"文件坏了"与"密钥配错"，
	// 因为后者的处置方式（找回配对的 pepper，或显式承担轮换后果）完全不同。
	ErrPepperMismatch = errors.New("store: pepper 与该备份不配对")
)

// PepperFingerprint 返回 pepper 的单向指纹（hex sha256，带域分隔）。
//
// 用途只有一个：把"这份备份和这把 pepper 配不配对"变成恢复前可执行的一次检查。
// 安全前提是 pepper 满足本仓库的 ≥32 字节 CSPRNG 要求（config.minKeyBytes 已强制），
// 此时反推不可行；它**不是**新增的加密原语需求，只是"离线验证配对"的最小代价。
// 指纹只写进 manifest 文件，不进数据库、不进日志、不进 HTTP 响应；
// manifest 与备份分开保管，理由与 pepper/DB 分库存放同一条（DESIGN §6）。
func PepperFingerprint(pepper []byte) string {
	h := sha256.New()
	h.Write([]byte(pepperFingerprintPurpose))
	h.Write(pepper)
	return hex.EncodeToString(h.Sum(nil))
}

// FingerprintMatches 常数时间比较（红线 2：密钥派生值的比较没有例外）。
func FingerprintMatches(fp string, pepper []byte) bool {
	return subtle.ConstantTimeCompare([]byte(fp), []byte(PepperFingerprint(pepper))) == 1
}

// Ready 检查连接、schema 版本及文件权限，不分配题目或消费 token。
// 题池水位不参与就绪判定，写锁和业务事务也不作为探针负载。
// 文件检查不等于一次 WAL 提交成功，磁盘余量与业务写入错误仍须独立监控。
func (d *DB) Ready(ctx context.Context) error {
	if d == nil || d.h == nil {
		return fmt.Errorf("%w: 句柄缺失", ErrNotReady)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: 请求已取消", ErrNotReady)
	}
	ctxv, cancel := context.WithTimeout(ctx, readyMaxWait)
	defer cancel()

	if err := ctxv.Err(); err != nil {
		return fmt.Errorf("%w: 请求已取消", ErrNotReady)
	}
	if err := d.h.PingContext(ctxv); err != nil {
		return fmt.Errorf("%w: 连接", ErrNotReady)
	}
	var v sql.NullString
	if err := d.h.QueryRowContext(ctxv, `SELECT v FROM meta WHERE k='schema_version'`).Scan(&v); err != nil {
		return fmt.Errorf("%w: 版本记录", ErrNotReady)
	}
	if err := ctxv.Err(); err != nil {
		return fmt.Errorf("%w: 请求已取消", ErrNotReady)
	}
	n, err := strconv.Atoi(v.String)
	if err != nil {
		return fmt.Errorf("%w: 版本记录", ErrNotReady)
	}
	if n != schemaVersion {
		return fmt.Errorf("%w: schema 版本不匹配", ErrNotReady)
	}
	if err := d.writeProbe(ctxv); err != nil {
		return err
	}
	if err := ctxv.Err(); err != nil {
		return fmt.Errorf("%w: 请求已取消", ErrNotReady)
	}
	return nil
}

// writeProbe 不写业务数据，只检查主文件可写和同目录文件创建能力。
func (d *DB) writeProbe(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: 请求已取消", ErrNotReady)
	}
	if d.path == "" {
		return fmt.Errorf("%w: 路径缺失", ErrNotReady)
	}
	f, err := os.OpenFile(d.path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("%w: 主文件不可写", ErrNotReady)
	}
	_ = f.Close()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: 请求已取消", ErrNotReady)
	}
	// WAL 需要能在同目录创建 -wal/-shm；目录不可写时上面那步可能仍然通过。
	dir := filepath.Dir(d.path)
	tmp, err := os.CreateTemp(dir, ".epocha-ready-*")
	if err != nil {
		return fmt.Errorf("%w: 数据目录不可写", ErrNotReady)
	}
	name := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(name)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: 请求已取消", ErrNotReady)
	}
	return nil
}

// Manifest 是一次备份的自描述记录：版本、schema、体积、内容计数、快照哈希与 pepper 指纹。
// 字段一律**不含**任何密钥或行数据；它可以与备份分开保管。
type Manifest struct {
	Format        string           `json:"format"`
	Name          string           `json:"name"`
	DBFile        string           `json:"db_file"`
	CreatedAt     string           `json:"created_at"`
	AppVersion    string           `json:"app_version"`
	Commit        string           `json:"commit"`
	SchemaVersion int              `json:"schema_version"`
	SHA256        string           `json:"sha256"`
	PepperFP      string           `json:"pepper_fingerprint"`
	BackupBytes   int64            `json:"backup_bytes"`
	SourceMainB   int64            `json:"source_main_bytes"`
	SourceWALB    int64            `json:"source_wal_bytes"`
	Integrity     string           `json:"integrity_check"`
	Counts        map[string]int64 `json:"counts"`
	Warning       string           `json:"-"`
}

// BackupOptions 是一次备份的输入。AppVersion/Commit 由调用方（cmd/ops）注入，
// store 不反向依赖 version 包。
type BackupOptions struct {
	SrcPath    string
	DestDir    string
	Pepper     []byte
	Now        time.Time
	AppVersion string
	Commit     string
}

// countQuery 是备份计数的单一来源：manifest 里写什么、恢复后核对什么，用同一张表。
var countQuery = []struct {
	key   string
	sql   string
	allow bool // 表在 schema v1..v4 里都存在；为 false 的项在缺表时跳过
}{
	{"sites", `SELECT count(*) FROM sites`, true},
	{"sites_enabled", `SELECT count(*) FROM sites WHERE enabled=1`, true},
	{"site_secrets_active", `SELECT count(*) FROM site_secrets WHERE revoked_at IS NULL`, true},
	{"admin_keys_active", `SELECT count(*) FROM admin_keys WHERE revoked_at IS NULL`, true},
	{"challenges_issued", `SELECT count(*) FROM challenges WHERE status='issued'`, true},
	{"challenges_redeemed", `SELECT count(*) FROM challenges WHERE status='redeemed'`, true},
	{"challenges_total", `SELECT count(*) FROM challenges`, true},
	{"responses_total", `SELECT count(*) FROM responses`, true},
	{"responses_unused", `SELECT count(*) FROM responses WHERE used=0`, true},
	{"pool_ready", `SELECT count(*) FROM pool WHERE status='ready'`, true},
	{"pool_total", `SELECT count(*) FROM pool`, true},
	{"pool_asset_bytes", `SELECT coalesce(sum(length(asset_blob)),0) FROM pool`, true},
}

// Backup 用 VACUUM INTO 生成一致性快照，并写出 manifest。
// 暂存目录为每次调用独占；两个最终文件都采用无覆盖发布，manifest 最后落位。
// 数据库发布后不再回滚删除，失败响应必须说明已保留的产物与尚未完成的步骤。
func Backup(ctx context.Context, o BackupOptions) (manifest *Manifest, result error) {
	if len(o.Pepper) < 32 {
		return nil, fmt.Errorf("%w: pepper 长度不足 32 字节", ErrBadConfig)
	}
	if o.SrcPath == "" || o.DestDir == "" {
		return nil, fmt.Errorf("%w: 缺少源路径或目标目录", ErrBadConfig)
	}
	if err := checkDSNPath(o.SrcPath); err != nil {
		return nil, err
	}
	if err := checkDSNPath(o.DestDir); err != nil {
		return nil, err
	}
	src, err := os.Stat(o.SrcPath)
	if err != nil || !src.Mode().IsRegular() {
		return nil, fmt.Errorf("store: 源数据库不存在或不是普通文件")
	}
	dir, err := os.Stat(o.DestDir)
	if err != nil || !dir.IsDir() {
		return nil, fmt.Errorf("store: 备份目标目录不存在")
	}
	now := o.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	name := "epocha-" + now.Format("20060102T150405Z") + "-" + sanitizeToken(o.Commit)
	destDB := filepath.Join(o.DestDir, name+".db")
	destManifest := filepath.Join(o.DestDir, name+manifestSuffix)
	for _, p := range []string{destDB, destManifest} {
		if _, err := os.Lstat(p); err == nil {
			return nil, fmt.Errorf("store: 目标已存在，拒绝覆盖：%s", filepath.Base(p))
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("store: 无法确认备份目标是否存在")
		}
	}

	h, err := sql.Open("sqlite", fmt.Sprintf(readOnlyDSN, o.SrcPath))
	if err != nil {
		return nil, fmt.Errorf("store: 打开源库: %w", err)
	}
	defer func() { _ = h.Close() }()
	if err := h.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("store: 连接源库失败")
	}
	// 版本闸门：本程序读不懂的库不要备份——那会把一次"恢复回去就崩"的操作存进仓库。
	cur, err := schemaVersionOfConn(ctx, h)
	if err != nil {
		return nil, err
	}
	if cur > schemaVersion {
		return nil, fmt.Errorf("%w: 库为 v%d，程序支持 v%d", ErrSchemaConflict, cur, schemaVersion)
	}

	staging, err := os.MkdirTemp(o.DestDir, "."+name+"-")
	if err != nil {
		return nil, fmt.Errorf("store: 无法创建备份暂存目录")
	}
	defer func() {
		if err := os.RemoveAll(staging); err != nil {
			if manifest != nil && result == nil {
				manifest.Warning += " 本次备份暂存目录清理失败；已发布文件保留。"
			} else {
				result = errors.Join(result, fmt.Errorf("store: 本次备份暂存目录清理失败"))
			}
		}
	}()
	partial := filepath.Join(staging, "snapshot.db")
	if _, err := h.ExecContext(ctx, `VACUUM INTO ?`, partial); err != nil {
		return nil, fmt.Errorf("store: 快照写入失败")
	}
	if err := syncFile(partial); err != nil {
		return nil, err
	}
	if err := IntegrityCheck(ctx, partial); err != nil {
		return nil, err
	}
	counts, err := readCounts(ctx, partial)
	if err != nil {
		return nil, err
	}
	// 一致性检查与计数都以**只读**句柄打开快照，SQLite 会给 WAL 头的库留下 -shm/-wal。
	// 必须在算哈希与改名之前收掉，否则交付目录里会出现"备份的半边文件"，
	// 而 manifest 里的 sha256 记录的却是收掉它们之前的主文件。
	if err := dropSidecars(partial); err != nil {
		return nil, err
	}
	fi, err := os.Stat(partial)
	if err != nil {
		return nil, err
	}
	if fi.Size() == 0 {
		return nil, fmt.Errorf("%w: 快照为空", ErrBackupInvalid)
	}
	sum, err := sha256File(partial)
	if err != nil {
		return nil, err
	}
	// 快照先落地再改权限：0600 与真实数据目录的期望一致，避免同机其它账号读走备份。
	if err := os.Chmod(partial, 0o600); err != nil {
		return nil, fmt.Errorf("store: 设置备份权限失败")
	}

	m := &Manifest{
		Format: BackupFormat, Name: name, DBFile: filepath.Base(destDB),
		CreatedAt:  now.Format(time.RFC3339),
		AppVersion: orDefault(o.AppVersion, "unknown"), Commit: orDefault(o.Commit, "unknown"),
		SchemaVersion: cur, SHA256: sum, PepperFP: PepperFingerprint(o.Pepper),
		BackupBytes: fi.Size(), SourceMainB: src.Size(),
		SourceWALB: fileSizeOrNil(o.SrcPath + "-wal"),
		Integrity:  "ok",
		Counts:     counts,
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	mpartial := filepath.Join(staging, "manifest.json")
	if err := os.WriteFile(mpartial, body, 0o600); err != nil {
		return nil, fmt.Errorf("store: 写 manifest 失败")
	}
	if err := syncFile(partial); err != nil {
		return nil, err
	}
	if err := syncFile(mpartial); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	warning, err := publishBackup(partial, mpartial, destDB, destManifest, syncDir)
	if err != nil {
		return nil, err
	}
	m.Warning = warning
	return m, nil
}

// manifest 最后发布；中途失败保留已发布数据库，不能在错误清理中删除已交付路径。
func publishBackup(db, manifest, destDB, destManifest string, syncDirectory func(string) error) (string, error) {
	if err := os.Link(db, destDB); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("store: 备份目标已存在，拒绝覆盖：%s", filepath.Base(destDB))
		}
		return "", fmt.Errorf("store: 备份数据库发布失败")
	}
	if err := os.Link(manifest, destManifest); err != nil {
		return "", fmt.Errorf("store: 备份数据库 %s 已发布并保留，但 manifest 未发布；不是完整备份，请核查冲突，勿覆盖重跑", filepath.Base(destDB))
	}
	if err := syncDirectory(filepath.Dir(destDB)); err != nil {
		return "备份数据库与 manifest 已发布并保留；目录同步失败，持久化尚未确认，请核验存储和备份，勿覆盖重跑。", nil
	}
	return "", nil
}

// IntegrityCheck 跑 PRAGMA integrity_check：只有返回单个 "ok" 才算通过。
// "文件非空"不是一致性，"能被打开"也不是一致性。
func IntegrityCheck(ctx context.Context, path string) error {
	if err := checkDSNPath(path); err != nil {
		return err
	}
	h, err := sql.Open("sqlite", fmt.Sprintf(readOnlyDSN, path))
	if err != nil {
		return fmt.Errorf("%w: 打开失败", ErrBackupInvalid)
	}
	defer func() { _ = h.Close() }()
	rows, err := h.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("%w: integrity_check 无法执行", ErrBackupInvalid)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return fmt.Errorf("%w: 读取失败", ErrBackupInvalid)
		}
		out = append(out, s)
		if len(out) > 8 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: integrity_check 失败", ErrBackupInvalid)
	}
	if len(out) != 1 || out[0] != "ok" {
		return fmt.Errorf("%w: %s", ErrBackupInvalid, strings.Join(out, "; "))
	}
	return nil
}

func schemaVersionOfConn(ctx context.Context, h *sql.DB) (int, error) {
	var v sql.NullString
	err := h.QueryRowContext(ctx, `SELECT v FROM meta WHERE k='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: 读版本失败")
	}
	n, err := strconv.Atoi(v.String)
	if err != nil {
		return 0, fmt.Errorf("%w: schema 版本记录不可解析", ErrBackupInvalid)
	}
	return n, nil
}

// schemaVersionOfFile 以只读方式读一个库文件的 schema 版本，用于恢复报告的 after 值。
func schemaVersionOfFile(ctx context.Context, path string) (int, error) {
	if err := checkDSNPath(path); err != nil {
		return 0, err
	}
	h, err := sql.Open("sqlite", fmt.Sprintf(readOnlyDSN, path))
	if err != nil {
		return 0, fmt.Errorf("%w: 打开失败", ErrBackupInvalid)
	}
	defer func() { _ = h.Close() }()
	return schemaVersionOfConn(ctx, h)
}

// readCounts 以只读方式统计快照内容。计数是"恢复之后应该看到什么"的基线，
// 也是运维口头交接最容易出错的地方（"那个备份里大概有十几个站点"）。
func readCounts(ctx context.Context, path string) (map[string]int64, error) {
	if err := checkDSNPath(path); err != nil {
		return nil, err
	}
	h, err := sql.Open("sqlite", fmt.Sprintf(readOnlyDSN, path))
	if err != nil {
		return nil, fmt.Errorf("store: 打开快照失败")
	}
	defer func() { _ = h.Close() }()
	out := make(map[string]int64, len(countQuery))
	for _, q := range countQuery {
		var n sql.NullInt64
		if err := h.QueryRowContext(ctx, q.sql).Scan(&n); err != nil {
			return nil, fmt.Errorf("store: 备份计数失败")
		}
		out[q.key] = n.Int64
	}
	return out, nil
}

// ReadManifest 读取并校验 manifest 的形状。上限 maxManifestBytes：
// manifest 是我们自己写的小 JSON，越界说明它被换成了别的东西，不按可信输入处理。
func ReadManifest(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("store: 找不到 manifest")
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil || len(body) > maxManifestBytes {
		return nil, fmt.Errorf("%w: manifest 过大或不可读", ErrBackupInvalid)
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: manifest 解析失败", ErrBackupInvalid)
	}
	if m.Format != BackupFormat || m.SHA256 == "" || m.DBFile == "" {
		return nil, fmt.Errorf("%w: manifest 格式不是 %s 或字段缺失", ErrBackupInvalid, BackupFormat)
	}
	return &m, nil
}

// VerifyBackup 把"备份文件 + manifest"重新对上一遍：哈希、大小、完整性检查、schema 版本。
// 这是每日备份后该跑的那一步——非空的 .db 文件不等于可用的备份。
func VerifyBackup(ctx context.Context, dbPath, manifestPath string) error {
	m, err := ReadManifest(manifestPath)
	if err != nil {
		return err
	}
	if err := checkDSNPath(dbPath); err != nil {
		return err
	}
	fi, err := os.Stat(dbPath)
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: 备份文件不存在", ErrBackupInvalid)
	}
	if fi.Size() != m.BackupBytes {
		return fmt.Errorf("%w: 体积与 manifest 不符（%d vs %d）", ErrBackupInvalid, fi.Size(), m.BackupBytes)
	}
	sum, err := sha256File(dbPath)
	if err != nil {
		return err
	}
	// 红线 2：摘要比较一律常数时间。
	if subtle.ConstantTimeCompare([]byte(sum), []byte(m.SHA256)) != 1 {
		return fmt.Errorf("%w: sha256 与 manifest 不符", ErrBackupInvalid)
	}
	if err := IntegrityCheck(ctx, dbPath); err != nil {
		return err
	}
	cur, err := schemaVersionOfFile(ctx, dbPath)
	if err != nil {
		return err
	}
	if cur > schemaVersion {
		return fmt.Errorf("%w: 备份为 v%d，程序支持 v%d", ErrSchemaConflict, cur, schemaVersion)
	}
	// 校验动作本身是只读的，但只读打开会留下 sidecar；收掉它们，免得"跑一次校验"
	// 就把备份目录变成"一个 .db 加两个说不清来历的文件"。
	return dropSidecars(dbPath)
}

// RestoreOptions 是一次保守恢复的输入。
//
// 语义：**恢复产物是一份新文件，绝不覆盖当前工作库**。
// 恢复完成后运维需要显式停服、换路径、再启动，并人工确认每个站点后才启用。
type RestoreOptions struct {
	BackupPath   string
	ManifestPath string
	OutDir       string
	DestName     string
	Pepper       []byte
	AdminKey     string
	RotatePepper bool
	Now          time.Time
}

// RestoreReport 是恢复的结果说明（不含任何凭据明文）。
type RestoreReport struct {
	DBPath        string           `json:"db_path"`
	SourceBackup  string           `json:"source_backup"`
	SchemaBefore  int              `json:"schema_version_before"`
	SchemaAfter   int              `json:"schema_version_after"`
	RestoredAt    string           `json:"restored_at"`
	PepperRotated bool             `json:"pepper_rotated"`
	CountsBefore  map[string]int64 `json:"counts_before"`
	CountsAfter   map[string]int64 `json:"counts_after"`
	Warning       string           `json:"warning,omitempty"`
}

// String 是给运维看的一行摘要；Manifest 之外单独实现，避免报告里带出指纹。
func (r *RestoreReport) String() string {
	keys := make([]string, 0, len(r.CountsAfter))
	for k := range r.CountsAfter {
		keys = append(keys, k)
	}
	// 排序交给调用方保证稳定；这里只列关键三项，其余靠 JSON 报告。
	return fmt.Sprintf("恢复产物=%s schema=%d→%d 站点=%d→%d(全部禁用) 有效站点凭据=%d→%d 管理凭据=%d→%d response=%d→%d 题池=%d→%d pepper_rotated=%t",
		filepath.Base(r.DBPath), r.SchemaBefore, r.SchemaAfter,
		r.CountsBefore["sites"], r.CountsAfter["sites"],
		r.CountsBefore["site_secrets_active"], r.CountsAfter["site_secrets_active"],
		r.CountsBefore["admin_keys_active"], r.CountsAfter["admin_keys_active"],
		r.CountsBefore["responses_total"], r.CountsAfter["responses_total"],
		r.CountsBefore["pool_total"], r.CountsAfter["pool_total"], r.PepperRotated)
}

// Restore 校验快照 → 复制到隔离目录 → 在同一事务里做完保守失效 → 交付单文件库。
//
// 为什么失效动作是"删与置为终态"而不是"标记来源"：恢复回来的是备份时刻的状态，
// 备份之后发生的消费、撤销、停用在快照里**不存在**。这些状态无法从快照里恢复出来，
// 所以唯一诚实的处置是让快照里所有"还能被接受"的东西一次性失效：
//   - challenges 全部置 terminal 状态 invalid、responses 全删 ⇒ 恢复前签发的 token 一律兑换不了；
//   - pool 全删 ⇒ 主密钥换过之后 K(qver) 派生的 verifier 必然判假，留着只会消耗用户作答机会
//     （DESIGN §6 的轮换运维约束在这里是同一条）；
//   - sites 全部 enabled=0 ⇒ 恢复完成前不接收业务流量，由人工逐个确认后再开；
//   - site_secrets 与 admin_keys 全删 ⇒ 备份后被撤销的凭据不会在快照里复活；
//     新的管理凭据由这次操作本地装入（**没有任何未鉴权的恢复接口**）。
//
// 主密钥**不在库里**，因此这里不处理它：恢复后必须用**新的** EPOCHA_MASTER_KEY 启动，
// 且**不设** EPOCHA_MASTER_KEY_PREVIOUS——那把旧签名回退正是"旧 token 重新可用"的唯一通道。
//
// 运行锁由**调用方**（`cmd/ops` 的 restore）对 OutDir 取一次，本函数不取：
// `flock` 锁绑在 open file description 上，同进程内两次独立打开同一把锁会互相冲突。
func Restore(ctx context.Context, o RestoreOptions) (*RestoreReport, error) {
	if len(o.Pepper) < 32 {
		return nil, fmt.Errorf("%w: pepper 长度不足 32 字节", ErrBadConfig)
	}
	if o.BackupPath == "" || o.ManifestPath == "" || o.OutDir == "" {
		return nil, fmt.Errorf("%w: 缺少备份/manifest/输出目录", ErrBadConfig)
	}
	if err := checkDestName(o.DestName); err != nil {
		return nil, err
	}
	if len(o.AdminKey) < MinSecretLen {
		return nil, fmt.Errorf("%w: 新的管理凭据短于 %d 字符", ErrBadConfig, MinSecretLen)
	}
	if err := checkDSNPath(o.BackupPath); err != nil {
		return nil, err
	}
	m, err := ReadManifest(o.ManifestPath)
	if err != nil {
		return nil, err
	}
	if err := VerifyBackup(ctx, o.BackupPath, o.ManifestPath); err != nil {
		return nil, err
	}
	// 配对检查：pepper 与这份备份不配对时，快照里的所有摘要本来就已失效；
	// 但那要么是拿错了密钥文件，要么是一次**有意**的 pepper 轮换，两者不能靠猜。
	rotated := !FingerprintMatches(m.PepperFP, o.Pepper)
	if rotated && !o.RotatePepper {
		return nil, fmt.Errorf("%w: 该备份由另一个 pepper 生成；确认要按 pepper 轮换恢复才加 --rotate-pepper"+
			"（后果：站点与管理凭据必须重新签发）", ErrPepperMismatch)
	}
	if fi, err := os.Stat(o.OutDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("store: 恢复输出目录不存在")
	}
	dest := filepath.Join(o.OutDir, o.DestName)
	// 不覆盖，也不给 -wal/-shm 留下"半个库"的可能：三者任一存在即拒绝。
	for _, p := range []string{dest, dest + "-wal", dest + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			return nil, fmt.Errorf("store: 恢复目标已存在，拒绝覆盖：%s", filepath.Base(p))
		}
	}
	now := o.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Keep the copied snapshot hidden and uniquely named until every transform and
	// verification step has succeeded. A pre-existing dest.partial belongs to the
	// operator; never use or remove it as this operation's scratch file.
	staging, err := copyFileToTemp(o.BackupPath, o.OutDir, "."+o.DestName+partialSuffix+"-")
	if err != nil {
		return nil, err
	}
	defer func() {
		// This path is owned by this invocation. In particular, do not derive the
		// cleanup path from dest, because an older user-created .partial may exist.
		_ = os.Remove(staging)
		_ = dropSidecarsSafe(staging)
	}()
	if err := syncFile(staging); err != nil {
		return nil, err
	}

	before, err := readCounts(ctx, staging)
	if err != nil {
		return nil, err
	}
	if err := dropSidecars(staging); err != nil {
		return nil, err
	}
	d, err := Open(ctx, staging, o.Pepper)
	if err != nil {
		return nil, fmt.Errorf("store: 打开恢复副本: %w", err)
	}
	// 失效动作必须在**一条事务**里：中途失败留下"站点已禁用但凭据还在"或反之的状态，
	// 比整个恢复失败更难判断。
	err = d.transform(ctx, o.AdminKey, now)
	cerr := d.Close()
	if err != nil {
		return nil, err
	}
	if cerr != nil {
		return nil, fmt.Errorf("store: 关闭恢复副本: %w", cerr)
	}
	if err := IntegrityCheck(ctx, staging); err != nil {
		return nil, err
	}
	after, err := readCounts(ctx, staging)
	if err != nil {
		return nil, err
	}
	schemaAfter, err := schemaVersionOfFile(ctx, staging)
	if err != nil {
		return nil, err
	}
	// 只在临时副本上收尾：上面的只读检查会再次留下 sidecar，checkpoint
	// 与干净关闭必须完成后，才能把它作为单文件交付。
	if err := finalizeSingleFile(ctx, staging); err != nil {
		return nil, err
	}
	if err := syncFile(staging); err != nil {
		return nil, err
	}

	// os.Link is the no-overwrite publication point: creation is atomic and an
	// existing destination is rejected by the filesystem. The staging file is
	// removed only after the final name is known to reference the complete file.
	if err := publishNoOverwrite(staging, dest); err != nil {
		return nil, err
	}
	// Publication is already complete. A sync/cleanup failure must preserve the
	// final inode and be reported separately; it is not a rollback.
	var warning string
	if err := syncDir(o.OutDir); err != nil {
		warning = "恢复库已完成保守失效并交付；目录同步失败，持久化尚未确认。最终文件已保留，请勿删除或覆盖重跑，先检查存储并重新核验。"
	}
	if err := os.Remove(staging); err != nil {
		warning += " 本次隐藏临时文件清理失败；最终文件已保留。"
	}
	if err := syncDir(o.OutDir); err != nil && warning == "" {
		warning = "恢复库已完成保守失效并交付；临时文件清理后的目录同步失败，清理状态持久化尚未确认。最终文件已保留，请勿删除或覆盖重跑。"
	}
	return &RestoreReport{
		DBPath: dest, SourceBackup: filepath.Base(o.BackupPath),
		SchemaBefore: m.SchemaVersion, SchemaAfter: schemaAfter,
		RestoredAt:    now.Format(time.RFC3339),
		PepperRotated: rotated,
		CountsBefore:  before, CountsAfter: after, Warning: warning,
	}, nil
}

// transform 在一条事务里完成保守失效 + 装入新管理凭据 + 记一条审计。
// 注意红线 3：**不要**在 *sql.Tx 上再执行 BEGIN IMMEDIATE（嵌套事务错误）。
func (d *DB) transform(ctx context.Context, adminKey string, now time.Time) error {
	tx, err := d.h.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 恢复事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	steps := []string{
		`UPDATE challenges SET status='invalid', redeemed_at=NULL WHERE status <> 'invalid'`,
		`DELETE FROM responses`,
		`DELETE FROM pool`,
		`UPDATE sites SET enabled=0`,
		`DELETE FROM site_secrets`,
		`DELETE FROM admin_keys`,
	}
	for _, q := range steps {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("store: 恢复失效步骤未生效")
		}
	}
	keyID, err := NewKeyID()
	if err != nil {
		return err
	}
	// key_mac 必须与 AddAdminKey/MatchAdminKey 同一种编码（hex）：写成原始字节
	// 会让"恢复装入的新管理凭据"在鉴权时因 hex.DecodeString 失败而永久不可用。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO admin_keys(key_id, label, key_mac, created_at) VALUES(?,?,?,?)`,
		keyID, restoreAdminLabel, hex.EncodeToString(d.AdminKeyMac(adminKey)), formatTime(now)); err != nil {
		return fmt.Errorf("store: 装入新管理凭据失败")
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit(ts, actor, action, target, success) VALUES(?,?,?,?,1)`,
		formatTime(now), keyID, "restore_prepare", "restore", 1); err != nil {
		return fmt.Errorf("store: 恢复审计写入失败")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 恢复提交: %w", err)
	}
	return nil
}

// RecoverReport 是一次"恢复管理访问"的**阶段结论**。
//
// 为什么不返回 (key, revoked, err) 三个值就够：调用方手上有一把刚写好的凭据文件，
// 它该删还是该留，取决于**数据库到底采没采用这把凭据**，而不取决于 error 是不是 nil。
// 用 err 的有无来推断这件事，就是把"提交失败"和"提交结果不确定"当成同一件事——
// 前者可以安全清理临时凭据，后者清理掉的就是这把凭据的唯一副本。
type RecoverReport struct {
	// Key 是本次生成并尝试写入的凭据登记信息。key_id 在生成之前失败时为 nil。
	Key *AdminKey
	// Revoked 是本次撤销掉的存量有效凭据数（仅提交成功时有意义）。
	Revoked int
	// Committed：事务的 Commit 调用返回了成功。
	Committed bool
	// Adopted：库里存在这把**未撤销**的凭据。提交成功时来自读回；提交结果不确定时来自
	// 另一个只读连接的核查（见 AdminKeyAdopted 的理由说明）。
	Adopted bool
	// VerifyErr：那次只读核查自己失败了。非 nil 时 Adopted 的 false 不代表"没采用"，只代表"没查到"。
	VerifyErr error
	// Err 是失败原因；提交成功时为 nil。
	Err error
}

// KeepCredentialFile 回答调用方唯一关心的问题：这把凭据文件**不能**删。
//
// 依据是"有任何理由相信库里已经（或可能已经）采用它"：提交确认了、或只读核查读到了、
// 或连核查都没做成（未知方向时保留文件是可逆的，删除是不可逆的）。
func (r *RecoverReport) KeepCredentialFile() bool {
	if r == nil {
		return false
	}
	return r.Committed || r.Adopted || r.VerifyErr != nil
}

// AdminKeyAdopted 用**只读连接**确认库里是否存在这把未撤销的管理凭据。
//
// 存在的理由不是"我们知道 COMMIT 会在写盘之后才报错"——我没测过 SQLite 的这条路径，
// 也不打算把一个没验证过的驱动行为写进依据。理由是更弱、也更硬的一件事：
// **`tx.Commit()` 返回的 error 本身不能证明库里没有这次写入**，所以"提交失败 ⇒ 可以放心删掉凭据文件"
// 是一个推断，不是一个观测。观测比推断便宜：读一次就行。
// 凭 error 猜方向的代价不对称——猜错的后果是"库里有一把没人知道的活凭据 + 唯一副本已被销毁"。
//
// 为什么必须另开连接而不能复用 *DB：服务/运维句柄是 `SetMaxOpenConns(1)` 的**同一条连接**，
// 而一个仍未结束的事务在它自己的连接上看得见自己的未提交写入——那会把"没采用"读成"采用了"。
// 只读连接走 WAL 的已提交快照，才是数据库侧的事实（这条可见性边界有测试钉住）。
// key_id 不是秘密（它进日志与管理面响应），所以它可以出现在文案里；凭据值本身不行。
func AdminKeyAdopted(ctx context.Context, dbPath, keyID string) (bool, error) {
	if keyID == "" {
		return false, fmt.Errorf("%w: key_id 为空", ErrBadConfig)
	}
	if err := checkDSNPath(dbPath); err != nil {
		return false, err
	}
	h, err := sql.Open("sqlite", fmt.Sprintf(readOnlyDSN, dbPath))
	if err != nil {
		return false, fmt.Errorf("store: 只读打开: %w", err)
	}
	defer func() { _ = h.Close() }()
	var one int
	if err := h.QueryRowContext(ctx,
		`SELECT 1 FROM admin_keys WHERE key_id=? AND revoked_at IS NULL LIMIT 1`, keyID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: 核查管理凭据: %w", err)
	}
	return true, nil
}

// RecoverAdminKey 撤销库里所有仍然有效的管理凭据，并登记一把新的（同一条事务）。
//
// 存在的理由是一条真实的锁死路径：`BootstrapAdminKey` 只在"库里**没有任何** admin_keys 记录"时登记，
// 所以撤销掉最后一把凭据之后，改 `EPOCHA_ADMIN_KEY(_FILE)` 也进不去——"库里已有记录"这个条件
// 已经满足，而引导逻辑必须忽略它（否则运维在管理 API 里的撤销会被下一次重启推翻，DESIGN §6）。
// 交付的镜像里没有 sqlite3 之类可以手工改库的东西（AGENTS 硬约束），所以那个局面唯一的出口
// 就是一条**明确、本地、可审计**的命令。
//
// 它刻意很窄：只动 `admin_keys` 与一条审计；不读任何旧凭据（旧凭据只有带 pepper 的摘要，
// 本来也读不回明文）；不做通用管理面；不接 HTTP——没有"未鉴权的恢复接口"这条路。
// 调用方必须在服务停用时执行：判据是 `cmd/ops` 那侧对 `--db` 所在数据目录取的运行锁
// （不是端口探测，也不是在本函数里重复取锁——同一进程内两次独立 flock 会互相冲突，
// 在库层再取一次会直接把成功的恢复变成 ErrLockHeld）。
// "服务还在跑就改库"造出的是两份真相，不是一个可以事后调和的分歧。
//
// 返回的 report 在**所有** key_id 已生成的失败路径上都非 nil，并带一次只读核查的事实；
// 只有"还没生成 key_id 就失败"才返回 nil report（那种情况下库里不可能有本次写入）。
func (d *DB) RecoverAdminKey(ctx context.Context, key string, now time.Time) (*RecoverReport, error) {
	if len(key) < MinSecretLen {
		return nil, fmt.Errorf("%w: 管理凭据短于 %d 字符", ErrBadConfig, MinSecretLen)
	}
	if d == nil || d.h == nil {
		return nil, fmt.Errorf("%w: 句柄缺失", ErrBadConfig)
	}
	tx, err := d.h.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 恢复事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 只复制当前仍可接受的凭据：NULL 与未来截止时间都必须在恢复时立即失效。
	// 已经到期的历史截止时间保持不变，便于审计还原原始生命周期。
	res, err := tx.ExecContext(ctx, `UPDATE admin_keys SET revoked_at=?
		WHERE revoked_at IS NULL OR revoked_at > ?`, formatTime(now.UTC()), formatTime(now.UTC()))

	if err != nil {
		return nil, fmt.Errorf("store: 撤销存量管理凭据: %w", err)
	}
	revoked64, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: 统计撤销数: %w", err)
	}
	if revoked64 < 0 || revoked64 > maxRecoverRevoked {
		return nil, fmt.Errorf("%w: 撤销数量异常", ErrConflict)
	}

	keyID, err := NewKeyID()
	if err != nil {
		return nil, err
	}
	// 此后每个失败出口都要带上核查结果：key_id 已经存在，库里可能已经有它的行。
	attempted := func(reason error, committed bool) *RecoverReport {
		rep := &RecoverReport{Key: &AdminKey{KeyID: keyID}, Err: reason, Committed: committed}
		rep.Adopted, rep.VerifyErr = AdminKeyAdopted(ctx, d.path, keyID)
		return rep
	}

	label := recoverLabelPrefix + "@" + keyID
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO admin_keys(key_id, label, key_mac, created_at) VALUES(?,?,?,?)`,
		keyID, label, hex.EncodeToString(d.AdminKeyMac(key)), formatTime(now.UTC())); err != nil {
		return attempted(fmt.Errorf("store: 登记恢复用管理凭据: %w", err), false), err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit(ts, actor, action, target, success) VALUES(?,?,?,?,1)`,
		formatTime(now.UTC()), keyID, "admin.recover_local", "admin_keys", 1); err != nil {
		return attempted(fmt.Errorf("store: 恢复审计写入失败"), false), err
	}
	if err := tx.Commit(); err != nil {
		// 提交报错 ⇒ 结论是"不确定"，而 attempted() 的只读核查会把它变成有依据的结论。
		return attempted(fmt.Errorf("store: 恢复提交: %w", err), false), err
	}
	k, err := d.AdminKeyByKey(ctx, keyID)
	if err != nil {
		// Commit 已成功，只是读不回细节：文件必须保留（Committed 已经为真）。
		rep := &RecoverReport{Key: &AdminKey{KeyID: keyID}, Committed: true, Adopted: true,
			Err: fmt.Errorf("store: 恢复后读回管理凭据: %w", err)}
		return rep, rep.Err
	}
	return &RecoverReport{Key: k, Revoked: int(revoked64), Committed: true, Adopted: true}, nil
}

// --- 小工具 ---

// checkDSNPath 拒绝会破坏 file: URI 或不可预期的路径。
// 宁可让运维改一下目录名，也不要一个"看起来备份成功、其实读回的是另一个文件"的结果。
func checkDSNPath(p string) error {
	if p == "" || len(p) > 4096 {
		return fmt.Errorf("%w: 路径长度不合法", ErrBadConfig)
	}
	if strings.ContainsAny(p, "\"?#%&'") {
		return fmt.Errorf("%w: 路径含 URL/SQL 特殊字符，请换目录名", ErrBadConfig)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: 路径含控制字符", ErrBadConfig)
		}
	}
	return nil
}

// checkDestName 只接受一个纯文件名（不含目录分隔符），并限定 .db 后缀。
// 恢复的目标名如果来自命令行参数，就必须按不可信输入处理。
func checkDestName(s string) error {
	if s == "" || s != filepath.Base(s) || strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("%w: 恢复目标必须是纯文件名", ErrBadConfig)
	}
	if len(s) > 64 || !strings.HasSuffix(s, ".db") {
		return fmt.Errorf("%w: 恢复目标文件名须以 .db 结尾且不超过 64 字符", ErrBadConfig)
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return fmt.Errorf("%w: 恢复目标文件名字符不合法", ErrBadConfig)
		}
	}
	return nil
}

func sanitizeToken(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	if b.Len() > 16 {
		return b.String()[:16]
	}
	return b.String()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func fileSizeOrNil(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("store: 打开失败")
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("store: 计算哈希失败")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFileToTemp(src, dir, pattern string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("store: 打不开备份")
	}
	defer func() { _ = in.Close() }()

	out, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("store: 恢复目标不可写")
	}
	path := out.Name()
	remove := true
	defer func() {
		_ = out.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return "", fmt.Errorf("store: 复制失败")
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("store: 复制失败")
	}
	remove = false
	return path, nil
}

func publishNoOverwrite(staging, dest string) error {
	if err := os.Link(staging, dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("store: 恢复目标已存在，拒绝覆盖：%s", filepath.Base(dest))
		}
		return fmt.Errorf("store: 恢复文件落位失败")
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("store: 打不开备份")
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("store: 恢复目标不可写")
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("store: 复制失败")
	}
	return out.Close()
}

func syncFile(p string) error {
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("store: 落盘失败")
	}
	return nil
}

func syncDir(p string) error {
	d, err := os.Open(p)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// dropSidecars 收掉**只读**打开 WAL 头数据库时留下的 -wal/-shm。
//
// 为什么需要它：以 mode=ro 打开的连接不负责回收 WAL（只有最后一个可写连接干净关闭才会），
// 于是备份的一致性检查会在产物旁边留下两个空文件。它们没有内容，但会让"这份备份是单文件"
// 变成假话，也会让运维在搬运时不确定要不要一起拷。
// 非空的 -wal 一律**不删**并报错：那说明这个 .db 不是我们交付的完整快照，
// 缺的那半边状态正躺在 -wal 里（红线：绝不能把"半个数据库"标成成功备份）。
func dropSidecars(path string) error {
	if _, err := os.Stat(path + "-wal"); err == nil {
		if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
			return fmt.Errorf("%w: 产物旁有非空 -wal，不是一致快照", ErrBackupInvalid)
		}
	}
	return dropSidecarsSafe(path)
}

// dropSidecarsSafe 只用于"我们自己刚创建的临时文件"的清理路径：忽略不存在与错误。
func dropSidecarsSafe(path string) error {
	e1 := os.Remove(path + "-wal")
	e2 := os.Remove(path + "-shm")
	if e1 != nil && !errors.Is(e1, os.ErrNotExist) {
		return e1
	}
	if e2 != nil && !errors.Is(e2, os.ErrNotExist) {
		return e2
	}
	return nil
}

// finalizeSingleFile 把交付物收成"只有一个 .db 文件"：checkpoint(TRUNCATE) 把 WAL 折回主文件，
// 随后的干净 Close 删除 sidecar（实测：最后一个可写连接关闭后目录里只剩 a.db）。
// 恢复流程需要它，因为副本确实被可写连接动过；只读检查也可能再次留下 sidecar，所以放在最后一步。
func finalizeSingleFile(ctx context.Context, path string) error {
	if err := checkDSNPath(path); err != nil {
		return err
	}
	h, err := sql.Open("sqlite", fmt.Sprintf(dsnTemplate, path))
	if err != nil {
		return fmt.Errorf("store: 收尾打开失败")
	}
	h.SetMaxOpenConns(1)
	h.SetMaxIdleConns(1)
	_, execErr := h.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	closeErr := h.Close()
	if execErr != nil {
		return fmt.Errorf("store: WAL 收尾失败")
	}
	if closeErr != nil {
		return fmt.Errorf("store: 收尾关闭失败")
	}
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("store: 交付物仍带 %s，不该按单文件交付", filepath.Base(p))
		}
	}
	return nil
}
