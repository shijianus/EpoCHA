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
- 不提供商业 SLA、不做多节点集群（SQLite 单写者决定了它是单机方案）。

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

## 6. 密钥体系与 token

**主密钥**：`EPOCHA_MASTER_KEY`（32B，env 或 `--master-key-file`，权限 0600）；**pepper**：`EPOCHA_PEPPER`（32B，与 DB 文件分离存放，独立挂载）。两者缺一服务拒绝启动（不静默降级）。

派生（**带域分隔的 HMAC-SHA256**，标准库即可；不引入 `x/crypto/hkdf`，理由见 ADR-007——本设计每个用途只需要一把固定长度密钥，
单次 HMAC 展开在此场景与 HKDF-expand 等价，且域分隔串含版本与 purpose，无跨用途复用风险）：
```
kid     = hex(HMAC-SHA256(master, "epocha/v1|kid")[:8])
K(purpose, kid) = HMAC-SHA256(master, "epocha/v1|" + purpose + "|" + kid)     // purpose ∈ {chal, resp}
```
**token 线格式**：`v1.<kid>.<b64url(payload_json)>.<b64url(tag)>`，`tag = HMAC(K(purpose,kid), "epocha:v1|" + purpose + "|" + b64url(payload))`。
签名覆盖 payload 的**原始 base64url 字节**（不重新序列化），因此难度、TTL、站点、challenge id 全部不可被客户端改动；
PoW 参数以 **DB 行为权威**再校验一次（纵深防御：即使密钥泄露导致伪造 token，参数与站点仍受库内记录约束）。

payload 字段（阶段 1 实现）：`typ`(`chal`|`resp`)、`kid`、`sid`(站点 id，硬绑定)、`cid`(challenge id)、
`rid`(仅 resp，response id)、`iat`、`exp`(Unix 秒)；`chal` 额外带 `bits`、`seed`(base64url)。未知字段一律拒绝，payload ≤4KB。
验证顺序：总长度上限 → 分段解析 → kid ∈ {current, previous} → **常数时间比 tag** → `DisallowUnknownFields` 解 payload →
`typ`/purpose 匹配 → `exp`/`iat` 时钟窗口 → `sid` 与请求站点一致 → 业务层原子消费。任何一步失败对外都是同一族错误码，不区分原因。
轮换：`kid` 允许同时存在 current + previous（`--master-key-previous`），旧 token 在 TTL 内自然过期。

`secret_key` 校验：`want = HMAC(pepper, provided_secret)`，`subtle.ConstantTimeCompare(want, site.secret_mac)`。admin_key 同法（存摘要，不存明文）。
**管理密钥引导**：库中无 admin 记录时，用 `EPOCHA_ADMIN_KEY` 登记其摘要（只此一次，之后靠 `/api/admin/keys/rotate` 与审计管理）；有记录后该环境变量忽略。
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
**本节全部内容属阶段 3，本轮不实现**；`internal/generator` 现有代码仍是契约占位。

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
| `arith-v1` 算术/逻辑文字题 | 输入数字 | 模板 40+ × 参数区间 ≥10^4 → ≥4×10^5 | 答案域 ≥10^4、精确匹配 → 猜测通过率 ≤10^-4（未测） | 待测：模型直接算 vs 打码人工 |
| `text-warp-v1` 扭曲字符图 | 输入字符串 | 5–7 字符 × 字体随机指派 × 逐字旋转/错切/扭曲 × 干扰线 × 噪声 | 字符集去易混后约 32^n（n≥5）→ >3×10^7；容差 0 → 猜测通过率极低（未测） | 待测：本地 OCR/多模态基线通过率与单次成本，**必须替换 §4 的 60–90% 估算** |
| `cjk-v1` 中文/成语选择题 | 点选 4 选项 | 语料 × 干扰项组合 × 遮罩/描边/色相 × 问法模板 | **答案空间仅 4，接受域 1 → 猜测通过率 25%，这是本题型最弱的量**；靠 3 次尝试封顶（≤58%）+ PoW 成本叠加，不靠渲染混淆 | 待测 |
| `rotate-v1` 图片旋转/拼合 | 拖动滑块转正 | 底图 × θ∈[0,360) × 翻转/裁剪/水印 | 原始设计写"粗 24 × 精 6 = 144"——**该数值是未验证估算，且它只描述 M2，不代表安全性**；若容差取 ±7.5°，接受域将显著放大猜测通过率（阶段 3 实测给出） | 待测 |

