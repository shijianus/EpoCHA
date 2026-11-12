# epocha 自托管人机验证服务 — 技术方案 v2

> 状态：设计基线（v2 已按 `docs/DECISIONS.md` 的 ADR-001…ADR-008 校正）。
> **本文是被修正后的结论；修订理由与原始错误表述只记录在 `docs/DECISIONS.md`，不要在本文里追加辩论。**
> 后续会话若与本文冲突，先改本文并在 §19 留一行索引，再改代码。
> 读者：后续会话的工程师。假设你完全没有上下文。
> 标注约定：`[目标]` = 期望值未实测；`[估算]` = 引自外部经验的推算；`[实测]` = 带测量条件与日期的数字。**禁止把前两者写成事实**。

## 1. 目标 / 非目标

**目标**
- 单二进制自托管人机验证服务，调用方式与 reCAPTCHA v2 语义兼容（`POST /siteverify`，字段 `secret` / `response` / `remoteip`）。
- 核心防线是**生成式题池**：题目由可插拔生成器程序化生成，每题随机参数 + 随机变换 + 一次性使用 + 用后即焚；不存在"下载来的固定题库"。
- 三层防线联合抬高攻击成本：PoW 成本门槛 → 生成式题目 → 风险打分决策。
- 多站点、多 Key、每站点独立难度/题型权重/TTL/域名白名单。
- 部署在 1 vCPU / 1GB / 50GB VPS，常驻 RSS `[目标]` <100MB。Cloudflare 边缘**本版不承载权威状态**（理由见 §12 与 ADR-001），且不在阶段 1 范围内。

**非目标（明确不做）**
- 不是 WAF/反爬虫平台，不做设备指纹画像，不做跨站点追踪，不采集个人数据（IP 只存加盐哈希截断值）。
- 不承诺"不可破解"。本设计只承诺"攻击成本可度量、可随题型轮换与难度调节而提高"（见 §4 威胁模型与 §18 残余风险）。
- 不做 reCAPTCHA 的 invisible/ score-only 模式，不实现 hCaptcha/Turnstile 的私有协议（只兼容 `siteverify` 的对外契约）。
- 不提供商业 SLA、不做多节点集群。**这是本项目的部署边界**：权威状态在单台机器的数据目录上，
  并由数据目录的运行锁保证同一时刻只有一个写者（§12）。SQLite 本身支持跨进程共享库文件与事务并发
  （一次性消费的保证见 ADR-004，与进程数无关），所以"单机"不是数据库能力上限的结论，
  而是运维形状与一致性权威的选择。本轮不开放多实例。

## 2. 关键决策摘要

| # | 决策 | 理由 / 代价 |
|---|---|---|
| D1 | Go 单体 + 纯 Go 依赖，禁止 cgo | 1GB 内存、静态编译、Docker 镜像小；cgo 交叉编译复杂 |
| D2 | SQLite（WAL）唯一持久化，禁止 Mongo/Postgres/JVM | 运维面最小。代价：单写者，吞吐 `[估算]` 数千 claim/s。**`SetMaxOpenConns(1)` 只是第一版规避 `SQLITE_BUSY` 抖动的简化，不是安全边界**——一次性保证见 D11/§5/ADR-004 |
| D3 | 驱动用 `modernc.org/sqlite` v1.46.1（纯 Go，BSD-3） | 无 cgo；代价：二进制体积上升、性能低于 cgo 版（本负载不敏感）。锁版本原因与工具链纪律见 §16 / ADR-007 |
| D4 | 三层防线，PoW 永远是第一层，题目不可单独作为防线 | 单独图片/文字题抗不住多模态模型与打码服务 `[估算]`（§4，具体数字待阶段 3 实测替换） |
| D5 | 每题分**公开 payload** 与**服务端私有 verifier 数据**；后者可摘要、可含参数 | 禁止下发答案与可推导答案的种子；**不强制离散精确匹配**，容差由题型自定（ADR-003，方案细节阶段 3） |
| D6 | 站点 `secret_key` 入库为带 pepper 的 HMAC 摘要，仅创建时明文展示一次 | DB 泄露不等于凭据泄露；代价：无法找回，只能轮换 |
| D7 | token 签名密钥用**带域分隔的 HMAC-SHA256 派生**（按 purpose+kid），站点密钥由主密钥派生 | 不落库每站点密钥、支持 kid 轮换；不引入 `x/crypto/hkdf`（等价性理由见 §6/ADR-007） |
| D8 | 阶段 1–5 的**权威验证节点是 VPS 上的 Go + SQLite**；CF Worker 延后，不属于阶段 1 | 工程选择而非平台限制：故障域与备份边界单一、写配额成本形状可控、降级语义（fail closed）更易证明。Cloudflare 免费计划**可以**使用 SQLite-backed Durable Objects，故本版删去"边缘不能持状态"的错误断言（ADR-001） |
| D9 | 管理 API 独立路径前缀 `/api/admin/*` + Bearer admin_key；管理台页面同源嵌入 | 与公开 API 分离便于反向代理层二次限流 |
| D10 | widget 原生 JS 零依赖，`go:embed` 打进二进制 | 无构建工具链，`<30KB gzip` 作为 `[目标]` 由 `make widget-size` 守护 |
| D11 | 一次性消费的正确性来自四条：原子条件状态转移、同一事务提交、检查影响行数、并发+重启重放测试（§5、§14-4） | 与具体 SQL 隔离级别/连接池形态解耦；阶段 1 已按此实现并有测试覆盖 |
| D12 | 安全关键状态**事务提交后才对外返回成功**；只有访问统计可异步 | 防重放账本不属于可异步类别（ADR-006） |
| D13 | 题目与 challenge **一对一绑定且永不回池**；池空时给 503 而**不降级成 PoW-only**（阶段 3A） | 代价：容量不足直接对调用方表现为"暂时不可用"（R12），且被作废 challenge 占用的题会沉没到题目 TTL 到期。换取的是"同一题面不会面对第二个挑战者"与"站点策略不会静默失效"（§5.4、ADR-011） |

## 3. 架构

```
                      ┌──────────────────────────────────────────┐
 浏览器 widget ──HTTP─▶│ Worker(可选/阶段5): 静态缓存 + 无状态 PoW 签发│──┐
                      └──────────────────────────────────────────┘  │ failback
 业务服务器 ───POST /siteverify──┐                                   ▼
                                 └──────────▶┌────────────────────────────────┐
                                              │ epocha 单二进制（VPS 主节点）  │
                                              │  api/ ─ router + 校验 + 限流   │
                                              │  pow/ ─ 难度签发与验证         │
                                              │  token/ ─ 域分隔派生+HMAC 签名  │
                                              │  risk/ ─ 频率/时延/行为打分     │
                                              │  generator/ ─ 题型插件         │
                                              │  pool/ ─ 题池水位与预生成      │
                                              │  store/ ─ SQLite(WAL)+迁移     │
                                              │  admin/ ─ 站点/Key/配置/统计   │
                                              └────────────────────────────────┘
```

**一次完整验证的生命周期**（标注：`[阶段1]` 已实现的最小闭环只有 PoW；题库与风控分别属阶段 3、阶段 3+）
1. `[阶段1]` `POST /api/challenge`：校验 site_key（`[阶段3]` 再加域名白名单）+ 限流 → 生成 crypto/rand 的 PoW seed → **落库 challenge 行（status=issued）** → 返回 PoW 参数与签名 token。`[阶段3]` 在此步追加"从池中原子领取一道题（不足水位则后台补）"。
2. 客户端做 PoW（SHA-256 前缀）`[阶段3]` 并解答题目 → `POST /api/redeem`。
3. `[阶段1]` `POST /api/redeem`：验签 token（用途、站点绑定、过期）→ 以**库中行为权威**校验 PoW 参数与解 → **原子条件消费 challenge 并在同一事务创建 response 行** → 事务提交后才签发短期 `response` token（TTL 120s `[目标]`）。`[阶段3]` 在消费前插入题型 verifier 比较（容差由题型自定），`[阶段3+]` 插入风险打分。
4. `[阶段1]` 业务服务器 `POST /siteverify {secret, response, remoteip}` → 先校验 secret（失败不得消费任何状态）→ 验签 + 站点一致性 → **原子置 used** → 事务提交后返回 reCAPTCHA 兼容 JSON。

失败处理策略（有限、明确）：token/PoW 无效不消费 challenge，但 `attempts+1`；累计 3 次失败该 challenge 直接置 `invalid` 作废（防无限试答案）。过期 challenge 与过期 response 一律按无效处理，不做宽限。响应丢失的客户端必须重新领 challenge——**先保证不能重复成功，再谈体验**。

任一层失败均返回**同一族错误码**（不透露具体哪层失败），避免给攻击者反馈信号。

## 4. 信任边界与威胁模型（必须诚实）

**信任边界**
- 客户端不可信：payload、行为数据、PoW 解全部视为攻击者可控输入；widget 上报的行为信号只作弱证据。
- 网络不可信：`CF-Connecting-IP` 仅在显式开启 `trust-proxy` 且源站防火墙只放行 CF IP 段时才可信，否则可被伪造。
- DB 泄露视为"部分泄露"：答案/verifier 数据以摘要或私有参数形式存储、站点密钥摘要存储，均依赖 §6 的 pepper 与主密钥**分离存放**；pepper 与 DB 同处一个备份则此假设不再成立。
- 管理 API 视为高危面：默认只监听 localhost + 需 Bearer key，反代侧再加一层 IP 限制。

**攻击者能力与对应缓解**（"能力假设"列除标注外均为 `[估算]`，来源是外部经验而非本专案测量；阶段 3 用自己的基线测量替换）

| 攻击 | 能力假设 | 缓解 | 残余风险 |
|---|---|---|---|
| 通用多模态模型直接答题 | `[估算]` 单次调用成本 ~$0.001–0.01；对扭曲字符准确率 60–90%（**未实测**，ADR-008） | 题面参数化（降低同题复用）+ 题目一次性 + 尝试次数上限 + PoW/频率层抬高每次尝试成本。**注意：随机字体/噪声/CSPRNG 只影响"题面随机空间"，不构成抗 AI 证据**（四指标见 §8.2） | 高难度题仍可能被解；安全性主张只能来自成本-尝试模型，不来自题面复杂度 |
| 打码服务人工/半自动破解 | `[估算]` 市场价约 $0.6–2 / 1000 次，延迟 5–60s | 题目 TTL 180s `[目标]`、redeem 时延信号、题面随机化使同一题不可复用 | 延迟容忍型攻击（慢速注册）有效但单位成本线性上升 |
| PoW 外包到 GPU/集群 | `[估算]` 单卡 10^10–10^11 SHA-256/s | PoW seed 与 challenge 一一绑定（不可摊销复用）；难度按风险动态提升；预留 memory-hard 协议位 | SHA-256 前缀对 GPU 友好，绝对成本上限有限（§7、§18-R1） |
| 令牌重放 / 一题多答 | 截获 token 或并发抢答 | §5/ADR-004 的四条保证（原子条件状态转移 + 同事务 + 影响行数 + 并发与重启重放测试）；response 单次使用 | 保证强度取决于测试是否覆盖真并发与重启，而非取决于连接池形态 |
| 答案枚举（选择题类） | 不解题，纯猜 | 按 §8.2 报告"接受域/答案空间"得到猜测通过率，并用尝试次数上限封顶 `1-(1-p)^n`；失败计入风险分 | 猜测通过率无法归零，业务侧必须把验证当作"提高成本"而非"绝对门" |
| 题池预生成 DoS（打空池） | 大量请求 /api/challenge 耗尽池 `[阶段3]` | 池有上限与水位、按 IP+site 限流、池被攻击性消耗时降级为同步生成并返回 429 | 1 vCPU 上限即吞吐上限，边缘可分担一部分 |
| DB 读泄露 | 只读 SQL 注入/备份泄露 | §5 全参数化查询、§6 摘要存储、pepper 独立于 DB 文件 | 活跃题目在其 TTL 内可被离线暴力猜测（答案空间小的题型） |
| 源站 IP 暴露后绕过边缘 | 证书透明/历史 DNS 泄露 | 源站防火墙只放行 CF 段；`/api/*` 与 `/api/admin/*` 分离限流 | 需运维正确配置，文档给出检查清单 |

**明确写下的结论**：本系统的安全性来自"每次尝试成本 + 一次性 + 轮换 + 决策层"，不来自"题目难"。任何单一题型被攻破是预期事件，由 §9/ADR-006 的**分级**降权与告警处理（不实现"仅凭通过率自动下线"）。

## 5. 数据模型（SQLite）

### 5.1 阶段 1 实际实现（最小防重放账本）

DSN（**实测**：`PRAGMA` 回读为 `wal / 5000 / 1 / 1`，见 ADR-007）：
`file:<path>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)`

`db.SetMaxOpenConns(1)` 是当前选择的简化实现（规避 `SQLITE_BUSY` 抖动），**不是安全保证的一部分**；
一次性保证来自下面四条，并且必须被测试证明（ADR-004）：

1. **原子条件状态转移**：检查旧状态与写入新状态在同一条 SQL 内完成。
2. **同一事务提交**：一个业务动作的多写放在同一 `Tx`；由 `db.BeginTx` 让驱动发起 `BEGIN`，**禁止在 `*sql.Tx` 上再执行 `BEGIN IMMEDIATE`**（嵌套事务错误）。
3. **检查影响行数**：`RowsAffected != 1` 即失败并回滚。
4. **测试证明**：同池 N 路并发仅一个成功；**两个独立 `*sql.DB` 句柄争抢同一临时文件**仍只有一个成功；**进程重启后**重放已消费 token 仍失败。

```sql
-- migrations/0001_stage1.sql（阶段 1 实现；只增不减，幂等）
CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);           -- schema_version 等
CREATE TABLE sites (
  id        INTEGER PRIMARY KEY,
  name      TEXT NOT NULL UNIQUE,
  site_key  TEXT NOT NULL UNIQUE,                                  -- 公开标识
  secret_mac TEXT NOT NULL UNIQUE,                                 -- HMAC(pepper, secret)，明文不落库
  pow_bits  INTEGER NOT NULL CHECK (pow_bits BETWEEN 12 AND 24),
  created_at TEXT NOT NULL
);
CREATE TABLE challenges (
  id         TEXT PRIMARY KEY,                                     -- challenge id（token 的 cid）
  site_id    INTEGER NOT NULL REFERENCES sites(id),
  pow_seed   BLOB NOT NULL,
  pow_bits   INTEGER NOT NULL CHECK (pow_bits BETWEEN 12 AND 24),
  status     TEXT NOT NULL CHECK (status IN ('issued','redeemed','invalid')),
  attempts   INTEGER NOT NULL DEFAULT 0,                           -- 失败尝试计数，3 次作废
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  redeemed_at TEXT                                                 -- 仅 redeemed 时非空
);
CREATE INDEX challenges_site_status ON challenges(site_id, status, expires_at);
CREATE TABLE responses (
  id           TEXT PRIMARY KEY,                                   -- response id（token 的 rid）
  challenge_id TEXT NOT NULL UNIQUE REFERENCES challenges(id),      -- 一个 challenge 最多签发一个 response
  site_id      INTEGER NOT NULL REFERENCES sites(id),
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  used         INTEGER NOT NULL DEFAULT 0 CHECK (used IN (0,1))
);
```
阶段 1 无 `sessions`/`pool`/`gen_stats` 表：PoW 参数就存在 `challenges` 行里，状态转移 `issued → redeemed | invalid`，
过期由时间戳判定（不做状态写回）。防重放只依赖这张账本，**不依赖内存**——重启后语义不变（有测试）。

### 5.2 目标 schema（阶段 2/3 扩展，尚未实现）

阶段 2 追加：域名白名单、限流与 TTL 的站点级配置、`admin_key` 摘要与 `audit`。
阶段 3 追加：`pool`（题池）、`generator_config`、`gen_stats`、`sessions`（链路记录）。
**其中 `pool` 已在 3A 定稿并实现，实际列名与状态集见 §5.4（`ready|assigned|terminal`，不是下面草稿的
`ready|claimed|consumed|expired`）**；`generator_config`/`gen_stats`/`sessions` 仍未实现。
`pool` 的判分列按 ADR-003 命名与定义为 **`verifier_blob`（服务端私有 verifier 数据：摘要、参数或私有结构）**，
而不是强制 `answer_mac`；是否可用摘要存储由每个题型在阶段 3 单独论证。

```sql
-- 以下为 §5.2 目标形态（草稿，非当前实现；列名与约束在阶段 3 定稿前不具约束力）
CREATE TABLE site_domains (site_id INTEGER NOT NULL REFERENCES sites(id), host TEXT NOT NULL, PRIMARY KEY(site_id, host));
CREATE TABLE generator_config (
  site_id INTEGER NOT NULL REFERENCES sites(id),
  generator_id TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  weight INTEGER NOT NULL DEFAULT 100,          -- 0..1000，含降权结果
  difficulty INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY(site_id, generator_id)
);
CREATE TABLE pool (
  id INTEGER PRIMARY KEY,
  generator_id TEXT NOT NULL,
  difficulty INTEGER NOT NULL,
  status TEXT NOT NULL,                         -- ready|claimed|consumed|expired
  payload_json TEXT NOT NULL,                   -- 公开下发部分，不得含答案或可推导种子（ADR-003）
  asset_blob BLOB,                              -- 图片二进制，可空
  verifier_blob BLOB NOT NULL,                  -- 服务端私有 verifier 数据，永不下发
  created_at TEXT NOT NULL,
  claimed_at TEXT, expires_at TEXT NOT NULL,
  solve_ms_p50_hint INTEGER
);
CREATE INDEX pool_claim ON pool(status, generator_id, difficulty, expires_at);
CREATE TABLE sessions (                          -- 一次 challenge→redeem→verify 的链路（统计用）
  nonce TEXT PRIMARY KEY,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  pool_id INTEGER,
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
  ip_mac TEXT,                                   -- HMAC(pepper, ip掩码+日) — 隐私
  pow_bits INTEGER NOT NULL, pow_solved INTEGER NOT NULL DEFAULT 0,
  redeem_at TEXT, risk_score REAL, verdict TEXT,
  response_id TEXT,
  used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE gen_stats (                          -- 降权/告警的输入（分级，见 §9 与 ADR-006）
  day TEXT NOT NULL, generator_id TEXT NOT NULL, site_id INTEGER NOT NULL,
  shown INTEGER NOT NULL DEFAULT 0, passed INTEGER NOT NULL DEFAULT 0,
  failed INTEGER NOT NULL DEFAULT 0, suspect INTEGER NOT NULL DEFAULT 0,
  solve_ms_sum INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(day, generator_id, site_id)
);
CREATE TABLE audit (ts TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL, detail TEXT);
```

清理任务（阶段 3 起）：`consumed`/过期 pool 行**删除**（用后即焚），过期 sessions/responses 删除，`gen_stats` 保留 90 天。硬约束：pool 表行数上限（默认 20000 `[目标]`）与磁盘占用告警。

**查询安全**：一律 `db.Query(sql, args...)` 参数化；动态列名/排序（统计接口）必须白名单映射，禁止字符串拼接。

### 5.3 阶段 2 实际实现（多站点、密钥生命周期、来源策略、审计）

迁移是**增量**的（`0002_stage2.sql` + Go 侧数据步骤），阶段 1 的库升级后账本数据全部保留；
实测（本机 modernc 内置 SQLite 3.51.2）**删不掉带 UNIQUE 约束的列**（`cannot drop UNIQUE column`），
所以 `sites.secret_mac` 留在 schema 里但被**中和**成 `HMAC(pepper, "epocha/v1|retired|" + 旧摘要)`——
任何真实 secret 都匹配不上它，凭据的权威位置变成 `site_secrets`。有测试同时钉住"旧列不可用于认证"
与"旧 secret 升级后仍可用"两件事。

```sql
-- 0002_stage2.sql（阶段 2 实现）
ALTER TABLE sites ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1));
ALTER TABLE sites ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 2147483647);
ALTER TABLE sites ADD COLUMN challenge_ttl_sec INTEGER NOT NULL DEFAULT 180 CHECK (challenge_ttl_sec BETWEEN 1 AND 3600);
ALTER TABLE sites ADD COLUMN response_ttl_sec INTEGER NOT NULL DEFAULT 120 CHECK (response_ttl_sec BETWEEN 1 AND 3600);
ALTER TABLE sites ADD COLUMN rate_ip_rpm INTEGER NOT NULL DEFAULT 60 CHECK (rate_ip_rpm BETWEEN 1 AND 100000);
ALTER TABLE sites ADD COLUMN rate_site_rpm INTEGER NOT NULL DEFAULT 300 CHECK (rate_site_rpm BETWEEN 1 AND 100000);
ALTER TABLE sites ADD COLUMN allow_missing_origin INTEGER NOT NULL DEFAULT 0 CHECK (allow_missing_origin IN (0,1));
ALTER TABLE challenges ADD COLUMN origin TEXT NOT NULL DEFAULT '';   -- 签发时绑定的规范化来源
CREATE TABLE site_secrets (key_id TEXT PRIMARY KEY, site_id INTEGER NOT NULL REFERENCES sites(id),
  secret_mac TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL, revoked_at TEXT);
CREATE TABLE site_origins (site_id INTEGER NOT NULL REFERENCES sites(id), origin TEXT NOT NULL,
  created_at TEXT NOT NULL, PRIMARY KEY(site_id, origin));
CREATE TABLE admin_keys (key_id TEXT PRIMARY KEY, label TEXT NOT NULL UNIQUE, key_mac TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL, revoked_at TEXT);
CREATE TABLE audit (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, actor TEXT NOT NULL,
  action TEXT NOT NULL, target TEXT NOT NULL, success INTEGER NOT NULL CHECK (success IN (0,1)));
```

三个概念必须分开，否则"轮换密钥"会被误写成"换站点"：稳定的内部 `id`（账本外键指向它，永不复用）、
公开的 `site_key`、以及可多把并存的验证凭据（`site_secrets`，公开 `key_id` 与随机 secret 彼此独立）。
**撤销用时间戳表达**：`revoked_at IS NULL OR revoked_at > now` 即有效。
"立即撤销"= 写入当前时刻，"过渡期"= 写入 `now + grace`（上限 86400 秒）。判定只读库，不需要后台任务，重启后同样成立。

**站点版本 `version` 是签名覆盖的一部分**（token claims 的 `svid`，见 §6）。启停站点各提升一次版本，
于是"停用前签发的未消费 token 不会因为重新启用而复活"这条性质由签名保证，而不是依赖库里恰好还留着那行状态。
重复启停同一状态不再提升（启停是状态动作，不是计数器）。

**配置生效语义分两类**（阶段 2 定稿，测试逐条覆盖）：
- *参数类*改动（`pow_bits`、两个 TTL、两个 rpm）沿用**签发快照**：已签发的 challenge 仍按它入库时的难度与
  过期时间判定，因为权威是账本行 + 签名，不是"当前站点配置"。这类改动**不**提升 `version`。
- *安全类*动作（停用站点、提升版本、撤销凭据、来源白名单）**立即生效**，不取快照、不缓存：
  第一版没有分布式缓存，也不引入；`site_secrets`/`sites.enabled` 每次请求都读库。
  撤销不回滚：一旦写入 `revoked_at`，重新启用站点或重启都不会让它复活。

**清理任务的边界**（阶段 1 复核后修正）：challenge 行只在**没有任何 response 行再引用它**时删除
（`responses.challenge_id` 有外键，而被兑换过但尚未验证的 response 可以活过所属 challenge）；
每批 500 行、两条删除同事务。`DELETE` 只归还空闲页，**文件不会自动缩小**（有测试记录实测字节数），
收缩需要 `VACUUM`，本版不做。审计行保留 90 天后按批清理。

**`hostname` 的来源与保证范围**（阶段 2 起，取代 §10.1 里"仅回显自报值"的旧语义）：
`/api/challenge` 把请求的规范化 Origin 写进 `challenges.origin` 并被签名绑定；
`/siteverify` 返回的 `hostname` **取自这一列**，调用方自报的 `hostname` 一律不采信也不回显；
无来源（站点放行直接调用）时该字段整体缺省。
保证范围必须说清楚：它表示"这个 challenge 在这个允许来源上签发"。Origin 头由浏览器强制设置、页面脚本改不了，
但**非浏览器客户端可以任意伪造**，所以它不是机器人身份证明，也没有任何硬件级/浏览器证明/可信远程证明的含义；
只有签名绑定还不够——被绑定的数据本身对浏览器来源可信，对手工构造的请求不可信。

### 5.4 阶段 3A 实际实现（题池、题目 ↔ challenge 绑定、单次作答终结）

迁移同样是**增量**的（`0003_stage3a.sql`）：阶段 1/2 的站点与账本数据全部保留，
旧站点迁移后是 `verify_mode='pow_only'`（列默认值），**现有业务行为不变**——`pow_question` 只能经管理 API 显式开启。

