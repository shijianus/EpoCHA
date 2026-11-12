# CAPTCHA 题库整改审计（2026-11-12）

> 状态：**审计与整改开工记录**，不是验收报告，不是发布批准。
> 基线：`fix/release-preparation-2026-11-02` @ `887d831`（`git status --short --branch` 干净）。
> 本文逐项区分 **PASS / FAIL / BLOCKED / NOT_RUN**；任何未实测项不得读成通过。
> 所有题型仍为 `experimental:true` / `production_ready:false`；M4、人工可读性、辅助技术、真机移动端
> 均未测。本文中的尺寸与耗时是 **[实测 2026-11-12 / Windows amd64 开发机]** 的单轮抽样，
> 不是目标机（1 vCPU / 1GB）稳态数字，也不是安全结论。

## 0. 结论摘要

1. 仓库当前登记 **12 个 generator**，全部 `experimental:true` / `production_ready:false`；schema 版本
   `6`（迁移 `0001`–`0006`）；没有题型被证明抗 AI、抗 OCR 或可生产使用。
2. `arithmetic-baseline-v1` 是**测试基线**：公开 payload 里的表达式可直接求值，M3 d1 最大单点概率
   约 4.5%–4.8%，3 次累计已超过 10% 红线。它只用于工程回归，**不是安全题型**。
3. `accessible-choice-v1` 是**可访问替代路径**，不是强安全题型：12 选 1、单次随机猜中率约 8.3%，
   只允许显式 `accessibility:true` + 站点池显式包含 + 24h 一次。
4. 现有 12 个题型的公共/私有分离、精确 HMAC 规范化入口、48B/208B/TSV 三种 verifier 形态、
   单资产 64KB 预算、题池 worker 的有界批次与让步都已在代码和测试中落地；本轮新题型必须复用这些契约。
5. 新题型（`tile-rotation-v1`、`symbol-path-v1`、`crowd-find-v1`、`visual-rank-v1`、
   `progressive-target-v1`、`shell-track-v1`、`silhouette-assembly-v1`）与
   `shape-grid-v1` 的 negative polarity 修饰器按 Wave 1 / Wave 2 实施；本文只记录审计事实与开工前状态。
6. `pipe-connect-v1` 的现状是「点选主路径 2 个断点」，不是设计草案里的「旋转管道块 / 填满格子」；
   它的 verifier 只能表达一个 1-based 格子集合，**无法表达整张图的逐格 edge mask**。在不破坏
   已签发 verifier 语义的前提下，不能把它原地改成新题型；整改见 §9（BLOCKED / 设计 + 测试边界）。

## 1. 仓库事实核对

| 项 | 实测值 | 说明 |
|---|---|---|
| 分支 | `fix/release-preparation-2026-11-02` | 与 `origin` 同名分支；本次未 reset/checkout/rebase |
| HEAD | `887d831009d707da45d7474a624320d84ee4a8f9` | `docs: add AI briefing…` |
| 工作区 | Wave 1 改动（未提交）；Wave 2 生成器代码存在但未登记 | `git status --short --branch`；未 reset/checkout/rebase |
| Go 工具链 | `GOTOOLCHAIN=local`，Go 1.27.3 | 未升级、未切换 |
| 依赖 | `modernc.org/sqlite v1.46.1`、`golang.org/x/image v0.36.0` | 本轮未新增依赖 |
| schema | `schemaVersion=6` | 迁移 `0001`–`0006`；新增 asset MIME/编码前必须评估迁移 |
| 注册题型 | 16 | `cmd/server/main.go` 的 `knownGenerators`（12 + Wave 1 的 4） |

## 2. 现有 12 个题型的真实代码状态

尺寸/耗时列 = [实测 2026-11-12 / Windows amd64 开发机]，每档 10 次 `Generate`，payload 为
`json.Marshal(Payload)` 的长度，asset 为 PNG 字节数；不是目标机、不是稳态、不是安全结论。
硬上限：payload `MaxPayloadBytes=8192`、verifier `MaxVerifierBytes=4096`、单资产
`MaxAssetBytes=65536`（`internal/store/store.go:59`–`64`）。

