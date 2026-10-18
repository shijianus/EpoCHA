# STAGE5A — 阶段 5A 结果：单节点部署包、密钥持久化、备份恢复与资源演练

日期：2026-10-18。执行环境：`linux/arm64`（4 核可见但容器被**限制**成 1 CPU），
Docker Engine 29.6.1 / Compose v5.3.1，Go 1.24.5（`GOTOOLCHAIN=local`）。
门禁：`make stage5a-check` → `docs/scripts/stage5a.sh`（下列数字取自已提交树上的那一次完整运行，脚本退出码 0）。

> **本轮结论的边界**：通过的是"部署包、密钥、备份恢复、边界与资源形状"的工程验收。
> **它不等于**题型抗 AI/抗 OCR 通过：`text-image-v1` 仍是 `experimental:true` +
> `production_ready:false`（DESIGN §8.4、红线 14），阶段 4B 的真实 OCR 基线（低/中/高档 96%/31%/3%）
> 仍然是唯一有效的效果数字。本轮**没有**重跑 OCR 基线，也没有改变任何题型判定语义。

---

## 1. 门禁结果（一次运行，逐项）

最终一次完整运行（`docs/scripts/stage5a.sh`）的结论：

```
工程项 PASS=90 FAIL=0；外部条件 BLOCKED=0；本轮未执行 NOT_RUN=0
RESULT=pass       脚本退出码 0
```

| 步 | 内容 | 结果 |
|---|---|---|
| 0 | 工具可用、磁盘余量、端口空闲（含宿主 8080/8081） | PASS |
| 1 | 一次代码回归：`make stage3b-check`（内含 fmt-check/vet/`-race` ./.../size/secret-scan/stage1/2/3a/3b） | PASS（退出码 0） |
| 1b/1c | 5A 新增包的定向 `-race`、`go list -deps` 依赖隔离、`cmd/ops` 与 `cmd/loadtest` 的按值扫描 | PASS |
| 2 | 镜像构建 + 交付清单 + 镜像默认形状（用户/健康检查/入口/监听） | PASS |
| 3/4 | 密钥文件工作区、compose 配置合法性、缺密钥/权限过宽/env+`_FILE` 同时设置的拒绝形状 | PASS |
| 4b | 宿主二进制上的同一套密钥规则（`*_FILE`、同时设置即拒、缺失即拒且不带出路径） | PASS |
| 5 | 首次初始化、`/readyz`+`/healthz`、`docker inspect` 核实安全边界、宿主监听表、管理面边界 | PASS |
| 6 | 站点创建与一次性消费、重建容器后的持久化、管理凭据锁死与 `admin-recover` | PASS |
| 7 | 备份 → 篡改检测 → 保守恢复 → 隔离恢复实例的旧 token/旧凭据语义 | PASS |
| 8 | 库不可写时的 fail closed、SIGTERM 优雅停止与端口释放 | PASS |
| 9 | 容器日志按值扫描 + 启动摘要记录密钥**来源** | PASS |
| 10 | 1 CPU / 128MiB 限额下的有界负载、图片补池、库与 WAL 体积、OOM 计数 | PASS |
| 11 | Nginx 模板的真实路由：管理面不回传、缓存语义、日志形状、413 | PASS |
| 12 | 交付文件与 `make` 入口存在性 | PASS |

三态口径按 AGENTS 执行：工程项只有 PASS/FAIL；本轮**没有** BLOCKED/NOT_RUN 项。
唯一没做的是"公网 TLS 证书签发与公网监听"——那是**明确不做**（本轮不是公网部署授权），
不是 BLOCKED，见 §7。

## 2. 交付面

