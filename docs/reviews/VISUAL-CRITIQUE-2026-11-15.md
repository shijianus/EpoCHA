# 当前题库视觉优化与复测报告

日期：2026-11-15
分支：`release-preparation`
范围：`web/widget/v1.js`、`web/widget/v1.interactive.js`、真实 PNG 夹具和视觉画廊。

## 已完成的修复

- 统一 widget 卡片、题图、按钮、输入框、状态提示和焦点环的视觉 token。
- 输入框、按钮和选项的最小高度设为 44 CSS px。
- `grid`、`rotate`、`placements` 使用独立命中盒，视觉图形不再决定按钮最小尺寸。
- 加入 hover、active、focus-visible 和 selected 状态。
- 视觉画廊加载真实 widget、真实 Shadow DOM 和 PNG 资产，mock challenge 只替代 Go 服务。
- 动态题夹具的帧数、帧列数、帧间隔和 sprite 资产已经对齐。
- 画廊的 3×3 网格夹具已按真实 480×340 资产修正为 72px 单元和 24px 间距，避免夹具越界造成假失败。
- 画廊切换 CSS 宽度时会销毁旧 widget 并重新领题，长时间滚动后不会把已过期 token 当成新的视觉结果。
- 新增低密度题型设计候选 `memory-sequence-v1`、`drag-align-v1`、`semantic-select-v1`，明确为未实现和不进入默认题池。

## 画廊复测结果

画廊入口是 [`docs/visual-gallery/index.html`](../visual-gallery/index.html)。它检查题图比例、Shadow DOM 几何、滚动宽度、28px 精细指针门槛和 44px 触屏门槛。

在 375 CSS px 内容宽度模拟下，当前结果如下。

| 题型 | 最小可见控件 | 结果 | 说明 |
|---|---:|---|---|
| 视觉文字 | 44px | PASS | 输入框和按钮达到触屏高度 |
| `shape-grid-v1` | 44px | PASS | 命中盒与图形区域分离 |
| `tile-rotation-v1` | 44px | PASS | 旋转块命中盒达到触屏门槛 |
| `silhouette-assembly-v1` | 44px | PASS | 部件和槽位达到触屏门槛 |
| `symbol-path-v1` | 约 18px | FAIL | 6×6 网格密度过高，不能靠 CSS 放大解决 |
| `shell-track-v1` | 44px | NOT_RUN | 已真实渲染，逐帧比例、跨浏览器动画和真机触控仍未完成 |

宽窗口静态题图的渲染比例为 1.000，画廊没有发现横向溢出。375px 是 CSS 内容宽度模拟，不等于真实设备 viewport。真实 `setViewport`、Firefox、真机触控和辅助技术仍需单独验收。

`shape-count-v1` 和 `visual-rank-v1` 已由前端生命周期测试覆盖交互状态与答案编码，当前画廊尚未放入这两个真实 PNG 夹具，因此不在上表宣称视觉 PASS。

2026-11-15 已在同一桌面会话的 Chromium 中打开本地 HTTP 画廊，实际观察了 `375×667` 和 `320×568` 两档。视觉文字、图形点选、路径块旋转和离散装配均实际加载 PNG 并达到 44px 最小控件高度。切换到 320px 后重新领题，图形点选仍保持 44px 命中盒。符号路径在 375px 下实际测得约 18.1px。动态容器实际显示 sprite 和目标容器，画廊按约定标记为 `NOT_RUN`，等待逐帧和跨浏览器证据。

## 当前题型的批判

### `symbol-path-v1`

6×6 网格在窄屏下把目标压到约 18px。下一步需要真正改变交互模型。

- 使用局部放大和相邻候选选择
- 或为触屏生成低密度窄屏变体
- 或将路径拆成逐步选择，每一步只显示可命中的候选

继续整体缩放图片会让题型保持“看得见但点不到”，不能作为修复。

### `crowd-find-v1`

36 个候选目标同样属于高密度搜索。移动变体完成前，调度器应把它从 touch 候选中排除。桌面键盘路径可以继续保留。

### `shell-track-v1`

当前画廊可以进入真实 sprite 渲染和结束状态，但还没有逐帧证据。必须检查自然尺寸、背景位置、目标容器中心、帧间漂移、动画结束时刻和 `prefers-reduced-motion` 行为。

### `silhouette-assembly-v1`

44px 命中盒已满足门槛，但部件托盘和槽位在窄屏会拥挤。真机测试应测误触率、旋转后焦点位置、Undo、占用槽位返还和横竖屏切换。

## 下一步放行顺序

1. 完成 `symbol-path-v1` 触屏变体，并将 `crowd-find-v1` 在移动变体完成前从 touch 调度中排除。
2. 在真实 Chromium 与 Firefox 的 320×568、375×667、414×896 viewport 上重跑画廊。
3. 使用每个 generator 的真实 holdout PNG 替代画廊夹具。
4. 对 `shell-track-v1` 做逐帧截图、帧位置和跨浏览器验收。
5. 完成人工、键盘、屏幕阅读器和真机触控验收后，才允许题型进入 `production_ready` 评估。

## 当前验证边界

- Node widget、PoW 和生命周期回归共 50 项通过。
- JavaScript 语法检查和 `git diff --check` 通过。
- 当前环境没有可用的 Go 工具链，Go 全量测试、race、Linux 静态构建和真实 Go 服务闭环未执行。
- 因此本报告是视觉工程优化和前端回归报告，不是生产发布批准。
