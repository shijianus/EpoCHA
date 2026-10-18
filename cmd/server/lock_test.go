package main

// 阶段 5B：跨进程运行互斥的真实测试。
//
// 全部用**真实子进程**（重新执行本测试二进制，靠 EPOCHA_TEST_ROLE 分派角色）。
// 理由很直接：这把锁的意义就在进程边界上——同一进程里两次 AcquireDataLock 成功与否
// 只能证明 flock 调用写对了，证明不了"服务在跑时别人挤不进来"。
//
// 每个 helper 进程都由本测试启动、记录 pid、并在 t.Cleanup 里只清理**自己**启动的进程
// （按 pid 定向 kill，绝不对某个名字模式做批量清理）。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/store"
)

const (
	roleEnv  = "EPOCHA_TEST_ROLE"
	roleArgs = "EPOCHA_TEST_ARGS"
	roleDB   = "EPOCHA_TEST_DB"
	roleHold = "server" // 跑真实 run()
	roleLock = "holder" // 只持有数据目录锁，用于冒充"另一个写者"
)

// TestMain 分派角色。非子进程时正常跑测试。
func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case roleHold:
		args := strings.Fields(os.Getenv(roleArgs))
		if err := run(args, os.Getenv, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "role-server: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	case roleLock:
		l, err := store.AcquireDataLock(os.Getenv(roleDB))
		if err != nil {
			fmt.Fprintf(os.Stderr, "role-holder: %v\n", err)
			os.Exit(1)
		}
		// 父进程按这一行同步：见到 locked 才认为锁已持有。
		fmt.Println("locked")
		sig := make(chan os.Signal, 1)
		signalNotify(sig)
		<-sig
		_ = l.Close()
		os.Exit(0)
	default:
		os.Exit(m.Run())
	}
}

func signalNotify(ch chan os.Signal) {
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
}

// child 是一个由本测试启动的 helper 进程。
//
// cmd.Wait 只能调用一次，而这里"父进程随时想知道它退了没"与"收尾时收割"两处都要等它，
// 所以启动时就建一个**唯一**的 Wait goroutine 把退出码送进通道，其它地方只读通道。
type child struct {
	cmd  *exec.Cmd
	out  *os.File
	name string
	rc   int
	exit chan struct{} // Wait 完成后 close；可被多处重复观察
}

func (c *child) pid() int { return c.cmd.Process.Pid }

// startChild 启动一个角色进程。env 追加到当前进程环境上；stdout/stderr 落到文件里，
// 便于断言"拒绝路径说了什么"，也避免测试输出被子进程日志污染。
func startChild(t *testing.T, role string, env map[string]string, args []string) *child {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), role+".log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, roleEnv+"="+role)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if len(args) > 0 {
		cmd.Env = append(cmd.Env, roleArgs+"="+strings.Join(args, " "))
	}
	// 子进程要能收到我们发的 SIGTERM/SIGKILL：它必须 own process group？
	// 不需要——我们按 pid 定向发信号，不用信号组广播（那会打到测试自己）。
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		t.Fatalf("启动 %s 子进程: %v", role, err)
	}
	c := &child{cmd: cmd, out: f, name: role, rc: -999, exit: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		switch {
		case err == nil:
			c.rc = 0
		default:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				c.rc = ee.ExitCode()
			} else {
				c.rc = -2 // Wait 自身出错（不是子进程的退出码）
			}
		}
		close(c.exit)
	}()
	t.Cleanup(func() {
		// 只收掉本测试启动的**这个 pid**：定向 kill，绝不做按名字模式的批量清理。
		_ = c.cmd.Process.Kill()
		select {
		case <-c.exit:
		case <-time.After(5 * time.Second):
			t.Errorf("子进程 %s (pid %d) 在 SIGKILL 后 5s 内没被收割", c.name, c.pid())
		}
		_ = c.out.Close()
	})
	return c
}

