# 公测与生产门禁报告

日期：2026-11-16
分支：`production-hardening-20261116`
基线：`sync/visual-bank-20261116`，`6fc8c657fbd5f553a44216dc786a2a01285fee36`

## 本轮实际修改

- 修正文档分支标识和 Node 回归数量，避免把 58 项结果写成旧的 50 或 57 项。
- 在 `symbol-path` 渲染入口增加粗指针密集布局保护。6×6 路径题在触屏环境下直接 fail closed，不渲染不可操作的微小命中盒。
- 让路径渲染器明确返回成功状态。渲染失败不会继续进入 `ready`，避免半成品题面被提交。
- 增加粗指针路径题回归测试。

## 已执行门禁

| 门禁 | 结果 |
|---|---|
| PoW 向量与 widget 生命周期 | 58/58 PASS |
| JavaScript 语法检查 | PASS |
| `git diff --check` | PASS |
| widget gzip | core 29,698B，interactive 10,364B，合计 40,062B，均在预算内 |
| `release-check --profile engineering` | INCOMPLETE / 3 |
| `release-check --profile full` | INCOMPLETE / 3 |

## 当前阻断

- 环境没有可用的 Go 1.27.3、`gofmt`、`go vet`、全量测试和 race 工具链。
- Linux/arm64、UID:GID 1000:1000、专用 Docker、stage4b、stage5a、stage5b 和 admin-ui 未完成。
- 真实 Firefox、真机移动端、辅助技术、人工可读性和 M4 评估未完成。
- `symbol-path` 的桌面布局仍然是 6×6 密集题型。粗指针路径现在会安全拒绝，低密度触屏变体仍未实现。
- `shell-track` 仍需逐帧截图、帧位置、动画结束时刻和跨浏览器验证。
- `docs/QUESTION-TYPES.md` 中题型仍保持 `experimental:true` 与 `production_ready:false`。

## 放行结论

当前分支可以作为工程验证版本和受控内部预览候选。完整发布门禁仍为 `INCOMPLETE / 3`，不满足正式公测或生产放行条件。

获得 Go 1.27.3 和 Linux 验收环境后，必须先执行：

```bash
gofmt -w internal/dispatch/dispatch.go
go test ./...
go test -race ./...
node docs/scripts/release-check.mjs --profile full
```

只有 full profile 为 `RESULT=PASS SCRIPT_RC=0`，并补齐人工、真机、辅助技术、M4、逐帧和跨浏览器证据后，才允许重新评估 `production_ready`。