| 文件 | 内容 |
|---|---|
| `deploy/docker/Dockerfile` | 两阶段：`toolchain`（`golang:1.24-alpine`，`CGO_ENABLED=0`、`GOTOOLCHAIN=local`）→ `runtime`（`FROM scratch`）。`USER 65534:65534`、`HEALTHCHECK` 用镜像内 `epocha-ops check --path /readyz`、`ENTRYPOINT` exec 形式 |
| `deploy/docker/compose.yml` | 只发布 `127.0.0.1:8080`；`read_only`、`cap_drop ALL`、`no-new-privileges`、`memory`/`memswap_limit` 128m、`cpus 1.0`、`pids 128`；数据/密钥(`:ro`)/备份三个分开挂载；`json-file` 日志 3×5m；`stop_grace_period 15s`；钉住子网供受信代理配置 |
| `deploy/docker/.env.example` | compose 变量样例，**不含密钥** |
| `deploy/docker/nginx.conf.example` | 反代主示例（Nginx），属性见 §6 |
| 根 `.dockerignore` | 把 `eval-out/`（ground truth）、开发期命令、密钥与库文件挡在构建上下文之外 |
| `cmd/ops` | `backup` / `verify` / `restore` / `check` / `admin` / `admin-recover` / `version`；退出码 0/1/2 分层 |
| `cmd/loadtest` | **开发期**有界负载工具（并发 ≤16、请求 ≤5000），分段时延 + 有限失败类别 |
| `internal/config/keyfile.go` | `EPOCHA_<NAME>_FILE` 入口；`LoadPepper`/`LoadAdminKey` 与 server 同源 |
| `internal/store/deploy.go` | `Backup`（VACUUM INTO + manifest）、`VerifyBackup`、`Restore`（保守失效）、`RecoverAdminKey`、`Ready` |
| `internal/store/capability_test.go` | 把 VACUUM INTO 依赖的四条驱动事实钉成测试（能力边界由测试给出） |
| `internal/api` | `GET /readyz`（与 `/healthz` 分列） |
| 文档 | `docs/DEPLOYMENT.md`、`docs/BACKUP-RESTORE.md`、本文件；DESIGN §6/§10.1/§12/§13/§15/§17/§19 与 ADR-014 |

镜像交付清单（`docker create` + `docker export` 的真实文件系统视图，20 个条目）：
镜像层里**只有** `/usr/local/bin/epocha` 与 `/usr/local/bin/epocha-ops`；
`.dockerenv`、`/etc/hosts`、`/dev/*`、`/proc`、`/sys` 是运行时挂进去的。
没有 shell、`curl`、`sqlite3`、OCR、评测样本、ground truth、浏览器、`go.mod`。
镜像大小 `[实测]` **≈8MB**（`docker image inspect .Size`），二进制 `[实测]` `make size` 口径 10MB 级、预算 35MB 内。

## 3. 端口与管理面的实际边界

`docker inspect` 核实的容器形状（不是"文件里写了"，是内核在执行）：

```
ReadonlyRootfs=true  SecurityOpt=[no-new-privileges:true]  CapDrop=[ALL]
Memory=134217728     MemorySwap=134217728（容器内不许 swap）  NanoCpus=1e9  PidsLimit=128
Config.User=1000:1000（本轮演练；镜像默认 65534:65534，见 §7 的限制说明）
```

宿主监听表 `[实测]`：只有 `127.0.0.1:18080`（演练端口，生产默认 8080）；
`8081` 与恢复实例端口在宿主上**不存在**。公共端口上 `/api/admin/*` 是 **404**（路由根本不存在，不是 401）。

管理面唯一通道是容器内 `epocha-ops admin`（loopback-only、`/api/admin/*` 前缀、方法白名单）。
本轮**没有**放宽应用既有的管理监听约束，也**没有**开第二套通道（无 SSH 隧道、不发布 8081、无未鉴权恢复接口）。

## 4. 资源演练（1 CPU / 128MiB 限额内）

测量形状必须先说明配置前提：默认 `EPOCHA_RATE_IP_RPM=60` 作用在**连接地址维度**上
（`internal/api/handlers.go:729`），站点级 `rate_ip_rpm` 只覆盖"地址×站点"那一对，盖不住它。
单源压测先撞上的是这个限流器，不是服务端容量 —— 所以两种形状**分开**记。

