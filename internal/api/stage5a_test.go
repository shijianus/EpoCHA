package api

// 阶段 5A：/readyz 就绪端点。
//
// 这一组测试钉住三条要求：探针**不改状态**（不分配题、不消费 token）、
// 写路径坏掉时必须 fail closed（不能"进程活着但每次验证都失败"还报就绪）、
// 响应不外泄内部信息（路径、驱动、站点名、失败原因）。

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ledgerCounts(t *testing.T, path string) map[string]int64 {
	t.Helper()
	h, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("打开库做计数: %v", err)
	}
	defer func() { _ = h.Close() }()
	out := map[string]int64{}
	for _, q := range []string{`SELECT count(*) FROM sites`, `SELECT count(*) FROM challenges`,
		`SELECT count(*) FROM responses`, `SELECT count(*) FROM pool`, `SELECT count(*) FROM audit`} {
		var n int64
		if err := h.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		out[q] = n
	}
	return out
}

func TestReadyzReadyAndLeakFree(t *testing.T) {
	h := newHarness(t, nil)
	r := h.do(http.MethodGet, "/readyz", nil, "", "203.0.113.7:1", nil)
	if r.status != http.StatusOK {
		t.Fatalf("健康实例应就绪，得到 %d: %s", r.status, r.body)
	}
	assertKeySet(t, r, "status")
	if r.str("status") != "ready" {
		t.Errorf("status = %q", r.str("status"))
	}
	for _, leak := range []string{h.dbPath, "epocha.db", "sqlite", "alpha", "bravo", "schema", "pool", "wal"} {
		if strings.Contains(strings.ToLower(r.body), strings.ToLower(leak)) {
			t.Errorf("就绪响应泄露 %q: %s", leak, r.body)
		}
	}
}

func TestReadyzDoesNotChangeAnyState(t *testing.T) {
	h := newHarness(t, nil)
	// 带一点真实内容再对账：空库上"行数没变"证明不了探针没领题。
	h.seedQuestions(t, 1, arithQuestion("1+2", "3"))
	before := ledgerCounts(t, h.dbPath)
	for range 25 {
		if r := h.do(http.MethodGet, "/readyz", nil, "", "203.0.113.7:1", nil); r.status != http.StatusOK {
			t.Fatalf("第 25 次之前的某次不就绪: %d %s", r.status, r.body)
		}
	}
	after := ledgerCounts(t, h.dbPath)
	for q, want := range before {
		if after[q] != want {
			t.Errorf("%s: 就绪探针改变了行数 %d → %d", q, want, after[q])
		}
	}
}

func TestReadyzFailsClosedWhenDataDirUnwritable(t *testing.T) {
	h := newHarness(t, nil)
	dir := filepath.Dir(h.dbPath)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Skipf("无法把数据目录改成只读（环境限制）: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	probe, probeErr := os.CreateTemp(dir, ".permission-capability-*")
	if probeErr == nil {
		probe.Close()
		if err := os.Remove(probe.Name()); err != nil {
			t.Fatal(err)
		}
		t.Skip("BLOCKED: current identity/filesystem can still create files after chmod 0555")
	}
	if !os.IsPermission(probeErr) {
		t.Fatalf("permission capability probe failed for another reason: %v", probeErr)
	}
	r := h.do(http.MethodGet, "/readyz", nil, "", "203.0.113.7:1", nil)
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("WAL 无法创建时仍报就绪（%d）——这正是要防的『进程活着但每次验证都失败』", r.status)
	}
	assertKeySet(t, r, "status")
	if r.str("status") != "unavailable" {
		t.Errorf("status = %q", r.str("status"))
	}
	for _, leak := range []string{h.dbPath, "epocha.db", "sqlite", "permission", "directory", "SQLITE"} {
		if strings.Contains(strings.ToLower(r.body), strings.ToLower(leak)) {
			t.Errorf("未就绪响应泄露失败细节 %q: %s", leak, r.body)
		}
	}
	// 失败原因只能进日志，且日志里也不许出现密钥/站点 secret 值。
	if !strings.Contains(h.logs.String(), "未就绪") {
		t.Error("未就绪没有留下任何可排障的日志行")
	}
	if strings.Contains(h.logs.String(), sentinelSecret) {
		t.Error("日志泄露了站点 secret")
	}
}

func TestReadyzRespectsRequestDeadline(t *testing.T) {
	h := newHarness(t, nil)
	// 探针必须沿用调用方的截止：探针自己变成阻塞源是不可接受的（阶段 5A 第 7 条）。
	if err := h.db.Ready(t.Context()); err != nil {
		t.Fatalf("前置条件：库应就绪: %v", err)
	}
	cctx, cancel := context.WithTimeout(t.Context(), 10*time.Nanosecond)
	defer cancel()
	if err := h.db.Ready(cctx); err == nil {
		t.Fatal("已过期的 ctx 下仍返回就绪")
	}
}

func TestReadyzIsPublicButAdminRoutesAreNot(t *testing.T) {
	h := newHarness(t, nil)
	// 就绪探针挂在公共监听器上；管理路由在公共端口根本不存在（既有红线）。
	for _, path := range []string{"/readyz", "/healthz"} {
		if r := h.do(http.MethodGet, path, nil, "", "203.0.113.7:1", nil); r.status == http.StatusNotFound {
			t.Errorf("%s 应该挂在公共监听器上", path)
		}
	}
	for _, path := range []string{"/api/admin/sites", "/api/admin/pool", "/api/admin/generators"} {
		if r := h.do(http.MethodGet, path, nil, "", "203.0.113.7:1", nil); r.status != http.StatusNotFound {
			t.Errorf("管理路由 %s 出现在公共监听器上（status=%d）", path, r.status)
		}
	}
}