// waitLine 等子进程在日志里打出包含 want 的一行（带超时）。
//
// 两个坑都在这里：
//   - slog 的行首是 time=…，所以必须按**包含**判定，不能按前缀；
//   - 不能用 bufio.Scanner 追一个还在增长的文件：它读到一次 EOF 就把 err 定住，
//     之后再 Scan 永远返回 false，于是"日志里明明有这行"却超时（本轮实测撞过）。
//     这里直接轮询整份文件，代价是几 KB 的重复读，换一个不会漏判的语义。
func (c *child) waitLine(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(c.tail(), want) {
			return
		}
		select {
		case <-c.exit:
			t.Fatalf("子进程 %s 提前退出（rc=%d）：%s", c.name, c.rc, c.tail())
		default:
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("子进程 %s 没打出 %q（日志：%s）", c.name, want, c.tail())
}

// tail 取日志末尾若干字节，失败时给出证据而不是空字符串。
func (c *child) tail() string {
	b, err := os.ReadFile(c.out.Name())
	if err != nil {
		return "(读不到日志)"
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 700 {
		s = s[len(s)-700:]
	}
	return s
}

// waitExit 在超时内等退出码，返回 (rc, 是否已退出)。
// 不能拿 -1 当"还没退出"的哨兵：被信号杀死的进程 ExitCode() **就是** -1（本轮实测撞过）。
func (c *child) waitExit(timeout time.Duration) (int, bool) {
	select {
	case <-c.exit:
		return c.rc, true
	case <-time.After(timeout):
		return 0, false
	}
}

func (c *child) signal(sig syscall.Signal) {
	_ = c.cmd.Process.Signal(sig)
}

// childEnv 给子进程准备一套完整的运行环境（密钥、库路径、监听地址）。
func childEnv(db string, listen string) map[string]string {
	return map[string]string{
		"EPOCHA_MASTER_KEY":   sentinelMasterB64,
		"EPOCHA_PEPPER":       sentinelPepperB64,
		"EPOCHA_DB":           db,
		"EPOCHA_LISTEN":       listen,
		"EPOCHA_ADMIN_LISTEN": "127.0.0.1:0",
		"EPOCHA_POOL_ENABLED": "false", // 互斥测试不关心补池，关掉它减少噪声
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // 释放后立即复用有一个小窗口；真被占了会由子进程报错并在断言里现形
	return addr
}

// 1. 服务运行时，第二个服务抢同一数据目录必须失败；输家不得碰库。
func TestSecondServerCannotOpenSameDataDir(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "epocha.db")
	a := startChild(t, roleHold, childEnv(db, freeAddr(t)), nil)
	a.waitLine(t, "已监听", 15*time.Second)

	// 赢家确实建了库（否则"输家没改库"这句是空话）。
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("第一个服务没建出数据库: %v", err)
	}
	before := fileSum(t, db)

	b := startChild(t, roleHold, childEnv(db, freeAddr(t)), nil)
	rc, exited := b.waitExit(20 * time.Second)
	if !exited {
		t.Fatalf("第二个服务在 20s 内没退出——互斥没生效：%s", b.tail())
	}
	if rc == 0 {
		t.Fatalf("第二个服务“正常退出”了，但根本没报错：%s", b.tail())
	}
	log := b.tail()
	if !strings.Contains(log, "数据目录已被另一个进程占用") {
		t.Errorf("第二个服务的失败原因不是运行锁：%s", log)
	}
	// 输家的拒绝发生在 store.Open 之前 ⇒ 它对库一个字节都没改。
	if got := fileSum(t, db); got != before {
		t.Error("抢锁失败的第二个服务仍改了数据库")
	}
	// 赢家不受影响。
	if rc, exited := a.waitExit(300 * time.Millisecond); exited {
		t.Errorf("第一个服务被第二个的失败连带拖停了（rc=%d）：%s", rc, a.tail())
	}
}

// 2. 正常停止后可重新启动（锁随进程退出释放，且不需要任何清理动作）。
func TestGracefulStopAllowsRestart(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "epocha.db")
	a := startChild(t, roleHold, childEnv(db, freeAddr(t)), nil)
	a.waitLine(t, "已监听", 15*time.Second)
	a.signal(syscall.SIGTERM)
	rc, exited := a.waitExit(15 * time.Second)
	if !exited {
		t.Fatalf("SIGTERM 后 15s 内没退出：%s", a.tail())
	}
	if rc != 0 {
		t.Fatalf("优雅停止退出码 = %d（期望 0）：%s", rc, a.tail())
	}
	b := startChild(t, roleHold, childEnv(db, freeAddr(t)), nil)
	b.waitLine(t, "已监听", 15*time.Second)
	if _, alive := b.waitExit(300 * time.Millisecond); !alive {
		if rc, exited := b.waitExit(0); exited {
			t.Errorf("重启后的服务立刻退出（rc=%d）：%s", rc, b.tail())
		}
	}
}