**统一约束（代码层强制，测试守护）**
- `Challenge.Payload` 序列化后必须经守卫检查：答案的任何可识别形式（含原始连续值、生成 seed 与题面的映射）不得出现。
- 单题尝试次数 ≤3（超限作废并计入风险）；每个题型必须给出 M3 的推导与 n=3 时的累计通过率，**下限不达标（猜测通过率 >10%）的题型不得上线**。
- 生成时记录 `solve_ms_p50_hint`（按难度档静态表 + 题型），用于 §9 时延异常判定；阈值待 `[实测]` 后设定。
- 素材与语料必须本地自有或许可证允许再分发（OFL 字体 / CC0 图片 / 自建成语表）；许可证清单落 `docs/NOTICE.md`。

### 8.3 题池水位与预生成

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
限流：内存令牌桶按 `ip` 与 `site_key` 两维，状态条目有硬上限（阶段 1 默认 8192，可配）+ 定期清理与超额淘汰，
否则攻击者可用随机源地址撑爆 1GB 内存；超限 `429 + Retry-After`。持久封禁（阶段 2+ 可选）写 `meta` 中的 ban 列表，避免重启清零。
**阶段 1 的限流就是这个有界实现**，不含打分。

## 10. API 契约

### 10.1 公开 API

| 方法 | 路径 | 鉴权 | 阶段 | 说明 |
|---|---|---|---|---|
| POST | `/api/challenge` | `site_key` | 1 | 签发 PoW challenge（阶段 3 起附带题目 payload） |
| POST | `/api/redeem` | challenge token | 1 | 提交 PoW 解，成功换一次性 `response`（阶段 3 起附带答案与行为数据） |
| POST | `/siteverify` | `secret` | 1 | reCAPTCHA 风格，服务端调用 |
| GET | `/healthz` | 无 | 1 | 存活探针，只返回 `{"status":"ok"}`，不含版本/路径/计数/依赖信息，不做健康即放行的语义 |
| GET | `/asset/<pool_id>/<sig>` | 签名 | 3 | 题目图片，`Cache-Control: no-store` |
| GET | `/widget/v1.js` | 公开 | 4 | widget 静态资源（gzip） |

阶段 1 的公开端点**只有 PoW**：响应里不存在 `challenge` 对象，也不接受 `answer`/`behavior`。文档与实现都必须如实反映这一点，
不得用空字段伪装成完整三层（ADR-005）。

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
阶段 3 在此响应里追加 `challenge: {id, media, asset_url, task}`（形状见 §8.2）。

`POST /api/redeem` — 请求（≤4KB）
```json
{ "token": "v1.6f2a1c9b8d0e4a71....", "pow_nonce": "98765432" }
```
`pow_nonce`：ASCII 十进制，长度 1–16，值 ≤1e12。成功 `200`：
```json
{ "response": "v1.6f2a1c9b8d0e4a71.<b64url(resp payload)>.<b64url(tag)>", "expires_in": 120 }
```
失败 `400/401/429`：`{ "error-codes": ["invalid-challenge"] }` —— 这两个 `/api/*` 端点**不是** reCAPTCHA 契约面，
使用自有且有限的码集：`invalid-request` / `invalid-challenge`（含篡改、过期、已消费、尝试次数耗尽，**不做区分**） / `rate-limited`。

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
- `hostname`：**阶段 1 仅回显调用方自报值**，服务端无域名白名单可裁决，因此它不是安全依据；阶段 2 引入 `site_domains` 后由服务端裁决并可为空。
- `challenge_ts`：challenge 创建时间（来自 `challenges.created_at`，非客户端上报）。
- 不返回 `scores` 等扩展字段以维持兼容；业务方需分数须走内部 `/api/admin/verdicts`（阶段 5，可选）。

### 10.2 管理 API（`Authorization: Bearer <admin_key>`）

