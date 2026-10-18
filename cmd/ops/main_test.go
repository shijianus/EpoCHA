package main

// 阶段 5A：运维命令的 CLI 层测试。
//
// store 层已经证明了一致性快照与保守失效的语义（internal/store/deploy_test.go），
// 这一组只证明"人真正敲的那条命令"把这些语义带到达、把失败变成可用信号：
//   - 退出码分三层（0 成功 / 1 操作失败 / 2 用法错误），否则 cron 与 shell 无法判断该不该继续；
//   - 密钥入口与 server 同源（env 与 *_FILE 同时设置即拒），且**任何输出都不带出密钥值**；
//   - admin 子命令只允许 loopback：Bearer 凭据不得走明文远端连接；
//   - check 子命令把非 200 变成非 0 退出码（容器健康检查唯一的判据）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/store"
)

// testPepper 是本包内**唯一**的 pepper 来源：库摘要、备份指纹、env 值全部由它派生，
// 免得测试自己配错而把"配对检查"测成了假阳性。
var (
	testPepper    = bytes.Repeat([]byte("P"), 32)
	testPepperB64 = base64.StdEncoding.EncodeToString(testPepper)
	testMasterB64 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("M"), 32))
)

func getenvOf(m map[string]string) func(string) string {
	return func(k string) string {
		if m == nil {
			return ""
		}
		return m[k]
	}
}

func baseEnv() map[string]string {
	return map[string]string{"EPOCHA_MASTER_KEY": testMasterB64, "EPOCHA_PEPPER": testPepperB64}
}

