# web/widget

阶段 4 交付物目录。零依赖原生 JS：`v1.js`（自定义元素 `<epocha-verify>`）、`i18n/`（zh-CN / en）、`test/`（人工验证用例清单）。

约束：gzip 后 ≤30KB（`make widget-size`）、键盘可完成、`aria-live` 播报、移动端 375px 无横向溢出、
并提供"不适合看图"用户的纯 PoW 高难度通道。产物由 `go:embed` 打进二进制，不引入打包工具链。