| 方法 | 路径 | 用途 |
|---|---|---|
| POST/GET | `/api/admin/sites` | 站点列表 / 创建（响应含一次性 `secret_key`） |
| GET/PUT/DELETE | `/api/admin/sites/{id}` | 详情 / 改难度、TTL、限流、阈值 / 吊销 |
| PUT | `/api/admin/sites/{id}/domains` | 域名白名单全量替换 |
| PUT | `/api/admin/sites/{id}/generators` | 题型 enabled/weight/difficulty（权重含自动降权） |
| GET | `/api/admin/pool` | 水位、各生成器 ready 数、pool_miss 计数 |
| GET | `/api/admin/stats?from=&to=&generator_id=&site_id=` | 通过率、可疑率、平均耗时 |
| POST | `/api/admin/keys/rotate` | master key kid 轮换登记（previous 生效） |

管理端全部写 `audit`。输入校验：分页上限 500、字段长度上限、枚举白名单。

## 11. Widget（阶段 4）

- 零依赖原生 JS + `<epocha-widget data-site-key data-action>`（自定义元素，Shadow DOM 隔离样式），或 `data-cap-*` 式属性 API 的等价 `<epocha-verify>`。
- 产物预算：`make widget-size` 断言 gzip <30KB（含 WASM 则计入）。
- 国际化：`zh-CN` / `en` 两套字符串表内联，按 `lang` 属性 → `Accept-Language` → 默认 `en`。
- 无障碍（不是可选装饰）：全部题型可键盘操作；`aria-live="polite"` 播报状态；图片题必须提供 **非视觉等价通道**：
  - 首选 `arith-v1`/`cjk` 文本朗读版（`aria` 文本替代，不用 audio CAPTCHA——不做音频依赖）；
  - 同时提供**"我不适合看图"→ 纯 PoW 高难度通道**（`bits+4`，无图像题目）。这是为无法解题的用户保留的正当出口，不是绕过口：PoW 成本仍是有效门槛。
- 移动端：触摸拖拽（`rotate`）、避免 hover 依赖、视口与 safe-area 适配、失败重绘不换页。
- 降级：`/api/challenge` 超时（>3s）自动重试一次并切换备用 endpoint（配置项 `fallback_base`，默认源站）；JS 不可用时提供 `<noscript>` 提示 + 服务端纯 PoW 表单模式（阶段 5 文档化，默认关闭）。
- 输出 `response` 给宿主页面 `data-epocha-response` + `epocha:ready` 事件；业务服务器再调 `/siteverify`。

## 12. 部署拓扑

**主节点（必需，VPS 1C1G）**
```
systemd 单元 → epocha server --listen :8080 --db /var/lib/epocha/epocha.db ...
nginx/caddy: TLS 终止 + client_max_body_size 8k + X-Forwarded-For 处理 + /api/admin 仅内网
ufw: 仅放行 443（+ CF IP 段），8080 不对外
swap: 512M + vm.swappiness=10；GOMEMLIMIT=56MiB GOMAXPROCS=1
```
备份：`sqlite3 .backup` 每日 + WAL checkpoint；恢复演练写入 `deploy/README.md`。

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

**本表除"守护方式"外全部是 `[目标]` 或 `[估算]`，阶段 1 未做任何性能测量。**"实测"列必须由阶段 5 的 `make loadtest`
（注明机器、日期、负载形状）填写后才能作为选型依据。

