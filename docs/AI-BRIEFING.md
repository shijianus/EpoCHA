# EpoCHA 项目简报（写给无法访问本仓库的 AI）

> 本文是 2026-11-09 一次快照式审计的输出，绑定分支 `fix/release-preparation-2026-11-02` @ `495ebf2`
> （与远端 `origin` 同 OID）。目的是让接触不到代码与文档的另一个 AI 准确理解：**这个项目是什么、
> 最终要什么、现在做到哪、哪些事实已验证、哪些必须保持"未完成"表述**。
> 仓库继续演进后，权威来源依次是 `AGENTS.md`（工程约束）、`docs/DECISIONS.md`（ADR）、
> `docs/DESIGN.md` §19（变更日志）。本文与它们冲突时，以它们为准。

---

## 0. 阅读纪律（先于一切事实）

本项目对"如何报告"有硬性约定，违反即视为错误输出，无论内容多合理：

1. **验收状态三态**：工程测试只有 **PASS / FAIL**；外部条件不足（缺工具、缺授权、缺真人）是 **BLOCKED**；
   本轮没执行是 **NOT_RUN**。**禁止**"PASS：某项未测"这类混合表达。
   "测阻塞检测逻辑通过" ≠ "效果评测通过"，两者必须分列。
2. **数字标注**：`[目标]`（设计目标）/ `[估算]`（未测估计）/ `[实测]`（真实测量，需带日期与环境）。
   不得把未实测的数字写成事实，不得把旧实测外推到新环境。
3. **frozen 分两层，不得混**：①**测试里程碑 frozen**（代码 + 链路 + 收据可提交、可复现）；
   ②**生产发布 frozen**（`release-check --profile full` 全 PASS + 人工/效果验收真实证据）。
   当前只达到①的部分状态，从未达到②。
4. **能力边界由能力测试给出**：不能把"我没实现的那个调用失败了"读成"平台不提供"（历史上 BiDi
   `setViewport` 就这样被误记成 BLOCKED，后实测可用并改正）。
5. **退出码折叠**：实测 `go run` 把任何非 0 折叠成 1、`make` 把任何配方失败折叠成 2；
   需要机器可读的真实退出码时必须先 build 再直接跑二进制/脚本。shell 里禁止 `cmd | head`/`| tail`
   掩盖上游退出码。
6. **无密钥/无实测不得声称**：任何题型都**不**声称"抗 AI / 抗 OCR / 抗多模态 / 抗打码"。
   全部题型保持 `experimental:true` / `production_ready:false`，直到 ADR-002 的 M1–M4 四指标
   全部有数据并通过人工基线。

---

## 1. 项目是什么（一句话与全貌）

**EpoCHA（epocha）是一个自托管的人机验证服务（CAPTCHA）**：单个 Go 二进制 + SQLite，
对外提供 reCAPTCHA **风格**的 `POST /siteverify` 契约（`secret`/`response`/`remoteip` →
`success`/`challenge_ts`/`hostname`/`error-codes`）。它**不声称**无条件兼容所有现成 reCAPTCHA
SDK（v2/v3 的额外参数未实现）。

三层防线设计：

1. **PoW 成本门槛**：SHA-256 前缀工作量证明（bits 12–24），服务端以库中行为权威复核，不接受客户端自报；
2. **生成式题池**：服务端预生成一次性题目（后台 worker 按水位补充），一题一 challenge，第一笔有效作答即终结；
3. **风险打分决策**：**尚未完整实现**。当前落地的是调度层信号（风险带 → 加权抽题、难度地板、冷却，
   见 §6.4），不存在自动拒绝/自动淘汰/统计面。

关键性质：一次性消费由**原子条件状态转移**（同一 SQL 内检查旧状态 + 写入新状态、同事务提交、
`RowsAffected == 1`、测试证明）保证，与进程数无关；答案明文不落库不下发不写日志
（verifier 是 `nonce || HMAC(K_qver, …)` 的域分隔派生密钥形态，或 TSV 标签集）；管理面在**独立监听器**
（默认 `127.0.0.1:8081`）上，公共端口没有任何管理路由。

