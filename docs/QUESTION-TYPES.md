# 题型目录与操作边界（2026-11-06，ADR-024）

本文说明当前登记了哪些题型、怎么选、怎么本地预览、答案怎么编码，以及**哪些结论还没测**。
它不是效果评测报告：除特别标注的历史数字外，本文的 M4、人工可读性、抗 AI/抗 OCR 都是
**NOT_RUN**，不得读成通过。

## 1. 当前题型

| `generator_id` | 介质 | 输入 `answer_type` | 定位 | 生产可用 |
|---|---|---|---|---|
| `arithmetic-baseline-v1` | text | `integer` | 工程基线；题面可直接求值 | 否（`production_ready:false`） |
| `text-image-v1` | image | `text` | 6 字符视觉基线；有历史单工具 OCR 数字 | 否 |
| `text-image-v2` | image | `text` | 8 字符强形变；v1 的独立替代 | 否 |
| `text-image-v3` | image | `text` | 6 字符抽象文字；字形层模糊/位移/擦除 + 单字容差 | 否（M4/OCR/人工可读性 NOT_RUN） |
| `shape-grid-v1` | image | `cells` | 3×3 图形点选：点选所有满足判据的格子 | 否（M4 NOT_RUN） |
| `shape-count-v1` | image | `pairs` | 两类图形计数：两个数字 | 否（M4 NOT_RUN） |
| `parallax-code-v1` | image | `text` | 4 位数字分散在 8–12 帧视差动画中；单帧只有部分笔画 | 否（M4/人工可读性 NOT_RUN） |
| `bank-shot-v1` | image | `angle` | 一次反弹物理题；0.1° 角度 + TSV 容差 | 否（M4 NOT_RUN） |
| `accessible-choice-v1` | text | `text` | 无障碍通道：12 选 1 纯文本单选；显式申请 + 24h 一次 | 否（M4/辅助技术 NOT_RUN） |
| `energy-window-v1` | image | `timing` | 36 帧能量游标，进入标记窗口时记录时机；TSV 时间桶 | 否（M4/人工可读性 NOT_RUN） |
| `pipe-connect-v1` | image | `cells` | 4×4 拓扑网格，点选主路径 2 个断点 | 否（M4 NOT_RUN） |
| `intersection-point-v1` | image | `point` | 两条虚线在随机位置相交，点击延长线交点；网格量化 + 3×3 邻格 TSV | 否（M4/人工可读性 NOT_RUN） |
| `tile-rotation-v1` | image | `masks` | 3×3 路径块旋转，连成一张无越界边的网络；唯一解由枚举证明 | 否（M4/人工可读性 NOT_RUN） |
| `symbol-path-v1` | image | `path` | 6×6 符号路径，按目标序列连接相邻格；唯一匹配路径由 DFS 证明 | 否（M4/人工可读性 NOT_RUN） |
| `crowd-find-v1` | image | `select` | 36 个程序化非人形角色，点击与顶部样本完全相同的一个 | 否（M4/人工可读性 NOT_RUN） |
| `visual-rank-v1` | image | `order` | 5 张折线卡按线段数从少到多排列 | 否（M4/人工可读性 NOT_RUN） |
| `progressive-target-v1` | image | `sequence` | 三轮 3×3 定位，客户端逐轮推进不判对错 | 否（M4/人工可读性 NOT_RUN） |
| `shell-track-v1` | image | `sequence` | 三轮四容器交换；服务端预渲染 33 帧雪碧图，动画结束前不可选 | 否（M4/跨浏览器动画 NOT_RUN） |
| `silhouette-assembly-v1` | image | `placements` | 5 部件/5 槽位离散旋转装配，唯一匹配由枚举证明 | 否（M4/人工可读性 NOT_RUN） |

`shape-grid-v1` 还有一个 **negative polarity** 指令修饰器：kind/category 模式可出"选出所有
**不是** X 的格子"；k 仍为 2/3，至少 2 个应选、至少 2 个不应选，fill 模式固定 positive。
它只改变指令表达，不改变答案空间或 verifier，不得描述为显著提高安全性。

所有题型在管理面 `/api/admin/generators` 里都返回 `experimental:true` / `production_ready:false`。
**没有**任何题型被证明抗 AI、抗 OCR、抗多模态或已完成人工可读性验收。

