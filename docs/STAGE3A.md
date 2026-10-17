# 阶段 3A：生成式题库基础设施 + 流程基线生成器

> 定位：这一阶段交付的是**工程基线**——生成器接口、有界预生成题池、原子领题、题目与 challenge 的一对一绑定、
> 单次答案提交，以及一条纯文本基线题型 `arithmetic-baseline-v1`。
> **它不是一道抗自动解题的题目**（见文末"基线题型能被脚本解出"），也不包含视觉题型、风险打分、自动淘汰、widget。
> 因此标签是 `stage-3a-done`，不是 `stage-3-done`。
> 设计依据：`docs/DESIGN.md` §5.4、§8.3、§8.4、§10.1、§10.2、§13、§17；决策记录 `docs/DECISIONS.md` ADR-011。

## 1. 状态转换表

### 1.1 题池记录 `pool.status`

| 当前 | 触发 | 转移后 | 事务/条件 | 之后能否再领 |
|---|---|---|---|---|
| `ready` | `/api/challenge` 领题成功 | `assigned`（写 `challenge_id`、`assigned_at`） | 与 challenge 行**同一事务**；`UPDATE … WHERE status='ready' AND expires_at>?` 且影响行数 = 1 | — |
| `ready` | 领题竞争失败 | **整笔回滚**（challenge 行也不留下） | 同进程内 `SetMaxOpenConns(1)` 让事务串行，这一支走不到；跨进程共享库文件时过期快照那一笔 `UPDATE` 拿到 `database is locked` 而报错，"影响 0 行"只是理论形态（`TestStaleSnapshotClaimFailsClosed` 钉住） | 能（客户端重试，得到的是**另一张** challenge） |
| `ready` | 未被领取就过期 | 删除 | `Purge`：`expires_at < now` | 否 |
| `assigned` | PoW 通过且答案**正确** | `terminal`（写 `terminal_at`，保留 `challenge_id`） | 与 challenge 置 `redeemed` + 插 `responses` 行同一事务，然后 commit | **否** |
| `assigned` | PoW 通过但答案**错误** | `terminal` | 与 challenge 置 `invalid` 同一事务，**commit 后**返回 `OutcomeWrongAnswer`（不是 error） | **否** |
| `assigned` | 畸形请求 / 答案文法不合法 / 来源不符 | 不变 | 这些判定在进入终结事务之前就返回 | 是（但仍是同一个 challenge 的那道题，不能换题） |
| `assigned` | PoW 解错 | 不变 | 沿用阶段 1 的失败计数（3 次作废 challenge） | 是，直到所属 challenge 作废；challenge 作废后该题**不回池**，随题目 TTL 过期被清理 |
| `terminal` | 清理 | 删除 | `Purge` 第一步之后（先 `responses`、再 `pool`、最后 `challenges`） | 否 |

**没有任何代码路径把 `assigned`/`terminal` 改回 `ready`。** "记录不复用"是结构性的，不是概率性的。

### 1.2 challenge `challenges.status`（3A 变化部分）

| 当前 | 输入 | 结果 | 对外 |
|---|---|---|---|
| `issued` 且未过期 | token 验签失败 / 站点停用 / 版本不符 / 来源不符 | 不变 | `invalid-challenge` 400 |
| `issued` | 请求体畸形、`answer` 文法不合法 | 不变 | `invalid-request` 400（**不消耗作答机会**） |
| `issued` | PoW 解错 | `attempts+1`，满 3 次转 `invalid` | `invalid-challenge` 400 |
| `issued` | PoW 正确 + 答案**正确**（`pow_question`）或无题目层（`pow_only`） | `redeemed` + `redeemed_at`，并签发 `responses` 行 | 200 + `response` token |
| `issued` | PoW 正确 + 答案**错误** | `invalid` + `attempts+1`，题目转 `terminal` | `invalid-challenge` 400（**同一 token 之后永远这个结果**） |
| `redeemed`/`invalid`/过期 | 任何再次提交 | 不变 | `invalid-challenge` 400 |

