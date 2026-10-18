package store

// 阶段 5B：跨进程运行互斥。
//
// 要解决的问题不是"端口被占了"，而是"同一个数据集被两个写者同时打开"：
// 服务在跑的时候离线改 admin_keys，或把恢复产物直接盖进正在服务的数据目录，得到的都不是失败，
// 而是**两份真相**——防重放账本、题池与凭据撤销状态都不再有任何单一权威。
// 阶段 5A 用 HTTP 探活挡这件事，那是错的依据：探测端口配错、管理面没起、服务换了地址，
// 任何一条都会让"看起来安全"变成"根本没检查"。
//
// 机制选 `flock(LOCK_EX | LOCK_NB)`，理由是它的失效方向：
//   - 内核在**进程退出**时释放，崩溃、被 SIGKILL、容器被杀都不留悬挂锁；
//   - 锁与"打开文件描述"绑定，不是与路径内容绑定，所以"文件还在但没人持有"是正常状态，
//     不需要任何"清理陈旧锁文件"的动作（那种动作本身就是竞态源）；
//   - 纯 syscall，不需要外部二进制，也不引入依赖。
//
// 刻意**不是** PID 文件：PID 会复用，"PID 不在了就删掉锁文件"的窗口里两个写者可以同时通过；
// 而且删除重建锁文件会把两个进程分到两个不同的 inode 上——flock 只认 inode，那时锁形同不存在。
// 所以本文件从不删除锁文件，也不写 PID。
//
// 能力边界必须说清楚：flock 是**建议锁（advisory）**。它约束的是"按规矩办事的进程"，
// 也就是本仓库的服务与 epocha-ops。一个拥有同等文件权限、故意绕过锁直接打开 SQLite 的本地程序
// **不受**它阻止——这条不是缺陷披露的客套，而是这个机制的能力上限。它防的是误操作、竞态与
// 编排层面的双启动，不防同等权限下的恶意本地进程。真要防后者需要的是文件属主隔离/独占挂载/
// MDL 之外的强制机制，不在本轮范围内。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// LockFileName 落在**数据目录**里，与主库文件同级。
	// 选数据目录而不是 /var/run 之类的地方，是为了让"锁保护的东西"和"锁本身"在同一个挂载点上：
	// 容器与宿主只要 bind 同一个数据目录，看到的就是同一个 inode，因此 flock 真的互通；
	// 反过来，如果两个进程用的是**不同**的数据目录，它们本来也不共享状态，不该互斥。
	LockFileName = "epocha.lock"

	// lockFileMode 0600：锁文件本身不含任何秘密（flock 状态在内核里，文件通常是空的），
	// 但把它限制成"仅属主可读写"能避免"别人的进程顺手打开它并持有锁"。
	// 代价是一条必须写进部署文档的规则：**离线运维要与数据目录属主同 uid 执行**，
	// 否则拿到的是 ErrLockAccess（权限），而不是 ErrLockHeld（被占用）。两者在文案里分开，
	// 因为处置方式完全不同——一个是"换个身份执行"，一个是"先停服"。
	lockFileMode = 0o600
)

var (
	// ErrLockHeld 表示这个数据目录已被另一个进程排他持有。拒绝，不等待。
	ErrLockHeld = errors.New("store: 数据目录已被另一个进程占用")

	// ErrLockAccess 表示连锁文件都打不开：目录不存在、属主/权限不匹配、只读挂载。
	// 单独成族的理由：把它读成"被别人占了"会让运维去等一个永远不会到来的释放。
	ErrLockAccess = errors.New("store: 数据目录不可写")
)

// DataLock 是一次排他持有的句柄。Close 即释放；进程退出时内核也会释放。
type DataLock struct {
	f    *os.File
	path string
}

// LockPathFor 返回某个库路径对应的锁文件路径（数据目录 + LockFileName）。
// dbPath 为空调 lockFile 时会退化成当前目录——那是最容易误伤的配置错误，必须直接拒绝。
func LockPathFor(dbPath string) (string, error) {
	if dbPath == "" {
		return "", fmt.Errorf("%w: 数据库路径为空，无法确定数据目录", ErrLockAccess)
	}
	dir := filepath.Dir(dbPath)
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, LockFileName), nil
}

// AcquireDataLock 对给定库文件所在的数据目录取得非阻塞排他锁。
// 冲突时立刻返回 ErrLockHeld（不会等待），权限/目录问题时返回 ErrLockAccess。
func AcquireDataLock(dbPath string) (*DataLock, error) {
	p, err := LockPathFor(dbPath)
	if err != nil {
		return nil, err
	}
	return AcquireDirLock(filepath.Dir(p))
}

// AcquireDirLock 直接对某个目录加锁，用于"操作目标是目录而不是某个库文件"的场景
// （epocha-ops restore 的 --out-dir：它写的是隔离目录里的新文件，而危险面恰恰是
// "这个目录正在被某个服务用作数据目录"）。
func AcquireDirLock(dir string) (*DataLock, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: 目录为空", ErrLockAccess)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: 目录不存在或不可读：%s", ErrLockAccess, dir)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s 不是目录", ErrLockAccess, dir)
	}
	path := filepath.Join(dir, LockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, lockFileMode)
	if err != nil {
		// 只带路径与错误类别：EACCES/EROFS/EPERM 都说明"身份或挂载不对"，
		// 而不是"有人在用"——把两者混成一句会让排障方向反掉。
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) ||
			errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EPERM) {
			return nil, fmt.Errorf("%w：%s（执行身份的 uid 需与数据目录属主一致）", ErrLockAccess, path)
		}
		return nil, fmt.Errorf("%w：%s：%v", ErrLockAccess, path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w：%s", ErrLockHeld, path)
		}
		return nil, fmt.Errorf("%w：%s：%v", ErrLockAccess, path, err)
	}
	return &DataLock{f: f, path: path}, nil
}

// Path 返回锁文件路径，用于日志与错误文案（不含任何密钥或凭据）。
func (l *DataLock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close 释放锁。
//
// 这里**不**删除锁文件：文件存在但没有被任何进程持有，是 flock 的正常状态。
// 删除它会打开一个竞态——A 删掉文件后 B 重新打开得到新 inode，
// 两者各自持有不同 inode 上的锁，互斥就此消失。
func (l *DataLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil && !errors.Is(err, syscall.EBADF) {
		_ = f.Close()
		return fmt.Errorf("store: 释放运行锁失败: %w", err)
	}
	return f.Close()
}