`text-image-v3` 的 d1/d2 是相对保守的档位；d4/d5 的后处理更强（模糊 2 遍、位移 3px、更多擦除点），
在人工验收完成前不要把 d4/d5 当作"人能轻松读出来"的默认配置。任何档位的人工可读性都是 NOT_RUN。

## 2. 站点怎么选题型

站点策略仍只有**一个** `generator_id`（ADR-024 没有实现按权重混合多题型）：

```bash
# 创建站点：pow_question + 指定题型与难度
curl -sS -X POST "$ADMIN_BASE/api/admin/sites" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"grid-demo","verify_mode":"pow_question",
       "generator_id":"shape-grid-v1","difficulty":2,
       "pow_bits":14,"origins":["https://app.example.com"]}'
```

管理控制台的创建/详情页会从 `/api/admin/generators` 动态列出全部题型；
widget 按题面的 `answer_type` 自动切换控件（`text/cells/pairs/angle/point/timing` 与
`masks/path/select/order/sequence/placements` 共 12 种）；后 6 种由按需加载的
`v1.interactive.js` 提供，核心 `v1.js` 不因实验题型膨胀。
`EPOCHA_GENERATORS` 是**进程级白名单**：留空 = 全部启用；填了列表就只有列表里的题型能领题。
关掉一个题型不会让已签发但未作答的 challenge 静默降级——它会得到 503 可重试（见 DESIGN §8.4）。

## 3. 答案编码（服务端是唯一裁决者）

| `answer_type` | 规范化后的提交串 | 规范化规则 | 畸形（不消耗作答机会） |
|---|---|---|---|
| `text` | 6/8 个大写字符 | 裁首尾 ASCII 空白、小写折大写、长度必须恰好等于题面 `length` | 内部空白、全角、字符集外（含 `I/O/0/1`）、长度不符 |
| `cells` | `3,7` | 每个序号 1..9、逗号分隔、排序、去重；`3, 7` 与 `7,3` 等价 | 空、0、越界、非十进制、空字段 |
| `pairs` | `4,2` | 恰好两个 ASCII 十进制数（各 1..2 位）、剥前导零 | 少/多字段、负数、小数、空字段 |
| `angle` | `1234` | 0..3599 的 0.1° 量化；只接受 ASCII 数字、≤4 位 | 符号、小数、>3599 |
| `point` | `512,340` | 两个 0..999 的 ASCII 十进制数（各 1..3 位）；逗号两侧可裁 ASCII 空格 | 缺/多字段、>999、负数、小数、空字段 |
| `timing` | `1234` | 0..60000 的毫秒整数；`energy-window-v1` 额外拒绝 <200ms（零思考抢答） | 非数字、>60000、energy-window 的 <200 |
| `masks` | `v1:1a2b3c4d5` | 每格一个 4-bit edge mask；固定格数、严格 ASCII 十六进制、小写规范 | 空、长度不符、非 hex、尾随垃圾、Unicode 同形 |
| `path` | `v1:03.09.10.16.22.28` | 每段两位十进制、固定长度范围、无重复；题型再校验相邻性 | 少/多段、一位数、越界、重复、不相邻、非 ASCII |
| `select` | `v1:7` | 单个 0-based 索引；无前导零 | 越界、负数、前导零、多字段 |
| `order` | `v1:A.B.C.D.E` | 固定 n 个字母 ID、无重复、保持选择顺序 | 缺/多字段、重复、非 A–Z、非法版本 |
| `sequence` | `v1:0.4.8` | 固定 n 个一位十进制、允许重复（每轮独立） | 缺/多字段、>max、非数字 |
| `placements` | `v1:0:2,3:1,…` | 固定 slot 数；`piece:rotation`，piece 不重复、rotation 0..3 | 缺/多字段、重复 piece、越界 rotation |

Phase 1（ADR-025）定义的 `angle`（0.1° 整数）、`point`（`x,y` 千分位）与 `timing`（毫秒）三种契约类型
已在 Phase 2–4.5 被实际题型使用：`bank-shot-v1`（`angle`）、`intersection-point-v1`（`point`）、
`energy-window-v1`（`timing`）。`parallax-code-v1` 仍用 `text` + 雪碧图。

