# epocha 自托管人机验证服务 — 技术方案 v1（阶段 0 定稿）

> 状态：设计基线。阶段 1 起若与本文件冲突，先改本文件（在 §19 记录变更原因）再写代码。
> 读者：后续会话的工程师。假设你完全没有上下文。

## 1. 目标 / 非目标

**目标**
- 单二进制自托管人机验证服务，调用方式与 reCAPTCHA v2 语义兼容（`POST /siteverify`，字段 `secret` / `response` / `remoteip`）。
- 核心防线是**生成式题池**：题目由可插拔生成器程序化生成，每题随机参数 + 随机变换 + 一次性使用 + 用后即焚；不存在"下载来的固定题库"。
- 三层防线联合抬高攻击成本：PoW 成本门槛 → 生成式题目 → 风险打分决策。
- 多站点、多 Key、每站点独立难度/题型权重/TTL/域名白名单。
- 部署在 1 vCPU / 1GB / 50GB VPS，常驻 RSS 目标 <100MB。可选 Cloudflare Workers 免费版承载无状态部分。

**非目标（明确不做）**
- 不是 WAF/反爬虫平台，不做设备指纹画像，不做跨站点追踪，不采集个人数据（IP 只存加盐哈希截断值）。
- 不承诺"不可破解"。本设计只承诺"攻击成本可度量、可随题型轮换与难度调节而提高"（见 §4 威胁模型与 §18 残余风险）。
- 不做 reCAPTCHA 的 invisible/ score-only 模式，不实现 hCaptcha/Turnstile 的私有协议（只兼容 `siteverify` 的对外契约）。
- 不提供商业 SLA、不做多节点集群（SQLite 单写者决定了它是单机方案）。

## 2. 关键决策摘要

| # | 决策 | 理由 / 代价 |
|---|---|---|
| D1 | Go 单体 + 纯 Go 依赖，禁止 cgo | 1GB 内存、静态编译、Docker 镜像小；cgo 交叉编译复杂 |
| D2 | SQLite（WAL）唯一持久化，禁止 Mongo/Postgres/JVM | 运维面最小；代价：单写者，claims 吞吐上限约数千/s（1 vCPU 足够本用途） |
| D3 | 驱动用 `modernc.org/sqlite`（纯 Go） | 无 cgo；代价：二进制体积 +10MB 左右、性能低于 cgo 版（本负载不敏感） |
| D4 | 三层防线，PoW 永远是第一层，题目不可单独作为防线 | 单独图片/文字题已无法抗多模态模型与打码服务（§4） |
| D5 | 答案只存 HMAC 摘要，明文永不落库、永不下发 | 题目生命周期内 DB 泄露不可直接利用；代价：验证只能是离散精确匹配 → 容差靠分桶实现 |
| D6 | 站点 `secret_key` 入库为带 pepper 的 HMAC 摘要，仅创建时明文展示一次 | DB 泄露不等于凭据泄露；代价：无法找回，只能轮换 |
| D7 | token 用 HKDF 派生密钥签名（按用途分域），站点签名密钥由主密钥派生 | 不落库每站点密钥，支持 kid 轮换 |
| D8 | 防重放、题池、统计全部在 VPS 主节点，CF 边缘只做无状态签发与静态缓存 | 免费 KV 只有 1000 写/天、D1 10 万行/天；Durable Objects 需付费 |
| D9 | 管理 API 独立路径前缀 `/api/admin/*` + Bearer admin_key；管理台页面同源嵌入 | 与公开 API 分离便于反向代理层二次限流 |
| D10 | widget 原生 JS 零依赖，`go:embed` 打进二进制 | 无构建工具链，<30KB gzip 可验证 |

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
                                              │  token/ ─ HKDF+HMAC 签名令牌   │
                                              │  risk/ ─ 频率/时延/行为打分     │
                                              │  generator/ ─ 题型插件         │
                                              │  pool/ ─ 题池水位与预生成      │
                                              │  store/ ─ SQLite(WAL)+迁移     │
                                              │  admin/ ─ 站点/Key/配置/统计   │
                                              └────────────────────────────────┘
