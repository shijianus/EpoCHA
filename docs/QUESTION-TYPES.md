# 题型目录与操作边界（2026-11-05，ADR-024）

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
选择 `shape-grid-v1` / `shape-count-v1` 后，widget 会按 `answer_type` 自动换成点选/双数字控件。
`EPOCHA_GENERATORS` 是**进程级白名单**：留空 = 全部启用；填了列表就只有列表里的题型能领题。
关掉一个题型不会让已签发但未作答的 challenge 静默降级——它会得到 503 可重试（见 DESIGN §8.4）。

## 3. 答案编码（服务端是唯一裁决者）

| `answer_type` | 规范化后的提交串 | 规范化规则 | 畸形（不消耗作答机会） |
|---|---|---|---|
| `text` | 6/8 个大写字符 | 裁首尾 ASCII 空白、小写折大写、长度必须恰好等于题面 `length` | 内部空白、全角、字符集外（含 `I/O/0/1`）、长度不符 |
| `cells` | `3,7` | 每个序号 1..9、逗号分隔、排序、去重；`3, 7` 与 `7,3` 等价 | 空、0、越界、非十进制、空字段 |
| `pairs` | `4,2` | 恰好两个 ASCII 十进制数（各 1..2 位）、剥前导零 | 少/多字段、负数、小数、空字段 |

Phase 1（ADR-025）另定义了 `angle`（0.1° 整数）、`point`（`x,y` 千分位）与 `timing`（毫秒）三种**契约类型**；
它们已有服务端规范化与 widget 输入，但**没有注册生产题型**，当前 6 个题型都不使用它们。

`question.max_answer_len` 只是输入提示，不是服务端判定依据；传输层另有 `len ≤64` 上限。
`cells` 的规范化只把"同一集合的另一种写法"折叠掉，**不扩大接受域**：一个 challenge 只接受
唯一一个规范化集合。

## 4. widget 的输入（现有三种 + Phase 1 契约三种）

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

后三种是 ADR-025 的契约输入，当前没有生产生成器登记，不属于上面的 6 个题型。
widget 不解释答案、不做判定；隐藏字段仍然只承载一次性 response token（验证通过后），
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

`--generator` 支持 `text-image-v1/v2/v3`、`shape-grid-v1`、`shape-count-v1`、
`arithmetic-baseline-v1`。预览只允许 loopback，不接受非回环监听地址。

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
`shape-grid-v1` / `shape-count-v1` 也没有 OCR 基线：OCR 不是它们的威胁模型，
它们对机器视觉本来就容易（见 §7）。

## 7. M1–M4（诚实口径）

| 题型 | M1 题面随机空间 | M2 接受域 / 答案空间 | M3 单次 / 3 次 | M4 |
|---|---|---|---|---|
| `shape-grid-v1` | 判据组合 × 种类/填充 × 9 格排列 | 唯一规范化集合；`C(9,2)=36` 或 `C(9,3)=84` | 最坏 `1/36`；3 次上界 `1-(35/36)^3 ≈ 8.1%` | **未测** |
| `shape-count-v1` | 两类类别组合 × 数量 × 干扰排列 | 唯一 `a,b`；`6×6=36` | `1/36`；3 次 ≈8.1% | **未测** |
| `text-image-v3` | `32^6` 答案串 × 形变参数 | 精确答案 + 6×31 个单字替换 = **187** 个规范化串；答案空间 `32^6=1,073,741,824` | `187/32^6 ≈ 1.74e-7`；3 次 ≈5.2e-7 | **未测** |
| `parallax-code-v1` | `10^4` 代码 × 帧数/背景/分层速度 | 精确 4 位数字；接受域 1 | `1e-4`；3 次 ≈`3e-4` | **未测** |
| `bank-shot-v1` | 母球/目标袋/障碍/视觉种子组合 | 3600 个 0.1° 桶；接受域 `2×tolerance+1`，d1 最大 41 个桶 | 最坏 `41/3600 ≈ 1.14%`；3 次 ≈3.4%；API 仍只给 1 次有效尝试 | **未测** |
| `accessible-choice-v1` | 12 个数值选项 × 判据 | 精确选项；12 选 1 | `1/12 ≈ 8.3%` 单次；仅 1 次 + 24h 限额 | **未测** |

`shape-grid` / `shape-count` 的 M3 计算假设攻击者在**正确的 k/正确数量范围**内均匀猜；
如果它连 k 都不知道，接受域更小。两者都在 3 次尝试的 10% 红线以内，但这只是"随机猜不通"，
不是"机器解不了"——图形对机器视觉很容易，M4 未测，不能声称抗 AI。
`parallax-code` / `bank-shot` 的 M3 同样只是随机猜测上界；两者的 M4（VLM/OCR/脚本/人工）
均为 **NOT_RUN**，不得把"单帧不完整"或"角度容差小"写成抗 AI 证明。
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
- **单选/单点点击**：答案空间只有 N（4..12），3 次尝试通过率超过 10% 红线，没有实现。
- **拖拽/滑块/旋转/区域容差**：未实现；会放大接受域，需要另一套判分构造。
- **非视觉等价通道（音频等）**：未实现；PoW-only 降级不是等效替代。
- **人工可读性、辅助技术、真机移动端、M4、抗 AI/抗 OCR**：全部 NOT_RUN。
  人工可读性必须在目标设备上按 `docs/MANUAL-ACCEPTANCE.md` 的方式另行验收；
  本文件与生成器测试里的覆盖率/几何断言只证明"字没被擦掉/图形没被裁切"，不证明"人能读出来"。