// seedDB 建一个带站点凭据与引导管理密钥的库，返回站点 secret（按值扫描用的哨兵）。
// 关闭后交给调用方按文件路径使用——备份/恢复只认文件，不认句柄。
func seedDB(t *testing.T, path, adminKey string) string {
	t.Helper()
	d, err := store.Open(context.Background(), path, testPepper)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	siteKey, err := store.NewSiteKey()
	if err != nil {
		t.Fatalf("NewSiteKey: %v", err)
	}
	_, secret, err := d.CreateSite(context.Background(), "alpha", siteKey, store.DefaultPolicy())
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if _, _, err := d.BootstrapAdminKey(context.Background(), adminKey, "bootstrap"); err != nil {
		t.Fatalf("BootstrapAdminKey: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return secret
}

func openRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	h, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("只读打开 %s: %v", path, err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func countRows(t *testing.T, h *sql.DB, q string) int64 {
	t.Helper()
	var n int64
	if err := h.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func fileSum(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("打开 %s: %v", p, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// assertNoKeyMaterial 按值扫描（红线 11）：哨兵值不得出现在任何输出里。
func assertNoKeyMaterial(t *testing.T, blob string, sentinels ...string) {
	t.Helper()
	for _, s := range sentinels {
		if s == "" {
			continue
		}
		if strings.Contains(blob, s) {
			t.Errorf("输出泄露哨兵值 %q…：%s", s[:6], blob)
		}
	}
}

// backupNameFromOutput 从 backup 的那一行机器可读输出里取产物名。
func backupNameFromOutput(t *testing.T, s string) string {
	t.Helper()
	for _, f := range strings.Fields(s) {
		if v, ok := strings.CutPrefix(f, "name="); ok {
			return v
		}
		if v, ok := strings.CutPrefix(f, "manifest="); ok {
			return strings.TrimSuffix(v, ".manifest.json")
		}
	}
	t.Fatalf("备份输出里找不到产物名: %q", s)
	return ""
}

func TestRunExitCodesAreLayered(t *testing.T) {
	var out, errb bytes.Buffer
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"无参数是用法错误", nil, 2},
		{"未知子命令是用法错误", []string{"destroy-everything"}, 2},
		{"backup 缺 --dest 是用法错误", []string{"backup", "--db", "/tmp/x.db"}, 2},
		{"backup 有多余位置参数是用法错误", []string{"backup", "--db", "/tmp/x.db", "--dest", "/tmp", "extra"}, 2},
		{"verify 缺 manifest 是用法错误", []string{"verify", "--db", "/tmp/x.db"}, 2},
		{"restore 缺 --out-dir 是用法错误", []string{"restore", "--backup", "/tmp/x.db", "--manifest", "/tmp/m.json"}, 2},
		{"check 只允许两个探针路径", []string{"check", "--path", "/api/admin/sites"}, 2},
		{"admin 只允许 /api/admin/* 前缀", []string{"admin", "GET", "/healthz"}, 2},
		{"admin 方法白名单", []string{"admin", "PATCH", "/api/admin/sites"}, 2},
		{"version 不需要密钥", []string{"version"}, 0},
		{"help 不需要密钥", []string{"help"}, 0},
	}
	for _, c := range cases {
		rc := run(context.Background(), c.args, getenvOf(baseEnv()), &out, &errb)
		if rc != c.want {
			t.Errorf("%s: 退出码 = %d，期望 %d（stderr=%q）", c.name, rc, c.want, errb.String())
		}
		out.Reset()
		errb.Reset()
	}
}

func TestMissingKeyMaterialRefusesWithExit1(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	seedDB(t, dbPath, "ak_"+strings.Repeat("b", 64))

	var out, errb bytes.Buffer
	noPepper := map[string]string{"EPOCHA_MASTER_KEY": testMasterB64}
	rc := run(context.Background(), []string{"backup", "--db", dbPath, "--dest", dir}, getenvOf(noPepper), &out, &errb)
	if rc != 1 {
		t.Fatalf("缺 pepper 的备份退出码 = %d，期望 1（操作失败，不是用法错误）: %s", rc, errb.String())
	}
	assertNoKeyMaterial(t, out.String()+errb.String(), testPepperB64, testMasterB64)
	if !strings.Contains(errb.String(), "EPOCHA_PEPPER") {
		t.Errorf("错误文案没点名缺失的入口: %s", errb.String())
	}
	for _, e := range dirEntries(t, dir) {
		if strings.Contains(e, "manifest.json") || (strings.HasSuffix(e, ".db") && e != "epocha.db") {
			t.Errorf("失败的备份留下了产物 %s", e)
		}
	}
}

// TestKeyFileEntryIsHonoredByOps 证明 cmd/ops 真的走 *_FILE 入口，而不只是 config 有测试。
// "server 能读文件、ops 只能读 env"这种不对称会在备份时炸出 pepper 不配对，
// 而那是最容易被误诊成"备份坏了"的一类故障。
func TestKeyFileEntryIsHonoredByOps(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	seedDB(t, dbPath, "ak_"+strings.Repeat("b", 64))
	pepperFile := filepath.Join(dir, "pepper")
	if err := os.WriteFile(pepperFile, []byte(testPepperB64+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"EPOCHA_MASTER_KEY": testMasterB64, "EPOCHA_PEPPER_FILE": pepperFile}

	var out, errb bytes.Buffer
	if rc := run(context.Background(), []string{"backup", "--db", dbPath, "--dest", dir}, getenvOf(env), &out, &errb); rc != 0 {
		t.Fatalf("文件入口备份退出码 = %d: %s", rc, errb.String())
	}
	// 同时设置两份 ⇒ 拒绝，且错误只点名变量名、不带内容与路径。
	env["EPOCHA_PEPPER"] = testPepperB64
	out.Reset()
	errb.Reset()
	if rc := run(context.Background(), []string{"backup", "--db", dbPath, "--dest", filepath.Join(dir, "x")}, getenvOf(env), &out, &errb); rc != 1 {
		t.Fatalf("env 与 *_FILE 同时设置的退出码 = %d，期望 1", rc)
	}
	msg := errb.String()
	if !strings.Contains(msg, "EPOCHA_PEPPER") || !strings.Contains(msg, "EPOCHA_PEPPER_FILE") {
		t.Errorf("拒绝文案没点名两个入口（运维无法判断该删哪一个）: %s", msg)
	}
	assertNoKeyMaterial(t, msg, testPepperB64)
	if strings.Contains(msg, pepperFile) {
		t.Errorf("拒绝文案带出了密钥文件路径: %s", msg)
	}
}

func TestBackupVerifyRestoreThroughCLI(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	adminKey := "ak_" + strings.Repeat("c", 64)
	siteSecret := seedDB(t, dbPath, adminKey)
	backupDir := filepath.Join(dir, "backups")
	restoreDir := filepath.Join(dir, "restore")
	for _, d := range []string{backupDir, restoreDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		"EPOCHA_MASTER_KEY": testMasterB64,
		"EPOCHA_PEPPER":     testPepperB64,
		"EPOCHA_ADMIN_KEY":  adminKey,
	}
	g := getenvOf(env)
	var out, errb bytes.Buffer

	if rc := run(context.Background(), []string{"backup", "--db", dbPath, "--dest", backupDir}, g, &out, &errb); rc != 0 {
		t.Fatalf("backup 退出码 = %d: %s", rc, errb.String())
	}
	name := backupNameFromOutput(t, out.String())
	snap := filepath.Join(backupDir, name+".db")
	man := filepath.Join(backupDir, name+".manifest.json")
	for _, p := range []string{snap, man} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("备份产物缺失 %s: %v", filepath.Base(p), err)
		}
	}
	if fi, err := os.Stat(snap); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("备份文件权限 = %v，期望 0600（同机其它账号不得读走站点账本）", fi.Mode())
	}

	out.Reset()
	errb.Reset()
	if rc := run(context.Background(), []string{"verify", "--db", snap, "--manifest", man}, g, &out, &errb); rc != 0 {
		t.Fatalf("verify 退出码 = %d: %s", rc, errb.String())
	}

	// 备份之后再发生撤销与停用：快照**不知道**这两件事，所以恢复产物必须一次性失效。
	revokeAndDisable(t, dbPath)
	liveBefore := fileSum(t, dbPath)

	out.Reset()
	errb.Reset()
	if rc := run(context.Background(), []string{"restore", "--backup", snap, "--manifest", man, "--out-dir", restoreDir}, g, &out, &errb); rc != 0 {
		t.Fatalf("restore 退出码 = %d: %s", rc, errb.String())
	}
	if got := fileSum(t, dbPath); got != liveBefore {
		t.Fatal("恢复流程改了工作库（必须只产出新文件）")
	}
	// **紧接着**检查交付形状：恢复产物必须是单文件。任何后来的只读打开都会按 WAL 语义
	// 重新生成 -shm/-wal（那是 SQLite 的正常行为，不是恢复的缺陷），所以这条断言只能
	// 在"恢复刚结束、还没有第二个连接碰它"的时刻做。
	for _, e := range dirEntries(t, restoreDir) {
		if strings.HasSuffix(e, "-wal") || strings.HasSuffix(e, "-shm") || strings.Contains(e, ".partial") {
			t.Errorf("恢复目录留下 %s（单文件交付是“拷走一个 .db 就完整”的前提）", e)
		}
	}
	for _, e := range dirEntries(t, backupDir) {
		if strings.HasSuffix(e, "-wal") || strings.HasSuffix(e, "-shm") || strings.Contains(e, ".partial") {
			t.Errorf("备份目录留下 %s", e)
		}
	}
	restored := filepath.Join(restoreDir, "epocha.db")
	h := openRO(t, restored)
	for _, c := range []struct {
		q    string
		want int64
		name string
	}{
		{`SELECT count(*) FROM sites WHERE enabled=1`, 0, "启用站点"},
		{`SELECT count(*) FROM site_secrets`, 0, "站点凭据"},
		{`SELECT count(*) FROM responses`, 0, "response"},
		{`SELECT count(*) FROM challenges WHERE status <> 'invalid'`, 0, "非终态 challenge"},
		{`SELECT count(*) FROM admin_keys WHERE revoked_at IS NULL`, 1, "有效管理凭据"},
	} {
		if n := countRows(t, h, c.q); n != c.want {
			t.Errorf("恢复产物里的%s = %d，期望 %d", c.name, n, c.want)
		}
	}
	// 新管理凭据必须真能鉴权：编码写错会变成"看着恢复了但管理面进不去"。
	d, err := store.Open(context.Background(), restored, testPepper)
	if err != nil {
		t.Fatalf("打开恢复产物: %v", err)
	}
	if _, err := d.MatchAdminKey(context.Background(), adminKey, time.Now()); err != nil {
		t.Errorf("恢复装入的管理凭据无法鉴权: %v", err)
	}
	if _, err := d.MatchAdminKey(context.Background(), "ak_"+strings.Repeat("z", 64), time.Now()); err == nil {
		t.Error("旧/未知管理凭据在恢复产物上竟然通过鉴权")
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	all := out.String() + errb.String()
	assertNoKeyMaterial(t, all, adminKey, siteSecret, testPepperB64, testMasterB64)
}

func TestRestoreRefusesToOverwriteExistingDestination(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	adminKey := "ak_" + strings.Repeat("d", 64)
	seedDB(t, dbPath, adminKey)
	backupDir := filepath.Join(dir, "backups")
	restoreDir := filepath.Join(dir, "restore")
	for _, d := range []string{backupDir, restoreDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"EPOCHA_PEPPER": testPepperB64, "EPOCHA_ADMIN_KEY": adminKey}
	g := getenvOf(env)
	var out, errb bytes.Buffer
	if rc := run(context.Background(), []string{"backup", "--db", dbPath, "--dest", backupDir}, g, &out, &errb); rc != 0 {
		t.Fatalf("backup: %d %s", rc, errb.String())
	}
	name := backupNameFromOutput(t, out.String())
	args := []string{"restore", "--backup", filepath.Join(backupDir, name+".db"),
		"--manifest", filepath.Join(backupDir, name+".manifest.json"), "--out-dir", restoreDir}
	if rc := run(context.Background(), args, g, &out, &errb); rc != 0 {
		t.Fatalf("首次 restore 退出码 = %d: %s", rc, errb.String())
	}
	sum := fileSum(t, filepath.Join(restoreDir, "epocha.db"))
	out.Reset()
	errb.Reset()
	if rc := run(context.Background(), args, g, &out, &errb); rc != 1 {
		t.Fatalf("重复 restore 退出码 = %d，期望 1: %s", rc, errb.String())
	}
	if !strings.Contains(errb.String(), "拒绝覆盖") {
		t.Errorf("重复 restore 的文案没说明是覆盖保护: %s", errb.String())
	}
	if got := fileSum(t, filepath.Join(restoreDir, "epocha.db")); got != sum {
		t.Error("被拒绝的第二次恢复仍然改了产物")
	}
}

func TestRestoreWithForeignPepperNeedsExplicitFlag(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	adminKey := "ak_" + strings.Repeat("e", 64)
	seedDB(t, dbPath, adminKey)
	backupDir := filepath.Join(dir, "backups")
	restoreDir := filepath.Join(dir, "restore")
	for _, d := range []string{backupDir, restoreDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	g0 := getenvOf(map[string]string{"EPOCHA_PEPPER": testPepperB64, "EPOCHA_ADMIN_KEY": adminKey})
	if rc := run(context.Background(), []string{"backup", "--db", dbPath, "--dest", backupDir}, g0, &out, &errb); rc != 0 {
		t.Fatalf("backup: %d %s", rc, errb.String())
	}
	name := backupNameFromOutput(t, out.String())
	args := []string{"restore", "--backup", filepath.Join(backupDir, name+".db"),
		"--manifest", filepath.Join(backupDir, name+".manifest.json"), "--out-dir", restoreDir}

	// 换一把 pepper：默认必须拒绝——静默恢复会得到"摘要全不匹配、看起来可服务"的库。
	g1 := getenvOf(map[string]string{
		"EPOCHA_PEPPER":    base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("Q"), 32)),
		"EPOCHA_ADMIN_KEY": adminKey,
	})
	out.Reset()
	errb.Reset()
	if rc := run(context.Background(), args, g1, &out, &errb); rc != 1 {
		t.Fatalf("pepper 不配对时的恢复退出码 = %d，期望 1", rc)
	}
	if _, err := os.Stat(filepath.Join(restoreDir, "epocha.db")); !os.IsNotExist(err) {
		t.Error("pepper 不配对的恢复留下了产物——那就是“半成品被当成恢复完成”的入口")
	}
	// 显式承担轮换后果才允许继续。
	out.Reset()
	errb.Reset()
	args = append(args, "--rotate-pepper")
	if rc := run(context.Background(), args, g1, &out, &errb); rc != 0 {
		t.Fatalf("带 --rotate-pepper 的恢复退出码 = %d: %s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "pepper_rotated=true") {
		t.Errorf("恢复报告没记录 pepper 轮换: %s", out.String())
	}
}

func TestAdminSubcommandIsLoopbackOnly(t *testing.T) {
	adminKey := "ak_" + strings.Repeat("f", 64)
	g := getenvOf(map[string]string{"EPOCHA_ADMIN_KEY": adminKey})
	var out, errb bytes.Buffer
	for _, base := range []string{"http://10.0.0.5:8081", "http://192.168.1.9:8081", "http://admin.example.invalid:8081"} {
		out.Reset()
		errb.Reset()
		rc := run(context.Background(), []string{"admin", "--base-url", base, "GET", "/api/admin/sites"}, g, &out, &errb)
		if rc != 1 {
			t.Errorf("%s 的 admin 退出码 = %d，期望 1（非 loopback 必须拒绝）", base, rc)
		}
		if !strings.Contains(errb.String(), "loopback") {
			t.Errorf("%s 的拒绝文案没点出 loopback 约束: %s", base, errb.String())
		}
		assertNoKeyMaterial(t, out.String()+errb.String(), adminKey)
	}
}

func TestAdminSubcommandReachesLoopbackAdminAPI(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/admin/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sites":[]}`))
	}))
	defer srv.Close()

	adminKey := "ak_" + strings.Repeat("g", 64)
	g := getenvOf(map[string]string{"EPOCHA_ADMIN_KEY": adminKey})
	var out, errb bytes.Buffer
	if rc := run(context.Background(), []string{"admin", "--base-url", srv.URL, "GET", "/api/admin/sites"}, g, &out, &errb); rc != 0 {
		t.Fatalf("loopback 管理请求退出码 = %d: %s", rc, errb.String())
	}
	if atomic.LoadInt32(&hits) == 0 || !strings.Contains(out.String(), "status=200") {
		t.Errorf("管理请求没真的打到服务端: hits=%d out=%s", hits, out.String())
	}
	assertNoKeyMaterial(t, out.String()+errb.String(), adminKey)

	// 4xx 必须变成非 0：否则脚本会带着"管理面没改成功"的状态继续往下走。
	body := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(body, []byte(`{"name":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv2.Close()
	if rc := run(context.Background(), []string{"admin", "--base-url", srv2.URL, "POST", "/api/admin/sites", "--body-file", body}, g, &out, &errb); rc != 1 {
		t.Fatalf("管理 API 返回 403 时退出码 = %d，期望 1: %s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "status=403") {
		t.Errorf("403 响应没有如实打印: %s", out.String())
	}

	// flag 写在位置参数**后面**是同一条命令的自然写法，必须得到同样的结果，
	// 而不是"需要 METHOD 与 PATH 两个位置参数"这种指错原因的错误。
	out.Reset()
	errb.Reset()
	if rc := run(context.Background(), []string{"admin", "POST", "/api/admin/sites", "--base-url", srv2.URL, "--body-file", body}, g, &out, &errb); rc != 1 {
		t.Fatalf("flag 后置时退出码 = %d，期望 1: %s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "status=403") {
		t.Errorf("flag 后置时没有真的发出请求: out=%s err=%s", out.String(), errb.String())
	}
}

func TestCheckExitCodeFollowsProbeStatus(t *testing.T) {
	var ready int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			if atomic.LoadInt32(&ready) == 0 {
				_, _ = w.Write([]byte(`{"status":"ready"}`))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable"}`))
		case "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if rc := run(context.Background(), []string{"check", "--base-url", srv.URL, "--path", "/readyz"}, getenvOf(nil), &out, &errb); rc != 0 {
		t.Fatalf("就绪时退出码 = %d: out=%s err=%s", rc, out.String(), errb.String())
	}
	atomic.StoreInt32(&ready, 1)
	out.Reset()
	if rc := run(context.Background(), []string{"check", "--base-url", srv.URL, "--path", "/readyz"}, getenvOf(nil), &out, &errb); rc != 1 {
		t.Fatalf("不就绪时退出码 = %d，期望 1（容器健康检查只看这个码）: out=%s", rc, out.String())
	}
	if !strings.Contains(out.String(), "not-ready") {
		t.Errorf("不就绪时没有机器可读的一行: %s", out.String())
	}
	atomic.StoreInt32(&ready, 0)
	out.Reset()
	if rc := run(context.Background(), []string{"check", "--base-url", srv.URL, "--path", "/healthz"}, getenvOf(nil), &out, &errb); rc != 0 {
		t.Fatalf("存活探针退出码 = %d", rc)
	}
	// 服务不在时也非 0，且不外泄底层地址（运维看类别就够）。
	dead := httptest.NewServer(http.NotFoundHandler())
	addr := dead.URL
	dead.Close()
	out.Reset()
	errb.Reset()
	if rc := run(context.Background(), []string{"check", "--base-url", addr, "--path", "/readyz"}, getenvOf(nil), &out, &errb); rc != 1 {
		t.Fatalf("服务不可达时退出码 = %d，期望 1", rc)
	}
	if !strings.Contains(out.String(), "not-ready err=") {
		t.Errorf("不可达时未给出类别: %s", out.String())
	}
}

