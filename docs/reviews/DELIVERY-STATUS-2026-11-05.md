# EpoCHA 交付状态与收敛冻结 GO 方案（2026-11-05）

> 本文件回答三个问题：**现在开发到什么程度、还差什么、怎样收敛到 frozen。**
> 它不是发布批准；`production_ready:false` 不变。当前 Cloady 实例是 **amd64 测试部署**，
> 正式支持范围仍是 **linux/arm64**。

## 1. 一句话状态

**链路已经完整跑通，v2 题库已经替换并经过真实 OCR 基线验证，Cloady 测试页可以实际作答；
但这不是生产上线批准：F-01、B-03、arm64 full、人工/效果验收和持久化仍未闭合。**

## 2. 当前可测试入口

| 项 | 值 |
|---|---|
| Cloady 应用 | `jian-shi/jpn1` production，region `jpn1`，状态 `running` |
| 公共基址 | `https://jpn1-rmw1vpfv-jian-shi.cloady.io` |
| **浏览器测试页** | `https://jpn1-rmw1vpfv-jian-shi.cloady.io/demo?site_key=pk_jpn1_v2_20261105` |
| API | `POST /api/challenge`、`POST /api/redeem`、`POST /siteverify` |
| 测试 site_key（公开） | `pk_jpn1_v2_20261105` |
| 测试 site secret | 本机 `tmp/cloady-site-v2-secret-final2.txt`（仅当前用户 NTFS ACL，gitignore；不要提交/贴聊天） |
| 题型 | `text-image-v2`，难度 1，8 字符，480×160 |
| 管理面 | 不发布；公网 `/api/admin/*` 404 |
| 持久化 | **无持久卷**；重建应用会丢 SQLite，站点需重新播种 |

真实浏览器（VPS headless Chromium + CDP）已确认 `/demo` 标题/占位符都是 8 字符，widget 状态 `ready`。
页面只调用公开 challenge/redeem，不调用 `/siteverify`、不读 secret、不写业务数据。

## 3. 本轮实际完成

### 3.1 题库 v2

- 新增 `text-image-v2`：8 字符、32 字符集、480×160；单字旋转 10°..35°、剪切 0.15..0.55、
  缩放 0.90..1.30、主动重叠 4..12px、波浪基线、穿字干扰线、随机墨色、1..3px 加粗。
- v1 保留为历史基线；v1 的全部档位使用零值 style，渲染路径不因 v2 改变。
- v2 仍为 `experimental:true` / `production_ready:false`；管理面 note 已改为如实描述
  “已有固定工具 OCR 基线，不代表抗多模态/抗机器能力，人工可读性未验收”。

### 3.2 OCR 基线（正式 holdout）

工具：Ubuntu 24.04 + tesseract **5.3.4** / eng **4.1.0**；`psm=6`、`oem=3`、字符集白名单；
holdout 每档 **100 张**；工具失败、缺测与畸形输出都计不通过。

| 题型/档位 | 严格整串通过率 | 读法 |
|---|---:|---|
| v1 d1 | **95.00%** | 低档挡不住 OCR |
| v1 d3 | **31.00%** | 中档 |
| v1 d5 | **3.00%** | 高档 |
| **v2 d1** | **0.00%** | 8 字符 + 形变；可读性最好 |
| **v2 d2** | **0.00%** | 更强形变，部分样本人眼偏难 |
| **v2 d3** | **0.00%** | 更强形变 |
| **v2 d5** | **0.00%** | 最强形变 |

结论只对“该固定工具/参数/样本集/机器”成立；**不是**抗多模态、抗打码平台、人工可读性或
目标机攻击成本证明。人工可读性、辅助技术、真机移动端仍是 **NOT_RUN**。

### 3.3 链路与部署

- Cloady 专用 `deploy/cloady/Dockerfile` + `entrypoint.sh`：Go 1.27.3 多阶段构建、alpine 运行时、
  数字 uid、`/readyz` 健康检查、环境密钥（Cloady `secretKeys`）。
- 上线验证：`/healthz`、`/readyz`、`/widget/v1.js` 200；`/api/challenge` 200（v2, 8 字符）；
  `/api/redeem` 200；`/siteverify` 首次 `success:true`、重放 `timeout-or-duplicate`。
- `GET /demo` 只在 `EPOCHA_DEMO_PAGE=1` 时挂载；生产默认关闭。
- widget 中文占位符从写死“6 个字符”改为 `{length} 个字符`，新增 Node 回归；
  `v1.js` 54,803 B / gzip 18,998 B，仍在 30 KiB 预算内。

### 3.4 测试

| 检查 | 结果 |
|---|---|
| VPS 干净 worktree `go test -race -count=1 ./...` | **22 包全 PASS**（释放 18091 固定端口后） |
| v1/v2 generator、config、api、admin、server、evalgen/evalocr 测试 | 全 PASS |
| widget 语法 + PoW 向量 + 生命周期（含新增 8 字符占位符） | **35 PASS / 0 FAIL** |
| `gofmt`、`git diff --check`、`bash -n` | PASS |
| Cloady 真实 API 闭环 | PASS |
| Cloady `/demo` 真实 headless Chromium | 状态 `ready`，8 字符文案一致 |

## 4. 还不能称为“生产可交付”的原因

