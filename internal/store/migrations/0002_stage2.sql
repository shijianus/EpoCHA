-- 0002_stage2.sql：阶段 2 的多站点管理、密钥生命周期、来源策略与审计（DESIGN §5.2、§10.2）。
--
-- 只增不减的增量迁移：阶段 1 的库升级后，站点、challenge、response 三张账本表的数据全部保留，
-- 已消费/已过期的防重放状态不因升级而丢失（有测试证明）。
-- 注意：SQLite 3.51.2 实测**不能** DROP 带 UNIQUE 约束的列（"cannot drop UNIQUE column"），
-- 因此 sites.secret_mac 保留在 schema 里但不再参与任何判定——迁移脚本执行后，Go 侧会把它的值
-- 替换成 HMAC(pepper, "epocha/v1|retired|" + 旧摘要)，任何真实 secret 都无法与之匹配（DESIGN §5.3）。

-- 1) 站点生命周期与站点级策略
ALTER TABLE sites ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1));
ALTER TABLE sites ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 2147483647);
ALTER TABLE sites ADD COLUMN challenge_ttl_sec INTEGER NOT NULL DEFAULT 180 CHECK (challenge_ttl_sec BETWEEN 1 AND 3600);
ALTER TABLE sites ADD COLUMN response_ttl_sec INTEGER NOT NULL DEFAULT 120 CHECK (response_ttl_sec BETWEEN 1 AND 3600);
ALTER TABLE sites ADD COLUMN rate_ip_rpm INTEGER NOT NULL DEFAULT 60 CHECK (rate_ip_rpm BETWEEN 1 AND 100000);
ALTER TABLE sites ADD COLUMN rate_site_rpm INTEGER NOT NULL DEFAULT 300 CHECK (rate_site_rpm BETWEEN 1 AND 100000);
-- 无 Origin 的直接 API 调用是否放行：默认拒绝。演示客户端必须显式打开，不能对所有站点静默放开。
ALTER TABLE sites ADD COLUMN allow_missing_origin INTEGER NOT NULL DEFAULT 0 CHECK (allow_missing_origin IN (0,1));

-- 2) challenge 绑定签发时看到的规范化 Origin（''= 无浏览器来源且站点允许）。
--    /siteverify 返回的 hostname 取自这一列，不取自调用方自报值。
ALTER TABLE challenges ADD COLUMN origin TEXT NOT NULL DEFAULT '';

-- 3) 验证凭据：一个站点可同时持有 1..N 把（过渡期内新旧并存），明文不落库
CREATE TABLE IF NOT EXISTS site_secrets (
  key_id     TEXT PRIMARY KEY,                              -- 公开标识，与 secret 的随机部分彼此独立
  site_id    INTEGER NOT NULL REFERENCES sites(id),
  secret_mac TEXT NOT NULL UNIQUE,                          -- HMAC(pepper, secret)
  created_at TEXT NOT NULL,
  revoked_at TEXT                                           -- NULL=有效；非空=该时刻起失效（立即撤销=当时）
);
CREATE INDEX IF NOT EXISTS site_secrets_site ON site_secrets(site_id, revoked_at);

-- 4) 允许来源：规范化后的完整 Origin 串精确匹配。无通配符、无后缀匹配、不允许 null。
CREATE TABLE IF NOT EXISTS site_origins (
  site_id    INTEGER NOT NULL REFERENCES sites(id),
  origin     TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (site_id, origin)
);

-- 5) 管理凭据（与站点 secret 同样的摘要存储 + 同样的撤销时刻语义）
CREATE TABLE IF NOT EXISTS admin_keys (
  key_id     TEXT PRIMARY KEY,
  label      TEXT NOT NULL UNIQUE,
  key_mac    TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  revoked_at TEXT
);

-- 6) 最小审计：只记操作事实，不记凭据、不记请求体
CREATE TABLE IF NOT EXISTS audit (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts       TEXT NOT NULL,
  actor    TEXT NOT NULL,                                   -- 管理者 key_id（公开标识，不是密钥）
  action   TEXT NOT NULL,
  target   TEXT NOT NULL,
  success  INTEGER NOT NULL CHECK (success IN (0,1))
);
CREATE INDEX IF NOT EXISTS audit_ts ON audit(ts DESC, id DESC);
