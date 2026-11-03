# TRIAL-DEPLOYMENT — 试部署计划（待确认，不是授权）

状态：**等待用户提供目标 VPS 信息与部署授权**。本文件是"缺哪些信息"的清单，不是执行许可。
开发机上的任何预检数字都**不**代表 VPS；Docker 可达、端口空闲、目录可写也都**不**等于获准部署。
ADR-021 的可靠性冻结不取消原产品需求；风险评分、权重/选择、统计淘汰、非视觉通道、缺失管理页面、
`fallback_base` 与 Cloudflare 的状态继续保留在 [`部署审计` §6](reviews/DEPLOYMENT-READINESS-AUDIT-2026-11-02.md)。
本轮没有执行目标机预检或真实部署，Linux 完整目录接管和 Nginx 全日志载体哨兵检查 **NOT_RUN**。

---

## 0. 候选必须绑定已验收的完整 OID 与 manifest

**本轮尚未生成可用的正式发布候选 OID，也没有可替代它的 freeze OID。** 当前修复可以先交付带基线 OID、dirty 状态和文件/二进制哈希的冻结收据；那不是已验收的部署候选，不提供公网部署授权。

| 项 | 要求或历史事实 |
|---|---|
| 正式范围 | **B 路径 / Linux arm64 / UID:GID `1000:1000`**，Go 1.27.3、`GOTOOLCHAIN=local`；A 路径可选 NOT_RUN，amd64 仅工程测试 |
| 候选身份 | 获准的 **40 位完整源码 OID** + 与该提交、两份二进制 SHA-256、工具链、构建输入和当次验收结果相匹配的 manifest |
| 历史阶段标签 | `stage-5b-done` 指向 `993a7926a71d16e5fdc46a9dc7d4e584780d42b9`，缺少 ADR-018 恢复修复，**禁止作为本轮部署候选**；不移动历史标签 |
| 历史修复候选 | `be95275` 的记录为必需项 36 PASS / 0 FAIL / **1 BLOCKED**，没有完成当时持续演练，不能作为本轮已验收候选 |
| 本轮验收收据 | `node docs/scripts/release-check.mjs --profile full` 产生新的 `release-artifacts/full-*/summary.json`；BLOCKED/NOT_RUN 与原脚本 exit 3 保留 |
| 未覆盖 | 公网 TLS/DNS、人工可读性、辅助技术、真机移动端、抗现代 AI；`production_ready:false` 不变 |

目标 VPS 若是 x86-64，不能直接使用 arm64 二进制；在该架构通过工程测试不自动扩展正式支持。需要先决定并实际完成目标范围的验收，再选择产物。

审批材料必须给出 `APPROVED_OID` 和 `APPROVED_MANIFEST` 的实际值以及 manifest 的可信来源。散列用于完整性与追踪，未签名的 manifest 本身不证明来源真实。两项缺失、OID 不一致、工作树不干净或任一必需项未完成，都应停止试部署。

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
| 9 | Cloudflare 是否在前面 | Worker/DO 未实现，阶段 5 之后评估；前置 CDN 不等于边缘签发 | ⬜ 若使用 CDN：确认源站防火墙、当日官方网段、真实连接来源与 `CF-Connecting-IP`；当前 widget 没有 `fallback_base` 自动切源能力 |
| 10 | 代理信任来源（精确网段） | 无默认值，**必须实测** | ⬜ 当前支持同机反代经发布端口，容器所见连接来源是 Compose 网关，需实测后配置。不同机反代和 Unix socket 不属于现有配方，不能当成已提供的可选入口；禁止照抄样例网段（DEPLOYMENT §6.3） |
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
**报告里的 warn 不改变退出码**——0 只表示报告已生成，不表示可部署。`ss` / `netstat` / `/proc/net/tcp*`
探测失败或不可用时端口为 `unknown` 并告警；只有成功探测的空结果才能报告 `free`。
目录不可写告警必须同时保留在目录字段、`warnings`、文本列表与最终 warn 计数中。

本地最小回归入口为 `node --test --test-reporter=tap docs/scripts/preflight_regression.test.mjs`：
用 stub 运行真实 Bash 脚本，阻止 Docker/反代命令执行，覆盖探测失败/空闲/占用与目录告警丢失。
这是工程回归，不是开发机或 VPS 的部署事实，更不会发起远端连接。

预检之后要人决定的三件高频卡点：
1. 密钥文件 group/other 位不为空 ⇒ 服务**拒绝启动**（不是降级）；
2. 数据目录属主与执行 uid 不一致 ⇒ 服务或 `epocha-ops` 报 `ErrLockAccess`；
3. 宿主 8081 有人在听 ⇒ 管理面本不该有任何监听，先查清来源。

---

## 3. 站点与 Origin：三种访问方式是三个不同的 Origin

**不得混为一谈**，否则"验证通过"测的是别的东西：

| 访问方式 | 浏览器看到的 Origin | 站点 origins 该怎么配 |
|---|---|---|
| 直接在运行示例业务的主机打开 `http://127.0.0.1:8091` | `http://127.0.0.1:8091` | 明确允许 `http://127.0.0.1:8091`（`cmd/exampleapp` 默认端口；验证 API 另在 8080） |
| 获授权后把示例业务 8091 转到本机 8091，再访问 `http://localhost:8091` | `http://localhost:8091` | 允许 `http://localhost:8091` —— 与上一行不是同一个 Origin |
| 公网业务域 `https://site.example` 经反代 | `https://site.example` | 生产站点只允许这个；换域名要改来源策略 |

