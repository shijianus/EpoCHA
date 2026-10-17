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
| 2 | 站点与 Key 管理 API、域名白名单、限流配置、审计 | 未开始 |
| 3 | 生成器契约 + `arith-v1`/`text-warp-v1` + 题池 + 风险打分 | 未开始 |
| 4 | 前端 widget（零依赖，gzip <30KB） | 未开始 |
| 5 | 管理后台、分级淘汰、Docker 与部署文档（CF 边缘为之后评估项） | 未开始 |

## 阶段 1 现状

`POST /api/challenge` → `POST /api/redeem` → `POST /siteverify` 已可用，PoW 为唯一防线（尚无题目层与风险打分，
响应里不伪装它们存在）。防重放状态持久化在 SQLite，进程重启后旧 token 不可重放（有测试与验收脚本证明）。

```bash
make build          # 单二进制（实测 9.19MB）
make stage1-check   # 端到端验收：真实暴力求解 PoW，10 项检查
make check          # fmt-check + vet + go test -race -count=1 + 体积预算
```

启动需要三个外部输入：`EPOCHA_MASTER_KEY`、`EPOCHA_PEPPER`（base64，≥32 字节，不进仓库）与站点配置文件
（见 `.env.example` 与 `docs/scripts/stage1.sh` 的用法示例）。`/api/admin/*` 与域名白名单属阶段 2。