**默认配置形状**（80 次闭环，并发 4，打到默认站点）：`[实测]`（ADR-016 修好限流窗口之后重测的一次；
本节所有 5A 数字都取自那次重测，不保留修复前的旧值再加脚注）
```
flows=80 ok=19 wall=90ms rps=892.09  err=challenge:429/rate-limited=58 redeem:429/rate-limited=3
```
⇒ 19 次成功、61 次被限流。这证明限流器在起作用，**不能**读成"服务端容量是 19"。

**容量测量实例**（`EPOCHA_RATE_IP_RPM=60000`、其余限额与生产一致，150 次闭环 / 并发 4）：`[实测]`

| 段 | n | p50 | p95 | max |
|---|---|---|---|---|
| `/api/challenge` | 150 | 2.162ms | 7.676ms | 36.414ms |
| `/api/redeem` | 150 | 2.765ms | 8.832ms | 41.189ms |
| `/siteverify` | 150 | 2.044ms | 6.430ms | 54.217ms |
| PoW（**工具侧代算**） | 150 | 3.116ms | 11.123ms | 27.310ms |

```
wall=572ms  flows=150  ok=150（0 失败、0 超时、0 `SQLITE_BUSY`）  吞吐=262.18 闭环/秒 ≈ 787 请求/秒
容器（负载期间）：CPU=40.00%  MEM=7.754MiB / 128MiB
OOMKilled=false  RestartCount=0
库体积 147,456 → 438,272 字节；WAL 622,152 → 4,128,272 字节
```

**图片补池**：分桶来自"启用的 `pow_question` 站点"（`store.QuestionBuckets` 的 WHERE 条件），
所以本轮建了一个真的视觉题站点（`text-image-v1`，难度档 5）来测补池 —— 否则池永远是空的，
"图片补池"这一项根本没测到东西（第一轮就是 `ready=0`）。`[实测]`
```
限额内补池：ready=20 行、图片字节 126,778；负载后 ready=24 / 合计 24 行、图片字节 152,143
```

必须写下的表述纪律（AGENTS/DESIGN §13）：
- PoW 由**访问者浏览器**求解。上表 "PoW（工具侧代算）" 是测试工具负载，**不是**真人解题耗时；
  浏览器侧的真实数字在 4A/4B（Firefox headless、纯 JS SHA-256 422,833 次/秒、bits=20 提交→verified 309–8,991ms）。
- 本服务端的 150 次闭环**没有**跨阶段缓存复用，但它是**单一负载形状**（pow_only、bits=12、`allow_missing_origin`），
  不是真实流量分布；样本上限 150，属于"有界演练"，不是稳态容量结论。
- `[实测]` 宿主有 4 核；容器被限制成 1 CPU 后测得，不能拿宿主机核数替代。

**运行边界的另外两条实测**：`docker compose stop -t 15` 下进程在 1s 内退出且发布端口释放
（`ss -ltn` 复测无该端口，容器状态 `exited`）；把数据目录以只读方式挂给一个一次性实例时，
`/readyz` 与 `/api/challenge` 都拿不到 200，`/siteverify` 也绝不返回 `success:true`
（本轮实测的形状是"进程起不来"，比"起来了但每次验证都失败"要好——两种都被门禁接受，"照常服务"不被接受）。

## 5. 备份与恢复的实测结论（演练证据）
逐条对应 `docs/BACKUP-RESTORE.md`：

- 备份产物：`epocha-<stamp>-<commit>.db`（**0600**）+ `.manifest.json`，出现在**独立的备份挂载目录**；
  manifest 字段齐备（`app_version`/`commit`/`schema_version`/`sha256`/`pepper 指纹`/`integrity_check`/`counts`），
  且不含任何凭据明文（按值扫描）。
- `epocha-ops verify` 独立复核通过；**改一个字节后 verify 拒绝**（sha256 与 `integrity_check` 都参与判定）。
- 保守恢复：产出**新文件**，工作库逐字节未变；交付物是单文件库（无 `-wal`/`-shm`/`.partial`）。
- pepper 与备份不配对 ⇒ 拒绝恢复且**不产生任何文件**。
- 用**新主密钥**（且不设 previous）启动的隔离恢复实例上：
  `/readyz` 200（可服务探针 ≠ 业务开放）、备份前签发的旧 response 被判失败、
  所有站点默认禁用（领题 400）、新装入的管理凭据可用而旧管理凭据被拒、
  **旧站点凭据一律不可用**（`site_secrets` 已清空）⇒ 必须重新签发；重签后新闭环成功。