- Go 模块：`github.com/shijianus/EpoCHA`；远端 `git@github.com:shijianus/epocha.git`（private，GitHub 大小写不敏感）
- 主分支 `main`；开发分支如 `fix/release-preparation-2026-11-02`
- 规模 [实测 2026-11-09]：git 跟踪的 Go 文件 143 个 / 47,414 行（实现 53 文件 20,748 行，测试 90 文件 26,666 行）

## 2. 最终期望（产品要达成什么）

设计基线 `docs/DESIGN.md`（v2.6）定义的终态：**可部署到 1 vCPU / 1GB RAM / 50GB 磁盘单节点 VPS
的生产级自托管验证服务**，常驻 RSS [目标] <100MB、二进制 [目标] ≤35MB。具体包括：

- **三层防线全部生效**：PoW + 题池 + 完整风险打分（评分、分级淘汰：候选告警 + 人工确认 + 多信号自动降权）；
- **题库达到可用强度**：多题型、按权重混合选型；每个题型通过 M1–M4 论证与人工可读性基线；
- **接入面完整**：零依赖 widget（gzip <30KiB、中英双语、键盘可完成、aria-live、移动端适配）、
  `fallback_base` 多源切换、非视觉等价通道（音频等）；
- **运维面完整**：备份/恢复/恢复演练节律、监控、统计、管理台全功能（题池观测、审计、凭据轮换视图）；
- **发布纪律**：干净候选 OID + 匹配 manifest + 当次完整验收（`release-check --profile full` = PASS/0）
  才允许真实部署。

**阶段边界是刻意保守的**：里程碑标签只打子阶段（`stage-3a-done` 等），**不打** `stage-3-done` /
`stage-4-done` / `stage-5-done` / `stage-5c-done`，因为每个大阶段的全部内容并不都完成。
"工程闭环完成" ≠ "效果验收完成"；"部署演练通过" ≠ "题型抗 AI 通过"。

## 3. 硬性约束（不可协商，改它需要用户明确批准）

- **只用 Go，纯 Go 依赖，禁止 cgo**。禁止 JVM/Mongo/Postgres/Redis/外部二进制（ImageMagick、
  fontconfig、headless chrome 等）。OCR/tesseract 只允许出现在隔离的**开发期评测容器**里
  （识别阶段 `--network none`），服务运行时不含不调用。
- **运行时依赖只有两个**（锁版本）：`modernc.org/sqlite v1.46.1`（ADR-007）、
  `golang.org/x/image v0.36.0`（字形光栅；v0.37+ 要求 go≥1.25 会隐式升工具链，故锁定）。
  新增任何依赖/系统安装/在线调用必须停下来问用户。
- **工具链 Go 1.27.3 + `GOTOOLCHAIN=local`**（ADR-020，用户 2026-11-02 显式授权的升级）：
  构建不得隐式下载/切换 Go 版本。
- **目标机形状**：1 vCPU / 1GB / 50GB；资源纪律是题池 worker 的第一设计约束（有界批次、让步、退避）。
- **widget**：原生 JS 零依赖，gzip [目标] <30KiB（[实测 2026-11-05] 18,998B）；`v1.js` 与
  `v1.worker.js` 的 Blob 路径与同源文件路径跑**逐字节相同**的源码（测试钉住）。
- **开发期代码隔离**：`internal/eval` 与 `cmd/{preview,devseed,evalgen,evalocr,exampleapp,loadtest}`
  是开发期代码，`cmd/server` 不得链接（有测试钉住 `go list -deps`）；生产 HTTP API 不得有答案出口。
- **密钥材料绝不入库**：`EPOCHA_MASTER_KEY`、`EPOCHA_PEPPER`、站点 `secret_key`、`admin_key`、
  `.env`、`*.key`、`*.pepper` 一律 gitignore；验收脚本用运行时随机演示密钥。
- **架构边界**：权威状态（防重放账本、题池、统计）在 VPS 源站 SQLite——这是工程选择不是平台限制；
  单实例是实现与运维边界（数据目录 `flock`），**不得**把它表述成"SQLite 不支持多进程并发"。

### 安全红线（16 条，全文见 AGENTS.md；最常被 AI 违反的几条）