func TestBackupRejectsUnusablePaths(t *testing.T) {
	dir := t.TempDir()
	g := getenvOf(baseEnv())
	var out, errb bytes.Buffer
	cases := []struct {
		name string
		args []string
	}{
		{"源是目录", []string{"backup", "--db", dir, "--dest", dir}},
		{"源不存在", []string{"backup", "--db", filepath.Join(dir, "nope.db"), "--dest", dir}},
		{"目标目录不存在", []string{"backup", "--db", filepath.Join(dir, "a.db"), "--dest", filepath.Join(dir, "nope")}},
		{"恢复目标名带目录分隔", []string{"restore", "--backup", filepath.Join(dir, "a.db"), "--manifest", filepath.Join(dir, "m.json"), "--out-dir", dir, "--dest-name", "../evil.db"}},
		{"恢复目标名不是 .db", []string{"restore", "--backup", filepath.Join(dir, "a.db"), "--manifest", filepath.Join(dir, "m.json"), "--out-dir", dir, "--dest-name", "evil.txt"}},
	}
	for _, c := range cases {
		out.Reset()
		errb.Reset()
		if rc := run(context.Background(), c.args, g, &out, &errb); rc == 0 {
			t.Errorf("%s: 竟然成功了（应当拒绝）", c.name)
		}
		assertNoKeyMaterial(t, out.String()+errb.String(), testPepperB64, testMasterB64)
	}
}

