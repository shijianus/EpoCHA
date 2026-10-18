# DEPLOYMENT.md — 阶段 5A：单节点 VPS 部署

目标形状：**1 vCPU / 1GB RAM / 50GB 磁盘的一台 VPS**，单个 Go 验证服务 + SQLite，单副本，
反向代理负责 TLS，管理面不向公网发布。
本文件不含公网部署授权，也不含多节点/CDN/自动扩缩容方案。

> **一句话边界**：本文的容器化与运维验收通过 **不等于** CAPTCHA 的抗 AI/抗 OCR 验证通过。
> 题型仍是 `experimental:true` + `production_ready:false`（DESIGN §8.4、红线 14）。
> 部署做对了只证明"服务在受限机器上按预期活着、状态不丢、失败时 fail closed"。

---

## 1. 这台机器需要什么

| 项 | 要求 | 为什么 |
|---|---|---|
| 内核/OS | 任意支持 Docker Engine 的 64 位 Linux；或 systemd + 直接跑二进制 | 纯 Go 二进制，无 cgo（AGENTS 硬性约束） |
| SQLite | **不需要**安装 `sqlite3` | 备份/恢复走 `epocha-ops`（VACUUM INTO），驱动是纯 Go 的 `modernc.org/sqlite` |
| 磁盘 | `/var/lib/docker`（或数据分区）≥2GB 空闲才可在容器内编译；仅装配运行时层 ≈50MB | 见 §3 的两条构建路径 |
| swap | `[目标]` 512M + `vm.swappiness=10`（DESIGN §12） | 只用于吸收 OS 抖动；容器内**禁** swap（`memswap_limit == memory`），超额应当 OOM 而不是悄悄吃宿主 |
| 文件系统 | SQLite 必须在**本地**持久文件系统 | WAL 依赖 `fsync` 与文件锁；网络共享存储（NFS 等）不提供可靠的 POSIX 锁 |
| 副本数 | 1 | **本项目的边界，不是 SQLite 的限制**：一次性消费由原子条件更新保证（ADR-004），跨进程共享库文件时同样成立；只跑一个副本是因为数据目录上的运行锁与"单一权威"的运维形状。本轮不开放多实例 |

## 2. 一次性准备（数据目录、密钥目录、身份）

```bash
# ① 目录：数据与密钥**分开**，备份目录单独一个（DESIGN §6：pepper 与 DB 同处一库则摘要保护失效）
sudo install -d -o 65534 -g 65534 -m 700 /var/lib/epocha /var/backups/epocha
sudo install -d -o 65534 -g 65534 -m 700 /etc/epocha/keys

# ② 密钥：一次性生成，之后**不再重新生成**（重建容器不产生新密钥）
#    主密钥与 pepper 各 32 字节随机数，内容是 base64；文件权限 0600，属主 = 容器运行 uid。
#    没有内置默认密钥：缺文件就拒绝启动，这是设计而不是缺陷。
head -c32 /dev/urandom | base64 | sudo tee /etc/epocha/keys/master >/dev/null
head -c32 /dev/urandom | base64 | sudo tee /etc/epocha/keys/pepper >/dev/null
sudo chmod 600 /etc/epocha/keys/master /etc/epocha/keys/pepper

# ③（可选）管理凭据引导：只在"库里还没有任何管理凭据"时生效一次。
#    值是运维实际发出的 Bearer 串本身（不做 base64 变换）。
#    留空且库里无记录 = 管理面拒绝所有请求；缺凭据不等于放开管理功能。
printf 'ak_%s' "$(head -c32 /dev/urandom | base64 | tr -d '/+=' | head -c48)" | sudo tee /etc/epocha/keys/admin >/dev/null
sudo chmod 600 /etc/epocha/keys/admin
```

`deploy/docker/.env`（由 `.env.example` 复制）里把 `EPOCHA_UID/EPOCHA_GID` 设成上面目录的属主，
`EPOCHA_DATA_DIR`/`EPOCHA_KEY_DIR`/`EPOCHA_BACKUP_DIR` 指向上面三个目录。

**权限与"非 root 容器实际读得到什么"**：镜像的 `USER` 是数字 uid `65534:65534`（scratch 里没有
`/etc/passwd`，因此不使用用户名）。`epocha` 读密钥文件走 `os.Stat` + `os.Open`，并且要求
**解析后的目标文件**权限的 group/other 位为空（0600 或 0400），否则拒绝启动。符号链接是允许的
（编排平台的 secret 挂载本身就是指向 `..data/<uuid>` 的链接），但目标必须是普通文件。
也就是说：容器内进程能读的只有它自己 uid 能读的那几把密钥，而密钥目录还是 `:ro` 挂载——
运行时无法换掉自己的密钥。

