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
| 副本数 | 1 | 权威状态是本地单文件库；副本 >1 直接破坏一次性消费语义（ADR-004） |

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

## 3. 构建镜像：两条路径，同一个运行时层

`deploy/docker/Dockerfile` 有两个阶段：`toolchain`（编译）与 `runtime`（`FROM scratch` + 两把静态二进制）。

```bash
# A. 规范路径：在容器内编译。需要 registry 可达 + ≥2GB 空闲磁盘（Go 模块解包与编译中间产物）。
make docker-image

# B. 装配路径：toolchain 阶段用命名上下文替换成宿主已编译产物（`make build` + `make build-ops`）。
#    磁盘不足或构建机无法访问 module proxy 时用这条；**运行时层与 A 完全相同**。
make docker-image-local
```

两条路径的差别只在"二进制由谁编译"，镜像里的文件集合、`USER`、`HEALTHCHECK`、`ENTRYPOINT` 一样；
`docs/scripts/stage5a.sh` 用 `docker save` 的文件清单证明这一点（镜像里只有
`/usr/local/bin/epocha` 与 `/usr/local/bin/epocha-ops`，没有 shell、curl、sqlite3、OCR、
评测样本、ground truth、node/firefox）。

**架构**：本轮只在 `linux/arm64` 上真实构建与运行过（宿主即 arm64）。配方本身不含架构假设
（`CGO_ENABLED=0` 的静态二进制 + scratch），x86-64 支持是**意图**，未实测；
本轮没有引入跨架构构建体系（无 buildx/QEMU 配置）。在 x86-64 上构建时请用该架构的构建机，
并按 §10 重跑一次资源演练。

## 4. 启动

```bash
cd deploy/docker
cp .env.example .env        # 按需改端口/目录/uid；这里不写任何密钥
docker compose up -d --wait # --wait 依镜像里的 /readyz 健康检查判定"可接流量"
docker compose ps           # STATUS 应含 (healthy)
```

停止与更新：

```bash
docker compose stop          # SIGTERM → 排空在途请求 → 关闭 DB（stop_grace_period 15s）
docker compose pull && docker compose up -d   # 更新镜像；数据卷与密钥文件不变
```

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
docker compose stop                      # 前置条件：服务不在跑（命令会自己探活，能探到就拒绝）
EPOCHA_PEPPER_FILE=/etc/epocha/keys/pepper \
  epocha-ops admin-recover \
    --db /var/lib/epocha/epocha.db \
    --key-file /etc/epocha/keys/admin.new \
    --admin-base-url http://127.0.0.1:8081
# 输出只给 key_id；凭据本身只写进那个 0600 文件
mv /etc/epocha/keys/admin.new /etc/epocha/keys/admin && chmod 600 /etc/epocha/keys/admin
docker compose up -d --wait
docker compose exec epocha /usr/local/bin/epocha-ops admin GET /api/admin/whoami
```

它做的事只有一件：一条事务里**撤销库里全部有效管理凭据** + 登记新凭据 + 写一条 `admin.recover_local` 审计。
不读旧凭据（旧凭据只有带 pepper 的摘要，本来也读不回明文），不做通用管理面。
凭据文件已存在 ⇒ 拒绝（`O_EXCL`，不覆盖可能正在使用的凭据）。

运维纪律由此推出：**撤销管理凭据前先新增一把并验证可用**。`stage5a.sh` 第 6 步把这条锁死路径和它的出口
都真的跑了一遍（撤销 → 重建后仍撤销 → 换引导文件仍进不去 → admin-recover 后 whoami 给出新 key_id）。

**本轮不提供第二套管理通道**（不做 SSH 隧道、不发布 8081、不开任何未鉴权的恢复接口）。
如果运维确实需要远程管理入口，那是阶段 5B 的设计题：先写 ADR，再动代码。

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

## 11. 复制即可执行的最小启动（本机演练，非公网）

```bash
cd deploy/docker
mkdir -p data keys backups && chmod 700 data keys backups
head -c32 /dev/urandom | base64 > keys/master; head -c32 /dev/urandom | base64 > keys/pepper
chmod 600 keys/master keys/pepper
cp .env.example .env
# 没有 root 的机器上把 uid 设成当前用户（镜像默认 65534 需要目录属主匹配）
sed -i "s/^EPOCHA_UID=.*/EPOCHA_UID=$(id -u)/;s/^EPOCHA_GID=.*/EPOCHA_GID=$(id -g)/" .env
make docker-image-local          # 在仓库根执行
(docker compose up -d --wait)    # 在 deploy/docker 执行
curl -s http://127.0.0.1:8080/readyz
```

## 12. 上线前的人工确认清单（agent 无法替你完成）

- [ ] 密钥文件与 DB 在**不同的备份策略**下（DESIGN §6：两者同进一次备份 = 摘要保护归零）。
- [ ] `EPOCHA_MASTER_KEY`/`PEPPER` 从未进过 git 历史、镜像层、`docker inspect`、CI 日志。
- [ ] 站点难度档不由低档承担"挡 OCR"的职责：阶段 4B 的真实基线是低档 96%/中档 31%/高档 3%
      （tesseract 5.3.4，holdout 各 100 张），生产默认不得落低档（红线 14）。
- [ ] 备份**定时任务真的在跑**，且每周做一次真实恢复演练（不演练的备份等于没有备份）。
- [ ] 恢复演练后的站点逐个确认再启用；管理凭据用恢复时新装入的那把。
- [ ] 反代日志的磁盘占用与轮转确认（`logrotate` 或服务自己的 json-file 上限）。
- [ ] `make stage5a-check` 在本机退出码 0；若非 0，逐条读 RESULT 行而不是把文件写完当成验收通过。
