package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// The restore blocker is a real SQLite scalar function installed in the test
// driver. The trigger is copied into the backup by VACUUM INTO, so the pause
// happens inside the actual transform transaction rather than in a fake hook.
var (
	restoreBlockRegisterOnce sync.Once
	restoreBlockRegisterErr  error
	restoreBlockMu           sync.Mutex
	restoreBlockEntered      chan struct{}
	restoreBlockRelease      chan struct{}
	restoreBlockHit          bool
)

func ensureRestoreBlockFunction(t *testing.T) {
	t.Helper()
	restoreBlockRegisterOnce.Do(func() {
		restoreBlockRegisterErr = sqlite.RegisterScalarFunction(
			"epocha_test_restore_block", 0,
			func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
				restoreBlockMu.Lock()
				entered, release := restoreBlockEntered, restoreBlockRelease
				first := entered != nil && !restoreBlockHit
				if first {
					restoreBlockHit = true
					close(entered)
				}
				restoreBlockMu.Unlock()
				if first {
					<-release
				}
				return nil, nil
			},
		)
	})
	if restoreBlockRegisterErr != nil {
		t.Fatalf("注册恢复阻塞函数: %v", restoreBlockRegisterErr)
	}
}

func armRestoreBlock(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	ensureRestoreBlockFunction(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }

	restoreBlockMu.Lock()
	restoreBlockEntered = entered
	restoreBlockRelease = release
	restoreBlockHit = false
	restoreBlockMu.Unlock()
	t.Cleanup(func() {
		restoreBlockMu.Lock()
		restoreBlockEntered = nil
		restoreBlockRelease = nil
		restoreBlockHit = false
		restoreBlockMu.Unlock()
		releaseFn()
	})
	return entered, releaseFn
}

func TestRecoverAdminKeyRevokesEveryCurrentlyAcceptedKey(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	now := time.Date(2026, 11, 2, 12, 0, 0, 123456789, time.UTC)
	activePlain := strings.Repeat("a", 40)
	gracePlain := strings.Repeat("g", 40)
	gracePlain2 := strings.Repeat("h", 40)
	expiredPlain := strings.Repeat("e", 40)
	for _, tc := range []struct {
		label string
		plain string
	}{
		{label: "active", plain: activePlain},
		{label: "grace", plain: gracePlain},
		{label: "grace2", plain: gracePlain2},
		{label: "expired", plain: expiredPlain},
	} {
		if _, err := d.AddAdminKey(ctx, tc.label, tc.plain); err != nil {
			t.Fatalf("AddAdminKey(%s): %v", tc.label, err)
		}
	}
	keys, err := d.ListAdminKeys(ctx)
	if err != nil || len(keys) != 4 {
		t.Fatalf("ListAdminKeys: %v n=%d", err, len(keys))
	}
	var graceID, graceID2, expiredID string
	for _, key := range keys {
		switch key.Label {
		case "grace":
			graceID = key.KeyID
		case "grace2":
			graceID2 = key.KeyID
		case "expired":
			expiredID = key.KeyID
		}
	}
	graceUntil := now.Add(time.Hour)
	expiredAt := now.Add(-time.Hour)
	if _, err := d.RevokeAdminKey(ctx, graceID, graceUntil); err != nil {
		t.Fatalf("设置 grace 截止: %v", err)
	}
	if _, err := d.RevokeAdminKey(ctx, graceID2, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("设置第二把 grace 截止: %v", err)
	}
	if _, err := d.RevokeAdminKey(ctx, expiredID, expiredAt); err != nil {
		t.Fatalf("设置 expired 截止: %v", err)
	}
	if _, err := d.MatchAdminKey(ctx, gracePlain, now); err != nil {
		t.Fatalf("前置条件：grace key 在恢复前应可用: %v", err)
	}

	newPlain := strings.Repeat("n", 40)
	rep, err := d.RecoverAdminKey(ctx, newPlain, now)
	if err != nil {
		t.Fatalf("RecoverAdminKey: %v", err)
	}
	if rep.Revoked != 3 {
		t.Errorf("Revoked=%d，期望撤销 active + 两把未到期 grace", rep.Revoked)
	}
	for name, plain := range map[string]string{
		"active": activePlain, "grace": gracePlain, "grace2": gracePlain2, "expired": expiredPlain,
	} {
		for _, at := range []time.Time{now, now.Add(30 * time.Minute)} {
			if _, err := d.MatchAdminKey(ctx, plain, at); !errors.Is(err, ErrNotFound) {
				t.Errorf("恢复后旧 %s key 在 %s 仍可用: %v", name, at, err)
			}
		}
	}
	if _, err := d.MatchAdminKey(ctx, newPlain, now); err != nil {
		t.Fatalf("新恢复凭据不可用: %v", err)
	}
	graceKey, err := d.AdminKeyByKey(ctx, graceID)
	if err != nil {
		t.Fatalf("读取 grace key: %v", err)
	}
	if !graceKey.RevokedAt.Equal(now) {
		t.Errorf("grace key 的截止时间=%s，期望恢复时刻 %s", graceKey.RevokedAt, now)
	}
	graceKey2, err := d.AdminKeyByKey(ctx, graceID2)
	if err != nil {
		t.Fatalf("读取第二把 grace key: %v", err)
	}
	if !graceKey2.RevokedAt.Equal(now) {
		t.Errorf("第二把 grace key 的截止时间=%s，期望恢复时刻 %s", graceKey2.RevokedAt, now)
	}
	expiredKey, err := d.AdminKeyByKey(ctx, expiredID)
	if err != nil {
		t.Fatalf("读取 expired key: %v", err)
	}
	if !expiredKey.RevokedAt.Equal(expiredAt) {
		t.Errorf("已过期 key 的历史截止时间被改写为 %s，期望保持 %s", expiredKey.RevokedAt, expiredAt)
	}
}