| # | generator ID | 输入模态 | verifier 形态 | payload 上限（实测最大） | asset 上限（实测最大/最小） | 随机猜测上界（3 次） | M1 | M2 | M3 | M4 | 375px 布局 | 键盘实现 | 键盘浏览器 E2E | 辅助技术 | production_ready |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | `arithmetic-baseline-v1` | text / integer | 48B 精确 HMAC | 8192（71） | 无 | d1 最大单点 ≈4.5%–4.8%，3 次 ≈13.5%–14.4% ⇒ **超 10% 红线**；仅基线 | PASS（工程） | PASS（精确） | **FAIL（生产门槛）** | NOT_RUN | PASS（纯文本无图） | PASS | NOT_RUN | NOT_RUN | false |
| 2 | `text-image-v1` | image / text | 48B 精确 HMAC | 8192（134） | 65536（7,036 / 1,549） | `32^-6`，3 次 ≈2.8e-9 | PASS（32^6） | PASS（接受域 1） | PASS | **NOT_RUN** | PASS（4B 真实窄视口） | PASS | NOT_RUN | NOT_RUN | false |
| 3 | `text-image-v2` | image / text | 48B 精确 HMAC | 8192（134） | 65536（13,451 / 7,152） | `32^-8`，3 次 ≈1.3e-12 | PASS（32^8） | PASS（接受域 1） | PASS | **NOT_RUN** | PASS（headless 视口） | PASS | NOT_RUN | NOT_RUN | false |
| 4 | `text-image-v3` | image / text | 208B 逐字 HMAC（≤1 字符替换） | 8192（147） | 65536（20,544 / 11,033） | 接受域 187；`187/32^6 ≈1.74e-7`，3 次 ≈5.2e-7 | PASS（32^6） | PASS（187 串） | PASS | **NOT_RUN** | PASS（headless 视口） | PASS | NOT_RUN | NOT_RUN | false |
| 5 | `shape-grid-v1` | image / cells | 48B 精确 HMAC | 8192（291） | 65536（7,613 / 3,821） | 最坏 `C(9,2)=36`；3 次 ≈8.1% | PASS | PASS | PASS（<10%） | **NOT_RUN** | PASS（headless 视口） | PASS（cells 按钮） | NOT_RUN | NOT_RUN | false |
| 6 | `shape-count-v1` | image / pairs | 48B 精确 HMAC | 8192（364） | 65536（8,128 / 3,098） | `1/36`；3 次 ≈8.1% | PASS | PASS | PASS（<10%） | **NOT_RUN** | PASS（headless 视口） | PASS（两个数字输入） | NOT_RUN | NOT_RUN | false |
| 7 | `parallax-code-v1` | image / text（雪碧图） | 48B 精确 HMAC | 8192（355） | 65536（8,305 / 5,245） | `10^-4`；3 次 ≈3e-4 | PASS（10^4） | PASS（接受域 1） | PASS | **NOT_RUN** | PASS（headless 视口） | PASS（文本输入） | NOT_RUN | NOT_RUN | false |
| 8 | `bank-shot-v1` | image / angle | TSV（角度桶） | 8192（338） | 65536（3,195 / 3,053） | 最坏 41/3600≈1.14%；3 次 ≈3.4% | PASS | PASS（41 桶） | PASS | **NOT_RUN** | PASS（headless 视口） | PASS（滑块） | NOT_RUN | NOT_RUN | false |
| 9 | `accessible-choice-v1` | text / text（12 选 1） | 48B 精确 HMAC | 8192（713） | 无 | `1/12≈8.3%` 单次；仅 1 次 + 24h 限额 | PASS（12 选项） | PASS（接受域 1） | PASS（8.3%，仅 1 次） | **NOT_RUN** | PASS（纯文本） | PASS | NOT_RUN | **NOT_RUN** | false |
| 10 | `energy-window-v1` | image / timing（雪碧图） | TSV（时间桶） | 8192（361） | 65536（12,329 / 11,784） | 13/400=3.25%；3 次 ≈9.44% | PASS（36 帧） | PASS（13 桶） | PASS（<10%，无余量） | **NOT_RUN** | PASS（headless 视口） | PASS（按钮） | NOT_RUN | NOT_RUN | false |
| 11 | `pipe-connect-v1` | image / cells | 48B 精确 HMAC | 8192（347） | 65536（2,633 / 2,108） | d1 最少 E=9 ⇒ `1/C(9,2)=1/36`；3 次 ≈8.1% | PASS（拓扑组合） | PASS（空候选格集合） | PASS（8.1%） | **NOT_RUN** | PASS（headless 视口） | PASS（cells 按钮） | NOT_RUN | NOT_RUN | false |
| 12 | `intersection-point-v1` | image / point | TSV（3×3 邻格） | 8192（232） | 65536（2,922 / 2,371） | d1 最坏 9/351≈2.56%；3 次 ≈7.5% | PASS（网格×目标格） | PASS（9 格） | PASS | **NOT_RUN** | PASS（headless 视口） | PASS（方向键微调） | NOT_RUN | NOT_RUN | false |

