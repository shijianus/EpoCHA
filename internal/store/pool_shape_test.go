package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestMigration0003Shape 钉住 0003 迁移落地后的实际约束（不是"看起来建了表"，而是逐条验证语义）：
// partial UNIQUE 生效、NULL 不受该索引约束、外键挡悬挂绑定、旧站点默认 pow_only。
func TestMigration0003Shape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	d, _ := newDBAt(t, path)
	if v, _ := d.SchemaVersion(t.Context()); v != 3 {
		t.Fatalf("schema version = %d，期望 3", v)
	}
	var n int
	if err := d.h.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE type='index'
AND name IN ('pool_bound_challenge','pool_claim','pool_expire')`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("题池索引数量 = %d (err %v)", n, err)
	}
	s := mustSite(t, d, "alpha")
	c := newChallenge(t, d, s.ID, time.Hour)

	ins := func(id, status string, cid any) error {
		_, err := d.h.ExecContext(t.Context(), `
INSERT INTO pool(id, generator_id, version, difficulty, status, payload_json, verifier_blob, created_at, expires_at, challenge_id)
VALUES(?,?,?,?,?,?,x'01','2026-01-01T00:00:00.000000000Z','2030-01-01T00:00:00.000000000Z',?)`,
			id, "g", "v1", 1, status, "{}", cid)
		return err
	}
	if err := ins("p1", PoolAssigned, c.ID); err != nil {
		t.Fatalf("首条绑定应成功: %v", err)
	}
	if err := ins("p2", PoolAssigned, c.ID); err == nil {
		t.Fatal("partial UNIQUE 未生效：同一 challenge 绑了两条题")
	}
	for i := 0; i < 2; i++ {
		if err := ins("r"+string(rune('0'+i)), PoolReady, nil); err != nil {
			t.Fatalf("ready 行的 challenge_id 为 NULL 时不应受唯一约束限制: %v", err)
		}
	}
	if err := ins("orphan", PoolAssigned, "no-such-challenge"); err == nil {
		t.Fatal("外键未生效：可以绑到不存在的 challenge")
	}
	// 站点默认必须是 pow_only：迁移不得静默改变现有业务方的响应形状
	site, err := d.SiteByID(t.Context(), s.ID)
	if err != nil || site.Policy.VerifyMode != ModePowOnly || site.Policy.Difficulty != MinDifficulty {
		t.Fatalf("站点默认模式不符: %+v err %v", site, err)
	}
	// challenge 的旧行读成 pow_only
	got, err := d.Challenge(t.Context(), c.ID)
	if err != nil || got.Mode != ModePowOnly || got.GeneratorID != "" {
		t.Fatalf("challenge 模式读回不符: %+v err %v", got, err)
	}
}
