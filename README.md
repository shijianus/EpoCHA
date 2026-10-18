# epocha

自托管人机验证服务：单 Go 二进制 + SQLite，调用契约与 reCAPTCHA 的 `POST /siteverify` 兼容。
核心防线是**生成式题池**（程序化生成、随机变换、一次性使用、用后即焚），外层是 PoW 成本门槛，最内层是风险打分决策。

- 设计与决策：`docs/DESIGN.md`（v2.6）；修订理由与历史错误表述：`docs/DECISIONS.md`
- 工程约定与安全红线：`AGENTS.md`
- 阶段 3A 现状（状态转换表、水位默认值、资源实测与未测项）：`docs/STAGE3A.md`
- 阶段 3B 现状（图片数据流、配置、本地预览步骤、评测方法与未测项）：`docs/STAGE3B.md`
- widget 接入契约（API、状态机、CSP 清单、无障碍边界）：`docs/WIDGET.md`
- 阶段 4A 现状（浏览器交互验收、实测数字、BLOCKED/NOT_RUN 清单）：`docs/STAGE4A.md`
- 阶段 4B 现状（真实窄屏、表单接入 fixture、OCR 基线数字、人工盲测入口）：`docs/STAGE4B.md`
- 单节点部署（容器、端口与管理面边界、密钥文件、反代）：`docs/DEPLOYMENT.md`
- SQLite 备份与保守恢复（旧快照的安全语义在这里，必读）：`docs/BACKUP-RESTORE.md`
- 阶段 5A 现状（演练结果、资源实测、BLOCKED 清单）：`docs/STAGE5A.md`
- 阶段 5B 现状（运行互斥、恢复边界、候选清单与持续演练）：`docs/STAGE5B.md`
- 依赖与字体许可：`docs/NOTICE.md`
- 前端 widget：中英双语、键盘可完成（`docs/WIDGET.md`）
- 命令：`make help`

## 状态

| 阶段 | 内容 | 状态 |
|---|---|---|
| 0 | 设计基线 + 目录骨架 + 测试入口 | 完成（tag `stage-0-done`） |
| — | 设计校正 v2（ADR-001…008，见 `docs/DECISIONS.md`） | 完成 |
| 1 | PoW + token + 持久化防重放 + `/siteverify` 最小闭环 | 完成（tag `stage-1-done`） |
| 2 | 多站点与 Key 管理 API、来源白名单、站点级配置、审计 | 完成（tag `stage-2-done`） |
| 3A | 生成器契约 + 有界预生成题池 + 原子领题 + 单次答案提交 + `arithmetic-baseline-v1` | 完成（tag `stage-3a-done`） |
| 3B | 首个视觉题型 `text-image-v1` + 图片资产入库/内联下发 + 本地预览 + 离线评测脚手架 | 工程闭环完成（tag `stage-3b-done`）；**OCR 与人工可读性未测**，`production_ready:false` |
| 3C | 题型权重与多生成器选择、风险打分（第三层）、分级淘汰、OCR/人工基线测量 | 未开始 |
| 4A | 生产 widget（零依赖原生 JS）+ 示例业务后端 + 真实浏览器闭环验收 | 工程与浏览器闭环完成（tag `stage-4a-done`）；人工项 NOT_RUN |
| 4B | 接入收尾（表单 fixture + 原生 POST）、真实窄屏视口、离线 OCR 基线、人工盲测入口 | 工程与窄屏完成；**OCR 基线已测出真实数字**（低/中/高档 96%/31%/3%）；人工可读性需真人执行 ⇒ NOT_RUN |
| 4C | 非视觉等价通道（音频等）、`fallback_base`、人工验收项（题型人工各过 5 次、辅助技术实测） | 未开始 |
| 5A | 单节点容器部署包、密钥文件持久化、SQLite 备份/保守恢复、边界与资源受限演练 | 部署包与演练完成（`make stage5a-check`）；**容器/恢复验收通过不等于抗 AI 验证通过** |
| 5B | 运行互斥（数据目录 `flock`）、恢复边界收敛、启动配方实测、部署候选清单、约 5 分钟持续演练 | 必需项（**B 路径**）全部实测通过；A 路径容器内编译与 amd64/UID 65534/公网 TLS 属**可选项**，如实保留 NOT_RUN/BLOCKED（ADR-017） |
| 5C | 管理后台页面、统计与分级淘汰、风险打分（第三层）、非视觉通道 | 未开始 |

## 阶段 1 复核（2026-10-17）