// revokeAndDisable 在活库上制造"备份之后发生的状态变化"：撤销凭据 + 停用站点。
// 恢复回来的快照不知道这两件事，所以恢复产物必须把它们一次性失效掉。
func revokeAndDisable(t *testing.T, dbPath string) {
	t.Helper()
	d, err := store.Open(context.Background(), dbPath, testPepper)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = d.Close() }()
	sites, err := d.ListSites(context.Background(), 0, 10)
	if err != nil || len(sites) == 0 {
		t.Fatalf("ListSites: %v n=%d", err, len(sites))
	}
	secrets, err := d.ListSecrets(context.Background(), sites[0].ID)
	if err != nil || len(secrets) == 0 {
		t.Fatalf("ListSecrets: %v n=%d", err, len(secrets))
	}
	if _, err := d.RevokeSecret(context.Background(), sites[0].ID, secrets[0].KeyID, time.Now()); err != nil {
		t.Fatalf("RevokeSecret: %v", err)
	}
	if _, err := d.SetSiteEnabled(context.Background(), sites[0].ID, false); err != nil {
		t.Fatalf("SetSiteEnabled: %v", err)
	}
}

func TestVerifyDetectsTamperedBackup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	seedDB(t, dbPath, "ak_"+strings.Repeat("h", 64))
	var out, errb bytes.Buffer
	g := getenvOf(baseEnv())
	if rc := run(context.Background(), []string{"backup", "--db", dbPath, "--dest", dir}, g, &out, &errb); rc != 0 {
		t.Fatalf("backup: %d %s", rc, errb.String())
	}
	name := backupNameFromOutput(t, out.String())
	snap := filepath.Join(dir, name+".db")
	man := filepath.Join(dir, name+".manifest.json")

	// 改一个字节：manifest 的 sha256 必须先失败——"文件还在"不等于"内容可信"。
	b, err := os.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0x01
	if err := os.WriteFile(snap, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if rc := run(context.Background(), []string{"verify", "--db", snap, "--manifest", man}, g, &out, &errb); rc != 1 {
		t.Fatalf("被改过的备份 verify 退出码 = %d，期望 1: %s", rc, errb.String())
	}
}

