# STAGE5B — 阶段 5B：运行互斥、恢复边界收敛、部署候选清单与持续演练

日期：2026-10-18。起点 HEAD `8e5c136`（`stage-5a-done`），工作树起点干净。
环境：`linux/arm64`（宿主 4 核可见，容器一律限制成 1 CPU）、Docker Engine 29.6.1、Compose v5.3.1、
Go 1.24.5。门禁：`make stage5b-check` → `docs/scripts/stage5b.sh`。

> **本轮不新增产品功能**：没有新题型、没有后台页面、没有风险打分/自动淘汰、没有 CF 边缘、没有多节点。
> 做的是两件事——"谁能在什么时候动这个数据目录"，以及"这一版到底实测了哪条构建路径"。
> 与 5A 同样的边界：**容器/运维演练通过不等于题型抗 AI/抗 OCR 通过**，
> `text-image-v1` 仍是 `experimental:true` + `production_ready:false`。

---

## 1. 本轮实现的东西

### 1.1 运行互斥：数据目录上的 `flock` 排他锁

`internal/store/lock.go`：`<数据目录>/epocha.lock`，`O_RDWR|O_CREATE`（0600）+ `LOCK_EX|LOCK_NB`。

| 规则 | 为什么是这条而不是别的 |
|---|---|
| 服务在 `store.Open`（含迁移）**前**取得，`db.Close` **后**释放 | defer 是 LIFO，注册顺序决定这一点。写反会留下"锁已放、库还开着"的缝，那正是离线运维能挤进来的位置 |
| 非阻塞，拿不到就失败退出 | 不出现"运维以为在等、其实在死等"；也没有 `--wait` 这种可选语义需要维护 |
| **永不删除锁文件** | 崩溃/`SIGKILL`/容器被杀后内核释放锁但文件留下，这是正常状态。删除会把两个进程分到两个 inode，`flock` 只认 inode ⇒ 锁**静默失效**，比没有锁更危险 |
| **不是 PID 文件** | PID 会复用，"PID 不在就删锁"的窗口里两个写者可同时通过；也不需要在锁文件里写任何东西 |
| `ErrLockHeld`（被占用 → 先停服）与 `ErrLockAccess`（uid/挂载不对 → 换身份）分族 | 两者的处置方式相反。混成一句会让运维去等一个永远不会到来的释放 |

| 命令 | 取锁？ | 依据 |
|---|---|---|
| `epocha`（服务） | 是（按 `EPOCHA_DB` 所在目录） | 唯一写这条数据目录的常驻进程 |
| `epocha-ops admin-recover` | 是（按 `--db`） | 它改 `admin_keys`；`--admin-base-url` 探活 flag 已删除，无引用的 `probeStatus` 一并删掉 |
| `epocha-ops restore` | 是（按 `--out-dir`） | 拦住的正是"隔离恢复被写成就地恢复"这个最常见手滑；恢复到隔离目录不受影响 |
| `epocha-ops backup` / `verify` | **否** | 在线一致性快照的正确语义允许与服务并发（只读连接 + `VACUUM INTO`）。加锁会把按频率备份变成不可持续的事——所以脚本里有一条**反向断言**防"顺手加锁" |
| `check` / `admin` | 否 | 只走 HTTP，不碰库 |

能力边界（不是客套，是上限）：`flock` 是建议锁，只约束按规矩办事的进程。
同等文件权限下绕开本仓库代码直接打开 SQLite 的本地程序不受它阻止。本轮不引入强制隔离
（独立属主、独占挂载、LSM 策略），因此**不得**把它描述成"防恶意本地进程"。

取锁会在目标目录留下 `epocha.lock`，失败时也不删除——`stage5b.sh` 与 `BACKUP-RESTORE.md` 都按这个事实判定"半成品"
（只看 `.db`/`.partial`/`-wal`/`-shm`，不看锁文件）。

### 1.2 恢复边界的收敛

- `BACKUP-RESTORE.md` §2 新增"哪些命令需要停服"表，与 §1.1 一一对应。
- §2 新增**四种"可信"分开**的表：checksum 与数据库一致性这一版**有**；来源真实性与保密性**没有**
  （manifest 无签名、快照是明文库）。⇒ 明确写下"备份目录的可写者 = 备份内容的作者"，
  不宣称抵御恶意替换。
