# AGENTS.md — epocha 工程约定

给后续会话的约束。你（agent）在此仓库工作时必须遵守本文。设计基线在 `docs/DESIGN.md`，**冲突时以 DESIGN.md 为准，且必须先改文档再改代码**（在其 §19 记录原因）。

## 项目是什么
自托管人机验证服务（CAPTCHA）：单 Go 二进制 + SQLite。三层防线 = PoW 成本门槛 → 生成式题池 → 风险打分决策。对外契约与 reCAPTCHA 的 `POST /siteverify` 兼容（`secret`/`response`/`remoteip` → `success`/`challenge_ts`/`hostname`/`error-codes`）。

## 硬性技术约束
- **只用 Go，纯 Go 依赖，禁止 cgo**。禁止引入 JVM、MongoDB、Postgres、Redis、外部二进制（ImageMagick/fontconfig/headless chrome）。
- 目标机器：1 vCPU / 1GB RAM / 50GB 磁盘。常驻 RSS 目标 **<100MB**，二进制 ≤35MB。任何可能显著抬高内存或 CPU 的改动，先量再改。
- 允许依赖：`modernc.org/sqlite`、`golang.org/x/image`、`golang.org/x/crypto`（HKDF）。标准库 `net/http` + Go 1.22+ `ServeMux` 路由。**新增任何依赖必须停下问我**（DESIGN §16）。
- 防重放、题池、统计一律在主节点（SQLite 事务），**不得**放到 Cloudflare（免费 KV 1000 写/天、D1 10 万行/天、DO 需付费、单次 10ms CPU）。
- 前端 widget：原生 JS 零依赖，gzip **<30KB**，中英双语，键盘可完成、`aria-live` 播报、移动端适配。

## 安全红线（违反即视为 bug，不接受"以后再说"）
1. challenge/response token 必须 HMAC-SHA256 签名，含过期与站点绑定；密钥用 HKDF 按用途派生。
2. 密钥、答案、token 的比较一律 `crypto/subtle.ConstantTimeCompare`。
3. nonce / 题目一次性消费必须是原子操作：`BEGIN IMMEDIATE` + `UPDATE ... WHERE status='ready'`（或 `used=0`）并检查 `RowsAffected == 1`。双花窗口由测试守护。
4. `secret_key` 只存带 pepper 的 HMAC 摘要，仅创建时展示一次；不存明文、不可找回。
5. 答案**绝不下发客户端**、**绝不落库明文**、**绝不写日志**。每个 Generator 必须有测试断言 payload 不含答案。
6. 所有输入严格校验 + 大小上限（`http.MaxBytesReader`：公开 4KB / 管理 64KB）；按 IP + site_key 限流。
7. SQL 100% 参数化；动态列名/排序走白名单映射。
8. 错误响应只暴露 reCAPTCHA 既有错误码族，**不透露哪一层失败**。
9. 随机数：安全语义（nonce/seed/答案参数）只用 `crypto/rand`；图形抖动可用 `math/rand` 但每次独立播种。
10. 日志字段白名单：可写 nonce 前 8 字符、site_key、耗时、错误码；不可写 secret/token 全文/answer/完整 payload。
11. **没有测试就不要改动安全相关代码**（`internal/token`、`internal/pow`、`internal/pool` 的原子消费、`internal/risk`、`internal/admin` 鉴权）。先补测试。

## 目录约定
`cmd/server`、`internal/{api,pow,token,store,pool,generator,risk,admin,config,version}`、`web/widget`、`deploy/{docker,cloudflare}`、`migrations`、`docs`。
偏离此结构要在 DESIGN §15 写理由。测试与被测文件同包同目录；集成测试用 `t.TempDir()` 建独立 DB，禁止依赖外部服务或本机已有文件。

## 每阶段工作流（固定顺序）
1. 读 `docs/DESIGN.md` 对应阶段验收标准（§17）。
2. **先写测试**（此时应红）。
3. 实现到绿。
4. `make fmt vet test race` 全通过；涉及性能阶段跑 `make loadtest`。
5. 更新 DESIGN（含 §19 变更日志）与本文（若约定变化）。
6. `git add` 相关文件 → commit → `git tag stage-N-done`。

常用命令见 `Makefile`：`make help` 列出。关键项：`make build` / `make test` / `make race` / `make vet` / `make stage1-check`（curl 全流程）/ `make secret-scan`（日志泄露密钥的 grep 测试）/ `make widget-size`。

## 仓库与推送
远端：`git@github.com:shijianus/EpoCHA`（private，SSH 已认证为 `shijianus`）。模块路径 `github.com/shijianus/EpoCHA`。
**用户已预先授权：每次阶段完成后 `git commit` → `git tag stage-N-done` → `git push origin main --follow-tags`。**
但推送仍有硬边界：
- **禁止 `--force` / `--no-verify`**；非快进（远端有别人的提交）→ 停下来问我，不要 rebase 或覆盖。
- **禁止把真实密钥材料提交进仓库**，即使仓库是 private：`EPOCHA_MASTER_KEY`、`EPOCHA_PEPPER`、任何站点 `secret_key`、`admin_key`、`.env`、`*.key`、`*.pepper`（已在 `.gitignore`）。理由见 DESIGN §6 末段：private 不等于安全边界（历史不可撤销、任何 repo 令牌/CI 产物/未来的可见性切换都会泄露），且 pepper 与 DB 同仓库存放会让摘要保护直接失效。需要示例时更新 `.env.example` 的占位符。
- **题库、语料、字体、图片素材属于非机密资产，可以提交**（DESIGN §8.2 R3 的许可问题仍需单独确认）。注意 GitHub 单文件 >100MB 直接拒收、仓库 >1GB 告警：这类资产先量体积，超限时改用 Git LFS 或改为"由生成器确定性派生（记录 seed）"而不入库。
- 提交前跑 `git status` 复核暂存内容，发现像密钥的东西（高熵串、`*_key`、`credentials`）就停下来问。

## 停止并问我（不要自作主张）
- 要**删除已有文件**（含看起来无用的）。
- 要**改动依赖大版本**或新增依赖。
- 与 `docs/DESIGN.md` 的决策（尤其 D2 SQLite-only、D5 答案摘要、D8 边缘不承担状态、§12 降级语义）**相冲突**。
- 需要真实语料/字体/图片素材的许可证判断（DESIGN §18 R3）。
- 发现需要放宽任何安全红线。

其余阶段内工作：**自主推进，不必逐步征求批准**，包括补测试、修 bug、调整内部函数签名、写文档。

## 不要做的事
- 不要编造库/标准库的 API。不确定就读 `$(go env GOROOT)/src/<pkg>` 源码或 module cache 里的真实签名，或跑一个最小验证程序；arm64 本机 Go 1.24.5。
- 不要为"将来可能需要"写抽象层、feature flag、兼容垫片。
- 不要写注释解释代码在做什么；只写非显而易见的**为什么**（安全约束、SQLite 单写者、CF 免费额度这类）。
- 不要跳过 hook、不要 `--no-verify`、不要 force push。
- 不要在未运行 `go test -race` 的情况下声称测试通过。UI/widget（阶段 4）若无法在浏览器实测，**必须明说"人工验证项未完成"**，不得假称通过。
- 参考 `tiagozip/cap`（Apache-2.0）的协议与 `siteverify` 语义：**读思路，自己实现，禁止整段拷贝**。AGPL 项目（mCaptcha）不得作为代码基座。