// --- admin-recover：最后一把管理凭据被撤销之后的受控出口 ---
//
// 这组测试钉住的是"服务还在跑就不许离线改库"与"凭据只进那个 0600 文件"两条。
// 前者防止两份真相（服务正在写账本的同时另一个进程改 admin_keys），
// 后者防止新凭据进 shell 历史、CI 日志或容器 stdout。

// TestAdminRecoverRefusesWhileLockHeldByAnotherProcess 是本轮的核心断言：
// 判据是数据目录上的排他锁，不是端口。测试里同时开一个"看起来像管理面"的 HTTP 服务
// 在**别的端口**上答话，用来证明配错端口、管理面没起、地址换了这些情形
// 都不会让离线恢复挤进去（阶段 5A 的探活实现恰恰会在这些情形下放行）。
func TestAdminRecoverRefusesWhileLockHeldByAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	oldKey := "ak_" + strings.Repeat("a", 64)
	seedDB(t, dbPath, oldKey)
	sum := fileSum(t, dbPath)

	// 真实进程持锁；cleanup 会定向 kill 这个 pid。
	startChild(t, roleHolder, map[string]string{roleDB: dbPath})
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer decoy.Close()

	env := map[string]string{"EPOCHA_PEPPER": testPepperB64}
	keyFile := filepath.Join(dir, "new-admin")
	var out, errb bytes.Buffer
	rc := run(context.Background(), []string{"admin-recover", "--db", dbPath, "--key-file", keyFile},
		getenvOf(env), &out, &errb)
	if rc != 1 {
		t.Fatalf("有真实进程持锁时 admin-recover 退出码 = %d，期望 1: %s", rc, errb.String())
	}
	if !strings.Contains(errb.String(), "停服") || !strings.Contains(errb.String(), "已被另一个进程占用") {
		t.Errorf("拒绝文案没有说明是运行锁冲突: %s", errb.String())
	}
	if got := fileSum(t, dbPath); got != sum {
		t.Error("被拒绝的 admin-recover 仍然改了库")
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Error("被拒绝的 admin-recover 留下了凭据文件")
	}
	// 服务侧（持锁方）不该因为这次尝试被改动：旧凭据仍然有效。
	d, err := store.Open(context.Background(), dbPath, testPepper)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := d.MatchAdminKey(context.Background(), oldKey, time.Now()); err != nil {
		t.Errorf("被拒绝的恢复把存量管理凭据也弄坏了: %v", err)
	}
	_ = d.Close()
}

