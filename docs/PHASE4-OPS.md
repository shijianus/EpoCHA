# Phase 4 运维与证据入口（ADR-038）

生产入口 `deploy/docker/compose.yml` 固定 1000:1000，`EPOCHA_IMAGE` 无可变默认值，
`.env.example` 的 digest 是必须替换的占位符。`EPOCHA_PRODUCTION_PROFILE` 默认 true。
当前全部 19 型仍不具备生产资格，因此生产启动明确失败；工程测试通过不改变该事实。

`deploy/preflight.sh --strict --compose-file /srv/epocha/compose.yml --env-file /srv/epocha/.env`
只读解析配置；可追加 `--require-container NAME_OR_ID` 校对运行状态、实际镜像 ID、镜像引用、
1000:1000、生产开关及 Compose 项目/服务标签。需要已有 Docker Compose major >=2 与 jq，
不安装、不拉取、不启动服务，不输出完整配置、环境或凭据。普通报告 RC=0；严格阻断 RC=3；
用法错误 RC=2。JSON 的 `strict_failures` 列明阻断，`deployment_valid` 只代表部署字段校验，
不能取代 ARM64、主机与生产批准门槛。运行实例占用了严格要求空闲的端口仍会阻断；本轮不为
已有监听者增加放行例外。

开发演练必须显式追加 `-f deploy/docker/compose.test.yml`；它只用于独立目录与项目，关闭
生产开关，要求唯一 `EPOCHA_DRILL_OWNER`。stage5a/5b 自动生成每轮前缀，也可分别配置
`EPOCHA_5A_PROJECT` / `EPOCHA_5B_PROJECT`。stage5b 的 second、measure、recipe 项目/网络
均派生自前缀；`EPOCHA_5B_PORT`、`_SECOND_PORT`、`_MEASURE_PORT`、`_RECIPE_PORT` 可配置，
子网为 `EPOCHA_5B_SUBNET` / `EPOCHA_5B_RECIPE_SUBNET`。端口已占或资源同名一律拒绝。
退出仅清本轮捕获且所有权标签仍匹配的资源 ID，不删除镜像、不执行 prune 或全项目 down。

`deploy/systemd/` 提供备份、就绪探测两个 service/timer。它们面向宿主上的 ops 二进制和
明确的目录/loopback URL，**不是**对任意既有 Docker 实例的自动运维。部署者需先调整路径与
健康 URL，并实现 `epocha-alert@.service` 告警模板后审核安装。备份使用已有在线 VACUUM INTO
路径，不停服、不取运行锁；探测失败只触发 OnFailure，不重启或终止服务。本轮不安装/启用
systemd 单元，不自动发告警。

`node docs/scripts/node-tests.mjs <显式 .test.mjs 列表>` 先检查全部文件存在，再调用 Node。
管理台生命周期文件唯一入口为 `docs/scripts/browser/adminui_lifecycle.test.mjs`，该文件是
不需要浏览器的 VM 回归。真实 Firefox 两套 `*_browser.test.mjs` 不计入非浏览器测试。

release-check 的 `summary.json.commands` 保存所有直接子命令；各检查用 `command_ids`
关联。每条命令有 `started_at` / `finished_at` UTC、`duration_ms`、`raw_exit_code`、signal/error，
以及 `command-NNNN.stdout.log` / `.stderr.log` 和 SHA-256。对应 JSON 收据单独落盘，启动失败
也保留记录。脚本退出打印自身实际 `SCRIPT_RC`；make 失败仍折叠为 2。

engineering 仅验工程条件；full 继续要求正式 ARM64、专用空 Docker、Firefox 和人工证据。
生产批准单独默认 false；不得把 engineering PASS 或模拟 Preflight 通过当作部署、ARM64
原生、真实浏览器、真机、人工可读性或抗 OCR/AI 的通过证据。
