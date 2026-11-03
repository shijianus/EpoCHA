# RELEASE-FREEZE-2026-11-02

状态：**可靠性修复冻结快照；不是正式发布候选，不是上线批准。**

## 本快照

- 基线提交：`de65ec72696102c26bc1360a3203b50600e54d5e`
- 工作分支：`fix/release-preparation-2026-11-02`
- 工作树：dirty；本轮没有 commit、push、tag，也没有移动历史标签。源码与新增检查已在 `release-artifacts/full-SB7ig0/summary.json` 的 snapshot 中记录；完整收据仍绑定基线提交 + dirty snapshot，不能当正式候选。
- 工具链：官方 Go 1.27.3 Windows amd64 归档已由官方 JSON 清单核验，SHA-256 `f26081f438319757f8a1b46d7d0b2c5ec7583d4038e485e0aa725cfde7885fff`；安装在用户目录，旧系统 Go 与全局 PATH 未改。
- 冻结范围：凭据撤销/轮换、widget 异步生命周期、部署密钥配方、readyz 文案、LF/发布检查与 CI 定义。
- 明确不在范围：风险评分、多题型权重、音频/fallback、多实例、Cloudflare 边缘、公网 TLS、人工可读性、辅助技术、抗 OCR/抗 AI 认证。

## 代码与回归结果

### 凭据生命周期

- 修复仍在过渡期旧凭据未被立即轮换覆盖的问题。
- 修复 site secret/admin key 重复 revoke 延长截止时间、复活失效凭据的问题。
- grace 凭据纳入同时有效上限；撤销/新建在同一事务中完成，满额立即轮换释放旧槽位，grace 轮换失败回滚。
- 修复前定向红测：8 个顶层测试失败，15 个叶子失败。
- 修复后同一 store 定向命令：8 个顶层、17 个叶子全部通过，退出码 0，真实 `modernc.org/sqlite`，仅通过仓库外 overlay 排除 Windows 不支持的 `lock.go`。
- 管理 HTTP 生命周期回归：3/3 通过，退出码 0；覆盖 grace→immediate、重复 revoke site/admin、grace 容量。
- 既有 API 安全回归：`TestRevokedSecretDoesNotConsumeAtAPILayer` 与 `TestWrongSecretAcrossSitesDoesNotConsume` 通过，退出码 0。
- 独立只读复核：未发现事务/截止时间/容量修复的合并阻断问题。

### widget

- `reset/load/destroy/refresh` 先使旧世代失效，再取消请求、worker、计时器和重试。
- challenge/redeem 使用有界超时；超时覆盖响应体读取，迟到成功不能重新填 token。
- 旧请求不能清理新请求的计时器；同步 `onState` 回调内 reset/destroy 不会让原调用继续执行。
- 修复前生命周期红测：15 项中 14 项失败。
- 修复后 lifecycle + PoW 向量：26/26 PASS、0 FAIL、0 SKIP；Node v24.14.0，退出码 0。
- JavaScript 语法、Shell 语法、工程规则均通过；widget gzip 18,693 B，低于 30,720 B。
- 独立只读复核：未发现可证实的合并阻断问题。
- 真实 IAB 页面已加载真实 HTTP 题型和图片，进入 `ready` 的一次观察通过；IAB 对 Shadow DOM 控件定位/截图出现超时，完整真实浏览器用例未计为 PASS。新增 `widget_browser.test.mjs` 的浏览器用例仍需 Firefox BiDi 环境执行。

### 部署与工程

- Go 模块、Dockerfile 和文档固定 Go 1.27.3，并保留 `GOTOOLCHAIN=local`。
- `deploy/prepare-keys.sh` 只允许 Linux 非 root 服务身份；0600、UID/GID 校验、排他创建、重跑不覆盖。Windows 自测按设计返回 BLOCKED/3，不冒充权限验收。
- `TRIAL-DEPLOYMENT.md` 禁止旧 `stage-5b-done` 作为本轮候选，要求真实 40 位 OID + 匹配 manifest；当前未填造 APPROVED_OID。
- `.gitattributes` 固定文本 LF，二进制不做换行转换；已有 Windows checkout 已在本轮归一化，Node 向量副本断言现已通过。
- `.dockerignore` 排除开发归档、评测/负载工具和 release-artifacts；生产运行镜像内容边界未放宽。
- `.github/workflows/ci.yml` 已加入 Go 1.27.3、Node 24、Linux arm64/amd64 工程检查与待配置的专用 Linux arm64 full acceptance。CI 文件存在不等于远端已运行。

