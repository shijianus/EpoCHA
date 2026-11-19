# 公测与生产门禁报告

日期：2026-11-16
分支：`p0-p1-hardening-20261116`
基线：`sync/visual-bank-20261116`，`6fc8c657fbd5f553a44216dc786a2a01285fee36`

## 本轮实际修改

- 修正文档分支标识和 Node 回归数量，避免沿用旧的 50、57 或 58 项统计。
- 在 `symbol-path` 渲染入口增加粗指针密集布局保护。6×6 路径题在触屏环境下直接 fail closed，不渲染不可操作的微小命中盒。
- 让路径渲染器明确返回成功状态。渲染失败不会继续进入 `ready`，避免半成品题面被提交。
- 增加粗指针路径题回归测试。
- 窄屏控件统一 `box-sizing` 与可换行布局，select、progressive、shell 和 grid 命中盒达到 44px，键盘焦点环与 reduced-motion 样式明确。
- 修复路径拖动在格子外释放后可能保持拖动态的问题。使用 pointer capture、pointerup/pointercancel 收尾，并在题目切换时移除这些监听。
- 统一交互题的 `aria-pressed` 状态。装配题拒绝覆盖已占用槽位，保留原部件并给出中英文状态提示。
- `deploy/preflight.sh --strict` 现在要求 master/pepper 明确存在、密钥属主等于当前服务 UID，并拒绝密钥目录中的符号链接（包括断链）。

## 已执行门禁

| 门禁 | 结果 |
|---|---|
| PoW 向量与 widget 生命周期 | 60/60 PASS（生命周期 53、PoW 7） |
| JavaScript 语法检查 | PASS |
| `git diff --check` | PASS |
| widget gzip | core 29,876B，interactive 10,706B，合计 40,582B，均在预算内，合计余量 378B |
| 预检、发布工具与集成回归 | 13/13、22/22、4/4 PASS |
| `release-check --profile engineering` | INCOMPLETE / 3 |
| `release-check --profile full` | INCOMPLETE / 3 |

## 当前阻断

- Windows 主机可执行 `gofmt` 和 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet ./...`。Linux 运行测试与 race 仍需 Linux/WSL/Docker。
- Oracle/VPS 的 Linux/arm64、UID:GID 1000:1000、专用 Docker、stage4b、stage5a、stage5b 和 admin-ui 未完成。
- 真实 Firefox、真机移动端、辅助技术、人工可读性和 M4 评估未完成。
- `symbol-path` 的桌面布局仍然是 6×6 密集题型。粗指针路径现在会安全拒绝，低密度触屏变体仍未实现。
- `shell-track` 仍需逐帧截图、帧位置、动画结束时刻和跨浏览器验证。
- `docs/QUESTION-TYPES.md` 中题型仍保持 `experimental:true` 与 `production_ready:false`。

本机当前的完整 Node 组合回归为 99 个测试全部通过。`release-check` 仍明确报告 Go 工具链不可用、Linux 运行时与 Docker/Oracle 证据缺失，因此没有把静态回归误报成生产放行。

## 放行结论

当前分支可以作为工程验证版本和受控内部预览候选。完整发布门禁仍为 `INCOMPLETE / 3`，不满足正式公测或生产放行条件。

Windows 静态门禁可执行：

```powershell
$env:GOTOOLCHAIN="local"
$env:GOOS="linux"
$env:GOARCH="amd64"
$env:CGO_ENABLED="0"
gofmt -d internal/dispatch/dispatch.go
go vet ./...
```

获得 Linux 验收环境后，必须再执行：

```bash
gofmt -w internal/dispatch/dispatch.go
go test ./...
go test -race ./...
node docs/scripts/release-check.mjs --profile full
```

Oracle 上线前还必须在目标机执行 `bash deploy/preflight.sh --strict --json`。strict 返回 3 时禁止部署。只有 full profile 为 `RESULT=PASS SCRIPT_RC=0`，并补齐人工、真机、辅助技术、M4、逐帧和跨浏览器证据后，才允许重新评估 `production_ready`。
