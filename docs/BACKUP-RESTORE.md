# BACKUP-RESTORE.md — 阶段 5A：SQLite 备份与保守恢复

适用范围：单节点、本地持久文件系统上的 SQLite（`EPOCHA_DB`），WAL 模式（`internal/store/store.go:81`
的 DSN 钉住 `journal_mode(WAL)`）。

实现位置：`internal/store/deploy.go`（能力）+ `cmd/ops`（人敲的命令）+ `docs/scripts/stage5a.sh`（演练）。
**没有**引入 `sqlite3` CLI、也没有引入任何外部二进制（AGENTS 硬性约束：备份工具属于交付面的一部分）。

---

## 1. 主方案：`VACUUM INTO` 的一致性快照

为什么不选另两种：

| 方案 | 结论 | 理由 |
|---|---|---|
| 直接 `cp` 主 `.db` | **禁止** | 库在 WAL 模式，最新那段状态在 `-wal` 里。DESIGN §13 记着实测形状："写入进行中主文件 4,096B + WAL 2,855,192B"。`cp` 主文件得到的是一个**存在但落后**的库，而且看起来成功 |
| `sqlite3 .backup`（DESIGN §12 原文） | **不改代码，改文档** | 它是外部二进制，与本仓库"服务与运维能力都用纯 Go"的约束冲突。ADR-014 记录了这条更正 |
| SQLite backup API（在线） | 本轮不选 | 驱动层是否暴露该能力未做逐调用验证；本轮要的是**可证明**的方案，不是"看起来更专业"的方案 |

选择 `VACUUM INTO ?`：SQLite 官方的整库一致性快照，把源库（含 WAL 内容）重写成**单个**目标文件，
产物不带 `-wal`，目标路径走绑定参数（不拼字符串）。这四条事实不是从文档推断来的，
由 `internal/store/capability_test.go` 对 `modernc.org/sqlite v1.46.1` 的真实行为钉住：

1. 以 `mode=ro` 打开源库即可执行 `VACUUM INTO` ⇒ 备份动作**不写源文件**；
2. 目标名可以走绑定参数；
3. **未 checkpoint 的 WAL 内容会进快照**（这正是不能 `cp` 主文件的理由，也是本方案的价值）；
4. 目标目录不可用时直接报错，且不在别处留下同名半成品。

任何一条哪天不再成立，备份就必须换成"停服冷备份"，而不是继续假装 `cp` 主文件安全。

## 2. 命令

```bash
# 备份（容器内；dest 是独立的备份挂载点 /var/backups/epocha）
docker compose exec epocha /usr/local/bin/epocha-ops backup \
    --db /var/lib/epocha/epocha.db --dest /var/backups/epocha

# 独立校验一份备份是否可信（哈希 + 体积 + integrity_check + schema 版本）
docker compose exec epocha /usr/local/bin/epocha-ops verify \
    --db /var/backups/epocha/epocha-<stamp>.db \
    --manifest /var/backups/epocha/epocha-<stamp>.manifest.json
```

pepper 与 server 走**同一套**入口规则（`EPOCHA_PEPPER` 或 `EPOCHA_PEPPER_FILE`，两者同时设置即拒绝）：
备份工具与服务的密钥解析必须同源，否则会出现"server 能读、ops 读不到"的误诊，
而那种误诊会被读成"备份坏了"。

### 产物形状与"半成品不算成功"

一次成功的备份留下**恰好两个文件**：

```
epocha-20261018T091500Z-56d3911.db            # 0600，单文件库
epocha-20261018T091500Z-56d3911.manifest.json  # 0600，自描述记录
```

保证机制（`store.Backup`）：

- 快照先写成 `.<name>.db.partial`，通过 `integrity_check` 与内容计数后才 `rename` 到位；
  manifest 同样先 partial 再 rename。**任何一步失败都清掉自己刚创建的临时文件**。
  于是"目录里有一个 `.db`"这件事本身就等于"它是一次成功的备份"。
- 只读打开做校验会在 WAL 头库旁留下 `-wal/-shm`；算哈希与改名**之前**必须收掉它们
  （`dropSidecars`），否则 manifest 里的 `sha256` 记录的是一份"半边状态"的库。
- **非空的 `-wal` 一律不删并直接报错**：那说明这个 `.db` 不是我们交付的完整快照。
- 目标名已存在 ⇒ 直接拒绝，不覆盖、不加后缀。"覆盖上一份"必须是显式动作。
- 版本闸门：库的 `schema_version` 高于本程序支持值 ⇒ 拒绝备份（不把"恢复回去就崩"的状态存进归档）。
- 备份目录**必须**与数据目录分开挂载（`compose.yml` 里是三个独立挂载点）。

### manifest 里有什么

`format` / `name` / `db_file` / `created_at` / `app_version` / `commit` / `schema_version` /
`sha256` / `pepper_fingerprint` / `backup_bytes` / `source_main_bytes` / `source_wal_bytes` /
`integrity_check` / `counts{...}`。

