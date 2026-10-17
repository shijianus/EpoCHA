# 阶段 1 安全复核（STAGE1-REVIEW）

> 日期：2026-10-17。执行者：本项目实现模型自检。
> **这不是独立第三方安全审计**：复核者与上一轮实现者是同一模型上下文，没有独立预算、没有渗透测试授权、
> 也没有覆盖未实现的范围（题库/widget/边缘）。文中"通过"仅指下列可复现检查在本机跑绿。
> 环境：linux/arm64，go1.24.5，`GOTOOLCHAIN=local`，仓库起点 commit `93306ef`。

## 1. 结论

阶段 1 的核心安全性质（一次性消费、签名绑定、来源地址裁决、日志不泄密、重启后账本仍生效）**成立**，
但发现 **6 个真实缺陷**，其中 2 个会破坏长期运行的正确性/契约。全部已修复并有回归测试；
另有 4 项"要求存在但测试缺失"，已补测试。修复后 `make stage1-check` 10 项验收、`go test -race -count=1 ./...`、
`make secret-scan` 均通过（退出码见 §5）。

结论：**可以进入阶段 2**。没有需要停下来征询用户的阻塞项。

## 2. 缺陷与修复

| # | 严重度 | 现象 | 复现证据 | 修复 |
|---|---|---|---|---|
| D-1 | 高 | `store.Purge` 在"challenge 已过期但由其签发的 response 仍有效且未消费"时**必然失败**，清理任务从此永久报错，账本只增不减 | 新增 `TestPurgeKeepsUnconsumedResponse`：修复前报 `store: 清理 challenges: FOREIGN KEY constraint failed (787)` | `Purge` 改为单事务、challenge 仅在 `NOT EXISTS(response 引用)` 时删除、每批 500 行；仍有效的 response 行保留 |
| D-2 | 中高 | `/siteverify` 的服务端故障返回 `invalid-request`——既不是 reCAPTCHA 码族、也不是 DESIGN §10.1 承诺的唯一扩展码 `internal-error`；`ErrInternalError` 常量定义了却从未被使用 | 新增 `TestSiteverifyUsesRecaptchaErrorFamily` | `/siteverify`（含 panic 兜底）故障改为 HTTP 5xx + `{"success":false,"error-codes":["internal-error"]}`；`/api/*` 保持自有码集 |
| D-3 | 中 | 站点维度限流按**调用方自报的 site_key 字符串**计数：随机编造 site_key 即可不断新建条目、把真实站点的计数挤掉并使其归零 | 新增 `TestUnknownSiteKeysDoNotPolluteSiteLimiter`（伪造 40 个 key 后 `siteLimit.Len()` 必须为 0，真实站点仍严格 2/min） | 先查库定位站点，再以**站点 id** 计入站点维度；`/api/redeem` 也在验签后补上站点维度 |
| D-4 | 中 | **没有任何机制限制库里同时存在的活动 challenge 行数**：限流器只控制速率，攻击者用足够多源地址即可持续扩大 SQLite 文件 | 新增 `TestActiveChallengeCapBoundsLedger`（store）与 `TestChallengeCapReturnsRateLimited`（api） | `InsertChallenge` 改为"写入 + 存量判定"在同一条 `INSERT ... SELECT ... WHERE (count)<limit` 内完成，返回 `ErrCapExceeded`；API 侧折叠为 429 `rate-limited`；配置项 `EPOCHA_MAX_ACTIVE_CHALLENGES{,_PER_SITE}` |
| D-5 | 低中 | `challenge/response TTL` 无上界：`EPOCHA_CHALLENGE_TTL_SEC=86400` 这类配置会把"必须保留以防重放"的窗口拉到一天，等价于让库永久增长 | 新增 `TestTTLUpperBoundAndActiveCaps` | 配置校验加 `[1s, 1h]` 上界，理由写入代码与文档 |
| D-6 | 低 | `serve()` 在 `srv.Serve` 返回错误的分支里 `close(purgeDone)`，而清理 goroutine 自身 `defer close(done)` 会在 ctx 取消后再关一次 → close of closed channel（进程随即退出，实际窗口很小，但属真实缺陷） | 代码路径审阅（`cmd/server/main.go`） | 删除手动 close，统一依赖 ctx 取消；注释说明为什么不能在这里关 |

## 3. 缺失测试已补齐（不一定对应缺陷）

- **PoW 的独立验算路径**（复核要求 4：不能只靠 devclient 复用 `internal/pow` 自证正确）
  - `TestSHA256KnownAnswerVectors`：`sha256("abc")`、`sha256("")` 对硬编码的 FIPS 180-4 摘要，钉死哈希输入的构造方式。
  - `TestPrefixBoundaryAgreesWithIndependentCheck`：测试内用**另一套逐位右移实现** `independentZeroBits` 判定 12/13/15/17（含非整字节难度），不调用被测 `LeadingZeroBits`/`Verify` 的判定结果。
  - `TestExactBoundaryOneBitShortIsRejected`：12..24 每一位的边界向量，两套实现必须给出同一个前导零数。
- **fail closed**：`TestDatabaseFailureFailsClosed` 关掉库句柄后 `/api/challenge`、`/api/redeem`、`/siteverify` 全部 5xx，响应体不含 `"success":true`，故障期间重放也不会变成成功。
- **竞争不泄露 SQL 错误**：`TestTwoHandlesConsumeSameResponse` 现在断言失败方拿到的是 `ErrConsumed` 哨兵而非驱动错误。
- **删除行 ≠ 文件缩小**：`TestPurgeFreesPagesNotFileShrink` 断言删空账本后主库文件**不会**缩回空库大小（实测 155648 vs 57344 字节），把这条运维事实固定下来；收缩需要 `VACUUM`，本专案不在清理任务里做它。

