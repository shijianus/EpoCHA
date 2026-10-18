# TRIAL-DEPLOYMENT — 试部署计划（待确认，不是授权）

状态：**等待用户提供目标 VPS 信息与部署授权**。本文件是"缺哪些信息"的清单，不是执行许可。
开发机上的任何预检数字都**不**代表 VPS；Docker 可达、端口空闲、目录可写也都**不**等于获准部署。

---

## 0. 用哪个候选，以及它测过什么

| 项 | 值 |
|---|---|
| 交付标签 | `stage-5b-done` → 提交 `993a792`（标签正文限定：B 路径、`linux/arm64`、实测 uid `1000:1000`） |
| 构建路径 | **只有 B 路径受支持**：宿主 `CGO_ENABLED=0` 静态编译 → `deploy/docker/.prebuilt/out` → named build context 装配。A 路径（容器内编译）NOT_RUN |
| 候选清单 | `deploy/docker/.prebuilt/candidate-manifest.txt`（忽略目录里的构建产物，绑定干净提交；不入库以免自引用） |
| 未覆盖 | amd64、UID 65534 的 `chown` 路径、公网 TLS/DNS、人工可读性、抗 OCR/抗 AI |

**待确认第 0 项就是架构**：如果 VPS 是 x86-64，现有候选（arm64 二进制 + `scratch`）**不能直接用**，
必须在目标机上按 B 路径重建，并重跑 `make stage5b-check`——那不是"换个标签"，是重新验收。

---

## 1. 需要用户提供 / 确认的信息

| # | 字段 | 已知默认 | 待确认 |
|---|---|---|---|
| 1 | 目标机架构、内核、发行版 | 预检脚本会给 | ⬜（决定候选是否可用） |
| 2 | CPU / 可用内存 / swap | 目标形状 1 vCPU / 1GB / 512M swap | ⬜ |
| 3 | 数据目录、密钥目录、备份目录 | `/var/lib/epocha`、`/etc/epocha/keys`、`/var/backups/epocha` | ⬜ 路径与所在分区余量 |
| 4 | 运行 UID/GID | 镜像默认 `65534:65534`；**实测过的是 `1000:1000`** | ⬜ 有没有 root？用哪个身份跑 compose？离线运维与容器是否同 uid（§5.5 的锁权限规则依此不同） |
| 5 | 公共端口与绑定 | `127.0.0.1:8080`（**只绑宿主 loopback**） | ⬜ 宿主上 8080 是否空闲；反代回源端口 |
| 6 | 管理面边界 | 容器内 `127.0.0.1:8081`，**从不发布** | ⬜ 确认不需要任何对外管理入口 |
| 7 | 反代 | Nginx 模板 `deploy/docker/nginx.conf.example`；compose 用发布端口回源 | ⬜ Nginx 还是 Caddy；是否已有站点/证书；反代与容器同机还是不同机 |
| 8 | 域名与 TLS | 本轮不签发、不验证 | ⬜ 域名、证书来源（LE / 自签 / 云 LB）、是否强制 HTTPS |
| 9 | Cloudflare 是否在前面 | 不在（阶段 5 之后才评估边缘） | ⬜ 若在：真实客户端地址来自 `CF-Connecting-IP`，`EPOCHA_TRUST_PROXY=true` + 精确网段；还要确认**业务后端拿到的验证请求**从哪来 |
| 10 | 代理信任来源（精确网段） | 无默认值，**必须实测** | ⬜ 三种形状给出三个不同答案：①同机反代经发布端口 → 源地址是 compose 网关（模板里钉的 `172.28.0.0/24` 只是样例）；②反代与容器**不同机** → 是那个内网地址；③宿主 nginx 直连容器 unix socket → 本机。**照抄样例网段等于"开着 trust-proxy 但永远不采信"**（DEPLOYMENT §6.3） |
| 11 | compose 子网 | `172.28.0.0/24` | ⬜ 与宿主其它网段是否冲突 |
| 12 | 站点与 Origin | 无默认站点 | ⬜ 站点名、允许 Origin 列表、`verify_mode`、难度档（见 §3） |
| 13 | 密钥托管 | 只支持 `EPOCHA_*_FILE` | ⬜ 谁生成 master/pepper、放哪、是否与库分属**不同**备份策略、丢了谁负责 |
| 14 | 备份节律 | 手动 `epocha-ops backup` | ⬜ cron/systemd timer 的频率、保留份数、异机副本目的地、每周恢复演练的时间 |
| 15 | Docker 安装与镜像分发 | 不推 registry、不做归档 | ⬜ VPS 上装 Docker 的方式；无 registry 时是否在目标机 `make docker-image-local`（需要该机有 Go 与模块缓存）还是 `docker save` 8.7MB 传输后 `load` |
| 16 | 磁盘告警阈值 | 未设 | ⬜ 备份 + WAL + 日志轮转（3×5MB）之外要留多少 |

---

## 2. 预检（在 VPS 上手动执行，只读）

```bash
# 把 deploy/preflight.sh 拷到目标机（git clone / scp 都行），然后：
bash preflight.sh --json \
  --data-dir /var/lib/epocha --keys-dir /etc/epocha/keys --backup-dir /var/backups/epocha \
  --ports 80,443,8080,8081 \
  --out /tmp/epocha-preflight.json

# 给人看的版本：
bash preflight.sh --text
```