### 2.1 状态读法（不得混用）

- **M1/M2/M3 PASS** 只表示「已在文档中给出定义、代码中可实现/可测试」；它不是安全认证，也不是
  「机器解不了」。M3 的数字是随机猜测上界，不是 M4。
- **M4 全部 NOT_RUN**：没有真实多模态/打码平台/人工成本实验。历史 v1 的固定 tesseract 基线、
  v2 的 0% 都只对「该固定工具/样本集/机器」成立，不能外推到新题型或其他模型。
- **375px PASS** 指阶段 4B 的 headless Firefox 真实视口断言（`innerWidth/clientWidth` 实测），
  **不等于真机移动端**；真机一律 NOT_RUN。
- **键盘实现 PASS** 指代码路径存在且有 Node/单元测试；**键盘浏览器 E2E** 除 4A 的 text 题型外
  没有逐题型浏览器实测，因此 NOT_RUN。辅助技术（屏幕阅读器/开关设备）全部 NOT_RUN。
- `arithmetic-baseline-v1` 的 M3 是 **FAIL（生产门槛）**：d1 3 次累计超过 10% 红线。这不影响它作为
  工程基线存在，但禁止把它描述成安全题型或用于真实流量。
- `accessible-choice-v1` 是**可访问替代路径**，不是强安全题型；单次 8.3% 已接近红线，只靠显式请求、
  池内显式包含、24h 一次和一次性 challenge 限制。

## 3. 契约与实现入口（新题型必须复用）

### 3.1 Generator 接口与注册

- 接口：`internal/generator/generator.go:75`–`95`
  - `ID() / Version() / Media()`
  - `Generate(ctx, Params, entropy io.Reader) (Question, error)`
  - `Verify(PrivateVerifier, canonical string) bool`
  - `NormalizeAnswer(input) (canonical, ok)`；`ok=false` 是**畸形**，不消耗作答机会
  - `SanitizePublic(raw []byte) (map[string]any, error)`；未知键 ⇒ 整次报错（fail closed）
- 注册：`cmd/server/main.go:465` `knownGenerators(qKey)` 构造并登记全部题型；
  `cmd/server/main.go:133` 创建 `generator.Registry`。
- 管理面生命周期：`internal/admin/handlers.go:793` `generatorLifecycle`（默认 `true,false`）
  与 `noteForGenerator`（`internal/admin/handlers.go:806`）。新增题型必须显式登记，否则管理面
  仍按最保守的 `experimental:true / production_ready:false` 报告。
- 预览：`cmd/preview/main.go:181` 的白名单；`cmd/devseed/main.go:158` 的 `buildGenerator`。
- 调度画像：`internal/dispatch/dispatch.go:Catalog`；未登记 ID 走 `FallbackCandidate`（中性权重，
  不推断可用性）。新增题型必须显式登记其 `Motion` / `DeviceFit` 等属性。

### 3.2 公开 payload 与私有 verifier 的分离

- `PublicPayload` 是具名结构体（字段即白名单），`PrivateVerifier []byte` 是不可序列化的独立类型；
  `Question{Payload, Verifier, Asset, MimeType}`（`generator.go:40`–`56`）。
- 出网唯一通道：`internal/api/handlers.go:468` `questionDTO` → `SanitizePublic`；图片以 base64 内联，
  没有裸媒体端点；`media` 与资产必须成对一致，否则作废 challenge + 503（`handlers.go:479`–`499`）。
- 判分只读 verifier：`internal/api/handlers.go:624`–`644`，**不读题面、不读图片资产**。

### 3.3 精确 HMAC 的规范化入口

