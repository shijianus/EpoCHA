# AGENTS.md — epocha 工程约定

给后续会话的约束。你（agent）在此仓库工作时必须遵守本文。
设计基线在 `docs/DESIGN.md`（v2）；**修订理由与历史错误表述在 `docs/DECISIONS.md`**。冲突时以 DECISIONS 的 ADR 为准，且必须先改文档再改代码。

## 项目是什么
自托管人机验证服务（CAPTCHA）：单 Go 二进制 + SQLite。三层防线 = PoW 成本门槛 → 生成式题池 → 风险打分决策。
`/siteverify` 采用 reCAPTCHA **风格**的契约（`secret`/`response`/`remoteip` → `success`/`challenge_ts`/`hostname`/`error-codes`），
**不声称无条件兼容所有现成 SDK**（v2/v3 的额外参数未实现）。

## 当前实现进度（阶段 1、2、3A 已完成，2026-10-17）
已实现：`config`、`store`（三条增量迁移 0001/0002/0003 + 原子条件消费 + 题池绑定）、`pow`、`token`
（`chal`/`resp`/`qver` 三把域分隔密钥）、`risk`（有界令牌桶 + 代理信任裁决）、`api`（`/api/challenge`、`/api/redeem`、
`/siteverify`、`/healthz`）、`admin`（站点与凭据生命周期、来源策略、模式/生成器配置、`/api/admin/generators` 与
`/api/admin/pool` 只读观测，独立监听器）、`generator`（生成器契约 + `arithmetic-baseline-v1`）、
`pool`（后台预生成 worker：水位迟滞、有界批次、CPU 让步、指数退避）、`cmd/server`（优雅关闭 + worker 生命周期）、
`cmd/devclient`、`docs/scripts/{stage1,stage2,stage3a}.sh`、`docs/STAGE3A.md`。
**尚未实现**：视觉/图形题型与渲染依赖、题型权重与多生成器选择、风险打分、自动淘汰、widget（阶段 4）、后台页面/容器/边缘（阶段 5 及之后）。
**阶段 3A 只有"文本基线题型 + 题池工程骨架"两层（PoW + 一次性题目）**：第三层风险打分不存在，
且 `arithmetic-baseline-v1` 的答案**可由公开 payload 程序化推出**——不得在响应或文档里把它说成抗 AI/抗 OCR 题型，
也不得用"随机实例多"论证安全性（DESIGN §8.4、ADR-011、红线 14）。
阶段边界与验收：`make stage1-check` / `stage2-check` / `stage3a-check`；标签只打 `stage-3a-done`，**不打 `stage-3-done`**。

## 硬性技术约束
- **只用 Go，纯 Go 依赖，禁止 cgo**。禁止引入 JVM、MongoDB、Postgres、Redis、外部二进制（ImageMagick/fontconfig/headless chrome）。
- 目标机器：1 vCPU / 1GB RAM / 50GB 磁盘。常驻 RSS `[目标]` <100MB、二进制 `[目标]` ≤35MB；遵守 DESIGN §13 与 ADR-006 的标注纪律。
  3A 已有的 `[实测]` 是**功能闭环形状**而非稳态：服务进程 VmRSS=VmHWM=15.1MB（200 题 + 20 次完整流程）、
  二进制 9,896,228 字节（阶段 2 与 3A 同 flag 构建字节数相同）。**不得**据此宣布"预算达标"——长期运行、
  真实流量形状与 20000 题满载仍未测。
- 已批准依赖：**只有** `modernc.org/sqlite v1.46.1`（BSD-3，锁版本原因见 ADR-007）。不得为后续阶段提前引入 `golang.org/x/image` 等；新增任何依赖必须停下问我。
- **`GOTOOLCHAIN=local`（已写入 Makefile）**：构建不得下载/切换 Go 版本；不升级工具链（本机 go1.24.5 / linux-arm64）。
- 权威状态（防重放账本、题池、统计）在 VPS 源站 SQLite。**这是工程选择，不是平台限制**——Cloudflare 免费计划**可以**使用
  SQLite-backed Durable Objects（ADR-001 已更正旧的错误断言）。CF 边缘是阶段 5 之后的评估项，不在阶段 1。
- 前端 widget：原生 JS 零依赖，gzip `[目标]` <30KB，中英双语，键盘可完成、`aria-live` 播报、移动端适配。

