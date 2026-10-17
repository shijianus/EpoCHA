# epocha

自托管人机验证服务：单 Go 二进制 + SQLite，调用契约与 reCAPTCHA 的 `POST /siteverify` 兼容。
核心防线是**生成式题池**（程序化生成、随机变换、一次性使用、用后即焚），外层是 PoW 成本门槛，最内层是风险打分决策。

- 设计与决策：`docs/DESIGN.md`（v2.1）；修订理由与历史错误表述：`docs/DECISIONS.md`
- 工程约定与安全红线：`AGENTS.md`
- 命令：`make help`

## 状态

| 阶段 | 内容 | 状态 |
|---|---|---|
| 0 | 设计基线 + 目录骨架 + 测试入口 | 完成（tag `stage-0-done`） |
| — | 设计校正 v2（ADR-001…008，见 `docs/DECISIONS.md`） | 完成 |
| 1 | PoW + token + 持久化防重放 + `/siteverify` 最小闭环 | 完成（tag `stage-1-done`） |
| 2 | 多站点与 Key 管理 API、来源白名单、站点级配置、审计 | 完成（tag `stage-2-done`） |
| 3 | 生成器契约 + `arith-v1`/`text-warp-v1` + 题池 + 风险打分 | 未开始 |
| 4 | 前端 widget（零依赖，gzip <30KB） | 未开始 |
| 5 | 管理后台、分级淘汰、Docker 与部署文档（CF 边缘为之后评估项） | 未开始 |

## 阶段 1 复核（2026-10-17）

阶段 1 安全复核记录在 `docs/reviews/STAGE1-REVIEW.md`（本实现模型的自检，**不是**独立第三方审计）：
修掉 6 个真实缺陷（清理任务与外键冲突、`/siteverify` 错误码族、站点维度限流可被污染、缺少存量上限、
TTL 无上界、清理 goroutine 双关），并补齐 PoW 独立验算向量与 fail-closed 测试。

## 阶段 2 现状

`POST /api/challenge` → `POST /api/redeem` → `POST /siteverify` 已可用，PoW 为唯一防线（尚无题目层与风险打分，
响应里不伪装它们存在）。防重放状态持久化在 SQLite，进程重启后旧 token 不可重放（有测试与验收脚本证明）。

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

阶段 2 **仍然只有 PoW 一层**：没有生成式题库、没有风险打分判定、没有 widget 与后台页面。