## 3. 构建镜像：受支持的是 B 路径，A 路径保持可选

`deploy/docker/Dockerfile` 有两个阶段：`toolchain`（编译）与 `runtime`（`FROM scratch` + 两把静态二进制）。

```bash
# B. 受支持路径（第一版验收的就是这条）：宿主静态编译，toolchain 阶段用命名上下文替换成
#    deploy/docker/.prebuilt/out 里已编译好的两份二进制。运行时层与 A 完全相同。
make docker-image-local

# A. 可选路径：在容器内编译。需要 registry 可达 + ≥2GB 空闲磁盘（Go 模块解包与编译中间产物）。
#    第一版**不**把它列为受支持路径，也没在低磁盘机器上为它拉大镜像：保持 NOT_RUN（docs/STAGE5B.md §5.0）。
make docker-image
```

两条路径的差别只在"二进制由谁编译"，镜像里的文件集合、`USER`、`HEALTHCHECK`、`ENTRYPOINT` 一样；
`docs/scripts/stage5a.sh` 用 `docker save` 的文件清单证明这一点（镜像里只有
`/usr/local/bin/epocha` 与 `/usr/local/bin/epocha-ops`，没有 shell、curl、sqlite3、OCR、
评测样本、ground truth、node/firefox）。

**为什么 B 是"受支持"而不是"临时替代"**：B 的编译输入是仓库源码 + 本机 Go 工具链，构建过程可以在
无外网的机器上复现，产物 SHA-256 可与候选清单逐字节对上；A 的复现依赖 registry 可达性。
**代价**也必须写清楚：B 把"工具链一致性"交给了执行者的本机 Go（`GOTOOLCHAIN=local` 锁住版本，
但不同机器的 Go 补丁版可能不同）。所以候选清单里 `Go: go1.24.5 linux/arm64` 那一行是**复现条件**，
不是装饰。

**架构**：本轮只在 `linux/arm64` 上真实构建与运行过（宿主即 arm64）。配方本身不含架构假设
（`CGO_ENABLED=0` 的静态二进制 + scratch），x86-64 支持是**意图**，未实测；
本轮没有引入跨架构构建体系（无 buildx/QEMU 配置）。在 x86-64 上构建时请用该架构的构建机，
并按 §10 重跑一次资源演练。

**版本串会改变二进制**：`ldflags` 在构建时注入 `git describe` 与 `rev-parse` 的结果
（`epocha --version` 与备份 manifest 的 `app_version`/`commit` 就来自这里）。因此
**同一份源码在不同标签状态下构建，二进制与 SHA-256 就不同**，镜像 ID 也随之变化。
这不是"代码变了"，所以不要为了"让版本串显示新标签"而重建候选，再把那份新产物当成"已经验收过"——
候选的身份由"源码提交 + 实测记录"决定，标签名不参与。要带新标签版本串的镜像，
就在标签存在后重新构建，并**把验收一起重跑**。

**运行身份**：镜像 `USER` 是 `65534:65534`，但**实测过的组合是 `1000:1000`**（本机无 root，
数据目录属主就是执行者）。换成 65534 的那次 `chown`/`install -o 65534` 没有验证过（BLOCKED），
上线时按 §2 执行并把 §5.5 的 uid 规则复测一遍。

## 4. 启动

**整段从仓库根执行，不需要 `cd`**：compose 用 `-f` + `--project-directory` 指定文件与项目根，
绑定挂载里的 `./data` 这类相对路径按**项目目录**（也就是 compose 文件所在目录）解析。
把 `cd` 留在文档里是错的形状——它决定了同一段里 `make` 找哪个 Makefile、compose 找哪个 `.env`，
而"记得先 cd"是人的纪律，不是命令本身的可读约束。

```bash
EP=deploy/docker            # 部署根：compose 文件、.env、data/keys/backups 都在这里的相对位置
COMPOSE="docker compose -f $EP/compose.yml --project-directory $EP --env-file $EP/.env"
cp -n $EP/.env.example $EP/.env     # -n：已有 .env 不覆盖（按需改端口/目录/uid；这里不写任何密钥）
make docker-image-local             # B 路径（受支持）：宿主静态编译 + named build context 装配
$COMPOSE up -d --wait               # --wait 依镜像里的 /readyz 健康检查判定"可接流量"
$COMPOSE ps                         # STATUS 应含 (healthy)
```