这里的 Origin 是**承载 widget 的业务页面**，不是 API 的基址。生产镜像不含 `cmd/exampleapp`，
验证服务的公共 8080 也没有示例业务页面；开发演示应用必须另获授权、独立启动。
若演示通过 SSH 查看，还需转发浏览器可达的 API 地址，并将 widget 的 `apiBase` 配成该地址；
只转发页面不会让浏览器访问到 VPS 的 loopback。管理 8081 始终不转发、不发布。
本轮未开隧道，也不为了“先看看”改成 0.0.0.0。

公开 challenge/redeem JSON/CORS 路径仍拒绝 `Origin:null`；示例业务的原生表单可回落
`Sec-Fetch-Site` 是**另一个端点的策略**，不能用来绕过验证服务的精确 Origin 白名单。

测试站点的形状（默认值都取保守方向）：
- `verify_mode`：先用 `pow_only` 打通闭环，再单独建一个 `pow_question` 站点测题型；
- 题型限制：`text-image-v1` 仍是 `experimental:true` + `production_ready:false`，
  低档在 4B 的真实 OCR 基线里整串通过率 **96%** ⇒ **生产默认不得落低档**（红线 14）。
  `arithmetic-baseline-v1` 的答案可由公开 payload 程序化推出，只能当流程基线；
- 站点状态：`store.CreateSite` 当前默认**启用**；如需先审核，创建后立即用管理 API 显式 disable，确认策略与 Origin 后再 enable。恢复演练产物仍是全部站点禁用；不能把恢复默认套到新建站点。
- `allow_missing_origin`：只在明确的测试站点上开，生产站点不开。

---

## 4. 部署顺序（确认 §1 全部有答案之后才执行）

```bash
# 仅在目标机与部署授权均已确认后，在干净 checkout 内执行；变量来自已批准的实际材料。
set -euo pipefail
: "${APPROVED_OID:?缺少已验收候选的完整源码 OID}"
: "${APPROVED_MANIFEST:?缺少已验收候选 manifest 的绝对路径}"
[[ "$APPROVED_OID" =~ ^[0-9a-f]{40}$ ]]
[[ "$APPROVED_OID" != 993a7926a71d16e5fdc46a9dc7d4e584780d42b9 ]]
test -s "$APPROVED_MANIFEST"
test -z "$(git status --porcelain)"
test "$(git rev-parse "${APPROVED_OID}^{commit}")" = "$APPROVED_OID"
git checkout --detach "$APPROVED_OID"
test "$(git rev-parse HEAD)" = "$APPROVED_OID"
# 逐项核对 manifest：source OID、干净状态、Go版本、架构、两份二进制SHA-256、验收结果和未测项。
# ① 按 DEPLOYMENT §2，以实际服务 UID/GID 生成密钥，已存在文件不覆盖。
# ② 使用 manifest 对应的已验收二进制/镜像。若现场 make docker-image-local 重建，须重新生成清单并验收。
# ③ 按 DEPLOYMENT §11 起服务，默认只绑 127.0.0.1。
# ④ /readyz → 容器内 ops admin whoami → 测试站点闭环 → 同 token 重放必须被拒。
# ⑤ 在线备份 + 隔离目录恢复演练；接管按 BACKUP-RESTORE §5 切完整目录，保留旧 WAL/SHM 现场。
#    新 master/no previous、新 admin 文件落位与 whoami、新 secret + 启用后的闭环均须验证。
# ⑥ 仅按已批准的反代/TLS 配置变更执行 nginx -t 与 reload；按 DEPLOYMENT §6.2 验证 80/443 全日志载体。
```

这里故意不填造一个新 OID，也不把手工设置这两个变量解释成验收通过；批准的 manifest 与当次日志必须先真实存在。`release-check` 不接受 override 赋予 PASS。

停止 / 备份 / 恢复的口径：

| 动作 | 命令 | 注意点 |
|---|---|---|
| 停止 | `$COMPOSE stop` | SIGTERM → 排空在途 → 关库 → **释放运行锁**；`stop_grace_period: 15s` |
| 备份 | `$COMPOSE exec epocha /usr/local/bin/epocha-ops backup --db /var/lib/epocha/epocha.db --dest /var/backups/epocha` | **不停服**（在线一致性快照允许并发）；产物 `.db` + `.manifest.json`，0600 |
| 校验 | `epocha-ops verify --db <快照>.db --manifest <快照>.manifest.json` | 非 0 就停下，不要往下恢复 |
| 恢复 | `epocha-ops restore --backup … --manifest … --out-dir <隔离目录>` | 对目标目录取锁；备份可在线做，但**接管前必须停旧服务并解除旧启动入口**。切整个新目录及新密钥挂载，不复制主库到旧 WAL 目录；错误身份/锁占用即停止，详见 BACKUP-RESTORE §5 |
| 管理锁死 | `epocha-ops admin-recover --db … --key-file …` | 需要停服（锁）+ pepper。它**只管管理凭据**：丢了 master 是旧 token 全部失效，丢了 pepper 是站点/管理摘要全部失效，这两样它都不负责（BACKUP-RESTORE §5.5） |
| 回滚 | 镜像是 `epocha:5a` 单标签 | 数据目录被新版本写过就不保证可回退（schema 只能前滚）；真要退，先备份再按恢复流程走 |

## 5. 完成后要记的事实（不要只写"部署成功"）

候选 commit 与两个二进制 SHA-256、本地镜像 ID（**不是** registry digest）、schema 版本、
实际运行 UID/GID、`/readyz` 首次成功时间、第一次闭环与第一次重放拒绝的证据、
第一次备份的 manifest 摘要、预检 JSON 的存放位置，以及**本轮没测过什么**。