## 4. 逐条对照复核清单（要求 → 实际结论）

**4.1 两个消费点** — 成立。`Redeem`/`ConsumeResponse` 都是"条件更新 + `RowsAffected==1` + 同事务 + 提交后返回"；
成功响应只在 `tx.Commit()` 之后写出（`internal/store/store.go:Redeem/ConsumeResponse`、`internal/api/handlers.go:handleRedeem`）。
安全关键消费**全部同步落库**，本阶段没有任何异步持久化路径（只有访问统计在设计上允许异步，且尚未实现）。
库故障 → 5xx（D-2 修复后语义明确）。

**4.2 并发与重启** — 成立，测试覆盖：同池 N 路（`TestConcurrentRedeemExactlyOneSucceeds`、
`TestConcurrentSiteverifyExactlyOneSucceeds`）、两个独立 `*sql.DB` 句柄争抢同一文件
（`TestTwoIndependentHandlesSameFile`、`TestTwoHandlesConsumeSameResponse`）、重开句柄后重放
（`TestReplayAfterReopenFails`）、真实进程重启（api 层 `TestReplayAfterServerRestart` + `stage1.sh` 第 9 项）。
竞争失败方得到哨兵（本轮加强）。未消费且未过期的 token 重启后仍可验证——这是 DESIGN 的承诺，
`stage1.sh` 未测该路径，但 `TestMigrationIdempotentAndDataSurvives` 覆盖了数据存活；阶段 2 的 `stage2-check` 会补上端到端。

**4.3 token 与 PoW** — 成立。签名覆盖 `typ/kid/sid/cid/rid/iat/exp/bits/seed`，未知版本（前缀非 `v1`）、
未知 kid、用途混淆、跨站点、逐字节篡改、非规范 base64 均有测试；PoW 参数以库中行为权威复核
（`ch.PowBits != claims.Bits` 或 seed 不一致即拒），客户端无法降级难度或延长 TTL。
长度上限：token ≤4096、payload ≤2048、nonce 1..16 位十进制且 ≤1e12、bits 由 `NewParams` + DB CHECK 双重限定 12..24。
畸形/过期 token 在**昂贵的 PoW 验证之前**就被拒（`handleRedeem` 的顺序即如此）。
一处口径差异：DESIGN §6 写"payload ≤4KB"，实现是更严格的 2048B——**取更严者**，已在 DESIGN §19 记录。

**4.4 客户端与服务端不共享同一个错误** — 已按 §3 补独立向量。devclient 复用 `internal/pow` 只用于"能解出题"，
不再被当作正确性证明。

**4.5 签发滥用与数据增长** — 修复后成立，三者关系明确：
**限流**控速率（有界状态 + 淘汰）、**Caps** 控存量（活动 challenge 行数硬上限，原子判定）、
**Purge** 控体积（每批 500 行、只删已失效且无人引用的行）。
无效 site_key 不再产生站点维度条目（D-3）。清理不会删除仍在有效期内的防重放状态（D-1 + 新增测试）。
文件体积与行数是两件事（§3 最后一项）。

**4.6 HTTP 与凭据** — 成立。请求体改用 `http.MaxBytesReader`（此前是 `io.LimitReader` + 手工长度判断，
虽也有界但不符合红线 6 的写法要求）；服务器四个超时 + `MaxHeaderBytes` 全设；
`secret`/master key/pepper 不进日志（`make secret-scan` 按值扫描 + `stage1.sh` 对真实进程扫描）；
默认不信代理头（`TestClientIPIgnoysSpoofedHeadersByDefault`）；
`/healthz` 只回答"进程在跑"，**不查库**——DESIGN §10.1 把它定义为存活探针且明确"不做健康即放行的语义"，
实现与文档一致，本文件同时记录"目前没有就绪探针"这一事实（属运维缺口，不是缺陷）。

## 5. 本次运行结果

| 命令 | 退出码 |
|---|---|
| `gofmt -l .` | 0（无输出） |
| `go vet ./...` | 0 |
| `go test -count=1 ./...` | 0 |
| `go test -race -count=1 ./...` | 0 |
| `make stage1-check` | 0（10/10 PASS） |
| `make secret-scan` | 0 |

阶段 1 范围内**没有** `t.Skip`；仍存在的 8 条占位测试全部属阶段 3（`internal/pool`、`internal/generator`）
与阶段 5（`internal/admin` 的分页与淘汰规则），不计入本阶段验收。

## 6. 未解决 / 留给阶段 2 及以后

- 站点配置目前每次启动由配置文件**覆盖回库**（`PutSite` 的 `ON CONFLICT DO UPDATE`）：这正是阶段 2 必须消除的
  "重启复活已撤销密钥"风险，届时 SQLite 才是权威，配置只作显式一次性导入。
- `hostname` 仍是调用方自报值的回显，不是安全依据；阶段 2 引入服务端裁决的允许来源后重写这段语义。
- 无就绪探针、无 `VACUUM` 策略、无监控指标——属阶段 5 运维范围。
- PoW "浏览器 300–900ms" 等性能数字**仍未实测**，本次复核没有改变这一点。
