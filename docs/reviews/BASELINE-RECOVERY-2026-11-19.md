# 2026-11-19 基线恢复与生产候选治理

本报告对应 `fix/baseline-recovery-20261119` 的工程恢复，尚非正式发布批准。所有源码在 Windows 修改，先提交/推送 GitHub，再由 Linux 拉取；部署主机未就地修改源码，未修改 main、阶段标签或现有生产实例。

## 仓库事实与环境差异

- 初始 HEAD：`6fc8c657fbd5f553a44216dc786a2a01285fee36`，分支 `sync/visual-bank-20261116`，存在审计修复与 M4 的既有未提交内容。
- 本地与 GitHub 可见分支未列出 `p0-p1-production-hardening-next`；指定对象 `164bb6327b85fe1642bbb8bd95611186211c5625` 在已查本地/远端旧副本中不存在。不能冒充原候选。
- 基线 `adcbe0b42328231ec801098f390ba817fe8e057a` 存在，较初始 HEAD 多两个加固提交。先以 `25e518cc580d954ca7206c16b6702db302b31878` 保存 WIP，再以 `fb1fb2f9dddce7bdeff9b345c8321af6691f50c0` 合并基线。
- Windows amd64：默认 Go 1.24.2；已安装 Go 1.27.3 可通过绝对路径使用。Node 24.14.0，Docker CLI 29.1.2，Docker daemon 不可达。初始 D: 可用约 22GiB。
- Linux 补充验证机：amd64、UID:GID 1000:1000、4 个逻辑 CPU；默认 Go 1.27.1/Node 22.23.3，已安装 Go 1.27.3/Node 24.22.3。Docker Engine 29.8.2/Compose 5.6.0、Firefox 153.6.0esr、gcc 可用。
- 正式支持范围仍为 B / linux/arm64 / UID:GID 1000:1000。当前 Linux amd64 不替代 arm64 原生运行验收；Docker 有既有容器/镜像/卷，未删除或修改这些资产，full 专用空态前提不满足。
- Linux `/data` 与 `/tmp` 有余量，但 `/var/tmp` 所在 `/` 可用 0（100%）。SQLite 默认临时文件选择造成批量删除 `SQLITE_FULL`；隔离 `SQLITE_TMPDIR` 对照见下。

## 最小复现与处置顺序

| 项 | 最小命令/观察 | 实际结果 | 根因与最小处置 |
|---|---|---|---|
| A Wave 注册 | 基线 `go test -count=1 -run 'TestWave[12]Generators' -v ./internal/api` | FAIL / 1，七型的成功兑换与错答终结共 14 个子测试未注册 | 旧 API `testRegistry` 缺七型，生产 `knownGenerators` 已注册。保留 WIP 注册补丁，新增 API/server 集合回归 |
| A 同命令对照 | `fb1fb2f…` 同命令 | PASS / 0 | 测试夹具问题；没有生产漏注册的证据 |
| B 管理生命周期 | 基线 `timeout 300s bash docs/scripts/admin-ui-check.sh` | FAIL / 1，浏览器后创建站点 `000000` | 浏览器终止父 shell 共享服务；改为私有 loopback 故障代理，父脚本保留所有权、阶段探针与原 DB 恢复，故障仍计 FAIL |
| C 图片断言 | 真实浏览器 PNG 280×104，CSS width 318 | natural=280×104，HTML attrs=280×104，IDL=318×118 | `img.width/height` 是显示尺寸；改读 HTML 属性，另外验证显示比例，不取消真实尺寸断言 |
| C DTO 负例 | `84dee4a… go test -count=1 -run '^TestQuestionDTOImage' -v ./internal/api` | FAIL / 1，四种坏图/错配可出网 | `questionDTO` 增加 `png.DecodeConfig`；静态核对实际宽高，sprite sheet 核对单帧×网格，坏题沿原路径 503/隔离 |
| D FULL | `go test -json -count=1 ./...` | FAIL / 1 | schema 7 仍有旧测试断言；批量 Purge 的 FULL 单独诊断 |
| D 串行 | `go test -p 1 -count=3 -run '^(TestLedgerPurgeKeepsPerTransactionBatch500|TestPurgeAssignedBatchBudgetSurvivesReopen)$' -v ./internal/store` | FAIL / 1，三次均 FULL | 并非跨包并发独有；1 行可过、499 行微型账本亦失败，page_count≈60，max_page_count=4294967294，integrity_check 成功 |
| D 临时目录对照 | 相同源码，`SQLITE_TMPDIR=<本轮独占/data目录> go test -count=1 -run 'TestPurgeSQLite|TestLedgerPurgeKeepsPerTransactionBatch500|TestPurgeAssignedBatchBudgetSurvivesReopen|TestPurgeFreesPagesNotFileShrink' -v ./internal/store` | PASS / 0 | 根分区临时空间不足；不改 SQL、不放宽事务/引用/批量边界，不清理未知资源 |