停止与更新：

```bash
$COMPOSE stop                        # SIGTERM → 排空在途请求 → 关闭 DB（stop_grace_period 15s）
$COMPOSE pull && $COMPOSE up -d      # 更新镜像；数据卷与密钥文件不变
```

> `$COMPOSE` 只在**当前这个 shell** 里有效。换了终端就重新赋值那一行，或改回
> `cd deploy/docker && docker compose ...` 的形式——但一段命令里只能有一种，别混着用。

## 5. 端口与管理面边界（最容易读错的一节）

| 位置 | 地址 | 谁可达 |
|---|---|---|
| 容器内公共面 | `0.0.0.0:8080`（`EPOCHA_LISTEN`） | 容器内所有网卡；被宿主端口映射打到 |
| 容器内管理面 | `127.0.0.1:8081`（`EPOCHA_ADMIN_LISTEN`） | **只有容器内进程** |
| 宿主 | `127.0.0.1:8080`（compose 的 `ports` 显式绑宿主 loopback） | 同机反代；公网不可达 |
| 宿主 | 管理口**不存在**（compose 里没有 8081 的发布项） | 无人 |

三条必须一起读的事实：

1. **容器内的 loopback 不是宿主的 loopback。** `127.0.0.1:8081` 在容器命名空间里，
   宿主上 `curl 127.0.0.1:8081` 连的是宿主自己，不会进容器。
2. `EPOCHA_LISTEN=0.0.0.0:8080` 的"0.0.0.0"指**容器内**全部网卡。对外边界由 compose 的
   发布表决定（`127.0.0.1:8080:8080`）。省略宿主 IP 写成 `8080:8080` 会绑到宿主 0.0.0.0，
   等于把验证服务直接放到公网网卡——这一条在 `stage5a.sh` 第 5 步用 `ss -ltn` 实测钉住。
3. 管理路由在公共监听器上**不存在**（404，而不是 401）。所以"从公网带正确的 Bearer 也打不进管理面"
   不依赖凭据，而依赖路由本身。

### 管理操作走受控容器内路径

唯一支持的方式是在容器内执行 `epocha-ops admin`（打到容器内 loopback 的管理监听器）：

```bash
# 列站点
docker compose exec epocha /usr/local/bin/epocha-ops admin GET /api/admin/sites
# 建站点（请求体走管道；镜像里没有 shell 重定向的临时文件可留）
printf '{"name":"prod","verify_mode":"pow_question","generator_id":"text-image-v1","difficulty":5}' |
  docker compose exec -T epocha /usr/local/bin/epocha-ops admin POST /api/admin/sites
```

`epocha-ops admin` 自带三条约束（有测试钉住）：目标必须是 loopback、路径必须以 `/api/admin/` 开头、
方法只允许 GET/POST/PUT/DELETE。它拒绝 `http://10.0.0.5:8081` 这类地址，因为管理凭据是 Bearer 明文，
而管理监听器是明文 HTTP——放宽这条需要的是 TLS + 明确的通道设计，不是加一个 flag。

### 最后一把管理凭据被撤销 = 锁死，唯一出口是 `admin-recover`

这是一条真实存在过的死路，也是本轮演练**自己撞上**的：
`BootstrapAdminKey` 只在库里**没有任何** `admin_keys` 记录时才登记引导凭据（否则运维在管理 API 里的撤销
会被下一次重启推翻，DESIGN §6）。于是"撤销最后一把凭据"之后的状态是：库里有记录、全部无效 ⇒
管理面永远 401；而换 `EPOCHA_ADMIN_KEY_FILE` 不生效；镜像里也没有 `sqlite3` 可以手工改库。

出口只有一条，且它是**本地、离线、需停服**的操作，不是一个 HTTP 接口：

```bash
docker compose stop                      # 前置条件：服务不在跑
EPOCHA_PEPPER_FILE=/etc/epocha/keys/pepper \
  epocha-ops admin-recover \
    --db /var/lib/epocha/epocha.db \
    --key-file /etc/epocha/keys/admin.new
# 输出只给 key_id；凭据本身只写进那个 0600 文件
mv /etc/epocha/keys/admin.new /etc/epocha/keys/admin && chmod 600 /etc/epocha/keys/admin
docker compose up -d --wait
docker compose exec epocha /usr/local/bin/epocha-ops admin GET /api/admin/whoami
```

