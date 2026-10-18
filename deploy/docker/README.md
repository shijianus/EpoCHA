# deploy/docker

阶段 5A 的单节点容器部署包。**怎么用、边界在哪、测过什么**都在
[`docs/DEPLOYMENT.md`](../../docs/DEPLOYMENT.md)；备份与恢复在
[`docs/BACKUP-RESTORE.md`](../../docs/BACKUP-RESTORE.md)；本轮验收状态在
[`docs/STAGE5A.md`](../../docs/STAGE5A.md)。本文件只做目录索引，不重复那些内容——
两处各写一份就会有两处不同步。

| 文件 | 职责 |
|---|---|
| `Dockerfile` | 两阶段：`toolchain` 编译（`golang:1.24-alpine`，`CGO_ENABLED=0`、`GOTOOLCHAIN=local`）+ `runtime`（`FROM scratch` + 两把二进制）。`USER` 是数字 uid；`HEALTHCHECK` 用镜像内的 `epocha-ops check --path /readyz` |
| `compose.yml` | 单副本；只发布 `127.0.0.1:<端口>:8080`；`read_only` + `cap_drop ALL` + `no-new-privileges` + `pids` + `memory`/`memswap_limit` 硬限额；三个分开挂载的目录（数据 / 密钥 `:ro` / 备份）；日志 `json-file` 轮转；`stop_grace_period 15s` |
| `.env.example` | compose 变量样例（**不含任何密钥**）。复制为 `.env` 使用 |
| `nginx.conf.example` | 同机反代主示例：不回源管理面、动态端点 `no-store`、widget 透传后端缓存语义、`client_max_body_size 8k`、访问日志不记 query/体、默认不转发 `CF-Connecting-IP` |
| `../cloudflare/` | 阶段 5 之后的评估项占位，本轮未实施（ADR-001） |

## 这一版有意不做的东西

- 不做多副本、不做编排平台、不做主机网络模式：SQLite 是本地单文件权威状态，
  副本 >1 直接破坏一次性消费语义。
- 不发布管理端口（8081）。管理操作只有一条被测试覆盖的路径：容器内 `epocha-ops admin`。
- 不在镜像里放 shell、CA 证书、`sqlite3` 或任何包管理器：交付面里没有可执行任意命令的东西，
  运维能力就是 `epocha-ops` 的子命令集合。
- 不做跨架构构建体系：本轮只实测 `linux/arm64`，x86-64 是支持意图而非实测结论。
- 不签 TLS 证书：反代模板用占位路径，本轮只测路由与响应头形状。

`stage5a.sh` 会构建镜像并真的起容器演练；退出码 0 只在这些演练全部通过时给出，
外部条件不足时是 **BLOCKED**（退出码 3），不是通过。