## 安全红线（违反即视为 bug，不接受"以后再说"）
1. token 必须 HMAC-SHA256 签名，含过期与站点绑定；签名密钥用**带域分隔的 HMAC 派生**（按 purpose + kid，DESIGN §6）。
   难度、TTL、站点、challenge id 必须在签名覆盖范围内；服务端以**库中行为权威**复核 PoW 参数，不接受客户端自报。
2. 密钥、摘要、token tag 的比较一律 `crypto/subtle.ConstantTimeCompare`。
3. 一次性消费的四条保证（ADR-004，缺一不可）：**原子条件状态转移**（检查旧状态与写入新状态在同一条 SQL 内）、
   **同一事务提交**、**检查 `RowsAffected == 1`**、**测试证明**（同池并发 / 两个独立 DB 句柄 / 进程重启后重放）。
   禁止"读-判-写回"。`db.BeginTx` 已发起 `BEGIN`，**严禁在 `*sql.Tx` 上再执行 `BEGIN IMMEDIATE`**（嵌套事务错误）。
   `SetMaxOpenConns(1)` 只是简化实现，**不是安全边界**，不得据此声称并发安全。
4. `secret_key` 只存带 pepper 的 HMAC 摘要，仅创建时展示一次；不存明文、不可找回。secret 校验必须先于任何状态消费——
   **错误 secret 不得消费其他站点的合法 token**。
5. 每题数据分**公开 payload** 与**服务端私有 verifier 数据**：答案、可推导答案的种子、判分秘密材料**绝不下发**、
   **绝不落库明文**、**绝不写日志**。容差验证由题型自行实现，**不得**为凑 HMAC 精确匹配而牺牲合理容差（ADR-003）。
6. 所有输入严格校验 + 大小上限（`http.MaxBytesReader`）+ 字段长度与格式上限；公开端点必须有**有界**限流
   （状态条目硬上限 + 清理策略），否则随机源地址可撑爆 1GB 内存。
7. SQL 100% 参数化；动态列名/排序走白名单映射。
8. 错误响应只暴露既定错误码族（`/siteverify` 用 reCAPTCHA 族；`/api/*` 用自有有限码集），**不透露哪一层失败**。
9. 来源地址默认只信真实连接地址；`X-Forwarded-For` / `CF-Connecting-IP` / `remoteip` 仅在 trust-proxy 显式开启且连接来自受信网段时采信（有测试）。
10. 随机数：安全语义（seed / nonce / 答案参数）只用 `crypto/rand`；图形抖动可用 `math/rand` 但每次独立播种、不共享实例。
11. 日志字段白名单；`secret`、token 全文、answer 不出现。验证方式是**按值扫描哨兵**，不是按字段名 grep。
12. 安全关键状态**事务提交之后**才返回成功；提交失败一律 fail closed。只有访问统计可异步，防重放账本不可（ADR-006）。
13. **没有测试就不要改动安全相关代码**（`internal/token`、`internal/pow`、`internal/store` 的消费路径、`internal/risk`、`internal/admin` 鉴权）。先补测试。
14. 论证题型安全性必须分别给出 M1 题面随机空间 / M2 答案空间与接受容差 / M3 随机猜测通过率 / M4 每次尝试成本（ADR-002）。
    **不得**用"用了 CSPRNG / 字体随机 / 噪声 / HMAC / 多步骤"或"题面组合数很大"直接证明抗 AI；3 次尝试下 M3 累计 >10% 的题型不得启用。
15. 不得仅凭通过率判定题型被机器破解并自动下线；淘汰必须分级（候选告警 + 人工确认，多信号同时成立才自动降权，ADR-006）。

## 目录约定
`cmd/server`、`cmd/devclient`、`internal/{api,pow,token,store,pool,generator,risk,admin,config,version}`、`migrations`、
`web/widget`、`deploy/{docker,cloudflare}`、`docs/{,scripts}`。
偏离此结构要在 DESIGN §15 写理由。测试与被测文件同包同目录；集成测试用 `t.TempDir()` 建独立 DB，禁止依赖外部环境或本机已有文件。