`question.max_answer_len` 只是输入提示，不是服务端判定依据；传输层另有 `len ≤64` 上限。
`cells` 的规范化只把"同一集合的另一种写法"折叠掉，**不扩大接受域**：一个 challenge 只接受
唯一一个规范化集合。

## 4. widget 的输入类型

- `text`：文本输入（v1/v2/v3）。v3 的题面带 `"tolerant":1`，widget 会追加一行
  "本题最多允许 1 个字符认错"的说明。
- `cells`：图片上覆盖 3×3（或服务端声明的其他几何）可聚焦按钮。选中状态用 `aria-pressed`，
  再次点击取消；键盘 Space/Enter 可用；提交串按 1-based 序号排序去重。
  几何来自题面的 `grid_cols/grid_rows/grid_x/grid_y/grid_cell/grid_gap`，widget 会校验
  网格完全落在图片内；越界即 fail closed（不进 ready、不渲染按钮）。
- `pairs`：两个 `type=number` 输入，标签取自 `labels_zh/labels_en`；两个都有值才允许提交。
- `angle`：角度滑块，提交 0.1° 整数；题面给 `angle_min/angle_max/angle_step`。
- `point`：图片点击/键盘移动，提交 `x,y`（各 0..999）。
- `timing`：雪碧图动画 + “记录时机”，提交毫秒；必须 `render_mode:"sprite_sheet"`，减少动态时 fail closed。
- `masks`：路径块旋转。3×3 可聚焦按钮；Enter/Space 顺时针 90°，Shift+Enter 逆时针；同一 tile 在
  140ms 动画结束前拒绝重入；答案由每格初始 mask 与 turns 组合成 `v1:<9 hex>`。
- `path`：6×6 符号路径。点击/拖动相邻格追加，Backspace 撤销，方向键移动焦点；已选格显示序号，
  路径线只表示用户输入；达到目标长度才合成 `v1:NN.NN...`。
- `select`：36 选 1。点击/键盘选择单个角色；提交前可改选，只有最后一个索引进入答案。
- `order`：5 卡排序。点击卡片加入顺序栏，已选卡不会重复；Undo 撤销最后一张；选满 5 张才合成答案。
- `sequence`：固定 n 轮的单值序列。`progressive-target` 逐轮推进并在每轮重建 3×3 按钮；
  `shell-track` 播放服务端预渲染帧，每轮动画结束前按钮 disabled，三轮都选完才合成答案。
- `placements`：5 部件托盘 + 5 槽位。选择部件、独立 Rotate 按钮改朝向、点击槽位放置；
  已占用槽位再放置会把原部件返回托盘（确定行为）；Undo/Reset 不生成 token、不改服务端 challenge；
  五格填满才合成 `v1:id:rot,...`。

这些输入都已有登记题型使用；widget 不解释答案、不做判定；隐藏字段仍然只承载一次性 response token（验证通过后），
答案只通过 `/api/redeem` 发给服务端。服务端文本只进 `textContent`，没有 HTML 拼接。

## 5. 本地预览

前置：一个已启动的 `cmd/server`（公共监听器 + 管理监听器）。预览进程会通过管理 API
创建一个**只用于本地**的站点，来源白名单是本预览页自己的 loopback 地址：

```bash
# 终端 A：起服务（示例，密钥用运行时随机文件，不要用本文的命令生成生产密钥）
go run ./cmd/server ...

# 终端 B：预览某个题型
go run ./cmd/preview \
  --api http://127.0.0.1:8080 --admin http://127.0.0.1:9090 \
  --admin-key "$EPOCHA_ADMIN_KEY" \
  --generator shape-grid-v1 --difficulty 3
```

`--generator` 支持全部已登记题型：`text-image-v1/v2/v3`、`shape-grid-v1`、`shape-count-v1`、
`parallax-code-v1`、`bank-shot-v1`、`accessible-choice-v1`、`energy-window-v1`、`pipe-connect-v1`、
`intersection-point-v1`、`tile-rotation-v1`、`symbol-path-v1`、`crowd-find-v1`、`visual-rank-v1`、
`progressive-target-v1`、`shell-track-v1`、`silhouette-assembly-v1`、
`arithmetic-baseline-v1`。预览只允许 loopback，不接受非回环监听地址。
**注意**：`cmd/preview` 页面是自包含的 text 答案页（测试禁止外部 `<script src>`），
非 `text` 题型会在页面上明确提示"本预览页只支持 text 答案"并停止，不会给一个误导性的
文本输入框；交互题型的真实预览请用生产 widget 或 `cmd/exampleapp`。因此"新增题型出现在
`--generator` 白名单"**不等于**"交互预览已验收"。