// TestAdminRecoverSucceedsAfterHolderExit 证明锁的失效方向是对的：
// 持锁进程退出（正常或被 SIGKILL）后不需要任何"清理陈旧锁"动作。
func TestAdminRecoverSucceedsAfterHolderExit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	seedDB(t, dbPath, "ak_"+strings.Repeat("k", 64))
	env := map[string]string{"EPOCHA_PEPPER": testPepperB64}

	holder := startChild(t, roleHolder, map[string]string{roleDB: dbPath})
	rc := run(context.Background(), []string{"admin-recover", "--db", dbPath,
		"--key-file", filepath.Join(dir, "k1")}, getenvOf(env), &bytes.Buffer{}, &bytes.Buffer{})
	if rc != 1 {
		t.Fatalf("持锁期间退出码 = %d，期望 1", rc)
	}
	stopChild(t, holder)

	for attempt := range 3 {
		rc = run(context.Background(), []string{"admin-recover", "--db", dbPath,
			"--key-file", filepath.Join(dir, fmt.Sprintf("k%d", attempt+2))},
			getenvOf(env), &bytes.Buffer{}, &bytes.Buffer{})
		if rc == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond) // 收割是异步的：给内核一点时间
	}
	t.Fatal("持锁进程退出后仍取不到锁")
}

// TestBackupDoesNotNeedTheLock 是反向断言：在线一致性备份**不**要求停服。
// 如果哪天有人"顺手"给 backup 也加上锁，这条会红——那会把备份变成没人愿意按频率做的事。
func TestBackupDoesNotNeedTheLock(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	seedDB(t, dbPath, "ak_"+strings.Repeat("m", 64))
	dest := filepath.Join(dir, "backups")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	// 服务在跑（另一个进程持有数据目录锁）的情况下，在线备份必须照常成功。
	startChild(t, roleHolder, map[string]string{roleDB: dbPath})

	env := map[string]string{"EPOCHA_PEPPER": testPepperB64}
	var out, errb bytes.Buffer
	rc := run(context.Background(), []string{"backup", "--db", dbPath, "--dest", dest},
		getenvOf(env), &out, &errb)
	if rc != 0 {
		t.Fatalf("服务在跑时在线备份应成功（rc=%d）: %s", rc, errb.String())
	}
	if !strings.Contains(out.String(), "备份完成") {
		t.Errorf("备份输出不如预期: %s", out.String())
	}
}