它做的事只有一件：一条事务里**撤销库里全部有效管理凭据** + 登记新凭据 + 写一条 `admin.recover_local` 审计。
不读旧凭据（旧凭据只有带 pepper 的摘要，本来也读不回明文），不做通用管理面。
凭据文件已存在 ⇒ 拒绝（`O_EXCL`，不覆盖可能正在使用的凭据）。

**"服务在跑"的判据是数据目录上的排他锁，不是端口。** `admin-recover` 与 `restore` 都必须先拿到
`<数据目录>/epocha.lock` 的 `flock` 排他锁；拿不到就拒绝执行并提示先停服。
阶段 5A 这里是探管理端口，那是错的依据：地址配错、管理面没挂、监听形状换了——任何一条都会让
"检查通过"变成"根本没检查"，然后离线改库照样执行。现在这条判定与监听器是否可达完全无关，
`--admin-base-url` 这个 flag 已经删掉（`stage5a.sh` 与测试同步改）。

运维纪律由此推出：**撤销管理凭据前先新增一把并验证可用**。`stage5a.sh` 第 6 步把这条锁死路径与出口
都跑了一遍（撤销 → 重建后仍撤销 → 换引导文件仍进不去 → admin-recover 后 whoami 给出新 key_id），
锁本身由 `cmd/server`/`cmd/ops` 的真实子进程测试钉住（`docs/STAGE5B.md`）。

**本轮不提供第二套管理通道**（不做 SSH 隧道、不发布 8081、不开任何未鉴权的恢复接口）。
如果运维确实需要远程管理入口，那是后续阶段的设计题：先写 ADR，再动代码。

## 5.5 运行互斥：谁能在什么时候动这个数据目录

机制是数据目录里一个 `flock(LOCK_EX|LOCK_NB)` 排他锁（`internal/store/lock.go`），
**不是** PID 文件。它要防的是"同一个数据集被两个写者同时打开" ⇒ 出现两份真相，
而不是防端口冲突（端口冲突由 bind 失败自然给出）。

| 动作 | 取这把排他锁？ | 冲突时的行为 |
|---|---|---|
| `epocha`（服务） | **是**：在 `store.Open`（含迁移）**之前**取得，`db.Close` **之后**才释放 | 第二个服务立刻拒绝启动，报"数据目录已被另一个进程占用"，且不创建/不改库 |
| `epocha-ops admin-recover` | **是**（按 `--db` 所在目录） | 立刻拒绝并提示先停服；不改库、不留凭据文件 |
| `epocha-ops restore` | **是**（按 `--out-dir`） | 拒绝"就地恢复到正在服务的数据目录"；恢复到隔离目录不受影响 |
| `epocha-ops backup` | **否** | 在线快照：只读连接 + `VACUUM INTO` 自带一致性语义，**不要求停服** |
| `epocha-ops verify` / `check` / `admin` | **否** | 只读快照文件或只走 HTTP |

三条容易读错的性质，写在这里而不是埋在代码注释里：

- **锁文件永远不删**。进程崩溃、被 `SIGKILL`、容器被杀之后，内核释放锁，但文件还在——这是正常状态，
  直接重新取得即可。删锁文件反而是事故来源：两个进程会锁到两个不同的 inode 上，互斥就此消失。
- **锁不是无限等待**。`LOCK_NB`：拿不到就失败退出，绝不出现"运维以为在等、其实在死等"的形状。
- **它约束的是按规矩办事的进程**。`flock` 是建议锁：一个拥有同等文件权限、绕开本仓库代码直接打开
  SQLite 的本地程序不受它阻止。这条不是客套的风险提示，而是这个机制的能力上限——
  要防同等权限下的恶意本地进程，需要的是文件属主隔离/独占挂载这类强制机制，不在本轮范围。

**容器与宿主访问同一数据目录的规则**：锁文件与主库同在数据目录里，因此只要 bind mount 指向**同一个
宿主目录**，两边看到的就是同一个 inode，`flock` 真的互通（不同挂载点、不同副本目录则本来就不共享状态，
不该互斥）。由此导出一条硬性运维规则：