## 6. 离线样本与 OCR 基线

`cmd/evalgen` 支持 v3 的样本生成（dev/holdout 分离，holdout seed 写死）：

```bash
go run ./cmd/evalgen --set dev --generator text-image-v3 --difficulty 3 --count 40
```

manifest 的 `render_params` 只序列化 `ImageParams` 的导出字段；v3 的逐字形后处理参数
（模糊/位移/擦除点）由生成器版本固定，不在 manifest 的 JSON 里逐项展开。复现以
“生成器 ID+版本+seed+已生成样本文件”为准，样本文件本身才是评测对象。

`cmd/evalocr --normalize textimage-v3` 会使用 v3 的生产 `NormalizeAnswer`，但评分器
**只做严格整串比较**：它不会应用 v3 的"允许 1 个字符替换"规则。因此从这里读到的
`StrictAccuracy` 是生产接受率的**下界**，不是等价指标。要测量生产接受率需要容差感知的
评分器（目前没有），不要用严格数字反推"人能过/不能过"。

v3 的 OCR 基线（M4）**尚未执行**；不得把 v2 的 0% 结果外推到 v3。
`shape-grid-v1` / `shape-count-v1` / `pipe-connect-v1` / `intersection-point-v1` 也没有 OCR 基线：
OCR 不是它们的威胁模型，它们对机器视觉本来就容易（见 §7）。`energy-window-v1` 的 M4 同样未测。

## 7. M1–M4（诚实口径）

| 题型 | M1 题面随机空间 | M2 接受域 / 答案空间 | M3 单次 / 3 次 | M4 |
|---|---|---|---|---|
| `shape-grid-v1` | 判据组合 × 种类/填充 × 9 格排列 | 唯一规范化集合；`C(9,2)=36` 或 `C(9,3)=84` | 最坏 `1/36`；3 次上界 `1-(35/36)^3 ≈ 8.1%` | **未测** |
| `shape-count-v1` | 两类类别组合 × 数量 × 干扰排列 | 唯一 `a,b`；`6×6=36` | `1/36`；3 次 ≈8.1% | **未测** |
| `text-image-v3` | `32^6` 答案串 × 形变参数 | 精确答案 + 6×31 个单字替换 = **187** 个规范化串；答案空间 `32^6=1,073,741,824` | `187/32^6 ≈ 1.74e-7`；3 次 ≈5.2e-7 | **未测** |
| `parallax-code-v1` | `10^4` 代码 × 帧数/背景/分层速度 | 精确 4 位数字；接受域 1 | `1e-4`；3 次 ≈`3e-4` | **未测** |
| `bank-shot-v1` | 母球/目标袋/障碍/视觉种子组合 | 3600 个 0.1° 桶；接受域 `2×tolerance+1`，d1 最大 41 个桶 | 最坏 `41/3600 ≈ 1.14%`；3 次 ≈3.4%；API 仍只给 1 次有效尝试 | **未测** |
| `accessible-choice-v1` | 12 个数值选项 × 判据 | 精确选项；12 选 1 | `1/12 ≈ 8.3%` 单次；仅 1 次 + 24h 限额 | **未测** |
| `energy-window-v1` | 36 帧 × 游标相位 × 视觉种子 | 480 个 6ms 桶；目标只在 40..439 桶（400 个可行桶），接受域 13 个桶 | `13/400 = 3.25%`；3 次 ≈9.44% | **未测** |
| `pipe-connect-v1` | 主路径行位 × 死路位置 × 视觉种子 | 断点与死路都显示为空候选格；d1 最少 `E=9`、d5 最少 `E=13` 个候选 | d1 `1/C(9,2)=1/36 ≈ 2.78%`；3 次 ≈8.1%；d5 `1/C(13,2)=1/78 ≈ 1.28%` | **未测** |
| `intersection-point-v1` | 两条线斜率组合 × 目标格（`cols×rows`）× 视觉种子 | 目标格 + 3×3 邻格；d1 32×18 网格（目标可行 351 格），d5 64×36（1404 格） | 最坏 `9/351 = 2.56%`；3 次 ≈7.50%；d5 `0.64%` / 3 次 ≈1.91% | **未测** |
| `tile-rotation-v1` | 生成树/额外边 × 每格初始旋转 × 视觉种子 | 每格不同旋转的组合；生成器要求 ≥64 个候选最终状态 | 最坏 `1/64`；3 次 ≈4.69% | **未测** |
| `symbol-path-v1` | 自避路径 × 目标符号排列 × 干扰符号 | 合法位置路径 ≥128；符号匹配路径由 DFS 证明恰好 1 条 | 盲猜合法路径 `1/128`；3 次 ≈2.34% | **未测** |
| `crowd-find-v1` | 6 特征组合 × 目标位置 × 干扰组合 | 36 个角色中目标签名恰好 1 个 | `1/36`；3 次上界 `3/36 = 8.33%` | **未测** |
| `visual-rank-v1` | 5 个线段数 × 卡片排列 × 公开 ID 排列 | 5! 个排列中唯一一个 | `1/120`；3 次 `2.5%` | **未测** |
| `progressive-target-v1` | 三轮独立目标字形 × 每轮 9 格 | 每轮恰好一个目标格；`9^3` 个序列 | `1/729`；3 次 ≈`0.41%` | **未测** |
| `shell-track-v1` | 三轮目标起点 × 每轮 4 次交换 | 四容器、三轮；`4^3=64` 个最终序列 | `1/64`；3 次 ≈`4.69%` | **未测** |
| `silhouette-assembly-v1` | 5 部件无放回 × 槽位旋转 × 公开 ID 排列 | 唯一 `slot→piece+rotation` 映射 | `1/(5!×4^5)=1/122880`；3 次 ≈`2.45e-5` | **未测** |