- token 必须 HMAC-SHA256 签名、域分隔派生密钥；比较一律 `crypto/subtle.ConstantTimeCompare`；
- 一次性消费四条保证缺一不可；`*sql.Tx` 上严禁再 `BEGIN IMMEDIATE`；
- `secret_key` 只存带 pepper 的 HMAC 摘要，创建时展示一次不可找回；secret 校验先于任何状态消费；
- 公开 payload 与私有 verifier 数据类型级隔离；答案/种子/密钥绝不下发、不落库明文、不写日志；
  容差判分由题型实现，不得为 HMAC 精确匹配牺牲合理容差；
- 所有输入严格校验 + 大小上限 + 有界限流；SQL 100% 参数化；错误响应不透露哪一层失败；
- `crypto/rand` 用于安全语义随机；日志字段白名单，验证方式是按值扫描哨兵；
- 安全关键状态事务提交之后才返回成功；**没有测试就不要改安全相关代码**（先补测试）；
- 题型安全论证必须 M1–M4 分列；M3（3 次随机猜测累计）>10% 的题型不得启用；
  M4 未测的题型不得声称抗 AI/抗 OCR；
- 不得仅凭通过率自动下线题型（淘汰必须分级 + 人工确认）；
- 离线写数据目录的操作必须取 `internal/store/lock.go` 的排他锁；`backup`/`verify` 明确**不**取锁。

## 4. 架构与数据流（当前实现的真实形状）

```
浏览器（业务页面）
  └─ widget v1.js（Shadow DOM，零依赖，状态机 idle→loading→ready→solving→submitting→verified…）
       ├─ GET /widget/v1.js、/widget/v1.worker.js   ← 公共监听器（默认 127.0.0.1:8080）
       ├─ POST /api/challenge  → 校验 site_key/Origin →（PoW 参数 + 题目 DTO，事务内领题绑定）
       │     503 challenge-unavailable（池空，绝不降级 PoW-only）
       ├─ PoW 在 Dedicated Worker 内算（Blob 或同源文件，同字节源码）
       ├─ POST /api/redeem     → PoW 复核 + 答案判定（一次有效作答终结）→ 返回一次性 token
       │     （可选上报量化行为遥测摘要：6 个计数字段，弱证据，永不单独拒绝正确回答）
       └─ 业务后端 POST /siteverify（secret + response[+remoteip]）→ success/error-codes
             （**唯一授权点**；浏览器 onSuccess 不是授权）

管理面（独立监听器，默认 127.0.0.1:8081，从不发布到公网）
  ├─ /admin + /admin/app.js|app.css   内嵌管理控制台（5C-M1：登录/站点列表/创建/详情编辑/启停）
  └─ /api/admin/*                     站点与凭据生命周期、来源策略、题型与题池只读观测、审计

后台 worker（同进程）：题池预生成（水位迟滞、有界批次、CPU 让步、指数退避、
  图片字节预算、分桶水位 + 升档预热桶 d_base+1..+3）
```

- **数据层**：SQLite，`schemaVersion=6`（迁移 0001..0006：基础表 / 管理面 / 题池与一次性消费 /
  图片资产列 / payload 上限重建为 8KB / 站点多题型池字段）。备份用 `VACUUM INTO` 在线一致性快照
  （不是 `cp`），恢复是**保守恢复**（快照内一切可被接受的东西一次性失效 + 新 master + 新管理凭据）。
- **密钥体系**：主密钥按 purpose 域分隔派生 `chal`/`resp`/`qver` 三把（签发 challenge、签发 token、
  题目判分），外加 pepper（站点 secret 与管理凭据摘要）。支持 `EPOCHA_<NAME>_FILE` 密钥文件入口
  （env 与 `_FILE` 同时非空 ⇒ 拒绝启动；文件 group/other 权限位必须为空）。
- **verifier 三种形态**：48B 精确 HMAC（v1/v2 文字题）、208B 逐字 HMAC（v3，允许 ≤1 字符替换）、
  TSV Tag-Set Verifier（`ETSV` v1，≤127 tag，交互题连续容差：angle 0.1° 量化、point 千分位坐标、
  timing 毫秒相位）。TSV 只能测试"候选是否命中集合"，不存答案明文或可逆种子。