阶段 1 安全复核记录在 `docs/reviews/STAGE1-REVIEW.md`（本实现模型的自检，**不是**独立第三方审计）：
修掉 6 个真实缺陷（清理任务与外键冲突、`/siteverify` 错误码族、站点维度限流可被污染、缺少存量上限、
TTL 无上界、清理 goroutine 双关），并补齐 PoW 独立验算向量与 fail-closed 测试。

## 阶段 2 现状

`POST /api/challenge` → `POST /api/redeem` → `POST /siteverify` 已可用。防重放状态持久化在 SQLite，
进程重启后旧 token 不可重放（有测试与验收脚本证明）。**题目层见下方"阶段 3A 现状"**：默认站点仍是 `pow_only`，
只有显式把站点配成 `pow_question` 才会走题目判定。

```bash
make build          # 单二进制（实测 10.31 MiB，阶段 3B 后；预算 35MB）
make stage1-check   # 端到端验收：真实暴力求解 PoW，10 项检查
make check          # fmt-check + vet + go test -race -count=1 + 体积预算
make stage2-check   # 阶段 2 端到端验收
make stage5a-check  # 部署包 + 备份恢复 + 容器边界 + 资源演练（退出码 0/1/3）
make stage5b-check  # 运行互斥 + 恢复边界 + 启动配方 + 候选清单 + 5 分钟持续演练（结论只由必需项决定；make 只会报 0 或 2，真码看 RESULT/SCRIPT_RC）
```

启动需要两个必需的外部输入：`EPOCHA_MASTER_KEY`、`EPOCHA_PEPPER`（base64，≥32 字节，**不进仓库**）。
站点配置文件（`--sites` / `EPOCHA_SITES_FILE`）自阶段 2 起是**可选的一次性导入**入口：
库里已有同一个 `site_key` 时该条配置完全不生效——**SQLite 是权威数据源**，重启不会把旧配置覆盖回库，
已撤销的密钥与已停用的站点因此都不会"复活"（理由与代价见 ADR-010）。

```bash
make stage2-check   # 阶段 2 端到端：管理 API + 来源策略 + 轮换 + 重启语义，10 项
make stage1-check   # 阶段 1 验收仍然有效
```

管理面走**独立的监听器**（默认 `127.0.0.1:8081`，公共端口上没有管理路由）：

```bash
export EPOCHA_MASTER_KEY=$(openssl rand -base64 32)
export EPOCHA_PEPPER=$(openssl rand -base64 32)
export EPOCHA_ADMIN_KEY=ak_$(openssl rand -hex 32)   # 与发出的 Bearer 值逐字节相同；库里已有管理凭据后不再读取
bin/epocha --db /tmp/epocha.db --listen 127.0.0.1:8080 --admin-listen 127.0.0.1:8081

# 建站点（secret 只在这一次响应里出现）
curl -sX POST http://127.0.0.1:8081/api/admin/sites \
  -H "Authorization: Bearer $EPOCHA_ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"demo","site_key":"pk_demo","pow_bits":14}'
# 设置允许来源（精确匹配，无通配符）
curl -sX PUT http://127.0.0.1:8081/api/admin/sites/1/origins \
  -H "Authorization: Bearer $EPOCHA_ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"origins":["https://app.example.com"]}'
# 轮换密钥：旧密钥进入 1 小时过渡期
curl -sX POST http://127.0.0.1:8081/api/admin/sites/1/secrets/rotate \
  -H "Authorization: Bearer $EPOCHA_ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"transition_sec":3600}'
# 停用站点（未消费的 token 立即失效，重新启用也不会复活）
curl -sX POST http://127.0.0.1:8081/api/admin/sites/1/disable \
  -H "Authorization: Bearer $EPOCHA_ADMIN_KEY"
# 审计（倒序游标分页）
curl -s "http://127.0.0.1:8081/api/admin/audit?limit=20" \
  -H "Authorization: Bearer $EPOCHA_ADMIN_KEY"

# 业务服务器侧验证（hostname 由服务端裁决，自报值不采信）
curl -sX POST http://127.0.0.1:8080/siteverify \
  --data-urlencode "secret=sk_<创建时返回的值>" --data-urlencode "response=<widget 拿到的 token>"
```

阶段 2 的管理面、来源策略与密钥生命周期语义不变。

## 阶段 3A 现状

站点可以选择两种验证模式（**迁移后的既有站点默认仍是 `pow_only`，行为不变**）：

- `pow_only`：只有 PoW 一层（阶段 1/2 的契约）。
- `pow_question`：PoW + 一次性题目。`/api/challenge` 在**同一个事务**里落 challenge 行并从题池领一道题绑定
  （`pool.challenge_id` 上的 partial UNIQUE 索引保证一题一 challenge），`/api/redeem` 同时收 `pow_nonce` 与 `answer`。
  第一笔**有效**作答即终结该 challenge：答错走**已提交**的终态（不是返回 error 触发回滚），
  因此同一 token 第二次提交只会得到 `invalid-challenge`，刷不了。畸形请求与 PoW 解错**不**算作答。

