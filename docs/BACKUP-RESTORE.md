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

### 停服要求：只有写这条数据目录的动作才需要

阶段 5B 起，"服务在不在跑"由数据目录上的 `flock` 排他锁判定（`internal/store/lock.go`，
规则与运维口径在 `docs/DEPLOYMENT.md` §5.5），不再靠探管理端口——探错地址、管理面没挂、
监听形状换了，任何一条都会让"检查通过"变成"根本没检查"。

| 命令 | 要不要停服 | 为什么 |
|---|---|---|
| `backup` | **不要** | 只读连接 + `VACUUM INTO` 的一致性快照本来就允许并发写；强制停服只会让按频率备份变成没人愿意做的事 |
| `verify` | 不要 | 只读快照文件，不碰活库 |
| `restore` | 看目标目录 | 它对 `--out-dir` 取排他锁：恢复到隔离目录不受影响；`--out-dir` 恰好是某个服务正在用的数据目录 ⇒ **拒绝**（"隔离恢复"写成"就地恢复"是最常见的手滑） |
| `admin-recover` | **要** | 它改 `admin_keys`；与服务争同一把锁，拿不到就拒绝，不等待、也不降级成"看起来没人在用" |

### 四种"可信"是四件事，别混着宣称

这份交付对备份提供的是前两种检查，**不是**后两种：

| 维度 | 这一版有没有 | 具体机制与它挡得住什么 |
|---|---|---|
| **完整性校验（checksum）** | **有** | `manifest.sha256` + `backup_bytes`，`verify` 用常数时间比较重算值。挡得住截断、位腐、写一半、拷错文件 |
| **数据库一致性** | **有** | `PRAGMA integrity_check` 必须只返回单个 `ok`，外加 schema 版本闸门（比程序新的库直接拒）。挡得住结构性损坏 |
| **来源真实性（provenance）** | **没有** | manifest 里没有任何签名。谁能拿到备份目录，谁就能同时改写 `.db` 与它的 manifest——sha256 一致，`verify` 照样通过。**因此不得宣称"能抵御恶意替换"** |
| **保密性** | **没有** | 快照是明文库（凭据摘要、账本、题池都在里面）。保护它靠的是文件权限（0600）、分库存放与存储层加密，不是工具本身 |

要补上来源真实性需要的是 manifest 签名（以及一把自己不进备份目录的签名密钥），那是设计题不是实现题；
在它存在之前，正确的心智模型是：**备份目录的可写者 = 备份内容的作者**。
同理，`pepper_fingerprint` 只回答"配对不配对"，它不是签名，也不提供来源证明。

### 产物形状与"半成品不算成功"

一次成功的备份留下**恰好两个文件**：

```
epocha-20261018T091500Z-56d3911.db            # 0600，单文件库
epocha-20261018T091500Z-56d3911.manifest.json  # 0600，自描述记录
```

保证机制（`store.Backup`）：

- 快照先写入本次调用独占的暂存目录，通过 `integrity_check`、计数和哈希核验，数据库及 manifest 使用无覆盖发布；manifest 最后落位。调用只清理自己创建的暂存目录，不删除已有 `.partial` 或其他调用的产物。
- 如果数据库已发布但 manifest 发布失败，命令明确报告数据库已保留且不是完整备份；不能把单独存在的 `.db` 当作成功。两个文件均发布后的目录同步或暂存清理失败会给出警告，保留产物并要求核验，不静默覆盖重跑。
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

## 5. 恢复流程（Linux：完整隔离目录接管）

**交付与接管的单位是整个新数据目录，不是一个要塞回旧目录的 `.db`。** 旧目录连同主库、
`-wal`、`-shm`、`epocha.lock` 原样保留；旧密钥与配置另外隔离保管，不作为自动回退入口。
禁止把新主库复制进旧 WAL 现场，也禁止为了“清理干净”删除旧 sidecar 或锁文件。

以下是需逐步确认的运维配方，不是本轮已执行的记录。2026-11-02 的 Windows 环境没有可用 Linux
容器引擎，**完整目录接管、锁竞争、权限与异常 WAL 场景均 NOT_RUN**；静态回归不替代该验收。
接管产物必须符合 ADR-021：在临时副本完成失效、审计、完整性与单文件收尾后才无覆盖交付最终名称。
非零退出、进程中断或只看见临时文件，都不能被当作可启动的恢复产物。

### 5.1 先停服并固定路径、身份与材料

- 操作者须有本次恢复授权，并控制旧服务的启动入口；维护期间停止业务流量及外部自动拉起任务。
  不用端口空闲、PID 文件或 `/healthz` 代替运行锁。