```

**一次完整验证的生命周期**
1. `POST /api/challenge`：校验 site_key + 域名白名单 + 限流 → 从池中原子领取一道 ready 题目（不足水位则后台补）→ 写 `sessions` 行（含 nonce）→ 返回 PoW 参数、题目 payload、签名 `challenge_token`。
2. 浏览器并行做 PoW（SHA-256 前缀）与解答题目 → `POST /api/redeem`。
3. `POST /api/redeem`：验签 token（含站点绑定与 TTL）→ PoW 验证 → **原子消费题目与 nonce** → 答案摘要比较（常数时间）→ 风险打分 → 通过则签发一次性 `response` token（TTL 120s）。
4. 业务服务器 `POST /siteverify {secret, response, remoteip}` → 验签 + **原子置 used** → 返回 reCAPTCHA 兼容 JSON。

任一层失败均返回**同一族错误码**（不透露具体哪层失败），避免给攻击者反馈信号。

## 4. 信任边界与威胁模型（必须诚实）

**信任边界**
- 客户端不可信：payload、行为数据、PoW 解全部视为攻击者可控输入；widget 上报的行为信号只作弱证据。
- 网络不可信：`CF-Connecting-IP` 仅在显式开启 `trust-proxy` 且源站防火墙只放行 CF IP 段时才可信，否则可被伪造。
- DB 泄露视为"部分泄露"：答案摘要、站点密钥摘要均不可逆（但依赖 §6 的 pepper 与主密钥分离存储）。
- 管理 API 视为高危面：默认只监听 localhost + 需 Bearer key，反代侧再加一层 IP 限制。

**攻击者能力与对应缓解**

| 攻击 | 能力假设 | 缓解 | 残余风险 |
|---|---|---|---|
| 通用多模态模型直接答题 | 单次调用成本 ~$0.001–0.01，准确率对扭曲字符 60–90% | 参数化随机 + 字体/扭曲/噪声/干扰 + 题目一次性 + 答案空间≥2^16 | 高难度题仍可能被解；靠 PoW+频率层把"每题多次尝试"变成付费成本 |
| 打码服务人工/半自动破解 | 市场价约 $0.6–2 / 1000 次，延迟 5–60s | 题目 TTL 180s、redeem 时延阈值、答案分桶随机化（同一题不可复用） | 延迟容忍型攻击（慢速注册）有效但单位成本线性上升 |
| PoW 外包到 GPU/集群 | 单卡 10^10–10^11 SHA-256/s | PoW 与站点/nonce 绑定（不可摊销复用）；难度按风险动态提升；预留 memory-hard 协议位 | SHA-256 前缀对 GPU 友好，绝对成本上限有限（见 §7 校准与 §18） |
| 令牌重放 / 一题多答 | 截获 token 或并发抢答 | nonce 与题目行原子消费（`UPDATE ... WHERE status='ready'` 判 RowsAffected）；response 单次使用 | 极窄的双花窗口依赖 SQLite 单写者串行化，需并发测试守护 |
| 答案枚举（选择题类） | 不解题，纯猜 | 每题答案空间下限（≥2^10）；同 IP/同站点失败率纳入风险分；失败即重出题 | 1/N 猜中率无法归零，业务侧必须把验证当作"提高成本"而非"绝对门" |
| 题池预生成 DoS（打空池） | 大量请求 /api/challenge 耗尽池 | 池有上限与水位、按 IP+site 限流、池被攻击性消耗时降级为同步生成并返回 429 | 1 vCPU 上限即吞吐上限，需 CF 挡在门外 |
| DB 读泄露 | 只读 SQL 注入/备份泄露 | §5 全参数化查询、§6 摘要存储、pepper 独立于 DB 文件 | 活跃题目在其 TTL 内可被离线暴力猜测（答案空间小的题型） |
| 源站 IP 暴露后绕过 CF | 证书透明/历史 DNS 泄露 | 源站防火墙只放行 CF 段；`/api/*` 与 `/api/admin/*` 分离限流 | 需运维正确配置，文档给出检查清单 |

**明确写下的结论**：本系统的安全性来自"成本 + 一次性 + 轮换 + 决策层"，不来自"题目难"。任何单一题型被攻破是预期事件，由 §9 的统计自动降权/淘汰处理。

## 5. 数据模型（SQLite）

连接串与 PRAGMA（DSN 参数，阶段 1 固定）：
`journal_mode(WAL) synchronous(NORMAL) busy_timeout(5000) foreign_keys(ON) cache_size(-8000)`，`db.SetMaxOpenConns(1)`（写者串行，避免 SQLITE_BUSY 风暴；读多写少场景够用）。

```sql
-- 0001_init.sql（阶段 2 起以 migrations/NNNN_*.sql 追加，只增不减）
CREATE TABLE sites (
  id             INTEGER PRIMARY KEY,
  name           TEXT NOT NULL,
  site_key       TEXT NOT NULL UNIQUE,          -- 公开标识，随机 16B base32
  secret_mac     TEXT NOT NULL,                 -- HMAC(pepper, secret) hex
  created_at     TEXT NOT NULL,
  revoked_at     TEXT,
  pow_bits       INTEGER NOT NULL DEFAULT 18,   -- 12..24
  challenge_ttl_sec INTEGER NOT NULL DEFAULT 180,
  rate_per_ip_rpm   INTEGER NOT NULL DEFAULT 30,
  risk_threshold    REAL NOT NULL DEFAULT 0.6,
  min_score_action  TEXT NOT NULL DEFAULT 'pass' -- pass|harder|deny
);
CREATE TABLE site_domains (site_id INTEGER NOT NULL REFERENCES sites(id), host TEXT NOT NULL, PRIMARY KEY(site_id, host));
CREATE TABLE generator_config (
  site_id INTEGER NOT NULL REFERENCES sites(id),
  generator_id TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  weight INTEGER NOT NULL DEFAULT 100,          -- 0..1000，含自动降权结果
  difficulty INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY(site_id, generator_id)
);
CREATE TABLE pool (
  id INTEGER PRIMARY KEY,
  generator_id TEXT NOT NULL,
  difficulty INTEGER NOT NULL,
  status TEXT NOT NULL,                         -- ready|claimed|consumed|expired
  payload_json TEXT NOT NULL,                   -- 下发给客户端，绝不含答案
  asset_blob BLOB,                              -- 图片二进制，可空
  answer_mac TEXT NOT NULL,                     -- HMAC(pepper, canonical_answer)
  created_at TEXT NOT NULL,
  claimed_at TEXT, expires_at TEXT NOT NULL,
  solve_ms_p50_hint INTEGER                     -- 生成时估计的人类耗时（分档用）
);
CREATE INDEX pool_claim ON pool(status, generator_id, difficulty, expires_at);
CREATE TABLE sessions (                          -- 一次 challenge→redeem→verify 的链路
  nonce TEXT PRIMARY KEY,                        -- 16B base64url，原子消费对象
  site_id INTEGER NOT NULL REFERENCES sites(id),
  pool_id INTEGER,
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
  ip_mac TEXT,                                   -- HMAC(pepper, ip掩码+日) — 隐私
  pow_bits INTEGER NOT NULL, pow_solved INTEGER NOT NULL DEFAULT 0,
  redeem_at TEXT, risk_score REAL, verdict TEXT,
  response_id TEXT,                              -- 关联 responses
  used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE responses (
  id TEXT PRIMARY KEY,                           -- token 的 jti（不存 token 原文）
  nonce TEXT NOT NULL REFERENCES sessions(nonce),
  site_id INTEGER NOT NULL REFERENCES sites(id),
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL, used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE gen_stats (                          -- 自动淘汰的输入
  day TEXT NOT NULL, generator_id TEXT NOT NULL, site_id INTEGER NOT NULL,
  shown INTEGER NOT NULL DEFAULT 0, passed INTEGER NOT NULL DEFAULT 0,
  failed INTEGER NOT NULL DEFAULT 0, suspect INTEGER NOT NULL DEFAULT 0,
  solve_ms_sum INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(day, generator_id, site_id)
);
CREATE TABLE audit (ts TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL, detail TEXT);
CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);  -- schema_version 等
```

清理任务（后台 ticker，每 60s）：`consumed`/过期 pool 行**删除**（用后即焚），过期 sessions/responses 删除，`gen_stats` 保留 90 天。硬约束：pool 表行数上限（默认 20000）与磁盘占用告警。

**查询安全**：一律 `db.Query(sql, args...)` 参数化；动态列名/排序（统计接口）必须白名单映射，禁止字符串拼接。

## 6. 密钥体系与 token

**主密钥**：`EPOCHA_MASTER_KEY`（32B，env 或 `--master-key-file`，权限 0600）；**pepper**：`EPOCHA_PEPPER`（32B，与 DB 文件分离存放，独立挂载）。两者缺一服务拒绝启动（不静默降级）。

派生（HKDF-SHA256，`crypto/hkdf` 手写用 `golang.org/x/crypto/hkdf` 或标准库等价实现）：
```
K_chal  = HKDF(master, "epocha/v1/challenge", "")
K_resp  = HKDF(master, "epocha/v1/response",  "")
K_site  = HKDF(master, "epocha/v1/site/" + site_key, "")
kid     = hex(sha256(master)[:8])
```
**token 线格式**：`v1.<kid>.<b64url(payload_json)>.<b64url(tag)>`，`tag = HMAC(K_purpose, "epocha:v1:" + <b64url(payload)>)`。
payload 必填字段：`typ`（`chal`|`resp`）、`kid`、`jti`、`sid`（站点 id，硬绑定）、`n`（nonce，仅 chal）、`exp`、`iat`。
验证顺序：长度上限（≤4KB）→ 解析分段 → kid 查表（支持双活轮换）→ **常数时间比 tag** → `exp`/`iat` 时钟窗口 → `sid` 与请求 site_key 一致 → 业务层原子消费。任何一步失败返回 `bad-request`，不区分原因。
轮换：`kid` 允许同时存在 current + previous（`--master-key-previous`），旧 token 在 TTL 内自然过期。

`secret_key` 校验：`want = HMAC(pepper, provided_secret)`，`subtle.ConstantTimeCompare(want, site.secret_mac)`。admin_key 同法（存摘要，不存明文）。
**管理密钥引导**：库中无 admin 记录时，用 `EPOCHA_ADMIN_KEY` 登记其摘要（只此一次，之后靠 `/api/admin/keys/rotate` 与审计管理）；有记录后该环境变量忽略。
日志红线：禁止输出 `secret`、`response`/`token` 全文、`answer`、payload 完整 JSON、未掩码 IP。允许输出：nonce 前 8 字符、site_key、耗时、错误码。

**密钥材料的存放（硬约束）**：`EPOCHA_MASTER_KEY` / `EPOCHA_PEPPER` / 站点 `secret_key` / `admin_key` **一律不进仓库**，即使仓库是 private。
三条理由：(1) git 历史不可撤销，泄露后只能轮换全部密钥；(2) private 的访问面不等于零——任何带 repo 范围的令牌、CI 产物、镜像层、未来的可见性切换或开源导出都会带出历史；
(3) 本设计的安全前提是 **pepper 与 DB 分离存放**（§5 的答案摘要、§6 的密钥摘要都依赖它）。把两者放进同一个仓库/同一次备份 = 摘要保护归零。
存放方式：`/etc/epocha/env`（`0600`，属主为服务账号）或 systemd `EnvironmentFile`/密钥管理，与 `/var/lib/epocha/*.db` 分目录、分备份策略。
仓库里只放 `.env.example`（占位符）。题库、语料、字体、图片素材属非机密资产，可入库（R3 许可另议，>100MB 改用 LFS 或派生 seed）。

## 7. PoW 层

- 协议 `sha256-prefix`：服务端给 `seed`（16B 随机，绑定 nonce）与 `bits`；客户端找 `nonce_c` 使 `SHA256(seed || "<nonce_c>")` 前 `bits` 位为 0。服务端验证 = 1 次哈希。
- 难度语义：期望尝试次数 = 2^bits。默认 `bits=18`（≈26 万次哈希）；在真实浏览器（WebCrypto 循环，约 3–8 万 digest/s）下期望 300–900ms，移动设备上秒级；风险升高时服务端对同一站点升到 20–22。
- 关键约束：`seed` 与 `nonce` 一一绑定，PoW 解**不可跨站点/跨题目复用**，打码农场无法摊销。
- 时延证据：记录 `redeem_at - created_at` 的 `solve_ms`；过快（< 理论下限的 20%）视为可疑信号 → 计入风险分与 `suspect` 统计。
- **预留 memory-hard 协议位**（阶段 5+ 或 v2）：`scrypt/argon2id` 变体在 WASM 内计算。注意：memory-hard 使服务端验证也变贵（每次验证需重算一遍，1 vCPU 下约 30–50ms → 验证吞吐上限降到 20–30/s），因此**默认不启用**，作为文档化的可选强化项。
- 客户端实现选择（阶段 4 定）：`crypto.subtle.digest` 分批（每批 200 次，避免卡主线程）或内嵌 3KB WASM SHA-256。widget 必须上报 `pow_impl` 供难度校准。

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
    Payload  any    // 下发给客户端（JSON），绝不含答案
    Answer   Answer // 只进服务端，落库前摘要化
    Asset    []byte // 图片字节，可 nil；由签名 URL 单次取回
    MimeType string
}
type Answer struct{ Canonical string } // 规范化字符串，作为 HMAC 输入与比较基准
```
**为什么传 `io.Reader` 而不是 `*math/rand.Rand`**：决定答案的随机量（数值、正确选项位置、旋转角）必须直接来自 CSPRNG，
把 `*rand.Rand` 放进契约会诱导实现用它抽答案参数。仅图形抖动（扭曲、噪声、干扰线）可以由 entropy 播种的 `math/rand`
承担，且每次调用新建、禁止共享实例。
`Answer.Canonical` 生成时**量化为离散值**（数值取整、角度分桶、选项 id）→ 客户端提交离散值即可精确校验，无需容差逻辑。
注册表 `generator.Register(g)`；`pool` 按站点权重加权随机选择生成器；`--generators` 可白名单启停。
渲染仅用纯 Go：`image`、`image/draw`、`golang.org/x/image/font` + `opentype`；字体 `go:embed`。

### 8.2 四种题型与"不可枚举/不可复用"设计

| ID | 交互 | 参数随机空间 | 一次性与反枚举手段 |
|---|---|---|---|
| `arith-v1` 算术/逻辑文字题 | 输入数字 | 模板 × 数值 × 顺序：模运算、等差/等比数列下一项、日期差、混合运算取模 | 模板库 40+ × 参数区间 ≥10^4 → 实例空间 ≥4×10^5；答案域≥10^4；干扰文本每次随机（同题重排）；一次性：题目行 claim 后 status 变更 |
| `text-warp-v1` 扭曲字符图 | 输入字符串 | 5–7 字符 × 3–6 种嵌入字体随机指派 × 逐字旋转/错切/贝塞尔扭曲 × 贝塞尔干扰线 × 椒盐噪声 × 字符间距抖动 | 答案域 32^n（n≥5）> 3×10^7；字符集去易混（无 0/O/1/l/I）；字体随机使固定 OCR 模板失效；同一文本永不复用（生成后写 bloom 去重） |
| `cjk-v1` 中文/成语选择题 | 点选 4 选项中的正确项 | 成语库（自有、可再分发语料）× 4 干扰项组合 × 遮罩/描边/色相扰动 × 问法模板（选正确/选错误/选字形匹配） | 答案空间 4 选项 → **单题 N=4 不够**，故叠加：选项渲染为图像（OCR+对齐成本高）、正确项位置随机、每题答案空间扩展为"4 选 1 且需同时通过 PoW"；失败重出 3 次即锁定；语料版权由 §18 单独跟踪 |
| `rotate-v1` 图片旋转/拼合 | 拖动滑块把图转正 | 底图来源 × 旋转角 θ∈[0,360) 连续 × 分桶 15°（24 桶）× 翻转/裁剪/水印噪声 | 连续角 → 离散 24 桶，答案空间 24（配合 2 段式：先粗角后精角 → 有效 24×6=144）；底图取自服务端自有/CC0 素材池，每次裁剪区域随机 → 同素材不同题；一次性由 pool 行保证 |

**统一约束（代码层强制，测试守护）**
- `Challenge.Payload` 序列化后必须经 `assertNoAnswer(payloadJSON, answer)` 检查（答案子串/数值不得出现在 payload）。
- 单题尝试次数 ≤3（超限作废并计入风险）；答案空间下限常量 `MinAnswerSpace = 1<<10`，生成器自测（property test）验证每题型 ≥该下限。
- 生成时记录 `solve_ms_p50_hint`（按难度档静态表 + 题型），用于 §9 时延异常判定。
- 素材与语料必须本地自有或许可证允许再分发（OFL 字体 / CC0 图片 / 自建成语表）；许可证清单落 `docs/NOTICE.md`。

### 8.3 题池水位与预生成

```
low_watermark  = clamp(2 * 站点数, 50, 500)   # 低于则补
high_watermark = 2 * low_watermark            # 补到此停止
max_rows       = 20000                        # 硬上限，超出拒生成并告警
worker         = 单个后台 goroutine，priority=让出 CPU（每补 10 题 sleep 100ms）
```
- 启动时预热 `low_watermark` 题（否则首请求同步生成，延迟 >100ms）。
- claim：`BEGIN IMMEDIATE; SELECT id FROM pool WHERE status='ready' AND generator_id IN (?) ... ; UPDATE pool SET status='claimed' ...; INSERT sessions...; COMMIT` —— 单事务保证"领题 + 建 session"原子。
- 池空/被耗尽：同步生成（走同一 `Generate`）并打 metric `pool_miss`，连续 5 次 → 返回 429 并告警。

## 9. 风险打分层（第三层，决策）

`score ∈ [0,1]`，加权求和（阶段 3 起实现，权重存站点配置，默认值见代码常量）：

| 信号 | 权重 | 说明 |
|---|---|---|
| 频率：同 IP 掩码 5min/24h 请求数与通过率 | 0.30 | 唯一硬信号，可作 deny 依据 |
| PoW 时延 vs 理论下限 | 0.20 | 过快 = 可疑（自动化集群） |
| challenge→redeem 总时延 | 0.15 | <3s 完成难题 或 >TTL 均异常 |
| 答题正确但答案命中"高频模板" | 0.10 | 检测模板枚举 |
| widget 环境自检（WebGL/canvas 指纹、无头特征、`navigator` 一致性） | 0.10 | **弱信号，客户端可伪造，仅作 tie-breaker** |
| 语言/UA/时区/Referer 与站点历史分布偏离 | 0.15 | 仅统计偏离，不做身份判定 |

决策：`score < risk_threshold` → 签发 response；`threshold..threshold+0.2` → 提高难度重出题（`harder`）；更高 → `deny`。
限流：令牌桶（内存，`map[ip_mac]*bucket`，LRU 上限 65536 项防内存膨胀）按 `ip` 与 `site_key` 两维；超限 `429 + Retry-After`。持久封禁（可选）写 `meta` 中的 ban 列表，避免重启清零。

## 10. API 契约

### 10.1 公开 API

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/api/challenge` | `site_key` | 领题 + 返回 PoW 参数与 token |
| POST | `/api/redeem` | token | 提交 PoW 解 + 答案 + 行为数据 |
| POST | `/siteverify` | `secret` | reCAPTCHA 兼容，服务端调用 |
| GET | `/asset/<pool_id>/<sig>` | 签名 | 题目图片，`Cache-Control: no-store` |
| GET | `/widget/v1.js` | 公开 | widget 静态资源（gzip） |

`POST /api/challenge` — 请求（`application/json`，≤2KB）
```json
{ "site_key": "7QK4M2ZR6YTB8VXF", "action": "signup", "lang": "zh-CN" }
```
响应 `200`
```json
{
  "token": "v1.ab12cd34.eyJ0eXAi...<payload>.<tag>",
  "expires_in": 180,
  "pow": { "algo": "sha256-prefix", "seed": "base64url(16B)", "bits": 18 },
  "challenge": {
    "id": "rotate-v1", "media": "image",
    "asset_url": "/asset/9134/e5f0...?t=1700000000",
    "task": { "kind": "rotate", "prompt": "把图片转到正立", "buckets": 24, "coarse_only": false }
  }
}
```

`POST /api/redeem` — 请求（≤4KB）
```json
{ "token": "v1.ab12cd34...", "pow_nonce": "8f2k...", "answer": "9",
  "behavior": { "solve_ms": 1420, "pow_ms": 610, "pow_impl": "webcrypto", "lang": "zh-CN",
                "env": { "webdriver": false, "tz": "Asia/Shanghai", "dpr": 2 } } }
```
成功 `200`：`{ "response": "v1.ab12cd34.eyJ0eXAi...<resp payload>.<tag>", "expires_in": 120 }`
失败 `4xx/429`：`{ "error-codes": ["invalid-input-response"] }`

`POST /siteverify` — 请求（`application/json` 或 `application/x-www-form-urlencoded`，与 reCAPTCHA 一致）
```json
{ "secret": "sk_live_9f3c...", "response": "v1.ab12cd34...", "remoteip": "203.0.113.7", "hostname": "app.example.com" }
```
成功 `200`
```json
{ "success": true, "challenge_ts": "2026-10-17T08:12:44Z", "hostname": "app.example.com" }
```
失败 `200`（reCAPTCHA 失败也返回 HTTP 200）
```json
{ "success": false, "error-codes": ["timeout-or-duplicate"] }
```
错误码集合（对外只用 reCAPTCHA 已定义的族，内部原因只进审计日志）：
`missing-input-secret` / `invalid-input-secret` / `missing-input-response` / `invalid-input-response` / `bad-request` / `timeout-or-duplicate`。
**不返回** `scores` 等扩展字段以维持兼容；业务方需要分数须走内部 `/api/admin/verdicts`（阶段 5，可选）。

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

**Cloudflare 边缘（可选，阶段 5）**
- Worker 职责（严格无状态）：`/widget/v1.js` 与素材的 `Cache-Control` 缓存；`/api/challenge` 的 **PoW 参数签发**（用同一 master key 的 HKDF 派生密钥签 `chal` token，不含 nonce 记录）。
- Worker **绝不承担**：nonce 防重放、题池、统计写入。理由：免费 KV 1000 写/天、D1 10 万行/天且高延迟、Durable Objects 需付费计划；Worker 单次 CPU 10ms 上限 → 只做 HMAC + 少量 JSON，验证 PoW（1 次 digest）可以但会占用配额。
- 边缘模式语义折衷：Worker 签发的 token 无 nonce 记录，**防重放完全依赖 redeem 时源站原子消费 response token**（`responses.used` 置位 + TTL 120s）。文档必须写明："边缘签发 = 弱一致模式，只适合低价值表单；严格一次性请用源站签发"。
- 免费额度测算：10 万请求/天 ÷ 每次验证 3 请求（challenge + asset + redeem）≈ 3.3 万次验证/天，超出即 5xx 计费/限流 → 默认建议 DNS-only 或只走静态缓存。
- 降级路径（failback）：Worker `fetch` 源站超时 2s → 返回 `{"error":"edge_unavailable"}`，widget 按 §11 自动切 `fallback_base` 直连源站；源站不可用时验证失败（**安全优先：宁可对用户显示"暂时无法验证"，不做静默放行**）。管理后台与 `/siteverify` 永不走边缘。
- 源站保护：开启 CF 代理时必须同时启用 IP 白名单（只放行 CF 段）+ `trust-proxy` 取 `CF-Connecting-IP`；`docs` 提供自检 curl。

## 13. 资源与性能预算（1 vCPU / 1GB / 50GB）

| 项 | 预算 | 守护方式 |
|---|---|---|
| 常驻 RSS | <100MB（Go 基线 8–15MB + freetype 光栅 ~15–25MB + SQLite page cache 8MB + 池缓冲） | `GOMEMLIMIT=56MiB`；测试断言峰值 RSS（`/proc/self/status`） |
| 二进制体积 | ≤35MB（modernc/sqlite + x/image） | `make size` 打印 |
| 内嵌字体 | Latin 1–3MB；CJK 子集（常用 3500 字）4–6MB | 子集脚本 + NOTICE |
| 池预生成 CPU | 平均 ≤15% CPU（补 10 题睡 100ms） | metric `pool_gen_ms` 直方图 |
| `/siteverify` p99 | <60ms（验签 1 HMAC + 1 次 UPDATE + 1 SELECT） | 基准测试 `TestBenchmarkSiteVerify` |
| `/api/challenge` p99 | <120ms（池命中）/ <400ms（同步生成） | 同上 |
| 混合吞吐 | ≥150 req/s 且无 SQLITE_BUSY | `make loadtest`（hey/k6，阶段 5） |

## 14. 安全实现清单（review 时逐条对照）

1. HMAC 比较、`secret_mac`/`answer_mac` 比较 → `crypto/subtle.ConstantTimeCompare`。
2. token 解析前必须有长度上限；payload 段不得 `json.Decoder` 无限制读取（用 `io.LimitReader` + `DisallowUnknownFields`）。
3. 所有 nonce/seed/随机答案参数来自 `crypto/rand`；`math/rand` 仅用于图形抖动且每次 `rng` 独立播种。
4. 一次性消费必须 `UPDATE ... WHERE status='ready'`（或 `used=0`）且检查 `RowsAffected == 1`，写在 `BEGIN IMMEDIATE` 事务内。
5. HTTP：请求体 `http.MaxBytesReader`（公开 4KB / 管理 64KB）；`Read/Write/Idle Timeout` 全设；`CORS` 白名单来自站点配置（禁 `*` 与回显 Origin 不带校验）；`X-Content-Type-Options: nosniff`、`Cache-Control: no-store` 于 API 响应。
6. SQL 参数化 100%；动态列名走白名单。
7. 日志：结构化 slog，字段白名单；`secret`/`token`/`answer`/完整 payload 一律不出现。
   `make secret-scan` **按值扫描**：测试运行时注入已知高熵哨兵值（如 `SENTINEL_SECRET_<rand>`），
   断言日志、错误响应、audit detail 中不出现该值。按字段名 grep 会因 `admin_key`、`token` 等
   标识符产生假阳性，禁止这样实现。
8. admin：admin_key 摘要存储、失败计数锁定（内存）、默认仅 localhost；审计表覆盖所有写操作。
9. 依赖：仅 `modernc.org/sqlite`、`golang.org/x/image`（如需要 `golang.org/x/crypto` 用于 hkdf）；引入任何新依赖需在 §19 记录理由并由我确认。
10. 错误处理不吞异常；不 panic 出栈（中间件 recover + 500 通用文案）。

## 15. 目录骨架（现状与计划）

```
cmd/server/          单二进制入口（flag→config→store→http）
internal/api/        HTTP 层：路由、校验、限流中间件、错误码
internal/pow/        PoW 签发与验证、难度校准
internal/token/      HKDF 派生、签名/验签、kid 轮换
internal/store/      SQLite 打开、PRAGMA、迁移、查询（含原子消费）
internal/pool/       题池水位、预生成、claim 事务
internal/generator/  Generator 接口 + 注册表 + 各题型实现（arith-v1.go …）
internal/risk/       信号采集与打分、令牌桶
internal/admin/      管理 API 与页面
internal/config/     配置装载（flag + env，无 YAML 依赖）
internal/version/    构建信息（ldflags 注入）
web/widget/          v1.js + i18n + 样式（阶段 4）
deploy/docker/       Dockerfile + compose（阶段 5）
deploy/cloudflare/   Worker + wrangler.toml（阶段 5）
migrations/          NNNN_*.sql（阶段 2 起，go:embed）
docs/                本文件、AGENTS.md、部署/威胁模型补充
```
调整需说明理由；测试与被测文件同包同目录（`*_test.go`），集成测试用 `t.TempDir()` 建独立 DB，禁止依赖外部环境。

## 16. 依赖选择（阶段 1 落地时写入 go.mod）

| 依赖 | 用途 | 不选什么 |
|---|---|---|
| `modernc.org/sqlite` | 纯 Go SQLite 驱动 | `mattn/go-sqlite3`（需 cgo） |
| `golang.org/x/image`（`font/opentype`,`font`,`math/f64`） | 文本渲染与字体 | 外部字体二进制、ImageMagick、headless chrome |
| `golang.org/x/crypto/hkdf` | 密钥派生 | 手写 HKDF |
| 标准库 `net/http` | 路由用 Go 1.22+ `ServeMux` 模式 | `gin`/`echo`/`chi`（无必要） |
| 无 ORM | 手写 SQL 保证原子性与可读 | `gorm`/`sqlc`（阶段 0 判断：不必要复杂度） |

## 17. 开发阶段与验收标准

> 每阶段顺序固定为：**写测试（先红）→ 实现 → `make vet race` → 更新文档 → commit → tag `stage-N-done`**。
> 验收里带 `make` 目标的一律必须在 Linux/arm64 上真实跑绿才算完成。

**阶段 1 — PoW + token + /siteverify 最小闭环**
验收：`make stage1-check`（脚本 `docs/scripts/stage1.sh`）用 curl 走完 challenge→(python 算 PoW)→redeem→siteverify，输出 `"success": true`。
测试必须覆盖：① PoW 向量（已知 seed/bits/nonce → 验证 true/false）；② token 篡改任一字节 → `bad-request`；③ 过期 token、跨站点 token（`sid` 不符）→ 拒；④ 同一 response 二次 `siteverify` → `timeout-or-duplicate`；⑤ 缺 `secret`/错 `secret` → 对应错误码；⑥ 响应 JSON 的 key 集合与 §10.1 完全一致（含 `error-codes` 连字符）；⑦ 并发 100 次消费同一 nonce → 恰好 1 次成功。

**阶段 2 — 站点/Key 管理、限流、SQLite 迁移**
验收：迁移幂等（重复执行 → 版本不变、无错误）；创建站点仅一次性返回明文 secret，库中只有 `secret_mac`；域名白名单生效（Origin 不符 → `bad-request`）；限流触发 `429 + Retry-After`；`make secret-scan` 通过（按 §14-7 的值扫描方式，日志/错误响应/audit 中不出现注入的哨兵值）。
测试覆盖：迁移 up/down-兼容、admin Bearer 鉴权矩阵、IP 掩码哈希稳定性（同 /24 同日同值、跨日不同值）、令牌桶边界（LRU 上限内存不涨）。

**阶段 3 — Generator 接口 + `arith-v1` + `text-warp-v1` + 题池 + 一次性消费**
验收：池低于水位自动补至 `high_watermark`（测试注入低水位观察）；`payload` 属性测试断言答案不在其中（各 500 题随机）；并发生成/claim 在 `-race` 下无数据竞争、无重复下发（每题 id 唯一消费）；统计三计数（shown/passed/failed）与并发写一致；`solve_ms` 异常样本计入 `suspect`。
测试覆盖：答案空间下限（枚举模板×参数计算实例数 ≥2^10）、`answer_mac` 匹配（含大小写/空白规范化）、字体缺失时启动即失败（不静默降级）。

**阶段 4 — Widget + 示例页**
验收：浏览器中四种题型（此时至少两种）人工各过 5 次；`make widget-size` 断言 gzip <30KB；键盘-only 完成一题；`aria-live` 状态播报经截图/DOM 断言；移动端 375px 无横向溢出；中英切换正确；PoW 高难度无障碍通道可用；失败重试与 fallback endpoint 生效。
测试：`web/widget/test/*.html` 手工用例清单 + 一段 Playwright/无浏览器环境则标注"人工验证项"，**不得声称自动化通过**。

**阶段 5 — 管理后台、统计与自动淘汰、CF 边缘、Dockerfile 与部署文档**
验收：管理页可完成站点 CRUD/题型权重/池状态查看；自动降权规则（suspect 率 >15% 且 7 日样本 ≥200 → weight 减半，连续两轮 → enabled=0）有测试用例；`deploy/docker` 镜像非 root、`--db` 挂载可持久化、`make docker-run` 后 stage1 脚本仍通过；CF Worker 按 §12 部署文档跑通且 failback 经断网演练验证；`make loadtest` 在 1C1G 上 ≥150 req/s、p99 `/siteverify` <60ms、RSS <100MB。

## 18. 已知风险与未决问题（跟踪中）

| # | 风险/问题 | 现状 | 处理方向 |
|---|---|---|---|
| R1 | SHA-256 前缀 PoW 对 GPU 不友好抵抗 | 默认 `bits=18` 只增加线性成本 | 动态难度 + memory-hard 协议位（v2） |
| R2 | 多模态模型可解 `text-warp-v1`（准确率 60–90%） | 接受 | 一次性 + 时延信号 + 自动淘汰，不指望题目本身 |
| R3 | `cjk-v1` 成语语料与 `rotate-v1` 底图的再分发许可 | **未确认**，阶段 3 前必须定 | 自建/CC0/OFL；`docs/NOTICE.md` 清单 |
| R4 | 选择题答案空间仅 4（靠叠加成本弥补） | 弱 | 双段式答案（粗+精）扩到 ≥144；限制 3 次尝试 |
| R5 | CF 边缘签发模式下无严格一次性保证 | 设计折衷，已文档化 | 默认关闭，仅低价值表单建议 |
| R6 | SQLite 单写者在 claim 高并发下的吞吐上限 | 1 vCPU 目标负载内可接受 | 阶段 5 压测给出实测数字，超标则改内存队列+批量落库 |
| R7 | 无音频通道，纯视觉障碍用户体验 | 有 PoW 高难度替代通道 | 阶段 4 评估是否加音频（引入音频编码依赖，暂不做） |
| R8 | widget 行为数据可伪造 | 仅作 10% 弱权重 | 永不作为唯一 deny 依据（代码注释说明） |

## 19. 变更日志

- 2026-10-17 v1：阶段 0 定稿（目录骨架、三层防线、API 契约、SQLite schema、密钥体系、CF 折衷、四题型反枚举设计、威胁模型与残余风险 R1–R8）。
- 2026-10-17 v1.1：模块路径改为 `github.com/shijianus/EpoCHA`（远端 private 仓库已确定，SSH 推送为默认工作流）。§6 新增两条：管理密钥引导（`EPOCHA_ADMIN_KEY` 仅在建库首启时登记摘要）与**密钥材料不入仓库**的硬约束——`AGENTS.md` 同步；真实密钥走 `/etc/epocha/env`（0600）与 DB 分离存放、分离备份。非机密的题库/语料/字体/素材允许入库，>100MB 改 Git LFS 或记录派生 seed。