- **verifier/payload/资产上限**：MaxVerifierBytes 4096、MaxPayloadBytes 8192（0005 迁移）、
  MaxAssetBytes 65536（多帧用单张 PNG 雪碧图，未启用更大预算）。
- **API 契约**：`/api/challenge`、`/api/redeem`、`/siteverify`、`/healthz`、`/readyz`、
  `GET /widget/*`；`GET /demo` 只读测试页仅 `EPOCHA_DEMO_PAGE=1` 时挂载（生产默认关）。
  收紧规则：site_key 在 URL 与请求体同时出现时必须一致，冲突 400 且不消耗作答机会；
  `Origin: null` 不当 URL 解析，回落 `Sec-Fetch-Site`。

## 5. 当前开发状态（截至 2026-11-09）

### 5.1 仓库/版本状态

| 项 | 值 |
|---|---|
| 当前分支 | `fix/release-preparation-2026-11-02` @ `495ebf2`，与 `origin` 同 OID（已核对 `ls-remote`） |
| 与 `main` 关系 | 领先 22 个提交、落后 0；**尚未合回 main** |
| 标签 | `stage-0-done` … `stage-5b-done` 全部存在且两侧一致 |
| 已知标签问题 | `stage-5b-done`（指向 `993a7926`）**早于** ADR-018 恢复修复，禁止用作部署候选；README 提到的 `admin-ui-m1-done` 标签实际不存在（文档滞后） |
| AGENTS.md 滞后 | 最后更新于 `9d1b9df`，目录约定缺 `internal/dispatch`，"尚未实现"清单未反映 ADR-026 的 `generator_pool` 调度；以 ADR 与 DESIGN §19 为准 |
| 最近一次全量测试全绿 | 外部 Linux VPS 上 `go test -race -count=1 ./...` **22 包全 PASS** [实测 2026-11-05，DELIVERY-STATUS §3.4]；其后新增的 Phase 1–4.5 与门禁修复**未记录**全量 race 重跑 |
| 本机（Windows）编译 | `internal/store` 因 `syscall.Flock` **无法原生编译**（文档化的平台边界）；`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/server ./cmd/ops` rc=0 [实测 2026-11-09]；本地 `go build ./...` 还会被 gitignored 的 `release-artifacts/` 红测产物干扰 |

### 5.2 里程碑进度

| 阶段 | 内容 | 状态 |
|---|---|---|
| 0 / 1 / 2 | 设计基线；PoW+token+防重放+siteverify；多站点/凭据/来源白名单/审计 | 完成（各有标签） |
| 3A | 生成器契约 + 有界预生成题池 + 原子领题 + 单次作答 + `arithmetic-baseline-v1` | 完成 `stage-3a-done` |
| 3B | `text-image-v1` + 图片资产入库/内联下发 + 本地预览 + 离线评测脚手架 | 工程闭环完成 `stage-3b-done` |
| 4A / 4B | 生产 widget + 示例业务 + 真实浏览器闭环；表单接入 + 真实窄屏 + OCR 基线 | 完成 `stage-4a-done` / `stage-4b-done`（人工项 NOT_RUN） |
| 5A / 5B | 单节点容器部署包 + 备份/保守恢复 + 密钥文件；flock 运行互斥 + 恢复边界 + 启动配方演练 | 完成 `stage-5a-done` / `stage-5b-done`（仅 B 路径 linux/arm64，UID 1000:1000） |
| 5C-M1 | 管理控制台第一批（登录/列表/创建/详情策略与 Origin 编辑/启停） | 已交付；题池/审计/凭据轮换视图不存在 ⇒ 5C 整体未完成 |
| 2026-11-02 | 可靠性修复冻结（凭据生命周期、widget 竞态、恢复原子性、发布工程，ADR-020~022） | 已提交到本分支（`c727c99` 起） |
| 2026-11-05 | text-image-v2、Cloady amd64 测试部署、API 接入面（ADR-023） | 已交付（测试里程碑） |
| 2026-11-05~06 | 交互题型底座与题型扩展 Phase 1–4.5（ADR-024~029） | 已交付（见 5.3） |

### 5.3 题型库（12 个已注册，全部 `experimental:true` / `production_ready:false`）