| 项 | 目标/估算 | 实测 | 守护方式 |
|---|---|---|---|
| 常驻 RSS | `[目标]` <100MB（Go 基线 + 字体光栅 + SQLite page cache + 池缓冲，阶段 3 后才有光栅项） | 待填 | `GOMEMLIMIT`；测试读 `/proc/self/status` 断言峰值 |
| 二进制体积 | `[目标]` ≤35MB（含 modernc/sqlite） | `[实测]` 9.19MB（9,634,084 字节；2026-10-17，linux/arm64，go1.24.5，`-trimpath -ldflags "-s -w"`，含 modernc SQLite 驱动与内嵌迁移） | `make size` |
| 内嵌字体 | `[估算]` Latin 1–3MB；CJK 子集 4–6MB | 待填（阶段 3） | 子集脚本 + NOTICE |
| 池预生成 CPU | `[目标]` 平均 ≤15%（补 10 题睡 100ms） | 待填（阶段 3） | metric `pool_gen_ms` 直方图 |
| `/siteverify` p99 | `[目标]` <60ms | 待填（阶段 1 未做性能测量） | 基准测试 `TestBenchmarkSiteVerify` |
| `/api/challenge` p99 | `[目标]` <120ms | 待填 | 同上 |
| 混合吞吐 | `[目标]` ≥150 req/s 且无 `SQLITE_BUSY` | 待填 | `make loadtest`（阶段 5） |
| PoW 求解时延 | `[估算]` bits=18 浏览器 300–900ms | 待填（阶段 4，需真实设备） | widget 上报 `pow_impl` 与耗时 |

## 14. 安全实现清单（review 时逐条对照）

1. 密钥/摘要/token 的比较一律 `crypto/subtle.ConstantTimeCompare`（阶段 1 已覆盖 secret 比对与 tag 比对）。
2. token 解析前必须有总长度上限；payload 段用 `io.LimitReader` + `DisallowUnknownFields`，禁止无限制读取。
3. 所有 nonce/seed/随机答案参数来自 `crypto/rand`；`math/rand` 仅用于图形抖动且每次独立播种（阶段 3）。
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
9. 限流状态必须有硬上限与清理策略，避免攻击者用随机源地址撑爆内存（阶段 1 实现为有界令牌桶）。
10. admin（阶段 2）：admin_key 摘要存储、失败计数锁定（内存）、默认仅 localhost；审计表覆盖所有写操作。
11. 依赖：见 §16；引入任何新依赖需记录理由并由我确认。`GOTOOLCHAIN=local`，禁止构建过程隐式升级 Go。
12. 错误处理不吞异常；不 panic 出栈（中间件 recover + 通用文案）；安全关键状态**事务提交后才返回成功**（D12）。

## 15. 目录骨架（现状与计划）

```
cmd/server/          单二进制入口（flag→config→store→http）
internal/api/        HTTP 层：路由、校验、限流中间件、错误码
internal/pow/        PoW 签发与验证、难度校准
internal/token/      域分隔密钥派生、签名/验签、kid 轮换
internal/store/      SQLite 打开、PRAGMA、迁移、原子条件消费（阶段 1 实现）
internal/pool/       题池水位、预生成、claim 事务（阶段 3）
internal/generator/  Generator 契约（阶段 3 定稿并实现）
internal/risk/       有界令牌桶与来源地址裁决（阶段 1）；打分（阶段 3+）
internal/admin/      管理 API 与页面（阶段 2/5）
internal/config/     配置装载（flag + env + 站点文件，无 YAML 依赖）
internal/version/    构建信息（ldflags 注入）
cmd/devclient/       开发/验收客户端：真实暴力求解 PoW，驱动 stage1.sh（阶段 1）
web/widget/          v1.js + i18n + 样式（阶段 4）
deploy/docker/       Dockerfile + compose（阶段 5）
deploy/cloudflare/   Worker + wrangler.toml（阶段 5 之后评估）
internal/store/migrations/  NNNN_*.sql（阶段 1 起）——偏离"顶层 migrations/"是有意的：
                     go:embed 只能嵌入本包子树，放在仓库根就无法编进单二进制
docs/                DESIGN.md、DECISIONS.md、scripts/（验收脚本）
```
调整需说明理由；测试与被测文件同包同目录（`*_test.go`），集成测试用 `t.TempDir()` 建独立 DB，禁止依赖外部环境。

## 16. 依赖（阶段 1 已落地，详见 ADR-007）