- 在 Linux 以实际服务 UID/GID 操作；正式范围仍是 `1000:1000`。`EP` 是原部署根的绝对路径，
  `PROJECT` 是原 Compose 项目名，`OLD_ENV` 是原环境文件。**不要换项目名额外启动第二套权威实例**。
- 预先确认 `OPS` 是已验收的本机 `epocha-ops` 绝对路径，`REPO` 是配套源码路径，`BACKUP` / `MANIFEST`
  是选定快照。`OLD_KEYS` 是原密钥目录。`NEW_DATA` / `NEW_KEYS` 是两个不同、尚不存在的绝对路径，
  位于本地持久文件系统、各自父目录已由运维准备；不能用会定期清理的 `/var/tmp` 作为接管后的数据卷。
  `NEW_ENV` 是尚不存在的新配置文件。数据、密钥、备份仍分目录，不嵌套。

```bash
set -euo pipefail
umask 077
: "${EP:?}" "${PROJECT:?}" "${OLD_ENV:?}" "${OPS:?}" "${REPO:?}"
: "${BACKUP:?}" "${MANIFEST:?}" "${OLD_KEYS:?}" "${NEW_DATA:?}" "${NEW_KEYS:?}" "${NEW_ENV:?}"
test "$(id -u):$(id -g)" = 1000:1000
OLD_COMPOSE=(docker compose --project-name "$PROJECT" -f "$EP/compose.yml" --project-directory "$EP" --env-file "$OLD_ENV")
"${OLD_COMPOSE[@]}" stop epocha
running="$("${OLD_COMPOSE[@]}" ps -q --status running epocha)"
test -z "$running"

# 去掉可能覆盖文件入口或 Compose 配置的继承值；不能把旧 master 当 previous 带回。
unset EPOCHA_MASTER_KEY EPOCHA_MASTER_KEY_FILE EPOCHA_MASTER_KEY_PREVIOUS EPOCHA_MASTER_KEY_PREVIOUS_FILE
unset EPOCHA_PEPPER EPOCHA_PEPPER_FILE EPOCHA_ADMIN_KEY EPOCHA_ADMIN_KEY_FILE
unset EPOCHA_DATA_DIR EPOCHA_KEY_DIR

# mkdir 不带 -p：任一目标已存在就停，不复用失败目录、不覆盖其中的库/凭据。
mkdir -m 700 -- "$NEW_DATA" "$NEW_KEYS"
# 默认沿用与备份配对的 pepper；内容只进新文件，不进终端、环境或命令行。
(set -C; cat -- "$OLD_KEYS/pepper" > "$NEW_KEYS/pepper")
bash "$REPO/deploy/prepare-keys.sh" "$NEW_KEYS"
# 新目录中的 master/admin 是本次新生成的；pepper 被校验并保留。不要对旧目录执行生成脚本。
```

如 pepper 已丢失或明确要轮换，**先独立确认该决定**：省略旧 pepper 的复制，由准备脚本生成新 pepper，
恢复时显式加 `--rotate-pepper`。不得因“配对失败”自动重试成轮换；两条路径都要重签发所有站点 secret。

### 5.2 校验并在新目录恢复

```bash
EPOCHA_PEPPER_FILE="$NEW_KEYS/pepper" "$OPS" verify --db "$BACKUP" --manifest "$MANIFEST"
EPOCHA_PEPPER_FILE="$NEW_KEYS/pepper" EPOCHA_ADMIN_KEY_FILE="$NEW_KEYS/admin" \
  "$OPS" restore --backup "$BACKUP" --manifest "$MANIFEST" --out-dir "$NEW_DATA" --dest-name epocha.db
# 必须有成功报告，核对 schema、站点全部禁用、有效站点凭据/response/题池为零、新管理凭据为一把。
test -s "$NEW_DATA/epocha.db"
test ! -e "$NEW_DATA/epocha.db-wal"
test ! -e "$NEW_DATA/epocha.db-shm"
```

`restore` 在唯一的隐藏临时副本上完成失效、审计、完整性核验和单文件收尾，最后以无覆盖方式交付 `epocha.db`。在最终文件交付后，目录同步或本轮临时文件清理仍可能失败；这时命令保留有效的最终库，并通过报告的 `Warning` 与终端警告说明“已交付，但持久化或清理状态尚未确认”。退出码 0 不会消除该警告。遇到它先检查存储并重新核验，不能当作事务回滚、删除最终库或覆盖重跑，也不能直接进入接管步骤。