| generator_id | 介质 / answer_type | 一句话 |
|---|---|---|
| `arithmetic-baseline-v1` | text / integer | 工程基线；**答案可由公开 payload 程序化推出**，不抗自动解题 |
| `text-image-v1` | image / text | 6 字符视觉基线；OCR 基线低/中/高档 96%/31%/3% [实测 4B] |
| `text-image-v2` | image / text | 8 字符强形变；tesseract 严格通过率 0% [实测 2026-11-05，仅该固定工具] |
| `text-image-v3` | image / text | 6 字符抽象文字（模糊/正弦位移/擦除点/低对比），逐字 verifier 允许 ≤1 字符替换 |
| `shape-grid-v1` | image / cells | 3×3 图形点选（非纯文字输入） |
| `shape-count-v1` | image / pairs | 两类图形计数（两个数字输入） |
| `parallax-code-v1` | image / text | 4 位数字分散在 8–12 帧视差动画（单帧截图不完整） |
| `bank-shot-v1` | image / angle | 一次反弹物理题，0.1° 角度 + TSV 容差 |
| `energy-window-v1` | image / timing | 36 帧能量游标，记录时机；reduced-motion 下被调度器排除 |
| `pipe-connect-v1` | image / cells | 4×4 拓扑网格点选主路径 2 个断点 |
| `intersection-point-v1` | image / point | 两条虚线延长线交点点选，3×3 邻格 TSV |
| `accessible-choice-v1` | text / text | 无障碍 12 选 1 纯文本单选；显式 `accessibility:true` + 24h 一次，critical 风险带不开放 |

M1（题面随机空间）/ M2（接受域）/ M3（3 次随机猜测上界，全部低于 10% 红线）已有纸面论证
（数字见 `docs/QUESTION-TYPES.md` §7）；**M4（每次尝试成本）全部未测**。
`docs/CAPTCHA-BANK-DESIGN.md` 还设计了 8 族 27 变体（滑动拼图、三杯跟踪、棋局一步杀、画圆轨迹等），
**仅 12 个已登记实现，其余是设计提案**，不得当成已有能力。

### 5.4 风险调度 / 遥测 / 无障碍（Phase 3，ADR-026/027/029）

- 站点可配 `generator_pool`（最多 8 个题型，0006 迁移）；签发时 `internal/dispatch` 按
  `BaseWeight × RiskFit[band] × DeviceFit[device]` 用 CSPRNG 加权抽题，**无候选时不回落**（503）；
- 风险带 0..100：`<25 low / <50 medium / <75 high / ≥75 critical`。Phase 3 信号只有进程内可观测的
  刷新计数、近期失败数、已校验遥测的弱异常分；**IP 信誉/ASN/Tor 名单没有接入**；
- 刷新惩罚（10 分钟窗口单调难度地板）：3–5 次 +1 档 +2bit；6–9 次 +2 档 +4bit +3s 冷却只从强池选；
  ≥10 次 +3 档 +6bit +15min 冷却 + critical；冷却回 429 不消耗题池；题池 worker 维护 d_base+1..+3 预热桶；
- 行为遥测只采集 6 个计数字段摘要（总时长/首输入/事件数/指针数/键盘数/切窗数），进程内保存不落库不写日志，
  只折算 0..20 风险修正，**永不单独拒绝正确回答**；
- 题型权重是静态画像表（`dispatch.Catalog`），**运营者可配置的权重**未实现；高难度动态自适应未实现。

### 5.5 发布工程与部署现状

- **门禁**：`node docs/scripts/release-check.mjs --profile engineering|full`（退出码 0/1/3，
  输出 JSON 收据，无人工 PASS 覆盖开关）；`build-receipt.mjs` 把二进制字节与构建命令绑进收据
  （wx 独占创建，不覆盖）；`manual-evidence.mjs` 校验真人验收证据（绑定全源码快照哈希）；
  `soak-evidence.mjs` 校验 450 次 @660ms 节流负载证据（330s 看门狗）。
- **CI**：`.github/workflows/ci.yml` 已加，**尚无远端执行证明**。
- **两条部署配方**：`deploy/docker/`（生产配方：多阶段、scratch、文件密钥、compose、
  公共端口只绑 127.0.0.1、管理端口不发布、`deploy/preflight.sh` 目标机只读预检）；
  `deploy/cloady/`（平台适配层：amd64 测试部署）。