> **离线运维（`admin-recover`、`restore` 到数据目录）必须以与数据目录属主相同的 uid 执行。**
> 锁文件是 `0600`：身份不对时拿到的是 `ErrLockAccess`（"锁文件不可写，uid 需与数据目录属主一致"），
> 而不是 `ErrLockHeld`（"被另一个进程占用"）。两个错误的处置方式完全不同（换身份 vs 先停服），
> 所以文案里刻意分开——把它们混成一句，运维就会去等一个永远不会到来的释放。

`--db` 为空、数据目录不存在、目录只读，同样落在 `ErrLockAccess` 这一族，不会伪装成"被人占了"。

## 6. 反向代理（TLS 归它，本轮不签发公网证书）

模板：`deploy/docker/nginx.conf.example`（主示例只维护 Nginx 一种）。

```bash
sudo cp deploy/docker/nginx.conf.example /etc/nginx/conf.d/epocha.conf
# 改三处：server_name、证书路径、（仅当确有前置 CDN 时）real_ip 段
sudo nginx -t && sudo nginx -s reload
```

模板承担的属性，以及每一条为什么这么写，都写在文件注释里。要点：

- 只回源 `127.0.0.1:8080`；`location ^~ /api/admin/` 显式 404（纵深防御，即使有人误发布 8081）。
- 动态端点（`/api/challenge`、`/api/redeem`、`/siteverify`、`/healthz`、`/readyz`）显式 `no-store`
  且 `proxy_no_cache/proxy_cache_bypass`——一次性的东西被缓存就等于把一次性语义变成可重放。
- widget 静态脚本**透传**后端自己的 `Cache-Control: public, max-age=3600`（`web/widget/widget.go:27`），
  不覆盖成 `immutable`：同一 URL 的内容在小版本里会变。
- `client_max_body_size 8k`；超时 2s 连接 / 10s 读写。
- 访问日志用自定义 `log_format`，只记 `$uri`，**不记 query、不记体**。
- `X-Forwarded-For` 由 nginx **覆写**为 `$remote_addr`（客户端自写的 XFF 丢弃）。
- `CF-Connecting-IP` 默认**不转发**；Cloudflare 信任是一段注释掉的显式配置，需要三条前提同时成立才启用。

### 6.3 受信代理网段必须测，不要凭印象写

服务端只在**连接确实来自受信网段**且 `EPOCHA_TRUST_PROXY=true` 时才采信 `X-Forwarded-For`/
`CF-Connecting-IP`（红线 9；`internal/risk/risk.go:173` 取 XFF 链上**最右**一个值）。
容器场景里有个反直觉的点：宿主上的 nginx 经发布端口进入容器时，容器看到的 TCP 源地址
**不是 127.0.0.1**，而是该 compose 网络的网关。因此：

1. `compose.yml` 把默认网络子网**钉住**（`EPOCHA_SUBNET`，默认 `172.28.0.0/24`），
   这样"受信网段"是一个可写进配置的具体值，而不是每次现猜。
2. 与之配对的配置是：
   ```
   EPOCHA_TRUST_PROXY=true
   EPOCHA_TRUSTED_PROXIES=172.28.0.0/24      # 与 EPOCHA_SUBNET 一致；绝不写 0.0.0.0/0
   ```
3. 只置 `true` 不填网段 ⇒ 服务**拒绝启动**（`risk.NewSource` 直接报错）。这是有意的 fail closed：
   "开了 trust 但没配网段"的实际含义是"无条件相信客户端自写的头"。
4. 改了子网就要同步改 `EPOCHA_TRUSTED_PROXIES`。两条不一致的表现是限流与审计里所有用户共享一个地址，
   而不是报错——所以 `stage5a.sh` 的管理面/端口边界检查同时覆盖了"配置形状"。

反代本身的路由语义（管理面 404、widget 缓存透传、动态端点 no-store、日志无 query 无凭据）在
`docs/scripts/stage5a.sh` 第 11 步用真实 nginx 容器验证过；那一轮把上游改成服务名、用自签名测试证书，
**这不等于**公网 TLS 证书签发已验收（见 §7）。

## 7. TLS 的边界（本轮做到哪、没做到哪）

- 已做：反代模板的 TLS 段配置；用**自签名测试证书**跑通 `https://` 全链路的路由与响应头。
- 未做：真实证书签发（Let's Encrypt/商业 CA）、DNS 指向、公网 443 暴露、HSTS 预加载、
  OCSP/TLSA 之类。这些需要域名与控制权，属于"公网部署授权"范围，本轮明确不执行。
