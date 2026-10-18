// Command ops 是阶段 5A 的最小运维入口：备份、校验、恢复、就绪探针、容器内管理请求，
// 以及"最后一把管理凭据被撤销"那条锁死路径的唯一受控出口（admin-recover）。
//
// 它存在的理由不是"多一个 CLI"，而是生产镜像的形状：
//   - 镜像是最小运行时，**没有** shell、curl、sqlite3 CLI；容器健康检查必须有个真能跑的命令；
//   - 管理监听器绑在**容器内** loopback 上（应用既有约束，不为部署方便放宽），
//     宿主 loopback 与容器 loopback 不互通，所以管理操作只能从容器内部发起；
//   - 备份/恢复要用到 store 的同一套一致性语义（VACUUM INTO、manifest 配对、保守失效），
//     用外部脚本拼 SQL 会造出第二套真相，而且天然更容易在活库上动手。
//
// 刻意**不做**的：不是通用管理平台，不接管站点业务的每一天，不接管未列出的运维动作。
// 每条子命令都对应 docs/BACKUP-RESTORE.md 或 docs/DEPLOYMENT.md 里一个具体步骤。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/version"
)

const (
	defaultPublicBase = "http://127.0.0.1:8080"
	defaultAdminBase  = "http://127.0.0.1:8081"
	envDB             = "EPOCHA_DB"

	checkTimeout    = 3 * time.Second
	adminTimeout    = 15 * time.Second
	maxAdminReqB    = 64 << 10
	maxAdminRespB   = 1 << 20
	defaultDestName = "epocha.db"
)

// errUsage 单独成族：用法错误要以退出码 2 表达，与"操作失败"（1）区分开，
// 这样脚本能分辨"我命令行写错了"和"备份本身没成功"。
var errUsage = errors.New("usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rc := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	os.Exit(rc)
}

// run 返回退出码：0 成功、1 操作失败、2 用法错误。
// 分成三层可测：cmd/server 的既有约定是"入口可注入 stdout"，这里再加 stderr 与退出码，
// 因为运维脚本要按退出码决定要不要继续（备份失败还往下走是事故，不是噪音）。
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "backup":
		err = backup(ctx, rest, getenv, stdout)
	case "verify":
		err = verify(ctx, rest, stdout)
	case "restore":
		err = restore(ctx, rest, getenv, stdout)
	case "check":
		err = check(rest, getenv, stdout)
	case "admin":
		err = admin(ctx, rest, getenv, stdout)
	case "admin-recover":
		err = adminRecover(ctx, rest, getenv, stdout)
	case "version":
		fmt.Fprintf(stdout, "epocha-ops %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildDate)
	case "-h", "--help", "help":
		usage(stdout)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n", cmd)
		usage(stderr)
		return 2
	}
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprintf(stderr, "ops: %s\n", strings.TrimPrefix(err.Error(), "usage: "))
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "ops: %s\n", err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `用法：epocha-ops <子命令> [参数]

  backup   --db <路径> --dest <目录>        生成一致性快照 + manifest（密钥走 EPOCHA_PEPPER[_FILE]）
  verify   --db <快照> --manifest <manifest> 重新校验备份（哈希、大小、integrity_check、schema 版本）
  restore  --backup <快照> --manifest <manifest> --out-dir <隔离目录>
                                            保守恢复到**新文件**（绝不覆盖工作库）；--rotate-pepper 显式承担轮换后果
  check    [--base-url <URL>] [--path /readyz]  就绪/存活探针，非 200 即退出码 1（容器健康检查用）
  admin    <METHOD> <PATH> [--body-file F]  向管理监听器发一次带 Bearer 的请求（容器内 loopback）
  admin-recover --db <路径> --key-file <新凭据文件>
                                  撤销库里全部有效管理凭据并登记一把新的（**必须先停服**；
                                  这是"最后一把管理凭据被撤销"导致锁死的唯一受控出口）
  version

密钥入口与 server 完全一致：EPOCHA_PEPPER / EPOCHA_PEPPER_FILE、EPOCHA_ADMIN_KEY / EPOCHA_ADMIN_KEY_FILE，
两者同时设置会被拒绝。
`)
}