func TestRecoverAdminKeyAuditFailureRollsBackGraceRevocation(t *testing.T) {
	ctx := context.Background()
	d := newDB(t)
	now := time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC)
	activePlain := strings.Repeat("a", 40)
	gracePlain := strings.Repeat("g", 40)
	if _, err := d.AddAdminKey(ctx, "active", activePlain); err != nil {
		t.Fatalf("AddAdminKey(active): %v", err)
	}
	grace, err := d.AddAdminKey(ctx, "grace", gracePlain)
	if err != nil {
		t.Fatalf("AddAdminKey(grace): %v", err)
	}
	graceUntil := now.Add(time.Hour)
	if _, err := d.RevokeAdminKey(ctx, grace.KeyID, graceUntil); err != nil {
		t.Fatalf("设置 grace 截止: %v", err)
	}
	if _, err := d.h.ExecContext(ctx, `DROP TABLE audit`); err != nil {
		t.Fatalf("DROP TABLE audit: %v", err)
	}

	rep, err := d.RecoverAdminKey(ctx, strings.Repeat("n", 40), now)
	if err == nil {
		t.Fatal("审计写入失败却报告恢复成功")
	}
	if rep == nil || rep.Committed || rep.Adopted || rep.VerifyErr != nil {
		t.Fatalf("审计失败后的阶段报告不对: %+v", rep)
	}
	for name, plain := range map[string]string{"active": activePlain, "grace": gracePlain} {
		if _, err := d.MatchAdminKey(ctx, plain, now); err != nil {
			t.Errorf("审计失败回滚后旧 %s key 不可用: %v", name, err)
		}
	}
	gotGrace, err := d.AdminKeyByKey(ctx, grace.KeyID)
	if err != nil {
		t.Fatalf("读取 grace key: %v", err)
	}
	if !gotGrace.RevokedAt.Equal(graceUntil) {
		t.Errorf("审计失败回滚改写了 grace 截止时间: got=%s want=%s", gotGrace.RevokedAt, graceUntil)
	}
	if _, err := d.AdminKeyByKey(ctx, rep.Key.KeyID); !errors.Is(err, ErrNotFound) {
		t.Errorf("审计失败回滚后新 key 仍存在: %v", err)
	}
}