- `counts` 是"恢复之后应该看到什么"的基线（站点数、启用数、有效凭据数、issued/redeemed/total、
  response 总数与未用量、池行数与图片字节数）。运维口头交接最容易出错的就是这个。
- `pepper_fingerprint` = `sha256("epocha/v1|pepper-fp|" || pepper)` 的 hex。
  它的唯一用途是把"这份备份和这把 pepper 配不配对"变成恢复前一次可执行的检查。
  前提：pepper 满足本仓库 ≥32 字节 CSPRNG 要求（`config.minKeyBytes` 已强制），此时反推不可行。
  它**不是**新增的加密原语需求，也不是"密钥的另一种形式"。
- manifest **不含**任何凭据明文、行数据或答案；它可以与备份分开保管（`stage5a.sh` 第 7 步按值扫描确认）。

## 3. 保留策略

- 建议：每日一次，保留 7 份日级 + 4 份周级；每周做一次真实恢复演练（不演练的备份等于没有备份）。
- **本工具不自动删除任何历史文件**。保留轮转交给运维（`systemd.timer` + 显式 `find -mtime`，
  或备份目录挂在快照存储上）。理由：自动删除的代码无法区分"我这轮生成的旧文件"与
  "别人放在这个目录里的东西"，误删的失败面是不可逆的。
- `--dest` 只接受**已存在**的目录，且拒绝含 URL/SQL 特殊字符与控制字符的路径（`checkDSNPath`）：
  路径会拼进 `file:` URI，宁可让运维改目录名，也不要"看起来备份成功、其实读回的是另一个文件"。

## 4. 恢复：旧快照的安全语义（必须先读这一段）

**恢复会把备份之后发生的消费、撤销与停用记录全部丢掉。** 这不是可以"小心一点"绕过去的风险，
而是快照的定义。具体到本服务的四张关键表：

| 备份之后发生的事 | 旧快照里的状态 | 后果如果不做处理 |
|---|---|---|
| 某 response 已被验证 | 该行仍 `used=0` | **同一个 token 可以被第二次接受**（防重放账本被回滚） |
| 某 challenge 已终结/过期 | 仍 `issued`/`redeemed` | 已作废的作答机会复活 |
| 某站点凭据已被撤销 | `revoked_at IS NULL` | 撤销掉的 `sk_...` 重新可用 |
| 某管理凭据已被撤销 | 仍在 `admin_keys` | 撤销掉的管理入口重新可用 |
| 某站点被停用 | `enabled=1` | 已下线的站点重新接流量 |

因此本项目的恢复是**保守恢复**：产物是一份"所有还能被接受的东西一次性失效"的新文件，
而不是一台"回到过去"的服务。`store.Restore` 在**一条事务**里做完：

```sql
UPDATE challenges SET status='invalid', redeemed_at=NULL WHERE status <> 'invalid';
DELETE FROM responses;
DELETE FROM pool;
UPDATE sites SET enabled=0;
DELETE FROM site_secrets;
DELETE FROM admin_keys;
-- 然后装入本次操作指定的**新**管理凭据（label='restore'），并写一条 restore_prepare 审计
```

每条对应的后果：

- `responses` 全删 + `challenges` 全部终态 ⇒ 恢复前签发的 token 一律兑换不了。
- `pool` 全删 ⇒ 主密钥换过之后 `K(qver)` 派生的判分密钥必然判假，留着存量题只会白白消耗
  用户的作答机会（DESIGN §6 的轮换运维约束在这里是同一条）。
- `sites.enabled=0` ⇒ **恢复完成前不接收业务流量**，由人工逐个确认后再启用。
  表现是 400 `invalid-request`（与"站点不存在"同形，不透露哪一层失败）。
- `site_secrets`/`admin_keys` 全删 ⇒ 备份后被撤销的凭据不会在快照里复活；
  管理访问靠**这次操作本地装入的新凭据**，**没有任何未鉴权的恢复 HTTP 接口**。

主密钥**不在库里**，所以恢复不处理它。恢复后的启动要求（`Restore` 的文档注释与 `stage5a.sh` 演练一致）：

- 用**新的** `EPOCHA_MASTER_KEY`（或 `EPOCHA_MASTER_KEY_FILE`）启动；
- **不设** `EPOCHA_MASTER_KEY_PREVIOUS`——那把"旧签名回退"正是"旧 token 重新可用"的唯一通道。

pepper 的处置是显式的：

```bash
# 默认：pepper 与该备份不配对 ⇒ 拒绝（ErrPepperMismatch），且不产出任何文件
epocha-ops restore --backup <快照> --manifest <manifest> --out-dir <隔离目录>

# 确认按 pepper 轮换恢复才加这个 flag；后果写进错误文案：站点与管理凭据必须重新签发
epocha-ops restore ... --rotate-pepper
```

不做"自动检测并顺手轮换"：拿错密钥文件与有意轮换在两可之间，靠猜的失败面是签名与摘要。

## 5. 恢复流程（复制即可执行）

恢复**永远**在隔离目录里做，产物是新文件；当前工作库逐字节不动（演练里用 sha256 前后对账证明）。

