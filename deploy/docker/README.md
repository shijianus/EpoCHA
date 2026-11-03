# deploy/docker

阶段 5A 交付、5B 收敛的单节点容器部署包。**怎么用、边界在哪、测过什么**都在
[`docs/DEPLOYMENT.md`](../../docs/DEPLOYMENT.md)；备份与完整隔离目录接管在
[`docs/BACKUP-RESTORE.md`](../../docs/BACKUP-RESTORE.md)。
[`docs/STAGE5A.md`](../../docs/STAGE5A.md) / [`docs/STAGE5B.md`](../../docs/STAGE5B.md) 是历史验收，
不替代 Go 1.27.3 当前候选的运行证据。2026-11-02 本轮 Linux 容器引擎受阻，目录接管与反代日志动态验收 **NOT_RUN**。
原功能缺口仍见 [`部署审计` §6](../../docs/reviews/DEPLOYMENT-READINESS-AUDIT-2026-11-02.md)，不因冻结而记为完成。

| 文件 | 职责 |
|---|---|
| `Dockerfile` | 两阶段：`toolchain` 编译（`golang:1.27.3-alpine`，`CGO_ENABLED=0`、`GOTOOLCHAIN=local`）+ `runtime`（`FROM scratch` + 两把二进制）。正式支持 B 路径以宿主已编译产物替换 `toolchain`；容器内编译 A 路径保持可选 NOT_RUN。`USER` 是数字 uid，`HEALTHCHECK` 用 `epocha-ops check --path /readyz` |
| `compose.yml` | 单副本；只发布 `127.0.0.1:<端口>:8080`；`read_only` + `cap_drop ALL` + `no-new-privileges` + `pids` + `memory`/`memswap_limit` 硬限额；三个分开挂载的目录（数据 / 密钥 `:ro` / 备份）；日志 `json-file` 轮转；`stop_grace_period 15s` |
| `.env.example` | compose 变量样例（**不含任何密钥**）。复制为 `.env` 使用 |
| `nginx.conf.example` | 同机反代主示例：不回源管理面、动态端点 `no-store`、widget 透传后端缓存语义、`client_max_body_size 8k`；80/443 显式使用无 query/体/任意请求头的访问日志并丢弃可能含 query 的原生错误日志；默认不转发 `CF-Connecting-IP` |
| `../cloudflare/` | 阶段 5 之后的评估项占位，本轮未实施（ADR-001） |

## 这一版有意不做的东西

- 不做多副本、不做编排平台、不做主机网络模式：这是本项目的实现与运维支持边界，不是 SQLite 不支持多进程事务。
  一次性消费来自原子条件更新、同事务提交与 `RowsAffected == 1`（ADR-004）；共享同一库时该保证不因进程数失效。
  数据目录运行锁仍限制单实例，不开放多副本，也不允许独立账本各自接受同一 token。
- 不发布管理端口（8081）。管理操作只有一条被测试覆盖的路径：容器内 `epocha-ops admin`。
- 不在镜像里放 shell、CA 证书、`sqlite3` 或任何包管理器：交付面里没有可执行任意命令的东西，
  运维能力就是 `epocha-ops` 的子命令集合。
- 正式范围仍仅 B 路径 / `linux/arm64` / 历史实测 `1000:1000`（ADR-017）。amd64 工程测试与交叉构建不扩展支持范围。
- 不签 TLS 证书：反代模板用占位路径，历史自签证书演练不等于公网 TLS；本轮没有重跑真实 Nginx。

`stage5a.sh` 会构建镜像并真的起容器演练；退出码 0 只在这些演练全部通过时给出，
外部条件不足时是 **BLOCKED**（退出码 3），不是通过。