```sql
-- 0003_stage3a.sql
ALTER TABLE sites      ADD COLUMN verify_mode TEXT NOT NULL DEFAULT 'pow_only'
  CHECK (verify_mode IN ('pow_only','pow_question'));
ALTER TABLE sites      ADD COLUMN generator_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sites      ADD COLUMN difficulty INTEGER NOT NULL DEFAULT 1 CHECK (difficulty BETWEEN 1 AND 5);
ALTER TABLE sites      ADD COLUMN generator_pool TEXT NOT NULL DEFAULT ''; -- 0006：JSON 候选池（ADR-026）
ALTER TABLE challenges ADD COLUMN mode TEXT NOT NULL DEFAULT 'pow_only';
ALTER TABLE challenges ADD COLUMN generator_id TEXT NOT NULL DEFAULT '';
CREATE TABLE pool (
  id TEXT PRIMARY KEY, generator_id TEXT NOT NULL, version TEXT NOT NULL,
  difficulty INTEGER NOT NULL CHECK (difficulty BETWEEN 1 AND 5),
  status TEXT NOT NULL CHECK (status IN ('ready','assigned','terminal')),
  payload_json TEXT NOT NULL CHECK (length(payload_json) <= 8192),   -- 公开题面（ADR-028）
  verifier_blob BLOB NOT NULL,                                      -- 私有判分数据，永不下发
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
  challenge_id TEXT REFERENCES challenges(id), assigned_at TEXT, terminal_at TEXT);
CREATE UNIQUE INDEX pool_bound_challenge ON pool(challenge_id) WHERE challenge_id IS NOT NULL;
CREATE INDEX pool_claim  ON pool(generator_id, version, difficulty, status, expires_at);
CREATE INDEX pool_expire ON pool(status, expires_at);
```

**四个概念必须分开，不能塞进同一个可序列化结构体**（红线 5、ADR-003）：
- `generator.PublicPayload`：**具名公开结构体的接口**（不是 `map[string]any`、不是 `json:"-"`）。
  `ArithPublic` 只有 `task`/`answer_type`/`max_answer_len` 三个字段——答案、操作数、内部随机数**根本不在这个类型里**，
  所以"漏抑制"这类错误在结构上不可能发生。出网走 `SanitizePublic`：先解成白名单键集合，未知键直接报错。
- `generator.PrivateVerifier`（`[]byte`）：服务端私有判分数据，只在 `pool.verifier_blob` 与判分路径上存在。
- `store.PoolQuestion`：持久化题池记录（含 `Status`/`ChallengeID`/三个时间戳），是上面两者的容器加上状态机字段。
- 绑定后的挑战状态：`challenges.mode/generator_id`（签发快照）+ `pool.challenge_id`（唯一绑定）+ `challenges.status`。

**绑定关系只存一处**：`pool.challenge_id`。`challenges` **不**反向存 `pool_id`——两处都存就要回答"谁是真的"，
而且清理时两条外键互相咬。于是"一题一 challenge"由 partial UNIQUE 索引（`WHERE challenge_id IS NOT NULL`，
`ready` 行的 NULL 不参与唯一性）加上"条件更新 + 影响行数"共同保证。

**题池状态机 `ready → assigned → terminal` 是单向的**，五条不变量与实现手段（逐条有测试，见 §17 阶段 3A）：

| # | 不变量 | 实现手段 |
|---|---|---|
| 1 | 一条题只绑一个 challenge | partial UNIQUE 索引 + `UPDATE … WHERE status='ready' AND expires_at>?` 判 `RowsAffected==1` |
| 2 | `assigned` 永不回 `ready`（断线/超时/答错都不回池） | 只有 `InsertQuestion` 写 `ready`；没有任何把状态改回 `ready` 的代码路径 |
| 3 | 分配与 challenge 创建同事务 | `IssueChallengeWithQuestion` 在一个 `BeginTx` 内插 challenge 行 + 选题 + 条件更新 + 回读题面 |
| 4 | 慢操作不进写事务 | 生成（抽熵、构造表达式、序列化）全在 `pool` worker 的事务外完成，事务里只有 SQL |
| 5 | 池空 ⇒ 文档化的可重试错误，**不静默降级成 PoW-only** | `store.ErrPoolEmpty` → `503 challenge-unavailable + Retry-After: 2` |
| 6 | 重启后 `assigned`/`terminal` 行不再作为可领取题发出 | 领取条件要求 `status='ready'`，状态在库里而非内存里（有重启测试） |
| 7 | 容量共同限流 | 池有全局行数硬上限（`INSERT … SELECT … WHERE (SELECT count(*))<max` 同语句判定）；challenge 存量沿用阶段 1 的 `Caps` |

"记录不复用"不等于"题面永不重复"：算术题的答案空间小，**没有**为它加"重复题面就重抽"的无限重试循环——
那只会把失败模式变成忙循环，且不改变安全性（见 §8.4）。

**领题竞争的两种形态（本轮实测，别把其中一种当成另一种）**：同一进程内 `SetMaxOpenConns(1)` 让事务在这唯一的
连接上串行，"条件更新影响 0 行"这一支实际走不到；真正会发生的是**跨句柄**（多进程共享同一库文件，不是部署形态）：
后写的一方因为读快照已过期，`UPDATE` 直接拿到 `database is locked`，于是**整笔事务回滚**——challenge 行不留、
别人的绑定不被改写。`claimAttempts=5` 的存在只是给循环封顶，**不是**正确性依赖；正确性依赖"同一事务内的条件更新
+ 影响行数判定 + partial UNIQUE 索引"。两个方向都有测试（`TestOneQuestionPerChallengeTwoHandles` 断言
"绑定行数 == distinct challenge_id == 成功次数"，`TestStaleSnapshotClaimFailsClosed` 断言过期快照那一笔不得改写绑定）。

**`verifier_blob` 的形态**（阶段 3A 只做可离散化的题型，因此可以走摘要）：
`verifier_blob = nonce(16B) || HMAC(K(qver, kid), "epocha/v1|qver|" || nonce || 0x00 || 规范化答案)`。
`K(qver)` 由 §6 的域分隔派生按 `purpose="qver"` 得到，与 `chal`/`resp` 两把密钥互不可替代。
答案明文不落库、不进日志、不进响应（红线 5、11）；判分是"对候选答案做同一条 HMAC 再常数时间比"，
所以**没有**任何接口能把已存答案读出来。`nonce` 让同一答案在不同题目行里的摘要不同，避免"摘要相同 ⇒ 题面相同"的旁路观察。

**2026-11-05 的第二种形态（ADR-024）**：`text-image-v3` 用逐字标签
`nonce(16B) || tag_0 || … || tag_{L-1}`（每标签 32B，L=6 ⇒ 208B），以支持"最多 1 个字符替换"的容差判分。
它只用于 v3；`arithmetic-baseline-v1`、`text-image-v1/v2`、`shape-grid-v1`、`shape-count-v1`
仍使用上面的 48B 精确构造。两种形态都满足"答案不落库明文"，逐字构造的额外边界见 §8.7.3 与 ADR-024。

**清理顺序**（`Purge`，响应 `PurgeCounts`）：`responses` → `pool` → `challenges`。
必须先删 response 行再删 pool 行、最后删 challenge 行，否则外键把清理永久卡死（阶段 1 复核就是这个 bug，ADR-009）。
只删：过期且已 `terminal` 的题、过期且未绑定的 `ready` 题、以及**没有任何 response 引用**的过期终态 challenge。
仍绑定着有效 challenge 的 `assigned` 行不删——删了就等于"题目被静默回收"，而 challenge 与签名还活着。

**第一笔有效作答即终结该 challenge**（`RedeemAttempt`，取代阶段 1 的 `Redeem`）：
校验顺序是 token 签名 → 站点/来源 → 请求结构与答案文法 → PoW → **终结性作答**。
只有前四步全过才叫"一次有效答题尝试"；畸形请求与错误 PoW **不**消耗题目（它们连事务都不进入判分分支）。
判分结果在同一事务里落终态：答对 ⇒ `challenges.status='redeemed'` + `responses` 行 + `pool.status='terminal'`；
答错 ⇒ `challenges.status='invalid'` + `pool.status='terminal'`，并**提交**、返回 `OutcomeWrongAnswer` 而非 error。
把"答案错"实现成返回 error 会让整笔事务回滚，题目回到 `ready`、challenge 回到可重试状态 ⇒ 无限重试同一道题。
这个失败模式有专门的故障注入回归（§17）。

### 5.5 阶段 3B 实际实现（图片资产、字节预算、判分与资产解耦）

迁移仍是**增量**的（`0004_stage3b.sql`）：阶段 1/2/3A 的数据全部保留，旧行 `asset_blob` 为 NULL、
`asset_mime` 为 `''`，因此算术题与 `pow_only` 的行为完全不变（有测试用一个只跑过 `0001..0003` 的库验证）。

```sql
-- 0004_stage3b.sql
ALTER TABLE pool ADD COLUMN asset_blob BLOB  CHECK (asset_blob IS NULL OR length(asset_blob) <= 65536);
ALTER TABLE pool ADD COLUMN asset_mime TEXT NOT NULL DEFAULT '' CHECK (asset_mime IN ('', 'image/png'));
```

四条纪律，每条都有测试：

1. **两条资源边界各自独立、都在写入的同一条 SQL 内判定**：`pool` 的行数上限（3A 已有）与
   **全局图片字节预算**（`(SELECT coalesce(sum(length(asset_blob)),0) FROM pool) + length(?) <= ?`）。
   只看条数等于没看——一条图片题的字节可以是算术题的上百倍（实测 6KB vs 63B）。
   预算为 0 的含义是"禁止任何图片资产"，不是"不限"（与 ADR-009 给 TTL 上界同一理由）。
2. **统计绝不把 BLOB 读进 Go**：字节数一律用 SQLite 的 `sum(length(asset_blob))` 聚合，
   跨进程边界的只有一个整数。`PoolStats` 因此多了 `ready_asset_bytes` / `total_asset_bytes`。
3. **只有领取路径读资产**：`readQuestionTx`（按 id 精确读那一行）是资产进入 Go 内存的唯一位置；
   判分走 `QuestionVerifierForChallenge`，只 SELECT `verifier_blob`。于是"图片被清理或损坏"
   在结构上不可能影响验证——测试把 `asset_blob` 置为 NULL 后仍兑换成功来钉这条。
4. **资产与题目同一行、同生同死**：`Purge` 删 `pool` 行时 PNG 一起消失，不存在孤儿文件，
   也不需要路径清理或对象存储。代价是库体积，所以第 1 条是硬要求而不是优化项。

MIME 存进库（而不是由生成器当前代码推断）是为了让资产**自描述**且已签发题目不会因为后来改代码而换类型；
CHECK 把可选值封成一项，浏览器就没有 `data:<任意 MIME>` 的空间。资产与 MIME 必须成对出现，
这条在 Go 侧拒绝（库侧两条 CHECK 各自限取值，跨列约束 `ADD COLUMN` 写不了）。

## 6. 密钥体系与 token

**主密钥**：`EPOCHA_MASTER_KEY`（32B，base64）；**pepper**：`EPOCHA_PEPPER`（32B，与 DB 文件分离存放，独立挂载）。两者缺一服务拒绝启动（不静默降级）。
**阶段 5A 的实现方式更正**：本文原先写的 `--master-key-file` 命令行入口从未实现；真实入口是**同名变量加 `_FILE` 后缀**
（`EPOCHA_MASTER_KEY_FILE` / `EPOCHA_PEPPER_FILE` / `EPOCHA_MASTER_KEY_PREVIOUS_FILE` / `EPOCHA_ADMIN_KEY_FILE`）。
理由是容器里 env 的泄露面：`ps`、`docker inspect`、编排平台的 KV 与 CI 日志都会原样带出环境变量，
而 bind mount / secret 文件不会。规则（`internal/config/keyfile.go`，有测试）：
文件必须存在、解析后的目标是普通文件、**group/other 权限位必须为空**（0600/0400）、非空且 ≤4096B（达到上限即失败，不截断）；
允许符号链接（secret 挂载本身就是指向 `..data/<uuid>` 的链接，为原子轮换设计）；
**同一个入口的 env 与 `_FILE` 同时非空 ⇒ 拒绝启动**，不做优先级——"文件优先"或"env 优先"都会制造
"运维改的那一份不生效"这类极难定位的故障，而密钥配错的失败面是签名与摘要。
文件内容与密钥值不进错误文案、启动摘要与日志；摘要只记录每把密钥的**来源**（`env`/`file`/`none`）。
`cmd/ops` 与 `cmd/server` 共用同一套解析规则（`config.LoadPepper`/`LoadAdminKey`），避免出现两套真相。

派生（**带域分隔的 HMAC-SHA256**，标准库即可；不引入 `x/crypto/hkdf`，理由见 ADR-007——本设计每个用途只需要一把固定长度密钥，
单次 HMAC 展开在此场景与 HKDF-expand 等价，且域分隔串含版本与 purpose，无跨用途复用风险）：
```
kid     = hex(HMAC-SHA256(master, "epocha/v1|kid")[:8])
K(purpose, kid) = HMAC-SHA256(master, "epocha/v1|" + purpose + "|" + kid)     // purpose ∈ {chal, resp, qver}
```
`qver` 是阶段 3A 新增的第三把派生密钥，只用于题目判分摘要（§5.4）：`token.DerivePurpose(master, "qver")`
走的是与 token 内部完全同一个 `deriveKey(master, purpose, Kid(master))`，因此它既不是签 token 的密钥、也不能反过来签 token。
`TestDerivedPurposeKeysAreDistinct` 逐对比对三把密钥：长度 32、两两不同、换主密钥结果改变、短主密钥与空 purpose 返回 nil。
**token 线格式**：`v1.<kid>.<b64url(payload_json)>.<b64url(tag)>`，`tag = HMAC(K(purpose,kid), "epocha:v1|" + purpose + "|" + b64url(payload))`。
签名覆盖 payload 的**原始 base64url 字节**（不重新序列化），因此难度、TTL、站点、challenge id 全部不可被客户端改动；
PoW 参数以 **DB 行为权威**再校验一次（纵深防御：即使密钥泄露导致伪造 token，参数与站点仍受库内记录约束）。

payload 字段（阶段 1 实现）：`typ`(`chal`|`resp`)、`kid`、`sid`(站点 id，硬绑定)、`cid`(challenge id)、
`rid`(仅 resp，response id)、`iat`、`exp`(Unix 秒)；`chal` 额外带 `bits`、`seed`(base64url)。未知字段一律拒绝。
**阶段 2 追加 `svid`**（站点版本，§5.3）；**阶段 3A 追加 `mode` 与 `gen`**：
`mode` 必填且只能是 `pow_only`/`pow_question`，`pow_question` 必须带非空 `gen`、`pow_only` 必须不带——
四种非法组合在**签名阶段**就拒绝（`ErrUnusableKey`），不等到消费时。这样一张 `pow_question` 的 token
无法被当作 `pow_only` 使用（少一层判定即验签失败），生成器也不能被客户端替换。
`mode`/`gen` 同时落进 `challenges` 行并被交叉核对：**签名说一套、库行说另一套时按失败处理**。
实现取 **token 总长 ≤4096B、payload ≤2048B**（比本文原先写的"payload ≤4KB"更严，复核后按取严者固化）。
验证顺序：总长度上限 → 分段解析 → kid ∈ {current, previous} → **常数时间比 tag** → `DisallowUnknownFields` 解 payload →
`typ`/purpose 匹配 → `exp`/`iat` 时钟窗口 → `sid` 与请求站点一致 → 业务层原子消费。任何一步失败对外都是同一族错误码，不区分原因。
轮换：`kid` 允许同时存在 current + previous（`EPOCHA_MASTER_KEY_PREVIOUS`，阶段 5A 起同样有 `_FILE` 入口），
旧 token 在 TTL 内自然过期。**恢复旧快照时不得设置 previous**——那把签名回退正是"旧 token 重新可用"的唯一通道（§12.4、ADR-014）。
**题目 verifier 不参与这套双密钥**：`K(qver)` 只由**当前**主密钥派生，所以轮换主密钥后，题池里存量题的摘要
在新密钥下**必然判假**——即使提交正确答案（`TestVerifierFailsClosedAfterKeyChange` 钉住这条 fail-closed 语义）。
后果与运维程序：轮换后这些题会消耗掉用户的作答机会（答案被拒 ⇒ challenge 终态作废），直到它们被清理或过期。
**因此轮换主密钥的操作程序里必须包含"清空或等空题池"**（停 worker → 等存量题过期，或直接删 `pool` 行 → 起服务）。
本版不为此实现自动失效（那需要"给 verifier 盖密钥指纹"这类新增结构），如实记录为运维约束而不是隐藏它。

`secret_key` 校验：`want = HMAC(pepper, provided_secret)`，`subtle.ConstantTimeCompare(want, site.secret_mac)`。
遍历该表全部行、不因命中而提前返回，避免用响应时序定位"命中第几条记录"。
**阶段 2 保持站点 secret 的摘要算法不变**（不加域分隔串）：`0002` 迁移要把 `sites.secret_mac` 原样搬进
`site_secrets`，改算法会让升级后的旧密钥全部失效（ADR-010）。管理凭据是新东西，因此单独域分隔：
`admin_mac = HMAC(pepper, sha256hex("epocha/v1|admin|" + key))`，所以同一个明文当作站点 secret 和当作管理凭据
时摘要不同，两类用途不可互换。
生成的 secret 形如 `sk_<64 hex>`（256 bit 来自 `crypto/rand`），管理凭据形如 `ak_<64 hex>`；
`sk_`/`ak_` 只是可读标识，熵全部来自随机部分，公开 `key_id` 形如 `k<12 hex>` 与 secret 彼此独立。

**管理密钥引导**（阶段 2 实现）：库中**没有任何 `admin_keys` 记录**时，用 `EPOCHA_ADMIN_KEY` 登记其摘要（label=`bootstrap`）；
有记录后该环境变量**完全忽略**——否则运维在管理 API 里的撤销会被下一次重启推翻。
**阶段 5A 补上一条这条规则的必然后果**：撤销掉最后一把凭据之后，"库里已有记录"仍成立，因此换引导文件（env 或 `_FILE`）
**不会**恢复访问；而交付的镜像里没有 `sqlite3` 可以手工改库。唯一的受控出口是
`epocha-ops admin-recover`（停服 + 探活拒绝 + 一条事务内撤销全部有效凭据并登记新凭据 + 审计；
新凭据只写进 `--key-file` 的 0600 文件，stdout 只给 `key_id`）。仍然没有任何未鉴权的恢复 HTTP 面。
运维纪律由此反推：**撤销管理凭据前，先新增一把并验证可用**（ADR-014 第 5 条）。
`EPOCHA_ADMIN_KEY` 的值与运维实际发出的 Bearer 值**逐字节相同**（不做 base64 变换），
否则"登记的形式"与"发送的形式"会变成两套真相。库里没有可用管理凭据时管理 API 一律 401：**缺凭据不等于放开管理功能**。
日志红线：禁止输出 `secret`、`response`/`token` 全文、`answer`、payload 完整 JSON、未掩码 IP。允许输出：nonce 前 8 字符、site_key、耗时、错误码。

**密钥材料的存放（硬约束）**：`EPOCHA_MASTER_KEY` / `EPOCHA_PEPPER` / 站点 `secret_key` / `admin_key` **一律不进仓库**，即使仓库是 private。
三条理由：(1) git 历史不可撤销，泄露后只能轮换全部密钥；(2) private 的访问面不等于零——任何带 repo 范围的令牌、CI 产物、镜像层、未来的可见性切换或开源导出都会带出历史；
(3) 本设计的安全前提是 **pepper 与 DB 分离存放**（§5 的答案摘要、§6 的密钥摘要都依赖它）。把两者放进同一个仓库/同一次备份 = 摘要保护归零。
存放方式：`/etc/epocha/env`（`0600`，属主为服务账号）或 systemd `EnvironmentFile`/密钥管理，与 `/var/lib/epocha/*.db` 分目录、分备份策略。
仓库里只放 `.env.example`（占位符）。题库、语料、字体、图片素材属非机密资产，可入库（R3 许可另议，>100MB 改用 LFS 或派生 seed）。

## 7. PoW 层（阶段 1 实现）

- 协议 `sha256-prefix`：服务端给 `seed`（16B，`crypto/rand`，每个 challenge 独立）与 `bits`；客户端找 `nonce_c` 使
  `SHA256(seed || nonce_c_ascii)` 前 `bits` 位为 0。服务端验证 = 1 次哈希。
- **哈希输入必须含该 challenge 的不可预测上下文**（`seed` 即承担此角色），因此解无法跨 challenge 复用；`bits` 与 `seed` 同时被
  token 签名覆盖并落库，客户端改难度/TTL 会直接验签失败，参数再以库中行为权威复核（§6）。
- 客户端提交的 `nonce_c` 格式约束：ASCII 十进制数字，长度 1–16，值 ≤1e12；不符即 `invalid-input-response`（不做任何解析式宽容）。
- 难度语义：期望尝试次数 = 2^bits，允许范围 **12..24**（DB CHECK + 配置校验）。默认 `bits=18 [估算]`。
  "浏览器 300–900ms"、"WebCrypto 3–8 万 digest/s"、"memory-hard 验证 30–50ms → 20–30/s" 全部是 `[估算]`，
  **阶段 1 未在本机或任何目标机测量过**；阶段 4/5 用 `[实测]` 数字替换后才可作为选型依据（ADR-006）。
- 时延证据（`redeem_at - created_at`）在阶段 3+ 才进入风险打分；阶段 1 只记录、不判定，因为阈值本身还没被测过。
- **预留 memory-hard 协议位**（v2 或阶段 5 之后）：`scrypt/argon2id` 在 WASM 内计算。代价是服务端验证同样变贵（每次需重算），
  在 1 vCPU 上可能把验证吞吐压到两位数 `[估算]`，因此默认不启用，且启用前必须先 `[实测]`。
- 客户端实现选择（阶段 4 定）：`crypto.subtle.digest` 分批或内嵌 WASM SHA-256；widget 上报 `pow_impl` 供难度校准。

## 8. 生成式题池

### 8.1 Generator 接口（阶段 3 落地）

> **3A 更新**：本节是阶段 0 的草稿，`Challenge`/`Verifier` 的最终形状、`version` 与 `SanitizePublic` 等
> 与实现有差别的地方以 **§8.4** 为准；保留原文是为了不重写整份设计、并留下"接口是怎么被实际约束改出来的"。
> 下面两处草稿表述已失效：`internal/generator` 不再是契约占位（已实现 `arithmetic-baseline-v1`）；
> "注册表 `generator.Register(g)`"改为**实例注册表** `generator.NewRegistry()`（按 purpose 派生的密钥要先注入，全局注册表做不到）。

```go
package generator

// Generator 是单个题型的生成与校验器。注册表 generator.Register(g) 在阶段 3 实现。
type Generator interface {
    ID() string                       // "arith-v1"、"text-warp-v1"、"cjk-v1"、"rotate-v1"
    Media() string                    // "text" | "image"
    // entropy 由调用方提供，是 crypto/rand 流；每次调用独立、用完即弃。
    Generate(ctx context.Context, p Params, entropy io.Reader) (Challenge, error)
    Verify(ans Answer, in string) bool // in 已按协议截断到上限；内部用常数时间比较
}

type Params struct { Difficulty int; Lang string }
type Challenge struct {
    Payload  any    // 公开下发部分（JSON）：不得含答案、可推导答案的种子、判分秘密材料
    Verifier Verifier // 服务端私有判分数据：可为摘要、参数或私有结构（ADR-003），永不下发
    Asset    []byte // 图片字节，可 nil；由签名 URL 单次取回
    MimeType string
}
// Verifier 的具体形态由每个题型在阶段 3 定义；只有可安全离散化的题型才用摘要存储。
type Verifier interface{ /* 阶段 3 定稿 */ }
```
**为什么传 `io.Reader` 而不是 `*math/rand.Rand`**：决定答案的随机量（数值、正确选项位置、旋转角）必须直接来自 CSPRNG，
把 `*rand.Rand` 放进契约会诱导实现用它抽答案参数。仅图形抖动（扭曲、噪声、干扰线）可以由 entropy 播种的 `math/rand`
承担，且每次调用新建、禁止共享实例。
**接口按 ADR-003 修订**：原设计把答案强制"量化为离散值 + HMAC 精确比较"，会迫使需要容差的题型（旋转角、区域选择）
在容差边界上做不安全的裁剪，也可能诱导把原始答案塞进 payload。现在契约区分 `Payload`（公开）与 `Verifier`（私有），
判分方式由题型自定，但**答案与可推导种子绝不下发**是硬约束。
注册表 `generator.Register(g)`；`pool` 按站点权重加权随机选择生成器；`--generators` 可白名单启停。
渲染仅用纯 Go：`image`、`image/draw`、`golang.org/x/image/font` + `opentype`；字体 `go:embed`。
**3A 实现范围**：`internal/generator` 已实现文本类基线题型 `arithmetic-baseline-v1`（§8.4）；
带权重的多题型选择（`--generators` 白名单、按站点权重）与图片渲染路径属 3B，**3A 一律没有**，
因此 3A 的站点策略只有一个生成器可选。

### 8.2 题型的安全论证必须拆成四个独立指标（ADR-002）

每个题型在设计定稿时必须分别填出下表四列，且**不得用"题面随机空间大"或"用了 CSPRNG/字体随机/HMAC/多步骤"代替安全性论证**：