- `answerVerifier/verifyAnswer`：`internal/generator/generator.go:152`–`192`；
  形态 `nonce(16B) || HMAC(K_qver, "epocha/v1|qver|" || nonce || 0x00 || canonical)`，48B。
- 逐字容差：`charVerifier/verifyChars`（208B，仅 `text-image-v3`），`generator.go:194`–`247`。
- TSV：`internal/generator/tsv.go`，`ETSV` v1，最多 127 tags，`TSVMaxBytes=4087`；候选值命中集合，
  不含答案明文或可逆种子。
- 规范化工具：`internal/generator/answer.go`（`NormalizeAngle/Point/Timing/CellSet`、
  `KnownAnswerType/ValidateAnswerType`）。新题型应优先复用；新增编码必须固定版本、固定字段顺序、
  固定分隔符、无空白、无浮点、严格长度/范围，并拒绝前导符号、重复字段、未知字段与尾随垃圾。

### 3.4 Widget 的状态机与输入分发

- 文件：`web/widget/v1.js`（2135 行；[实测 2026-11-05] 原文 54,803B / gzip 18,998B，
  gzip 预算 <30KiB）。
- 状态机：`STATES`（`v1.js:257`）`idle/loading/ready/solving/submitting/verified/expired/error/destroyed`；
  只有服务端 `/api/redeem` 成功才进入 `verified`。
- 输入分发：`v1.js:1601` 起按 `answer_type` 选择 `text/cells/pairs/angle/point/timing`；
  `_render*` 函数在 `v1.js:1046`–`1329`。新输入必须走同一状态机、同一 `/api/redeem`，
  不得在客户端判定对错。
- Dedicated Worker 边界：PoW 只在 `v1.worker.js` / 内嵌 Blob Worker 中计算；主线程不做判分，
  `POW_WORKER_SOURCE` 与同源文件逐字节一致（`web/widget/widget_test.go` + Node 向量测试钉住）。
- `answer_mode` 与提交：`_syncCells`/`_syncPairs`/`_syncAngle`/`_setPoint` 只写 `answer.value`，
  提交仍是原 `/api/redeem`；隐藏字段只承载一次性 response token。

### 3.5 资产存储与 64KB

- `pool.asset_blob` + `asset_mime`；迁移 `0004` 增加列，`0005` 重建表时保留；
  当前 CHECK 只允许 `''` 与 `'image/png'`（`internal/store/migrations/0005_payload_8k.sql:24`）。
- 服务端硬上限 `MaxAssetBytes=65536`；题池按全局图片字节预算判定，不与行数混同
  （`internal/store/pool.go:111`–`122`、`internal/pool/pool.go:456`）。
- 资产与 MIME 必须成对；判分路径不读资产。新增 GIF 等 MIME 必须做迁移与回归，
  不能只改 Go 常量（CHECK 会拒绝）。

### 3.6 题池 worker 生成预算

- `internal/pool/pool.go`：`MaxRows=20000`、`BatchPerTick=20`、`YieldEvery=10`、
  `YieldPause=100ms`、图片桶 `ImageYieldPause=200ms`、全局图片字节预算
  `ImageMaxTotalBytes`；`MaxWorkers` 硬上限。
- 新题型不得把重计算移到 redeem 热路径；图片/动画只在 worker 预生成。
- 生成失败有界退避；`ErrGenerateExhausted` 可重试，不得降级成固定题。

### 3.7 challenge TTL、刷新冷却与一次性消费

- TTL/签名/绑定：`internal/token`、`internal/api/handlers.go:514`–`704`；
  一次性消费 `RedeemAttempt` 原子条件更新（ADR-004），错误答案提交终态，
  畸形答案不消耗机会。
- 刷新惩罚：`internal/risk` + `internal/api/handlers.go:291`–`307`；
  冷却返回 429，不消耗题池；`reduced_motion` 由 dispatch 的 `Motion` 过滤。
- 测试入口：`internal/store` 的事务/并发/重启测试、`internal/api` 的 redeem 测试、
  `docs/scripts/stage*.sh` 的端到端；新题型必须补同类测试。

## 4. 现有题型的审计结论（按类型）

### 4.1 `arithmetic-baseline-v1`（测试基线）

