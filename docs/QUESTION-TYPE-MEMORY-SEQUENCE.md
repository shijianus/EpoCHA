# `memory-sequence-v1` 低密度记忆序列题型

状态：设计候选，不在默认题池，未实现。
目的：为触屏和键盘提供低密度的多样化输入，避免继续增加 6×6 高密度小目标题。

## 1. 交互定义

题面是一张 3×3 或 4×3 网格。服务端预渲染一个短动画，依次高亮 3–5 个格子。动画结束后恢复为空白网格，用户按相同顺序选择格子。

- 观察阶段只播放服务端生成的 sprite sheet。
- 选择阶段只显示空白网格和当前选择序号。
- 客户端不接收目标序列，不保存答案，不判断每一轮是否正确。
- 选择阶段支持点击、触屏、Tab、方向键和 Space/Enter。
- Backspace 撤销最后一次选择，Reset 清空本地操作但不重新签发 challenge。
- 选满序列长度后才生成规范答案 `v1:2.6.4.1`。
- 题目过期、动画中断、模块错误或 `prefers-reduced-motion` 时必须换成静态等价题或返回可重试状态。

## 2. 服务端契约

公开 payload 只包含渲染和交互描述。

```json
{
  "kind": "memory-sequence",
  "answer_type": "sequence",
  "input_mode": "memory",
  "image_mime": "image/png",
  "image_width": 360,
  "image_height": 300,
  "frame_count": 12,
  "frame_cols": 4,
  "frame_rows": 3,
  "frame_ms": 180,
  "frame_loop": false,
  "grid_cols": 3,
  "grid_rows": 3,
  "grid_x": 36,
  "grid_y": 72,
  "grid_cell": 88,
  "grid_gap": 10,
  "sequence_length": 4,
  "prompt_zh": "记住高亮格子的顺序，然后按相同顺序选择。",
  "prompt_en": "Remember the highlighted cells, then select them in order."
}
```

目标序列只进入私有 verifier。建议使用固定长度 nonce 加 HMAC tag-set，禁止把序列、随机 seed 或目标索引写进公开 payload、DOM 或 URL。

### 难度

| 难度 | 网格 | 长度 | 每帧 | 目标耗时 |
|---|---:|---:|---:|---:|
| 1 | 3×3 | 3 | 700 ms | 4–6 s |
| 2 | 3×3 | 4 | 450 ms | 5–8 s |
| 3 | 4×3 | 4 | 350 ms | 6–9 s |
| 4 | 4×3 | 5 | 280 ms | 7–11 s |

同一题型的随机猜测上界必须按真实长度计算。高难度不能只缩短帧时间，还要保持每格至少 44 CSS px 的选择盒。

## 3. 前端视觉要求

- 观察阶段的高亮色与背景对比度达到 WCAG AA，不能只用颜色区分，必须同时有边框或图案变化。
- 高亮格中心与选择阶段格中心必须完全一致，允许误差不超过 1 CSS px。
- 选择阶段的每个格子在 coarse pointer 下至少为 44×44 CSS px，在 fine pointer 下至少为 28×28 CSS px。
- 选择序号显示在格子内部，不改变网格布局，不覆盖焦点环。
- 动画结束后必须有明确的“现在可以选择”的状态提示，按钮在此之前全部 disabled。
- `prefers-reduced-motion` 时排除本题型，或使用不需要动画的静态记忆题，不能强迫用户观看动画。
- 320×568、375×667 和 414×896 的真实 viewport 都必须单独测量，不使用 CSS 宽度模拟替代。

## 4. 验收标准

### 工程验收

- 30 个固定 seed 生成成功，payload、资产和 verifier 都在上限内。
- 同 seed 生成结果一致，不同 seed 至少 95% 的序列或视觉实例不同。
- 公开 payload 不含目标序列、答案、可逆 seed 或交换映射。
- 畸形序列不消耗 challenge，正确格式的错误序列终结 challenge。
- 并发重复提交只允许一个结果成功。

### 视觉验收

- Chromium 与 Firefox 各执行 30 个 holdout seed。
- 每个 seed 留存观察阶段、动画结束、第一步选择、中间选择、完成和错误状态截图。
- 逐帧比较 sprite 的自然尺寸、背景位置和高亮中心，不能只比较最终截图。
- 记录 `getBoundingClientRect()`、滚动宽度、焦点元素、最小命中盒和控制台错误。
- 触屏中心点命中率 30/30，键盘完整流程 30/30。
- 任何一个 viewport 出现布局溢出、中心偏移超过 1px、按钮小于 44px 或动画结束时机错误，状态为 `FAIL`。

### 人工和效果验收

- 普通参与者至少 30 人，无障碍队列至少 10 人。
- 普通参与者首次通过率目标至少 85%，P90 完成时间不超过 12 秒。
- 屏幕阅读器、键盘和 200% 缩放全部完成后才能进入 `production_ready` 候选。
- M4、人工或真机条件缺失时保持 `NOT_RUN` 或 `BLOCKED`。

## 5. 不能接受的实现

- 把序列直接放在 JSON 或 DOM 中，再让客户端“记忆”。
- 用 CSS 缩小网格后声称触屏通过。
- 让客户端判断每一轮是否正确。
- 动画题在 reduced-motion 下静默改成更弱的 PoW-only。
- 用一张最终截图代替逐帧动画证据。