| 指标 | 含义 | 与什么挂钩 |
|---|---|---|
| M1 题面随机空间 | 同模板可产生的不同题面实例数 | 只影响同题复用/缓存类攻击，**不影响单次求解难度** |
| M2 答案空间与接受容差 | 正确答案取值集合大小；以及服务端接受域占答案空间的比例 | 容差会**放大接受域**，必须与答案空间一起报 |
| M3 随机猜测通过率 | `接受域 / 答案空间`，n 次尝试封顶为 `1-(1-p)^n` | 尝试次数上限（当前 3）是唯一可用的收敛手段 |
| M4 每次尝试成本 | 攻击者解一次的实测代价（模型调用费/打码费/人工时间/PoW 算力） | **唯一能与外部世界比较的量**，必须带测量条件与日期 |

规划中的四种题型（定稿前均为 `[估算]`，M3/M4 必须由阶段 3 的基线测量填实）：

| ID | 交互 | M1 题面随机空间 `[估算]` | M2/M3 答案空间·容差·猜测通过率 | M4 每次尝试成本 |
|---|---|---|---|---|
| `arith-v1` 算术/逻辑文字题 | 输入数字 | **3A 以 `arithmetic-baseline-v1` 实现，M2/M3 已实测，见 §8.4**；下表数字是阶段 0 草稿（且用的是"模板组合数"，属 M1，不是防御力） | 答案域 ≥10^4、精确匹配 → 猜测通过率 ≤10^-4（未测） | 待测：模型直接算 vs 打码人工 |
| `text-warp-v1` 扭曲字符图 | 输入字符串 | 5–7 字符 × 字体随机指派 × 逐字旋转/错切/扭曲 × 干扰线 × 噪声 | 字符集去易混后约 32^n（n≥5）→ >3×10^7；容差 0 → 猜测通过率极低（未测） | 待测：本地 OCR/多模态基线通过率与单次成本，**必须替换 §4 的 60–90% 估算** |
| `cjk-v1` 中文/成语选择题 | 点选 4 选项 | 语料 × 干扰项组合 × 遮罩/描边/色相 × 问法模板 | **答案空间仅 4，接受域 1 → 猜测通过率 25%，这是本题型最弱的量**；靠 3 次尝试封顶（≤58%）+ PoW 成本叠加，不靠渲染混淆 | 待测 |
| `rotate-v1` 图片旋转/拼合 | 拖动滑块转正 | 底图 × θ∈[0,360) × 翻转/裁剪/水印 | 原始设计写"粗 24 × 精 6 = 144"——**该数值是未验证估算，且它只描述 M2，不代表安全性**；若容差取 ±7.5°，接受域将显著放大猜测通过率（阶段 3 实测给出） | 待测 |

**统一约束（代码层强制，测试守护）**
- `Challenge.Payload` 序列化后必须经守卫检查：答案的任何可识别形式（含原始连续值、生成 seed 与题面的映射）不得出现。
- 单题尝试次数 ≤3（超限作废并计入风险）；每个题型必须给出 M3 的推导与 n=3 时的累计通过率，**下限不达标（猜测通过率 >10%）的题型不得上线**。
- 生成时记录 `solve_ms_p50_hint`（按难度档静态表 + 题型），用于 §9 时延异常判定；阈值待 `[实测]` 后设定。
- 素材与语料必须本地自有或许可证允许再分发（OFL 字体 / CC0 图片 / 自建成语表）；许可证清单落 `docs/NOTICE.md`。

### 8.4 阶段 3A 实现：`arithmetic-baseline-v1` 与生成器契约

**定位（必须写在显眼处）**：这是**生成器接口、题池与提交生命周期的工程基线**，不是生产抗自动解题题型。
公开 payload 里的 `task` 就是一道可以直接算出答案的表达式，因此**它的"答案可从 payload 推导"是设计事实而不是缺陷**——
本节不声称、§10.1 的响应也不掩盖这一点。任何"抗 OCR / 抗 AI / 证明是人"的结论都不适用于此题型（阶段 3B 的视觉题型才需要论证）。
**不得用"随机实例多"论证安全性**：M1 只影响同题复用，对本题型毫无防御价值（ADR-002）。

已实现的契约（`internal/generator/generator.go`）：
```go
type PublicPayload interface{ PublicKind() string }   // 公开题面：具名结构体，字段即白名单
type PrivateVerifier []byte                           // 私有判分数据：与 payload 不是同一个可序列化类型
type Question struct{ Payload PublicPayload; Verifier PrivateVerifier; Asset []byte; MimeType string }
type Params struct{ Difficulty int; Lang string }
type Generator interface {
    ID() string; Version() string; Media() string
    Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error)
    Verify(v PrivateVerifier, input string) bool
    NormalizeAnswer(input string) (canonical string, ok bool)
    SanitizePublic(payloadJSON []byte) (map[string]any, error)   // 出网唯一通道：未知键 ⇒ 报错
}
type Registry  // NewRegistry / Register / Lookup / IDs
```
与 §8.1 草稿的三处差别，都是被实际约束逼出来的：
1. `Verifier` 从"接口"改成 `PrivateVerifier []byte`：库里的 `verifier_blob` 就是一段字节，
   判分由生成器自己解释。再套一层接口只会诱导把私有结构体直接 `json.Marshal` 出网。
2. 新增 `Version()`：题池按 `(generator_id, version, difficulty)` 分桶。**改了判分实现必须升 version**，
   否则库里遗留的旧 verifier 会被新实现解释——那是静默的正确性事故。
3. 新增 `NormalizeAnswer`：把"什么算一个答案"从 `Verify` 里拆出来单独可测（前导零、符号、空白、非 ASCII 数字各是一条独立断言）。
   `Lookup` 未登记的 generator_id ⇒ `ErrUnknownGenerator`，**没有任何回落**（不"随便挑一个题型顶上"）。

`arithmetic-baseline-v1`（`internal/generator/arith.go`）的取值范围与判定：
- 难度档 1..5；操作数 ≤99、中间值与答案一律落在 ±1e9 内、表达式串 ≤64 字符；**乘积在求值前做溢出检查**；
  除法只允许"右操作数 2..9 且整除"的组合，否则重抽（有界 128 次，耗尽 ⇒ `ErrGenerateExhausted`，可重试）。
- 决定答案的随机量全部来自 `crypto/rand`（`rand.Int`），**不接收** `math/rand`；`Generate` 的 `entropy` 由调用方给（worker 传 `crypto/rand.Reader`），
  测试可注入确定性流，生产路径不回落到固定种子。
- 规范化答案：裁首尾 ASCII 空白、内部空白即畸形、允许单个 `+`/`-` 前缀、只接受 ASCII 数字、剥前导零（`-000` ⇒ `0`）、≤12 位。
  非 ASCII 数字（如 `١٢٣`）、`1e3`、`0x10`、`+1` 之后再接符号等一律拒绝——拒绝是"非法答案"，不是"答案错"。
- 判分：`Verify` 先规范化再重算 HMAC 并 `subtle.ConstantTimeCompare`；**没有**任何读回答案的接口。

M1–M4 填表（ADR-002；实测条件：本机 go1.24.5/linux-arm64，2026-10-17，每档 5000 样本，见 `TestArithAnswerDistribution`）：

| 指标 | 值 | 说明 |
|---|---|---|
| M1 题面随机空间 | ≥10^6 `[估算]` | **无防御价值**，只说明同题复用概率低 |
| M2 答案空间与接受容差 | 依赖难度档，答案域约 10^2..10^7；容差 0（精确匹配） | 规范化后每个答案对应唯一串 |
| M3 随机猜测通过率 | 单点最高概率 **实测**：d1 ≈0.045–0.048 / d2 ≈0.022 / d3 ≈0.008 / d4 ≈0.014 / d5 ≈0.011 | **答案分布不均匀**：d1 只有约 35 个不同答案，且**众数是并列的**（`a-b` 的 0 与 `a+b` 的 14 各有 11 种组合 ⇒ 同概率，所以"热点是 14"与"热点是 0"是同一份分布的两次抽样，不是两个结论），"均匀随机猜"的乐观假设不成立，因此按**最大单点概率**算。3 次尝试的累计上界 ≈ 3×P（d1 ⇒ **13.5%–14.4%**，已超 §8.2 的 10% 线）。数字是每档 5000 样本的**单次抽样**，重复运行会在小数点第二位抖动（`TestArithAnswerDistribution` 每次打印；2026-10-17 复核时第二次抽样实录 d1=0.048/众数 0、d2=0.0222、d3=0.0076、d4=0.0136、d5=0.0106） |
| M4 每次尝试成本 | 未测 | 基线题型可被程序 O(1) 解出（解析 `task` 即得答案），成本 ≈ 0。**这正是它不进生产的原因** |

结论照 §8.2 的硬门槛执行：**`arithmetic-baseline-v1` 只用于验收与接口回归，不得作为站点面向真实流量的题型配置**；
文档、错误码、管理响应都不声称它抗自动解题。分布数据是"答案聚集程度"的度量，不是"安全性"的度量。

### 8.5 阶段 3B 实现：`text-image-v1`（实验性视觉题型）

**定位**：这是第一个视觉生成器，交付的是"图片资产 → 存储 → 下发 → 判分"这条工程链路，
**不是**已验证的抗 AI 方案。管理面里它带 `experimental:true` + `production_ready:false`。

取值定义：长度固定 6；字符集 `ABCDEFGHJKLMNPQRSTUVWXYZ23456789`（32 个，剔除易混的 I/1、O/0，
它是**公开常量**、进 payload）；字符由 `crypto/rand.Int` 均匀抽取；归一化 = 裁首尾 ASCII 空白 →
小写折大写 → 长度必须恰好 6 → 每字符必须在字符集内，否则一律**畸形**（不消耗作答机会，ADR-011 #3）。
verifier 与算术题**同一条构造**（`nonce(16B) ‖ HMAC(K(qver), domain‖nonce‖0x00‖答案)`，48B），
本轮没有发明新的密码学协议。

渲染参数**必须有界**且只有五档（不组合出几百个未测配置）：画布 ≤640×256 且像素总数 ≤120,000、
字号 16..96pt、DPI 72..150、单字旋转 ≤15°、基线/字间抖动 ≤12px、干扰线 ≤8 条×≤8 段、
噪点 ≤ 像素数的 5%、PNG ≤65,536B。越界即 `ErrBadParams`，**不"夹紧后继续画"**。
可读性由几何保证而非目测：字形旋转后的包围盒必须完整落在画布内（离边 ≥2px）且相邻水平重叠
≤ 较窄者的 1/4，做不到就有界重试（128 次）后返回可重试的 `ErrGenerateExhausted`——
**代码里不存在"发出被裁切或字符挤在一起的图"这条路径**。d1 是零抖动的"干净档"（可读性对照 + 字节级可复现）。

M1–M4 填表（ADR-002；实测条件：本机 go1.24.5/linux-arm64，2026-10-18，见 `docs/STAGE3B.md` §5–§6）：

| 指标 | 值 | 说明 |
|---|---|---|
| M1 题面随机空间 | 答案串 32^6 ≈ 1.07e9，再乘连续旋转/抖动/干扰线/噪声的组合 | **无防御价值**，只说明同题复用概率低（红线 14：不得用它论证抗 AI） |
| M2 答案空间与接受容差 | 答案空间 32^6 = 1,073,741,824；容差 **0**（精确匹配） | 大小写折叠只增加"同一答案的写法数"，接受域仍是 1 个规范化串 |
| M3 随机猜测通过率 | 单题 `p = 9.31e-10`；3 次**独立新挑战**累计 `1-(1-p)^3 = 2.79e-9`（上界 3p） | 一题只有一次作答机会 ⇒ 要 n 次机会必须领 n 道题、每次重付 PoW 与池容量。**猜测下限极低不等于抗机器**：它只说明"猜"不是攻击路径 |
| M4 每次尝试成本 | **已有一次真实基线（2026-10-18，仅通用 OCR）**：固定最强配置（tesseract 5.3.4 + eng 4.1.0 模型、`--psm 6 --oem 3` + 32 字符白名单、无预处理），holdout 各 100 张的**整串严格通过率**（分母含工具失败与畸形输出）= 低档 **96%** [90.2%, 98.4%] / 中档 **31%** [22.8%, 40.6%] / 高档 **3%** [1.0%, 8.5%]（Wilson 95%） | 这条**推翻了"低档可用"的默认**：96% 与随机猜测的 `9.31e-10` 差 9 个数量级，只能来自实际读出字符 ⇒ 生产默认不得落在低档。同时它**不构成**抗 AI/抗多模态证据：只测了一个离线 OCR、一种参数组合、本机 arm64 容器；高档那 3% 的字符级正确率仍是 85%（读对了大部分字符、只是整串没精确匹配上）。"M4 未测所以不得声称抗 OCR"这条限制已解除，取而代之的限制是：**只有单一工具、单一配置的基线，不足以支撑任何"抗机器"结论**，要下结论还需多工具/多模态/人工组合的对照与真机成本测量 |

评测纪律：样本分 **dev 调参集**与 **holdout 固定验收集**（holdout 的 seed 写死在代码里、不允许覆盖），
避免"看完验收集结果再调参、仍拿同一集当独立证明"。文件名只带序号、不编码答案；
ground truth 与样本图落在 `eval-out/`（已 gitignore）。固定测试熵源在 `internal/eval`，
`stage3b.sh` 用 `go list -deps ./cmd/server` 证明**服务二进制不链接它**。
评分器同时报两个分母（`StrictAccuracy` 把缺测与工具失败算不通过、`ScoredAccuracy` 只在成功跑完的样本上算），
并把"工具失败"与"答错"分开计数——只报一个数就能把结论做出来。

无障碍边界（不许含糊）：这是**纯视觉**题型，对无法看图的用户本质上不可完成；本轮没有音频或文本等价通道
（§11 的非视觉等价通道属阶段 4）。**PoW-only 降级不是等效无障碍方案**：它换掉的是"题目这一层"，
防线少一层，安全语义不同。预览页与 `alt` 文本都按这个口径写。

### 8.3 题池水位与预生成

> **3A 实现见本节末尾与 §5.4**；下面第一段是阶段 0 草稿，`low_watermark` 公式与 `max_rows` 沿用，
> 但"池空 ⇒ 同步生成、连续 5 次转 429"**没有实现**——3A 改成 **503 `challenge-unavailable` + `Retry-After`**，
> 因为同步生成会把生成成本搬进公共请求路径（1 vCPU 下一个慢生成就是一个慢请求），
> 而"降级成 PoW-only 先发出去"会让站点声明的策略静默失效，两者都不如"如实说现在给不出题"。

```
low_watermark  = clamp(2 * 站点数, 50, 500)   # 低于则补
high_watermark = 2 * low_watermark            # 补到此停止
max_rows       = 20000                        # 硬上限，超出拒生成并告警
worker         = 单个后台 goroutine，priority=让出 CPU（每补 10 题 sleep 100ms）
```
- 启动时预热 `low_watermark` 题（否则首请求同步生成，延迟上升，量级待 `[实测]`）。
- claim：在**一个事务**内完成"选一道 ready 题 → 条件更新为 claimed（检查影响行数）→ 建 session 行"，提交后才对外返回。
  由 `db.BeginTx` 让驱动发起 `BEGIN`；**不得**在 `*sql.Tx` 上手工执行 `BEGIN IMMEDIATE`（嵌套事务错误，ADR-004）。
  若因性能需要 immediate 事务，只能通过驱动连接参数实现，并另写并发测试证明等价性。
- 池空/被耗尽：同步生成（走同一 `Generate`）并打 metric `pool_miss`，连续 5 次 → 返回 429 并告警。

**3A 实际形状**（`internal/pool/pool.go`，默认值即 `DefaultConfig()`，可经 6 个 `EPOCHA_POOL_*` 环境变量覆盖）：

| 参数 | 默认 | 约束 | 为什么这样定 |
|---|---|---|---|
| `LowWatermark` / `HighWatermark` | 50 / 100 | `high ≥ low`，均为正 | 沿用 §8.3 草稿，仍是 `[目标]` |
| `MaxRows` | 20000 | 全局（不是每桶）硬上限 | 写满即停；判定与插入在同一条 SQL 里，不靠"先查再插" |
| `Workers` | 1 | 上限 `MaxWorkers=4` | 1 vCPU 下多 worker 只是抢同一份 CPU；上限防手滑 |
| `QuestionTTL` | 4200s | **必须 > 站点最大 challenge TTL** | 领题条件要求题目比 challenge 晚过期；不成立时永远 503。启动打警告而非静默 |
| `PollInterval` | 2000ms | ≥100ms | 水位检查周期 |
| `BatchPerTick` | 20 | 正数 | 把写事务与 CPU 都切成小段 |
| `YieldEvery` / `YieldPause` | 10 / 100ms | 正数 | 每 10 题主动让出 CPU |
| `BackoffStart` / `BackoffMax` | 100ms / 10s | 指数增长 | 生成或写库失败绝不打成忙循环 |

三条实现细节不是装饰：
- **水位迟滞**：`refilling` 是一个内存态标志。跌破 `low` 才置位、补到 `high` 才撤除。没有它，
  当"一轮补不完"（`BatchPerTick` 小于缺口）时 worker 会永远在"补到 low 就停 → 立刻又低于 low"之间抖，
  池子长期停在低位。代价：这一位是内存态，重启最多晚一轮或多补一轮，不影响任何安全判定。
- **熵读取有上限**：每次生成最多消费 4096 字节熵（`io.LimitReader`）。一条"重复返回同一字节"的熵源
  会让 `crypto/rand.Int` 的拒绝采样**永久自旋**（这是本轮实测踩到的挂死），上限把它变成一个有界的失败。
- **`Stats` 是进程内计数**（`Generated`/`Failed`/`Rejected`/`LastErr`/`LastTick`），重启归零，
  管理响应里显式带 `restart_resets:true` 说明这一点——它是可观测性，不是账本，绝不参与任何安全判定。
  `LastErr` 只写 `errClass` 折叠后的有限类别（`pool-full` / `entropy` / `generate-exhausted` / `bad-params` /
  `unknown-generator` / `canceled` / `unspecified`），不含答案、题面、库或驱动原文、密钥材料。
- claim 落在**单个事务** `IssueChallengeWithQuestion`：插 challenge 行 + 选 ready 题 + 条件更新为 `assigned` +
  回读题面。`SELECT` 候选与 `UPDATE` 之间的竞争由条件更新的**影响行数**裁决（不是靠"先抢到行锁"），
  输了就换下一个候选，最多 `claimAttempts=5` 次；5 次都输 ⇒ `ErrPoolEmpty`（可重试，不是错误升级）。

### 8.6 阶段 4A：widget 与业务接入（工程闭环完成，效果仍未测）

生产 widget 是零依赖原生 JS（`web/widget/v1.js`），直接 Shadow DOM 挂载，**不用 iframe**：

- Shadow DOM 只是样式与命名隔离，**不是安全边界**（同源脚本照样能读它）。文档与代码注释都这么写。
- 不用 iframe 就不存在"换 Origin 之后再拿客户端自报的父域名冒充可信来源"这条路；
  Origin 语义与 §10.3 完全不变。
- PoW 在 **Dedicated Worker** 里算，两种加载方式都实现了：A) 内嵌同一份源码建 Blob Worker
  （需要 `worker-src blob:`）；B) 接入方同源部署 `v1.worker.js` 并用 `workerUrl` 指定（只接受同源 URL）。
  两条路径的字节由防漂移测试钉住必须相同。没有 `eval` / `new Function` / 动态 import / 远程代码地址。
- 生命周期与并发语义见 §11；接入面与 CSP 见 `docs/WIDGET.md`；实测与验收见 `docs/STAGE4A.md`。
- **仍然只有视觉通道**：非视觉等价通道（音频等）未实现，PoW-only 降级不是等效替代（§8.5 的结论不变）。

### 8.7 2026-11-05 题型扩展：图形点选、图形计数与抽象文字（ADR-024）

这一节记录三个**新增**题型；它们不替换 v1/v2/算术基线，站点按 `generator_id` 显式选择。
三个题型一律 `experimental:true` / `production_ready:false`：本轮没有新的 M4 实测，也没有人工可读性验收。

#### 8.7.1 `shape-grid-v1`（非纯文字输入的图形点选）

- 画布 340×340（115,600 像素，仍在 §13 的 120,000 上限内），3×3 网格，格 100px、间距 8px、边距 12px；
  每格一个几何图形：圆/三角/正方形/五边形/六边形，实心或空心。
- 判据是**公开题面**的一部分，三选一：按种类（"所有三角形"）、按填充（"所有实心图形"）、
  按两者组合（"所有空心正方形"）。生成器保证恰好 `k ∈ {2,3}` 个格子满足判据，其余格子放**不可能**满足判据的图形。
- **交互是点选，不是文字输入**：widget 在图片上覆盖 9 个可聚焦按钮（百分比定位，随图片缩放；
  `aria-pressed` 表示选中；键盘 Space/Enter 可切换）。
- 答案编码：1-based 格子序号排序去重后的逗号串（`3,7`）。规范化接受 `3, 7` 与 `7,3`，
  重复序号去重（`3,3,7` ⇒ `3,7`）；空、0、越界、非十进制一律**畸形**（不消耗作答机会）。
  规范化不扩大接受域：一个 challenge 只接受唯一一个规范化集合。
- M1–M4：M1 = 判据组合 × 图形种类/填充 × 格子排列；M2 = 唯一规范化集合；答案空间 `C(9,2)=36`
  或 `C(9,3)=84`；M3 = 最坏单次 `1/36`，3 次上界 `1-(35/36)^3 ≈ 8.1%`；M4 **未测**。
  图形题对机器视觉并不难，它交付的是交互多样性，**不是抗 AI 证明**。

#### 8.7.2 `shape-count-v1`（图形计数）

- 画布 480×240（115,200 像素）。场景里只有两类**目标**图形（六个类别中选两个：实心/空心 × 圆/三角/正方形/
  五边形/六边形），每类数量 1..6，另有 0..2 个不属于任何目标类别的干扰图形；图形放在 4×4 的槽位里，
  槽位内做有界抖动，避免重叠——不存在"图形叠在一起导致数不清"这条路径。
- 题面同时问两类各有多少个；widget 渲染两个带标签的数字输入（不需要文字作答）。
- 答案编码：`a,b`，两个 ASCII 十进制数，剥前导零；负数、小数、多余字段一律畸形。
- M1–M4：M1 = 类别组合 × 数量 × 干扰排列；M2 = 唯一 `a,b` 串；答案空间 `6×6=36`；
  M3 = 单次 `1/36`，3 次 ≈8.1%；M4 **未测**。

#### 8.7.3 `text-image-v3`（抽象文字 + 单字容差）

- 固定 6 字符、32 字符集、480×160、灰度 PNG。在 v2 的逐字形变（旋转/剪切/缩放/重叠/波浪/穿字线/加粗）之上，
  再对**字形层**做后处理：
  1. 低对比墨色/纸色（五档墨 44..52、纸 238..242，差值必须 ≥96）；
  2. 1..2 遍 3×3 加权模糊（核 1-2-1，只模糊字形层，不糊背景）；
  3. 两条平滑正弦位移（水平位移随 y、垂直位移随 x；位移幅度 ≤6px，周期 24..80px）；
  4. 稀疏擦除点（≤32 个，只落在字形包围盒并集内）；
  5. 纸面低频纹理（幅度 ≤16）。
- 布局边距按 `warp + blur + 2` 放大，字形不会因为后处理被画布裁切。渲染后计算字墨覆盖率：
  后处理保留的 alpha 总量必须 ≥ 原始的 55%，否则**有界重排**（不重抽答案）。这条守卫证明的是
  "字没有被擦掉"，**不是**"人能读出来"；人工可读性仍为 NOT_RUN。
- 判分允许**最多 1 个字符替换**：私有 verifier 是逐字 HMAC 标签
  `nonce(16B) || tag_0..tag_5`（每标签 32B，共 208B），`Verify` 对每个位置重算标签并常数时间比较，
  累计不匹配数 ≤1 才通过。没有读回答案的接口；答案明文仍不落库/不下发/不写日志。
  v1/v2 继续使用 48B 的精确匹配 verifier，路径不变。
- **接受域与 M3**：答案空间 32^6 = 1,073,741,824；接受域 = 精确答案 + 6 个位置 × 其余 31 个字符 = 187 个规范化串；
  单次随机猜中率 187/32^6 ≈ 1.74e-7，3 次累计 ≈5.2e-7。M4 **未测**（v3 没有 OCR 基线）。
- 逐字标签的边界必须写明：拿到判分密钥的人本就能离线枚举 verifier；逐字标签把"有密钥时的离线枚举"
  从 32^6 降到 6×32，**不改变无密钥时的安全边界**。
- 容差同时降低 OCR 的整串门槛（读错一个字符仍可能通过）：这是为"人认错一个字仍能过"付出的代价，
  **不是**抗 OCR 措施；v3 的 M4 未测，不得据此声称抗 OCR/抗 AI。
- 人工可读性目标（"可能认错但基本能认出"）目前只有参数设计与覆盖率守卫；**没有**人工验收证据。

#### 8.7.4 新题型的公开 DTO 与 widget 映射

- `shape-grid-v1` 的公开字段：`kind:"shape-grid"`、`answer_type:"cells"`、`image_width/image_height`、
  `grid_cols/grid_rows/grid_x/grid_y/grid_cell/grid_gap`、`prompt_zh/prompt_en`、`max_answer_len`。
  `SanitizePublic` 逐键白名单；任何未知键让整次出网失败。