- 文件：`internal/generator/arith.go`；媒体 text；答案 `integer`；48B 精确 HMAC。
- 公开 payload 是可直接求值的表达式；答案可从 payload 推导是**设计事实**，不是缺陷。
- M3 d1 最大单点概率约 4.5%–4.8%，3 次约 13.5%–14.4%，**超过 10% 红线**。
- 结论：**测试基线，不是安全题型**；不得用于真实流量，不得声称抗 AI/OCR。

### 4.2 `accessible-choice-v1`（可访问替代路径）

- 文件：`internal/generator/accessible.go`；媒体 text；12 选 1；48B 精确 HMAC。
- 只允许 `accessibility:true` + 站点池显式包含 + 24h 一次；critical 风险带不开放。
- 单次随机猜中率约 8.3%，仅 1 次有效作答；它不是强安全题型，也不是「已经无障碍验收」。
- 结论：**可访问替代路径**；辅助技术真机实测 NOT_RUN。

### 4.3 其余 10 个题型

- 视觉文字（v1/v2/v3）：48B 精确 HMAC 或 208B 逐字容差；v3 的容差是为「人认错一个字」，
  不是抗 OCR；历史 OCR 数字只对固定工具成立。
- 图形点选/计数（shape-grid/shape-count）：48B 精确 HMAC；M3 <10%；图形对机器视觉不难，M4 未测。
- 时序/物理/几何（parallax/bank-shot/energy-window/intersection-point）：48B 或 TSV；
  M3 <10%，但 M4 全部 NOT_RUN；`energy-window` 在 reduced-motion 下被调度器排除。
- `pipe-connect-v1`：48B 精确 HMAC；答案只是 2 个断点格，**不是**完整管道状态；见 §9。

## 5. 新题型开工前的共同安全契约（本次实施约束）

1. 服务端唯一权威：客户端只提交原始操作结果（最终 edge masks、格子索引序列、选择索引、
   卡片随机 ID 排列、slot/piece/rotation 映射）；不得提交 `solved`、`scorePassed`、
   `targetIndex`、`expectedAnswer`、私有 seed 或可重算答案的 RNG 状态。
2. 答案规范化：优先复用 `answer.go` 的 helper；新增编码使用 `v1:<严格 ASCII>`，
   固定版本/字段顺序/分隔符、无空白、无浮点、严格长度与索引范围；拒绝前导符号、重复字段、
   未知字段、Unicode 同形字符与尾随垃圾。HMAC 只对规范化后的串计算。
3. 公共 payload 不得含答案、目标索引、piece-slot mapping、swap sequence、solution path、
   可推导私有答案的 seed、调试用 answer 字段。
4. verifier 优先复用 48B 精确 HMAC；离散题必须转成唯一 canonical answer 后 HMAC。
   多解 seed 必须拒绝并重新生成，不得在客户端决定「等价答案」，不得扩展 verifier 格式。
5. 错误处理 fail closed：payload 解析失败、asset 解码失败、答案越界、答案错误都不得泄露失败层级；
   日志只用既有白名单字段，不含答案/目标索引/私有 verifier。
6. 资源上限：payload ≤8KB、verifier ≤4KB、单资产 ≤64KB；数组长度先验证再分配；
   动画帧数、网格尺寸、piece 数、路径长度有编译期/配置上限；DFS/回溯有节点上限；
   重计算只在题池 worker。
7. 随机性：安全语义只用 `crypto/rand`；测试通过注入 `io.Reader` 获得确定性；
   生产生成器不得用固定 seed 或 `time.Now().UnixNano()` 作为安全随机源。

## 6. Wave 1 / Wave 2 计划与验收边界

| 波次 | 题型 | 输入原语 | canonical answer | 3 次随机猜测上界（设计目标） |
|---|---|---|---|---|
| Wave 1 | `tile-rotation-v1` | 3×3 tile 旋转 | `v1:<9 hex masks>` | 有效旋转候选空间 ≥64 ⇒ ≤1/64；3 次 ≤4.7% |
| Wave 1 | `symbol-path-v1` | 6×6 符号路径 | `v1:03.09.…` | 合法位置路径 ≥128 ⇒ ≤1/128；3 次 ≤2.34% |
| Wave 1 | `crowd-find-v1` | 36 选 1 | `v1:<0..35>` | 1/36；3 次 ≤8.33%（上界 3/36） |
| Wave 1 | `visual-rank-v1` | 5 卡排序 | `v1:A.B.C.D.E` | 1/120；3 次 =2.5% |
| Wave 2 | `progressive-target-v1` | 3 轮 3×3 定位 | `v1:a.b.c` | 1/729；3 次 ≈0.41% |
| Wave 2 | `shell-track-v1` | 3 轮 4 容器跟踪 | `v1:a.b.c` | 1/64；3 次 ≈4.7% |
| Wave 2 | `silhouette-assembly-v1` | 5 槽位装配 | `v1:id:rot,…` | 1/(5!×4^5)=1/122880；3 次 ≈2.4e-5 |
| 修饰器 | `shape-grid-v1` negative polarity | 网格点选 | 现有 cells 集合 | 与 positive 相同 C(9,k)；k=2/3 ⇒ <10% |