// --- backup ---

func backup(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbPath := fs.String("db", "", "SQLite 文件路径（默认取 "+envDB+"）")
	dest := fs.String("dest", "", "备份输出目录（必须已存在）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: backup: %s", errUsage, err)
	}
	if *dbPath == "" {
		*dbPath = getenv(envDB)
	}
	if *dbPath == "" || *dest == "" {
		return fmt.Errorf("%w: backup 需要 --db 与 --dest（--db 也可由 %s 提供）", errUsage, envDB)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: backup 不接受位置参数：%v", errUsage, fs.Args())
	}
	pepper, err := config.LoadPepper(getenv)
	if err != nil {
		// 错误文案由 config 给出，只含入口变量名与失败类别，不含内容。
		return err
	}
	m, err := store.Backup(ctx, store.BackupOptions{
		SrcPath: *dbPath, DestDir: *dest, Pepper: pepper,
		AppVersion: version.Version, Commit: version.Commit,
	})
	if err != nil {
		return err
	}
	// 只打印自描述字段：不含指纹之外的任何密钥信息（PepperFP 是单向的，仍不打印，
	// 因为它一旦进日志就成了"离线爆破摘要"的语料，而运维并不需要它）。
	fmt.Fprintf(stdout, "备份完成 name=%s bytes=%d schema=%d app=%s commit=%s sha256=%s integrity=%s\n",
		m.Name, m.BackupBytes, m.SchemaVersion, m.AppVersion, m.Commit, shortHash(m.SHA256), m.Integrity)
	fmt.Fprintf(stdout, "计数 sites=%d sites_enabled=%d site_secrets_active=%d admin_keys_active=%d responses=%d pool_ready=%d\n",
		m.Counts["sites"], m.Counts["sites_enabled"], m.Counts["site_secrets_active"],
		m.Counts["admin_keys_active"], m.Counts["responses_total"], m.Counts["pool_ready"])
	fmt.Fprintf(stdout, "manifest=%s%s\n", m.Name, ".manifest.json")
	return nil
}

// --- verify ---

func verify(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbPath := fs.String("db", "", "备份 .db 文件")
	manifest := fs.String("manifest", "", "对应 manifest 文件")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: verify: %s", errUsage, err)
	}
	if *dbPath == "" || *manifest == "" {
		return fmt.Errorf("%w: verify 需要 --db 与 --manifest", errUsage)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: verify 不接受位置参数：%v", errUsage, fs.Args())
	}
	if err := store.VerifyBackup(ctx, *dbPath, *manifest); err != nil {
		return err
	}
	m, err := store.ReadManifest(*manifest)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "备份可信 name=%s bytes=%d sha256=%s integrity=%s\n",
		m.Name, m.BackupBytes, shortHash(m.SHA256), m.Integrity)
	return nil
}

// --- restore ---

func restore(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bp := fs.String("backup", "", "备份 .db 文件")
	mp := fs.String("manifest", "", "对应 manifest 文件")
	outDir := fs.String("out-dir", "", "恢复输出目录（隔离目录，不是工作数据目录）")
	destName := fs.String("dest-name", defaultDestName, "恢复产物文件名（纯文件名，必须以 .db 结尾）")
	rotate := fs.Bool("rotate-pepper", false, "确认按 pepper 轮换恢复（站点与管理凭据必须重新签发）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: restore: %s", errUsage, err)
	}
	if *bp == "" || *mp == "" || *outDir == "" {
		return fmt.Errorf("%w: restore 需要 --backup --manifest --out-dir", errUsage)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: restore 不接受位置参数：%v", errUsage, fs.Args())
	}
	pepper, err := config.LoadPepper(getenv)
	if err != nil {
		return err
	}
	adminKey, err := config.LoadAdminKey(getenv)
	if err != nil {
		// 保留 config 给出的失败类别（只有入口名，不含内容）："没配凭据"与"配了两份"的处置不同。
		return fmt.Errorf("恢复需要一把**新的**管理凭据（EPOCHA_ADMIN_KEY / EPOCHA_ADMIN_KEY_FILE，没有未鉴权的恢复接口）: %w", err)
	}
	rep, err := store.Restore(ctx, store.RestoreOptions{
		BackupPath: *bp, ManifestPath: *mp, OutDir: *outDir, DestName: *destName,
		Pepper: pepper, AdminKey: string(adminKey), RotatePepper: *rotate,
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, rep.String())
	fmt.Fprintln(stdout, "恢复产物处于**保守失效**状态：站点全部禁用、凭据全部撤销、题池与 response 已清空。")
	fmt.Fprintln(stdout, "下一步：停服 → 把工作库路径指向该产物（或把它移到数据目录）→ 用新的 master key 启动 → 逐个确认站点后再启用。")
	return nil
}