| # | 项 | 状态 | 为什么阻断 |
|---|---|---|---|
| B-01 | 正式 arm64 full 验收 | **BLOCKED** | 当前只有 amd64 试部署；arm64 只做了交叉编译 |
| B-02 | 项目自有 TLS/DNS/反代 | **NOT_RUN** | 当前 HTTPS 是 Cloady 平台入口 |
| B-03 | 掉电持久化 | **开放** | `synchronous=NORMAL` 缺口仍在；需 FULL 决策/测量或书面接受 |
| B-04 | 人工/效果验收 | **NOT_RUN** | 人工可读性、辅助技术、真机、题型人工各过 5 次未做 |
| F-01 | 停用/撤销与在途消费 | **开放** | 站点检查与 `ConsumeResponse` 仍是两个事务 |
| — | Cloady 持久化 | **测试限制** | 无持久卷；重建应用丢 SQLite |
| — | 正式候选 | **未生成** | 工作树尚未形成干净候选 OID/manifest；旧 `stage-5b-done` 标签不可用 |
| — | 容量 | **上一轮实测** | 约 1.5–2 RPS 持续通过；300 并发瞬时不全成功，需产品容量决策 |

## 5. 收敛冻结的 GO 方案

### P0：冻结“代码 + 测试链路”（本里程碑可以做）

1. **本地 commit** 当前 v2/demo/Cloady/文档/测试改动，形成可追踪 OID；**不 push、不打阶段标签**。
   提交正文写明：Cloady amd64 测试部署、v2 仍 experimental、非生产发布。
2. 把 `docs/CLOADY-DEPLOY.md`、本文件、`docs/DECISIONS.md` 的 ADR-023 一并纳入提交。
3. commit 后重新跑一次 `go test -race -count=1 ./...`（干净 worktree）和 widget 检查；
   记录 OID、命令与结果。正式候选仍需后续 arm64 full，不把这次 commit 当发布候选。

### P0：生产前必须闭合（不做完不能上线）

4. **F-01 语义**：收紧 `ConsumeResponse`/`RedeemAttempt` 的站点 enabled/version 检查到同一事务，
   补“停用先提交 ⇒ 消费必失败”的确定性交错测试；或明确放宽语义并改文档/测试。建议收紧。
5. **B-03 持久化**：改 `synchronous=FULL` 并在目标 1 vCPU/1GB 上测量写入时延/吞吐；
   或书面接受 `NORMAL` 掉电窗口。不要部署时临时改。
6. **干净候选 + arm64 full**：干净 OID + manifest；在 Linux arm64 / UID 1000:1000 上跑
   `go test -race`、vet、static build、secret-scan、stage4b/5a/5b/admin-ui、真实 Firefox、
   启动配方、备份/恢复、重放拒绝；把 `INCOMPLETE/3` 推到 `PASS/0`。
7. **项目自有入口**：域名/TLS/反代/防火墙/trust-proxy 网段验收；Cloady 平台入口不能替代。
8. **持久化与备份**：Cloady 测试实例若要长期使用，必须配持久卷/备份；当前“重建即丢库”是测试限制。
   生产按 `DEPLOYMENT`/`BACKUP-RESTORE` 做在线备份、异机副本与恢复演练。

### P1：生产运行前

9. 人工可读性、辅助技术、真机移动端、题型人工各过 5 次；M4/多模态/打码平台另测。
10. 容量决策：目标持续 RPS、瞬时并发、可接受 503 比例；调 pool 水位、图片预算、worker 与限流。
11. 监控：`/readyz`、容器重启、磁盘/WAL、5xx、题池水位与图片预算；告警接收人。
12. API 接入面：把 `docs/CLOADY-DEPLOY.md` §2/§3 作为对外契约；为业务方提供 site_key/secret
    的签发/轮换流程，secret 只在服务端。

### P2：收敛质量

13. F-02 `MaxSites` 并发上限、F-03 备份持久化退出码、F-05 管理凭据锁死告警、
    F-07 非 loopback 管理监听、F-09 反代 Host 文档、F-10 固定端口测试、F-11 测试夹具清理。
14. 产品补齐：风险评分（第三层）、题型权重/多生成器选择、统计与分级淘汰、非视觉通道、
    `fallback_base`、完整管理台。当前没有因为 v2 而完成这些。

## 6. 什么叫“frozen”

本项目的 frozen 必须分两层，不能混：

- **测试里程碑 frozen**：当前代码 + v2 + Cloady 测试部署 + OCR 收据 + API 文档可提交、
  可复现、可追踪。完成 P0 第 1–3 条即可。
- **生产发布 frozen**：P0 第 4–8 条全部 PASS，且人工/效果验收有真实证据；`release-check --profile full`
  为 PASS/0。当前不是。

**当前判断：可以冻结为“Cloady amd64 测试里程碑”，不能冻结为“生产发布候选”。**

## 7. 你现在要做的测试

打开：

`https://jpn1-rmw1vpfv-jian-shi.cloady.io/demo?site_key=pk_jpn1_v2_20261105`

步骤：看图输入 8 个字符 → 提交 → 等待 PoW → 看到 `verified`。这是只读测试页，不会写业务数据。
如果你要测 API：用 §2 的 curl，secret 从本机 `tmp/cloady-site-v2-secret-final2.txt` 读取，
不要贴到聊天或提交进仓库。

请记录：浏览器/系统、时间、状态是否到 `verified`、错误文案、是否换题；你的本人操作是最后一段验收。