`restore` 对 **NEW_DATA** 取 `flock`；没有改旧目录，所以不靠修改旧库来取得接管资格。
`ErrLockHeld` ⇒ 有服务/离线操作占用新目录，立即停止接管并查清来源；`ErrLockAccess` ⇒ 核对服务 UID/GID、
目录与锁文件权限，不提权硬闯。恢复结束到启动之间若有别的进程抢锁，服务会再次非阻塞拒绝；
不得改端口重试来绕开它。旧容器的停止状态是运维核对项，**不是锁的替代实现**。

确认新数据目录、密钥目录为服务身份所有、0700；新库、锁文件和三把密钥为该身份所有、0600（密钥也可0400），
`stat` 核对权限但不打印内容。`epocha.lock` 留在新目录里，服务接管同一目录的同一把锁，不移动或删除它。
失败时保留新目录、凭据与报告供诊断，旧现场保持不动；重做须另选全新路径，删除任何旧材料另行授权。

### 5.3 同步切换整个数据挂载与新密钥挂载

先用无覆盖复制保留旧配置，**只编辑新的配置文件**。

```bash
(set -C; cat -- "$OLD_ENV" > "$NEW_ENV")
```

在 `NEW_ENV` 里核对以下值；`$NEW_DATA` / `$NEW_KEYS` 必须替换为第一步选定的**实际绝对路径**，
不能把这些占位符原样保存。其余端口、网段、限额及镜像身份沿用经批准的配置。

```ini
EPOCHA_DATA_DIR=$NEW_DATA
EPOCHA_KEY_DIR=$NEW_KEYS
EPOCHA_ADMIN_KEY_FILE=/etc/epocha/keys/admin
EPOCHA_UID=1000
EPOCHA_GID=1000
```

新 `keys` 目录整体挂到容器 `/etc/epocha/keys:ro`，因此 master/pepper/admin 都对应恢复时选定的文件。
尤其不能只把新 admin 作为一次性环境变量交给 restore，而让容器的 ops 继续读旧 admin 文件。
同时核对 Compose 主文件、override 与运行入口：**`EPOCHA_MASTER_KEY_PREVIOUS` 和
`EPOCHA_MASTER_KEY_PREVIOUS_FILE` 均未设置**，主密钥来自新目录；不挂载旧密钥目录作为后备。

```bash
NEW_COMPOSE=(docker compose --project-name "$PROJECT" -f "$EP/compose.yml" --project-directory "$EP" --env-file "$NEW_ENV")
"${NEW_COMPOSE[@]}" up -d --wait --force-recreate --no-build --pull never epocha
"${NEW_COMPOSE[@]}" exec -T epocha /usr/local/bin/epocha-ops check --path /readyz
"${NEW_COMPOSE[@]}" exec -T epocha /usr/local/bin/epocha-ops admin GET /api/admin/whoami
"${NEW_COMPOSE[@]}" exec -T epocha /usr/local/bin/epocha-ops admin GET /api/admin/admin-keys
```

`whoami` 必须实际返回 **`status=200` + 新 `key_id`**，并与列表中本次 `label=restore` 的有效记录对应。
`readyz` 成功不代表管理凭据可用，更不代表站点已开放。若仍为 401，先核对**容器实际挂载**和 admin 文件入口，
不要再跑 restore/admin-recover 猜测修复；已有库不会因为引导文件被替换而重新登记凭据。
后续启动、备份、定时任务全部固定这个 `NEW_ENV`；旧配置隔离保管、解除旧启动入口的引用，不能再裸用
`docker compose up` 或旧环境文件把旧数据目录拉起。失败后也**不自动回退旧目录**——它可能接受已撤销凭据或旧 token。

### 5.4 先重签发与配置业务，再逐站点启用并验证闭环

1. 新管理凭据只读列站点，逐个核对禁用状态、模式、题型、TTL 与精确 Origin。未核对的站点保持禁用。
2. 为选定站点先签发新 secret，把唯一一次的明文交付到受控 0600 文件/凭据保管系统，再更新业务后端。
   不把它粘到工单、日志或 shell 历史；交付断线时先查凭据列表，不能盲目重放轮换。

```bash
: "${SITE_ID:?已核对的站点 ID}" "${SECRET_RECEIPT:?新的受控交付文件路径}"
(set -C; printf '{}' | "${NEW_COMPOSE[@]}" exec -T epocha /usr/local/bin/epocha-ops admin \
  POST "/api/admin/sites/$SITE_ID/secrets" > "$SECRET_RECEIPT")
grep -q '^status=201$' "$SECRET_RECEIPT"
```

停在这里，由接入方安全取出新 secret 并更新服务端配置。**人工确认后**才单独执行启用，不能整段自动跑完。