| 依赖 | 版本 | 用途 | 许可 | 不选什么 |
|---|---|---|---|---|
| `modernc.org/sqlite` | **v1.46.1**（锁定） | 纯 Go SQLite 驱动，防重放账本持久化 | BSD-3-Clause（SQLite 本体 public domain） | `mattn/go-sqlite3`（需 cgo）；`v1.46.2+` 要求 Go≥1.25、`v1.60.x` 要求 Go≥1.26 → 会隐式升级工具链，禁止 |
| 标准库 `net/http` | — | 路由用 Go 1.22+ `ServeMux` 方法模式 | — | `gin`/`echo`/`chi`（无必要） |
| 标准库 `crypto/*` | — | HMAC-SHA256 签名与派生、`crypto/rand`、`crypto/subtle` | — | `golang.org/x/crypto/hkdf`（本设计每用途一把固定长度密钥，域分隔 HMAC 即可，见 §6） |
| 无 ORM | — | 手写 SQL 保证原子条件更新与可读性 | — | `gorm`/`sqlc` |

阶段 3 才可能需要 `golang.org/x/image`（`font/opentype` 等），**现在不引入**。传递依赖清单与许可记录在 ADR-007，我们只直接 import `modernc.org/sqlite`。
`Makefile` 设 `GOTOOLCHAIN=local`：构建过程不得下载或切换 Go 工具链；不擅自升级 Go。

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

**阶段 2 — 站点与 Key 管理、域名白名单、限流配置、审计**（SQLite 与迁移已提前到阶段 1）
验收：迁移幂等；创建站点仅一次性返回明文 secret，库中只有 `secret_mac`；域名白名单生效（Origin 不符 → `bad-request`）；
限流可按站点配置并触发 `429 + Retry-After`；`make secret-scan` 按值扫描通过。
测试覆盖：admin Bearer 鉴权矩阵、IP 掩码哈希稳定性（同 /24 同日同值、跨日不同值）、限流状态上限与内存不涨、审计覆盖所有写操作。

**阶段 3 — Generator 契约定稿 + `arith-v1` + `text-warp-v1` + 题池 + 一次性消费**
验收：池低于水位自动补至 `high_watermark`；payload 属性测试断言答案与可推导种子不在其中（各 500 题随机）；
并发生成/claim 在 `-race` 下无竞争、无重复下发；统计计数与并发写一致；字体缺失时启动即失败（不静默降级）。
**ADR-002 的硬门槛**：每个题型必须提交 M1–M4 四个指标，其中 M4（每次尝试成本）与 M3（猜测通过率）需带**本地基线测量**的
方法与日期；`rotate` 的"144"这类数字在完成测量前不得写成安全性论据。M3 在 3 次尝试下累计通过率 >10% 的题型不得启用。

**阶段 4 — Widget + 示例页**
验收：题型人工各过 5 次；`make widget-size` 断言 gzip `<30KB [目标]`；键盘-only 完成一题；`aria-live` 播报经 DOM 断言；
移动端 375px 无横向溢出；中英切换；PoW 高难度无障碍通道可用；失败重试与 fallback endpoint 生效；
并首次给出浏览器侧 PoW 时延的 `[实测]` 数据（设备、日期）。无法自动化的项必须标注"人工验证"，**不得声称自动化通过**。

**阶段 5 — 管理后台、统计与分级淘汰、Dockerfile 与部署文档；CF 边缘仅作评估项**
验收：管理页完成站点 CRUD/权重/池状态；淘汰规则按 ADR-006 分级（候选告警 + 人工确认，自动降权需多信号同时成立）且有测试；
`deploy/docker` 镜像非 root、`--db` 挂卷持久化、容器启动后 `make stage1-check` 仍通过；
`make loadtest` 在 1C1G 上给出 `[实测]` 吞吐/p99/RSS 并回填 §13（未达标要如实记录，不得写成达标）。
CF Worker/DO 仅在 §12 的重新评估结论为"做"时才实施，届时先补边缘原子性可行性验证。

## 18. 已知风险与未决问题（跟踪中）