`/siteverify` 的语义 3A **未改动**：先按 secret 定位站点（错误 secret 绝不消费任何 token），
一次性消费 `responses.used`，已用/过期/不存在 ⇒ `timeout-or-duplicate`；站点停用或重新启用不复活旧 token（`svid` 签名快照）。
它**不接受**调用方自报"题目已通过"之类的任何字段：JSON 请求体只读 `secret`/`response`/`remoteip`/`hostname` 四个键。

## 2. 配置与默认水位

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `EPOCHA_POOL_ENABLED` | `true`（未设即启用） | 设 `false`/`0` 关闭后台 worker。**关闭后 `pow_question` 站点在池空时得到 503，不会降级** |
| `EPOCHA_POOL_LOW_WATERMARK` | 50 | 低于此值触发补题 |
| `EPOCHA_POOL_HIGH_WATERMARK` | 100 | 补到此值停止（`refilling` 迟滞的撤除线） |
| `EPOCHA_POOL_MAX_ROWS` | 20000 | 全局行数硬上限，判定与插入在同一条 SQL 内 |
| `EPOCHA_POOL_WORKERS` | 1 | 上限 `MaxWorkers=4`（防配置手滑把 1 vCPU 打成 N 倍负载） |
| `EPOCHA_POOL_QUESTION_TTL_SEC` | 4200 | 必须 > 站点最大 challenge TTL（3600s），否则领题条件永不成立 |
| `EPOCHA_POOL_POLL_MS` | 2000 | 水位检查周期，下限 100ms |

代码内固定值（不开放为配置，避免"配置面"变成攻击面）：`BatchPerTick=20`、`YieldEvery=10`、`YieldPause=100ms`、
退避 `100ms → 10s`、每次生成熵读取上限 4096B、领题重试 5 次、payload ≤4096B / verifier ≤512B（库内 CHECK 同步强制）。

站点侧只有三个字段决定用不用题：`verify_mode`（`pow_only`|`pow_question`）、`generator_id`、`difficulty`（1..5）。
**迁移后的旧站点是 `pow_only`**；`pow_question` 只能经 `PUT /api/admin/sites/{id}` 显式开启，且写入前校验
`generator_id` 已在本进程登记（否则库里就存了一条只会 503 的策略）。

## 3. 资源实测（2026-10-17，本机 linux/arm64，go1.24.5，`go test -bench BenchmarkPoolFill -benchtime 1x ./internal/pool/`）

测量形状：`t.TempDir()` 独立库、`BatchPerTick = n`（一轮补满）、`YieldPause` 取默认 100ms、难度档 3。

| 项 | 实测值 |
|---|---|
| 补 100 题 | 墙钟 1.029s，其中**主动让出 CPU = 1.0s**；纯生成+写入 **29ms** ⇒ **0.29ms/题** |
| 补 1000 题 | 墙钟 10.34–10.39s，其中让出 **10s**；纯生成+写入 **339–391ms** ⇒ **0.34–0.39ms/题** |
| 单题较大块（payload 或 verifier 的较大者） | **62–63 字节**（上限 4096/512，由 CHECK 与 `InsertQuestion` 双重强制） |
| 库文件（写入进行中，含 `-wal`/`-shm`） | 100 题：1,944,456 B；1000 题：4,677,256 B |
| 库文件（关闭即 checkpoint 后） | 100 题：184,320 B；1000 题：573,440 B ⇒ **净增 ≈432 字节/题**（(573440−184320)/900，含页填充开销，远大于裸字段和） |
| 进程 VmRSS（读 `/proc/self/status`） | 9.5–9.6MB（100 题）、13.0–13.1MB（1000 题） |

**真实服务进程**（`cmd/server` 二进制，非测试进程）另测一次，形状是"池里 200 道 ready 题 + 20 次
challenge→redeem→siteverify 完整闭环全部成功"：

