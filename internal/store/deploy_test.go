package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 阶段 5A 的备份/恢复与就绪探针。这一组测试关心的是**失败形状**：
// 半成品不能叫成功、密钥配错不能静默可服务、恢复不能碰活库、旧凭据不能复活。

func fileSum(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// openSnapshot 以只读方式打开一个备份/恢复产物，用于在测试里核对内容。
func openSnapshot(t *testing.T, path string) *sql.DB {
	t.Helper()
	h, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开快照: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func countRows(t *testing.T, h *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := h.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func seedLedger(t *testing.T, d *DB, now time.Time) {
	t.Helper()
	s := mustSite(t, d, "alpha")
	c := &Challenge{
		ID: "ch-1", SiteID: s.ID, Mode: ModePowOnly, PowSeed: []byte("0123456789abcdef"),
		PowBits: 14, Status: statusIssued, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := d.InsertChallenge(t.Context(), c, testCaps); err != nil {
		t.Fatalf("InsertChallenge: %v", err)
	}
	if _, err := d.Redeem(t.Context(), "ch-1", now, time.Minute); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	q := &PoolQuestion{
		ID: "q-1", GeneratorID: "arithmetic-baseline-v1", Version: "v1", Difficulty: 1,
		Status: "ready", PayloadJSON: `{"a":1}`, Verifier: []byte(strings.Repeat("v", 48)),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := d.InsertQuestion(t.Context(), q, PoolLimits{MaxRows: 1000, MaxImageBytes: 1 << 20}); err != nil {
		t.Fatalf("InsertQuestion: %v", err)
	}
	if _, err := d.AddAdminKey(t.Context(), "bootstrap", "ak_oldadminkey0000000000000000000000000000000000000000000000"); err != nil {
		t.Fatalf("AddAdminKey: %v", err)
	}
}

func TestReadyHealthy(t *testing.T) {
	d := newDB(t)
	if err := d.Ready(t.Context()); err != nil {
		t.Fatalf("正常库被判为不就绪: %v", err)
	}
}

func TestReadyFailsAfterClose(t *testing.T) {
	d, _ := newDBAt(t, filepath.Join(t.TempDir(), "epocha.db"))
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := d.Ready(t.Context()); err == nil {
		t.Fatal("句柄已关闭却仍返回就绪")
	}
}

func TestReadyFailsWhenDBFileNotWritable(t *testing.T) {
	d, path := newDBAt(t, filepath.Join(t.TempDir(), "epocha.db"))
	if err := d.Ready(t.Context()); err != nil {
		t.Fatalf("前置条件：新库应就绪: %v", err)
	}
	// 只读挂载/权限漂移的最常见形状：文件本身不可写，读路径却仍然正常。
	if err := os.Chmod(path, 0o444); err != nil {
		t.Skipf("无法改权限（环境限制）: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if err := d.Ready(t.Context()); err == nil {
		t.Fatal("主文件不可写却返回就绪（WAL 库无法提交事务，必须 fail closed）")
	}
}

func TestReadyFailsWhenDataDirReadOnly(t *testing.T) {
	dir := t.TempDir()
	d, _ := newDBAt(t, filepath.Join(dir, "epocha.db"))
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Skipf("无法改目录权限（环境限制）: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if probe, err := os.CreateTemp(dir, "readonly-probe-*"); err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		// chmod 555 在 Windows ACL 语义下可能不阻止写入：权限前提不成立 ⇒ 不是 PASS 也不是 FAIL。
		t.Skip("当前环境 chmod 555 未实际阻止目录写入；只读目录行为须在 Linux 权限模型下验收")
	}
	if err := d.Ready(t.Context()); err == nil {
		t.Fatal("数据目录不可写却返回就绪（SQLite 建不了 -wal/-shm）")
	}
}

func TestReadyDoesNotChangeLedgerOrPool(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	d, path := newDBAt(t, filepath.Join(t.TempDir(), "epocha.db"))
	seedLedger(t, d, now)
	before := map[string]int64{}
	for _, q := range []string{`SELECT count(*) FROM sites`, `SELECT count(*) FROM challenges`,
		`SELECT count(*) FROM responses`, `SELECT count(*) FROM pool`, `SELECT count(*) FROM admin_keys`} {
		before[q] = countRows(t, openSnapshot(t, path), q)
	}
	for range 20 {
		if err := d.Ready(t.Context()); err != nil {
			t.Fatalf("Ready: %v", err)
		}
	}
	h := openSnapshot(t, path)
	for q, want := range before {
		if got := countRows(t, h, q); got != want {
			t.Errorf("%s: 就绪探针改变了行数 %d → %d", q, want, got)
		}
	}
}

func TestReadyHonoursContextDeadline(t *testing.T) {
	d := newDB(t)
	cctx, ccancel := context.WithTimeout(t.Context(), 10*time.Nanosecond)
	defer ccancel()
	// 允许实现以任意错误表达"来不及"，但绝不能是 nil：探针不可阻塞服务，也不可越过调用方的截止。
	if err := d.Ready(cctx); err == nil {
		t.Fatal("已过期的 context 仍返回就绪——探针的超时边界不存在")
	}
}

func TestBackupProducesConsistentSnapshotOfLiveWAL(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	d, path := newDBAt(t, filepath.Join(t.TempDir(), "epocha.db"))
	seedLedger(t, d, now)
	// 刻意不做 checkpoint：备份必须读到 WAL 里最新的那部分，否则"只 cp 主文件"的旧结论会重现。
	mainBefore := fileSum(t, path)
	if walSize(t, path) == 0 {
		t.Fatal("前置条件不成立：种子写入没有留在 -wal 里，这一轮测不到 WAL 快照路径")
	}
	dir := t.TempDir()

	m, err := Backup(t.Context(), BackupOptions{SrcPath: path, DestDir: dir, Now: now, Pepper: testPepper()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, m.Name+".db-wal")); !os.IsNotExist(err) {
		t.Fatal("备份产物带 -wal：快照必须是单文件，否则恢复会漏掉半边数据库")
	}
	h := openSnapshot(t, filepath.Join(dir, m.Name+".db"))
	// Redeem 之后 challenge 的权威状态是 redeemed，response 是它生成的新行：
	// 这两项都只存在于 WAL 里，快照读到它们才算证明"最新写入进了快照"。
	if got := countRows(t, h, `SELECT count(*) FROM challenges WHERE status='redeemed'`); got != 1 {
		t.Errorf("快照里 redeemed challenge = %d，期望 1（WAL 内容没进快照）", got)
	}
	if got := countRows(t, h, `SELECT count(*) FROM responses`); got != 1 {
		t.Errorf("快照里 response = %d，期望 1", got)
	}
	if got := countRows(t, h, `SELECT count(*) FROM pool WHERE status='ready'`); got != 1 {
		t.Errorf("快照里的 ready 题 = %d，期望 1", got)
	}
	if err := IntegrityCheck(t.Context(), filepath.Join(dir, m.Name+".db")); err != nil {
		t.Fatalf("快照一致性检查: %v", err)
	}
	if fileSum(t, path) != mainBefore {
		t.Fatal("备份改写了源库主文件——备份必须是只读动作")
	}
	if m.SchemaVersion != schemaVersion || m.BackupBytes <= 0 || len(m.SHA256) != 64 {
		t.Errorf("manifest 字段不完整: %+v", m)
	}
	if m.Counts["challenges_redeemed"] != 1 || m.Counts["pool_ready"] != 1 || m.Counts["admin_keys_active"] != 1 {
		t.Errorf("manifest 计数不符: %+v", m.Counts)
	}
	if m.AppVersion == "" {
		t.Error("manifest 没记应用版本：出问题时无法回溯是哪次构建做的备份")
	}
	// manifest 必须留下"源库当时正把新写入放在 WAL 里"这个事实，否则下次没人能解释
	// 为什么备份不能用 cp 主文件来做。
	if m.SourceWALB == 0 {
		t.Errorf("manifest 记的源 WAL 体积为 0：%+v", m)
	}
}

// walSize 返回 -wal 的字节数（不存在时 0）。
func walSize(t *testing.T, dbPath string) int64 {
	t.Helper()
	fi, err := os.Stat(dbPath + "-wal")
	if err != nil {
		return 0
	}
	return fi.Size()
}

func TestBackupRefusesToOverwriteAndLeavesNoPartial(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	d, path := newDBAt(t, filepath.Join(t.TempDir(), "epocha.db"))
	seedLedger(t, d, now)
	dir := t.TempDir()
	if _, err := Backup(t.Context(), BackupOptions{SrcPath: path, DestDir: dir, Now: now, Pepper: testPepper()}); err != nil {
		t.Fatalf("首次 Backup: %v", err)
	}
	// 同一时间戳 ⇒ 同一目标名：必须拒绝，而不是悄悄覆盖上一份备份。
	if _, err := Backup(t.Context(), BackupOptions{SrcPath: path, DestDir: dir, Now: now, Pepper: testPepper()}); err == nil {
		t.Fatal("重复目标名覆盖成功")
	}
	before := len(dirEntries(t, dir))

	ro := t.TempDir()
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Skipf("无法把目标目录改成只读: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	if probe, err := os.CreateTemp(ro, "readonly-probe-*"); err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		// chmod 555 在 Windows ACL 语义下可能不阻止写入：权限前提不成立 ⇒ 显式跳过，不冒充验收。
		t.Skip("当前环境 chmod 555 未实际阻止目录写入；只读目录行为须在 Linux 权限模型下验收")
	}
	if _, err := Backup(t.Context(), BackupOptions{SrcPath: path, DestDir: ro, Now: now.Add(time.Minute), Pepper: testPepper()}); err == nil {
		t.Fatal("目标目录不可写却报告成功")
	}
	if got := len(dirEntries(t, ro)); got != 0 {
		t.Errorf("失败后备选目录里留下 %d 个文件：半成品不能留在盘上冒充备份", got)
	}
	if len(dirEntries(t, dir)) != before {
		t.Error("失败连带污染了已有备份目录")
	}
}

func TestBackupRejectsMissingSourceAndUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	if _, err := Backup(t.Context(), BackupOptions{SrcPath: filepath.Join(dir, "nope.db"), DestDir: dir, Now: time.Now(), Pepper: testPepper()}); err == nil {
		t.Fatal("源库不存在却成功")
	}
	// 未来版本的库：备份一个本程序读不懂的东西没有意义，且会把误恢复带上线。
	fut := filepath.Join(dir, "future.db")
	h, err := sql.Open("sqlite", "file:"+fut)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Exec(`CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL); INSERT INTO meta(k,v) VALUES('schema_version','99')`); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	if _, err := Backup(t.Context(), BackupOptions{SrcPath: fut, DestDir: dir, Now: time.Now(), Pepper: testPepper()}); err == nil {
		t.Fatal("schema 版本高于程序支持时仍备份成功")
	}
}

func TestManifestAndBackupFilePairUp(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	d, path := newDBAt(t, filepath.Join(t.TempDir(), "epocha.db"))
	seedLedger(t, d, now)
	dir := t.TempDir()
	m, err := Backup(t.Context(), BackupOptions{SrcPath: path, DestDir: dir, Now: now, Pepper: testPepper()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	mp := filepath.Join(dir, m.Name+".manifest.json")
	if _, err := os.Stat(mp); err != nil {
		t.Fatalf("manifest 未写出: %v", err)
	}
	got, err := ReadManifest(mp)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.SHA256 != fileSum(t, filepath.Join(dir, m.Name+".db")) {
		t.Error("manifest 里的 sha256 与实际文件不一致")
	}
	// 篡改快照必须被发现（只按 size 或"非空"判断是不够的）。
	b, err := os.ReadFile(filepath.Join(dir, m.Name+".db"))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(filepath.Join(dir, m.Name+".db"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBackup(t.Context(), filepath.Join(dir, m.Name+".db"), mp); err == nil {
		t.Fatal("被篡改的快照通过了校验")
	}
}

func TestRestoreInvalidatesEverythingTheSnapshotCouldStillAccept(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	d, path := newDBAt(t, filepath.Join(t.TempDir(), "epocha.db"))
	seedLedger(t, d, now)
	site := mustSite(t, d, "beta")
	dir := t.TempDir()
	m, err := Backup(t.Context(), BackupOptions{SrcPath: path, DestDir: dir, Now: now, Pepper: testPepper()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	newAdmin := "ak_newadminkey00000000000000000000000000000000000000000000000000"
	out := filepath.Join(t.TempDir(), "restored")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := Restore(t.Context(), RestoreOptions{
		BackupPath: filepath.Join(dir, m.Name+".db"), ManifestPath: filepath.Join(dir, m.Name+".manifest.json"),
		OutDir: out, DestName: "epocha.db", Pepper: testPepper(), AdminKey: newAdmin, Now: now,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rep.DBPath != filepath.Join(out, "epocha.db") {
		t.Errorf("产物路径 = %s", rep.DBPath)
	}
	// 源快照必须还能被重新校验：恢复只读快照，不在原文件上动手。
	if err := VerifyBackup(t.Context(), filepath.Join(dir, m.Name+".db"), filepath.Join(dir, m.Name+".manifest.json")); err != nil {
		t.Fatalf("源快照被恢复过程改动了: %v", err)
	}

	rd, err := Open(t.Context(), rep.DBPath, testPepper())
	if err != nil {
		t.Fatalf("打开恢复产物: %v", err)
	}
	defer func() { _ = rd.Close() }() // sql.DB.Close 幂等，下面已显式关过一次
	if _, err := rd.MatchAdminKey(t.Context(), "ak_oldadminkey0000000000000000000000000000000000000000000000", now); err == nil {
		t.Error("快照里的旧管理凭据仍能鉴权")
	}
	if _, err := rd.MatchAdminKey(t.Context(), newAdmin, now); err != nil {
		t.Errorf("新的管理凭据不可用: %v", err)
	}
	if s, err := rd.SiteByKey(t.Context(), "sk-alpha"); err != nil {
		t.Fatalf("SiteByKey: %v", err)
	} else if s.Enabled {
		t.Error("恢复出来的站点仍是启用状态（必须默认禁用、人工确认后才开）")
	}
	if _, err := rd.FindSiteBySecret(t.Context(), "secret-alpha-SENTINEL", now); err == nil {
		t.Error("快照里的站点验证凭据仍可用：备份后被撤销的凭据会在旧快照里复活")
	}
	if _, err := rd.ConsumeResponse(t.Context(), "ch-1:resp", site.ID, now); err == nil {
		t.Error("恢复前签发的 response 仍可被兑换成验证成功")
	}
	// 行内容核对完后必须把所有句柄收掉再查 sidecar：任何打开（哪怕只读）都会给
	// WAL 库留下 -wal/-shm，用自己的活连接去断言"交付物是单文件"只会测到自己。
	h, err := sql.Open("sqlite", "file:"+rep.DBPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开恢复产物做计数: %v", err)
	}
	for _, tc := range []struct {
		query string
		label string
	}{
		{`SELECT count(*) FROM pool`, "题池"},
		{`SELECT count(*) FROM challenges WHERE status='issued'`, "issued challenge"},
		{`SELECT count(*) FROM responses`, "response"},
	} {
		if got := countRows(t, h, tc.query); got != 0 {
			t.Errorf("恢复产物里还有 %d 条 %s（应全部失效）", got, tc.label)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatalf("关闭计数句柄: %v", err)
	}
	if err := rd.Close(); err != nil {
		t.Fatalf("关闭恢复产物句柄: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(rep.DBPath + suffix); !os.IsNotExist(err) {
			t.Errorf("恢复产物带 %s：交付单文件才不会让『只搬了半边数据库』的恢复成立", suffix)
		}
	}
	// 恢复报告里不得出现任何凭据明文（按值扫描）。
	if strings.Contains(rep.String(), newAdmin) || strings.Contains(rep.String(), "secret-alpha-SENTINEL") {
		t.Error("恢复报告回显了凭据")
	}
}

func TestRestoreRefusesUnpairedKeysAndForeignFiles(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	d, path := newDBAt(t, filepath.Join(t.TempDir(), "epocha.db"))
	seedLedger(t, d, now)
	dir := t.TempDir()
	m, err := Backup(t.Context(), BackupOptions{SrcPath: path, DestDir: dir, Now: now, Pepper: testPepper()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	bp := filepath.Join(dir, m.Name+".db")
	mp := filepath.Join(dir, m.Name+".manifest.json")

	// 换一个 pepper：与这份快照不配对，绝不能变成"可服务状态"。
	other := []byte(strings.Repeat("q", 32))
	out := filepath.Join(t.TempDir(), "restored")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(t.Context(), RestoreOptions{
		BackupPath: bp, ManifestPath: mp, OutDir: out, DestName: "epocha.db",
		Pepper: other, AdminKey: "ak_x000000000000000000000000000000000000000000000000000000", Now: now,
	}); err == nil {
		t.Fatal("pepper 与快照不配对却恢复成功")
	}
	if _, err := os.Stat(filepath.Join(out, "epocha.db")); err == nil {
		t.Error("被拒绝的恢复留下了数据库文件")
	}
	// 显式承担 pepper 轮换的后果才放行。
	rep, err := Restore(t.Context(), RestoreOptions{
		BackupPath: bp, ManifestPath: mp, OutDir: out, DestName: "epocha.db",
		Pepper: other, RotatePepper: true, AdminKey: "ak_y000000000000000000000000000000000000000000000000000000", Now: now,
	})
	if err != nil {
		t.Fatalf("RotatePepper 路径应放行: %v", err)
	}
	if !rep.PepperRotated {
		t.Error("报告没标出这是换 pepper 的恢复")
	}
	// 目标已存在：不许覆盖。
	if _, err := Restore(t.Context(), RestoreOptions{
		BackupPath: bp, ManifestPath: mp, OutDir: out, DestName: "epocha.db",
		Pepper: testPepper(), AdminKey: "ak_z000000000000000000000000000000000000000000000000000000", Now: now,
	}); err == nil {
		t.Fatal("恢复覆盖了已有文件")
	}
	// 缺 manifest、manifest 与文件不配对、备份不是 SQLite 文件。
	if _, err := Restore(t.Context(), RestoreOptions{
		BackupPath: bp, OutDir: t.TempDir(), DestName: "epocha.db",
		Pepper: testPepper(), AdminKey: "ak_w000000000000000000000000000000000000000000000000000000", Now: now,
	}); err == nil {
		t.Fatal("没有 manifest 也能恢复")
	}
	notdb := filepath.Join(dir, "not-a-db.db")
	if err := os.WriteFile(notdb, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(t.Context(), RestoreOptions{
		BackupPath: notdb, ManifestPath: mp, OutDir: t.TempDir(), DestName: "epocha.db",
		Pepper: testPepper(), AdminKey: "ak_v000000000000000000000000000000000000000000000000000000", Now: now,
	}); err == nil {
		t.Fatal("非 SQLite 文件被当成备份接受了")
	}
}

func TestPepperFingerprintIsOneWayAndConstantTime(t *testing.T) {
	a, b := PepperFingerprint([]byte(strings.Repeat("p", 32))), PepperFingerprint([]byte(strings.Repeat("q", 32)))
	if a == b {
		t.Fatal("不同 pepper 指纹相同")
	}
	if a != PepperFingerprint([]byte(strings.Repeat("p", 32))) {
		t.Fatal("同一 pepper 指纹不稳定")
	}
	if len(a) != 64 || strings.Contains(a, "p") && strings.Count(a, "p") > 4 {
		t.Errorf("指纹形态异常: %s", a)
	}
	if FingerprintMatches(a, []byte(strings.Repeat("q", 32))) {
		t.Error("指纹比对把不配对的 pepper 判为配对")
	}
	if !FingerprintMatches(a, []byte(strings.Repeat("p", 32))) {
		t.Error("指纹比对把配对的 pepper 判为不配对")
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	fs, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, filepath.Join(dir, f.Name()))
	}
	return out
}

// --- 管理访问的本地恢复（ADR-014 第 5 条的落点）---
//
// 钉住的是那条真实存在过的锁死路径：撤销最后一把管理凭据之后，
// 引导逻辑（只在"库里没有任何记录"时登记）永远不会再生效，而镜像里没有 sqlite3 可以手工改库。

func TestRecoverAdminKeyAfterLastRevocation(t *testing.T) {
	ctx := context.Background()
	d, _ := newDBAt(t, filepath.Join(t.TempDir(), "a.db"))
	oldKey := strings.Repeat("o", 40)
	if _, _, err := d.BootstrapAdminKey(ctx, oldKey, "bootstrap"); err != nil {
		t.Fatalf("BootstrapAdminKey: %v", err)
	}
	// 撤销它：此后库里"有记录但全部无效"，引导逻辑必须完全忽略 env。
	ak, err := d.ListAdminKeys(ctx)
	if err != nil || len(ak) != 1 {
		t.Fatalf("ListAdminKeys: %v n=%d", err, len(ak))
	}
	if _, err := d.RevokeAdminKey(ctx, ak[0].KeyID, time.Now().UTC()); err != nil {
		t.Fatalf("RevokeAdminKey: %v", err)
	}
	if _, _, err := d.BootstrapAdminKey(ctx, strings.Repeat("n", 40), "bootstrap-2"); err != nil {
		t.Fatalf("BootstrapAdminKey: %v", err)
	}
	if _, err := d.MatchAdminKey(ctx, strings.Repeat("n", 40), time.Now()); err == nil {
		t.Fatal("前置条件不成立：这条路径本来就该是锁死的")
	}

	fresh := strings.Repeat("r", 40)
	rep, err := d.RecoverAdminKey(ctx, fresh, time.Now().UTC())
	if err != nil {
		t.Fatalf("RecoverAdminKey: %v", err)
	}
	if rep.Revoked != 0 {
		t.Errorf("Revoked = %d，期望 0（旧的那把在这一步之前就已经撤销）", rep.Revoked)
	}
	if !rep.Committed || !rep.Adopted {
		t.Errorf("成功恢复的阶段结论不对: Committed=%v Adopted=%v", rep.Committed, rep.Adopted)
	}
	got := rep.Key
	if got.KeyID == "" || !strings.HasPrefix(got.Label, recoverLabelPrefix) {
		t.Errorf("恢复登记的管理凭据形状不对: %+v", got)
	}
	if _, err := d.MatchAdminKey(ctx, fresh, time.Now()); err != nil {
		t.Errorf("恢复后的新凭据无法鉴权: %v", err)
	}
	if _, err := d.MatchAdminKey(ctx, oldKey, time.Now()); err == nil {
		t.Error("被撤销的旧凭据在恢复后竟然可用")
	}
	// 审计必须留下事实，且不含凭据值。
	entries, err := d.ListAudit(ctx, 0, 50)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Action == "admin.recover_local" {
			found = true
		}
		if strings.Contains(fmt.Sprint(e), fresh) || strings.Contains(fmt.Sprint(e), oldKey) {
			t.Errorf("审计条目带出了凭据值: %+v", e)
		}
	}
	if !found {
		t.Error("恢复动作没有写审计")
	}
}

func TestRecoverAdminKeyRevokesLiveKeysAndIsRepeatable(t *testing.T) {
	ctx := context.Background()
	d, _ := newDBAt(t, filepath.Join(t.TempDir(), "b.db"))
	if _, err := d.AddAdminKey(ctx, "ops-1", strings.Repeat("a", 40)); err != nil {
		t.Fatalf("AddAdminKey: %v", err)
	}
	if _, err := d.AddAdminKey(ctx, "ops-2", strings.Repeat("b", 40)); err != nil {
		t.Fatalf("AddAdminKey: %v", err)
	}
	first, err := d.RecoverAdminKey(ctx, strings.Repeat("c", 40), time.Now().UTC())
	if err != nil {
		t.Fatalf("RecoverAdminKey: %v", err)
	}
	if first.Revoked != 2 {
		t.Errorf("Revoked = %d，期望 2（两把有效凭据都要撤销，否则恢复等于留后门）", first.Revoked)
	}
	for _, k := range []string{strings.Repeat("a", 40), strings.Repeat("b", 40)} {
		if _, err := d.MatchAdminKey(ctx, k, time.Now()); err == nil {
			t.Error("存量有效凭据在恢复后仍可用")
		}
	}
	// 再做一次：label 必须不冲突（库里是 UNIQUE），且上一次的凭据也要被撤销。
	second, err := d.RecoverAdminKey(ctx, strings.Repeat("d", 40), time.Now().UTC())
	if err != nil {
		t.Fatalf("第二次 RecoverAdminKey（label 冲突？）: %v", err)
	}
	if second.Revoked != 1 {
		t.Errorf("第二次 Revoked = %d，期望 1", second.Revoked)
	}
	if second.Key.KeyID == first.Key.KeyID {
		t.Error("两次恢复得到同一个 key_id")
	}
	if _, err := d.MatchAdminKey(ctx, strings.Repeat("c", 40), time.Now()); err == nil {
		t.Error("第一次恢复的凭据在第二次之后仍可用")
	}
	if _, err := d.MatchAdminKey(ctx, strings.Repeat("d", 40), time.Now()); err != nil {
		t.Errorf("第二次恢复的凭据不可用: %v", err)
	}
}

func TestRecoverAdminKeyRefusesShortKey(t *testing.T) {
	ctx := context.Background()
	d, _ := newDBAt(t, filepath.Join(t.TempDir(), "c.db"))
	before, err := d.ListAdminKeys(ctx)
	if err != nil {
		t.Fatalf("ListAdminKeys: %v", err)
	}
	rep, err := d.RecoverAdminKey(ctx, "short", time.Now())
	if err == nil {
		t.Fatal("短凭据必须被拒绝")
	}
	// key_id 还没生成就失败了 ⇒ 库里不可能有本次写入 ⇒ 报告必须是 nil，
	// 调用方据此才敢清理自己写的凭据文件。
	if rep != nil {
		t.Errorf("未生成 key_id 的失败却给出了报告: %+v", rep)
	}
	if rep != nil && rep.KeepCredentialFile() {
		t.Error("这种失败路径应当允许清理凭据文件")
	}
	after, err := d.ListAdminKeys(ctx)
	if err != nil {
		t.Fatalf("ListAdminKeys(after): %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("失败的恢复改动了 admin_keys 行数 %d → %d", len(before), len(after))
	}
}

// AdminKeyAdopted 是"能不能删掉那把唯一副本凭据文件"的依据，所以它必须回答
// **数据库侧的事实**，而不是本进程事务里未提交的猜测。
func TestAdminKeyAdoptedUsesCommittedState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "adopted.db")
	d, _ := newDBAt(t, path)

	const pending = "k_pending_uncommitted"
	tx, err := d.h.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	// 先证明"同连接看得见自己的未提交写入"——这正是不能拿写句柄自查的原因。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO admin_keys(key_id, label, key_mac, created_at) VALUES(?,?,?,?)`,
		pending, "pending", strings.Repeat("0", 64), formatTime(time.Now().UTC())); err != nil {
		_ = tx.Rollback()
		t.Fatalf("插入未提交凭据: %v", err)
	}
	var same int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM admin_keys WHERE key_id=?`, pending).Scan(&same); err != nil {
		_ = tx.Rollback()
		t.Fatalf("事务内读回: %v", err)
	}
	if same != 1 {
		_ = tx.Rollback()
		t.Fatal("前置不成立：事务里看不到自己的写入，这条测试就没意义")
	}
	// 只读核查必须是 false：库里**尚未**采用它。真在这里返回 true，
	// 调用方就会保留一把永远验不过的凭据文件，并把"恢复失败"说成"已生效"。
	adopted, err := AdminKeyAdopted(ctx, path, pending)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("AdminKeyAdopted: %v", err)
	}
	if adopted {
		_ = tx.Rollback()
		t.Error("只读核查看见了未提交的行 ⇒ 会把“没采用”报成“已采用”")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if a, err := AdminKeyAdopted(ctx, path, pending); err != nil || !a {
		t.Errorf("提交之后应查到: adopted=%v err=%v", a, err)
	}
	// "存在"不等于"还能用"：撤销之后必须回到 false。
	if _, err := d.RevokeAdminKey(ctx, pending, time.Now().UTC()); err != nil {
		t.Fatalf("RevokeAdminKey: %v", err)
	}
	if a, err := AdminKeyAdopted(ctx, path, pending); err != nil || a {
		t.Errorf("已撤销的凭据仍被报成已采用: adopted=%v err=%v", a, err)
	}
	// 查不到（false,nil）与查不了（false,err）是两种结论，不能混：
	// 前者可以安全清理凭据文件，后者不可以。
	if a, err := AdminKeyAdopted(ctx, path, "k_never_existed"); err != nil || a {
		t.Errorf("未知 key_id 应返回 (false,nil): %v %v", a, err)
	}
	if _, err := AdminKeyAdopted(ctx, filepath.Join(dir, "missing.db"), "k_x"); err == nil {
		t.Error("库文件不存在时必须报“核查失败”，而不是“没采用”")
	}
}

// 提交之前的失败必须是**核查过**的"未采用"，而不是从 error 非空推出来的。
func TestRecoverAdminKeyPreCommitFailureIsConfirmedNotAdopted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "precommit.db")
	d, _ := newDBAt(t, path)
	// 制造一个"key_id 已生成、事务里已写入、提交之前才失败"的形状：
	// 拿掉 audit 表，审计那条 INSERT 就会失败，deferred Rollback 丢弃前面的写入。
	if _, err := d.h.ExecContext(ctx, `DROP TABLE audit`); err != nil {
		t.Fatalf("DROP TABLE audit: %v", err)
	}
	var before int
	if err := d.h.QueryRowContext(ctx, `SELECT COUNT(1) FROM admin_keys`).Scan(&before); err != nil {
		t.Fatalf("计数: %v", err)
	}
	rep, err := d.RecoverAdminKey(ctx, strings.Repeat("e", 40), time.Now().UTC())
	if err == nil {
		t.Fatal("审计插入应当失败")
	}
	if rep == nil {
		t.Fatal("key_id 已生成，必须给出阶段报告（调用方要靠它决定是否删凭据文件）")
	}
	if rep.Committed {
		t.Error("提交之前就失败，却报成已提交")
	}
	if rep.Key == nil || rep.Key.KeyID == "" {
		t.Errorf("报告缺 key_id: %+v", rep.Key)
	}
	if rep.Adopted {
		t.Error("库里确认没有这把，却报成已采用")
	}
	if rep.VerifyErr != nil {
		t.Errorf("只读核查本身应当成功: %v", rep.VerifyErr)
	}
	if rep.KeepCredentialFile() {
		t.Error("这条路径应当允许清理本次凭据文件（已确认库里没采用）")
	}
	var after int
	if err := d.h.QueryRowContext(ctx, `SELECT COUNT(1) FROM admin_keys`).Scan(&after); err != nil {
		t.Fatalf("计数(after): %v", err)
	}
	if after != before {
		t.Errorf("失败的恢复改动了 admin_keys: %d → %d（回滚没生效？）", before, after)
	}
}
