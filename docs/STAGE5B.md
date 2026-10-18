# STAGE5B — 阶段 5B：运行互斥、恢复边界收敛、部署候选清单与持续演练

日期：2026-10-18（主体）与 2026-10-19（收尾轮：支持范围取舍、门禁三类归属、启动配方实测）。
起点 HEAD `8e5c136`（`stage-5a-done`），工作树起点干净。
环境：`linux/arm64`（宿主 4 核可见，容器一律限制成 1 CPU）、Docker Engine 29.6.1、Compose v5.3.1、
Go 1.24.5。门禁：`make stage5b-check` → `docs/scripts/stage5b.sh`。

> **第一版正式支持范围只有 B 路径**（宿主静态编译 + named build context 装配）；
> A 路径容器内编译保持**可选、NOT_RUN**，见 §5.0。收尾轮没有新增产品功能，也没有重跑五分钟演练之外的历史阶段验收。

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

### 1.5 收尾轮加的东西：三类归属、启动配方演练、构建脚本修正

- **三类归属**（§2.0）+ 分类逻辑的 9 条负向自测（§2.0）。这是为了把"可选路径没跑"和"必需项没跑"
  在退出码上分开——前者不该阻塞已验证的交付，后者必须阻塞。
- **第 2.5 步：启动配方演练**（必需项）。`DEPLOYMENT` §11 原来那段是自相矛盾的：
  它先 `cd deploy/docker`，又写 `make docker-image-local # 在仓库根执行` —— `cd` 之后 `make` 找不到
  Makefile；而且它没有覆盖保护（`> keys/master` 会把已有主密钥直接冲掉，等价于撤销全部已签发 token）。
  新配方改成"整段从仓库根执行、不 `cd`，用 `-f/--project-directory/--env-file` 指定文件与项目根"，
  并加 `set -C`。第 2.5 步照着这个形状在**隔离部署根**里真跑一次，七条判据：
  ① 文档命令起得来且 `/readyz` 200；② 容器内 `epocha-ops admin` 通、`EPOCHA_ADMIN_KEY_FILE` 引导的凭据生效；
  ③ 建测试站点并完成一次真实闭环（`pow_bits=12` 由工具真解）；④ 同 token 二次验证被拒
  （`timeout-or-duplicate`）；⑤ `--force-recreate` 之后站点仍在用、**已消费 token 仍被拒**、锁仍在同一数据目录；
  ⑥ `stop` 之后宿主端口既连不上也查不到监听；⑦ 只定点清理本步的容器/网络/卷，第 2 步那套仍就绪。
  另外两条纯文本判据：compose 与 `.env.example` 的**变量名双向对账**（漂移就 FAIL，取不到列表也 FAIL），
  以及 `./data` 这类相对绑定按**项目目录**解析（防止演练写进仓库的 `deploy/docker/data`）。
- **`make run` 是坏的**（`run: build` 后面被一个 TAB 直接接了 `@mkdir ...`，make 把它当成 prerequisites，
  报 `No rule to make target '@mkdir'`）。这是 5A 引入的，本轮修正并把 `make -n run` 跑通。
- **退出码报告修正**：上一轮同时写了"make stage5b-check rc=3"和"make 会把 3 折叠成 2"——两句不可能同时成立。
  实测见 §2.1：`make` 只能给 0 或 2；配方现在把脚本真实码打印出来再按它退出。

---

## 2. 门禁：三类归属与真实结果

### 2.0 归属规则（`docs/scripts/stage5b.sh` 的口径）

| 类 | 内容 | 对退出码的作用 |
|---|---|---|
| **必需 R** | 受支持的 **B 路径**部署工程验收：分类自测、锁的真实进程测试、一次 `-race ./...`、静态检查与依赖隔离、容器/宿主互斥、恢复边界、候选清单、启动配方演练、持续演练 | 有 FAIL ⇒ **1**；有 BLOCKED/NOT_RUN ⇒ **3**；全 PASS ⇒ **0** |
| **可选 O** | 第一版正式支持范围之外：A 路径（容器内编译）、amd64 交叉构建、root 场景下的 UID 65534 | **不决定退出码**；只如实输出。未执行永远是 NOT_RUN，`opt_pass` 缺"实际执行证据"直接被结构拒绝（exit 2） |
| **超范围 S** | 产品效果项：人工可读性盲测、抗 OCR/抗 AI 基线、生产难度档 | 只登记边界，永不打印 `PASS`；由 `make blindtest` / `make ocr-baseline-real` 各自判定 |