- `shape-count-v1` 的公开字段：`kind:"shape-count"`、`answer_type:"pairs"`、`image_width/image_height`、
  `labels_zh/labels_en`（恰好 2 个）、`min/max`、`prompt_zh/prompt_en`、`max_answer_len`。
- `text-image-v3` 沿用 `text-image` 的公开字段，并增加可选的 `tolerant:1`（只允许 0 或 1，且必须与 profile 一致）。
- widget 对 `answer_type` 的处理：
  - `text`：现有文本输入（v1/v2/v3）；
  - `cells`：图片上覆盖可聚焦按钮，选中集合写入同一个 `answer` 串；
  - `pairs`：两个带标签的数字输入，`a,b` 写入同一个 `answer` 串。
  widget 不解释答案、不做判定；服务端仍是唯一权威。服务端文本只进 `textContent`，不拼 HTML。
- 站点仍只选一个 `generator_id`：本轮**不**实现按权重混合多题型（§8.2 的"题型权重与多生成器选择"
  仍未实现），"多样性"指可选题型增加与题型内变化。

### 8.8 2026-11-12 交互原语 Wave 1（ADR-030）

本轮把少数「程序化、可离散判分、键盘可完成」的交互原语实现为实验题型，**不照搬外部游戏关卡**：
不复制 Neal.fun 的源码、图片、角色、品牌、音乐、视频、文字表达或页面布局；不使用真实人脸、车牌、
品牌 logo、地标照片或人物角色；所有题面由代码 + 已批准的 `golang.org/x/image` 生成。

Wave 1 已登记：

| ID | 交互 | canonical answer | verifier | 唯一解证明 | 3 次随机猜测上界 |
|---|---|---|---|---|---|
| `tile-rotation-v1` | 3×3 路径块旋转 | `v1:<9 hex masks>` | 48B 精确 HMAC | 枚举每格不同旋转，只接受解数 ==1；节点上限 2^20 | 候选空间 ≥64 ⇒ ≤1/64（≈4.7%） |
| `symbol-path-v1` | 6×6 符号路径 | `v1:NN.NN…` | 48B 精确 HMAC | 目标符号序列的匹配路径 DFS 恰好 1 条；合法位置路径 ≥128 | ≤1/128（≈2.34%） |
| `crowd-find-v1` | 36 选 1 非人形角色 | `v1:<0..35>` | 48B 精确 HMAC | 目标 6 特征签名在 36 个角色中恰好 1 个 | ≤3/36（8.33%） |
| `visual-rank-v1` | 5 卡按线段数排序 | `v1:A.B.C.D.E` | 48B 精确 HMAC | 5 个线段数互异、间距 ≥2；答案唯一排列 | 1/120（3 次 2.5%） |

共同边界：

- 公开 payload 不含答案、目标索引、目标 masks、solution path、piece-slot mapping、swap sequence
  或可推导私有答案的 seed；`SanitizePublic` 逐键白名单，未知键整次报错。
- 新增离散编码 `masks/path/select/order/sequence/placements`（`internal/generator/answer.go`）：
  固定 `v1:` 前缀、固定字段顺序/分隔符、严格 ASCII、严格长度与范围；畸形不消耗作答机会。
- 客户端只提交原始操作结果；不提交 `solved`、`targetIndex`、`expectedAnswer`、私有 seed 或 RNG 状态。
- 所有题型 `experimental:true` / `production_ready:false`；默认不加入任何站点，必须由运营者在
  `generator_id` / `generator_pool` 显式选择。M4、人工可读性、辅助技术、真机移动端均 **NOT_RUN**。
- 资源：payload ≤8KB、verifier ≤4KB、单资产 ≤64KB；网格/帧数/路径长度/DFS 节点均有上限；
  重计算只在题池 worker，不在 redeem 热路径。
- Wave 2（`progressive-target-v1`、`shell-track-v1`、`silhouette-assembly-v1`）与 negative polarity
  修饰器、`pipe-connect` 完整管道整改见 §8.9、ADR-031 与
  `docs/reviews/CAPTCHA-BANK-REMEDIATION-2026-11-12.md`。`pipe-connect-v1` 的现状 verifier
  只能表达 2 个断点格，无法表达整张图的逐格 edge mask；在不动已签发 verifier 语义的前提下
  标记 **BLOCKED**，不得用客户端 `solved=true` 代替服务端验证。

### 8.9 2026-11-12 交互原语 Wave 2、negative polarity 与 pipe-connect 边界（ADR-031）

Wave 2 已登记三个题型；它们与 Wave 1 共用离散 canonical answer 与 48B 精确 HMAC：

| ID | 交互 | canonical answer | 唯一解/边界 | 3 次随机猜测上界 |
|---|---|---|---|---|
| `progressive-target-v1` | 三轮 3×3 定位，客户端逐轮推进不判对错 | `v1:a.b.c`（`sequence`） | 每轮生成器只放一个目标字形；干扰字形永不等同 | `1/9^3 = 1/729`；3 次 `3/729 ≈ 0.41%` |
| `shell-track-v1` | 三轮四容器，服务端预渲染 33 帧雪碧图 | `v1:a.b.c`（`sequence`） | 目标只由 swap 序列在服务端推进；payload 无 swap/目标索引 | `1/4^3 = 1/64`；3 次 `3/64 ≈ 4.69%` |
| `silhouette-assembly-v1` | 5 部件/5 槽位，旋转后离散放置 | `v1:id:rot,...`（`placements`） | 枚举 piece×slot×rotation，只接受恰好一个完整匹配；部件无旋转对称且两两不在 D4 下等价 | `5!×4^5 = 122,880`；3 次 ≈ `2.45e-5` |

- `shell-track-v1` 的动画是**服务端 worker 预生成**的 PNG 雪碧图（11 帧/轮 × 3 轮），
  不是把 swap sequence 下发给浏览器再用 CSS/JS 动画。为什么不用 GIF：现有题池单资产且
  `asset_mime` CHECK 只允许 PNG；三轮需要在轮次边界暂停，单张 GIF 无法表达。GIF 的
  disposal/时序与 Chrome/Firefox/WebKit 一致性 **NOT_RUN**，不声称通过。
- `shell-track-v1` 标记 `Motion:true`：`prefers-reduced-motion` 请求在 dispatch 层被排除；
  widget 也会明确失败并提示换题，不强迫用户完成运动题。
- **negative polarity** 只修饰 `shape-grid-v1` 的 kind/category 模式：k 仍 2/3，答案空间仍是
  `C(9,k)`；至少 2 个应选、至少 2 个不应选；fill 模式固定 positive。提示只有一层否定
  （中文「不是」/英文 NOT），不依赖颜色，不用"除了……以外不……"；positive/negative 错误率
  必须分开记录。它不提高随机猜测下界，也不得写成"显著提高安全性"。
- **`pipe-connect-v1` 仍是 legacy cells 题型**：答案只是两个断点格，不能表达多对端点配对、
  路径不交叉、每格被占用、唯一解或逐格 edge mask。原地改会破坏已签发 verifier 语义，
  因此整改标记 **BLOCKED**，必须新开 ID/version；新增测试
  `TestPipeConnectRemainsLegacyCellSelection` 钉住现状。
- 资源：payload ≤8KB、verifier ≤4KB、单资产 ≤64KB；33 帧雪碧图 1680×600 在客户端
  2048×2048 形状上限内；生成失败有界重试，不降级成固定题。
- 全部 Wave 2 题型与 polarity 仍为 `experimental:true` / `production_ready:false`；
  M4、人工可读性、辅助技术、真机移动端、抗 AI/抗 OCR 全部 **NOT_RUN**。

## 9. 风险打分层（第三层，阶段 3+ 实现）

`score ∈ [0,1]`，加权求和。**下表权重全部是 `[目标]` 初值，无任何一条经过测量**；阈值与权重的定稿必须依赖 `[实测]` 的
时延分布与题型成本基线（ADR-006），在此之前不得据此做 deny 决策：

| 信号 | 权重 `[目标]` | 说明 |
|---|---|---|
| 频率：同 IP 掩码 5min/24h 请求数与通过率 | 0.30 | 唯一可作 deny 依据的硬信号 |
| PoW 时延 vs 理论下限 | 0.20 | 过快 = 可疑（自动化集群）；下限本身需 `[实测]` 校准 |
| challenge→redeem 总时延 | 0.15 | 异常快或超 TTL 均为异常 |
| 答题正确但命中"高频模板" | 0.10 | 检测模板枚举 |
| widget 环境自检（WebGL/canvas、无头特征、`navigator` 一致性） | 0.10 | **弱信号，客户端可伪造，只能作 tie-breaker，禁止单独 deny** |
| 语言/UA/时区/Referer 与站点历史分布偏离 | 0.15 | 仅统计偏离，不做身份判定 |

决策：`score < risk_threshold` → 签发 response；中间带 → 提高难度重出题（`harder`）；更高 → `deny`。
**题型淘汰分级（ADR-006）**：统计信号只产生"候选降权"告警 + 人工确认入口；自动降权要求"高通过率 **且** 极低时延 **且** 样本充足"同时成立；
自动下线要求连续多窗口且不被更高优先级信号反驳。**禁止仅凭通过率判定题型被机器破解并自动下线。**
限流：内存**固定窗口计数器**（不是令牌桶——`internal/risk/risk.go` 自始如此，本文与 AGENTS 早期的"令牌桶"叫法已在
ADR-016 一并更正）按 `ip` 与**站点 id**（不是自报的 `site_key` 字符串，见 ADR-009）两维，状态条目有硬上限
（默认 8192，可配）+ 定期清理与超额淘汰，否则攻击者可用随机源地址撑爆 1GB 内存；超限 `429 + Retry-After`。
**窗口必须与名字一致**（ADR-016）：`EPOCHA_RATE_IP_RPM` / `rate_site_rpm` 声明的是**每分钟**，
因此分桶必须按 `windowSize`（1 分钟）对齐。阶段 5B 之前分桶用的是 `now().Unix()`（**每秒**一个窗口），
后果是限流比声称的松 60 倍（60 RPM ⇒ 实际 60 次/秒）；单测当时只推进整个窗口，测不出来。
现在的口径：`bucket = Unix() / 60`，`RetryAfter` 从该窗口的起始时刻算剩余时间，两条都有测试钉住。
持久封禁（阶段 2+ 可选）写 `meta` 中的 ban 列表，避免重启清零。
**阶段 1 的限流就是这个有界实现**，不含打分。

**速率与存量是两个正交的量**（ADR-009）：限流器控制"多快"，控制不了"库里同时有多少"。
`/api/challenge` 的写入与"活动 challenge 是否超限"必须在**同一条 SQL** 内判定
（`INSERT … SELECT … WHERE (SELECT count(*) …) < limit`），活动上限由 `EPOCHA_MAX_ACTIVE_CHALLENGES`（默认 10000 `[目标]`）
与 `EPOCHA_MAX_ACTIVE_CHALLENGES_PER_SITE`（默认 2000 `[目标]`）给出；**历史账本总量另由 `EPOCHA_MAX_LEDGER_ROWS`（默认 100000 `[目标]`）限制**，不随活动上限隐式变化。到顶对外表现为 429，不返回 5xx 也不假成功。
清理任务仍按每个事务最多 500 行执行，但服务调度器每 10 秒以最多 8 个批次、250ms 时间预算和 5ms 可取消让步推进 backlog；预算用尽会留到下一轮，不无限占住单写连接。总量上限释放只依赖安全清理：仍被 response 或有效 challenge/pool 引用的行不得提前删除。
**文件不会因此自动缩小**（有测试记录 155648 vs 空库 57344 字节）；收缩需要 `VACUUM`，本版不做。
TTL 有 `[1s, 1h]` 上下界——有效期长度直接决定防重放账本必须保留多久，不设上界等于允许一条配置让库永久增长。

## 10. API 契约

### 10.1 公开 API

| 方法 | 路径 | 鉴权 | 阶段 | 说明 |
|---|---|---|---|---|
| POST | `/api/challenge` | `site_key` | 1（3A 起按站点模式附带题目） | 签发 PoW challenge；`pow_question` 站点同时从池里领一道题并原子绑定 |
| POST | `/api/redeem` | challenge token | 1（3A 起接受 `answer`） | 提交 PoW 解（与答案），成功换一次性 `response` |
| POST | `/siteverify` | `secret` | 1 | reCAPTCHA 风格，服务端调用 |
| GET | `/healthz` | 无 | 1 | 存活探针，只返回 `{"status":"ok"}`，不含版本/路径/计数/依赖信息，不做健康即放行的语义 |
| GET | `/readyz` | 无 | 5A | **就绪探针**（阶段 5A）：存储层有界自检——连得上、`meta.schema_version` 与本程序一致、**写路径可用**（主文件可写 + 数据目录可创建临时文件）。就绪 ⇒ `{"status":"ready"}`；否则 **503** `{"status":"unavailable"}`。三条边界：不分配题目也不消费 token（有测试逐表对账行数）；自带 2s 上限并沿用请求 ctx（探针不得成为故障源）；只回一个词，不外泄路径/SQL/驱动文本/站点数/失败原因（原因只进日志的类别字段）。题池水位低**不是**未就绪，那是容量信号（§8.3） |
| ~~GET~~ | ~~`/asset/<pool_id>/<sig>`~~ | — | **未采用（3B）** | 草稿里的签名媒体端点没有实现：它会凭空造出一个需要防枚举、防跨 challenge 引用的面，而题目是一次性的。图片改为**内联在 `/api/challenge` 的公开 DTO 里**（§10.1 下方、ADR-012） |
| GET | `/widget/v1.js` | 公开 | **4A** | widget 主脚本（go:embed）。实测 gzip **28,968 B** [实测 2026-11-12]（预算 30KB [目标]） |
| GET | `/widget/v1.worker.js` | 公开 | **4A** | PoW worker 的**同源副本**（接入方选方案 B 时用）。内容与 v1.js 内嵌那份逐字节相同，由测试钉住 |
| GET | `/widget/v1.interactive.js` | 公开 | **2026-11-12** | 实验型交互题渲染器（ADR-025 第 5 条的按需模块）：只在 `answer_type` 为 `masks/path/order/select/sequence/placements` 时由 v1.js 同源加载；不含答案或判分逻辑。实测 gzip **10,311 B** [实测 2026-11-12] |

阶段 1 的公开端点**只有 PoW**：响应里不存在 `challenge` 对象，也不接受 `answer`/`behavior`。文档与实现都必须如实反映这一点，
不得用空字段伪装成完整三层（ADR-005）。**3A 之后这段话只适用于 `pow_only` 站点**：`mode` 由站点配置决定，
客户端没有任何字段可以选择它；三层里的第三层（风险打分）**仍然不存在**，3A 只加到第二层的工程骨架。
行为数据（`behavior`）**不接受、不存储、不判定**——它是阶段 3+/4 的东西。

`POST /api/challenge` — 请求（`application/json`，≤4KB `[目标]`）
```json
{ "site_key": "7QK4M2ZR6YTB8VXF", "lang": "zh-CN" }
```
阶段 1 响应 `200`
```json
{
  "token": "v1.6f2a1c9b8d0e4a71.<b64url(payload)>.<b64url(tag)>",
  "expires_in": 180,
  "pow": { "algo": "sha256-prefix", "seed": "<b64url(16B)>", "bits": 14 }
}
```
所有 API 响应带 `Cache-Control: no-store` 与 `X-Content-Type-Options: nosniff`（单次可用的 challenge 不得被中间缓存复用）。
**阶段 3A 追加两个键**（`pow_only` 站点不会看到 `question`）：
```json
{
  "token": "v1....", "expires_in": 180,
  "pow": { "algo": "sha256-prefix", "seed": "<b64url(16B)>", "bits": 14 },
  "mode": "pow_question",
  "question": { "generator": "arithmetic-baseline-v1", "generator_version": "v1",
                "task": "12 + 7", "answer_type": "integer", "max_answer_len": 14 }
}
```
`question` 的字段集合由生成器的 `SanitizePublic` 白名单给出（§5.4/§8.4），**不是**题池行的直接序列化：
里面没有答案、没有 verifier、没有内部随机数，也**没有题池行 id**（客户端不需要一个可枚举的题目标识，
题目 ↔ challenge 的绑定只存在于服务端 `pool.challenge_id`）。`mode` 是站点策略的**签发快照**，同时被签名覆盖。
草稿里写的 `challenge: {id, media, asset_url, task}` 形状**没有采用**：3A 无图片资产，
把 `id` 与 `asset_url` 发给客户端会凭空造出一个需要防枚举、防跨 challenge 引用的面。

**阶段 3B 追加三个键**（只有 `media=image` 的题型会出现；算术题的字段集合与 3A **逐字节相同**，
有测试钉住这条兼容性）：
```json
{ "question": { "kind": "text-image", "length": 6, "charset": "ABCDEFGHJKLMNPQRSTUVWXYZ23456789",
                "answer_type": "text", "max_answer_len": 6, "image_width": 300, "image_height": 112,
                "generator": "text-image-v1", "generator_version": "v1",
                "media": "image", "image_mime": "image/png", "image_base64": "iVBORw0KG…" } }
```
`image_base64` 是**标准 base64**（配 `data:image/png;base64,` 使用），不是 base64url；
它的上界由 `pool.asset_blob` 的 CHECK 给出（65,536B ⇒ base64 后 ≤87,386 字符，实测 d5 约 8KB 响应）。
媒体声明与资产**必须成对**：`media=image` 而库里没有资产（或 MIME 不是 `image/png`）时，
`questionDTO` 返回错误 ⇒ 作废该 challenge + 503 可重试，**绝不**下发一道看不见的题，
也**绝不**退化成 `pow_only`（§14-14）。图片只随这一次响应出去，之后任何路径都不再读它。

**2026-11-05 追加的图形题 DTO**（ADR-024；同样是 `media=image` + 内联 PNG）：
```json
{ "question": { "kind": "shape-grid", "answer_type": "cells", "max_answer_len": 32,
                "image_width": 340, "image_height": 340,
                "grid_cols": 3, "grid_rows": 3, "grid_x": 12, "grid_y": 12,
                "grid_cell": 100, "grid_gap": 8,
                "prompt_zh": "选出图中所有的三角形。", "prompt_en": "Select every triangle.",
                "generator": "shape-grid-v1", "generator_version": "v1",
                "media": "image", "image_mime": "image/png", "image_base64": "iVBORw0KG…" } }
```
```json
{ "question": { "kind": "shape-count", "answer_type": "pairs", "max_answer_len": 16,
                "image_width": 480, "image_height": 240,
                "labels_zh": ["实心三角形", "空心圆形"], "labels_en": ["filled triangles", "hollow circles"],
                "min": 1, "max": 6,
                "prompt_zh": "数一数：图中有多少个实心三角形、多少个空心圆形？",
                "prompt_en": "Count the filled triangles and the hollow circles.",
                "generator": "shape-count-v1", "generator_version": "v1",
                "media": "image", "image_mime": "image/png", "image_base64": "iVBORw0KG…" } }
```
`answer_type` 是**输入形态**，不是判定依据：`text` 是字符输入，`cells` 是格子点选，
`pairs` 是两个数字输入。三种最终都归一成一个字符串提交：
`text` 用 6/8 字符串；`cells` 用 1-based 格子序号的排序逗号串（`3,7`）；
`pairs` 用两个十进制数的逗号串（`4,2`）。服务端的 `NormalizeAnswer` 是唯一裁决者，
widget 只是把它渲染成合适的控件（§8.7.4、§11）。`text-image-v3` 的题面额外带 `"tolerant":1`，
表示服务端接受最多一个字符替换；v1/v2 不带这个字段。

给不出题时（题池空、或站点配的生成器本进程没登记）是**可重试的资源状态**，不是验证失败：
`503` + `Retry-After: 2` + `{ "error-codes": ["challenge-unavailable"] }`，且响应里**没有** `pow`/`token`。
**绝不静默降级成 PoW-only**——那会让站点的策略声明在容量不足时悄悄失效（§5.4 不变量 5）。

`POST /api/redeem` — 请求（≤4KB）
```json
{ "token": "v1.6f2a1c9b8d0e4a71....", "pow_nonce": "98765432" }
```
`pow_nonce`：ASCII 十进制，长度 1–16，值 ≤1e12。成功 `200`：
```json
{ "response": "v1.6f2a1c9b8d0e4a71.<b64url(resp payload)>.<b64url(tag)>", "expires_in": 120 }
```
**阶段 3A 增加 `answer` 字段**（`pow_question` 站点必填；`pow_only` 站点带了它也不参与判定——
`pow_only` 根本不进入判分分支，这个字段在那条路径上是**被忽略**的，服务端不会因为"多带了一个 answer"而报错，
但也绝不会因为它而改变结论。别把"必填"读成"pow_only 必须为空否则拒绝"，实现里没有这条判定）：
```json
{ "token": "v1.6f2a...", "pow_nonce": "98765432", "answer": "19" }
```
`answer` 有两层上限，作用不同：传输上限 `len ≤64`（畸形与垃圾输入在进入任何判定前就挡掉），
语义上限由生成器的 `NormalizeAnswer` 裁决（裁首尾 ASCII 空白、单个 `±` 前缀、仅 ASCII 数字、剥前导零、≤12 位；
`1e3`/`0x10`/`١٢٣`/内部空白一律**畸形**）。`question.max_answer_len` 只是给客户端的输入提示，**不是**服务端判定依据。

`/api/redeem` 的判定顺序是**契约的一部分**，不是实现细节（§5.4 的"一次有效作答"依赖它）：
token 验签 → 站点存在且启用且 `svid` 一致 → 站点/IP 限流 → 来源与签发一致 → 库内 challenge 交叉核对
（`sid`/`bits`/`seed`/`mode`/`gen`/`status`/`expires_at`）→ 答案**文法** → PoW → 终结性作答。
因此：**畸形请求（含答案文法不合法）、来源不符都不消耗那道题**（同一 token 仍可修正后重试）；
**PoW 解错**也不算"作答"，但沿用阶段 1 的失败计数——累计 3 次即作废该 challenge，
此时它绑定的那道题**不会回到池里**（不变量 2），而是随题目 TTL 到期被清理：宁浪费一条容量，也不让同一题面面对第二个挑战者。
**文法正确但数值错** ⇒ 同一事务里 `challenges.status='invalid'` + `pool.status='terminal'` 并提交，
之后同一 token 的第二次提交只会得到 `invalid-challenge`。一题一次机会，答错不能刷。
答案错与 PoW 错对外是**同一个** `invalid-challenge`：不透露"是哪一层失败"（红线 8）。

失败 `400/429/503`：`{ "error-codes": ["invalid-challenge"] }` —— 这两个 `/api/*` 端点**不是** reCAPTCHA 契约面，
使用自有且有限的码集：`invalid-request` / `invalid-challenge`（含篡改、过期、已消费、尝试次数耗尽，**不做区分**） /
`rate-limited` / **`challenge-unavailable`（3A 新增，可重试，随 503）**。
阶段 1 文档写过的 `401` 从未出现（复核后按实际实现记录：`/api/*` 只用 400/429/503）。

`POST /siteverify` — 请求（`application/x-www-form-urlencoded` 或 `application/json`）
```json
{ "secret": "sk_live_9f3c...", "response": "v1.6f2a...", "remoteip": "203.0.113.7", "hostname": "app.example.com" }
```
成功 `200`
```json
{ "success": true, "challenge_ts": "2026-10-17T08:12:44Z", "hostname": "app.example.com" }
```
失败 `200`（reCAPTCHA 失败同样返回 HTTP 200）
```json
{ "success": false, "error-codes": ["timeout-or-duplicate"] }
```
错误码只用 reCAPTCHA 已定义的族：`missing-input-secret` / `invalid-input-secret` / `missing-input-response` /
`invalid-input-response` / `bad-request` / `timeout-or-duplicate`；内部失败原因只进日志（§14-7）。
**唯一的扩展是 `internal-error`**：仅在服务端自身故障（库不可用等）时随 **HTTP 5xx** 返回，业务方必须按"验证未完成"处理，
不得当作"验证失败"放行。`/siteverify` 的判定类失败一律 HTTP 200（reCAPTCHA 语义），包含请求体超限这类形态错误。
字段语义与限制，**不声称无条件兼容所有现成 SDK**（v2/v3 的额外参数如 `response` 长度、`origin`、`token` 校验均未实现）：
- `secret`：先做摘要比对再碰任何状态；**secret 不匹配时不得消费该站点外的合法 token**（先鉴站点、再消费，测试覆盖）。
- `response`：必须是本 `secret` 所属站点签发的 token（`sid` 一致性）。
- `remoteip`：**阶段 1 忽略**（不参与判定、不写日志）。只有显式开启 trust-proxy 且连接来自受信代理网段时才采信，见 §12。
- `hostname`：阶段 1 曾"仅回显调用方自报值"；**阶段 2 起改为服务端裁决**——取自签发时绑定的规范化来源（§5.3、§10.3），
  调用方自报值不采信也不回显，无绑定来源时该字段整体缺省。
- `challenge_ts`：challenge 创建时间（来自 `challenges.created_at`，非客户端上报）。
- 不返回 `scores` 等扩展字段以维持兼容；业务方需分数须走内部 `/api/admin/verdicts`（阶段 5，可选）。