- **Cloady 测试实例** [实测 2026-11-05]：`jian-shi/jpn1/production`，
  `https://jpn1-ub5s1o5q-jian-shi.cloady.io`，只读测试页 `/demo?site_key=pk_jpn1_v2_20261105`
  （text-image-v2 难度 1），镜像 pin `cloady-2026-11-05 (commit 6db8556…)`。真实 API 闭环通过
  （首验 success:true、重放 timeout-or-duplicate）。**无持久卷**（重建丢库）；公网 HTTPS 是平台入口
  不是自有 TLS；管理面公网 404。这不是正式发布，正式支持范围仍是 linux/arm64。
- **二进制** [实测 2026-11-03 收据]：`epocha` linux/arm64 11,600,032B、`epocha-ops` 10,289,312B（预算 35MB）。
- **容量** [上一轮实测]：约 1.5–2 RPS 持续通过；300 并发瞬时不全成功——需要产品容量决策，不是 bug。

## 6. 未完成与未闭合清单（精确分列，禁止写成本文之前完成的）

### 6.1 上线阻断项（DELIVERY-STATUS-2026-11-05 §4 的编号）

| # | 项 | 状态 |
|---|---|---|
| F-01 | 停用/撤销与在途消费：站点 enabled/version 检查与 `ConsumeResponse` 仍是**两个事务** | 开放（建议收紧为同事务 + 交错测试） |
| B-03 | 掉电持久化：`synchronous=NORMAL` 的掉电窗口 | 开放（需改 FULL 并在目标机测量，或书面接受） |
| B-01 | 正式 arm64 full 验收 | BLOCKED（只有交叉编译与 amd64 试部署） |
| B-02 | 项目自有 TLS/DNS/反代 | NOT_RUN（当前 HTTPS 全是 Cloady 平台入口） |
| B-04 | 人工/效果验收（可读性、辅助技术、真机、每题型人工 5 次） | NOT_RUN |
| — | 正式发布候选 | 未生成（干净 OID + manifest + 当次验收；旧标签不可用） |
| — | 遥测真实人群阈值标定、风险调度在真实攻击流量下的效果 | NOT_RUN |

### 6.2 功能未实现（设计与计划中，不要当成已有能力）

- 第三层风险打分**决策**：自动拒绝、分级淘汰、统计与观测面；
- 运营者可配置的题型权重；同族不连出、题目级冷却（调度器留了扩展位）；
- 音频通道、开关扫描、语义列表（无障碍通道现只有文本单选）；`fallback_base` 多源切换；
- 管理台题池/审计/凭据轮换视图；多实例（明确不支持，单实例是运维边界）；
- CAPTCHA-BANK-DESIGN 的 27 变体中未登记的 15 个；
- `path`/`order`/`polygon` 三种交互原语（契约保留，未实现）；多帧分族资产预算（64KB 上限未动）。

### 6.3 全部题型的共同 NOT_RUN（不随任何工程进展自动改变）

M4 每次尝试成本、人工可读性（真人数据）、辅助技术真机实测、真机移动端（headless 窄视口不算）、
v3 的 OCR 基线（不得把 v2 的 0% 外推）、一切"抗 AI/抗 OCR/抗多模态"结论。

## 7. 给在本仓库工作的 AI 的行为红线（历史踩坑浓缩）

1. 遵守 AGENTS.md 的固定工作流：读 DESIGN §17 + 相关 ADR → **先写测试**（应红）→ 实现到绿 →
   `make fmt-check vet race` → 更新 DESIGN/DECISIONS → commit → 只有真实验收全过才打标签。
   标签一旦打上不移动不删除。
2. **停止并问用户**：删除已有文件；新增依赖/改依赖大版本/升工具链；与 ADR 或 DESIGN 关键决策冲突；
   素材许可证判断；放宽安全红线；影响远端的破坏性操作（force push、删分支、删标签、移动已推送标签）。
   其余阶段内工作自主推进。推送政策（2026-10-19 起）：完成的开发里程碑 commit 后及时 push，
   顺序为 测试 → 复核暂存内容 → commit → push → `git ls-remote` 回读核对 OID；非快进停下问。