- 因此文档里任何地方都不得写"公网 TLS 已验收"。上线前需要人工确认的三件事：
  证书链完整、`server_name` 与实际域名一致、80 端口的 ACME 路径没被 `location /` 的 301 吞掉。

## 8. 健康检查语义

| 端点 | 判据 | 用途 |
|---|---|---|
| `GET /healthz` | 只证明进程活着 | 存活探针（liveness）。**不要**用它做就绪判据 |
| `GET /readyz` | 存储层有界自检：连得上、`schema_version` 就位且与本程序一致、**写路径可用**（主文件可写 + 数据目录可创建临时文件） | 就绪探针与容器 `HEALTHCHECK` |

`/readyz` 的三条边界（`internal/api/handlers.go:642`、`internal/store/deploy.go:96`）：

- **不分配题目、不消费 token**：探针不得改变任何安全关键状态（有测试逐表对账行数）。
- **有界**：`store.Ready` 自带 2s 上限并沿用请求 ctx；探针慢成故障源就直接判不就绪。
- **不外泄**：只回 `{"status":"ready"|"unavailable"}`，不含路径、SQL、驱动文本、站点数、失败原因。
  原因只进日志（`errKind` 给类别，红线 8）。

题池水位低**不是**未就绪：那是容量信号（业务侧得到 503 + 不降级），把它当就绪判据会让编排
在补池期间反复重启服务。数据库不可用 ⇒ 503 `unavailable`，并且因为写路径检查的存在，
不会出现"每次验证都失败但探针说健康"。

镜像里没有 shell/curl/wget，所以 `HEALTHCHECK` 用镜像内真实存在的命令：
`epocha-ops check --path /readyz`（非 200 ⇒ 退出码 1，`stage5a.sh` 测过这个映射）。

### 8.5 健康检查与重启策略是两件事（容易读错，也是最容易做错的地方）

| 事实 | 谁负责 | 失败时的真实形状 |
|---|---|---|
| 进程退出/被 OOM kill | `restart: unless-stopped`（compose 里已有） | Docker 重启容器；`RestartCount` 增加，`docker inspect` 可查 |
| 探针返回非 200（unhealthy） | **没有人**——plain Docker/Compose 里 unhealthy 本身**不会**重启容器 | 容器保持 running、状态标 unhealthy，事件写进 `docker events`；只有外部编排（Swarm/K8s）才会因此重建容器 |
| 进程活着但写路径不可用 | `/readyz` 报 unavailable，公共端点 fail closed（503） | 业务侧看到 503；这恰恰是需要**告警**而不是自动 kill 的情形 |

所以本轮的口径是最小可操作的三条：

1. **unhealthy ⇒ 排查，不是重启**。先看 `docker inspect --format '{{json .State.Health}}'`、
   `docker logs`、宿主磁盘余量与数据目录属主（`epocha.lock` 是否可写、`ErrLockAccess` 那类），
   再决定动作。盲目 kill 会把"写路径暂时不可用"（常见原因是磁盘满或权限变了）变成"重启后仍然不可用"，
   还顺手丢掉最后一次可诊断的现场。
2. **不要用健康检查无条件 kill 服务**，也不引入 autoheal 之类的旁路重启组件：它把"探针失败"与
   "进程故障"混成同一个动作，而这两者的处置完全不同。要自动处置就用编排系统自己的探针语义，
   并在 §12 清单里写明由谁负责。
3. **就绪与存活分开看**：`/readyz` 回答"能不能接流量"（写路径可用），`/healthz` 只回答"进程在不在"。
   题池水位低不是未就绪（那是容量信号，见上），否则编排会在补池期间反复重启服务。

## 9. 备份与恢复

见 `docs/BACKUP-RESTORE.md`（主方案 VACUUM INTO、保守恢复流程、旧快照安全语义、演练步骤）。
一句必须放在部署文档里的话：**恢复一份旧快照会丢失备份之后发生的消费、撤销与停用记录，
它不天然保持全部防重放与撤销状态**；恢复流程必须先把站点全部禁用并换入新的管理凭据。

## 10. 资源限额与"该看哪个数"