### 10.2 管理 API（阶段 2 实现，`Authorization: Bearer <admin_key>`）

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/api/admin/whoami` | 当前管理凭据的公开 `key_id` |
| GET/POST | `/api/admin/sites` | 列表（`?after_id=&limit=`，上限 500）/ 创建（**仅此响应**返回一次性 `secret`） |
| GET/PUT/DELETE | `/api/admin/sites/{id}` | 详情 / 部分更新（指针字段，缺省=不改）/ **软吊销** |
| POST | `/api/admin/sites/{id}/enable` \| `/disable` | 启停（各提升一次站点版本） |
| GET/PUT | `/api/admin/sites/{id}/origins` | 允许来源列表 / 全量替换（非法条目整批拒绝） |
| GET/POST | `/api/admin/sites/{id}/secrets` | 列出凭据元数据 / 新增一把（返回一次性明文） |
| POST | `/api/admin/sites/{id}/secrets/rotate` | 建新 + 撤旧：`transition_sec=0` 立即撤销，`>0` 为过渡期（≤86400） |
| POST | `/api/admin/sites/{id}/secrets/{key_id}/revoke` | 撤销指定凭据（同样可带过渡期） |
| GET/POST | `/api/admin/admin-keys` | 列出管理凭据元数据 / 新增一把 |
| POST | `/api/admin/admin-keys/{key_id}/revoke` | 撤销管理凭据（自撤销时返回 `warning:"self-revoked"`） |
| GET | `/api/admin/audit?before_id=&limit=` | 审计倒序游标分页（上限 500） |
| GET | `/api/admin/generators` | **3A**：本进程登记的题型（`id`/`version`/`media`/`note`）+ `max_difficulty`；**3B** 追加 `experimental` 与 `production_ready`（两个题型现在都是 `true`/`false`） |
| GET | `/api/admin/pool` | **3A**：按 `(generator, version, difficulty)` 分桶的 `ready`/`assigned`/`terminal`/`total` 计数 + worker 观测；**3B** 每桶追加 `ready_asset_bytes`/`total_asset_bytes`，顶层追加 `image.{asset_bytes,budget_bytes,per_question_max_bytes}` |

尚未实现（阶段 5）：`/api/admin/sites/{id}/generators`（多题型权重）、`/api/admin/stats`、master key 的 `kid` 轮换登记接口。

**4A 给公开面加了三条收紧/新增**（都有测试）：

- 两条只读静态路由（`/widget/v1.js`、`/widget/v1.worker.js`）：资源名走**白名单常量**，
  没有目录、没有按参数拼路径；`Content-Type` 由我们自己写（**不用** `http.ServeContent`，
  它在缺 `/etc/mime.types` 的机器上会嗅探成 `text/plain`，配 `nosniff` 直接让脚本无法执行）。
- **`site_key` 一致性**：CORS 预检看不见请求体，只能从 URL（或 `X-Epocha-Site-Key`）认站点。
  于是规定 `/api/challenge` 的 URL 与请求体、`/api/redeem` 的 URL 与 **token 绑定的站点**必须一致，
  不一致一律 400 `invalid-request`；同一参数出现多个不同值也算冲突。
  检查排在**任何状态消费之前**：被拒的 redeem 不会消耗唯一一次作答机会（有测试钉住）。
  不带 URL 参数的老客户端（curl / devclient / 各阶段脚本）行为不变。

**阶段 3A 的模式与生成器配置**（写在这三个字段上：`verify_mode`/`generator_id`/`difficulty`，创建与部分更新都走同一校验）：
- `pow_only` 必须不带 `generator_id`；`pow_question` 必须带**本进程已登记**的 `generator_id` 与 1..`max_difficulty` 的难度档。
  校验在**写库之前**（`checkModePolicy`）：库里存一个没登记的 generator_id，等于给站点配了一条只会返回 503 的策略，
  那是运维事故而不是运行时兜底。未知模式串一律 `400 bad-request`。
- `GET /api/admin/pool` 的响应只有计数、时间戳与题型标识：**没有** `payload_json`、没有 `verifier_blob`、
  没有答案、没有内部随机数、没有可用 secret（红线 5/11）。worker 计数缺失时该键给出 `{"enabled":false}`，
  而不是伪装成"0 条已生成"。
- **这两个只读接口不写审计**：`/api/challenge` 每次都会消耗一条题池行，若为统计给公开查询挂一条审计写入，
  等于让公开流量把审计表打满（阶段 3A 明确不做，见下方"审计边界"）。
- `production_ready` 与 `note` 是刻意的自我标记：`arithmetic-baseline-v1` 在这里**如实写明**"题面可被脚本直接求值，
  不抗自动解题，仅用于验证题池与提交生命周期"，让运维在管理面就看到基线题型的定位，而不是只写在文档里。
- **3B 的自我标记默认拒绝**（`generatorLifecycle`）：没有登记结论的题型一律报
  `experimental:true` + `production_ready:false`，于是"忘了写"不会变成"看起来可用"。
  `text-image-v1` 的 `note` 明写"图片中可见字符本来就要给答题者看，不具备密码学保密性；
  抗 OCR/抗多模态能力**未经**评测"。**未经评测不得改成 `production_ready:true`；评测通过也只证明"测过"，
  不等于生产安全认证**（ADR-002/ADR-006）。
- 生成器的启用/禁用是**进程级**的（`EPOCHA_GENERATORS` 白名单，§16 配置面）：禁用后不再为该题型备题，
  指向它的站点得到 503（不降级）；**已经签发的题目**判分时找不到生成器 ⇒ 同样 503 且**不消耗作答机会**
  （状态不变，重新启用后仍可完成）。这套语义有测试（`TestDisabledGeneratorIssuesNothingButKeepsLedger`）。
- 字节观测只给聚合整数：`/api/admin/pool` 的实现里没有任何一处把 `asset_blob` 读进 Go（§5.5 纪律 2）。

`DELETE /sites/{id}` 是**软吊销**（停用 + 提升版本），不删任何账本行——已消费的 response 行正是"重放必失败"的证据。

管理面的结构隔离与错误族：
- 管理路由**只挂在独立的 admin 监听器**上（`--admin-listen` / `EPOCHA_ADMIN_LISTEN`，默认 `127.0.0.1:8081`）。
  公共监听器的路由表里根本没有这些路径，所以从公网打过来得到的是 **404 而不是 401**——不是"有路由但鉴权失败"。
- 管理 API 有自己的有限码集：`unauthorized` / `forbidden` / `bad-request` / `not-found` / `rate-limited` / `internal-error`，
  与公开 `/api/*` 码集和 `/siteverify` 的 reCAPTCHA 族都不同族。鉴权失败按来源地址计数，窗口内超限转 429 + `Retry-After`。
- 管理响应一律 `Cache-Control: no-store`，且**不带任何 CORS 头**（本阶段不做 Cookie 会话，跨源脚本拿不到 Bearer 头）。
- 每个**写**操作落一条 `audit`（actor = 管理凭据的公开 `key_id`，action，target，success）；只读接口不落审计。
  审计写失败就不对外声称操作成功。`detail` 列本阶段不写入任何请求体内容；不记 secret、不记 Bearer 值、不记答案。
- 输入校验：请求体 `http.MaxBytesReader` 64KB、字段长度上限、枚举与数值范围上限、分页上限；未知字段拒绝。

### 10.3 浏览器来源策略与 CORS（阶段 2）

允许来源存放在 `site_origins`，比较前一律规范化为 `scheme://host[:非默认端口]`：
scheme 只允许 `http`/`https`，host 小写化、IPv6 保留方括号形式、默认端口写入时被剥掉，
拒绝路径/查询/片段/用户信息/空标签/裸单标签主机名（`localhost` 例外，供开发环境显式放行）。
**第一版不支持通配符，不自动放行子域，绝不做字符串后缀匹配**：`example.com` 不涵盖 `app.example.com`，
`evil-example.com` 与 `example.com.evil.net` 都不算 `example.com` 的来源。

| 情形 | 行为 |
|---|---|
| Origin 在列表内 | 放行；响应挂 `Access-Control-Allow-Origin: <该来源>` + `Vary: Origin` + `Access-Control-Allow-Credentials: false` |
| Origin 不在列表内 / 畸形 / `null` | `/api/challenge`、`/api/redeem` 一律 400 `invalid-request`，且**不出现任何** `Access-Control-*` 头 |
| 无 Origin 头 | 只有 `allow_missing_origin=1` 的站点放行（演示客户端路径）；默认拒绝 |
| `OPTIONS` 预检 | 只应答预检（204 + `Allow`/`Allow-Methods`/`Allow-Headers`/`Max-Age`），**不签发、不消费、不落库**；未知或停用站点、非法方法或头 → 403 |
| `/siteverify` | 服务端接口，**不挂 CORS 头**，不允许任意页面在浏览器里读验证结果 |

必须写清楚的定位：**CORS 与来源白名单都不是机器人身份认证**。它们约束的是"哪个浏览器来源可以调用/读到这个站点的产物"，
前提是调用方是真实浏览器；非浏览器客户端绕过 CORS 直接构造请求，Origin 想写什么写什么。
因此本层的价值是站点隔离与来源可审计（配合 §5.3 的 `hostname` 语义），不是"证明对端是人"。

阶段 2 起 `/siteverify` 的 `hostname` 不再采信调用方自报值，改为服务端裁决（来源与保证范围见 §5.3）。
`remoteip` 仍然只在 trust-proxy 显式开启且连接来自受信网段时才有意义（§12）。

## 11. Widget（阶段 4）

当前交付是 `web/widget/v1.js` + `web/widget/v1.worker.js` 的原生 JS API：
`EpoCHA.render/getResponse/reset/refresh/destroy`。widget 通过版本化公共 URL 提供，Shadow DOM 只用于组件样式与内部 UI；表单隐藏字段必须落在 light DOM，见 `docs/WIDGET.md`。

widget 最初只支持 `text-image-v1` 图片题；2026-11-05 起支持 §8.7 的三种新题型（见下表）。
管理面默认的 `pow_only` 站点不会下发题目，图片题仍保持 `experimental:true`、`production_ready:false`。非视觉等价通道、`fallback_base`/多端点、无 JS 服务端表单和完整辅助技术验证仍是未交付/未验收项目，不能把 PoW-only 降级写成等价无障碍路径。

**2026-11-05（ADR-024）**：widget 除 `text-image-v1/v2/v3` 外还支持 `shape-grid-v1` 与 `shape-count-v1`，
按服务端 `answer_type` 选择输入控件：

| `answer_type` | 控件 | 提交串 |
|---|---|---|
| `text` | 文本输入，`letter-spacing` 提升可读性 | 规范化后的字符答案 |
| `cells` | 图片上覆盖的 9 个可聚焦按钮（百分比定位、随图片缩放、`aria-pressed`、键盘可切换） | 1-based 格子序号排序逗号串（`3,7`） |
| `pairs` | 两个带标签的数字输入（`type=number`、`inputmode=numeric`） | `a,b` |

三种控件最终都写入同一个 `answer` 字符串并按现有 `/api/redeem` 提交；widget 不解释答案、不做判定，
服务端仍是唯一权威。`cells` 的按钮只覆盖服务端声明并校验过的网格几何（`grid_*` 字段），
不按图片内容猜测位置；`pairs` 的标签取自题面的 `labels_zh/labels_en`，只进 `textContent`。

已实现并受 Node/部分真实浏览器用例约束的工程边界包括：请求/响应超时、世代隔离、reset/refresh/destroy 取消迟到回调、重复兑换保护、过期清空 token、CSP/Worker 两条资源路径、键盘焦点和 `aria-live`。这些工程行为不等于真人可读性、抗 OCR/AI 或真机辅助技术效果认证。

规划目标（不代表当前 API）：非视觉等价题型、可信备用 endpoint、JS 不可用时的服务端表单路径、更多题型与移动端交互；实现前必须另行补测试、设计决策和真实验收，不得让旧草稿接口继续作为接入文档。


## 12. 部署拓扑

**主节点（必需，VPS 1C1G）**
```
systemd 单元 → epocha server --listen :8080 --admin-listen 127.0.0.1:8081 --db /var/lib/epocha/epocha.db ...
                   （管理监听器与公共监听器分离；管理路由只存在于前者）
nginx/caddy: TLS 终止 + client_max_body_size 8k + X-Forwarded-For 处理 + 8081 绝不对外暴露（/api/admin 仅内网）
ufw: 仅放行 443（+ CF IP 段），8080 不对外
swap: 512M + vm.swappiness=10；GOMEMLIMIT=56MiB GOMAXPROCS=1
```
备份：~~`sqlite3 .backup` 每日 + WAL checkpoint~~ → **阶段 5A 更正**（ADR-014）：`sqlite3` 是外部二进制，
与本仓库"服务与运维能力都用纯 Go"的约束冲突；主方案改为 `epocha-ops backup`，走 `VACUUM INTO` 的整库一致性快照
（WAL 内容进快照，产物是单个无 `-wal` 的库）+ 自描述 manifest（版本/schema/哈希/计数/pepper 指纹）。
恢复是**保守恢复**：产物是新文件、站点全部禁用、凭据全部撤销、response 与题池清空、装入新的管理凭据，
用新的主密钥启动且不设 previous。详见 `docs/BACKUP-RESTORE.md`。恢复演练写入 `deploy/docker/README.md` 与 `docs/DEPLOYMENT.md`。

**运行互斥（阶段 5B，ADR-015）**："这个数据目录现在能不能被写"的判据是数据目录上的 `flock` 排他锁
（`internal/store/lock.go`），**不是**管理端口的可达性。服务在 `store.Open`（含迁移）之前取得、在 `db.Close`
之后释放；`admin-recover` 与 `restore` 取同一把锁（分别按 `--db` 与 `--out-dir`），拿不到就非阻塞拒绝。
`backup`/`verify` **不**取锁——在线一致性快照的正确语义允许与服务并发，强制停服只会让按频率备份变成
没人愿意做的事。锁文件从不删除（删了会把两个进程分到两个 inode，`flock` 就此静默失效），也不是 PID 文件。
它是建议锁：同等文件权限下绕开本仓库代码直接开库的程序不受它阻止，这一点写进文档而不是脚注。

**5A 的容器形状（与上面的 systemd 形状等价，不是另一套语义）**：`deploy/docker/` 的两阶段镜像
（`golang:1.24-alpine` 编译 → `FROM scratch` 运行时，`CGO_ENABLED=0`、`GOTOOLCHAIN=local`、数字 uid 非 root、
`read_only` 根、`cap_drop ALL`、`no-new-privileges`、`pids`/`memory`/`memswap`/`cpus` 硬限额、
`HEALTHCHECK` 用镜像内的 `epocha-ops check --path /readyz`）。
compose **只**发布 `127.0.0.1:8080`，管理口 8081 不发布——容器内 loopback 与宿主 loopback 不互通，
所以管理操作的受控路径是容器内 `epocha-ops admin`（只允许 loopback 目标、只允许 `/api/admin/*`）。
密钥走 `*_FILE` + `:ro` 挂载；数据/密钥/备份是三个分开挂载的目录。反代主示例是 Nginx
（`deploy/docker/nginx.conf.example`：不回传管理路由、动态端点 `no-store`、widget 透传后端缓存语义、
`client_max_body_size 8k`、访问日志不记 query 与体、`X-Forwarded-For` 覆写、默认不转发 `CF-Connecting-IP`）。

**Cloudflare 边缘（延后，阶段 5 之后评估；不属于阶段 1）**
- 事实更正（ADR-001）：Cloudflare **免费计划可以使用 SQLite-backed Durable Objects**（官方定价页：Workers Free 只能创建/访问
  SQLite-backed DO；DO 文档：Free 与 Paid 均可用；免费配额约为 10 万请求/天、13,000 GB-s/天、SQL 读 5000 万行/天、
  SQL 写 10 万行/天、存储 5 GB —— **使用时必须重新核对当日文档，本节数字只是记录时点**）。
  因此"边缘不能持状态"是错的；本版不这么做是**工程选择**：故障域与备份边界单一、写配额成本形状可控、fail-closed 语义更易证明。
- Worker 职责（若实施）：`/widget/v1.js` 与素材的缓存；可选的 PoW 参数签发（用主密钥派生的 `chal` 密钥，不落状态）。
- 权威状态（challenge 消费、response 消费、题池、统计）**默认留在 VPS 源站**。若将来改用 SQLite-backed DO 承载其中任一项，
  必须先做一次可行性验证（DO 内的原子条件更新语义 + 事务边界 + 与 §5 四条保证的对应关系），并在此处记录结论。
- 免费额度形状 `[估算]`：10 万请求/天 ÷ 每次验证约 3 个请求（challenge + asset + redeem）≈ 3.3 万次验证/天；
  这**不是**"边缘不可用"的论据，只是容量规划输入。
- 降级路径（failback）：Worker 回源超时 2s `[目标]` → 返回 `{"error":"edge_unavailable"}`，widget 按 §11 切 `fallback_base` 直连源站；
  源站不可用时**验证失败**（安全优先：宁可显示"暂时无法验证"，不做静默放行）。管理后台与 `/siteverify` 永不走边缘。
- 源站保护：开启 CF 代理时同时启用 IP 白名单（只放行 CF 段）并置 `trust-proxy=true`；否则一律以真实连接地址为准（§9、§14-5）。

## 13. 资源与性能预算（1 vCPU / 1GB / 50GB）

阶段 1 时本表全是 `[目标]`/`[估算]`；**阶段 3A/3B/4A/4B/5A 已逐行填入真实采样**（每行都注明机器、日期与负载形状）。
读表纪律不变：`make loadtest` / 验收脚本给出的数字必须注明形状才能用于选型，
而"实测"列里的每一项都仍是个别采样，不是稳态结论。

| 项 | 目标/估算 | 实测 | 守护方式 |
|---|---|---|---|
| 常驻 RSS | `[目标]` <100MB（Go 基线 + 字体光栅 + SQLite page cache + 池缓冲，阶段 3 后才有光栅项） | `[实测]` **真实服务进程**（`cmd/server` 二进制、`-trimpath -ldflags -s -w`）在"池里 200 道 ready 题 + 20 次完整 challenge→redeem→siteverify 闭环"下 `/proc/<pid>/status` 的 **VmRSS = VmHWM = 15,488 kB（15.1MB）、Threads=9**（2026-10-17，linux/arm64，go1.24.5）。这是**功能闭环形状**的采样、不是持续负载下的稳态，也没有压过 20000 行满载；基准确另见测试进程 VmRSS 9.6MB(100 题)/13.1MB(1000 题)，**含测试框架、不得混引** | `GOMEMLIMIT`；测试读 `/proc/self/status` 断言峰值 |
| 二进制体积 | `[目标]` ≤35MB（含 modernc/sqlite） | `[实测]` 9.44MiB（9,896,228 字节；2026-10-17，linux/arm64，go1.24.5，`-trimpath -ldflags "-s -w"`，含 modernc SQLite 驱动与**三条**内嵌迁移）。同一天用**完全相同的 flag** 把 HEAD（阶段 2）与工作树（阶段 3A）各构建一次，两者字节数相同 —— 体积未随 3A 的新代码增长，这是实测对比；**原因未做符号级分析，本文不解释** | `make size` |
| 单题数据体积 | `[目标]` payload ≤8192B / verifier ≤4096B（ADR-028；payload 由库内 CHECK 强制，verifier 由 Go 常量强制） | `[实测]` `arithmetic-baseline-v1` 较大块 **62–63 字节**（verifier 恒为 16B nonce + 32B HMAC = 48B）；库内**净增 ≈432 字节/题**（1000 题与 100 题两次 checkpoint 后大小之差 ÷ 900 行；SQLite 4KB 页填充率决定了它远大于裸字段和）。**该 432B 是旧上限下的实测，不是新预算的满载测量** | `store.MaxPayloadBytes`/`MaxVerifierBytes` + 有测试；0005 迁移重建 CHECK |
| 新题型单题体积（ADR-024） | — | `[实测]` 本地渲染样本：`shape-grid-v1` PNG 5.1–7.0KB、`shape-count-v1` 2.9–6.4KB、`text-image-v3` 11.7–19.0KB；v3 逐字 verifier **208B**（仍 <512B）。这是**样本**不是稳态分布，也未压满题池 | 生成器测试 + 本地渲染 |
| 有限批次生成耗时 | — | `[实测]` 补 100 题：纯生成+写入 **29ms**（0.29ms/题）；补 1000 题：**339–391ms**（0.34–0.39ms/题）。总墙钟 1.03s / 10.3s 里**主动让出 CPU 占 1s / 10s**（`YieldEvery=10`/`YieldPause=100ms`，是设计意图不是开销）。测量条件：本机 4 核可见但 `BatchPerTick=n` 单轮补满、`t.TempDir()` 独立库、2026-10-17 | `BenchmarkPoolFill`（需显式 `-bench`） |
| 题池磁盘占用 | `[目标]` 20000 行上限 | `[估算]` 按上行的 432 B/题 ⇒ 上限约 **8.4MB**（**外推值，未实测到 20000 行**；50GB 磁盘下不构成约束）。WAL 在写入进行中额外占 ~4.7MB（自动 checkpoint 阈值 1000 页） | `MaxRows` 同语句判定 |
| widget 体积 | `gzip [目标]` <30KB（`make widget-size`） | `[实测 2026-11-12]` **v1.js 原文 110,465 B / gzip 28,968 B**；交互模块 48,751 B / gzip 10,311 B（仅交互题型按需加载）；core+interactive gzip **39,279 B**；与 `v1.worker.js` 合计 gzip 33,678 B | 核心始终 <30KB；模块单独 <30KB、合计 <40KB，由 `release-check.mjs --widget-size` 守护 |
| 浏览器 PoW 吞吐 | 未设线（先测再说） | `[实测]` **422,833 次/秒**（Firefox 147 headless、linux/arm64、Dedicated Worker 内纯 JS SHA-256） | bits=12 求解 4–6ms；bits=20 提交→verified 五次采样 309–8,991ms（几何分布 + 负载差异，别当常量，也别据此抬难度） |
| 内嵌字体 | `[估算]` Latin 1–3MB；CJK 子集 4–6MB | `[实测]` **Go Regular 单档 148,672 字节**（`goregular.TTF`，3B 只用这一档，不子集化：字符集只有 32 个字形，收益远小于复杂度）；二进制因此从 9,896,228 → **10,813,732 字节** | `docs/NOTICE.md` + `make size` |
| 单题图片资产 | `[目标]` PNG ≤65,536B（库内 CHECK 强制） | `[实测]` d1 **1,126–2,557B**（均值 1,911）；d5 **5,648–6,902B**（均值 6,315）；base64 后 d5 一张约 8KB 响应（2026-10-18，60 样本） | `0004` CHECK + `store.MaxAssetBytes` + `TestAssetColumnChecksAreReal` |
| 视觉题生成成本 | `[目标]` 平均 CPU ≤15% | `[实测]` 纯生成 **1.81ms(d1) → 3.91ms(d5)/题**；生成+写库+落盘约 **6ms/题**（含进程启动）。视觉桶独立水位 8/24 + 独立让步 200ms + 全局 4MiB 字节预算，**不继承**算术桶的 50/100/20000 | `EPOCHA_POOL_IMAGE_*` + `TestImageBucketUsesOwnWatermarks` + `TestImageByteBudgetStopsGenerationWithoutWastingRender` |
| 图片池磁盘 | `[目标]` 全局图片字节预算 4MiB | `[实测]` 新库 + ≥20 条 ready 图片题 + 40 条算术题 + 20 次闭环 ⇒ `wal_checkpoint(TRUNCATE)` 后 **278,528B**（写入中主文件仅 4,096B + WAL 2,855,192B——**只 stat 主文件会得到假数**）。每行图片的净磁盘开销本轮**未单独隔离测量** | `ImageAssetBytes` + `TestPurgeReleasesImageBytes` |
| 服务进程驻留（含图片） | `[目标]` <100MB | `[实测]` **VmRSS 18,776 kB / VmHWM 18,904 kB / Threads 10**（`cmd/server` 二进制，形状 = 视觉桶 ≥20 条 + 20 次混合闭环 0 失败；2026-10-18）。仍是功能闭环形状采样，**不是**稳态，未压过预算满载 | `stage3b.sh` 第十三步 |
| 池预生成 CPU | `[目标]` 平均 ≤15%（补 10 题睡 100ms） | 待填（未做持续负载测量；0.35ms/题的纯成本意味着按默认 `BatchPerTick=20`/`PollInterval=2s` 稳态补题时 CPU 占用远低于该目标，但这是算术推论不是实测） | metric `pool_gen_ms` 直方图（阶段 5） |
| `/siteverify` p99 | `[目标]` <60ms | 待填（**阶段 5A 给出的是 p50/p95/max，不是 p99**：容器内限额下 `[实测]` p50 2.044ms / p95 6.430ms / max 54.217ms，150 次闭环、0 失败，2026-10-18 linux/arm64 1 CPU/128MiB） | 基准测试 `TestBenchmarkSiteVerify` + `make loadtest` |
| `/api/challenge` p99 | `[目标]` <120ms | 待填（同上：`[实测]` p50 2.162ms / p95 7.676ms / max 36.414ms） | 同上 |
| 混合吞吐 | `[目标]` ≥150 req/s 且无 `SQLITE_BUSY` | `[实测]` **262.18 闭环/秒 ≈ 787 请求/秒**（一次 challenge+redeem+siteverify 记为 3 个请求；`cmd/loadtest` 并发 4 / 150 次闭环 / 全部成功 / 0 超时 / 0 `SQLITE_BUSY`；容器限成 1 CPU + 128MiB + `GOMEMLIMIT=56MiB`，负载期间 `docker stats` CPU 40.00%、内存 7.754MiB，`OOMKilled=false`、`RestartCount=0`；2026-10-18）。**形状限制**：单一 pow_only 站点、`bits=12`、单一来源地址，且**必须**把全局 `EPOCHA_RATE_IP_RPM` 从默认 60 抬到 60000 才测得到容量——默认配置下 80 次闭环里 61 次是 429（那是限流器形状，不是服务端容量） | `make loadtest` / `docs/scripts/stage5a.sh` 第 10 步 |
| 容器内库与 WAL 体积 | — | `[实测]` 同一轮负载：主文件 147,456 → 438,272 字节，`-wal` 622,152 → 4,128,272 字节（2026-10-18）。再次印证"只 stat 主文件会得到假数" | `stage5a.sh` 记录两个数 |
| 图片补池（限额内） | `[目标]` 平均 CPU ≤15% | `[实测]` 有一个启用的 `pow_question` 视觉题站点时，1 CPU/128MiB 限额内补到 ready=20 行、图片字节 126,778；负载后 24 行 / 152,143 字节（2026-10-18）。**注意分桶来源**：`store.QuestionBuckets` 只取"启用的 pow_question 站点"，所以全部站点为 `pow_only` 时池恒为空——那不是故障，是语义 | `stage5a.sh` 第 10 步 |
| 服务内存（容器视角） | `[目标]` <100MB | `[实测]` 128MiB 硬限额下 `docker stats` 内存 7.918MiB（负载期间），无 OOMKill（2026-10-18）。这是**容器 cgroup 记账**，与 §13 上两行的 `/proc` VmRSS 口径不同，不可互相冒充 | `stage5a.sh` + `--memory` 硬限额 |
| 持续 5 分钟的稳态形状（5B） | 无历史目标值 | `[实测]` 1 CPU / 128MiB / `GOMEMLIMIT=56MiB`，**节流** 1.5 闭环/秒 × 450 次（并发 1，`--pace 660ms`）：450/450 成功、0 失败、0 超时；容器内存在 218s/249s/280s 采样为 8.117/8.297/8.625 MiB，CPU 0.39–0.57%，`OOMKilled=false`、`RestartCount=0`；题池 ready 0→24（图片字节 150,899）；库 139,264→450,560B、WAL 稳定在 4,128,272B；过期清理**实际触发**（一次 `清理 responses=89 challenges=90`）（2026-10-18）。**读法限制**：流量是 pow_only（图片题只观察到后台补池，没有解题器 ⇒ 不称"图片题吞吐"）；5 分钟不等于长期稳定性；PoW 由工具代算不代表真人解题耗时 | `docs/scripts/stage5b.sh` 第 4 步 |
| 运行互斥（写这条数据目录的权限） | 非性能项 | `[实测]` 容器与宿主 bind 同一数据目录时共享同一把 `flock`：服务在跑时宿主 `admin-recover` 被拒且库逐字节未变；第二个服务容器立即拒绝启动；停服后锁立即可取（锁文件未删）（2026-10-18） | `internal/store/lock.go` + `cmd/server`/`cmd/ops` 真实子进程测试 + `stage5b.sh` 第 2 步 |
| PoW 求解时延 | 未设线（先测再说） | `[实测]` 纯 JS SHA-256 在 worker 里 **422,833 次/秒**（Firefox 147 headless、linux/arm64、2026-10-18）：bits=12 求解 **4–6ms**、提交→verified 墙钟 **103–173ms**（三次采样）；bits=20 提交→verified 五次采样 **309 / 1,551 / 1,862 / 4,064 / 8,991 ms** —— 解的位置是几何分布 + 同机负载差异 ⇒ **bits=20 已经是"秒级到十秒级"**，别把单次采样当常量，也别据此继续往上抬难度 | 难度不能为了"看着快"往下降；`cmd/exampleapp` 的 8394 端口专测生产难度 |

**未测项与原因（3A 明确列出，不编造数值）**：
**稳态**服务进程 RSS 与库体积（已测的是"200 题 + 20 次闭环"的功能形状采样，见上表；
长期运行、真实流量形状与 20000 行满载仍未测——满载会把"池打满后行为"这个更该先测的性质掩盖掉）；
200 题时的库体积 `[实测]` **checkpoint 后 245,760 B（含 schema、站点、账本与 200 条题目）**，
写入进行中另含 `-wal` 3.8MB / `-shm` 32KB（自动 checkpoint 阈值 1000 页决定，不是净占用）；
claim 在双句柄并发下的**吞吐**上限（3A 测的是**正确性**——只有一个领取者、过期快照那一笔必须回滚，不是吞吐）；
`pool_gen_ms` 直方图（本版没有 metrics 导出面）。

## 14. 安全实现清单（review 时逐条对照）

1. 密钥/摘要/token 的比较一律 `crypto/subtle.ConstantTimeCompare`（阶段 1 已覆盖 secret 比对与 tag 比对）。
2. token 解析前必须有总长度上限；payload 段用 `io.LimitReader` + `DisallowUnknownFields`，禁止无限制读取。
3. 所有 nonce/seed/随机答案参数来自 `crypto/rand`；`math/rand` 仅用于图形抖动且每次独立播种（**3A 无图形抖动**，
   整个包树里没有 `math/rand`；每题熵读取另有 4096B 上限，防止畸形熵源把拒绝采样变成永久自旋，§8.3）。
4. 一次性消费的四条保证（ADR-004 / §5.1）：原子条件状态转移、同一 `Tx` 提交、检查 `RowsAffected == 1`、
   并发（含**两个独立 DB 句柄**）与**重启后重放**测试。`db.BeginTx` 已负责 `BEGIN`，**禁止在 `*sql.Tx` 上再执行 `BEGIN IMMEDIATE`**。
5. HTTP：请求体 `http.MaxBytesReader`（公开 4KB / siteverify 8KB / 管理 64KB）；`ReadHeaderTimeout/ReadTimeout/WriteTimeout/IdleTimeout` 全设；
   字段长度与格式硬上限；`Cache-Control: no-store` + `X-Content-Type-Options: nosniff` 于 API 响应；CORS 白名单（阶段 2，禁 `*` 与无条件回显 Origin）。
6. SQL 参数化 100%；动态列名走白名单。
7. 日志：结构化 slog，字段白名单；`secret`/`token`/`answer`/完整 payload 一律不出现。
   `make secret-scan` **按值扫描**：测试运行时注入已知高熵哨兵值（如 `SENTINEL_SECRET_<rand>`），
   断言日志、错误响应、audit detail 中不出现该值。按字段名 grep 会因 `admin_key`、`token` 等
   标识符产生假阳性，禁止这样实现。
8. 来源地址：默认只信真实连接地址；`X-Forwarded-For` / `CF-Connecting-IP` / `remoteip` 只有在 trust-proxy 显式开启
   且连接来自受信网段时才采信（阶段 1 已实现并有测试）。
9. 限流状态必须有硬上限与清理策略，避免攻击者用随机源地址撑爆内存（阶段 1 实现为有界固定窗口计数器）；
   窗口长度必须与配置名的单位一致（`*_RPM` = 每分钟，ADR-016）——"代码里的窗口比名字松一个数量级"是安全缺陷，不是精度问题。
10. admin（阶段 2）：admin_key 摘要存储、失败计数锁定（内存）、默认仅 localhost；审计表覆盖所有写操作。
11. 依赖：见 §16；引入任何新依赖需记录理由并由我确认。`GOTOOLCHAIN=local`，禁止构建过程隐式升级 Go。
12. 错误处理不吞异常；不 panic 出栈（中间件 recover + 通用文案）；安全关键状态**事务提交后才返回成功**（D12）。
13. **题目私有数据（3A）**：答案明文不落库、不下发、不落日志；`verifier_blob` 只由生成器解释；
    公开出网唯一通道是 `SanitizePublic`（白名单键，未知键报错）；管理观测面只给计数与题型元数据。
14. **降级即失效（3A）**：`pow_question` 在任何路径上都不能退化成 `pow_only`——池空 ⇒ 503 可重试、
    生成器未登记 ⇒ 503、绑定丢失 ⇒ `invalid-challenge`、题面不合法 ⇒ 作废该 challenge 并 503。
    四种情形都有测试，且都不返回 `pow`/`token`。

## 15. 目录骨架（现状与计划）

```
cmd/server/          单二进制入口（flag→config→store→http）
internal/api/        HTTP 层：路由、校验、限流中间件、错误码
internal/pow/        PoW 签发与验证、难度校准
internal/token/      域分隔密钥派生、签名/验签、kid 轮换
internal/store/      SQLite 打开、PRAGMA、迁移（0001/0002/0003）、原子条件消费（阶段 1）；
                     站点与凭据生命周期 sites.go/secrets.go、来源规范化 origins.go、审计 audit.go（阶段 2）；
                     题池与题目 ↔ challenge 绑定 pool.go、终结性作答 RedeemAttempt（阶段 3A）
internal/pool/       后台预生成 worker：水位迟滞、有界批次、CPU 让步、指数退避（阶段 3A 实现）；
                     领取与终态化在 store 的事务方法里，不在这里
internal/generator/  Generator 契约 + Registry + arithmetic-baseline-v1（3A）+ text-image-v1（3B：有界渲染参数、
                     字形包围盒几何校验、image.Gray → PNG）；共用的 verifier 构造在 generator.go
    internal/eval/     **开发期**评测底座：确定性测试熵源 + 与题型无关的评分器。cmd/server 不链接它（有证明）
    cmd/preview/       本地预览的演示后端（仅 loopback、持 secret 调 /siteverify、验证目标不可由浏览器指定）
    web/preview/       预览页单文件 HTML/CSS/JS + go:embed（放这里是因为 go:embed 只能嵌本包子树）
    cmd/evalgen/       离线样本生成（dev / holdout 两集，输出 eval-out/ 不入库）
    cmd/evalocr/       外部 OCR 适配器（exec argv、逐张计时、缺工具时退出码 3 = 未测）
    cmd/devseed/       受控题目注入（端到端验收用；需要主密钥，只操作本地库，不提供答案查询）
internal/risk/       有界固定窗口计数器（按分钟对齐，ADR-016）与来源地址裁决（阶段 1）；打分（阶段 3+）
internal/admin/      管理 API：鉴权/审计/站点与凭据生命周期（阶段 2，admin.go + handlers.go）；
                     模式与生成器配置校验、`/api/admin/generators` 与 `/api/admin/pool` 只读观测（阶段 3A）；页面属阶段 5
internal/config/     配置装载（flag + env + 站点文件，无 YAML 依赖）；3A 起含 6 个题池水位/节奏项与组合校验
    internal/store/lock.go 阶段 5B：数据目录的 flock 排他锁（服务在迁移前持有、关库后释放；
                     admin-recover/restore 取同一把；backup 明确不取）。永不删除锁文件、不是 PID 文件
internal/version/    构建信息（ldflags 注入）
cmd/devclient/       开发/验收客户端：真实暴力求解 PoW（阶段 1）、站点策略闭环与**独立求值**基线题答案（阶段 3A），
                     驱动 stage1.sh / stage2.sh / stage3a.sh
web/widget/          阶段 4A：**生产 widget**（v1.js + v1.worker.js + widget.go 内嵌与只读 handler）
cmd/exampleapp/      阶段 4A：**开发专用**示例业务后端（真实 /siteverify 闭环；默认 loopback；不链进 cmd/server）
docs/scripts/browser/ 阶段 4A：WebDriver BiDi 客户端与浏览器/向量测试（Node 内置能力，零第三方依赖）
cmd/blindtest/       阶段 4B：**开发期**人工可读性盲测记录服务（三段耗时分离、导出无凭据；不链进 cmd/server）
docs/scripts/ocr/    阶段 4B：隔离 OCR 评测容器与基线编排（tesseract 只在容器里跑，不是服务依赖）
deploy/docker/       阶段 5A：Dockerfile（两阶段，scratch 运行时）+ compose.yml + .env.example +
                     nginx.conf.example + README（索引）。仓库根的 `.dockerignore` 收窄构建上下文：
                     eval-out/（ground truth）、开发期命令、密钥与库文件一律不进上下文
cmd/ops/             阶段 5A：**运维入口**（backup / verify / restore / check / admin / version）。
                     它随生产镜像一起交付（镜像里没有 shell，健康检查与管理操作需要一个真能跑的命令），
                     但**不是**通用管理平台：每条子命令对应 DEPLOYMENT/BACKUP-RESTORE 里的一个具体步骤
    cmd/loadtest/    阶段 5A：**开发期**有界负载工具（分段时延与失败类别；并发与请求数都有硬上限）。
                     `cmd/server` 不链接它（stage4b.sh 的依赖隔离检查钉住）
deploy/cloudflare/   Worker + wrangler.toml（阶段 5 之后评估）
internal/store/migrations/  NNNN_*.sql（阶段 1 起）——偏离"顶层 migrations/"是有意的：
                     go:embed 只能嵌入本包子树，放在仓库根就无法编进单二进制
docs/                DESIGN.md、DECISIONS.md（含 ADR-015 运行互斥）、STAGE3A.md、STAGE3B.md（3B 数据流/配置/预览/评测方法与未测项）、
                     NOTICE.md（依赖与**字体**许可）、scripts/（stage1.sh…stage3b.sh）、reviews/
```
调整需说明理由；测试与被测文件同包同目录（`*_test.go`），集成测试用 `t.TempDir()` 建独立 DB，禁止依赖外部环境。

## 16. 依赖（阶段 1 已落地，详见 ADR-007）

| 依赖 | 版本 | 用途 | 许可 | 不选什么 |
|---|---|---|---|---|
| `modernc.org/sqlite` | **v1.46.1**（锁定） | 纯 Go SQLite 驱动，防重放账本持久化 | BSD-3-Clause（SQLite 本体 public domain） | `mattn/go-sqlite3`（需 cgo）；`v1.46.2+` 要求 Go≥1.25、`v1.60.x` 要求 Go≥1.26 → 会隐式升级工具链，禁止 |
| 标准库 `net/http` | — | 路由用 Go 1.22+ `ServeMux` 方法模式 | — | `gin`/`echo`/`chi`（无必要） |
| 标准库 `crypto/*` | — | HMAC-SHA256 签名与派生、`crypto/rand`、`crypto/subtle` | — | `golang.org/x/crypto/hkdf`（本设计每用途一把固定长度密钥，域分隔 HMAC 即可，见 §6） |
| 标准库 `image`、`image/png` | — | 画布与 PNG 编码（`image.Gray` 单通道，裸数据比 RGBA 小 4 倍） | — | 任何外部图像库/命令行工具 |
| `golang.org/x/image` | **v0.36.0**（锁定） | 3B 视觉题：`font/opentype` + `font/gofont/goregular`（内嵌 Go Regular）+ `draw`（逐字旋转合成） | BSD-3-Clause；字体本身是 Bigelow & Holmes 的 BSD-3（见 `docs/NOTICE.md`） | **不能升到 v0.37.0+**：其 `go` 指令要求 Go≥1.25，在 `GOTOOLCHAIN=local` 下等于隐式升级工具链（ADR-007）。也不引 `x/image/vector`（自己写十几行线段采样更好验证，且实测混用两种 Bresenham 约定会死循环） |
| 无 ORM | — | 手写 SQL 保证原子条件更新与可读性 | — | `gorm`/`sqlc` |

**阶段 2 没有引入任何新依赖**。阶段 3A 也没有。**阶段 3B 引入了 `golang.org/x/image` v0.36.0**
（用户在本轮明确批准，条件是锁版本、不触发工具链升级、核对字体许可）：直接 import 只有
`font/opentype`、`font/gofont/goregular`、`font`、`draw`、`math/f64`、`math/fixed` 六个子包。
传递依赖 `golang.org/x/text v0.34.0`（`font/sfnt` 的编码表需要，`go` 指令 1.24.0，兼容本机 go1.24.5）。
完整许可记录在 `docs/NOTICE.md`。除这两个之外仍**没有任何**第三方依赖。
`Makefile` 设 `GOTOOLCHAIN=local`：构建过程不得隐式下载或切换 Go 工具链。2026-11-02 用户授权升级后，本次固定 Go 1.27.3（ADR-020），现有依赖继续锁定；上表关于 Go 1.24.5 的限制是历史选型理由，不是新工具链能力声明。生产仍为 `CGO_ENABLED=0`，Linux race 验收的 C 编译器只属于测试工具，不进入运行镜像。

## 17. 开发阶段与验收标准

> 每阶段顺序固定为：**写测试（先红）→ 实现 → `make fmt-check vet race` → 更新文档 → commit → tag `stage-N-done`**。
> 带 `make` 目标的验收必须在 Linux/arm64 上**真实跑绿**；未实测的数字一律标 `[目标]`/`[估算]`（ADR-006）。
> 阶段内测试**不得保留 `t.Skip`**；未来阶段的占位测试可以保留，但必须在报告里单独列明、不计入验收。

**阶段 1 — PoW + token + 持久化防重放 + `/siteverify` 最小闭环（已实现）**
验收（全部由 `make stage1-check` 自动化，见 `docs/scripts/stage1.sh`）：
- 一个命令完成：生成临时密钥 → 启动临时服务 → `challenge` → **真实暴力求解 PoW** → `redeem` → `siteverify` → `"success": true`。
- 同一 challenge **第二次兑换失败**；同一 response **第二次验证失败**（`timeout-or-duplicate`）。
- **进程重启后**用重启前签发的 response 再验证 → 失败（证明状态在 SQLite 而非内存）。
- 脚本用 `set -euo pipefail`，`trap` 清理子进程与临时目录，退出码不被管道掩盖。
测试必须覆盖：① PoW 已知向量与错误解、bits 边界、客户端解格式校验；② token 篡改（逐字节）、过期、用途混淆（chal↔resp）、跨站点、未知 kid；
③ 同一 challenge N 路并发兑换恰好 1 个成功；④ 同一 response N 路并发验证恰好 1 个成功；
⑤ **两个独立 `*sql.DB` 句柄**争抢同一临时文件仍只有一个成功；⑥ 错误 `secret` **不消费**任何合法 token；
⑦ 请求体超限、畸形 JSON、伪造 `X-Forwarded-For`/`CF-Connecting-IP`（默认不采信，trust-proxy 开启且来源受信才采信）；
⑧ 缺主密钥/pepper 拒绝启动；⑨ 日志与错误响应不含本次注入的哨兵密钥值（按值扫描）；⑩ 响应 JSON key 集合与 §10.1 一致（含 `error-codes` 连字符）。

**阶段 2 — 多站点管理、密钥生命周期、来源策略、站点级配置与审计（已实现，2026-10-17）**
验收（`make stage2-check` 自动化，10 项）：增量迁移幂等且阶段 1 账本状态保留；创建站点仅一次性返回明文 secret，
之后详情/列表/凭据/审计/错误响应都不再返回，日志与库文件按值扫描也不含它；来源精确匹配（后缀/子域/端口/协议/`null` 全拒）；
`429 + Retry-After` 可按站点触发；停用站点使未消费 token 立即失效且重新启用不复活；两类轮换（立即撤销 / 有上限过渡期）都生效，
且**被撤销的凭据不消费合法 response**；重启后已撤销凭据仍失效、已消费状态仍不可重放、配置文件不得把旧值覆盖回库。

> 本文原先写的"Origin 不符 → `bad-request`"与 §10.1 自相矛盾（`/api/*` 只用自有码集）。
> 阶段 2 按 §10.1 实现：来源不符返回 **400 `invalid-request`**，`bad-request` 仍只属于 `/siteverify` 的 reCAPTCHA 族。
> 另外阶段 2 没有排序类参数，因此 §5 的"动态列名白名单"在本阶段没有落点（属阶段 5 统计接口）。

测试覆盖：admin Bearer 鉴权矩阵（缺失/错误/已撤销/非 Bearer 方案/超长/无登记凭据）、鉴权失败计数上限、
公共监听器无管理路由（404 而非 401）、站点 CRUD 与启停的重启持久化、部分更新的"缺省=不改"、
凭据数量与过渡期上限、跨站点 secret、签发快照语义（难度/TTL）与安全动作语义（停用/撤销）的分别验证、
来源规范化的 30 余条向量、预检不签发不消费、审计覆盖所有写操作 + 分页边界 + 不含凭据、
阶段 1 库的真实升级路径与"旧列不可用于认证"、清理与外键相容、验证请求与停用并发的提交顺序一致性。

**阶段 3A — 生成器契约 + 题池基础设施 + `arithmetic-baseline-v1` + 流程基线（已实现，2026-10-17）**
范围边界（与阶段 3B 分界）：3A 只做**工程基线**——可插拔生成器接口、有界预生成题池、原子领题、
challenge 绑定、单次答案提交，以及一条纯文本基线题型。**不含**视觉题型、渲染依赖、风险打分、自动淘汰、widget、
"用 LLM 生成题目"，且**不声称**基线题型抗 OCR/抗 AI（§8.4 明确它可被脚本 O(1) 解出）。
因此**不打 `stage-3-done`**，只打 `stage-3a-done`。

验收（`make stage3a-check` 自动化 10 项，见 `docs/scripts/stage3a.sh`）：
- 密钥运行时随机生成、DB 在临时目录、端口由内核分配（`:0`）；`trap` 只清理本脚本自己创建的进程与文件。
- 阶段 1/2 的站点在新库上仍是 `pow_only` 且流程不变（**不静默改变现有业务**）。
- 管理面配置 `pow_question` + 生成器 + 难度档；未登记的 generator_id 与"模式与生成器不匹配"必须 400。
- worker 关闭且池空 ⇒ **503 `challenge-unavailable` + `Retry-After`，且响应里没有 `pow`/`token`**（不是降级成 PoW-only）。
- 后台 worker 自动备题 ⇒ 池水位从 0 升到 `low_watermark` 后 challenge 恢复 200。
- 端到端**真实求解**：脚本自己解析公开 `task` 求值（独立于 `internal/generator` 的实现），
  **不读库里的答案、不新增答案调试接口、不硬编码固定解**。
- 错误答案 ⇒ `invalid-challenge`，且**同一 token 再提交仍然失败**（终结性由已提交的终态保证，不是靠内存）。
- 缺 `answer`、答案文法非法、PoW 解错 ⇒ 不终结该题（与上一条对照，证明两类语义不同）。
- **进程重启后**：终态 challenge 不可重放、已消费 response 仍 `timeout-or-duplicate`、
  `assigned`/`terminal` 题不再作为可领取题发出。
- 哨兵扫描：凭据与密钥材料**按值**扫（公开响应、管理响应、日志、**库文件原始字节**都不含站点 secret）；
  答案与私有 verifier 的 verifier **字节**只在集成测试里按值扫（测试能读库拿哨兵），
  脚本层**按字段名**扫（`"answer"`/`"verifier"`/`"seed"` 不得出现在日志里）——算术答案是小整数，
  按值扫日志会撞上 `dur_ms` 这类合法数值字段，那属于会把真问题掩盖掉的假阳性。

测试覆盖（`internal/generator` / `internal/pool` / `internal/store` / `internal/api`，**3A 无 `t.Skip`**）：
生成器侧——已知答案向量与独立求值器交叉验证（5 档 × 1000 题）、答案规范化接受/拒绝表、
公开 payload 不含私有字段（按字段名与按值双向断言）、`SanitizePublic` 未知键拒绝、熵源报错原样上抛、
注册表不回落、答案分布实测；池侧——低于水位补题、到 `MaxRows` 停、水位按桶、配置校验、
失败指数退避（并断言失败计数不爆表 = 无忙循环）、失败后可恢复、`Stop`/`Wait` 与 ctx 取消、未登记生成器不被替换；
库侧——一题只被一个 challenge 领走（**两个独立 `*sql.DB` 句柄争抢同一文件**，且**两个方向都断言**：
带绑定的行数与 `count(distinct challenge_id)` 都必须等于成功次数——只查"一个 challenge 绑多题"是半个不变量，
漏掉"同一题行被第二个事务改绑"那一半）、事务失败不留孤立绑定、
跨句柄的**过期读快照**去领题时必须失败而不是覆盖（`TestStaleSnapshotClaimFailsClosed`：实测形态是
`UPDATE` 拿到 `database is locked`，整笔事务回滚，被抢的题仍只属于先提交的那方）、
`assigned` 不回 `ready`、错误答案走已提交终态、清理顺序不破坏有效绑定与防重放记录、重启后终态保持；
API 侧——模式与生成器降级尝试被拒、答错终结、重复提交失败、畸形请求不消耗题、池空 503 形状、响应键集合。

**失败回归（不是"测试通过"的附注，是它的证据）**：把 `RedeemAttempt` 的"错误答案"分支临时改成
**返回 error**（等价于事务回滚 ⇒ 题目回 `ready`、challenge 回可重试），2026-10-17 实跑
`go test -count=1 ./internal/store/ ./internal/api/` 变红的**完整**名单是 7 条：
store 侧 `TestAssignedNeverReturnsToReady`、`TestRedeemAttemptWrongAnswerCommits`、`TestConcurrentRedeemMixedAnswers`；
api 侧 `TestWrongAnswerTerminatesChallenge`、`TestAnswerIsBoundToItsOwnChallenge`、
`TestDisableDoesNotReturnQuestionAndRestartKeepsState`、`TestFailedChallengeStaysFailedAfterRestart`。
随后恢复正确实现并复跑为绿；故意破坏的版本**没有提交**（`grep` 确认注入标记不在树里）。

**阶段 3B — 首个视觉生成器 + 图片资产接线 + 本地预览 + 可复现评测（已实现，2026-10-18）**
前置条件处理：字体许可已核（Go 字体是 BSD-3，非 OFL；`docs/NOTICE.md` 履行"二进制再分发须复制声明"）；
`golang.org/x/image` 经用户批准后引入并锁在 v0.36.0。素材方面**没有任何外部素材**——图片全部实时生成。

范围边界（与"下一步"分界）：3B 做**工程链路**，不做正式 widget、不做完整后台/容器/边缘、
**不做生产可用的结论**。`docs/scripts/stage3b.sh` 的 21 项工程检查全部真实跑绿（退出码 0；口径见下方**验收状态三态**：工程项只写 PASS/FAIL，外部条件不足写 BLOCKED，本轮没做写 NOT_RUN）：
依赖隔离（服务二进制不链接 `internal/eval` 与任何开发命令）、管理面题型登记与未知题型拒绝、
池空 503 不降级、注入受控图片题后 DTO 字段集合、base64 解码为合法灰度 PNG 且块类型仅 IHDR/IDAT/IEND、
客户端真实求解 PoW 的完整闭环、错误答案终结、畸形答案不消耗机会（与上一条构成对照）、
重启后终态与一次性、视觉桶用自己的水位而算术桶用自己的、全局图片字节预算先于水位生效、
`EPOCHA_GENERATORS` 禁用后不签发新题、`pow_only` 与 `arithmetic-baseline-v1` 两条旧路径回归、
凭据按值扫描（响应/日志/库文件）与私有字段按名扫描、预览拒绝非 loopback / 拒绝目标注入 /
secret 不进浏览器 / 演示后端真实验证成功、资源实测（PNG 分布、生成耗时、库与 WAL 体积、服务进程 RSS）。

**未达成项（如实记录，不算通过）**：
- **M4 每次尝试成本未测（BLOCKED）**：本机无离线 OCR（未获授权安装或下载模型），OCR 基线**未执行**；
  已交付适配器 + 有测试的评分器 + 固定输入输出格式。缺测**不得**被读成"题目抗机器能力强"；
  "缺工具时二进制退出码 3"验证的是**阻塞检测逻辑**，不是 OCR 效果（分层见 `docs/STAGE3B.md` §5.3：
  二进制 3 / `go run` 折叠成 1 / `make` 折叠成 2）。
- **人工可读性 NOT_RUN**：盲测页面与统计口径已交付，但没有真实参与者数据；agent 不代填。
- 3B 当时把"浏览器里输入→提交→成功"记为不可自动化（理由是没有 geckodriver/playwright），
  **这条结论是错的**：本机 Firefox 147 自带 WebDriver BiDi 端点，4A 已用真实交互补上（§17 阶段 4A）。
- 浏览器内"输入答案 → 提交 → 成功"这一击**未自动化**（无 geckodriver/playwright）：
  Firefox headless 已验证页面加载、脚本执行、预检 204 + 领题 200、以及真实 `image_base64` 可被浏览器
  渲染成可读图（这一步抓到一个 `?site_key=` 缺失导致预检 403 的真 bug），但截图不能点击/输入。
- §8.2 的 M3 门槛（3 次累计 >10% 不得启用）对本题型远低于阈值（2.79e-9），但**M3 低不代表安全**：
  真正的门槛是 M4，而它未测。因此 `production_ready:false` 保持不变。

**阶段 3C / 下一步（未开始）**：题型权重与多生成器选择、风险打分（第三层）、
分级淘汰与统计面、正式 widget（含非视觉等价通道）、OCR 与人工基线测量。
`rotate-v1` 的"144"这类数字在完成 M1–M4 测量前仍不得写成安全性论据。

**阶段 4A — Widget 初版 + 业务接入示例 + 浏览器闭环（已实现，2026-10-18）**
已交付并跑绿：`EpoCHA.render/getResponse/reset/refresh/destroy`、中英文、Shadow DOM 直挂（不用 iframe）、
状态机与并发语义（单实例单任务、换代丢弃晚到响应、重复点击不二次兑换、过期清空 token、
后台恢复复核 deadline、destroy 释放监听/计时器/worker/Blob URL、多实例隔离）、
Dedicated Worker 内的可取消 PoW（Blob 与同源文件两条路径都实现）、隐藏字段（light DOM，含 Shadow DOM
不参与表单提交这个坑）、`credentials:'omit'`、URL 上保留 `?site_key=`、服务端新增 site_key 一致性规则。
`make stage4a-check` 的 13 项工程检查退出码 0，其中**真实浏览器交互**（Firefox BiDi，零第三方驱动）覆盖：
显示图片→键盘输入→页面内 PoW→redeem→业务表单→后端写入、答错终结与换题、token 重放被拒、
多实例与隐藏字段互不覆盖、晚到响应不覆盖新题、reset/destroy、计算超时预算、高难度站点取消、
**过期清空 token 与隐藏字段**（专用 `response_ttl_sec=2` 站点，等真实 TTL 到期）、
CSP 禁 Worker 明确失败、键盘-only 完成一题、请求面与凭据面扫描。

**未达成项（如实记录，不算通过）**：
- **BLOCKED** 375px 无横向溢出：Firefox BiDi 不提供视口设置命令（node:test 记为 skip，不是通过）⇒ 需人工确认。
- **NOT_RUN** 人工可读性、以及"题型人工各过 5 次"（DESIGN §17 阶段 4 的原验收项）：无真实参与者数据。
- **未实现** 非视觉等价通道（音频等）：本阶段只有视觉通道；PoW-only 降级不是等效替代。
- **未实现** `fallback_base` 与多站点配置面、`aria-live` 的辅助技术实测、中英切换的真机验证。
- 因此本阶段只打 `stage-4a-done`，**不打** `stage-4-done`。

**阶段 4B — Widget 剩余验收项**
验收：题型人工各过 5 次；`make widget-size` 断言 gzip `<30KB [目标]`；键盘-only 完成一题；`aria-live` 播报经 DOM 断言；
移动端 375px 无横向溢出；中英切换；PoW 高难度无障碍通道可用；失败重试与 fallback endpoint 生效；
并首次给出浏览器侧 PoW 时延的 `[实测]` 数据（设备、日期）。无法自动化的项必须标注"人工验证"，**不得声称自动化通过**。

**阶段 5B — 运行互斥、恢复边界收敛与部署候选清单（本轮，2026-10-18）**
范围边界：**不新增产品功能**（无后台页面、无风险打分、无自动淘汰、无新题型、无 CF 边缘、无多节点），
只做"能不能安全地动这个数据目录"与"这一版到底测过哪条路径"两件事。
验收（`make stage5b-check` → `docs/scripts/stage5b.sh`，退出码 0/1/3 三态）：
- 锁的语义：非阻塞、崩溃后可重取、锁文件永不删除、`ErrLockHeld` 与 `ErrLockAccess` 分族；
  服务侧"迁移前持有、关库后释放"。
- 真实**子进程**测试（重新执行测试二进制按角色分派，按 pid 定向清理）：两个服务抢同一数据目录最多一个启动、
  服务与离线操作竞争不交叉修改、正常停止后可重启、`SIGKILL` 后锁可重取、陈旧锁文件不阻塞启动、
  两个 `admin-recover` 竞态恰好一个成功、拒绝路径不改凭据与业务数据也不留凭据文件。
- 真实**容器**演练：容器与宿主 bind 同一数据目录时共享同一把锁（宿主进程在服务运行时改不了库）、
  第二个服务容器立即拒绝启动、就地恢复（`--out-dir` 正在被用作数据目录）被拒而隔离恢复可成、
  在线备份在持锁期间照常成功（这条是反向断言，防止"顺手加锁"）。
- 恢复边界：凭据文件与库变更之间失败的明确出口写在 `BACKUP-RESTORE.md` §5；
  checksum / 数据库一致性 / 来源真实性 / 保密性四分在文档里分别标注，
  只有前两个存在 ⇒ 不得宣称抵御恶意替换。
- 候选清单：**第一版正式支持范围只有 B 路径**（宿主静态编译 + named build context 装配，ADR-017），
  A 路径（容器内编译）保持**可选、NOT_RUN**——这是缩小支持范围，不是把它写成通过。
  清单必须绑定**干净提交**（`git status --porcelain` 为空时构建；门禁第 3 步把这条变成硬判据——
  工作树非空 ⇒ 必需项 FAIL，因为"记录一下状态"在实测中出现过"清单是 dirty 而所有断言全绿"），含源码 commit、工作树状态、Go 版本、
  `GOOS/GOARCH/CGO_ENABLED`、编译参数与构建路径、两份进镜像二进制的 SHA-256、
  本地镜像 ID 与标签（**不**冒充 registry digest）、schema 版本、已测试的 uid/gid、未验证范围。
  清单作为**构建产物**落在忽略目录（`deploy/docker/.prebuilt/candidate-manifest.txt`），不入库：
  含当前 commit 的清单再提交会形成自引用循环；文档只指向固定的源码提交与标签。
- **启动配方实测**（`DEPLOYMENT` §4/§11 的形状）：在隔离部署根按文档命令真起一套，判据是
  `/readyz` 200、容器内管理命令可用、真实闭环成功、同 token 重放被拒、`--force-recreate` 后站点仍可用
  且**已消费 token 仍被拒**、`stop` 后宿主端口释放、且只定点清理本轮资源。
  另加两条静态判据：compose 与 `.env.example` 的变量名**双向**对账（取不到列表即 FAIL，不接受"空输入⇒无漂移"），
  以及相对绑定按项目目录解析（防止演练写进仓库目录）。
- 持续演练：1 CPU / 128MiB 限额、约 1.5 闭环/秒（`--pace` 节流）、5 分钟硬上限，
  分别记录 pow_only 流量、图片题后台补池、容器内存、DB/WAL 增长、题池水位、失败类别、
  以及**过期清理是否真的触发过**（看日志里有没有非零计数，不看"代码里有 purge 循环"）。
  抬限流只存在于测量实例，不写进 compose。
- **门禁结论按三类归属计算**（ADR-017）：必需项（受支持 B 路径的部署工程验收）决定退出码
  ——FAIL⇒1、BLOCKED/NOT_RUN⇒3、全 PASS⇒0；可选项（A 路径、amd64、UID 65534）如实输出但**不决定**退出码，
  且在结构上不能被写成 PASS（缺"实际执行证据"即拒绝）；超范围项（人工可读性、抗 OCR/抗 AI、难度档）
  只登记边界，永不打印 PASS。分类逻辑本身有 9 条负向自测（`--selftest-classify`）并被变异验证过。
  包装层实测：`make` 只会报 0 或 2（任何非 0 折叠成 2），因此配方打印脚本真实码，机器可读结论看
  `RESULT=` 与 `SCRIPT_RC=`。

**阶段 5 — 管理后台、统计与分级淘汰、Dockerfile 与部署文档；CF 边缘仅作评估项**
验收：管理页完成站点 CRUD/权重/池状态；淘汰规则按 ADR-006 分级（候选告警 + 人工确认，自动降权需多信号同时成立）且有测试；
`deploy/docker` 镜像非 root、`--db` 挂卷持久化、容器启动后 `make stage1-check` 仍通过；
`make loadtest` 在 1C1G 上给出 `[实测]` 吞吐/p99/RSS 并回填 §13（未达标要如实记录，不得写成达标）。
CF Worker/DO 仅在 §12 的重新评估结论为"做"时才实施，届时先补边缘原子性可行性验证。

**阶段 5A — 单节点部署包、密钥持久化、备份恢复与资源受限演练（本轮，2026-10-18）**
范围边界：**不含**管理后台页面、题型权重/风险打分/自动淘汰、非视觉通道、CF Worker、多节点、公网部署授权。
验收（`make stage5a-check` → `docs/scripts/stage5a.sh`；逐项三态与退出码契约见 `docs/STAGE5A.md`）：
- 交付：`deploy/docker/{Dockerfile,compose.yml,.env.example,nginx.conf.example,README.md}`、根 `.dockerignore`、
  `cmd/ops`、`cmd/loadtest`、`internal/store/deploy.go`（备份/恢复/`Ready`）、`internal/config/keyfile.go`（`*_FILE`）、
  `GET /readyz`、`docs/{DEPLOYMENT,BACKUP-RESTORE,STAGE5A}.md`。
- 镜像：只含 `epocha` 与 `epocha-ops` 两把静态二进制（用 `docker save` 的文件清单证明没有
  shell/curl/sqlite3/OCR/评测样本/ground truth/浏览器）；非 root；`read_only`；`cap_drop ALL`；
  `no-new-privileges`；`pids`/`memory`/`memswap`/`cpus` 硬限额**经 `docker inspect` 核实**；
  `HEALTHCHECK` 打 `/readyz` 而非 `/healthz`。
- 边界：宿主只出现 `127.0.0.1:<公共端口>`；管理口不发布；公共端口上 `/api/admin/*` 是 404。
- 密钥：缺文件/权限过宽/env 与 `_FILE` 同时设置 ⇒ 一律拒绝启动；日志与错误文案不带出密钥值与路径；
  重建容器不生成新 master/pepper。
- 持久化与旧快照语义：容器重建后已消费 response 仍不可重放、停用站点仍停用、被撤销的凭据与
  被撤销的管理凭据都不复活；恢复产物保守失效、旧 token 被拒、旧管理凭据被拒、人工启用后新闭环可走。
- 故障形状：数据目录/库不可写时不得返回验证成功（`/readyz` 503 或直接拒绝启动两种形状都可接受，
  但"照常服务"不可接受）；SIGTERM 后端口释放、无 OOMKill。
- 等价替换（记录在案，不假装成别的东西）：阶段 5 原文的"容器启动后 `make stage1-check` 仍通过"在 5A 里由
  **容器路径上的等价端到端断言**承担（领题→真解 PoW→兑换→`/siteverify` 成功、重放被拒、重启后仍不可重放、
  轮换/停用/撤销的持久化）。`stage1.sh` 本身按设计会自建密钥、自建 DB、自起实例，
  因此**不**把它指向已运行的容器去跑（那会让它的哨兵按值扫描与清理逻辑失真）。
- 资源：`cmd/loadtest` 在**限额内**给出 challenge/redeem/siteverify 的分段 `[实测]` 时延与吞吐、
  容器内存、库与 WAL 体积、失败类别、OOM 计数，并回填 §13（未达标如实记录）。
  表述纪律照旧：PoW 由浏览器求解，工具代算的那一段不得混进"服务端时延"或"用户体验"。
- 反代：真实 nginx 容器验证"不回传管理面 / 动态端点 no-store / widget 缓存透传 / 日志无 query 无凭据"。
  自签名测试证书**不等于**公网 TLS 已验收。

## 18. 已知风险与未决问题（跟踪中）

| # | 风险/问题 | 现状 | 处理方向 |
|---|---|---|---|
| R1 | SHA-256 前缀 PoW 对 GPU 友好，成本上限有限 | 默认 `bits=18 [估算]` | 动态难度 + memory-hard 协议位（启用前需 `[实测]` 验证侧成本，§7） |
| R2 | 多模态模型可解字符图（`[估算]` 60–90%，**非本专案测量**） | 接受，且此数字待阶段 3 基线替换 | 一次性 + 尝试次数封顶 + 成本层叠加；不指望题目本身 |
| R15 | widget 的可达性受页面 CSP 约束：`worker-src` 既不给 `blob:` 也不同源部署 worker 文件时，**验证无法完成**（组件给 `worker-blocked` 明确失败，不假装通过） | 两条 worker 路径都实现，接入文档给出 CSP 清单；不做"降级到主线程假装算完" | 接入方按 `docs/WIDGET.md` §6 配置；示例后端提供 blob / 'self' / 'none' 三种档位各自有测试 |
| R3 | `cjk-v1` 成语语料与 `rotate-v1` 底图的再分发许可 | **未确认**，现在是 **阶段 3B 的开工前置条件**（3A 是纯文本，不需要素材） | 自建/CC0/OFL；`docs/NOTICE.md` 清单 |
| R4 | 选择题答案空间仅 4（猜测通过率 25%，3 次尝试累计 ≤58%） | 这是题型的**主要弱点**，不能靠渲染混淆掩盖 | 阶段 3 用 M2/M3 正式量化；必要时改题型结构而非加噪声 |
| R5 | 边缘（Worker/DO）持状态会改变故障域与一致性模型 | 本版不在边缘放权威状态（工程选择，非平台限制，ADR-001） | 阶段 5 之后按 §12 的重新评估条件决定 |
| R6 | SQLite 单写者在 claim 高并发下的吞吐上限 `[估算]` | 未实测 | 阶段 5 压测给数字；超标则改内存队列 + 批量落库 |
| R7 | 无音频通道，纯视觉障碍用户体验 | 规划 PoW 高难度替代通道（阶段 4） | 阶段 4 评估音频（会引入编码依赖，暂不做） |
| R8 | widget 行为数据可伪造 | 设计为 ≤10% 弱权重 | 永不作为唯一 deny 依据（代码注释说明） |
| R9 | **§13 与 §7 的全部性能数字均未实测**，阶段 1 只做功能正确性 | 已按 `[目标]`/`[估算]` 标注 | 阶段 5 压测回填；在此之前不得据此做容量或选型决策 |
| R10 | 配额类外部事实会随平台变化（ADR-001 的教训） | 文档已改为"引用来源 + 使用时重新核对" | 任何平台限制写入文档时注明日期与出处 |
| R11 | `arithmetic-baseline-v1` 可被脚本 O(1) 解出（M4 ≈ 0）；d1 单点答案概率实测 ≈4.5%–4.8%（两次抽样，众数在 `0` 与 `14` 之间并列），3 次尝试累计 ≈13.5%–14.4%，已超 §8.2 的 10% 线 | **已知且刻意**：它是流程/生命周期的工程基线，不是生产题型。管理面用 `production_ready:false` + `note` 显式标记；把它当真实防线用属运维误配 | 3B 用视觉题型与基线测量替换；在此之前不得用它论证任何抗 AI 结论（§8.4） |
| R13 | `text-image-v1` 的**每次尝试成本（M4）未测**：没有 OCR、没有人工基线，因此它既没被证明能被机器解出、也没被证明解不出 | **已知且刻意**：管理面 `experimental:true` + `production_ready:false`，文档与错误响应都不声称抗机器；把它当生产防线用属运维误配 | 装一个离线 OCR 后跑 `make ocr-baseline`（适配器与评分器已交付并有测试）；再做人工可读性盲测；两者都有数据前不改 `production_ready` |
| R14 | 图片题把资源压力从 CPU 搬到**磁盘与响应体**：单题 6KB、base64 后约 8KB 响应，池里几十条就是几百 KB | 已用四条边界压住：单题 CHECK 65,536B、全局图片字节预算、视觉桶独立低水位（8/24）、生成不在事务内且预算先于渲染生效 | 阶段 5 用真实到达率重设水位与预算；widget 侧再评估是否需要分块或缓存策略（当前刻意不做裸媒体端点） |
| R12 | 池容量不足时公共请求直接吃 503（不降级是正确的，但对调用方表现为"暂时不可用"） | 默认水位 50/100 与 20000 行上限都是 `[目标]`，**未经真实流量校准** | 3B/阶段 5 按 `[实测]` 到达率重设水位；运维侧先用 `/api/admin/pool` 观测 |

## 19. 变更日志

- 2026-11-12 **交互原语 Wave 2 + negative polarity（ADR-031）**：新增 `progressive-target-v1`
  （三轮 3×3 定位）、`shell-track-v1`（三轮四容器，服务端预渲染 33 帧 PNG 雪碧图，`Motion:true`）、
  `silhouette-assembly-v1`（5 部件/5 槽位离散旋转装配）；`shape-grid-v1` 增加 negative polarity
  指令修饰器（kind/category，固定 k=2/3，fill 固定 positive）。三者均登记到 server/preview/devseed/
  admin/dispatch，全部 `experimental:true` / `production_ready:false`。`pipe-connect-v1` 完整管道整改
  继续 BLOCKED（legacy cells verifier 无法表达逐格 edge mask，需新 ID/version）。
  `web/widget/v1.interactive.js` 成为按需模块（核心 gzip 28,968 B / 模块 10,311 B / 合计 39,279 B）；
  GIF 与 Chrome/Firefox/WebKit 动画一致性、M4、人工可读性、辅助技术、真机移动端均 **NOT_RUN**。

- 2026-11-12 **交互原语 Wave 1（ADR-030）**：新增 `tile-rotation-v1`（3×3 路径块旋转，唯一解枚举）、
  `symbol-path-v1`（6×6 符号路径，DFS 唯一匹配路径）、`crowd-find-v1`（36 选 1 程序化非人形角色）、
  `visual-rank-v1`（5 卡线段数排序）；新增 `masks/path/select/order/sequence/placements` 六种严格
  `v1:` 离散编码与 widget 输入模式。全部 `experimental:true` / `production_ready:false`；M4、人工可读性、
  辅助技术、真机移动端、抗 AI/抗 OCR 均 NOT_RUN。`pipe-connect-v1` 完整管道整改标记 BLOCKED（现有
  verifier 无法表达逐格 edge mask）。`docs/reviews/CAPTCHA-BANK-REMEDIATION-2026-11-12.md` 是审计记录。

- 2026-11-06 **Phase 4.5**：`energy-window-v1` 增加跨周期相位取模（`ms % 2880`）与 <200ms 零思考拒绝，d1–d5 容差 6→2 桶（36 帧/2880ms 周期，M3 分母按 400 个可行桶算）；`pipe-connect-v1` 死路数 d1–d5 为 7→12，并把 M3 口径修正为按空候选格 `C(E,2)`（断点与死路外观相同，d1 3 次≈8.1%）；新增 `intersection-point-v1`（两条虚线延长线交点，`point` 量化 + 3×3 邻格 TSV；目标格有 50px 边距，M3 最坏 9/351≈2.56%，3 次≈7.5%，d5 9/1404≈0.64%）。三者 `experimental:true` / `production_ready:false`；M4、人工可读性、辅助技术仍 NOT_RUN。

- 2026-11-05 **第二批题型 Phase 4**：新增 `energy-window-v1`（36 帧游标 + TSV 时间桶，目标只在 400 个可行桶内抽样，M3 13/400≈3.25%）与 `pipe-connect-v1`（4×4 拓扑断点，M3 1/C(16,2)≈0.83%）。两者复用 `timing`/`cells`，Widget 无新增输入模式；`energy-window` 在 reduced-motion 下由调度器排除。M4/人工可读性/辅助技术仍 NOT_RUN。

- 2026-11-05 **风险调度/弱遥测/无障碍（ADR-026/027/029）**：`sites` 增加 `generator_pool`（0006），签发按风险带/设备/刷新惩罚在池内加权选型，pool worker 为 `d_base+1..+3` 维护轻量预热水位；Widget 只上报量化遥测摘要，服务端仅作风险修正，不改变显式判定；新增 `accessible-choice-v1` 纯文本无障碍通道（显式请求、池内显式包含、24h 一次）。调度、遥测与无障碍的 M4/真机辅助技术仍为 NOT_RUN。

- 2026-11-05 **首批标杆题型 Phase 2（parallax-code / bank-shot）**：新增 `parallax-code-v1`（4 位数字分帧视差，单帧只有部分笔画）与 `bank-shot-v1`（一次反弹，0.1° 量化 + TSV 41 桶容差）。两者 `experimental:true` / `production_ready:false`；M1–M3 已计算，M4/人工可读性/辅助技术/真机移动端均 NOT_RUN。Widget 增加雪碧图播放/暂停/重播与角度拖拽辅助线，主文件 gzip 仍 <30KB。

- 2026-11-05 **交互底座 Phase 1（ADR-025/028）**：新增 `angle`/`point`/`timing` 答案编码、雪碧图多帧契约与哑渲染器输入；新增 TSV（tag-set HMAC）连续容差 verifier，`MaxVerifierBytes` 512→4096、`MaxPayloadBytes` 4096→8192，并用 `0005` 迁移重建 `pool.payload_json` CHECK；现有 6 个题型与 48B/208B verifier 不变。实际生产题型、风险调度、遥测、无障碍通道与 M4 评测仍未实现。

- 2026-11-05 **题型扩展（ADR-024）**：新增 `shape-grid-v1`（3×3 图形点选，非纯文字输入）、
  `shape-count-v1`（两类图形计数，两个数字输入）与 `text-image-v3`（6 字符抽象文字，字形层模糊/正弦位移/擦除点/
  纸面纹理，允许最多 1 个字符替换）。新增逐字 HMAC 容差 verifier（`nonce || 每字 tag`，v3 专用）；
  v1/v2 的 48B 精确 verifier 与渲染路径不变。§8.7 记录三种题型的 M1–M4、资源上界与 NOT_RUN 清单；
  §10.1/§11 记录 `answer_type`（`text`/`cells`/`pairs`）与 widget 控件映射。三个题型均为
  `experimental:true` / `production_ready:false`；人工可读性、M4、非视觉通道与按权重混合题型仍未实现。
  `docs/QUESTION-TYPES.md` 是操作与验收边界说明。

- 2026-11-02 **本轮修复冻结证据索引**：`docs/reviews/REPAIR-FREEZE-2026-11-02.md` 记录审计项处置、真实 challenge/浏览器验证与未完成项；对应 `release-artifacts/repair-freeze-2026-11-02-ae546e9c/freeze-receipt.json` 保存最终源码指纹、原始产物及执行状态。源码快照不等于正式发布候选，Linux/race/容器与真人验收未自动通过。

- 2026-11-02 **审计后收敛与 challenge 验收（ADR-021）**：用户要求修复、测试及冻结；新凭据生成/交付与成功审计同事务，紧急撤销保留收紧语义；恢复仅在临时副本完成失效与验证后交付，包含过渡期管理 key 撤销；题池按有效 challenge TTL 补给，并可回收绑定 challenge 已过期/终结的题而不回池。widget/管理台生命周期、接入失败关闭、发布必需证据与实际构建字节一并收敛。必须执行真实 challenge/PoW/redeem/siteverify 及重放/过期/来源/题池负测；当前平台的测试工装不代替正式 Linux/arm64、race 或容器验收。原功能台账与未完成人工项继续保留。

- 2026-11-02 **可靠性修复与冻结边界（ADR-020）**：用户显式授权 Go 1.27.3；凭据撤销截止时间单调、轮换原子提交、过渡期凭据纳入有效上限；widget reset/兑换超时的世代隔离；修正部署候选与密钥配方，新增发布检查及 LF 约束。正式部署仍须在支持的 Linux 环境完整验收，源码冻结与交叉构建不等于上线批准。历史阶段、抗 OCR/人工可读性及支持平台不因本条自动升级为 PASS。

- 2026-10-19 **v2.12（阶段 5C-M1：管理控制台第一批——登录、站点列表、创建、策略与 Origin 编辑、启停）**：
  `web/adminui/` 内嵌原生 HTML/CSS/JS（零框架、零构建、无第三方 CDN/远程字体，默认中文），
  挂在**管理监听器**的 `/admin`、`/admin/app.js`、`/admin/app.css` 上；公共监听器没有这些路由，
  判据写成"公共端口 404 而非 401"（401 表示路由挂上了只差鉴权，仍是暴露）。
  **没有**为了浏览器方便发布管理端口、改 `EPOCHA_ADMIN_LISTEN`、加 CORS 或未鉴权代理，
  Docker 网络边界原样不动（ADR-019）。
  凭据只活在 JS 闭包：不进 localStorage/sessionStorage/IndexedDB/Cookie/URL/日志，
  提交即清空输入框，401 清状态；页面明说"不保证物理擦除内存"。四个视图都用真实端点，
  列表/详情**没有**任何 QPS、成功率、趋势图（统计能力还不存在）；
  题型下拉旁的 `experimental` / `production_ready` 两个徽标直接取自 `/api/admin/generators`，
  `arithmetic-baseline-v1` 与 `text-image-v1` 都仍是"未达生产可用"（§8.4、红线 14 不变）。
  详情页保存只发改动过的字段，保存后 `GET` 复读逐字段对账；脏标记按表单分部（否则保存策略会抹掉
  "来源未保存"这件事），确认横幅**叠加**在表单之上而不重建内容（重建会让"留下继续编辑"拿回空表单）。
  后端唯一最小扩展：`PUT /api/admin/sites/{id}` 可改**名称**（不参与判定 ⇒ 不提升站点版本、
  不使已签发东西失效；撞 `UNIQUE` ⇒ 409）；`site_key` 仍不可改（它是签发绑定与来源判定的锚点）。
  新增 `docs/ADMIN-UI.md`（含 NOT_RUN 清单：403/5xx 文案、剪贴板、真机移动端、辅助技术实测）、
  `docs/scripts/admin-demo.sh`（loopback 双监听 + 空临时库 + 运行时随机 0600 凭据文件，凭据不打印）、
  `docs/scripts/admin-ui-check.sh` + `make admin-demo / admin-ui-check`（退出码三态），
  以及 `web/adminui/adminui_test.go`、`internal/admin/ui_test.go`、
  `cmd/server:TestAdminUISitsOnlyOnAdminListener`、`docs/scripts/browser/adminui_browser.test.mjs`。
  本轮**不**打 `stage-5c-done`：题池/审计/凭据轮换页面不存在，阶段 5 的统计与分级淘汰也不存在。

- 2026-10-19 **v2.11（阶段 5B 收尾：正式支持范围收窄为 B 路径，门禁结论按三类归属计算）**：
  ADR-017 记录这个取舍本身——第一版只把 **B 路径**（宿主 `CGO_ENABLED=0` 静态编译 + named build context
  装配运行镜像）列为受支持且需要验收的构建路径，A 路径（容器内编译）保持**可选、NOT_RUN**；
  这不是把 A 写成通过，而是缩小支持范围，代价（工具链一致性落在执行者本机 Go）一并写进 `DEPLOYMENT` §3。
  `stage5b.sh` 的结论改为只由**必需项**决定：必需 FAIL⇒1、必需 BLOCKED/NOT_RUN⇒3、全 PASS⇒0；
  可选项（A 路径、amd64、root 场景的 UID 65534）如实输出但不决定退出码，且在结构上不能被写成 PASS；
  超范围项（人工可读性、抗 OCR/抗 AI、难度档）只登记边界、永不打印 PASS。分类逻辑有 9 条负向自测
  （`--selftest-classify`）并经变异验证（删规则⇒2 条 FAIL，并进必需项⇒4 条 FAIL）。
  新增门禁**第 2.5 步：启动配方演练**——按 `DEPLOYMENT` §11 的形状在隔离部署根真起一套，七条判据含
  "重建容器后已消费 token 仍被拒"与"只定点清理本轮资源"；另加 compose/`.env.example` 变量名双向对账
  （取不到列表即 FAIL，不接受"空输入⇒无漂移"）与相对绑定按项目目录解析的断言。
  `DEPLOYMENT` §4/§11 的启动配方改写（原来 `cd deploy/docker` 与"`make` 在仓库根执行"自相矛盾，
  且 `> keys/master` 没有覆盖保护 = 重跑毁密钥），现改为仓库根 + `-f/--project-directory/--env-file` + `set -C`。
  退出码报告更正：实测 `make` 只报 0 或 2（任何非 0 折叠成 2），故"make stage5b-check rc=3"的旧表述不成立；
  配方改为打印脚本真实码并输出 `SCRIPT_RC=`。修正 `Makefile` 的 `run:` 配方（自 5A 起 `run: build` 后接 TAB
  导致 `make run` 报 `No rule to make target '@mkdir'`）。候选清单改由**干净提交**构建并落在忽略目录
  （含当前 commit 的清单再入库会自引用循环）；`stage-5b-done` 标签正文限定其覆盖范围。

- 2026-10-18 **v2.10（阶段 5B：运行互斥、恢复边界收敛、部署候选清单与持续演练）**：
  §12 新增运行互斥一段：判据是数据目录上的 `flock` 排他锁而非端口探活；服务"迁移前持有、关库后释放"，
  `admin-recover`/`restore` 取同一把锁，`backup`/`verify` 明确不取（在线快照允许并发）。
  ADR-015 记录机制选择的四条理由（非阻塞、锁文件永不删除、不用 PID 文件、建议锁的能力上限）。
  `BACKUP-RESTORE.md` 把"可信"拆成 checksum / 数据库一致性 / 来源真实性 / 保密性四维并标注只存在前两个
  （只有 checksum ⇒ 不宣称抵御恶意替换；备份目录的可写者就是备份内容的作者），
  并补 `admin-recover` 在“凭据文件已写、库未提交”之间失败时的明确出口。
  §17 新增阶段 5B 边界与验收；`cmd/loadtest` 加 `--pace` 节流（没有它 450 次闭环会在 1 秒内打完，
  "5 分钟观察"就无从谈起），测量实例抬高限流**不**写进 compose。
  新增 `docs/scripts/stage5b.sh` 与 `make stage5b-check`、`docs/STAGE5B.md`。
- 2026-10-18 **v2.9（阶段 5A：单节点部署包、密钥持久化、备份恢复与资源演练）**：
  §6 的密钥入口按实现更正（`--master-key-file` 从未存在，真实入口是 `EPOCHA_*_FILE`），
  并写入"env 与 `_FILE` 同时设置即拒绝启动、不做优先级"与文件校验规则（`os.Stat` 解析后的目标、
  group/other 位必须为空、≤4096B 不截断、内容与路径不进文案）。
  §10.1 新增 `GET /readyz`（就绪 = 连接 + schema 版本 + **写路径可用**；不领题不消费 token；
  有 2s 边界并沿用请求 ctx；只回一个词）。§12 的备份方案更正为 `VACUUM INTO`（`sqlite3` 属外部二进制，
  与 AGENTS 约束冲突；理由与保守恢复语义见 ADR-014），并补 5A 的容器形状与"管理面只在容器内 loopback"。
  §15 新增 `cmd/ops/`（运维入口，随镜像交付）、`cmd/loadtest/`（开发期有界负载工具，`cmd/server` 不链接）、
  `deploy/docker/` 的真实文件清单与根 `.dockerignore`。
  §17 把阶段 5 拆出 **5A** 并写清范围边界与验收项。§13 的资源数字回填见 `docs/STAGE5A.md`（本轮实测）。
  新增 `docs/{DEPLOYMENT,BACKUP-RESTORE,STAGE5A}.md` 与 `make stage5a-check`（退出码 0/1/3 三态）。
- 2026-10-18 **v2.8（阶段 4B：接入收尾 + 真实窄屏 + 首次 OCR 基线）**：
  §8.4 的 M4 从"未测（阻塞）"改成**一次真实的通用 OCR 基线**（低/中/高档 96%/31%/3%，Wilson 95% 见该行），
  并据此写下两条结论与一条新限制：低档挡不住通用 OCR ⇒ 生产默认不得落低档；
  单一工具单一配置的基线**不足以**支撑任何"抗机器"结论。
  §8.6 补记隐藏字段的**归属规则**（`fieldName` 非空而容器不在目标 `<form>` 内 ⇒ `render()` 抛错，
  不静默留一个提交不到的字段；`form` 选项只接受当前文档内的真实 form）。
  §10.3 的来源判定补一条实测事实：Firefox 在原生 form POST 上发 `Origin: null`，
  它表示不透明来源而不是"另一个站"，因此必须回落 `Sec-Fetch-Site`（真跨站 Origin 仍直接拒）。
  §13 填入 widget gzip 从 16,687 B 变为 **17,704 B**（加 `form` 规则的结果）。
  §15 目录新增 `cmd/blindtest/` 与 `docs/scripts/ocr/`。
  **同时纠正一条此前写错的阻塞结论**：4A 记的"Firefox BiDi 不提供视口设置命令 ⇒ 375px BLOCKED"是错的——
  规范命令 `browsingContext.setViewport` 在 Firefox 147.0.2 上真实生效，窄屏已变成执行并断言的用例
  （先断言 `innerWidth`/`clientWidth` 等于目标值再量布局）。教训写进 AGENTS：**能力边界要由能力测试给出**，
  不能把"我没实现的那个调用失败了"读成"平台不提供"。
- 2026-10-18 **v2.7（阶段 4A 实现完成）**：新增 §8.6（widget 与业务接入的工程事实：Shadow DOM 直挂不用 iframe、
  Blob 与同源文件两条 worker 路径、状态机与并发语义、隐藏字段必须落在 light DOM）；§10.1 的两条
  `/widget/*` 路由从"阶段 4 计划"变成 `[实测]` 事实，并写入 **site_key 一致性规则**（URL 与请求体、
  URL 与 token 所属站点必须一致，冲突即拒且**不消耗**作答机会；不带 URL 参数的老客户端不变）；
  §13 填入 widget gzip **16,687 B**、浏览器 SHA-256 **422,833 次/秒**、bits=12/20 的墙钟区间；
  §15 增加 `web/widget/`、`cmd/exampleapp/`、`docs/scripts/browser/`；§17 把阶段 4 拆成
  **4A（已完成）/4B（未开始）**，未达成项含 **BLOCKED** 的 375px 视口检查与 **NOT_RUN** 的人工项；
  §18 新增 R15（CSP 禁 Worker 时验证无法完成，组件明确失败而不假装通过）。新增 ADR-013、
  `docs/WIDGET.md`、`docs/STAGE4A.md`、`make stage4a-check`、`docs/scripts/ocr-baseline.sh`。
  **口径纠正（写进 AGENTS.md 成为长期约定）**：验收状态一律 PASS / BLOCKED / NOT_RUN 三态，禁止
  "PASS：某项未测"这种混合表达；退出码按 `二进制 / go run / make` 三层分别实测并记录
  （`go run` 把任何非 0 折叠成 1，`make` 折叠成 2 ⇒ 只有直接跑二进制/跑脚本才拿得到 3）；
  3B 的 M3 明确区分**单次** `9.31e-10` 与 **3 次独立新挑战累计** `2.79e-9`（此前一处把后者写成了单次数值）。
  本轮由**真实浏览器**抓到、curl 层永远看不到的缺陷：`http.ServeContent` 在缺 `/etc/mime.types` 的机器上把
  脚本 `Content-Type` 覆盖成 `text/plain`（配 `nosniff` 直接拒绝执行）、严格 CSP 挡下示例页面的内联
  `<style>/<script>`（页面"就是不动"且无 HTTP 症状）、worker 的 `started` 被误当终态导致组件卡在
  `solving` 直到超时、BiDi 对视口外坐标报 `move target out of bounds`、示例应用端口冲突会让测试
  **因为错误的原因通过**（门禁现在开头就检查固定端口空闲）。

- 2026-10-18 **v2.6（阶段 3B 实现完成）**：新增 §5.5（`0004` 图片资产列与两条独立资源边界的原子判定、
  统计不加载 BLOB、判分与资产解耦、资产随 `pool` 行同生同死）、§8.5（`text-image-v1` 的取值定义、
  五档有界渲染参数、可读性的几何保证、M1–M4 填表与**M4 未测**的结论）、§10.1（图片以 base64 内联进
  `question` DTO，**不采用**草稿里的 `/asset/<id>` 签名端点；媒体与资产不配对即作废 challenge + 503）、
  §10.2（`experimental`/`production_ready` 默认拒绝、`EPOCHA_GENERATORS` 禁用语义、池的字节观测）、
  §13（图片资产字节、生成耗时、图片池磁盘、服务进程 RSS 的 `[实测]`）、§16（引入 `golang.org/x/image` v0.36.0
  与"不能升到 v0.37+"的理由）、§17（3B 已实现 + 未达成项清单）、§18（新增 R13/R14）。
  新增 ADR-012、`docs/STAGE3B.md`、`docs/NOTICE.md`、`make stage3b-check`（22 项）。
  本轮另修掉四个实测踩到的东西：① 混用两种 Bresenham 符号约定导致**陡线死循环**（改成有界步数的
  参数化采样，并有终止性测试）；② `x/image/draw` 的 `Transform` 实测**不采纳 `sr.Min`**（identity 矩阵下
  把 (10,20) 的块画到 (10,10)），字形 tile 因此改为原点建图、平移写进矩阵；③ 预览页配置用
  `HTMLEscapeString` 注入会让 `<script type="application/json">` 里的 `JSON.parse` 抛异常
  （script 数据块不解码 HTML 实体）——页面"加载成功但什么都不做"；④ 预览页 fetch 没带 `?site_key=`，
  JSON 请求触发的 CORS 预检只能从查询参数认站点 ⇒ 浏览器拿到 `OPTIONS 403`（curl 测不出来，
  是 Firefox headless 截图发现的）。
  纠正两条：跨题绑定测试改用**受控夹具**（不再靠随机抽题/有界重抽，且明确"答案相同是合法现象、
  不可跨题复用的是 verifier/绑定/PoW/token"）；标签规则改为"完成后打 `stage-3b-done`，
  不移动 3A 标签、不打 `stage-3-done`、不自动 push"。
- 2026-10-17 **v2.5（阶段 3A 交付复核）**：v2.4 的表述里有 6 处与代码不符或依赖了未验证机制，本轮逐条按实测更正——
  ① `0003` 迁移注释与 ADR-011 原稿把 verifier 写成 `HMAC(…, pool.id ‖ 答案)`，实现是 **每题随机 nonce**，注释按实现改写；
  ② `claimAttempts` 的注释声称"并发对手赢下该行后 `RowsAffected==0` ⇒ 换下一个候选"，实测**不成立**：同进程被
  `SetMaxOpenConns(1)` 串行化而走不到这一支，跨句柄则是 `UPDATE` 拿 `database is locked` 整笔回滚
  （新增 `TestStaleSnapshotClaimFailsClosed` 钉住该失败模式，重试上限重新定性为"只为封顶循环，不是正确性依赖"）；
  ③ `SanitizePublic` 的注释与测试名说"丢弃未知键"，实现是**整次报错**（fail closed，配合作废该 challenge），
  注释与 `TestSanitizePublicRejectsUnknownFields` 按实现更名；
  ④ §10.1 把 `pow_only` 的 `answer` 写成"必须为空"，实现是**忽略**该字段，按实现改述；
  ⑤ §5.4 不变量 #1 的测试原来只断言"一个 challenge 没绑多题"（partial UNIQUE 已经保证的那一半），
  补成"绑定行数 == `count(distinct challenge_id)` == 成功领取次数"，才真正覆盖"同一题行被改绑"那一半；
  ⑥ 故障注入回归的红名单从 4 条更正为实跑的 **7 条**（store 3 + api 4，见 §17）；
  ⑦ 补上一条验收要求却缺失的测试：`pow_question` 答错终结的 challenge **经进程重启后**拿正确答案仍必须被拒、
  其绑定题不得回到 `ready`（`TestFailedChallengeStaysFailedAfterRestart`；此前只测了"站点停用导致的版本失效"那条路径，
  没有测"答案错误导致的终态"这条路径的重启语义）；
  ⑧ `TestAnswerIsBoundToItsOwnChallenge` 原来在两道随机题面恰好相同时 `t.Skip()`，与本节"3A 无 `t.Skip`"的声明
  和 AGENTS 的禁令直接冲突（跳过会把这条安全性质的覆盖率交给概率，且 CI 永远绿）。改为**有界重抽**到答案不同为止，
  6 次仍撞同一个答案就 `t.Fatalf`——那时该报错的是"池多样性塌了"。3A 测试文件里现在没有任何 `t.Skip`；
  仓库剩余的 2 处都是阶段 5 的显式占位（排序白名单、分级淘汰），按约定不计入本阶段验收。
  另外：`burnQuestionTx` 的 `terminal_at` 原用 `time.Now()`（与注入时钟两套时间源）改为传入的 `now`；
  池 worker 的迟滞撤防原用**跨桶累计**的 `made` 判定（第二个桶会被第一个桶的产量提前撤防）改为按桶计数；
  删除两个无人使用的契约字段/常量（`generator.Params.Binding`、`MaxInputLen`）与管理面一处 `_ = g` 死赋值。
  §13 新增**真实服务进程** RSS 与库体积的 `[实测]`（200 题 + 20 次闭环：VmRSS=VmHWM=15.1MB、checkpoint 后库 240KB），
  并明确它不是稳态值；§8.4 的 M3 与 R11 按第二次抽样改为区间表述，并记录 d1 的众数 `0` 与 `14` **是并列的**。
- 2026-10-17 **v2.4（阶段 3A 实现完成）**：新增 §5.4（`0003` 增量迁移、PublicPayload/PrivateVerifier/题池记录/绑定挑战
  四层契约、题池状态机与七条不变量、`verifier_blob = nonce||HMAC(K_qver,…)` 的答案不落库形态、清理顺序
  responses→pool→challenges、第一笔有效作答终结语义）、§8.4（3A 的生成器契约与 `arithmetic-baseline-v1` 的 M1–M4，
  含**答案分布不均匀**的实测单点概率）、§8.3 补实现形状（水位迟滞、熵读取上限、进程内计数与 `restart_resets`）、
  §10.1 追加 `mode`/`question` 与 `answer` 及 **503 `challenge-unavailable`**、§10.2 追加 `/api/admin/generators`
  与 `/api/admin/pool`（只读、不落审计）、§6 追加第三把派生密钥 `qver` 与 `mode`/`gen` 签名快照、
  §13 回填生成耗时/单题体积/库增长速率/二进制体积的 `[实测]` 与未测项清单、§17 把阶段 3 拆成 **3A（已完成）/3B（未开始）**。
  两处草稿按实现更正：§8.1 的 `Verifier` 接口与 `generator.Register` 全局注册表（改为 `PrivateVerifier []byte` 与实例注册表）、
  §8.3 的"池空 ⇒ 同步生成 ⇒ 429"（改为 503 可重试，**不降级成 PoW-only**）。新增 ADR-011。
  3A 期间修掉三个真实缺陷：错误答案分支若返回 error 会让整笔事务回滚从而**永久可重试**（改为提交终态，并有故障注入回归）；
  一批补不完时水位长期停在低位（加 `refilling` 迟滞）；一条"重复返回同一字节"的熵源让 `crypto/rand.Int` 的拒绝采样
  **永久自旋**（测试挂死 5 分钟；生产路径改为每题熵读取上限 + 有界失败）。
- 2026-10-17 v1：阶段 0 定稿（目录骨架、三层防线、API 契约、SQLite schema、密钥体系、CF 折衷、四题型反枚举设计、威胁模型与残余风险 R1–R8）。
- 2026-10-17 v1.1：模块路径改为 `github.com/shijianus/EpoCHA`（远端 private 仓库已确定）。§6 新增管理密钥引导与**密钥材料不入仓库**硬约束；真实密钥走 `/etc/epocha/env`（0600）与 DB 分离存放。理由见 DECISIONS。
- 2026-10-17 **v2.1（阶段 1 实现完成）**：按 v2 落地 `config/store/pow/token/risk/api/cmd`；实测二进制 9.19MB（回填 §13）；
  §10.1 明确 `internal-error` 是唯一超出 reCAPTCHA 族的扩展码且只随 5xx 出现；§15 记录 `migrations/` 实际位于
  `internal/store/migrations/`（`go:embed` 只能嵌入本包子树）。验收期间修掉四个真实缺陷：token tag 比较误将 base64 文本与解码字节相比；
  限流 `RetryAfter` 使用真实时钟而非注入时钟；devclient 随机起点可能为负导致提交非法 nonce（改为复用 `internal/pow` 判定）；
  `serve()` 用 `fmt.Fprintf` 与 slog 并发写同一 writer 触发数据竞争（启动信息统一走 slog）。
- 2026-10-17 **v2.3（阶段 2 实现完成）**：新增 §5.3（站点生命周期、`site_secrets`/`site_origins`/`admin_keys`/`audit`、
  配置生效两类语义、`hostname` 来源与保证范围、清理与外键）、§10.2 管理 API 实况与监听器隔离与错误族、§10.3 来源策略与 CORS；
  §6 补齐管理凭据引导（逐字节同值、有记录后忽略 env、无凭据即拒绝）与摘要域分隔的例外理由；
  §17 阶段 2 验收落地并把旧的"Origin 不符 → `bad-request`"矛盾按 §10.1 更正为 `invalid-request`；
  新增 ADR-010。实测：`0002` 迁移在阶段 1 库上可重复执行；SQLite 3.51.2 不能 DROP UNIQUE 列（故中和旧列）。
  阶段 2 期间 `-race` 复现并修掉一处真实缺陷：`serve()` 若在 `Serve` 登记 listener 之前就因 ctx 取消而返回，
  `Shutdown` 关不到那个 listener（它只关"已被 Serve 登记"的），端口在进程退出路径上仍接受连接 → 现在两个 listener
  都由 `serve` 自己 `defer Close()`。§13 的二进制实测值随之更新。
- 2026-10-17 **v2.2（阶段 1 安全复核，见 `docs/reviews/STAGE1-REVIEW.md`）**：修复 6 个真实缺陷——
  `Purge` 与外键冲突导致清理永久失败；`/siteverify` 服务端故障误用 `invalid-request`（应为唯一扩展码 `internal-error` + 5xx）；
  站点维度限流按自报 `site_key` 计数可被污染（改按站点 id）；签发缺少**存量**上限（新增 `Caps` 与两个环境变量）；
  TTL 无上界（新增 `[1s,1h]`）；`serve()` 错误分支 `close(purgeDone)` 与 goroutine 自身 defer 双关。
  §9 补"速率 vs 存量"的区分与"删除行 ≠ 文件缩小"，§6 把 payload 上限按实现取严者（2048B）写死。新增 ADR-009。
- 2026-10-17 **v2（阶段 1 前的设计校正）**：ADR-001 Cloudflare 事实更正与边缘定位改述；ADR-002 题库安全论证拆为 M1–M4 四指标并撤回"随机性 ⇒ 抗 AI"的推论；ADR-003 payload/verifier 分离、取消强制 HMAC 精确匹配；ADR-004 一次性消费的四条保证与实现手段分离（删除"单连接/`BEGIN IMMEDIATE` 是唯一合法实现"，纠正 `sql.Tx` 嵌套 BEGIN 错误）；ADR-005 防重放持久化提前到阶段 1（含新增 §5.1 实际 schema）；ADR-006 数字标注 `[目标]/[估算]/[实测]`、淘汰改分级、安全状态提交后才返回成功；ADR-007 `modernc.org/sqlite` 锁 v1.46.1 与 `GOTOOLCHAIN=local`；ADR-008 两处措辞收紧。**原始错误表述只在 `docs/DECISIONS.md` 留存。**