```bash
printf '{}' | "${NEW_COMPOSE[@]}" exec -T epocha /usr/local/bin/epocha-ops admin \
  POST "/api/admin/sites/$SITE_ID/enable"
"${NEW_COMPOSE[@]}" exec -T epocha /usr/local/bin/epocha-ops admin GET "/api/admin/sites/$SITE_ID"
```

3. 在仍受维护访问限制的真实接入上完成 **challenge → PoW/作答 → redeem → 新 secret 的 siteverify**，
   只在 HTTP 200 且 `success:true` 时放行业务写入；同 token 重放必须失败。确认后才对该站点开放流量。
4. 恢复验收另外证明旧 token 与旧 secret 被拒、旧管理 key 实际 HTTP 401，新 key 实际 HTTP 200。
   旧 key 应在备份前曾验证为有效；负测只用一个凭据入口，不能把 env/FILE 冲突、超时或 CLI 失败当成认证拒绝。
   为排除“站点仍禁用所以一切都拒绝”的假证据，旧 token/secret 负测在新闭环成功的同一站点上复验。

**恢复 = 所有接入方的 `secret_key` 需要重新发放**。这是实际恢复成本；不能为了缩短停机保留快照中的旧凭据。
若不能接受该成本，应缩短备份窗口并演练恢复，而不是把保守失效改成“旧凭据继续用”。

### `admin-recover`：新凭据文件与库变更之间失败，怎么办

这条路径上有两个写入点（凭据文件、`admin_keys` 事务），中间还可能有进程被杀，所以必须有一段
明确写着"失败时你在哪"的说明，而不是让运维靠猜。执行顺序是刻意的：
**先拿锁 → 写凭据文件（`O_EXCL` + 写完 `Sync`）→ 再改库**。

判据不是一句"非 0 就是没改库"——那种说法在这条路径上是**错的**，因为它把两件不同的事混成一件：
`tx.Commit()` 返回错误不等于库里没有这次写入，而"提交已确认之后收尾报错"更是完全不同的处置。
所以代码在 key_id 已生成的每个失败出口都用**另一个只读连接**回查一次
（`store.AdminKeyAdopted`），按回查的事实决定删还是留：

| 失败发生在 | 库 | 凭据文件 | 恢复动作 |
|---|---|---|---|
| 取锁之前（服务在跑） | 未改 | **不创建** | 停服后原样重跑同一条命令 |
| 写凭据文件时（同名文件已存在、目录不可写） | 未改 | 未创建；已有文件**不被覆盖**（`O_EXCL`） | 换一个 `--key-file` 名字 |
| `key_id` 生成之前失败（参数/句柄/开事务） | 未改（不可能有本次写入） | 未创建 | 直接重跑 |
| 提交**之前**的写入失败（审计/登记语句报错） | 未改，且**已用只读连接确认库里没有这条 key_id** | 本次创建的那个被删掉 | 直接重跑（不需要换名字，文件已没） |
| `tx.Commit()` 报错，只读回查**读到**这把未撤销的凭据 | **已改**（写入已落盘） | **保留**，绝不删 | 不要重跑（重跑会再撤销一次并换成另一把）。把保留文件放到实际 `EPOCHA_ADMIN_KEY_FILE` 路径、启动服务、`whoami` 核对 `key_id` |
| `tx.Commit()` 报错，且**回查本身失败**（方向未知） | 不确定 | **保留**（未知方向时删除是不可逆的） | 同上：先用它启动并 `whoami`；确认它不可用之后再删文件重跑 |
| 提交已确认，但数据库句柄 `Close` 失败 | 已改（已提交并读回确认） | **保留** | **不需要**重跑。这是一条警告，不是回滚；输出里会写清"凭据已生效" |
| 崩溃在"凭据文件已写、事务未提交"之间 | 未改 | 留下一个没人使用的文件 | 它不影响任何东西；确认这次恢复没成功后删掉它再重跑（重跑要换文件名，否则被 `O_EXCL` 拒） |

三条口径：

1. **退出码 0 ⇒ 提交已确认**（库里就是这把，文件是它的唯一副本）。非 0 时看输出属于哪一行：
   文案会明确说"已采用/无法确认/确认未采用"，三者处置不同。
2. **确认未采用才删文件**。这条不是从"有 error"推出来的，而是从只读回查读到的事实来的——
   同一个连接看得见自己未提交的写入，所以回查必须另开只读连接（代码注释里写了原因）。
3. 事后可核对的两处事实是 `whoami` 返回的 `key_id` 与审计里的 `admin.recover_local` 一条。
   两者对不上就是没恢复成功，不要"看起来能用"就开放业务。凭据值本身从不进日志、stdout 或错误文案。