## 每阶段工作流（固定顺序）
1. 读 `docs/DESIGN.md` §17 对应验收标准 + `docs/DECISIONS.md` 相关 ADR。
2. **先写测试**（此时应红）。阶段内测试**不得用 `t.Skip` 占位**；未来阶段的占位测试要单独列明、不计入验收。
3. 实现到绿。
4. `make fmt-check vet race`（`-count=1` 不吃缓存）全通过；阶段 1 验收跑 `make stage1-check`。
5. 更新 DESIGN（§19 加一行索引）与 DECISIONS（若产生新决策）。
6. commit → **只有真实验收全部通过**才 `git tag stage-N-done`。失败就报告失败，不打完成标签。

命令见 `Makefile`，`make help`。关键项：`make build` / `make test` / `make race` / `make vet` / `make fmt-check` /
`make check`（fmt-check+vet+race+size）/ `make stage1-check`（端到端，真实暴力求解 PoW）/ `make secret-scan` / `make widget-size`。
**shell 必须保留退出码**：脚本用 `set -euo pipefail`，禁止用 `cmd | tail` 让下游成功掩盖上游失败。

## 仓库与推送
远端：`origin = git@github.com:shijianus/EpoCHA`（private，SSH 已认证为 `shijianus`）。Go 模块路径 `github.com/shijianus/EpoCHA`（与 origin 一致）。
**推送策略（2026-10-17 更新，取代上一轮"每阶段自动推送"的约定）：完成 commit/tag 后不自动 `git push`；推送只在用户明确指示时执行。**
既有边界不变：
- **禁止 `--force` / `--no-verify`**；非快进（远端有别人的提交）→ 停下来问用户，不要 rebase 或覆盖。
- **禁止删除或移动已推送的阶段标签**（`stage-0-done` 等）。
- **禁止把真实密钥材料提交进仓库**，即使 private：`EPOCHA_MASTER_KEY`、`EPOCHA_PEPPER`、站点 `secret_key`、`admin_key`、`.env`、`*.key`、`*.pepper`（已在 `.gitignore`）。
  理由（DESIGN §6）：git 历史不可撤销；private 的访问面不等于零；pepper 与 DB 同库存放会让摘要保护直接失效。仓库只放 `.env.example` 占位符。
  验收脚本里的密钥必须是**运行时随机生成的演示密钥**，写进临时目录并在清理时删除。
- 题库、语料、字体、图片素材属非机密资产，可以提交（R3 许可另议）。GitHub 单文件 >100MB 拒收、仓库 >1GB 告警：超限时改用 Git LFS 或"由生成器按 seed 确定性派生"而不入库。
- 提交前跑 `git status` 复核暂存内容；发现高熵串、`*_key`、`credentials` 之类可疑内容就停下来问。

## 停止并问我（不要自作主张）
- 要**删除已有文件**（含看起来无用的）。
- 要**新增依赖**、改动依赖大版本，或升级 Go 工具链。
- 与 `docs/DECISIONS.md` 的任一 ADR、或 DESIGN 关键决策（尤其 D2/D5/D8/D11/D12、§12 边缘与降级语义、§17 阶段边界）**相冲突**。
- 需要真实语料/字体/图片素材的许可证判断（DESIGN §18-R3）。
- 发现需要放宽任何安全红线。
- 要执行任何影响远端的操作（push、删分支、删标签）。

其余阶段内工作：**自主推进，不必逐步征求批准**，包括补测试、修 bug、调整内部函数签名、写文档。

## 不要做的事
- 不要编造库、标准库或第三方平台（如 Cloudflare 配额）的 API/限制。不确定就读 `$(go env GOROOT)/src/<pkg>`、module cache 里的真实签名、跑最小验证程序，或查当日官方文档并**注明日期**（ADR-001/R10 就是这条教训）。
- 不要为"将来可能需要"写抽象层、feature flag、兼容垫片。
- 不要写注释解释代码在做什么；只写非显而易见的**为什么**（安全约束、事务与并发边界、外部平台配额这类）。
- 不要跳过 hook、不要 `--no-verify`、不要 force push。
- 不要在未运行 `go test -race -count=1 ./...` 的情况下声称测试通过；UI/widget（阶段 4）无法浏览器实测时**必须明说"人工验证项未完成"**。
- 不要把未实测的数字写成事实；遵守 `[目标]` / `[估算]` / `[实测]` 标注约定（ADR-006）。
- 参考 `tiagozip/cap`（Apache-2.0）的协议与 `siteverify` 语义：**读思路，自己实现，禁止整段拷贝**。AGPL 项目（mCaptcha）不得作为代码基座。