compose 里的硬限额：`cpus 1.0`、`memory 128m`、`memswap_limit 128m`（禁 swap）、`pids 128`、
`GOMEMLIMIT=56MiB`、`GOMAXPROCS=1`、`read_only`、`cap_drop ALL`、`no-new-privileges`。
这些值写在文件里**不等于**内核在执行——`docs/scripts/stage5a.sh` 第 5 步用 `docker inspect`
逐项核实（`ReadonlyRootfs`、`SecurityOpt`、`CapDrop`、`Memory`、`MemorySwap`、`NanoCpus`、`PidsLimit`）。

实测数字与它们的形状（负载、库/WAL 体积、容器内存、OOM 计数）在 `docs/STAGE5A.md`。
读那些数字时必须记住两条（AGENTS/DESIGN §13）：

- PoW 由**访问者浏览器**求解。VPS 核数影响服务端吞吐，不能用来推断用户解题耗时；
  `cmd/loadtest` 代算 PoW 的部分属于"测试工具负载"，与服务端时延分开列。
- 目标机是 1 CPU。宿主有更多核不代表实测有效——演练是把容器**限制**成 1 CPU 后测的。

## 10.5 速率限制：默认值会先于服务端容量起作用

| 变量 | 默认 | 单位与含义 |
|---|---|---|
| `EPOCHA_RATE_IP_RPM` | 60 | **每分钟**、按来源地址维度的固定窗口配额（公共端点） |
| `EPOCHA_RATE_SITE_RPM` | 300 | 每分钟、站点维度（按库中站点 id，不按自报 `site_key`） |
| 站点级 `rate_ip_rpm` / `rate_site_rpm` | 随站点 | 只覆盖"地址×该站点"那一对维度，**盖不住**上面那条全局地址维度 |
| `EPOCHA_RATE_MAX_STATES` | 8192 | 限流状态条目硬上限（生存性：防随机源地址撑爆 1GB） |

三条部署时必须知道的形状：

1. 单源压测先撞的是 `EPOCHA_RATE_IP_RPM`，不是 CPU/容量。阶段 5A 实测：默认配置下 80 次闭环
   只有 18 次成功，其余是 `429`；要量服务端容量必须显式抬高这个值，且**只**在测量实例上抬，
   不写进部署配置。
2. 单位是**每分钟**（`internal/risk` 按窗口对齐分桶；ADR-016 记录了这个口径曾经错成每秒的事）。
   窗口是固定窗口，所以两个窗口交界处理论上可出现双倍突发——本版不引入滑动窗口/令牌桶，
   因为它没有对应需求，而多一个可配置项就多一处会写错的地方。
3. 反代后面所有用户会共享同一个连接地址（如果 `EPOCHA_TRUST_PROXY` 没配对好），
   于是全站一起被 `ip` 维度限流。这正是 §6.3 那条"受信网段必须实测"的实际代价。

## 11. 复制即可执行的最小启动（本机演练，非公网）

**这一整段从仓库根执行**，只赋值一次 `EP`。它是 §2/§4 的合并版，形状与 `docs/scripts/stage5b.sh`
的容器演练一致（那条路径在 `make stage5b-check` 里被真实跑过：启动 → 容器内管理命令 →
站点闭环 → 重放拒绝 → 重建容器后状态仍在 → 停止后端口释放）。

```bash
EP=deploy/docker                                  # 部署根（可以是仓库外的任何目录，见下面第 4 条注释）
set -C                                            # noclobber：这几行的 `>` 永不覆盖已存在的文件
mkdir -p "$EP/data" "$EP/keys" "$EP/backups"
chmod 700 "$EP/data" "$EP/keys" "$EP/backups"

# ① 密钥：只在**不存在时**生成。重跑这段绝不能把 master/pepper 换掉——
#    换了 master 就等于撤销所有已签发 token，换了 pepper 就等于让站点摘要与管理凭据全部失效。
for k in master pepper; do
  [[ -e "$EP/keys/$k" ]] || head -c32 /dev/urandom | base64 > "$EP/keys/$k"
  chmod 600 "$EP/keys/$k"
done
# ② 管理凭据引导文件（可选）：只在"库里还没有任何管理凭据"的那一次启动生效。
[[ -e "$EP/keys/admin" ]] || printf 'ak_%s' "$(head -c32 /dev/urandom | base64 | tr -d '/+=' | head -c48)" > "$EP/keys/admin"
chmod 600 "$EP/keys/admin"
set +C                                            # 保护只该管密钥那几行；留着会让后续 shell 里对已存在文件的 `>` 全部失败

# ③ compose 变量：uid/gid 必须等于 data 与 keys 的**属主**，否则服务在取运行锁这一步
#    直接返回 ErrLockAccess（§5.5），这是"身份不对"而不是"有人在用"。
test -f "$EP/.env" || cp deploy/docker/.env.example "$EP/.env"
sed -i "s|^EPOCHA_UID=.*|EPOCHA_UID=$(id -u)|;s|^EPOCHA_GID=.*|EPOCHA_GID=$(id -g)|" "$EP/.env"
sed -i "s|^# EPOCHA_ADMIN_KEY_FILE=.*|EPOCHA_ADMIN_KEY_FILE=/etc/epocha/keys/admin|" "$EP/.env"
chmod 600 "$EP/.env"

# ④ 构建并启动。`make` 走仓库根的 Makefile（B 路径）；compose 走 $EP 里的文件与 .env。
make docker-image-local
COMPOSE="docker compose -f $EP/compose.yml --project-directory $EP --env-file $EP/.env"
$COMPOSE up -d --wait
curl -sS http://127.0.0.1:8080/readyz            # ready
$COMPOSE exec epocha /usr/local/bin/epocha-ops admin GET /api/admin/whoami   # 管理面只在容器内可达
```