| 项 | 实测值 |
|---|---|
| `VmRSS` / `VmHWM` | **15,488 kB（15.1MB）** —— 两者相同，说明这一形状下没有更高的历史峰值 |
| `VmSize` | 1,827,984 kB（虚拟地址空间保留，**不是**驻留内存，别拿它当 RSS 读） |
| `Threads` | 9（HTTP + admin + purge + 1 个池 worker + runtime） |
| 库文件（运行中） | `epocha.db` 233,472 B + `-wal` 4,124,152 B + `-shm` 32,768 B |
| 库文件（`wal_checkpoint(TRUNCATE)` 后） | **245,760 B**（含 schema、站点、200 条题目与 20 次闭环的账本行） |

复现这一形状的最小步骤（密钥一律运行时生成，库在 `mktemp -d` 里，端口用 `:0` 由内核分配）：

```bash
go build -trimpath -ldflags "-s -w" -o $W/epocha-server ./cmd/server
EPOCHA_MASTER_KEY=… EPOCHA_PEPPER=… EPOCHA_ADMIN_KEY=… \
EPOCHA_POOL_ENABLED=true EPOCHA_POOL_LOW_WATERMARK=190 EPOCHA_POOL_HIGH_WATERMARK=200 \
EPOCHA_POOL_POLL_MS=500 \
  $W/epocha-server --db $W/epocha.db --listen 127.0.0.1:0 --admin-listen 127.0.0.1:0 &
# 建 pow_question 站点（pow_bits 必须 ≥12，低于 `pow.MinBits` 会被管理面 400 拒绝）→ PUT /origins
# 轮询 GET /api/admin/pool 直到 ready≥190 → 跑 20 次 devclient --mode full
awk '/VmRSS|VmHWM|VmSize|Threads/' /proc/$PID/status   # 驻留与峰值
python3 -c "import sqlite3;c=sqlite3.connect('$W/epocha.db');c.execute('PRAGMA wal_checkpoint(TRUNCATE)')"
```

**这些数字的含义边界**：
- "总耗时"里有 **97% 是故意睡掉的**（每 10 题让出 100ms）。把墙钟当生成成本会得出"10ms/题"这种错得离谱的数；
  纯成本是它的百分之一量级。两个数都报，是为了让"让出 CPU"这件事在数字里可见。
- 上表的 VmRSS 属于 **go test 进程**，含测试框架、`crypto/rand`、SQLite 驱动与 page cache，**不能代替服务进程**；
  服务进程另测得 **15.1MB**（见上表"真实服务进程"）。两者都**不是**长期运行/真实流量形状下的稳态值，
  因此不足以宣布 §13 的 `<100MB` 目标"已达标"，只能说"3A 的形状下远低于该线"。
- 库大小报"写入中"与"checkpoint 后"两个数：WAL 模式下只 stat 主文件会把刚补完 100 题的库测成 4096 字节（本轮真实踩到过这个假数），
  只报三文件之和又会把未合并的日志算成净占用。
- `0.29 → 0.34ms/题` 的随规模上升不是非线性生成成本，是小样本下让出次数计算与调度抖动的合成；
  两次运行的差值在 10% 量级，**没有做重复测量取均值**。

## 4. 未测项与原因（不编造数值）