3. **不要编造**库/平台 API 或配额（ADR-001 的教训）；不确定就读 GOROOT 源码、module cache 真实签名、
   跑最小验证程序，或查当日官方文档并注明日期。
4. 不要把未实测写成事实；不要把"文件都写好了"写成验收通过；不要用截图代替交互测试
   （本机路径：Firefox 自带 WebDriver BiDi + Node 内置 WebSocket + `node --test`，不得引入 npm 包）。
5. 不要在未运行 `go test -race -count=1 ./...` 的情况下声称测试通过；Windows 本机跑不了 store 包
   （flock），完整测试需要 Linux 环境。
6. 验收脚本连接固定端口前必须检查端口空闲，不许连遗留进程；不要在开发机上跑预检然后把结果当成
   目标 VPS 的事实。
7. 不要把测试工装里的演示凭据/高限流/已知答案写进部署配置；`allow_missing_origin=true` 只许出现在
   明确的测试站点。
8. 对账类判据在"什么都没取到"时必须 FAIL（空输入 ≠ 检查通过）。
9. 提交前 `git status` 复核暂存内容；发现高熵串/`*_key`/credentials 类内容停下来问。
10. `docs/reviews/` 与 `release-artifacts/`（gitignored）是审计与收据留痕区；`epocha-offline-assets.tar.gz`
    是被跟踪的 20.6MB **历史开发评测归档**，运行时不依赖，已从 Docker 构建上下文排除。

## 8. 权威文档索引（按重要度）

| 文档 | 内容 |
|---|---|
| `AGENTS.md` | 给 agent 的工程约定：约束、红线、工作流、推送政策、停止并问我清单 |
| `docs/DESIGN.md` | 技术方案 v2.6：目标/威胁模型/数据模型/密钥/PoW/题池/API/widget/部署/预算/阶段验收 |
| `docs/DECISIONS.md` | ADR-001…029：每个决策的理由与历史错误表述的更正（冲突时以它为准） |
| `docs/QUESTION-TYPES.md` | 12 个题型的状态表、M1–M4 论证、操作入口、NOT_RUN 清单 |
| `docs/CAPTCHA-BANK-DESIGN.md` | 题库设计提案（27 变体、Phase 划分、production_ready 七条门槛）；DRAFT |
| `docs/DELIVERY-STATUS…` → `docs/reviews/DELIVERY-STATUS-2026-11-05.md` | 一句话状态、Cloady 入口、完成/阻断清单、收敛 GO 方案 |
| `docs/reviews/` | 9 份审计/冻结/交付评审（2026-11-02~05 的收敛过程全记录） |
| `docs/DEPLOYMENT.md` / `docs/BACKUP-RESTORE.md` | 生产部署配方与备份/保守恢复语义（上线必读） |
| `docs/TRIAL-DEPLOYMENT.md` | 试部署计划（**待确认，不是授权**；APPROVED_OID/MANIFEST 缺失即停止） |
| `docs/WIDGET.md` / `docs/ADMIN-UI.md` | 接入契约与 CSP 清单；管理控制台边界与 NOT_RUN |
| `docs/CLOADY-DEPLOY.md` | Cloady 测试部署事实、边缘 caveat、OCR 复测收据 |
| `docs/STAGE3A…5B.md`、`docs/MANUAL-ACCEPTANCE.md`、`docs/NOTICE.md` | 各阶段现状与实测数字；人工证据入口；依赖与字体许可 |

## 9. 如何复核本文的关键声明（在仓库根执行）

```bash
git rev-parse HEAD                                  # 应为 495ebf2…（分支 fix/release-preparation-2026-11-02）
git ls-remote origin | grep heads/main              # 同步核对
grep -c "Register\|NewTextImage" cmd/server/main.go # 12 个生成器登记（knownGenerators）
grep -n "schemaVersion" internal/store/store.go     # = 6
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/server ./cmd/ops   # B 路径交叉编译，rc=0
node docs/scripts/release-check.mjs --profile engineering               # 工程门禁（0/1/3）
```

> 本文由 2026-11-09 的审计会话生成；文中所有 [实测] 均标注了日期与环境。仓库演进后请更新 §5.1 的
> OID 与 §6 的清单，不要删除 §0 的阅读纪律。