所有题型的 M4、人工可读性、辅助技术、真机移动端仍是 **NOT_RUN**；`production_ready` 保持 `false`。
新增题型默认**不加入任何站点**；运营者必须在站点 `generator_id` / `generator_pool` 中显式选择，
才可能被签发。现有站点的题型与权重不变。

## 7. 测试矩阵（每个新题型）

1. Generator：固定 reader 可重现；生成合法；不初始即完成；唯一解；随机猜测界满足文档；
   生成重试有界；payload/verifier/asset 不超限。
2. Parser：空答案、超长、非法版本、非法字符、负数、越界、重复索引、字段缺失、尾随垃圾、
   Unicode 同形字符、fuzz 不 panic。
3. Separation：公共 JSON 不含 answer/target/solution/verifier/private seed；日志不含答案；
   admin preview 不暴露 verifier；前端 bundle 不含固定答案表。
4. Redeem：正确答案成功；错误答案失败；同一 challenge 第二次失败；并发只有一笔成功；
   secret 校验先于消费；事务提交后才返回成功；错误不透露层级。
5. Widget：320px 无横向溢出；pointer/touch/keyboard；focus 顺序；disabled；
   submitting 防重复；success/failure live region；中英文案；devicePixelRatio 坐标；Shadow DOM 隔离。
6. Browser：仓库现有 Firefox BiDi + Node 内置 WebSocket，不新增 npm 包；
   shell-track 额外要求 Chrome/Firefox/WebKit、动画结束状态、GIF disposal、资产大小、
   reduced-motion 路径——本机只有 Firefox 路径可执行，其余必须如实标 BLOCKED/NOT_RUN。

## 8. 人工与 M4（本轮不代填）

本轮至少建立记录模板；没有真实执行就写 NOT_RUN。每题型初始人工证据要求：桌面 pointer 5 次、
键盘 5 次、真实移动设备 5 次、中文 5 次、英文 5 次；记录完成时间、失败原因、刷新次数。
产品目标只标 `[目标]`：中位 ≤20s、P95 ≤45s、每组 5 次意外失败 ≤1、无横向滚动、
无不可恢复 focus trap、不依赖颜色/声音/摄像头/精确高速运动。五次样本不得外推为总体统计。

## 9. `pipe-connect-v1` 整改审计（BLOCKED）

现状（`internal/generator/pipeconnect.go`）：
- 4×4 网格，主路径每列一个节点；第 2、3 列的两个节点被擦空，另有 7–12 个死路也被擦空；
  用户只能点选两个断点格；答案编码是 `cells` 集合（`NormalizeCellSet`），verifier 是 48B 精确 HMAC。
- 断点与死路外观相同，M3 按 `C(E,2)` 计算；这是**选择两个格**的题，不是「连接所有端点 / 填满所有格子
  的管道拼图」。

目标规则与现状的差距：
- 多对端点、同色/同纹理配对：现状只有一条主路径的两个断点，没有端点配对。
- 路径不重叠、每个格子被占用：现状不建模路径状态，无法验证。
- 唯一解：现状只保证两个断点格的位置，不保证整张图的路径唯一。
- 端点不能只靠颜色：现状只有实心/空心圆起点终点，没有形状/纹理配对。
- 键盘逐格绘制与撤销：现状是 cells 按钮，不能表达逐格 edge mask。
- 答案规范化为每格 edge mask：现状的 `cells` verifier 无法表达；改成新形态会破坏已签发
  verifier 的语义（DESIGN §8.4：判分实现变更必须升 version / 新 ID）。

