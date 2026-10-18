package store

// 阶段 5B：运行锁的语义测试。
//
// 这里测的是**锁本体**的性质（冲突、释放、陈旧文件、权限形状）。
// 真正的跨进程场景（两个服务抢同一目录、离线运维撞服务、SIGKILL 后可重新取得）
// 在 cmd/server 与 cmd/ops 的测试里用真实子进程做——锁的价值恰恰在进程边界上，
// 只在同一个进程里测它等于没测。

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAcquireDataLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "epocha.db")

	first, err := AcquireDataLock(db)
	if err != nil {
		t.Fatalf("首次取得锁: %v", err)
	}
	defer func() { _ = first.Close() }()

	if first.Path() != filepath.Join(dir, LockFileName) {
		t.Errorf("锁文件路径 = %s，期望在数据目录里", first.Path())
	}
	// 第二个 fd 是**另一个打开文件描述**：flock 的锁挂在 open file description 上，
	// 所以同进程内的第二次尝试同样应被拒（Linux 手册明确这条；本机已实测）。
	second, err := AcquireDataLock(db)
	if err == nil {
		_ = second.Close()
		t.Fatal("第二次取得锁竟然成功——那它保护不了任何东西")
	}
	if !errors.Is(err, ErrLockHeld) {
		t.Errorf("冲突错误 = %v，期望 ErrLockHeld", err)
	}
	if second != nil {
		t.Error("失败路径返回了非 nil 的锁句柄")
	}

	// 释放后必须能重新取得，且**不需要**删除锁文件。
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	third, err := AcquireDataLock(db)
	if err != nil {
		t.Fatalf("释放后重新取得失败: %v", err)
	}
	_ = third.Close()
}

func TestLockFileIsCreatedPrivateAndNeverRemoved(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "epocha.db")
	l, err := AcquireDataLock(db)
	if err != nil {
		t.Fatalf("AcquireDataLock: %v", err)
	}
	fi, err := os.Stat(l.Path())
	if err != nil {
		t.Fatalf("锁文件不存在: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("锁文件权限 = %o，期望 600（同机其它身份不该能持有这把锁）", fi.Mode().Perm())
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(l.Path()); err != nil {
		t.Errorf("Close 之后锁文件不见了：%v——删除它就是双写的入口（两个进程会锁到两个 inode）", err)
	}
}

func TestStaleLockFileDoesNotBlockStartup(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, LockFileName)
	// 崩溃、断电、容器被杀都可能留下一个没人持有的锁文件。
	// flock 的失效方向必须是"文件在但没被持有 ⇒ 可直接取得"，否则就要引入
	// "清理陈旧锁"的逻辑——而那正是 PID 文件方案的事故来源。
	if err := os.WriteFile(lockPath, []byte("left over by a killed process\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireDataLock(filepath.Join(dir, "epocha.db"))
	if err != nil {
		t.Fatalf("陈旧的锁文件阻止了取得锁: %v", err)
	}
	_ = l.Close()

	// 内容必须被无视：任何"读锁文件里的 PID 再决定"的实现都会在这里露馅。
	if err := os.WriteFile(lockPath, []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l2, err := AcquireDataLock(filepath.Join(dir, "epocha.db"))
	if err != nil {
		t.Fatalf("锁文件里写着别人家的 PID 就取不到锁: %v", err)
	}
	_ = l2.Close()
}

func TestAcquireDataLockErrorFamilies(t *testing.T) {
	t.Run("空路径直接拒绝", func(t *testing.T) {
		if _, err := AcquireDataLock(""); !errors.Is(err, ErrLockAccess) {
			t.Errorf("err = %v，期望 ErrLockAccess", err)
		}
	})
	t.Run("目录不存在是 access 不是 held", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope", "epocha.db")
		_, err := AcquireDataLock(missing)
		if err == nil {
			t.Fatal("目录不存在却成功了")
		}
		if errors.Is(err, ErrLockHeld) {
			t.Errorf("把“没人能用这个目录”读成“被占用”：%v", err)
		}
		if !errors.Is(err, ErrLockAccess) {
			t.Errorf("err = %v，期望 ErrLockAccess", err)
		}
	})
	t.Run("锁文件不可写是 access 不是 held", func(t *testing.T) {
		dir := t.TempDir()
		db := filepath.Join(dir, "epocha.db")
		l, err := AcquireDataLock(db)
		if err != nil {
			t.Fatalf("前置取得: %v", err)
		}
		// 属主自己也会受权限位约束：0400 之后 O_RDWR 直接 EACCES。
		if err := os.Chmod(l.Path(), 0o400); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		_ = l.Close()
		_, err = AcquireDataLock(db)
		if err == nil {
			t.Fatal("不可写的锁文件竟然能取得锁")
		}
		if !errors.Is(err, ErrLockAccess) {
			t.Errorf("err = %v，期望 ErrLockAccess（“权限不对”与“被占用”的处置完全不同）", err)
		}
		if errors.Is(err, ErrLockHeld) {
			t.Errorf("把权限问题读成被占用，运维会去等一个永远不会到来的释放: %v", err)
		}
		// 恢复权限后应能重新取得（说明这条错误族不是"永久报废"）。
		if err := os.Chmod(filepath.Join(dir, LockFileName), 0o600); err != nil {
			t.Fatal(err)
		}
		l2, err := AcquireDataLock(db)
		if err != nil {
			t.Fatalf("恢复权限后取不到锁: %v", err)
		}
		_ = l2.Close()
	})
}

func TestDataLockCloseIsSafeWhenNilOrAlreadyClosed(t *testing.T) {
	var nilLock *DataLock
	if err := nilLock.Close(); err != nil {
		t.Errorf("nil 接收者的 Close = %v，期望无害", err)
	}
	dir := t.TempDir()
	l, err := AcquireDataLock(filepath.Join(dir, "epocha.db"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("第一次 Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("重复 Close = %v，期望不报错（defer 与显式关闭可能都跑到）", err)
	}
	// 释放后另一个 fd 必须能取得，证明重复 Close 没有把 fd 状态留在"看似持有"的位置。
	l2, err := AcquireDataLock(filepath.Join(dir, "epocha.db"))
	if err != nil {
		t.Fatalf("释放后再取: %v", err)
	}
	_ = l2.Close()
}

// TestLockPerIsOnOpenFileDescription 钉住"锁绑在打开文件描述上"这条依赖事实：
// 如果哪天它不再成立（比如换机制或改语义），本文件开头写下的整段理由就要重写，
// 而不是留着一段听起来合理但已经不正确的说明。
func TestLockPerIsOnOpenFileDescription(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("直接 flock 失败: %v", err)
	}
	g, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	if err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(g.Fd()), syscall.LOCK_UN)
		t.Fatal("同一文件的第二个 open file description 拿到了第二把锁——flock 语义与本文假设不符")
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Errorf("释放后第二个 fd 仍拿不到锁: %v", err)
	}
	_ = syscall.Flock(int(g.Fd()), syscall.LOCK_UN)
}

func TestAcquireDirLockRejectsFileAsDir(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireDirLock(f); !errors.Is(err, ErrLockAccess) {
		t.Errorf("对文件取目录锁 = %v，期望 ErrLockAccess", err)
	}
	if _, err := AcquireDirLock(""); !errors.Is(err, ErrLockAccess) {
		t.Errorf("空目录 = %v，期望 ErrLockAccess", err)
	}
}