## 本轮真实命令收据

| 检查 | 结果 |
|---|---|
| `node --test docs/scripts/release-check.test.mjs docs/scripts/browser/pow_vectors.test.mjs docs/scripts/browser/widget_lifecycle.test.mjs` | **43 PASS / 0 FAIL / 0 SKIP** |
| Go 1.27.3 `gofmt` | 83 个 Go 文件检查通过 |
| Linux arm64 交叉构建 | PASS；服务 ELF 静态，11,534,496 B；ops ELF 静态，10,289,312 B |
| Linux arm64 交付二进制 SHA-256 | `bf6f0272d368920d1b452d88705ce99f9920ee6a9f9c9f74ddab1be41e0dc08d` / `589733fd03015fe2e52b6a1496d4b22b9555ef6c5e7bc44c73bd73c52695cb3a` |
| 完整 `release-check --profile full` | **INCOMPLETE / 真码 3**；最新收据 `release-artifacts/full-SB7ig0/summary.json` |
| 工程 `release-check --profile engineering` | **INCOMPLETE / 真码 3**；`release-artifacts/engineering-final/summary.json` |
| stage5b 分类负向自测 | 9/9 PASS（此前已有脚本自测） |
| `git diff --check` | PASS |
| `go mod verify` | PASS |

## 阻塞项与上线边界

完整收据明确记录：

- Windows 原生环境没有 Linux `flock`；Docker 后端报告 `hasNoVirtualization=true`，Windows `HypervisorPresent=False`，WSL 有内核但没有发行版。
- Linux 全量 `go test -race -count=1 ./...`、Linux `go vet`、真实 Linux `make`、真实 Linux 非 root 密钥权限/重跑测试、stage4b/stage5a/stage5b/admin-ui 全量部署验收均 **BLOCKED**。
- 当前工作树 dirty 且没有正式提交；`candidate-clean` BLOCKED。`de65ec7` 仅是修复前基线，不能把当前修改称为已发布候选。
- `release_ready:false`、`deployed:false` 是机器收据中的事实。
- 图片题继续保持 `experimental:true` / `production_ready:false`。历史 OCR 数字和本轮工程通过都不能证明抗 AI、抗 OCR、人工可读性或生产效果。
- 未执行 push、tag、公网端口、DNS、证书、远端部署或真实 VPS 试部署。

## 解锁正式交付的最小条件

在一台隔离、授权的 Linux arm64 环境，以正式支持的 UID/GID `1000:1000` 执行：

1. 将所有修改提交为干净 40 位 OID；生成与提交、Go 1.27.3、构建命令、两份 SHA-256 和验收日志匹配的 manifest。
2. 运行 `node docs/scripts/release-check.mjs --profile full`，必须得到 `RESULT=PASS SCRIPT_RC=0`；任何 BLOCKED/NOT_RUN 都继续阻止候选。
3. 实跑 `go test -race -count=1 ./...`、vet、静态 B 路径构建、secret-scan、真实 Firefox 浏览器交互、stage4b/stage5a/stage5b/admin-ui；不使用 overlay 代替完整包测试。
4. 在空的专用 Docker daemon 上按 DEPLOYMENT §11 做启动、readyz、真实闭环、重放拒绝、重建容器状态保持、停止释放端口、在线备份和隔离恢复演练。
5. 由有权限的人确认目标架构、UID/GID、TLS/DNS、备份节律、监控和人工/辅助技术验收，再决定是否发布；本快照本身不提供这些授权。

**最终判断：本轮已收敛为可追踪的可靠性修复冻结快照，工程和定向回归证据充分；但由于正式 Linux/race/容器部署验收和干净候选提交尚未完成，当前不能称为“达到上线要求”或交付正式部署。**