```bash
# 0) 停服。恢复期间不接收业务流量——这是流程的一部分，不是建议。
docker compose stop

# 1) 校验这份备份可信（不要跳过；非空文件不等于可用备份）
epocha-ops verify --db <快照>.db --manifest <快照>.manifest.json    # 非 0 就停下来

# 2) 隔离恢复到新目录（不是数据目录！）
install -d -m 700 -o <uid> -g <gid> /var/tmp/epocha-restore
EPOCHA_PEPPER_FILE=/etc/epocha/keys/pepper \
EPOCHA_ADMIN_KEY="$(cat /path/to/new-admin-credential)" \
  epocha-ops restore \
    --backup /var/backups/epocha/epocha-<stamp>.db \
    --manifest /var/backups/epocha/epocha-<stamp>.manifest.json \
    --out-dir /var/tmp/epocha-restore
# 输出的报告给出：schema_before→after、站点/凭据/response/池的 before→after 计数、pepper_rotated

# 3) 换入：把数据目录指向恢复产物（或把产物移入数据目录），并放好**新的**主密钥文件。
#    旧的 master/pepper 与旧的管理凭据文件不要留在原地当"回退通道"。
mv /var/lib/epocha/epocha.db /var/lib/epocha/epocha.db.quarantine-<stamp>   # 保留现场，不删
cp /var/tmp/epocha-restore/epocha.db /var/lib/epocha/epocha.db

# 4) 启动 + 只读检查（此时所有站点禁用、所有业务请求被拒，是预期状态）
docker compose up -d --wait
epocha-ops check --path /readyz

# 5) 用新管理凭据逐个确认站点后再启用。**没有**批量自动启用。
printf '{}' | docker compose exec -T epocha /usr/local/bin/epocha-ops admin POST /api/admin/sites/<id>/enable

# 6) 每把需要用的站点凭据都必须**重新签发**（快照里的凭据已被删除，旧 secret 不再有效）：
printf '{}' | docker compose exec -T epocha /usr/local/bin/epocha-ops admin POST /api/admin/sites/<id>/secrets
#    → 明文 secret 只在这一次响应里出现，业务后端配置要在这一步同步更新
```

第 6 步是真实成本：**恢复 = 所有接入方的 `secret_key` 需要重新发放**。把它写成风险而不是省略它。
如果这一点不可接受，正确的做法不是"温和恢复"（保留旧凭据），而是把备份窗口缩短 + 提升恢复频率——
因为保留旧凭据等价于"被撤销的凭据复活"。

## 6. 演练必须证明的事（`docs/scripts/stage5a.sh` 第 7 步）

1. 建站点 → 完成一次真实消费 → 备份；
2. 备份**之后**再发生一次消费、一次凭据轮换（立即撤销旧凭据）、一次站点停用；
3. `verify` 通过；改一个字节后 `verify` 必须失败；
4. 恢复到隔离目录：工作库 sha256 逐字节未变；产物是单文件（无 `-wal`/`-shm`/`.partial`）；
5. pepper 不配对 ⇒ 拒绝且不留任何产物文件；
6. 用**新主密钥**起一个隔离的恢复实例：
   - `/readyz` 通（可服务探针连通 ≠ 业务开放）；
   - 备份前签发的旧 response 被判失败；
   - 所有站点默认禁用，领题被拒（400）；
   - 新装入的管理凭据可用，**旧管理凭据被拒**；
   - 人工启用后，新的完整闭环成功。

## 7. 这一版**不能**保证的事（因此阻止安全开放）

写在这里不是"风险列表末尾"，而是每一项都会改变你能不能对外提供服务：

| # | 不能保证 | 对运维的硬约束 |
|---|---|---|
| 1 | 在线备份期间的一致性快照**不能保证零写入丢失**：快照是它开始那一刻的状态，之后的写入不在里面 | 备份窗口内不要做凭据撤销/站点停用这类**安全动作**；安全动作要么在备份前，要么在备份后重做一次 |
| 2 | 旧快照**不**保持备份之后的防重放、撤销、停用状态 | 恢复必须走 §4/§5 的保守流程；"直接换上旧库就跑"是被流程禁止的操作 |
| 3 | 恢复**不**保留任何站点凭据与管理凭据 | 恢复即需要重新签发全部接入方 secret；不可省略 |
| 4 | 单文件本地卷的备份**不**提供异地冗余；磁盘故障与备份目录同故障时两者一起没 | 备份必须拷出到另一存储（对象存储/另一主机），并保护 manifest 与 pepper 的配对关系（分库存放） |
| 5 | 本轮**没有**测跨 schema 版本的向后兼容升级恢复（v1→v4 的快照在新程序上恢复） | 版本升级前先在副本上演练一次；manifest 的 `schema_version` 就是给这一步核对用的 |
| 6 | 本轮**没有**测"备份正被第三方工具读取时"的并发行为 | 备份窗口不要与外部扫描/快照工具重叠 |

第 1/2/3 条如果不接受，就不要把服务开放给生产流量——它们不是"注意一下"级别的提醒，
而是本方案的安全边界本身。