// TestRestoreRefusesWhenOutDirIsInUse 覆盖"隔离恢复被写成就地恢复"这个手滑：
// out-dir 正被某个服务用作数据目录 ⇒ 拒绝，且不产生任何文件。
func TestRestoreRefusesWhenOutDirIsInUse(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	adminKey := "ak_" + strings.Repeat("n", 64)
	seedDB(t, dbPath, adminKey)
	bkDir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(bkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"EPOCHA_PEPPER": testPepperB64, "EPOCHA_ADMIN_KEY": adminKey}
	g := getenvOf(env)
	var out, errb bytes.Buffer
	if rc := run(context.Background(), []string{"backup", "--db", dbPath, "--dest", bkDir}, g, &out, &errb); rc != 0 {
		t.Fatalf("backup: %d %s", rc, errb.String())
	}
	name := backupNameFromOutput(t, out.String())

	// 让"数据目录正在被用"这件事由真实进程持锁来表达。
	holder := startChild(t, roleHolder, map[string]string{roleDB: dbPath})
	args := []string{"restore", "--backup", filepath.Join(bkDir, name+".db"),
		"--manifest", filepath.Join(bkDir, name+".manifest.json"), "--out-dir", dir}
	out.Reset()
	errb.Reset()
	if rc := run(context.Background(), args, g, &out, &errb); rc != 1 {
		t.Fatalf("out-dir 正在被服务使用时 restore 退出码 = %d，期望 1: %s", rc, errb.String())
	}
	if !strings.Contains(errb.String(), "恢复输出目录正被某个服务用作数据目录") {
		t.Errorf("拒绝文案没有点出就地恢复的风险: %s", errb.String())
	}
	// 除了 seedDB 自己的 epocha.db，恢复不得在该目录留下产物。
	if _, err := os.Stat(filepath.Join(dir, "epocha.db")); err != nil {
		t.Fatalf("工作库不见了: %v", err)
	}
	stopChild(t, holder)

	// 同一份备份、换个隔离目录就应当成功——证明刚才拦住它的是"目录正在被用"这把锁，
	// 不是备份文件本身、也不是 out-dir 的权限。
	iso := filepath.Join(dir, "isolated")
	if err := os.MkdirAll(iso, 0o700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	args2 := []string{"restore", "--backup", filepath.Join(bkDir, name+".db"),
		"--manifest", filepath.Join(bkDir, name+".manifest.json"), "--out-dir", iso}
	if rc := run(context.Background(), args2, g, &out, &errb); rc != 0 {
		t.Fatalf("释放锁后恢复到隔离目录应成功（rc=%d）: %s", rc, errb.String())
	}
	if _, err := os.Stat(filepath.Join(iso, "epocha.db")); err != nil {
		t.Errorf("隔离恢复没有产出文件: %v", err)
	}
}

// TestTwoAdminRecoverProcessesRace 是两个真实进程抢同一数据集：
// 恰好一个成功；失败方不得留下凭据文件，也不得改库。
func TestTwoAdminRecoverProcessesRace(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	seedDB(t, dbPath, "ak_"+strings.Repeat("p", 64))
	sum := fileSum(t, dbPath)

	// 同一个进程内先拿到锁的那一方会赢；这里用两个**子进程**保证是真的跨进程竞态。
	a := startChild(t, roleRecover, map[string]string{
		roleDB: dbPath, "EPOCHA_PEPPER": testPepperB64, "EPOCHA_KEYFILE": filepath.Join(dir, "ra"),
	})
	b := startChild(t, roleRecover, map[string]string{
		roleDB: dbPath, "EPOCHA_PEPPER": testPepperB64, "EPOCHA_KEYFILE": filepath.Join(dir, "rb"),
	})
	rcA, okA := waitChild(a, 20*time.Second)
	rcB, okB := waitChild(b, 20*time.Second)
	if !okA || !okB {
		t.Fatalf("恢复子进程没收尾（A=%v B=%v）", okA, okB)
	}
	if rcA == 0 && rcB == 0 {
		t.Fatal("两个 admin-recover 都成功了——管理凭据被写了两遍")
	}
	if rcA != 0 && rcB != 0 {
		t.Fatalf("两个 admin-recover 都失败了（A=%d B=%d）", rcA, rcB)
	}
	if got := fileSum(t, dbPath); got == sum {
		t.Error("赢家没有改库（说明两条路径都没真的执行）")
	}
	// 输家不得留下凭据文件。
	loser := "ra"
	if rcA == 0 {
		loser = "rb"
	}
	if _, err := os.Stat(filepath.Join(dir, loser)); !os.IsNotExist(err) {
		t.Errorf("输家留下了凭据文件 %s", loser)
	}
}

func TestAdminRecoverWritesKeyFileAndTakesEffect(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	oldKey := "ak_" + strings.Repeat("b", 64)
	seedDB(t, dbPath, oldKey)

	env := map[string]string{"EPOCHA_PEPPER": testPepperB64}
	keyFile := filepath.Join(dir, "new-admin")
	var out, errb bytes.Buffer
	rc := run(context.Background(), []string{"admin-recover", "--db", dbPath, "--key-file", keyFile}, getenvOf(env), &out, &errb)
	if rc != 0 {
		t.Fatalf("admin-recover 退出码 = %d: %s", rc, errb.String())
	}
	fi, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("凭据文件没写出来: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("凭据文件权限 = %o，期望 600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	newKey := string(raw)
	if len(newKey) < 32 || !strings.HasPrefix(newKey, "ak_") {
		t.Errorf("凭据内容形状不对（长度 %d）", len(newKey))
	}
	// 输出里只有 key_id 与文件名，不得有凭据值。
	if strings.Contains(out.String(), newKey) || strings.Contains(errb.String(), newKey) {
		t.Error("admin-recover 的输出带出了新凭据值")
	}
	if !strings.Contains(out.String(), "key_id=k") {
		t.Errorf("输出没有给出可核对的 key_id: %s", out.String())
	}
	// 新凭据真的能鉴权、旧凭据真的失效（这条路径就是产品语义本身）。
	d, err := store.Open(context.Background(), dbPath, testPepper)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = d.Close() }()
	if _, err := d.MatchAdminKey(context.Background(), newKey, time.Now()); err != nil {
		t.Errorf("恢复写出的凭据不可用: %v", err)
	}
	if _, err := d.MatchAdminKey(context.Background(), oldKey, time.Now()); err == nil {
		t.Error("旧凭据在恢复后仍可用（等于留了一个没人知道的后门）")
	}
}

func TestAdminRecoverGuardsItsOwnInputs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "epocha.db")
	seedDB(t, dbPath, "ak_"+strings.Repeat("c", 64))
	sum := fileSum(t, dbPath)
	env := map[string]string{"EPOCHA_PEPPER": testPepperB64}
	var out, errb bytes.Buffer

	// 缺 --key-file 是用法错误。
	if rc := run(context.Background(), []string{"admin-recover", "--db", dbPath}, getenvOf(env), &out, &errb); rc != 2 {
		t.Errorf("缺 --key-file 退出码 = %d，期望 2", rc)
	}
	// 端口探测这个入口已经不存在了：判据是锁。留下这条断言是为了防止有人"顺手"把它加回来——
	// 一个可以指向任意地址的 flag 会让互斥重新变成一次可选的对外请求。
	if rc := run(context.Background(), []string{"admin-recover", "--db", dbPath,
		"--key-file", filepath.Join(dir, "k"), "--admin-base-url", "http://127.0.0.1:8081"},
		getenvOf(env), &out, &errb); rc != 2 {
		t.Errorf("--admin-base-url 已废除，仍被接受（退出码 %d，期望 2 用法错误）", rc)
	}
	// 凭据文件已存在 ⇒ 拒绝覆盖（覆盖一个可能正在被使用的凭据不可逆）。
	existing := filepath.Join(dir, "exists")
	if err := os.WriteFile(existing, []byte("do-not-overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := run(context.Background(), []string{"admin-recover", "--db", dbPath,
		"--key-file", existing}, getenvOf(env), &out, &errb); rc != 1 {
		t.Errorf("已存在凭据文件的退出码 = %d，期望 1", rc)
	}
	if b, _ := os.ReadFile(existing); string(b) != "do-not-overwrite" {
		t.Error("被拒绝的恢复覆盖了已有凭据文件")
	}
	if got := fileSum(t, dbPath); got != sum {
		t.Error("上述任一失败路径改了库")
	}
	// pepper 缺失 ⇒ 拒绝（摘要算法依赖它，缺了就写出一把永远验不过的凭据）。
	out.Reset()
	errb.Reset()
	if rc := run(context.Background(), []string{"admin-recover", "--db", dbPath,
		"--key-file", filepath.Join(dir, "k2")},
		getenvOf(map[string]string{}), &out, &errb); rc != 1 {
		t.Errorf("缺 pepper 退出码 = %d，期望 1", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, "k2")); !os.IsNotExist(err) {
		t.Error("失败的恢复留下了凭据文件")
	}
}