结论：**BLOCKED**。在「不破坏已签发 challenge / 不改 `cells` verifier 语义」的前提下，
`pipe-connect-v1` 不能原地表达目标规则。整改路径是新增一个使用 `masks` canonical answer
（每格 edge mask）的独立 generator/version，并为其写生成器唯一解证明、端点形状/纹理配对、
路径覆盖与不交叉验证、键盘逐格绘制/撤销的测试；在这些测试完成前不得用客户端 `solved=true`
或「看起来连上了」代替服务端验证。本轮若实施新题型，将另立 ID/ADR；`pipe-connect-v1` 保持原样，
在题型文档中标注「legacy cells 题型，不满足完整管道拼图规则」。

## 10. 本轮不声称的内容

- 不声称任何新题型抗 AI、抗 OCR、抗多模态、抗打码平台或可生产使用。
- 不声称已完成人工可读性、辅助技术、真机移动端、M4 或正式 arm64 验收。
- 不声称 F-01、B-03、B-01、B-02、B-04 已解决；不生成正式发布候选。
- 不把「题型数量增加」作为完成标准；完成标准是每个题型都有确定通过条件、唯一 verifier、
  严格资源上限、可复现生成、测试证据和诚实的安全边界。

## 11. Wave 1 实施与验证（2026-11-12）

Wave 1 四个题型已实现、登记并接入 widget；Wave 2 生成器代码尚未登记，不计入本节。

| 项 | 状态 | 证据/命令 | 环境 |
|---|---|---|---|
| 生成器唯一解与 10,000 seed | PASS | `go test -count=1 ./internal/generator/`（含 `TestTileRotationTenThousandSeeds`、`TestSymbolPathSeedsUniqueAndBounded`、`TestCrowdFindTargetUniqueAndDistractors`、`TestVisualRankCountsDistinctAndOrdered`） | Windows amd64 / Go 1.27.3 |
| payload/verifier/asset 上限 | PASS | 同上的 `*AssetBudget*`、`*NoLeak*`、`SanitizePublic` 测试；全局上限仍为 8192/4096/65536 | 同上 |
| 答案编码拒绝表与 fuzz | PASS | `internal/generator/answer_encoding_test.go`；`go test` 的 `FuzzNormalizeNewEncodings` 种子模式 | 同上 |
| widget 交互（Node 工装） | PASS | `node --test docs/scripts/browser/pow_vectors.test.mjs docs/scripts/browser/widget_lifecycle.test.mjs`（39 项，含 7 种新输入与模块加载失败） | Node 24.14.0 / Windows |
| widget 体积 | PASS | `node docs/scripts/release-check.mjs --widget-size`：core gzip 28,909 B < 30,720 B；interactive 10,311 B；合计 39,220 B < 40,960 B | 同上（LF 工作区） |
| Go widget 内嵌/路由 | PASS | `go test -count=1 ./web/widget/`（含新模块的资源字节、ETag、404 边界） | 同上 |
| API 闭环（challenge/redeem/重放/错误终结） | **BLOCKED** | `internal/api/new_generators_test.go` 无法在 Windows 编译：`internal/store/lock.go` 使用 `syscall.Flock`（AGENTS 已声明 Windows 不做 flock 测试）。Linux 全量 race 未在本机执行。 | Windows amd64 |
| 真实浏览器 E2E（Firefox BiDi） | **BLOCKED** | 需要可运行的 `cmd/server` 与 `cmd/devseed`；同一 Windows 限制。Node 工装不等于真实浏览器。 | Windows amd64 |
| 320px 无横向滚动 | **NOT_RUN** | 无真实视口测量；Node 工装的 fake DOM 没有布局。 | — |
| 人工可读性 / 辅助技术 / 真机移动端 | **NOT_RUN** | 无真实参与者/设备；不代填。 | — |
| M4 | **NOT_RUN** | 无自动化成本实验；不得用"答案不在 JSON"论证抗 AI。 | — |

**模块化说明**：新增的 7 种交互渲染器放在 `web/widget/v1.interactive.js`，只在领到对应
`answer_type` 时由核心同源按需加载；核心 `v1.js` 仍 <30KB。模块失败时进入
`interactive-unavailable`，不渲染可点击的半成品。核心与模块共用同一个 Widget 状态机，
唯一通过点仍是服务端 `/api/redeem`。
