# web/widget

阶段 4A 交付的生产 widget。零依赖原生 JS：`v1.js` 主脚本与 `v1.worker.js` 同源 Worker 副本，均由 `go:embed` 打进 server 二进制并通过固定版本化 URL 提供。

当前公开接口是 `EpoCHA.render()`、`EpoCHA.getResponse()`、`EpoCHA.reset()`、`EpoCHA.refresh()` 与 `EpoCHA.destroy()`；不是自定义元素，也没有本目录下的 `i18n/` 或 `test/` 子目录。完整接入示例、表单字段归属和 CSP/Worker 选择见 `docs/WIDGET.md`。

当前 `v1.js` 只渲染 `text-image-v1` 图片题。管理面默认创建的 `pow_only` 站点不会下发题面；在图片题型的非视觉等价通道、`fallback_base` 和无 JS 服务端表单路径交付前，不得把 PoW-only 降级或当前图片题描述成完整无障碍生产方案。

约束：主脚本 gzip 后 ≤30KB（`make widget-size`）、键盘路径与 `aria-live` 保持；真实浏览器、真人可读性、辅助技术和真机移动端验收按阶段门禁分别记录，不以 Node 行为工装代替。
