package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestPayloadAndVerifierLimits 钉住 ADR-028 的两条新预算：
// 8192B payload / 4096B verifier 接受，超 1 字节拒绝；payload 还要在库层再挡一次。
func TestPayloadAndVerifierLimits(t *testing.T) {
	if MaxPayloadBytes != 8192 || MaxVerifierBytes != 4096 {
		t.Fatalf("预算常量 = (%d,%d)，期望 (8192,4096)", MaxPayloadBytes, MaxVerifierBytes)
	}
	d := newDB(t)
	questionSite(t, d, "alpha")
	base := func() *PoolQuestion {
		now := time.Now().UTC()
		return &PoolQuestion{
			ID: "q-" + randSuffix(), GeneratorID: testSpec.GeneratorID, Version: testSpec.Version,
			Difficulty: testSpec.Difficulty, Status: PoolReady,
			PayloadJSON: `{"task":"1 + 1","answer_type":"integer","max_answer_len":14}`,
			Verifier:    []byte("v"), CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
	}

	exactPayload := base()
	exactPayload.PayloadJSON = pad(exactPayload.PayloadJSON, MaxPayloadBytes)
	if err := d.InsertQuestion(t.Context(), exactPayload, poolLimits(100)); err != nil {
		t.Fatalf("payload=%d 应接受: %v", MaxPayloadBytes, err)
	}
	overPayload := base()
	overPayload.PayloadJSON = pad(overPayload.PayloadJSON, MaxPayloadBytes+1)
	if err := d.InsertQuestion(t.Context(), overPayload, poolLimits(100)); err == nil {
		t.Fatalf("payload=%d 应拒绝", MaxPayloadBytes+1)
	}

	exactVerifier := base()
	exactVerifier.Verifier = []byte(pad("v", MaxVerifierBytes))
	if err := d.InsertQuestion(t.Context(), exactVerifier, poolLimits(100)); err != nil {
		t.Fatalf("verifier=%d 应接受: %v", MaxVerifierBytes, err)
	}
	overVerifier := base()
	overVerifier.Verifier = []byte(pad("v", MaxVerifierBytes+1))
	if err := d.InsertQuestion(t.Context(), overVerifier, poolLimits(100)); err == nil {
		t.Fatalf("verifier=%d 应拒绝", MaxVerifierBytes+1)
	}

	// 直接绕过 Go 侧，证明库内 CHECK 也改成 8192。
	raw := func(payload string) error {
		now := time.Now().UTC()
		_, err := d.h.ExecContext(context.Background(), `
INSERT INTO pool(id, generator_id, version, difficulty, status, payload_json, verifier_blob, created_at, expires_at)
VALUES(?,?,?,?,?,?,?,?,?)`,
			"raw-"+randSuffix(), testSpec.GeneratorID, testSpec.Version, testSpec.Difficulty,
			PoolReady, payload, []byte("v"), formatTime(now), formatTime(now.Add(time.Hour)))
		return err
	}
	if err := raw(pad("x", 8192)); err != nil {
		t.Fatalf("库内 CHECK 应接受 8192B: %v", err)
	}
	if err := raw(pad("x", 8193)); err == nil {
		t.Fatal("库内 CHECK 应拒绝 8193B")
	}
}

// TestMigration0005PreservesAssets 从 3B 库（0001..0004）升级：
// 0005 重建 pool 时必须保留资产列、行与索引，而不是只改常量。
func TestMigration0005PreservesAssets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stage3b.db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	h, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{migration0001, migration0002, migration0003, migration0004} {
		if _, err := h.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("应用阶段迁移: %v", err)
		}
	}
	ctx := context.Background()
	if _, err := h.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
		 INSERT INTO meta(k, v) VALUES('schema_version', '4')`); err != nil {
		t.Fatal(err)
	}
	asset := []byte{1, 2, 3, 4, 5}
	if _, err := h.ExecContext(ctx, `
INSERT INTO pool(id, generator_id, version, difficulty, status, payload_json, verifier_blob,
                 created_at, expires_at, asset_blob, asset_mime)
VALUES('legacy-asset','text-image-v1','v1',3,'ready','{"length":6,"charset":"AB","answer_type":"text","max_answer_len":6,"image_width":280,"image_height":104}',
       x'0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20',
       '2026-01-01T00:00:00.000000000Z','2030-01-01T00:00:00.000000000Z',?, 'image/png')`, asset); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(ctx, path, testPepper())
	if err != nil {
		t.Fatalf("打开 3B 库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if v, _ := d.SchemaVersion(ctx); v != 6 {
		t.Fatalf("升级后版本 = %d，期望 6", v)
	}
	var got []byte
	var mime string
	if err := d.h.QueryRowContext(ctx,
		`SELECT asset_blob, asset_mime FROM pool WHERE id='legacy-asset'`).Scan(&got, &mime); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(asset) || mime != "image/png" {
		t.Fatalf("资产未保留: got=%v mime=%q", got, mime)
	}
	var n int
	if err := d.h.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index'
AND name IN ('pool_bound_challenge','pool_claim','pool_expire')`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("重建后索引数量 = %d (err %v)", n, err)
	}
}