| # | 风险/问题 | 现状 | 处理方向 |
|---|---|---|---|
| R1 | SHA-256 前缀 PoW 对 GPU 友好，成本上限有限 | 默认 `bits=18 [估算]` | 动态难度 + memory-hard 协议位（启用前需 `[实测]` 验证侧成本，§7） |
| R2 | 多模态模型可解字符图（`[估算]` 60–90%，**非本专案测量**） | 接受，且此数字待阶段 3 基线替换 | 一次性 + 尝试次数封顶 + 成本层叠加；不指望题目本身 |
| R3 | `cjk-v1` 成语语料与 `rotate-v1` 底图的再分发许可 | **未确认**，阶段 3 前必须定 | 自建/CC0/OFL；`docs/NOTICE.md` 清单 |
| R4 | 选择题答案空间仅 4（猜测通过率 25%，3 次尝试累计 ≤58%） | 这是题型的**主要弱点**，不能靠渲染混淆掩盖 | 阶段 3 用 M2/M3 正式量化；必要时改题型结构而非加噪声 |
| R5 | 边缘（Worker/DO）持状态会改变故障域与一致性模型 | 本版不在边缘放权威状态（工程选择，非平台限制，ADR-001） | 阶段 5 之后按 §12 的重新评估条件决定 |
| R6 | SQLite 单写者在 claim 高并发下的吞吐上限 `[估算]` | 未实测 | 阶段 5 压测给数字；超标则改内存队列 + 批量落库 |
| R7 | 无音频通道，纯视觉障碍用户体验 | 规划 PoW 高难度替代通道（阶段 4） | 阶段 4 评估音频（会引入编码依赖，暂不做） |
| R8 | widget 行为数据可伪造 | 设计为 ≤10% 弱权重 | 永不作为唯一 deny 依据（代码注释说明） |
| R9 | **§13 与 §7 的全部性能数字均未实测**，阶段 1 只做功能正确性 | 已按 `[目标]`/`[估算]` 标注 | 阶段 5 压测回填；在此之前不得据此做容量或选型决策 |
| R10 | 配额类外部事实会随平台变化（ADR-001 的教训） | 文档已改为"引用来源 + 使用时重新核对" | 任何平台限制写入文档时注明日期与出处 |

## 19. 变更日志

- 2026-10-17 v1：阶段 0 定稿（目录骨架、三层防线、API 契约、SQLite schema、密钥体系、CF 折衷、四题型反枚举设计、威胁模型与残余风险 R1–R8）。
- 2026-10-17 v1.1：模块路径改为 `github.com/shijianus/EpoCHA`（远端 private 仓库已确定）。§6 新增管理密钥引导与**密钥材料不入仓库**硬约束；真实密钥走 `/etc/epocha/env`（0600）与 DB 分离存放。理由见 DECISIONS。
- 2026-10-17 **v2.1（阶段 1 实现完成）**：按 v2 落地 `config/store/pow/token/risk/api/cmd`；实测二进制 9.19MB（回填 §13）；
  §10.1 明确 `internal-error` 是唯一超出 reCAPTCHA 族的扩展码且只随 5xx 出现；§15 记录 `migrations/` 实际位于
  `internal/store/migrations/`（`go:embed` 只能嵌入本包子树）。验收期间修掉四个真实缺陷：token tag 比较误将 base64 文本与解码字节相比；
  限流 `RetryAfter` 使用真实时钟而非注入时钟；devclient 随机起点可能为负导致提交非法 nonce（改为复用 `internal/pow` 判定）；
  `serve()` 用 `fmt.Fprintf` 与 slog 并发写同一 writer 触发数据竞争（启动信息统一走 slog）。
- 2026-10-17 **v2（阶段 1 前的设计校正）**：ADR-001 Cloudflare 事实更正与边缘定位改述；ADR-002 题库安全论证拆为 M1–M4 四指标并撤回"随机性 ⇒ 抗 AI"的推论；ADR-003 payload/verifier 分离、取消强制 HMAC 精确匹配；ADR-004 一次性消费的四条保证与实现手段分离（删除"单连接/`BEGIN IMMEDIATE` 是唯一合法实现"，纠正 `sql.Tx` 嵌套 BEGIN 错误）；ADR-005 防重放持久化提前到阶段 1（含新增 §5.1 实际 schema）；ADR-006 数字标注 `[目标]/[估算]/[实测]`、淘汰改分级、安全状态提交后才返回成功；ADR-007 `modernc.org/sqlite` 锁 v1.46.1 与 `GOTOOLCHAIN=local`；ADR-008 两处措辞收紧。**原始错误表述只在 `docs/DECISIONS.md` 留存。**
