package store

import "testing"

func TestOpenSetsWALAndPragmas(t *testing.T) {
	t.Skip("待阶段 1：t.TempDir() 建库，断言 journal_mode=WAL、busy_timeout=5000、foreign_keys=ON")
}

func TestMigrationIdempotent(t *testing.T) {
	t.Skip("待阶段 2（DESIGN §17 阶段 2）：同一迁移重复执行后 schema_version 不变且无错误")
}

func TestAtomicClaimSingleWinner(t *testing.T) {
	t.Skip("待阶段 1（DESIGN §17 ⑦）：100 goroutine 并发消费同一 nonce → 恰好 1 个成功")
}

func TestConsumeUsesRowsAffected(t *testing.T) {
	t.Skip("待阶段 3：UPDATE ... WHERE status='ready' 返回 0 行时调用方必须放弃，不得读-判-写回")
}

func TestSecretStoredAsMacOnly(t *testing.T) {
	t.Skip("待阶段 2（DESIGN §17 阶段 2）：建站点后直接读 DB 文件，明文 secret 不得出现")
}