> 同一台机器上要并存**多套**部署时，给每套加一个不同的 `-p <项目名>`，并在 `.env` 里给不同的
> `EPOCHA_NETWORK`/`EPOCHA_SUBNET`/`EPOCHA_PUBLIC_PORT`。`stage5b.sh` 第 2.5 步就是这么并起两套
> （两套各有自己的数据目录 ⇒ 各自持一把锁，互不冲突），并验证"定点清理只带走自己那套"。

读法与边界：

- **端口**：`.env` 里 `EPOCHA_PUBLIC_BIND=127.0.0.1` 不能省成 `8080:8080`（那会绑 0.0.0.0）；
  管理端口 8081 在 compose 里**没有**发布项，`ss -ltn` 在宿主上查不到它。
- **`EP` 在仓库外时**：`make docker-image-local` 仍然在仓库根跑（它编译源码并装配镜像），
  只是 compose 与数据/密钥目录落在别处——这时把上面第 ③ 步的 `cp` 换成从仓库里拷
  `deploy/docker/{compose.yml,.env.example}` 到 `$EP`。
- **`set -C` 的作用域**：它让这一段里的 `>` 对已存在文件失败退出，正是"重跑不毁密钥"的机制。
  它不影响 `cp -n`／`sed -i`／后续命令。
- 密钥文件权限必须 0600/0400 且属主 = 容器 uid；`epocha --version` 与备份 manifest 里的
  `app_version`/`commit` 应当对得上 `git describe`（§3 的 B 路径注入的就是这两个值）。
- 这一段**不**做：反代与 TLS（§6/§7）、备份定时任务（§9）、题型难度选择（红线 14：
  低档挡不住通用 OCR）、以及任何"把测试高限流写进部署配置"的事（测量用的大 RPM 只在 `stage5b.sh` 里出现）。

## 12. 上线前的人工确认清单（agent 无法替你完成）

- [ ] 密钥文件与 DB 在**不同的备份策略**下（DESIGN §6：两者同进一次备份 = 摘要保护归零）。
- [ ] `EPOCHA_MASTER_KEY`/`PEPPER` 从未进过 git 历史、镜像层、`docker inspect`、CI 日志。
- [ ] 站点难度档不由低档承担"挡 OCR"的职责：阶段 4B 的真实基线是低档 96%/中档 31%/高档 3%
      （tesseract 5.3.4，holdout 各 100 张），生产默认不得落低档（红线 14）。
- [ ] 备份**定时任务真的在跑**，且每周做一次真实恢复演练（不演练的备份等于没有备份）。
- [ ] 恢复演练后的站点逐个确认再启用；管理凭据用恢复时新装入的那把。
- [ ] 反代日志的磁盘占用与轮转确认（`logrotate` 或服务自己的 json-file 上限）。
- [ ] `make stage5a-check` 在本机退出码 0；若非 0，逐条读 RESULT 行而不是把文件写完当成验收通过。
- [ ] 在**目标机**上跑一次 `bash deploy/preflight.sh --text`（只读），把它报的 warn 逐条处置完再部署：
      密钥权限位、数据目录属主与 uid、端口占用、可用内存/swap、docker 组可达性。
      开发机的预检结果不算数，报告里的 warn 也不等于失败——它是给人判断的输入。