// --- check ---

func check(args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("base-url", "", "探针基址（默认 "+defaultPublicBase+"，可用 "+`EPOCHA_OPS_CHECK_URL 覆盖）`)
	path := fs.String("path", "/readyz", "探针路径：/readyz（能否接流量）或 /healthz（进程存在）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: check: %s", errUsage, err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: check 不接受位置参数：%v", errUsage, fs.Args())
	}
	if *path != "/readyz" && *path != "/healthz" {
		return fmt.Errorf("%w: check 只允许 /readyz 或 /healthz", errUsage)
	}
	u := *base
	if u == "" {
		u = getenv("EPOCHA_OPS_CHECK_URL")
	}
	if u == "" {
		u = defaultPublicBase
	}
	body, status, err := probe(u + *path)
	if err != nil {
		fmt.Fprintf(stdout, "not-ready err=%s\n", errKindText(err))
		return errors.New("探针请求失败")
	}
	if status != http.StatusOK {
		fmt.Fprintf(stdout, "not-ready status=%d body=%s\n", status, trimForLog(body))
		return fmt.Errorf("探针返回 %d", status)
	}
	fmt.Fprintf(stdout, "ready status=%d body=%s\n", status, trimForLog(body))
	return nil
}

func probe(u string) (string, int, error) {
	client := &http.Client{Timeout: checkTimeout}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(b), resp.StatusCode, nil
}

// --- admin ---

func admin(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("base-url", defaultAdminBase, "管理监听器地址（必须是 loopback）")
	bodyFile := fs.String("body-file", "", "JSON 请求体文件（POST/PUT 必需；省略则读 stdin）")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return fmt.Errorf("%w: admin: %s", errUsage, err)
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("%w: admin 需要 METHOD 与 PATH 两个位置参数", errUsage)
	}
	method, path := strings.ToUpper(fs.Arg(0)), fs.Arg(1)
	if method != http.MethodGet && method != http.MethodPost && method != http.MethodPut && method != http.MethodDelete {
		return fmt.Errorf("%w: admin 方法必须是 GET/POST/PUT/DELETE", errUsage)
	}
	if !strings.HasPrefix(path, "/api/admin/") {
		return fmt.Errorf("%w: admin 只用于 /api/admin/* 路径", errUsage)
	}
	u, err := url.Parse(*base)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: --base-url 不是合法 URL", errUsage)
	}
	// 管理凭据是 Bearer 明文，管理监听器又是明文 HTTP：只允许 loopback。
	// 这一条不是"顺手加的严"，而是把"从别处转发管理面"这类误用变成一条错误，
	// 而不是变成一次凭据上线。放宽它需要的是 TLS + 明确的通道设计，不是加个 flag。
	if !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("admin 只允许 loopback 目标（当前 %s）：Bearer 凭据不得走明文远端连接", u.Host)
	}
	key, err := config.LoadAdminKey(getenv)
	if err != nil {
		return err
	}
	var body io.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		body = nil
	} else {
		b, err := readRequestBody(*bodyFile)
		if err != nil {
			return err
		}
		body = strings.NewReader(b)
	}
	cctx, cancel := context.WithTimeout(ctx, adminTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, *base+path, body)
	if err != nil {
		return errors.New("请求构造失败")
	}
	req.Header.Set("Authorization", "Bearer "+string(key))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New("管理请求失败")
	}
	defer func() { _ = resp.Body.Close() }()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminRespB))
	if err != nil {
		return errors.New("读取响应失败")
	}
	fmt.Fprintf(stdout, "status=%d\n%s\n", resp.StatusCode, strings.TrimRight(string(rb), "\n"))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("管理 API 返回 %d", resp.StatusCode)
	}
	return nil
}

