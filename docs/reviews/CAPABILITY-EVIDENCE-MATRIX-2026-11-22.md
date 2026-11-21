# 能力 / 证据矩阵（2026-11-22，对应分支 `fix/acceptance-auth-deploy-20261120`）

本文件是单一事实表：每行一个能力，标注实现状态、最后实测 OID/环境、证据位置与未测边界。
状态口径遵守 AGENTS 三态：工程项 PASS/FAIL；外部条件不足 BLOCKED；本轮未执行 NOT_RUN。
"最后实测 OID"是**该行证据绑定的源码**，不代表当前 HEAD 自动继承；冲突时以 DECISIONS 的 ADR 为准。

| 能力 | 实现状态 | 最后实测 OID / 环境 | 证据 | 未测边界 / 下一动作 |
|---|---|---|---|---|
| PoW + token + `/siteverify` 主闭环 | 实现完成 | `3ed3a15` Linux amd64（full 内 stage1..3b） | `docs/reviews/baseline-evidence/2026-11-19/`（full-publish-final-summary.json） | 正式 arm64 容器闭环 NOT_RUN |
| 管理面站点/凭据生命周期 | 实现完成 | `3ed3a15` Linux amd64（admin-ui 10/10 浏览器 + shell 写路径） | 同上 full 收据 | 真机移动端/剪贴板/403 文案 NOT_RUN |
| 在途管理写的事务授权（本批新增） | 实现完成 | 本分支 `6d53e05+` Windows 诊断 overlay（五包全绿） | `internal/admin/audit_c04_c08_regression_test.go`、`channel_reachability_regression_test.go` | Linux 原生 + race 待正式验收（overlay 不替代） |
| siteverify 线性化 = 消费事务（本批新增） | 实现完成 | 同上 | `internal/api/pool_lifetime_regression_test.go`、`stage2_test.go` 两方向用例 | race/并发正式验收 NOT_RUN（本地） |
| 候选预检寿命边界 = 领取边界（本批新增） | 实现完成 | 同上 | `internal/api/pool_lifetime_regression_test.go`（红绿对照已存档于提交信息） | 长期流量形状未测 |
| 管理台无隐式默认题型（本批新增） | 实现完成 | 本分支 Node DOM 仿真 + admin HTTP | `docs/scripts/browser/adminui_generator_default.test.mjs`（真实 19 型目录） | UI→公开闭环浏览器 E2E NOT_RUN（Firefox BiDi 在 Linux 验收机） |
| stage5a 资源所有权清理（本批新增） | 实现完成（脚本逻辑 + 假 Docker 四情景） | 本分支（bash 自测 + Node 5/5） | `docs/scripts/stage5a.sh --selftest-resource-ownership`、`stage5a_rules.test.mjs` | 真实 Docker 演练 NOT_RUN（本机无 daemon；专属 arm64 环境执行） |
| stage4b 隔离检查点判定 | 实现完成（判定器 + Node 2/2） | 本分支 | `docs/scripts/stage4b-isolation.mjs/.test.mjs` | 真实 OCR 容器隔离实证 NOT_RUN |
| 发布判决 schema 2（工程/生产分离） | 实现完成 | 本分支 | `release-check.mjs` + `release-check.test.mjs`（production approval 恒 false） | 九项准入证据全部未产生（NOT_RUN） |
| Gate B 七型 M4 | **历史 FAIL，未复测** | `b550698`（2026-11-12）/ `b550698`（2026-11-14 复核） | `docs/reviews/CAPTCHA-M4-BASELINE-2026-11-12.md`、`audit-evidence/2026-11-14/m4-replication/` | 当前源码复测 NOT_RUN；重测须先冻结 solver/参数/holdout（ADR-032 §7） |
| 人工可读性 | NOT_RUN（无真人数据） | — | — | 盲测入口已交付（`make blindtest`），待执行 |
| 辅助技术 / 真机移动端 | NOT_RUN | — | — | 需真实设备；窄屏 headless 视口已测不等于真机 |
| 抗 OCR / 抗多模态 | 历史测量仅覆盖固定 tesseract 配置（v1 d1=96%/d3=31%/d5=3%） | `b550698` 容器内 | `docs/STAGE4B.md`、eval-out 收据 | 不等于抗多模态；低档挡不住通用 OCR ⇒ 生产默认不得落低档 |
| SQLite 耐久性（WAL+NORMAL） | 实现现状；**B-03 掉电阻断未解决** | — | ADR-022/032/035（草案） | 决策稿见 ADR-035；NORMAL vs FULL 基准未执行 |
| 单节点容器部署（B 路径） | 5A/5B 已交付并历史验收 | `3ed3a15` 前的历史标签 `stage-5a-done`/`stage-5b-done` | 历史 full 收据 | 本轮脚本改动后需重跑 stage5a/5b 容器演练（NOT_RUN） |

## 明确不作数的东西

- 哈希/收据是字节追溯，不是签名或执行者身份认证。
- Windows LockFileEx 诊断 overlay 的锁测试是平台等价诊断，不是 Linux flock 验收。
- 工程测试 PASS 不推出 `release_ready` / 生产批准（schema 2 的 `production_approval` 恒 false 直到九项准入全部候选绑定 PASS 并有真实权威决定）。
- Gate B 历史 FAIL 不因工程绿自动消除；题型全部保持 `experimental:true` / `production_ready:false`。