负向自测（`bash docs/scripts/stage5b.sh --selftest-classify`，9 条场景断言）钉住四件事：
必需项 FAIL/BLOCKED/NOT_RUN 都得到非 0；**必需项没执行时不可能绿**；可选项未执行必须显示
NOT_RUN 且不改变必需项计数（同一批必需项计数行逐字符比对）；可选项不能被改写成 PASS；
超范围项的输出里不出现 PASS。这套自测自身被变异测试验证过：
把 `verdict` 的 BLOCKED/NOT_RUN 规则删掉 ⇒ 自测报 2 条 FAIL、rc=1；
把可选项计数并进必需项 ⇒ 自测报 4 条 FAIL、rc=1。

### 2.1 退出码的真实链路（实测，不是"包装层会透传"）

| 执行方式 | 观察到的码 | 说明 |
|---|---|---|
| `bash docs/scripts/stage5b.sh --scenario r-pass-only` | `0` | 必需项全通过 |
| `... --scenario r-fail` | `1` | 必需项 FAIL |
| `... --scenario r-notrun` / `r-blocked` | `3` | 必需项未执行／外部条件不足 |
| `... --scenario r-only-optional-notrun` | `0` | **可选项 NOT_RUN 不阻塞**已验证的 B 路径 |
| `bash docs/scripts/stage5b.sh`（完整门禁） | `0/1/3` | 真码，最后一行同时打印 `SCRIPT_RC=` |
| `make stage5b-check` | 非 0 时一律 `2` | make 的固有折叠；配方改为**先打印脚本真实码**再按真实码退出 |
| `make stage5b-check GATE_ARGS=--scenario r-notrun` | make 报 `2`，输出含 `真实退出码 = 3` | 用受控分支验证包装行为，不重跑五分钟演练 |

上一轮报告里"make stage5b-check rc=3"是错的表述（make 只能是 0 或 2）；正确读法见上表。
被脚本调用的程序（`go test`、`make fmt-check vet size`、`docker`、`epocha-ops`）的码都被
`rc=0; cmd || rc=$?` 显式捕获并写进对应项的文案，没有任何一处用 `| tail`/`| head` 掩盖上游失败。

### 2.2 本轮结果

见 §2.3 的逐项表（由 `bash docs/scripts/stage5b.sh` 的一次真实运行产出，绑定 `stage-5b-done`
指向的提交）。结论行形如：

```
必需项(B 路径部署工程) PASS=… FAIL=… BLOCKED=… NOT_RUN=…
可选项(A 路径/未验证平台) PASS=0 FAIL=0 BLOCKED=1 NOT_RUN=2
超范围项(产品效果，本门禁不判定) 登记=1
RESULT=… ; SCRIPT_RC=…
```

### 2.3 逐项表