## 6. 反代模板的路由实测（真实 nginx 容器 + 自签名测试证书）

| 断言 | 结果 |
|---|---|
| 经反代 `/healthz`、`/api/challenge`、`/siteverify` 走通且保持各自响应族形状 | PASS |
| `/api/admin/*` 不回传（404）；未知路径不透传（404） | PASS |
| widget 脚本透传后端自身 `Cache-Control: public, max-age=3600`（不覆盖成 immutable） | PASS |
| 动态端点显式 `no-store` | PASS |
| `client_max_body_size 8k`：超大体在反代层 413 | PASS |
| 访问日志只记路径与状态：无 query、无凭据值（按值扫描） | PASS |

演练与生产的差别只有一处并已在脚本里注明：上游指向 compose 网络内的服务名（宿主部署用 `127.0.0.1:8080`），
证书是 1 天有效期的自签名。**这不构成"公网 TLS 已验收"**。

`X-Forwarded-For` / `EPOCHA_TRUSTED_PROXIES` 的配对形状（容器看到的源地址是网络网关而非 `127.0.0.1`）
写在 `docs/DEPLOYMENT.md` §6.3；本轮按**默认 `trust-proxy=false`** 运行，
即"所有请求共用一个连接地址桶"，这也是 §4 里 60 RPM 形状的直接成因。

## 7. 本轮没做、受环境限制、以及上线前必须人工确认的事

**明确不做（本轮范围内）**：公网 DNS/证书签发/公网监听；多节点与自动扩缩容；管理后台页面、
统计与分级淘汰、风险打分（阶段 5 其余部分）；CF Worker/DO（阶段 5 之后评估）；
题型、widget 判定语义的任何改动；OCR 基线重跑。

**受环境限制**：

1. **规范容器内编译路径（`make docker-image`）本轮 NOT_RUN**。原因是两个叠加的外部条件：
   Docker Hub 直连被黑洞（DNS 指向异常地址，仅镜像 mirror 可用），且 `/var/lib/docker` 所在分区
   在本轮起始只有 1.2GB 空闲（`/tmp` 下另有约 5.4GB 属于**其它工作**，不是本仓库产物，未清理）。
   在本机跑通它需要约 1.5–2GB 的 Go 模块解包与编译中间产物。
   实际交付并演练的是同一份 Dockerfile 的装配路径（`make docker-image-local`，
   `--build-context toolchain=<预编译目录>` 替换 toolchain 阶段），**运行时层完全相同**。
   恢复执行：在有 ≥2GB 空闲磁盘的构建机上 `make docker-image`，然后 `bash docs/scripts/stage5a.sh`。
2. **演练容器以 uid 1000 运行**：这台宿主没有 root，无法把 bind 目录 chown 给 65534。
   镜像的默认 `USER` 仍是 65534:65534；"非 root 且目录属主匹配"这一步的权限动作
   （`install -d -o 65534 ...`）写在 `docs/DEPLOYMENT.md` §2，需要在有 root 的机器上执行。
3. **没有跨架构验证环境**：只实测 `linux/arm64`。x86-64 是支持意图（配方里无架构假设），
   本轮不引入交叉构建体系，也**不**宣称多架构均已实测。
4. 宿主机**无 swap**（`free` 显示 Swap 0），因此 `memswap_limit == memory` 的效果在本机无法
   与"宿主有 swap"的场景对比；DESIGN §12 的 512M swap 属于宿主前置条件，未在本轮建立（无 root）。
5. 演练中的 curl 把 secret 放在 `-d` 参数里（进程 argv 短暂可见）。这是**测试工装**的形状，
   不是生产路径；生产里凭据由业务后端在服务端使用，且不进 URL。

