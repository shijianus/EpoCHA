# epocha

自托管人机验证服务：单 Go 二进制 + SQLite，调用契约与 reCAPTCHA 的 `POST /siteverify` 兼容。
核心防线是**生成式题池**（程序化生成、随机变换、一次性使用、用后即焚），外层是 PoW 成本门槛，最内层是风险打分决策。

- 设计与决策：`docs/DESIGN.md`（v2.5）；修订理由与历史错误表述：`docs/DECISIONS.md`
- 工程约定与安全红线：`AGENTS.md`
- 阶段 3A 现状（状态转换表、水位默认值、资源实测与未测项）：`docs/STAGE3A.md`
- 命令：`make help`

## 状态

| 阶段 | 内容 | 状态 |
|---|---|---|
| 0 | 设计基线 + 目录骨架 + 测试入口 | 完成（tag `stage-0-done`） |
| — | 设计校正 v2（ADR-001…008，见 `docs/DECISIONS.md`） | 完成 |
| 1 | PoW + token + 持久化防重放 + `/siteverify` 最小闭环 | 完成（tag `stage-1-done`） |
| 2 | 多站点与 Key 管理 API、来源白名单、站点级配置、审计 | 完成（tag `stage-2-done`） |
| 3A | 生成器契约 + 有界预生成题池 + 原子领题 + 单次答案提交 + `arithmetic-baseline-v1` | 完成（tag `stage-3a-done`） |
| 3B | 视觉题型与渲染、题型权重、反自动解题基线测量、风险打分 | 未开始 |
| 4 | 前端 widget（零依赖，gzip <30KB） | 未开始 |
| 5 | 管理后台、分级淘汰、Docker 与部署文档（CF 边缘为之后评估项） | 未开始 |

## 阶段 1 复核（2026-10-17）

阶段 1 安全复核记录在 `docs/reviews/STAGE1-REVIEW.md`（本实现模型的自检，**不是**独立第三方审计）：
修掉 6 个真实缺陷（清理任务与外键冲突、`/siteverify` 错误码族、站点维度限流可被污染、缺少存量上限、
TTL 无上界、清理 goroutine 双关），并补齐 PoW 独立验算向量与 fail-closed 测试。

## 阶段 2 现状

`POST /api/challenge` → `POST /api/redeem` → `POST /siteverify` 已可用。防重放状态持久化在 SQLite，
进程重启后旧 token 不可重放（有测试与验收脚本证明）。**题目层见下方"阶段 3A 现状"**：默认站点仍是 `pow_only`，
只有显式把站点配成 `pow_question` 才会走题目判定。

```bash
make build          # 单二进制（实测 9.19MB）
make stage1-check   # 端到端验收：真实暴力求解 PoW，10 项检查
make check          # fmt-check + vet + go test -race -count=1 + 体积预算
make stage2-check   # 阶段 2 端到端验收
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

阶段 3A **仍然没有**：视觉/图形题型与渲染依赖、题型权重与多生成器选择、风险打分判定、自动淘汰、widget 与后台页面。