// --- 真实子进程工装（跨进程互斥的证据）---
//
// 与 cmd/server 的那一组同构：重新执行本测试二进制，用 EPOCHA_TEST_ROLE 分派角色。
// 锁的意义在进程边界上，只在同一个进程里测它证明不了运维场景。

const (
	roleEnv     = "EPOCHA_TEST_ROLE"
	roleDB      = "EPOCHA_TEST_DB"
	roleKeyFile = "EPOCHA_KEYFILE"
	roleHolder  = "holder"  // 只持有数据目录锁
	roleRecover = "recover" // 真跑一次 admin-recover
)

func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case roleHolder:
		l, err := store.AcquireDataLock(os.Getenv(roleDB))
		if err != nil {
			fmt.Fprintf(os.Stderr, "role-holder: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("locked")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
		<-sig
		_ = l.Close()
		os.Exit(0)
	case roleRecover:
		env := func(k string) string {
			if v, ok := os.LookupEnv(k); ok {
				return v
			}
			return ""
		}
		rc := run(context.Background(),
			[]string{"admin-recover", "--db", env(roleDB), "--key-file", env(roleKeyFile)},
			env, os.Stdout, os.Stderr)
		os.Exit(rc)
	default:
		os.Exit(m.Run())
	}
}

type child struct {
	cmd  *exec.Cmd
	out  *os.File
	rc   int
	exit chan struct{}
}

func startChild(t *testing.T, role string, env map[string]string) *child {
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
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		t.Fatalf("启动 %s 子进程: %v", role, err)
	}
	c := &child{cmd: cmd, out: f, rc: -999, exit: make(chan struct{})}
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
				c.rc = -2
			}
		}
		close(c.exit)
	}()
	// 等 holder 真的把锁拿到，否则"竞争"测试可能测的是两个都没开始跑的进程。
	if role == roleHolder {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(logPath); err == nil && strings.Contains(string(b), "locked") {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Cleanup(func() {
		// 只 kill 本测试启动的这个 pid。
		_ = c.cmd.Process.Kill()
		select {
		case <-c.exit:
		case <-time.After(5 * time.Second):
			t.Errorf("子进程 (pid %d) 没被收割", c.cmd.Process.Pid)
		}
		_ = c.out.Close()
	})
	return c
}

func waitChild(c *child, timeout time.Duration) (int, bool) {
	select {
	case <-c.exit:
		return c.rc, true
	case <-time.After(timeout):
		return 0, false
	}
}

func stopChild(t *testing.T, c *child) {
	t.Helper()
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	if _, ok := waitChild(c, 10*time.Second); !ok {
		t.Fatalf("持锁子进程在 SIGTERM 后 10s 内没退出")
	}
}