脚本不安装、不提权、不拉镜像、不构建、不创建目录/用户、不改防火墙/DNS/证书、不终止占端口的进程，
也不读取或打印任何凭据内容（密钥目录只取 stat 的属主与权限位）。
它唯一可能的写操作是 `--out` 那一个文件，且已存在就拒绝覆盖。
**报告里的 warn 不改变退出码**——判断是人做的，脚本只把事实摆出来。

预检之后要人决定的三件高频卡点：
1. 密钥文件 group/other 位不为空 ⇒ 服务**拒绝启动**（不是降级）；
2. 数据目录属主与执行 uid 不一致 ⇒ 服务或 `epocha-ops` 报 `ErrLockAccess`；
3. 宿主 8081 有人在听 ⇒ 管理面本不该有任何监听，先查清来源。

---

## 3. 站点与 Origin：三种访问方式是三个不同的 Origin

**不得混为一谈**，否则"验证通过"测的是别的东西：

| 访问方式 | 浏览器看到的 Origin | 站点 origins 该怎么配 |
|---|---|---|
| 直接在宿主 `http://127.0.0.1:8080` 打开示例页 | `http://127.0.0.1:8080` | 明确允许 `http://127.0.0.1:8080`（只在这台机器上有意义） |
| SSH 本地转发 `ssh -L 8080:127.0.0.1:8080 vps` 后访问 `http://localhost:8080` | `http://localhost:8080` | 允许 `http://localhost:8080` —— **注意它与上一行不是同一个 Origin** |
| 公网业务域 `https://site.example` 经反代 | `https://site.example` | 生产站点只允许这个；换域名要改来源策略 |

远程看效果的推荐做法（**本轮不擅自开隧道、也不为了"先看看"绑 0.0.0.0**）：

```bash
ssh -N -L 8080:127.0.0.1:8080 <user>@<vps>     # 由你执行；我只在这条隧道存在时才有 127.0.0.1 的视野
```

测试站点的形状（默认值都取保守方向）：
- `verify_mode`：先用 `pow_only` 打通闭环，再单独建一个 `pow_question` 站点测题型；
- 题型限制：`text-image-v1` 仍是 `experimental:true` + `production_ready:false`，
  低档在 4B 的真实 OCR 基线里整串通过率 **96%** ⇒ **生产默认不得落低档**（红线 14）。
  `arithmetic-baseline-v1` 的答案可由公开 payload 程序化推出，只能当流程基线；
- 站点状态：新建站点默认**禁用**，人工逐个确认后再 enable；恢复演练后的站点同样全部禁用；
- `allow_missing_origin`：只在明确的测试站点上开，生产站点不开。

---

## 4. 部署顺序（确认 §1 全部有答案之后才执行）

```bash
# ① 建目录与密钥（运维手工，DEPLOYMENT §2）；有 root 时按 65534，无 root 时 .env 里改 EPOCHA_UID/GID
# ② 目标机上构建（B 路径）
git checkout stage-5b-done && make docker-image-local
# ③ 起服务（整段从仓库根执行，见 DEPLOYMENT §11；默认只绑 127.0.0.1）
# ④ 冒烟：/readyz → 容器内 ops admin whoami → 建测试站点 → 一次真实闭环 → 同 token 重放必须被拒
# ⑤ 在线备份 + 隔离目录恢复演练（不许在工作库上就地恢复）
# ⑥ 反代配置接入：先只加 location/证书，不动服务；改完 nginx -t && reload
```

停止 / 备份 / 恢复的口径：

| 动作 | 命令 | 注意点 |
|---|---|---|
| 停止 | `$COMPOSE stop` | SIGTERM → 排空在途 → 关库 → **释放运行锁**；`stop_grace_period: 15s` |
| 备份 | `$COMPOSE exec epocha /usr/local/bin/epocha-ops backup --db /var/lib/epocha/epocha.db --dest /var/backups/epocha` | **不停服**（在线一致性快照允许并发）；产物 `.db` + `.manifest.json`，0600 |
| 校验 | `epocha-ops verify --db <快照>.db --manifest <快照>.manifest.json` | 非 0 就停下，不要往下恢复 |
| 恢复 | `epocha-ops restore --backup … --manifest … --out-dir <隔离目录>` | 只有写数据目录的动作需要停服；`--out-dir` 指向正在服务的数据目录 ⇒ **拒绝** |
| 管理锁死 | `epocha-ops admin-recover --db … --key-file …` | 需要停服（锁）+ pepper。它**只管管理凭据**：丢了 master 是旧 token 全部失效，丢了 pepper 是站点/管理摘要全部失效，这两样它都不负责（BACKUP-RESTORE §5.5） |
| 回滚 | 镜像是 `epocha:5a` 单标签 | 数据目录被新版本写过就不保证可回退（schema 只能前滚）；真要退，先备份再按恢复流程走 |

## 5. 完成后要记的事实（不要只写"部署成功"）

候选 commit 与两个二进制 SHA-256、本地镜像 ID（**不是** registry digest）、schema 版本、
实际运行 UID/GID、`/readyz` 首次成功时间、第一次闭环与第一次重放拒绝的证据、
第一次备份的 manifest 摘要、预检 JSON 的存放位置，以及**本轮没测过什么**。