**上线前必须人工确认**（`docs/DEPLOYMENT.md` §12 的清单，agent 不能替你完成）：
备份定时任务真的在跑且每周演练一次；pepper 与 DB 分库存放；密钥从未进 git/镜像层/`docker inspect`；
恢复后逐个确认站点再启用并用新凭据重接业务方；生产难度档不落低档（OCR 基线 96%）。

## 8. 本轮发现并修掉的真实缺陷（不是"顺手改"，是演练撞出来的）

| # | 缺陷 | 证据 | 处置 |
|---|---|---|---|
| 1 | **撤销最后一把管理凭据 = 管理面永久锁死**：`BootstrapAdminKey` 只在库里无记录时登记，之后换 `EPOCHA_ADMIN_KEY_FILE` 完全无效，而镜像里没有 `sqlite3` 可手工改库 | 门禁第 6 步在"换密钥文件后管理面仍不可用"处失败（PASS 49/50 现在测的就是这条路径本身） | 新增 `epocha-ops admin-recover`（停服前置 + 探活拒绝 + 一条事务撤销全部有效凭据并登记新凭据 + 审计；凭据只写 0600 文件，stdout 只给 key_id）；`store.RecoverAdminKey` 有测试；文档写明"撤销前先新增一把并验证"的纪律 |
| 2 | **宿主 `CGO_ENABLED=1` 产出动态链接二进制 ⇒ scratch 镜像起不来**（`exec /usr/local/bin/epocha: no such file or directory`，健康检查一路 unhealthy） | 第一次 `compose up --wait` 直接失败 | `make docker-image-local` 强制 `CGO_ENABLED=0` 并在构建期断言两把二进制**无 PT_INTERP** |
| 3 | 门禁把 `devclient --mode verify-only`（**期望失败**，rc=0 才是正确）读成"重放成功"，报了个假 FAIL | 第一轮门禁 FAIL"同一 response 第二次验证竟然成功" | 改用 `redeem-only` 拿未消费 response（`acquire` 会自己先消费一次），并显式区分两种模式的期望；新增"重放响应形状是 `timeout-or-duplicate`"断言 |
| 4 | `ops admin` 的 flag 写在位置参数后面会被 stdlib flag 停掉解析，错误信息指向"缺位置参数"这个**错误原因** | 单测失败 | `hoistFlags` 把带值 flag 前置，并测两种顺序结果一致 |
| 5 | 备份/恢复产物旁的 WAL sidecar 语义：只读打开会重新生成 `-wal/-shm` | 单测在"恢复后目录必须只有单文件"处失败（是**测试自己**的连接造成的） | 断言移到"恢复刚结束、还没有第二个连接碰它"的时刻；`BACKUP-RESTORE.md` 写明搬运时要确认 `-wal` 不存在**或为空** |
| 6 | `TestRateLimitPerSite` 在争用下把瞬时非 429 响应当成"还没触发限流"继续循环，最终以"站点维度限流未生效：200"指错原因 | 门禁第 1 步回归一次失败（单独跑 10/10 通过） | 预热循环要求每次落在 `{200,429}`，否则直接把真实状态码与响应体报出来 |
| 7 | 题池水位在只有 `pow_only` 站点时恒为 0（分桶来自启用的 `pow_question` 站点），第一轮据此"测"到的补池数字其实是没测 | 门禁 `ready 合计=0 行合计=0` | 第 10 步改为先建真题目站点再测补池 |

## 9. 命令

```bash
make stage5a-check        # 全套演练（退出码 0=PASS / 1=FAIL / 3=BLOCKED）
make docker-image-local   # 装配路径构建镜像（本轮实测过的路径）
make docker-image         # 规范路径（容器内编译；需 registry + ≥2GB）
make build-ops            # bin/epocha-ops
make loadtest ARGS="--base http://127.0.0.1:8080 --site-key pk_xxx --requests 150 --concurrency 4"
                          # 有界负载实测（需服务已在跑 + EPOCHA_SITE_SECRET）
```

复制即可执行的本地启动：`docs/DEPLOYMENT.md` §11。
备份与恢复的逐步命令：`docs/BACKUP-RESTORE.md` §2 与 §5。