`shape-grid` / `shape-count` 的 M3 计算假设攻击者在**正确的 k/正确数量范围**内均匀猜；
如果它连 k 都不知道，接受域更小。两者都在 3 次尝试的 10% 红线以内，但这只是"随机猜不通"，
不是"机器解不了"——图形对机器视觉很容易，M4 未测，不能声称抗 AI。
`parallax-code` / `bank-shot` 的 M3 同样只是随机猜测上界；两者的 M4（VLM/OCR/脚本/人工）
均为 **NOT_RUN**，不得把"单帧不完整"或"角度容差小"写成抗 AI 证明。
`energy-window` 在 `reduced_motion` 下由调度器排除（运动题），`pipe-connect` 复用 `cells`，
`intersection-point` 复用 `point`（键盘方向键可微调）。`energy-window` 的 M3 分母是 400 个可行目标桶
（目标只在 40..439 抽样），d1 三次随机猜测上界 9.44%，三次点击各覆盖 13 桶的覆盖上界 39/400=9.75%
（仍 <10%，没有余量，任何放宽 d1 容差的改动都必须重算）。`pipe-connect` 的断点与死路**外观完全相同**，
盲猜者能把答案缩到 `E=死路数+2` 个空候选格，所以 M3 按 `C(E,2)` 算而不是 `C(16,2)`；
d1 至少 7 个死路（`E=9`）保证 3 次上界 8.1% 仍在 10% 内。能顺着主路径排除死路属于 M4，未测。`energy-window` 的 M3 是 3 次上界 ≈9.44%，
仍在 10% 内；但 M4 未测，不得写成抗 AI。
`intersection-point` 的接受域是**量化后的 3×3 邻格**，难度越高网格越细、同样的像素误差更容易落到
接受域之外；这是容差梯度，不是"像素级精确点击"。画面**不画交点、也不放遮挡块**：目标格的格中心
距画布边缘至少 50px，可行范围只有整张网格的约 54%，所以 M3 按可行格数算（d1 `9/351`）。
3 次点击各覆盖一个 3×3 接受域的上界是 `27/351 ≈ 7.69%`（d5 `27/1404 ≈ 1.92%`），仍在 10% 内。
如果交点被某个可见标记（例如遮挡块）指出，"点标记中心"就会成为 100% 通过的捷径，M3 的随机猜测模型
也就不成立——所以这个题型刻意没有遮挡块。它仍需要人工确认"交点看得见、点得准"（NOT_RUN），
也不得把 M3 小写成抗 AI 证明。
`accessible-choice` 是**弱安全**的无障碍通道：1/12 随机猜中率远高于普通题，
只靠显式 accessibility 请求、站点池显式包含、24h 一次和一次性 challenge 限制；
它不进入普通用户的题池，也不得被写成生产可用或抗 AI。

