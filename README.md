# epocha

自托管人机验证服务：单 Go 二进制 + SQLite，调用契约与 reCAPTCHA 的 `POST /siteverify` 兼容。
核心防线是**生成式题池**（程序化生成、随机变换、一次性使用、用后即焚），外层是 PoW 成本门槛，最内层是风险打分决策。

- 设计与决策：`docs/DESIGN.md`（阶段 0 已定稿）
- 工程约定与安全红线：`AGENTS.md`
- 命令：`make help`

## 状态

| 阶段 | 内容 | 状态 |
|---|---|---|
| 0 | 设计基线 + 目录骨架 + 测试入口 | 完成（tag `stage-0-done`） |
| 1 | PoW + token + `/siteverify` 最小闭环 | 未开始 |
| 2 | 站点与 Key 管理、限流、SQLite 迁移 | 未开始 |
| 3 | 生成器接口 + `arith-v1`/`text-warp-v1` + 题池 | 未开始 |
| 4 | 前端 widget（零依赖，gzip <30KB） | 未开始 |
| 5 | 管理后台、自动淘汰、CF 边缘签发、Docker 与部署文档 | 未开始 |

当前 `make build` 产出可执行文件，`--version` 可用；启动服务会明确报"尚未实现"，不提供任何可用的验证端点。