| 步 | 类 | 项 | 状态 |
|---|---|---|---|
| 0 | R | 工具、五个端口空闲（含宿主 8081 与配方端口 18183）、磁盘余量 | PASS |
| 1 | R | 门禁自身分类逻辑的 9 条负向断言 | PASS |
| 1 | R | `internal/store` / `cmd/server` / `cmd/ops` / `cmd/loadtest` 的 `-race` 测试 | PASS |
| 1 | R | 服务侧 6 条真实子进程测试、运维侧 5 条测试**存在性**对账 | PASS |
| 1b | R | 一次全量 `go test -race -count=1 ./...` | PASS |
| 1b | R | `fmt-check` / `vet` / `size` + `cmd/server` 不链接开发期包 | PASS |
| 2 | R | 镜像重建（B 路径，含锁实现）+ `/readyz` healthy | PASS |
| 2 | R | 数据目录里出现 `epocha.lock`（宿主可见 ⇒ 同一 inode 的前提） | PASS |
| 2 | R | **宿主进程在服务容器运行时改不了库**（拒绝原因＝锁占用；不改库、不留凭据文件） | PASS |
| 2 | R | **第二个服务容器立即拒绝启动**并给出锁冲突原因；赢家不受扰动、仍就绪 | PASS |
| 2 | R | 停服后锁立即可取（锁文件仍在、未删除）⇒ `admin-recover` 成功且只打印 `key_id` | PASS |
| 2 | R | 重启后管理面认的正是恢复装入的那把（`whoami` 与 `key_id` 一致） | PASS |
| 2 | R | 服务持锁期间在线备份照常成功（产物 0600 + manifest） | PASS |
| 2 | R | 就地恢复（`--out-dir` 正被用作数据目录）被拒，且对工作库逐字节无副作用 | PASS |
| 2 | R | 同一份备份恢复到隔离目录成功（锁只挡就地恢复） | PASS |
| 2 | R | pepper 不配对 ⇒ 拒绝且不留下任何库形态产物 | PASS |
| 2.5 | R | compose 与 `.env.example` 的变量名**双向**对账（取不到列表也算 FAIL） | PASS |
| 2.5 | R | 密钥段幂等：`set -C` 拒绝覆盖已存在密钥，整段重跑后 master 未变 | PASS |
| 2.5 | R | §11 的调用形状（仓库根、不 cd、`-f/--project-directory/--env-file`）渲染合法：`host_ip: 127.0.0.1`、8081 无发布项 | PASS |
| 2.5 | R | `./data` 按项目目录解析到隔离部署根（不会写进仓库目录） | PASS |
| 2.5 | R | ① 文档命令真起得来（`--wait` 依 `/readyz`）② 容器内管理命令 + 引导凭据 ③ 真实闭环 | PASS |
| 2.5 | R | ④ 同 token 重放被拒（`timeout-or-duplicate`）⑤ 重建容器后站点可用且消费状态仍生效 | PASS |
| 2.5 | R | ⑥ 停止后端口释放 ⑦ 只定点清理本步资源（第 2 步那套仍在就绪） | PASS |
| 3 | R | 候选清单落盘（commit／工作树状态／Go／CGO／构建路径／SHA-256／镜像 ID／schema／实测 uid） | PASS |
| 3 | R | 进镜像两份二进制无 `PT_INTERP` | PASS |
| 3 | R | **B 路径**实际执行并通过 | PASS |
| 3 | **O** | **A 路径**（`make docker-image`，容器内编译） | **NOT_RUN**（见 §5） |
| 3 | **O** | amd64 交叉构建的镜像 | **NOT_RUN** |
| 3 | **O** | UID/GID 65534 下的目录属主与 `chown` 动作 | **BLOCKED**（无 root） |
| 3 | **S** | 人工可读性盲测、抗 OCR/抗 AI、生产难度档 | **OUT_OF_SCOPE**（不由本门禁判定） |
| 4 | R | 测量实例就绪（1 CPU/128MiB/只读根/非 root；抬限流仅用于测量） | PASS |
| 4 | R | 持续演练 450 次闭环全成功 | PASS |
| 4 | R | 过期清理**实际触发过**（日志非零计数） | PASS |
| 4 | R | 窗口内无 OOM、无重启 | PASS |
| 4 | R | 宿主上没有多出意外监听（带 `ss` 证据） | PASS |

### 2.4 真实子进程测试到底测了什么

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

## 3. 部署候选清单

**候选产物由干净提交构建**：本轮的构建发生在 5B 的收尾提交之后、工作树 `git status --porcelain` 为空时。
清单本身是**构建产物**，落在忽略目录里（`deploy/docker/.prebuilt/candidate-manifest.txt`，`.prebuilt/`
已在 `.gitignore`）——把"含当前 commit 的清单"再提交进仓库会形成自引用循环（提交改了，清单里的 commit 就过期）。
文档只负责指向**固定的源码提交**：`stage-5b-done` 标注里写明的构建绑定。

清单字段（每轮 `make docker-image-local` + `bash docs/scripts/stage5b.sh` 都会重新实测）：

| 字段 | 为什么必须单独记 |
|---|---|
| 完整源码 commit + `git describe` + **工作树 clean/dirty** | `dirty` 的 SHA 不能归到某个干净提交上 |
| Go 版本、宿主架构、docker/compose 版本 | `GOTOOLCHAIN=local` 不下载工具链，构建结果与本机 Go 绑定 |
| 宿主 `go env` 的 `CGO_ENABLED` 与构建实际使用的值 | 宿主有 gcc 时默认 **1**，而 scratch 要求 **0**（5A 的真实翻车点） |
| 编译参数与 `ldflags`（版本注入） | 决定 `epocha --version` 与备份 manifest 的 `app_version`/`commit` 能否对上仓库 |
| **构建路径**＝B（宿主静态编译 → `.prebuilt/out` → named build context 装配） | A 路径未执行，不能写成"两条路径都验证过" |
| 进镜像两份二进制的 SHA-256 + 无 `PT_INTERP` 断言 | 动态链接的二进制在 scratch 里的报错是"文件不存在"，极难归因 |
| 本地镜像 ID + 标签 + 镜像大小 | **本地 image ID 不是 registry digest**；本轮不推送镜像，也就没有 digest |
| schema 版本 | 迁移状态是恢复流程的判据之一（`/readyz` 也复核它） |
| 实际测试的运行 UID/GID | 与镜像默认 `65534:65534` 不同就是"未验证项"，不能含糊 |
| 备份 manifest 的 `commit` 与 HEAD 是否一致 | 不一致意味着"备份说的是另一个版本的状态" |

