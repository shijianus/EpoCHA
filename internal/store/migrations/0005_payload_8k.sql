-- 0005_payload_8k.sql：交互式题型底座（ADR-028）。
--
-- 0003 把 pool.payload_json 的 CHECK 写死为 4096B；SQLite 不能原地修改 CHECK，
-- 因此这里重建 pool 表。迁移必须保留全部历史行、challenge 绑定、资产列与索引；
-- 0003/0004 本身保持只读，不回头改写历史迁移。
--
-- 新上限 8192B 与 Go 侧 store.MaxPayloadBytes 必须一致；两侧各有测试
-- （Go 侧拒绝、库侧 CHECK 拒绝、8192 边界接受）。

CREATE TABLE pool_v5 (
  id            TEXT PRIMARY KEY,
  generator_id  TEXT NOT NULL,
  version       TEXT NOT NULL,
  difficulty    INTEGER NOT NULL CHECK (difficulty BETWEEN 1 AND 5),
  status        TEXT NOT NULL CHECK (status IN ('ready','assigned','terminal')),
  payload_json  TEXT NOT NULL CHECK (length(payload_json) <= 8192),
  verifier_blob BLOB NOT NULL,
  created_at    TEXT NOT NULL,
  expires_at    TEXT NOT NULL,
  challenge_id  TEXT REFERENCES challenges(id),
  assigned_at   TEXT,
  terminal_at   TEXT,
  asset_blob    BLOB CHECK (asset_blob IS NULL OR length(asset_blob) <= 65536),
  asset_mime    TEXT NOT NULL DEFAULT '' CHECK (asset_mime IN ('', 'image/png'))
);

INSERT INTO pool_v5 (
  id, generator_id, version, difficulty, status, payload_json, verifier_blob,
  created_at, expires_at, challenge_id, assigned_at, terminal_at, asset_blob, asset_mime
)
SELECT
  id, generator_id, version, difficulty, status, payload_json, verifier_blob,
  created_at, expires_at, challenge_id, assigned_at, terminal_at, asset_blob, asset_mime
FROM pool;

DROP TABLE pool;
ALTER TABLE pool_v5 RENAME TO pool;

CREATE UNIQUE INDEX IF NOT EXISTS pool_bound_challenge ON pool(challenge_id) WHERE challenge_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS pool_claim  ON pool(generator_id, version, difficulty, status, expires_at);
CREATE INDEX IF NOT EXISTS pool_expire ON pool(status, expires_at);
