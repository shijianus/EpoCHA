-- 0003_stage3a.sql：阶段 3A 的生成器接入、题池与 challenge 的题目绑定（DESIGN §5.4、ADR-011）。
--
-- 只增不减的增量迁移：阶段 1/2 的站点与账本数据全部保留；
-- 旧站点迁移后是 verify_mode='pow_only'（默认值），**不静默改变现有业务行为**，
-- pow_question 只能通过管理 API 显式开启。

-- 1) 站点：验证模式与生成器选择（3A 只允许 arithmetic-baseline-v1，由 Go 侧白名单校验）
ALTER TABLE sites ADD COLUMN verify_mode TEXT NOT NULL DEFAULT 'pow_only'
  CHECK (verify_mode IN ('pow_only','pow_question'));
ALTER TABLE sites ADD COLUMN generator_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN difficulty INTEGER NOT NULL DEFAULT 1 CHECK (difficulty BETWEEN 1 AND 5);

-- 2) challenge：签发时的模式与生成器快照。
--    模式与生成器写进签名（token claims 的 mode/gen）并与这一行交叉核对，
--    因此客户端既不能把 pow_question 降级成 pow_only，也不能换生成器或自己声称"这题不算"。
ALTER TABLE challenges ADD COLUMN mode TEXT NOT NULL DEFAULT 'pow_only';
ALTER TABLE challenges ADD COLUMN generator_id TEXT NOT NULL DEFAULT '';

-- 3) 题池。绑定关系只存在一个地方：pool.challenge_id（唯一），
--    challenges 不反向存 pool_id——两处都存就会出现"谁是真的"的问题，而且删除顺序会互相咬。
CREATE TABLE IF NOT EXISTS pool (
  id            TEXT PRIMARY KEY,
  generator_id  TEXT NOT NULL,
  version       TEXT NOT NULL,                              -- 题型协议版本，与 generator_id 一起决定判分实现
  difficulty    INTEGER NOT NULL CHECK (difficulty BETWEEN 1 AND 5),
  status        TEXT NOT NULL CHECK (status IN ('ready','assigned','terminal')),
  -- 公开题面：只由生成器的**具名公开结构体**序列化而来；私有判分数据绝不进这里（ADR-011 #5）
  payload_json  TEXT NOT NULL CHECK (length(payload_json) <= 4096),
  -- 私有 verifier：nonce(16B) || HMAC(K(qver,kid), "epocha/v1|qver|" || nonce || 0x00 || 规范化答案)
  -- 答案明文不入库（红线 5）；带 per-题 nonce 是为了让同答案的题摘要也不同，
  -- 只读到库的人无法靠"摘要相同"聚类出"这些题同答案"。形态见 DESIGN §5.4
  verifier_blob BLOB NOT NULL,
  created_at    TEXT NOT NULL,
  expires_at    TEXT NOT NULL,
  challenge_id  TEXT REFERENCES challenges(id),             -- 仅 assigned/terminal 期间非空
  assigned_at   TEXT,
  terminal_at   TEXT
);

-- 一题只能绑一个 challenge；NULL（ready 行）不受该唯一约束限制，故用 partial index。
CREATE UNIQUE INDEX IF NOT EXISTS pool_bound_challenge ON pool(challenge_id) WHERE challenge_id IS NOT NULL;
-- 领取查询的形态：WHERE generator_id=? AND version=? AND difficulty=? AND status='ready' AND expires_at>?
CREATE INDEX IF NOT EXISTS pool_claim ON pool(generator_id, version, difficulty, status, expires_at);
CREATE INDEX IF NOT EXISTS pool_expire ON pool(status, expires_at);