复现与核对：

```bash
git status --porcelain            # 必须为空，否则下面的 SHA 不属于这个提交
make docker-image-local           # B 路径；版本串取自 git describe / rev-parse
bash docs/scripts/stage5b.sh      # 第 3 步会重新落盘候选清单
sed 's/^/  /' deploy/docker/.prebuilt/candidate-manifest.txt
docker image inspect epocha:5a --format 'id={{.Id}} size={{.Size}}'
```

启动方式统一用 `docs/DEPLOYMENT.md` §11（整段从仓库根执行、不 `cd`；`set -C` 保证重跑不覆盖密钥；
uid/gid 与数据目录属主一致）。旧的"`cd deploy/docker` + 后面又 `make docker-image-local # 在仓库根执行`"
那个形状已经在这一轮被替换掉——它自相矛盾：`cd` 之后 `make` 找不到 Makefile。
`stage5b.sh` 的第 2.5 步就是照着新形状在**隔离部署根**里真跑一遍（§2.3 里那 7 条）。

> 上一轮记录的 `8e5c136…-dirty` 清单（epocha `1c349b02…`、epocha-ops `d5aa006b…`、
> 镜像 `30871ca7…`）对应的工作树是 **dirty**（含未提交的 5B 改动）。它属于"5A 提交 + 未提交改动"，
> 不是任何干净提交的产物，保留在这里只作为历史测量值。

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

## 5. 支持矩阵、未完成项与完成标签的边界

### 5.0 第一版正式支持范围（架构取舍，已定）

**只把 B 路径列为受支持且需要验收的构建路径**：宿主 `CGO_ENABLED=0` 静态编译 → `deploy/docker/.prebuilt/out`
→ named build context 装配运行镜像。A 路径（`make docker-image`，容器内编译）**保持可选、NOT_RUN**——
这不是"把 A 改成 PASS"，而是**缩小第一版正式支持范围**：低磁盘环境下为它拉大镜像没有收益，
而装配出来的运行时层与 B 完全相同（同一份 Dockerfile 的 runtime 阶段）。

| 范围 | 状态 | 依据 |
|---|---|---|
| B 路径构建 + compose 启动 + 互斥/恢复/在线备份/持续演练 | **必需项，实测通过** | §2.3 的 R 行 |
| A 路径（容器内编译） | **可选，NOT_RUN** | registry 直连被黑洞；容器内解包模块需 1.5–2GB 而本机余量是百 MB 量级 |
| amd64 交叉构建 | **可选，NOT_RUN** | 本机 arm64，无 amd64 验证环境；不引入 buildx/QEMU 体系 |
| UID/GID 65534（镜像默认 USER）+ `chown` | **可选，BLOCKED** | 本机无 root；实测组合是 `1000:1000` |
| 公网 TLS / DNS / 真实反代证书 | **不在本轮范围** | 本轮不是公网部署授权；模板与语义写在 `DEPLOYMENT` §6/§7 |
| 题型抗 AI/抗 OCR、人工可读性、生产难度档 | **超范围（产品效果）** | 由 `make blindtest` / `make ocr-baseline-real` 判定；容器化通过**不**构成它们的结论 |

`make stage5b-check` 在这套归属下给出 0/3 两种结果：必需项全通过 ⇒ **0**（即使 A 路径仍是 NOT_RUN），
必需项有未执行 ⇒ **3**。可选项永远不会被计入通过，也不会成为"不通过"的理由——这正是"缩小支持范围"
在门禁里的表达方式。

### 5.1 其他未完成项