## 5.5 三把材料丢失是三件事，`admin-recover` 只管其中一件

| 丢的是什么 | 后果 | 能不能就地恢复 | 需要什么材料 |
|---|---|---|---|
| **管理凭据**（`admin key`，运维用 Bearer 串） | 管理面全部 401；`EPOCHA_ADMIN_KEY(_FILE)` 改也没用（库里已有记录，引导逻辑必须忽略 env） | **能**：`epocha-ops admin-recover --db … --key-file …`（停服、取锁、撤销存量、装入新的一把，同一事务） | 只要 **pepper**（摘要算不出来就连"撤销旧凭据"都做不到）与数据库文件本身。不需要 master |
| **主密钥**（`EPOCHA_MASTER_KEY`，签 `chal`/`resp`/`qver`） | 已签发 token 全部验不过（新 token 能正常签发/验证，业务表现为"老链接失效"） | **没有"找回"这条路**：旧 token 无法重算。只能**用新的 master 重新开始**，让旧 token 自然过期（TTL 上限 1 小时） | 新密钥材料即可；不需要备份。要"能验旧 token"的唯一办法是当初留了 `EPOCHA_MASTER_KEY_PREVIOUS`（轮换窗口），丢了就丢了 |
| **pepper**（站点 secret / 管理凭据的摘要密钥） | 库里所有 `secret_key`/`admin key` 的 HMAC 摘要永久对不上 ⇒ 站点凭据与管理凭据同时失效 | **没有"找回"这条路**：摘要不可逆。只能重新签发每一把凭据，或从**配对备份**恢复 | 要么备份里有同一把 pepper，要么走"全部重签发"：恢复库 + 逐站点 `POST /secrets` + 通知所有业务后端换 secret（DESIGN §6：pepper 与 DB 同库存放会让这层保护直接归零，所以它必须是**独立的**一份备份） |

由此得出配对备份的最低要求（缺一项就把一种丢失变成不可恢复）：

1. **库快照**（`epocha-ops backup` 的 `.db` + `.manifest.json`）——恢复业务状态；
2. **pepper 文件**——否则快照里的凭据摘要谁也验不过；
3. **当时的 master 文件**——否则恢复后所有在途 token 失效（可接受，但要**知道**自己在接受什么）；
4. 一份**不在同一故障域**的副本（备份目录的可写者就是备份内容的作者，见 §2 的四维表：这一版只有 checksum 与一致性，没有来源真实性）。

恢复时需要新材料的场景写在 §4/§5：**恢复实例用新的 master 启动、且不设
`EPOCHA_MASTER_KEY_PREVIOUS`**；管理凭据用恢复流程新装入的那把。
`admin-recover` 不生成 master、不生成 pepper，也不会在缺它们的时候替你兜底——缺 pepper 它会直接拒绝。


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

上述历史隔离演练不代替 §5 的整体目录接管验收。当前候选还必须在隔离 Linux 环境覆盖：
旧目录带非空 WAL/SHM 且接管不向其写入新主库、输出目录被另一进程持锁、错误 UID/GID、
容器仍读旧 admin 文件时明确拒绝或诊断；成功时新挂载与新凭据实际生效、旧目录/锁文件保留，
重新签发 secret 后完整闭环成功且旧 key/token 被拒。2026-11-02 本轮这些运行项 **NOT_RUN**（Linux 环境 BLOCKED）。

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
| 7 | **没有来源真实性**：manifest 无签名，`.db` 与 manifest 可被同一个可写者一起改掉而 `verify` 仍然通过（详见 §2 的"四种可信"表） | 不得宣称抵御恶意替换。备份目录的写权限按"能改写历史"的级别保护；要真做来源证明需要 manifest 签名 + 一把不进备份目录的签名密钥 |
| 8 | **没有保密性**：快照是明文库（账本、凭据摘要、题池都在里面），工具不做加密 | 0600 权限、与 pepper 分库存放、存储层加密由运维承担；`counts` 可以给人看，`.db` 不行 |
| 9 | 运行锁是 `flock`（建议锁），只约束按规矩办事的进程 | 同等文件权限下绕开本仓库代码直接打开 SQLite 的本地程序不受它阻止；需要强制隔离时用独立属主/独占挂载。离线运维还要与数据目录同 uid，否则拿到的是权限错误而不是"被占用" |

第 1/2/3 条如果不接受，就不要把服务开放给生产流量——它们不是"注意一下"级别的提醒，
而是本方案的安全边界本身。第 7/8/9 条同理：把它们当成"风险清单里的备注"，
就会在文档与口头上把"备份可信"说成"备份可信且可证明来源"。