// adminValueFlags 是 admin 子命令里**带值**的 flag（两种前缀写法都接受，与 stdlib 一致）。
var adminValueFlags = map[string]bool{
	"--base-url": true, "-base-url": true, "--body-file": true, "-body-file": true,
}

// hoistFlags 把带值的 flag 挪到位置参数之前。
//
// 理由不是"方便"：stdlib 的 flag 包在**第一个非 flag 参数**处停止解析，于是
// `admin POST /api/admin/sites --body-file f.json`（运维最自然的写法）会把
// `--body-file` 当成第三个位置参数，报出"需要 METHOD 与 PATH 两个位置参数"——
// 一个把原因指错的失败。恢复与管理操作是在压力下敲的，错误原因指错比失败更贵。
func hoistFlags(args []string) []string {
	flags := make([]string, 0, len(args))
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if adminValueFlags[a] {
			flags = append(flags, a)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if name, _, has := strings.Cut(a, "="); has && adminValueFlags[name] {
			flags = append(flags, a)
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

// adminRecover 是"最后一把管理凭据被撤销"这条锁死路径的受控出口。
//
// 为什么需要它（实测教训）：`BootstrapAdminKey` 只在库里**没有任何** admin_keys 记录时登记，
// 所以撤销掉最后一把之后，改 EPOCHA_ADMIN_KEY(_FILE) 也不会有任何效果——而交付的镜像里没有
// sqlite3 之类可以手工改库的东西。没有这条命令，那个局面的唯一出路是打开一个未鉴权的恢复接口，
// 而那正是本轮明确禁止的形状。
//
// 顺序是刻意的：**先**写凭据文件（O_EXCL，权限 0600），**再**改库；改库失败就删掉自己刚写的文件。
// 反过来会留下"库已经改完但没人知道新凭据是什么"的状态——那比失败更糟。
func adminRecover(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin-recover", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbPath := fs.String("db", "", "SQLite 文件路径（默认取 "+envDB+"）")
	keyFile := fs.String("key-file", "", "新管理凭据写入的文件（必须不存在；0600）")
	adminBase := fs.String("admin-base-url", defaultAdminBase, "管理监听器地址（用于探活：它还能答话就说明服务在跑）")
	if err := fs.Parse(hoistFlags(args)); err != nil {
		return fmt.Errorf("%w: admin-recover: %s", errUsage, err)
	}
	if *dbPath == "" {
		*dbPath = getenv(envDB)
	}
	if *dbPath == "" || *keyFile == "" {
		return fmt.Errorf("%w: admin-recover 需要 --db 与 --key-file（--db 也可由 %s 提供）", errUsage, envDB)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: admin-recover 不接受位置参数：%v", errUsage, fs.Args())
	}
	// 探活：任何 HTTP 响应（含 401）都说明服务正在用这个库。停服是前置条件，不是建议——
	// 一边有服务在写账本、一边离线改管理凭据，就是两份真相。
	// 与 admin 子命令同一条边界：只允许 loopback。这条探测的用途是"我本机那个服务还在不在"，
	// 把它指向远端地址既没有意义，也会让一条运维命令变成一次对外请求。
	u, err := url.Parse(*adminBase)
	if err != nil || u.Host == "" || !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("%w: admin-recover 的 --admin-base-url 必须是 loopback 地址（当前 %s）", errUsage, *adminBase)
	}
	if alive, _ := probeStatus(*adminBase + "/api/admin/whoami"); alive {
		return fmt.Errorf("管理监听器 %s 仍在应答：先停服（docker compose stop）再恢复管理访问", *adminBase)
	}
	pepper, err := config.LoadPepper(getenv)
	if err != nil {
		return err
	}
	key, err := store.NewAdminKey()
	if err != nil {
		return fmt.Errorf("生成管理凭据: %w", err)
	}
	// O_EXCL：不覆盖已有凭据文件。覆盖一个可能是别处正在用的凭据，是不可逆的动作。
	f, err := os.OpenFile(*keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("凭据文件不可创建（已存在即为拒绝覆盖）: %s", filepath.Base(*keyFile))
	}
	written := false
	defer func() {
		if !written {
			_ = f.Close()
			_ = os.Remove(*keyFile)
		}
	}()
	if _, err := f.WriteString(key); err != nil {
		return fmt.Errorf("写凭据文件失败")
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("写凭据文件失败")
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("写凭据文件失败")
	}

	d, err := store.Open(ctx, *dbPath, pepper)
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}
	ak, revoked, err := d.RecoverAdminKey(ctx, key, time.Now().UTC())
	cerr := d.Close()
	if err != nil {
		return err
	}
	if cerr != nil {
		return fmt.Errorf("库已更新但关闭失败（请以 verify 复核后再决定是否重来）: %w", cerr)
	}
	written = true
	// 只打印 key_id 与文件路径：凭据本身只存在于那个 0600 文件里，不进 stdout/日志。
	fmt.Fprintf(stdout, "管理访问已恢复 key_id=%s revoked_existing=%d key_file=%s\n", ak.KeyID, revoked, filepath.Base(*keyFile))
	fmt.Fprintln(stdout, "下一步：把这个文件放到 EPOCHA_ADMIN_KEY_FILE 指向的路径（0600、属主=容器 uid），再启动服务。")
	fmt.Fprintln(stdout, "注意：旧的凭据已全部撤销；恢复期间不要接收业务流量。")
	return nil
}

// probeStatus 只回答"有没有 HTTP 响应"，不回显状态码之外的任何信息。
// 401 也算"在应答"：它证明监听器活着，而这正是我们要拒绝的情况。
func probeStatus(u string) (bool, int) {
	client := &http.Client{Timeout: checkTimeout}
	resp, err := client.Get(u)
	if err != nil {
		return false, 0
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	return true, resp.StatusCode
}

// readRequestBody 读请求体：--body-file 优先，否则 stdin；一律有上限（红线 6）。
func readRequestBody(path string) (string, error) {
	var (
		in  io.Reader = os.Stdin
		clo io.Closer
	)
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", errors.New("请求体文件不可读")
		}
		in, clo = f, f
	} else {
		// stdin 只有在"确实被重定向"时才是合法输入，否则这里是TTY：
		// 交互式终端会挂住整个命令，而运维以为它在等服务端响应。
		if !stdinHasData() {
			return "", fmt.Errorf("非 GET 请求需要 --body-file 或管道输入 JSON")
		}
	}
	if clo != nil {
		defer func() { _ = clo.Close() }()
	}
	b, err := io.ReadAll(io.LimitReader(in, maxAdminReqB+1))
	if err != nil {
		return "", errors.New("读取请求体失败")
	}
	if len(b) > maxAdminReqB {
		return "", fmt.Errorf("请求体超过 %d 字节上限", maxAdminReqB)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", errors.New("请求体为空")
	}
	return string(b), nil
}

func stdinHasData() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice == 0
}

// isLoopbackHost 判定主机名是否为 loopback：接受 127.0.0.0/8、::1 与 localhost。
// 按 IP 解析结果判定，不接受"看起来像内网名"的地址。
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func shortHash(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

func trimForLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// errKindText 把探针失败归成有限类别：底层错误文本可能含 URL 与 DNS 细节，
// 那些对运维没用，却会把内部主机名带进日志。
func errKindText(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, os.ErrNotExist):
		return "not-found"
	default:
		return "unreachable"
	}
}