| 项 | 状态 | 说明 |
|---|---|---|
| 宿主 swap（512M + swappiness=10） | 未建立 | 本机 swap 为 0 且无 root；容器侧 `memswap_limit == memory` 已实测生效（`docker inspect`） |
| 真机移动端 / 辅助技术 / 人工各题型过 5 次 | 仍属阶段 4 的未完成人工项 | 与本轮无关，见 AGENTS「尚未实现」 |
| 后台页面、统计与分级淘汰、非视觉等价通道、CF 边缘 | 不存在 | 阶段 5 其余部分 |

### 5.2 `stage-5b-done` 到底覆盖什么

标签正文写明：仅覆盖 **B 路径、linux/arm64、实际运行 UID/GID 1000:1000**；
**不**代表 A 路径、amd64、公网 TLS、人工可读性或抗 AI/抗 OCR 已通过。
打标签的条件是必需项全部实测通过（互斥、恢复边界、候选构建、启动配方、持续演练）；
只要其中任何一条没跑，就提交结果与原因，不打标签。

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
| 9 | 收尾轮：`变量名双向对账` 在文件不存在时**通过**了 | 演练台里 `ROOT` 被替换成 `/`，两个 `grep` 都失败、列表为空 ⇒ "没有漂移"。判据本身写反的第二例：**空输入不等于通过**。现在取不到列表直接 FAIL，并且 PASS 文案带上两个列表的条数（23/20），条数为 0 就藏不住 |
| 10 | 收尾轮：`建不出测试站点` 但看不到原因 | `admin_r` 失败时只 tail 了 `.err`，而 400 的正文在 stdout 那份 `.raw` 里。真实原因是我填了 `pow_bits:10`，而 `internal/pow.MinBits=12` —— 下界由代码规定，不是文档里的数字 |
| 11 | 收尾轮：`compose 渲染出的端口…` 检查永远找不到那个字符串 | `docker compose config` 把端口渲染成 **映射形式**（`host_ip:` / `target:` / `published:`），不是 `"8080:8080"` 字符串。照 5A 时的字符串形状写的断言会因为"找不到违规形式"而恒真。现在按真实渲染形状断言，并且"缺 host_ip"才是 FAIL 条件 |
| 12 | 收尾轮：`set -C` 在函数里泄漏到主脚本 | `keys_section() { set -C; ... }` 之后所有对已存在文件的 `>` 都会失败。函数体写成 `(...)` 才把它关在子 shell 里 |
| 13 | 收尾轮：清理断言写成"第 2 步那套的网络必须存在" | 单独演练第 2.5 步时那套根本没起，于是"定点清理"被判成"波及了别人"。改成直接证明赢家仍就绪（`/readyz` 200），并且第 2 步没跑时如实说明"无从波及" |

第 5 条最值得留档：它和 5A 那条"容器里没引入 widget 脚本所以永远不出现"是同一类错误——
**判据本身写错时，失败方向是反的**，而且看起来像被测系统出了问题。

另有一类**不在脚本里**的发现，值得与上面并列记下：限流窗口按秒切（§1.3）。它同样是"判据本身不对"，
只是发生在被测系统内部，而且被一个恰好跨过盲区的测试长期掩盖。

## 7. 命令

```bash
make stage5b-check          # 本轮门禁：退出码只由必需项决定（见 §2.0/§2.1；make 本身只会报 0 或 2）
make stage5b-check GATE_ARGS=--selftest-classify    # 只验分类逻辑与包装层行为，不重跑五分钟演练
bash docs/scripts/stage5b.sh --selftest-classify    # 同一件事的**真码**版本（0/1）
make stage5a-check          # 5A 的部署/恢复演练（收尾轮只改了它的 admin-recover flag 与 §11 的形状）
go test -count=1 -race ./internal/store/ ./cmd/server/ ./cmd/ops/   # 只跑锁与互斥相关
EPOCHA_SITE_SECRET=… make loadtest ARGS="--base http://127.0.0.1:8080 --site-key pk_… --requests 450 --concurrency 1 --pace 660ms"
```

第 2.5 步单独调试时用的办法：把脚本里"第 2.5 步到第 3 步之间"的片段连同前面的记录器/工具函数
抽出来，`DOCKER_OK=1` 直接跑——同一份文本、不重新实现判据，迭代一次约 3 分钟而不是整套门禁十几分钟。
§6 的第 9–13 条全部是在这个演练台上暴露的；抽片段的 `ROOT` 需要手工指回仓库根（第 9 条就是这么撞出来的）。
