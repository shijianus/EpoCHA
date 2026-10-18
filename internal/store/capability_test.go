package store

// VACUUM INTO 能力测试。
//
// deploy.go 的整个备份方案建立在四条 SQLite 事实上，而它们**不是**我从文档推断来的：
// 本机 modernc.org/sqlite v1.46.1 的实际行为由这一组断言钉住。
// 任何一条哪天不再成立，备份就必须换成"停服冷备份"，而不是继续假装 cp 主文件安全。
// （教训与 AGENTS.md "能力边界必须由能力测试给出" 同源。）

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestCapabilityVacuumInto(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	d, _ := newDBAt(t, src)
	mustSite(t, d, "alpha")
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 事实 1：备份路径以 mode=ro 打开源库即可执行 VACUUM INTO —— 备份动作不写源文件。
	h, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开只读源库: %v", err)
	}
	defer func() { _ = h.Close() }()

	out := filepath.Join(dir, "out1.db")
	if _, err := h.Exec(`VACUUM INTO ?`, out); err != nil {
		t.Fatalf("只读连接上的 VACUUM INTO 不可用（备份方案需改为停服冷备份）: %v", err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("快照未产出或为空: %v", err)
	}

	// 事实 2：VACUUM INTO 的目标名可以走绑定参数，不需要字符串拼接（避免路径注入面）。
	if _, err := h.Exec(`VACUUM INTO ?`, filepath.Join(dir, "out2.db")); err != nil {
		t.Errorf("绑定参数形式不可用: %v", err)
	}

	// 事实 3：源库**未 checkpoint 的 WAL 内容会进快照**——这正是不能 cp 主文件的理由。
	d2, _ := newDBAt(t, src)
	mustSite(t, d2, "beta")
	snap := filepath.Join(dir, "out3.db")
	if _, err := h.Exec(`VACUUM INTO ?`, snap); err != nil {
		t.Fatalf("WAL 活跃时快照失败: %v", err)
	}
	sh, err := sql.Open("sqlite", "file:"+snap+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开快照: %v", err)
	}
	defer func() { _ = sh.Close() }()
	var n int
	if err := sh.QueryRow(`SELECT count(*) FROM sites`).Scan(&n); err != nil {
		t.Fatalf("快照读取失败: %v", err)
	}
	if n != 2 {
		t.Errorf("快照站点数 = %d，期望 2：WAL 里最新的写入没进快照，备份会漏状态", n)
	}
	if err := d2.Close(); err != nil {
		t.Fatalf("Close d2: %v", err)
	}

	// 事实 4：目标目录不可用时直接报错，且**不**在别处留下同名半成品文件。
	bad := filepath.Join(dir, "nope", "missing.db")
	if _, err := h.Exec(`VACUUM INTO ?`, bad); err == nil {
		t.Error("写到不存在的目录竟然成功——失败必须可检测")
	}
	if _, err := os.Stat(filepath.Join(dir, "nope", "missing.db")); !os.IsNotExist(err) {
		t.Error("失败的 VACUUM INTO 留下了文件")
	}
}