// 3. 被 SIGKILL 后锁可重新取得（内核释放，不留悬挂锁）。
func TestSIGKILLReleasesLock(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "epocha.db")
	a := startChild(t, roleHold, childEnv(db, freeAddr(t)), nil)
	a.waitLine(t, "已监听", 15*time.Second)
	a.signal(syscall.SIGKILL)
	if _, exited := a.waitExit(10 * time.Second); !exited {
		t.Fatal("SIGKILL 后子进程没退出")
	}
	// 锁文件仍在（我们不删它），但必须能直接重新取得。
	if _, err := os.Stat(filepath.Join(dir, store.LockFileName)); err != nil {
		t.Errorf("锁文件不见了：%v", err)
	}
	b := startChild(t, roleHold, childEnv(db, freeAddr(t)), nil)
	b.waitLine(t, "已监听", 15*time.Second)
}

// 4. 已存在但未被持有的锁文件不会永久阻止启动（冒充崩溃残留）。
func TestStaleLockFileDoesNotBlockServer(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "epocha.db")
	if err := os.WriteFile(filepath.Join(dir, store.LockFileName), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := startChild(t, roleHold, childEnv(db, freeAddr(t)), nil)
	a.waitLine(t, "已监听", 15*time.Second)
}

// 5. 服务启动与离线操作（持锁方）竞争：不交叉修改，最多一方成立。
func TestStartupVersusOfflineHolderIsMutuallyExclusive(t *testing.T) {
	for round := range 4 {
		dir := t.TempDir()
		db := filepath.Join(dir, "epocha.db")
		addr := freeAddr(t)
		// 两个进程几乎同时起跑：一个要迁移/建库，一个只要拿到锁。
		h := startChild(t, roleLock, map[string]string{roleDB: db}, nil)
		srv := startChild(t, roleHold, childEnv(db, addr), nil)

		// 输家在毫秒级就退出；赢家不会退出，所以这个窗口只是"存活判定"的等待时长。
		_, hExited := h.waitExit(3 * time.Second)
		_, sExited := srv.waitExit(3 * time.Second)
		hAlive, sAlive := !hExited, !sExited
		if hAlive && sAlive {
			t.Fatalf("第 %d 轮：持锁方与服务**同时**活着——两个写者共享同一个数据集", round+1)
		}
		if !hAlive && !sAlive {
			// 允许"双方都在启动竞态里失败"吗？不允许：那说明输的一方错误原因不对。
			t.Fatalf("第 %d 轮：双方都退出了。holder=%s server=%s", round+1, h.tail(), srv.tail())
		}
		if !hAlive {
			// holder 被拒 ⇒ 服务必须正常建库并在跑。
			if _, err := os.Stat(db); err != nil {
				t.Errorf("第 %d 轮：服务赢了却没建库: %v", round+1, err)
			}
			if !strings.Contains(h.tail(), "role-holder") || !strings.Contains(h.tail(), "已被另一个进程占用") {
				t.Errorf("第 %d 轮：holder 的失败原因不对：%s", round+1, h.tail())
			}
		}
		if !sAlive {
			// 服务被拒 ⇒ 它不得创建库文件（锁在 store.Open 之前）。
			if _, err := os.Stat(db); !os.IsNotExist(err) {
				t.Errorf("第 %d 轮：抢锁失败的服务仍然创建了数据库（err=%v）", round+1, err)
			}
			if !strings.Contains(srv.tail(), "已被另一个进程占用") {
				t.Errorf("第 %d 轮：服务的失败原因不是运行锁：%s", round+1, srv.tail())
			}
		}
		// 收尾：杀掉还活着的那个，避免下一轮带着脏进程。
		if hAlive {
			h.signal(syscall.SIGTERM)
			h.waitExit(10 * time.Second)
		}
		if sAlive {
			srv.signal(syscall.SIGTERM)
			srv.waitExit(10 * time.Second)
		}
	}
}

// 6. 服务持锁时，同目录上的第二个 AcquireDirLock（离线操作的形状）被拒。
func TestHolderAndServerOnSameDirConflictViaDirLock(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "epocha.db")
	a := startChild(t, roleHold, childEnv(db, freeAddr(t)), nil)
	a.waitLine(t, "已监听", 15*time.Second)

	_, err := store.AcquireDirLock(dir)
	if !errors.Is(err, store.ErrLockHeld) {
		t.Fatalf("对服务中的数据目录取锁 = %v，期望 ErrLockHeld", err)
	}
}

func fileSum(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("摘要 %s: %v", p, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