func TestRestoreDoesNotPublishFinalNameBeforeTransformCompletes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC)
	source, sourcePath := newDBAt(t, filepath.Join(t.TempDir(), "source.db"))
	seedLedger(t, source, now)
	ensureRestoreBlockFunction(t)
	if _, err := source.h.ExecContext(ctx, `
CREATE TRIGGER restore_test_block AFTER UPDATE OF enabled ON sites
BEGIN SELECT epocha_test_restore_block(); END`); err != nil {
		t.Fatalf("创建恢复阻塞 trigger: %v", err)
	}
	backupDir := t.TempDir()
	m, err := Backup(ctx, BackupOptions{SrcPath: sourcePath, DestDir: backupDir, Pepper: testPepper(), Now: now})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	backupPath := filepath.Join(backupDir, m.Name+".db")
	manifestPath := filepath.Join(backupDir, m.Name+manifestSuffix)
	// Prove the fault is carried by the real SQLite snapshot, not only by the source.
	h := openSnapshot(t, backupPath)
	var triggerSQL string
	if err := h.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='restore_test_block'`).Scan(&triggerSQL); err != nil {
		t.Fatalf("快照没有复制恢复 trigger: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("关闭快照校验句柄: %v", err)
	}
	if !strings.Contains(triggerSQL, "epocha_test_restore_block") {
		t.Fatalf("快照 trigger 内容不对: %s", triggerSQL)
	}

	entered, release := armRestoreBlock(t)
	outDir := t.TempDir()
	dest := filepath.Join(outDir, "restored.db")
	oldPartial := dest + partialSuffix
	if err := os.WriteFile(oldPartial, []byte("operator-owned-partial"), 0o600); err != nil {
		t.Fatalf("写入已有 partial: %v", err)
	}
	type restoreResult struct {
		rep *RestoreReport
		err error
	}
	result := make(chan restoreResult, 1)
	go func() {
		rep, err := Restore(ctx, RestoreOptions{
			BackupPath: backupPath, ManifestPath: manifestPath, OutDir: outDir,
			DestName: "restored.db", Pepper: testPepper(),
			AdminKey: strings.Repeat("n", 40), Now: now,
		})
		result <- restoreResult{rep: rep, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		release()
		t.Fatal("恢复没有进入真实 transform trigger")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("保守失效尚未完成时最终文件名已可见: stat err=%v", err)
	}
	release()
	got := <-result
	if got.err != nil {
		t.Fatalf("释放 transform 后恢复失败: %v", got.err)
	}
	if got.rep == nil || got.rep.DBPath != dest {
		t.Fatalf("恢复报告不对: %+v", got.rep)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("恢复成功后最终文件不存在: %v", err)
	}
	if body, err := os.ReadFile(oldPartial); err != nil || string(body) != "operator-owned-partial" {
		t.Errorf("恢复改动或删除了已有用户 partial: err=%v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dest + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("最终库仍带 %s: %v", suffix, err)
		}
	}
}

func TestRestorePublishNeverOverwritesExistingTarget(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	dest := filepath.Join(dir, "final.db")
	if err := os.WriteFile(staging, []byte("completed-restored-copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("existing-user-database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishNoOverwrite(staging, dest); err == nil {
		t.Fatal("无覆盖发布覆盖了已有目标")
	}
	for path, want := range map[string]string{staging: "completed-restored-copy", dest: "existing-user-database"} {
		if body, err := os.ReadFile(path); err != nil || string(body) != want {
			t.Errorf("发布拒绝后文件改变: %s err=%v", filepath.Base(path), err)
		}
	}
}

func TestRestoreFailureNeverLeavesFinalName(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	source, sourcePath := newDBAt(t, filepath.Join(t.TempDir(), "source.db"))
	seedLedger(t, source, now)
	backupDir := t.TempDir()
	m, err := Backup(ctx, BackupOptions{SrcPath: sourcePath, DestDir: backupDir, Pepper: testPepper(), Now: now})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	backupPath := filepath.Join(backupDir, m.Name+".db")
	manifestPath := filepath.Join(backupDir, m.Name+manifestSuffix)
	if err := source.Close(); err != nil {
		t.Fatalf("关闭源库: %v", err)
	}
	// Keep a valid SQLite file and integrity_check result, but make the later
	// restore count phase fail. This is a real damaged-backup shape that must
	// never be published under the final destination name.
	h, err := sql.Open("sqlite", "file:"+backupPath)
	if err != nil {
		t.Fatalf("打开快照做故障注入: %v", err)
	}
	if _, err := h.Exec(`DROP TABLE pool`); err != nil {
		_ = h.Close()
		t.Fatalf("删除 pool 以注入计数故障: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("关闭注入句柄: %v", err)
	}
	if err := rewriteRestoreManifest(t, backupPath, manifestPath); err != nil {
		t.Fatalf("更新故障快照 manifest: %v", err)
	}

	outDir := t.TempDir()
	_, err = Restore(ctx, RestoreOptions{
		BackupPath: backupPath, ManifestPath: manifestPath, OutDir: outDir,
		DestName: "restored.db", Pepper: testPepper(),
		AdminKey: strings.Repeat("n", 40), Now: now,
	})
	if err == nil {
		t.Fatal("计数阶段故障却报告恢复成功")
	}
	for _, name := range []string{"restored.db", "restored.db-wal", "restored.db-shm", "restored.db.partial"} {
		if _, statErr := os.Stat(filepath.Join(outDir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("恢复失败后留下 %s，可能被误当成最终库: stat err=%v", name, statErr)
		}
	}
}

func rewriteRestoreManifest(t *testing.T, backupPath, manifestPath string) error {
	t.Helper()
	m, err := ReadManifest(manifestPath)
	if err != nil {
		return err
	}
	fi, err := os.Stat(backupPath)
	if err != nil {
		return err
	}
	m.BackupBytes = fi.Size()
	m.SHA256 = fileSum(t, backupPath)
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, body, 0o600)
}