生成器正常 PNG 属性测试：text-image v1/v2/v3 五档各 3 样本，通过。静态 PNG 等于 DTO 宽高；sprite sheet 宽高表示单帧，不能误与整张 PNG 直接比较。

## 修改范围与独立回归

- A：继承 `internal/api/api_test.go` 注册补丁；新增 `internal/api/wave_registry_regression_test.go`、`cmd/server/wave_registry_regression_test.go`。
- B：`docs/scripts/admin-ui-check.sh`、`docs/scripts/browser/adminui_browser.test.mjs`、独占故障代理与 `admin-ui-ownership.test.mjs`。真实网络失败保留，不用 fake fetch 代替。
- C：`internal/api/handlers.go`、`image_contract_regression_test.go`、生成器属性测试、浏览器 `image-dimensions.mjs` 与独立 Node 测试。无效图片拒绝前置于出网；不读 verifier，不改变判分。
- D：旧 schema 夹具期望修正；新增 Purge 容量/查询对照测试；`deploy/preflight.sh` 单独报告 SQLite 临时目录余量，新增预检回归。浏览器逐用例资源隔离与进程清理在各自工装内。
- 治理：`docs/scripts/baseline-evidence.mjs` 将原始命令/退出码/日志/环境/完整 OID/缺陷和修复关联汇总；空输入、缺日志、假 PASS、日志越界均拒绝。它是汇总器，不是来源签名或执行证明；SHA-256 只证明所打包字节的一致性。正式结论仍由未放宽的 `release-check --profile full` 决定。

## 已执行联合回归

`9fada47b63f873c1bc72f0b2b75dc2ffd729f00f`，Linux amd64、Go 1.27.3、独占 SQLITE_TMPDIR：

```sh
go test -json -count=1 ./internal/api ./internal/generator ./internal/store ./cmd/server
```

四包全部 PASS，退出码 0。此结果不代替全仓 `go test -race -count=1 ./...`、浏览器、Docker、备份恢复与正式平台门禁。

完整门禁、最终提交及证据哈希在本报告完成后追加；未运行项保持 NOT_RUN/BLOCKED，不推测 PASS。

## 发布结论与下一阶段前置条件

当前 **不可发布**，`RELEASE_APPROVED=false`。正式 arm64 环境、专用 Docker、当次 full 门禁、人工可读性/真机/辅助技术及现有安全决策阻断仍需逐项证据。B-03 `synchronous=NORMAL` 的掉电保证缺口沿 ADR-022/032 保留，不在本轮擅改。

下一阶段 AI 攻防评测须先固定干净源码 OID、测试与构建产物 manifest、可复现工具链及运行环境；冻结 solver/样本分组与计时口径；核对 M1–M4、答案空间/容差、三次猜测累计概率；分离公开 DTO 攻击输入与私有 verifier/答案；准备有许可的数据/离线工具与真实人工基线。新增模型在线调用或依赖须另获授权。已有 M4 失败结果原样保留，工程测试绿不改变 `experimental:true` / `production_ready:false`。
