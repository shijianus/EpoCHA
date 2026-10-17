-- 0001_stage1.sql：阶段 1 的最小防重放账本（DESIGN §5.1）。
-- 站点由配置装载（完整站点管理属阶段 2）；无 pool/sessions/gen_stats（阶段 3+）。

CREATE TABLE IF NOT EXISTS sites (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  site_key   TEXT NOT NULL UNIQUE,
  secret_mac TEXT NOT NULL UNIQUE,
  pow_bits   INTEGER NOT NULL CHECK (pow_bits BETWEEN 12 AND 24),
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS challenges (
  id          TEXT PRIMARY KEY,
  site_id     INTEGER NOT NULL REFERENCES sites(id),
  pow_seed    BLOB NOT NULL,
  pow_bits    INTEGER NOT NULL CHECK (pow_bits BETWEEN 12 AND 24),
  status      TEXT NOT NULL CHECK (status IN ('issued','redeemed','invalid')),
  attempts    INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 8),
  created_at  TEXT NOT NULL,
  expires_at  TEXT NOT NULL,
  redeemed_at TEXT
);

CREATE INDEX IF NOT EXISTS challenges_site_status ON challenges(site_id, status, expires_at);

CREATE TABLE IF NOT EXISTS responses (
  id           TEXT PRIMARY KEY,
  challenge_id TEXT NOT NULL UNIQUE REFERENCES challenges(id),
  site_id      INTEGER NOT NULL REFERENCES sites(id),
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  used         INTEGER NOT NULL DEFAULT 0 CHECK (used IN (0,1))
);

CREATE INDEX IF NOT EXISTS responses_site ON responses(site_id, used, expires_at);