## 8. v3 的逐字容差 verifier 边界

v3 的私有 verifier 是：

```
nonce(16B) || tag_0 || … || tag_{L-1},   L=6
tag_i = HMAC(K(qver), "epocha/v1|qver-char|" || nonce || 0x00 || byte(i) || 0x00 || char_i)
```

`Verify` 对每个位置重算标签、常数时间比较、累计不匹配数，`≤1` 才通过；没有读回答案的接口。
v1/v2 仍是 48B 的精确 HMAC，路径与长度不变。

必须如实写出的限制：**拿到 qver 密钥的人本来就能离线枚举 verifier**（ADR-011 的残余风险）；
逐字标签把"有密钥时的离线枚举"从 `32^6` 降到 `6×32`。它不改变无密钥时的安全边界，
也不是比精确 HMAC 更强的构造。

这条容差同时降低了 OCR 的整串门槛：OCR 读错一个字符仍可能通过。它是为"人认错一个字仍能过"
付出的代价，**不是**抗 OCR 措施；v3 的 M4 未测，不得据此声称抗 OCR/抗 AI。

## 9. 明确未做 / 未测

- **按权重混合多题型**：未实现。站点仍只选一个 `generator_id`；"多样性"指可选题型增加，
  不是同一站点自动轮换。
- **从 N 个图形里点一个的单选题**：N 只有 4..12，3 次尝试通过率超过 10% 红线，没有实现。
  `intersection-point-v1` 是连续坐标点选（网格量化 + 3×3 容差），不是这一形态，不能拿它的 M3 为 N 选 1 背书。
- **自由拖拽/浮点坐标/大面积区域容差**：未实现；会放大接受域，需要另一套判分构造。
  已实现的是受限形态：`bank-shot-v1` 的角度滑块（0.1° + TSV）、`intersection-point-v1` 的点击点
  （9 格上限）与 `silhouette-assembly-v1` 的离散旋转/槽位放置（不上传浮点坐标）。
- **非视觉等价通道（音频等）**：未实现；PoW-only 降级不是等效替代。
- **`shell-track-v1` 的 reduced-motion**：dispatch 对 `Motion:true` 的候选在
  `prefers-reduced-motion` 请求下直接排除；如果站点的 `generator_pool` 只有 `shell-track-v1`，
  该请求会得到可重试的 `challenge-unavailable`，**不会**降级成普通题或 PoW-only。运营者必须
  在池里显式加入一个非 Motion 题型，才算提供"更换其他验证"。
- **人工可读性、辅助技术、真机移动端、M4、抗 AI/抗 OCR**：全部 NOT_RUN。
  人工可读性必须在目标设备上按 `docs/MANUAL-ACCEPTANCE.md` 的方式另行验收；
  本文件与生成器测试里的覆盖率/几何断言只证明"字没被擦掉/图形没被裁切"，不证明"人能读出来"。
- **Wave 2 交互题**（`progressive-target-v1`、`shell-track-v1`、`silhouette-assembly-v1`）已实现并登记；
  `shape-grid-v1` 的 negative polarity 修饰器已实现（kind/category，fill 固定 positive）。
  三题型仍 `experimental:true` / `production_ready:false`；`shell-track` 的服务端 PNG 雪碧图路径
  已有 Node 工装，但 GIF disposal 与 Chrome/Firefox/WebKit 动画一致性 **NOT_RUN**。
  `pipe-connect-v1` 现状只是"点选两个断点"，verifier 无法表达完整逐格 edge mask；在不动其已签发
  verifier 语义的前提下标记 **BLOCKED**，不得用客户端 `solved=true` 代替服务端验证。