| 项 | 状态 | 原因 |
|---|---|---|
| 服务进程 RSS | **已测（功能闭环形状）** | 见 §3"真实服务进程"：**15.1MB**，200 题 + 20 次闭环。**仍未测**的是长期运行、真实流量形状与 20000 题满载下的稳态 RSS |
| 池满载 20000 行的磁盘与耗时 | **未测** | 外推 ≈8.4MB（432B/题 × 20000），在 50GB 磁盘下不构成约束；先测"打满之后的行为"比先测"打满要多久"更有价值（已有 `TestRefillStopsAtMaxRows`） |
| claim 吞吐上限（双句柄并发） | **未测** | 3A 测的是**正确性**（同文件并发只有一个领取者），不是吞吐。SQLite 单写者的 claim/s 量级仍属 `[估算]`（R6，阶段 5 压测） |
| `pool_gen_ms` 直方图 | **没有该指标** | 本版没有 metrics 导出面，只有进程内计数 + `LastErr` 类别（§8.3） |
| 站点数与 `low_watermark` 公式 `clamp(2×站点数,50,500)` | **未按公式动态实现** | 3A 用水位常量。按站点数动态化需要"站点数变化时重算"的策略，那属于阶段 5 的运维面，不是本阶段性质 |
| 答案分布的多次重复测量 | **单次抽样** | 每档 5000 样本跑一次打印概率（§8.4 的 d1≈0.045 等）。重复运行在小数点第二位抖动，未取均值、未给置信区间 |
| PoW 与题池同时打开时的端到端 p99 | **未测** | 需要真实负载形状与并发到达过程（阶段 5 `make loadtest`） |
| widget 与浏览器侧 | **不存在** | 阶段 4 |

## 5. 基线题型能被脚本解出（必须显式声明）

`arithmetic-baseline-v1` 的公开 payload 里 `task` 就是一道可直接求值的表达式（如 `"(9 - 3) × 5"`）。
**答案可从 payload 推导是它的设计事实，不是泄露缺陷**：本阶段要验证的是生成器接口、题池不变量与提交生命周期，
不是题目的抗机器能力。因此：

- 验收脚本 `docs/scripts/stage3a.sh` 用**自己的独立求值器**解析公开题面求答案，
  **不读数据库**、**不依赖答案调试接口**、**不硬编码固定解**——这同时证明了 payload 自足性与判分正确性。
- 管理面 `/api/admin/generators` 对它返回 `production_ready:false` 与
  `note:"工程基线题型：题面可被脚本直接求值，不抗自动解题，仅用于验证题池与提交生命周期"`。
- 不得用"随机实例多"（M1）论证它安全；M3 按**最大单点概率**算，且**答案分布不均匀**已被实测记录（§8.4）。
  d1 档 3 次尝试的累计猜测上界 ≈13%，已超过 §8.2 的 10% 门槛 —— 这正是它"只作基线、不作生产题型"的依据。
- 本文与 DESIGN 中**没有**任何"抗 OCR / 抗多模态模型 / 能证明是真人"的表述适用于 3A。

## 6. 敏感材料的扫描分层

- 按值扫描哨兵：站点 secret、管理凭据、主密钥、pepper 都不出现在公开响应、管理响应、服务日志里；
  **答案与私有 verifier 的扫描分两层**——集成测试按**值**扫（`TestPowQuestionHappyPath` 从 challenge token 解出 cid、
  读回 48 字节 verifier，再扫它的 hex/std-base64/raw-base64 三种形态），端到端脚本按**字段名**扫
  （`"answer"`、`"verifier"`、`"seed"` 这类键不得进日志）。脚本层不按值扫答案是刻意的：
  算术答案是小整数，`dur_ms=97` 这种合法日志字段会撞出假阳性，而假阳性会掩盖真问题。

## 7. 命令

```bash
make stage3a-check          # 端到端 10 项（随机演示密钥、临时库、内核分配端口）
go test -count=1 ./...      # 单元/集成
go test -race -count=1 ./...
go test -bench BenchmarkPoolFill -benchtime 1x -run '^$' ./internal/pool/   # §3 的实测入口
```

手工最小闭环（服务已在 `127.0.0.1:8080`、管理面在 `127.0.0.1:8081`，站点 `sk-demo` 已配 `pow_question`）：

```bash
curl -s -XPOST 127.0.0.1:8080/api/challenge -d '{"site_key":"sk-demo"}'   # 看 mode / question.task
curl -s -XPOST 127.0.0.1:8081/api/admin/pool -H "Authorization: Bearer $ADMIN_KEY"
```