`arithmetic-baseline-v1` 是**工程基线题型**：公开 payload 里的 `task` 就是可直接求值的表达式，
**答案可从题面程序化推出，它不抗自动解题**。它存在的目的是把生成器接口、题池不变量与提交生命周期跑通并钉上测试；
管理面 `/api/admin/generators` 对它返回 `production_ready:false`。不得用"随机实例多"论证它安全（DESIGN §8.4、ADR-011）。

池空时 `/api/challenge` 返回 **503 `challenge-unavailable` + `Retry-After`**，响应里没有 `pow`/`token`——
**绝不静默降级成 PoW-only**，否则站点的策略声明会在容量不足时悄悄失效。

答案明文不落库：`pool.verifier_blob = nonce(16B) || HMAC(K_qver, "epocha/v1|qver|" || nonce || 0x00 || 规范化答案)`，
其中 `K_qver` 由主密钥按 purpose 域分隔派生（与签发 token 的两把密钥互不可替代）。公开出网只走生成器的白名单字段，
私有数据与公开题面**不是同一个可序列化结构体**（不是靠 `json:"-"` 抑制）。

```bash
make stage3a-check   # 阶段 3A 端到端 10 项：真实求解基线题、错误答案终结、重复提交失败、池空 503、重启语义、按值扫描
go test -bench BenchmarkPoolFill -benchtime 1x -run '^$' ./internal/pool/   # 资源实测（数字见 docs/STAGE3A.md）
```

给站点开启题目模式（管理面，独立监听器）：

```bash
curl -sX PUT http://127.0.0.1:8081/api/admin/sites/1 \
  -H "Authorization: Bearer $EPOCHA_ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"verify_mode":"pow_question","generator_id":"arithmetic-baseline-v1","difficulty":3}'
# 来源规则不变（§10.3）：该站点仍需匹配 Origin，或用 allow_missing_origin 显式放行无来源调用（仅演示）
curl -s http://127.0.0.1:8081/api/admin/pool -H "Authorization: Bearer $EPOCHA_ADMIN_KEY"   # 只读计数，不含题面与答案
```

阶段 3A 交付时**仍然没有**：视觉/图形题型与渲染依赖、题型权重与多生成器选择、风险打分判定、自动淘汰、widget 与后台页面。
其中"视觉题型 + 图片链路 + 本地预览 + 评测脚手架"在 3B 落地，其余见下方"仍未实现"。

## 阶段 3B 现状

`text-image-v1`：6 字符、32 字形字符集（大写去易混 + 数字去 `0/1`）、灰度 PNG、内嵌 Go Regular 字体实时栅格化，
**不依赖任何外部素材或系统二进制**。图片以 BLOB 与题目同库存放（单题上限 65,536 字节 + 全局字节预算，
两条边界都在 INSERT 的 SQL 里原子判定），签发时**内联**在 `/api/challenge` 的公开 DTO 里（`image_mime` + `image_base64`），
**没有可枚举的裸媒体端点**。判分沿用 3A 的 `nonce || HMAC(K_qver, 规范化答案)`，图片渲染与判分完全解耦。

**它是实验性题型，不是"抗 OCR/抗 AI"的结论**：字符在图里是给人读的，M4（每次尝试成本）**未测**——
本机没有离线 OCR，未获授权安装或下载模型；`make ocr-baseline` 在没有工具时**打印 BLOCKED**，
真实退出码 3 只有直接跑二进制/跑脚本才拿得到（`go run` 折叠成 1、`make` 折叠成 2）。
人工可读性同样**未测**（盲测页面已交付，但没有真实参与者数据）。因此管理面对它报
`experimental:true` / `production_ready:false`，且未经评测不得翻转。

一条命令起本地预览（需服务已在跑，浏览器里自己算 PoW 并走完签发→作答→`/siteverify`）：

```bash
go run ./cmd/preview --api http://127.0.0.1:8080 --admin http://127.0.0.1:8081 \
  --admin-key "$EPOCHA_ADMIN_KEY" --difficulty 2 --pow-bits 12
# → 用浏览器打开 http://127.0.0.1:8090/
```

它强制 loopback（`--listen 0.0.0.0:…` 直接拒绝启动）；站点 secret 只在演示后端进程内，不进 HTML/JS/响应；
浏览器不能指定验证目标（`/demo/verify` 只接受 `response` 一个字段）。**这不是正式 widget**（阶段 4）。