- §5 新增 `admin-recover` 的**逐步失败表**（凭据文件已写、库未提交之间崩溃怎么办）。
  顺序是"取锁 → 写凭据文件（`O_EXCL` + 失败自清）→ 一条事务改库"，判据是退出码，事后可核对
  `whoami` 的 `key_id` 与审计里的 `admin.recover_local`。
- §7 不能保证表补三条（来源真实性、保密性、建议锁的上限），并说明它们同样**阻止安全开放**，不是备注。

### 1.3 顺带查出并修掉的真实缺陷：限流窗口比配置名松 60 倍

写持续演练时才发现的问题（不是本轮引入的）：`internal/risk` 的窗口分桶是 `bucket() = now().Unix()`
（**每秒**一个窗口），而 `windowSize` 是一分钟、配置叫 `EPOCHA_RATE_IP_RPM` / `rate_site_rpm`、
`RetryAfter` 也按分钟报剩余时间。后果是 `rpm=60` 实际允许 60 次/**秒** ≈ 3600 次/分钟，
红线 6 要求的"有界限流"边界被静默放宽一个数量级。

- 为什么一直没人发现：唯一的窗口测试把时钟一次推进整个 `windowSize`（60 秒），
  而 60 秒当然也换了一个"秒桶"——它同时满足"窗口=1 分钟"和"窗口=1 秒"两种实现。
  **测试推进的步长恰好落在缺陷的盲区里**。
- 修法：分桶按窗口对齐（`Unix() / 60`），`RetryAfter` 用 `bucketStart()` 还原窗口起点
  （直接拿序号当 Unix 秒会算到 1970 年附近）。
- 钉住语义的两条新测试：`TestLimiterWindowIsNotPerSecond`（推进 1 秒不得恢复配额、跨过整窗口才恢复）、
  `TestLimiterBucketBoundaryIsStable`（同一窗口内跨 30 秒仍算同一窗口）。原测试保留，职责不同。
- 全量 `-race ./...` 在修复后通过：没有任何测试依赖过宽的旧窗口。
- 这是**收紧**，不是放宽：站点级 `rate_*_rpm` 的语义从"每秒"回到配置名声明的"每分钟"。
- 叫法修正：DESIGN/AGENTS 里"内存令牌桶"改成"有界固定窗口计数器"——实现从来不是令牌桶，
  而叫错会让读者以为存在连续补充速率与突发桶深两个可分开的参数（详见 ADR-016）。
- 连带影响：5A 的负载形状数字是在旧窗口下量的，本轮**重测**（见 §4 与 `STAGE5A.md`），
  不保留旧值再加注脚。

### 1.4 负载工具的节流

`cmd/loadtest` 增加 `--pace`（每 worker 两次闭环之间的最小间隔）。没有它，450 次闭环会在 1 秒内打完，
"5 分钟观察内存/WAL/题池随时间变化"就无从谈起；报告里现在必须打印 pace 与目标速率，
否则峰值数字与稳态数字长得一模一样。

---

## 2. 门禁结果（`docs/scripts/stage5b.sh`，真实一次运行）

```
工程项 PASS=29 FAIL=0；外部条件 BLOCKED=0；本轮未执行 NOT_RUN=1
RESULT=blocked（无失败，但 NOT_RUN=1 ⇒ 不算阶段完成）
脚本退出码 = 3        （直接跑脚本才是真码；make 会把 3 折叠成 2）
```

| 步 | 项 | 状态 |
|---|---|---|
| 0 | 工具、四个端口空闲（含宿主 8081）、磁盘余量 | PASS |
| 1 | `internal/store` / `cmd/server` / `cmd/ops` / `cmd/loadtest` 的 `-race` 测试 | PASS |
| 1 | 服务侧 6 条真实子进程测试、运维侧 5 条测试**存在性**对账 | PASS |
| 1b | 一次全量 `go test -race -count=1 ./...` | PASS |
| 1b | `fmt-check` / `vet` / `size` + `cmd/server` 不链接开发期包 | PASS |
| 2 | 镜像重建（含锁实现）+ `/readyz` healthy | PASS |
| 2 | 数据目录里出现 `epocha.lock`（宿主可见 ⇒ 同一 inode 的前提） | PASS |
| 2 | **宿主进程在服务容器运行时改不了库**（拒绝原因＝锁占用；不改库、不留凭据文件） | PASS |
| 2 | **第二个服务容器立即拒绝启动**并给出锁冲突原因；赢家不受扰动、仍就绪 | PASS |
| 2 | 停服后锁立即可取（锁文件仍在、未删除）⇒ `admin-recover` 成功且只打印 `key_id` | PASS |
| 2 | 重启后管理面认的正是恢复装入的那把（`whoami` 与 `key_id` 一致） | PASS |
| 2 | 服务持锁期间在线备份照常成功（产物 0600 + manifest） | PASS |
| 2 | 就地恢复（`--out-dir` 正被用作数据目录）被拒，且对工作库逐字节无副作用 | PASS |
| 2 | 同一份备份恢复到隔离目录成功（锁只挡就地恢复） | PASS |
| 2 | pepper 不配对 ⇒ 拒绝且不留下任何库形态产物 | PASS |
| 3 | 候选清单落盘 + 进镜像两份二进制无 `PT_INTERP` | PASS |
| 3 | **B 路径**（宿主 `CGO_ENABLED=0` 编译 + named build context 装配）实际执行并通过 | PASS |
| 3 | **A 路径**（`make docker-image`，容器内编译） | **NOT_RUN**（见 §5） |
| 4 | 测量实例就绪（1 CPU/128MiB/只读根/非 root；抬限流仅用于测量） | PASS |
| 4 | 持续演练 450 次闭环全成功，用时 311s（1.45 闭环/秒） | PASS |
| 4 | 过期清理**实际触发过**（日志非零计数） | PASS |
| 4 | 窗口内无 OOM、无重启 | PASS |
| 4 | 宿主上没有多出意外监听（带 `ss` 证据） | PASS |

### 2.1 真实子进程测试到底测了什么

`cmd/server/lock_test.go`（重新执行测试二进制、按 `EPOCHA_TEST_ROLE` 分派；每个子进程按 pid 定向 kill，
绝不按名字模式批量清理）：

1. 两个服务进程抢同一数据目录 ⇒ 最多一个启动；输家在 20s 内退出且原因＝运行锁，且**没碰过库**（锁在 `store.Open` 之前）。
2. SIGTERM 优雅停止后，新进程可正常启动。
3. SIGKILL 后锁可重新取得，锁文件仍在。
4. 目录里已有一个没人持有的 `epocha.lock` ⇒ 不阻塞启动（含"文件里写着别人家 PID"的变体）。
5. 启动竞态 4 轮：服务 vs 离线持锁方，任何一轮都不允许"两个写者同时活着"，也不允许"两个都失败"。
6. 服务持锁期间，同目录的第二次 `AcquireDirLock` 返回 `ErrLockHeld`。

`cmd/ops/main_test.go`：

1. 真实持锁进程在场时 `admin-recover` 拒绝（退出码 1、文案含"停服"），库 sha256 逐字节未变，凭据文件未留下，
   且存量管理凭据未被破坏。测试里还故意开一个"看起来像管理面"的 HTTP 服务在**别的端口**答话，
   用来证明判据与端口无关（这正是 5A 探活实现会放行的情形）。
2. 持锁进程退出后 `admin-recover` 可成功（不需要任何"清理陈旧锁"）。
3. `backup` 在持锁期间成功（反向断言）。
4. `restore --out-dir <被服务使用的目录>` 被拒；释放后同一份备份恢复到隔离目录成功。
5. 两个 `admin-recover` 子进程竞态 ⇒ 恰好一个成功、输家不留凭据文件、赢家确实改了库。

`internal/store/lock_test.go` 钉住机制本身：互斥、释放后可重取、0600、永不删除、
陈旧文件不阻塞、`ErrLockAccess` 与 `ErrLockHeld` 不互相冒充、以及"锁绑在 open file description 上"这条依赖事实。

---

## 3. 部署候选清单（实际测量值）

```
源码 commit（测量时的 HEAD）: 8e5c136c79c32b98f5929d975bc0886021d6773c   describe: stage-5a-done-dirty
工作树（测量时）: dirty（含本轮 5B 改动，随后提交为本节末的 commit）
Go: go1.24.5 linux/arm64
宿主 go 默认: GOOS=linux GOARCH=arm64 CGO_ENABLED=1 GOTOOLCHAIN=auto      ← 注意宿主默认是 1
镜像构建实际使用: CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
ldflags: -s -w -X internal/version.Version=… -X …Commit=8e5c136 -X …BuildDate=2026-10-18T13:41:46Z
宿主架构: aarch64   docker: 29.6.1

进镜像的 epocha      sha256: 1c349b02d297148b7706d38bfe655d894732c3a6aac64d3defeea38f5126c7a8
进镜像的 epocha-ops  sha256: d5aa006b63ccef4c80c713343cd74dae6dc61703661fc73119a236b5543b5d27
演练用 epocha-ops    sha256: 563b553ee871475f26a58ed52a8c7f549c12540b36c339a4eed138ab61e4529e
本地镜像 ID: sha256:30871ca7eb925dd1cfd3e408d6f279b17e917ea16964d4a1266a337cfc6d054f   （本地 image ID，**不是** registry digest）
镜像大小: 8,680,228 字节
schema 版本: 4        app_version: stage-5a-done-dirty        备份 manifest 的 commit 与 HEAD 一致: yes
已测试的运行 UID/GID: 1000:1000（镜像默认 USER 65534:65534；见 DEPLOYMENT §2/§5.5 的 uid 规则）
两份进镜像的二进制均无 PT_INTERP（静态链接，scratch 前提）
```

**这条清单的有效范围必须一起读**：SHA-256 对应的是**测量时那个工作树**（含未提交的 5B 改动）。
提交后重跑 `make docker-image-local` 会注入新的版本串、二进制 SHA 随之改变——这是预期，不是漂移。
候选启动方式（`docs/DEPLOYMENT.md` §11）：

```bash
cd deploy/docker
mkdir -p data keys backups && chmod 700 data keys backups
head -c32 /dev/urandom | base64 > keys/master; head -c32 /dev/urandom | base64 > keys/pepper
chmod 600 keys/master keys/pepper
cp .env.example .env    # 无 root 的机器把 EPOCHA_UID/GID 改成当前用户
make docker-image-local && (cd deploy/docker && docker compose up -d --wait)
curl -s http://127.0.0.1:8080/readyz
```

## 4. 持续演练（约 5 分钟，硬上限）

形状：1 CPU / 128MiB / 只读根 / 非 root / `GOMEMLIMIT=56MiB`，并发 1、`--pace 660ms`、450 次闭环封顶。
限流抬高（`EPOCHA_RATE_IP_RPM=6000`）**只存在于测量实例的命令行**，不写进 compose——
部署配置里不混入测试高限流、不混入演示凭据、不自动启用实验题型、不开放管理端口。

```
wall=5m0.054s  flows=450  ok=450（0 失败、0 超时）  吞吐=1.50 闭环/秒（目标 1.52）
采样（t=218s / 250s / 281s）：CPU 0.48%/0.57%/0.35%，内存 7.695→7.750→7.762 MiB / 128MiB，/readyz 全程 200
结束时容器内存 8.535MiB；OOMKilled=false、RestartCount=0
题池 ready 0→24 行，图片字节 152,305（text-image 高档站点在窗口内被后台补池）
库 139,264→454,656 字节；WAL 383,192→4,128,272 字节
过期清理实际触发：msg=清理 responses=90 pool=0 challenges=90 audit=0
```

第一次重跑（抬高站点配额之前）的形状也留在记录里，因为它是 ADR-016 那个修复**生效的证据**：
`flows=450 ok=169`、`challenge:429/rate-limited=281`。默认 `rate_ip_rpm=60` 在修复前根本拦不住 1.5 次/秒，
修复后按每分钟生效——同一份脚本、同一份负载，差别只有窗口口径。

必须写下的读法：

- 这是 **pow_only 流量**的持续观察。图片题这一路只测到"后台补池在限额内确实工作到水位"，
  **没有**真实或受控的图片解题器，因此**不**把它写成图片题闭环吞吐。
- PoW 由工具代算（`pow-tool` 段），不代表浏览器里真人的解题耗时；真人侧数字仍只有 4A/4B 的 Firefox 采样。
- 5 分钟不是长期稳定性证明：它没覆盖跨天的内存/WAL 增长、备份定时任务的实际抖动、也没有真实流量分布。
- WAL 停在 ~4.1MB 是因为自动 checkpoint 阈值（1000 页）；主文件增长反映的是清理与补池的正常写入，
  观察窗口内没有单调膨胀的迹象，但这不构成"无泄漏"的结论。

## 5. 未完成项与唯一出口

| 项 | 状态 | 原因与恢复执行 |
|---|---|---|
| A 路径：`make docker-image`（容器内编译） | **NOT_RUN** | Docker Hub 直连被黑洞（只有 mirror 可用）；实测在容器内解包 Go 模块需要 1.5–2GB，本轮起始磁盘 450–900MB 量级。任务明确禁止为"全绿"去拉大型镜像，故保留 NOT_RUN。恢复：在 registry 可达且 `/var/lib/docker` 所在分区 ≥2GB 的构建机上 `make docker-image && bash docs/scripts/stage5b.sh` |
| 跨架构（amd64）镜像 | NOT_RUN | 本机 arm64，无验证环境；不引入交叉构建体系（5A 的同一决定） |
| 真机 root 场景下的 `chown 65534` 权限动作 | BLOCKED（无 root） | 镜像 `USER` 仍是 65534；本轮演练以 uid 1000 跑（`DEPLOYMENT` §2 记录差异）。上线时按 §2 执行并复测 |
| 宿主 swap（512M + swappiness） | 未建立 | 本机 swap 为 0 且无 root；容器侧 `memswap_limit == memory` 已实测生效 |
| 公网 TLS / DNS / 部署 | 不做 | 本轮不是公网部署授权 |

因为 A 路径是 NOT_RUN，`make stage5b-check` 的退出码是 **3 而不是 0**。
按 AGENTS 的三态纪律，本轮**不打** `stage-5b-done`：本轮范围内的互斥、恢复边界与持续演练全部实测通过，
但"部署候选"要能被两条构建路径各自复现才算完整。唯一下一步是在满足条件的构建机上跑一次 A 路径
（或明确接受 A 路径长期 NOT_RUN 并据此决定标签）。

## 6. 本轮门禁自身修掉的假信号（记录，避免下次重犯）

| # | 现象 | 真因 |
|---|---|---|
| 1 | `pace=0 时 wall=133ms，看起来仍在睡` | 断言用了绝对毫秒数；8 次带 PoW 的闭环本来就要一百多毫秒。改成与节流下限比较 |
| 2 | `被拒的就地恢复改了工作库` | sha 基线取在 `admin-recover`（合法改库）**之前**。改成动作前当场取基线 |
| 3 | `pepper 不配对的恢复留下了产物` | 取锁会在目标目录留下 `epocha.lock`，被算成了产物。半成品的判据改成 `.db`/`.partial`/`-wal`/`-shm` |
| 4 | `这些二进制是动态链接` | 检查对象是演练自己 `go build`（宿主 `CGO_ENABLED=1`）的那份。改成断言**进镜像的两份** + 演练份也按 0 编 |
| 5 | `宿主上出现了预期之外的服务监听` | `curl -w '%{http_code}'` 连不上时自己打印 `000`，`|| echo 000` 又补一个 ⇒ 得到 `"000\n000"`，"端口空闲"被读成"有人在听" |
| 6 | `docker wait -t 30s` | 这个子命令没有 `-t`。改成有界轮询 `State.Status`，并区分"命令写错"与"第二个实例真的活着" |
| 7 | 持续演练中途 281/450 次 `challenge:429` | 不是缺陷，是**限流窗口修复（ADR-016）第一次真正生效**：站点默认 `rate_ip_rpm=60` 从此真的是每分钟 60 次。测量站点因此显式抬高配额（只抬在测量实例上，不写进 compose） |
| 8 | 脚本在第 4 步开头直接终止，没有 RESULT 行 | 我把注释插在了反斜杠续行的**中间**：续行被注释截断，下一行的 JSON 变成一个"命令名"，`set -e` 让脚本当场退出。`bash -n` 不报这种错（语法合法），只有真跑才暴露 |

第 5 条最值得留档：它和 5A 那条"容器里没引入 widget 脚本所以永远不出现"是同一类错误——
**判据本身写错时，失败方向是反的**，而且看起来像被测系统出了问题。

另有一类**不在脚本里**的发现，值得与上面并列记下：限流窗口按秒切（§1.3）。它同样是"判据本身不对"，
只是发生在被测系统内部，而且被一个恰好跨过盲区的测试长期掩盖。

## 7. 命令

```bash
make stage5b-check          # 本轮门禁（退出码 0/1/3；见 §2 与 §5）
make stage5a-check          # 5A 的部署/恢复演练（本轮未回归它，只改了它调用的 admin-recover flag）
go test -count=1 -race ./internal/store/ ./cmd/server/ ./cmd/ops/   # 只跑锁与互斥相关
EPOCHA_SITE_SECRET=… make loadtest ARGS="--base http://127.0.0.1:8080 --site-key pk_… --requests 450 --concurrency 1 --pace 660ms"
```