```bash
make stage3b-check   # 21 项工程检查 + 1 BLOCKED + 1 NOT_RUN（含 make check 与阶段 1/2/3A 全部验收）
make eval-samples    # 离线评测样本：dev 调参集 + 固定 seed 的 holdout 验收集（输出 eval-out/，不入库）
```

**3B 之后仍未实现**：题型权重与多生成器选择、风险打分（第三层）、分级淘汰与统计面、OCR 与人工基线测量。
`make stage3b-check` 全绿只代表工程闭环，不代表效果结论。

## 阶段 4A / 4B 现状（widget + 业务接入 + 真实窄屏 + OCR 基线）

`web/widget/v1.js` 是零依赖原生 JS 组件（`EpoCHA.render/getResponse/reset/refresh/destroy`，中英双语，
Shadow DOM 直挂**不用 iframe**）。PoW 在 **Dedicated Worker** 里算：默认用内嵌的同一份源码建 Blob Worker
（需要 `worker-src blob:`），也支持接入方同源部署 `v1.worker.js` 并用 `workerUrl` 指定；
CSP 挡住 Worker 时给明确错误，**不**回落到主线程假装算完，也**不**假装验证完成。
服务端脚本 `GET /widget/v1.js` 由 `cmd/server` 内嵌提供（实测 gzip 17,704 B，预算 30KB）。

```bash
make stage4b-check   # 阶段 4B 工程项 + 一次 stage4a 全套（回归与浏览器套件各只跑一遍）
make stage4a-check   # 4A 工程项 + 21 条真实浏览器交互（0 skip）+ 全部阶段回归
make widget-checks   # JS 语法 + JS↔Go 共享 PoW 固定向量（零第三方依赖）
```

一条命令起一个**真实业务形状**的示例（widget 跨域加载、后端持 secret、只有 `/siteverify` 成功才写留言）：

```bash
go run ./cmd/exampleapp --listen 127.0.0.1:8091 --api http://127.0.0.1:8080   --site-key pk_demo4a --site-secret '<建站时返回一次的 secret>' --csp blob
# → 浏览器打开 http://127.0.0.1:8091/
# → 文档 §1 的最小接入示例本身在 http://127.0.0.1:8091/fixture（容器在 form 内 + 原生表单 POST）
```

它故意让「提交留言」按钮一直可点：不带 token 也能提交，服务端必然拒绝且零写入 ——
**前端的 `onSuccess` 不是授权**。同一 token 再提交一次必然被拒（一次性消费）；
验证服务宕机/超时/5xx 一律 fail closed；页面里没有任何字段能指定验证目标。
接入契约、CSP 清单与无障碍边界写在 `docs/WIDGET.md`；浏览器侧到底测了什么写在 `docs/STAGE4A.md`，
4B 补的窄屏、表单接入、OCR 基线与盲测入口写在 `docs/STAGE4B.md`。

### 4B 新增的三件实事

1. **窄屏不再是不成立的 BLOCKED**。`browsingContext.setViewport`（W3C 规范命令）在 Firefox 147.0.2
   上真实生效，375×667 与 414×896 是**执行并断言**的用例：先断言 `innerWidth`/`clientWidth` 等于目标值，
   再量溢出、图片比例、控件在 shadow DOM 内的可命中性，并在 375 下真点真提交。
   这仍**不等于**真机移动端测试（DPR 是被设进去的）。
2. **离线 OCR 基线第一次有数字**（隔离容器里的 tesseract 5.3.4 + eng 4.1.0，识别阶段断网、
   只读挂载、答案文件从不进容器）。固定最强配置后 holdout 各 100 张的整串严格通过率：
   **低档 96%、中档 31%、高档 3%**（分母含工具失败与畸形输出）。
   结论方向要读对：**低档基本挡不住通用 OCR**，所以生产默认不能落在低档；
   高档数字好看也**不**改变 `production_ready:false`。详见 `docs/STAGE4B.md` §4。
3. **人工可读性有了稳定入口**：`make blindtest` 一条命令拉起临时服务与三档盲测页面，
   答题前不显示答案、不记录输入、导出无凭据的 CSV/JSON，并把"答题/PoW/兑换"三段耗时分开
   （兑换那段无法再拆成 RTT 与服务端时间，文档里如实标注）。真人做题这件事 agent 不代填。

**4B 之后仍未实现**：非视觉等价通道（音频等）、`fallback_base`、多题型权重与风险打分、
完整管理后台、人工验收项（题型人工各过 5 次、辅助技术实测）。因此**不打** `stage-4-done`。
