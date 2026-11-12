# WIDGET.md — 生产 widget 接入（阶段 4A）

对象：`web/widget/v1.js`（主脚本）+ `web/widget/v1.worker.js`（PoW worker 副本）+
`web/widget/v1.interactive.js`（实验型交互题渲染器，按需加载）。三者由 `cmd/server` 以 `go:embed`
内嵌并在**公共端口**提供（DESIGN D10、§10.1）：

```
GET /widget/v1.js          GET /widget/v1.worker.js
GET /widget/v1.interactive.js
Content-Type: text/javascript; charset=utf-8
Cache-Control: public, max-age=3600
X-Content-Type-Options: nosniff
（无 Access-Control-*：<script src> 跨域加载不需要 CORS 头）
```

URL 是**版本化**的：改协议或有不兼容变更就发 `v2.js`，不要把还在用的页面半路换掉。
条件请求用内容哈希 ETag（内容与字节严格一致；不用 `http.ServeContent`，原因见 §7）。

---

## 1. 最小接入

当前 `v1.js` 支持 `text-image-v1/v2/v3`、`shape-grid-v1`、`shape-count-v1`、`parallax-code-v1`、
`bank-shot-v1`、`accessible-choice-v1`、`energy-window-v1`、`pipe-connect-v1`、`intersection-point-v1`，
以及实验型交互题 `tile-rotation-v1`、`symbol-path-v1`、`crowd-find-v1`、`visual-rank-v1`（Wave 1）。
Wave 2 的 `progressive-target-v1`、`shell-track-v1`、`silhouette-assembly-v1` 复用同一批
`sequence`/`placements` 控件；它们仍为 `experimental:true` / `production_ready:false`。
`masks/path/order/select/sequence/placements` 六种输入由 `v1.interactive.js` 提供：核心只在领到
这些题型时才从**核心脚本所在位置**同源加载该模块（`script-src` 已允许核心脚本的接入方无需额外配置），
加载失败进入 `error` 的 `interactive-unavailable`，不会渲染半成品或自行判分。
管理 API 与控制台创建站点的默认模式仍为 `pow_only`；接入本 widget 前，需要显式设置
`verify_mode=pow_question` 与一个本进程登记的 `generator_id`，选择难度，并确认后台题池已有可领取题目。
默认设置未完成这一步时，widget 会拒绝不支持的题面，不能把页面加载成功当成接入成功。

将业务页面的完整 Origin（协议、主机、非默认端口）写入站点来源列表；验证服务自身的地址不能代替业务页面的 Origin。浏览器只持有公开的 `site_key`，站点 secret 仅留在业务服务器。图片题仍为 `experimental:true`、`production_ready:false`；非视觉等价通道尚未交付。

**这一节有可运行版本**：`cmd/exampleapp` 的 `/fixture` 页面就是下面这段代码，
由 `docs/scripts/browser/widget_browser.test.mjs` 在真实浏览器里加载并**原生提交**验证
（`FormData` 里有 token、POST 打到业务后端、服务端写库）。文档示例最容易出两种错误，
它们都不在文字里，只有真加载 + 真提交才现形：

- 跨域 widget 脚本带 `defer`，后面紧跟一个**同步**内联脚本立刻调 `EpoCHA.render`
  ⇒ 加载顺序竞争，首屏偶发 `EpoCHA is not defined`（缓存热了就复现不出来）。
- 容器放在 `<form>` **外** ⇒ 自动生成的隐藏字段不属于目标表单，页面显示"验证通过"，
  但 POST 里根本没有 token。

```html
<form id="order" method="post" action="/order">
  <label for="item">商品</label>
  <input id="item" name="text" type="text" maxlength="500">
  <div id="captcha"></div>            <!-- 容器在 form 内：隐藏字段才属于这个表单 -->
  <button type="submit">下单</button>
</form>

<!-- 两个脚本都带 defer。deferred 脚本按文档顺序执行，所以 init 一定跑在 EpoCHA 定义之后。 -->
<script src="https://captcha.example.com/widget/v1.js" defer></script>
<script src="/js/epocha-init.js" defer></script>
```

`/js/epocha-init.js` —— 外部文件，**不是**内联块（严格 CSP 因此不需要 `unsafe-inline`）：

```js
"use strict";
document.addEventListener("DOMContentLoaded", function () {
  EpoCHA.render(document.getElementById("captcha"), {
    apiBase: "https://captcha.example.com",
    siteKey: "pk_your_site_key",      // 浏览器侧只出现 site_key
    language: "zh-CN",                // 或 'en'
    fieldName: "epocha_response",     // 与后端读取的字段名保持一致
    onSuccess: function (token) { /* UI 信号，**不是**授权 */ }
  });
});
```

服务端（业务后端，**永远不要在浏览器里做**）：

```bash
curl -sX POST https://captcha.example.com/siteverify \
  --data-urlencode "secret=sk_…" --data-urlencode "response=$TOKEN"
# {"success":true,...} 才执行受保护操作
```

`onSuccess` **不是**授权信号：它是浏览器里的回调，攻击者可以随意伪造"看起来成功"的页面状态。
授权只由服务端 `/siteverify` 的 `success:true` 决定 —— `cmd/exampleapp` 的页面故意让提交按钮**一直可点**，
就是为了让人亲眼看到"没验证也会被服务端拒"。

上面的 `<script src="https://captcha.example.com/…">` 是**你自己的 epocha 源站**，不是公共 CDN。
如果改从第三方 CDN 分发，请按 §6 自托管或加 `integrity` + `crossorigin`：上游被攻陷就等于
在你的 origin 上执行任意代码。

### 1.1 容器不在 form 内怎么办

隐藏字段落在 form 外等于什么都没提交，所以组件**不会**静默放过这种情况：
`fieldName` 非空但容器不在任何 `<form>` 内时，`render()` 直接 `throw TypeError`，
且不会留下一个孤儿 input。三种正确写法（都有浏览器用例覆盖）：

```js
EpoCHA.render(el, { …, fieldName: "" })                          // 只要回调/getResponse，不要字段
EpoCHA.render(el, { …, form: document.getElementById("order") }) // 传 form 元素
EpoCHA.render(el, { …, form: "order" })                          // 或传同文档内 form 的 id
```

`form` 只接受**当前文档里的真实 `<form>`**（元素或其 id）。选择器字符串、跨文档元素、
不存在的 id 都抛错 —— 目的是把"随便挑一个表单"这种默认行为挡掉：字段进了错误的表单，
比没有表单更难排查。

## 2. API

| 调用 | 含义 |
|---|---|
| `EpoCHA.render(container, options) → id` | 在容器上挂载一个实例并**立刻领题**；返回实例 id |
| `EpoCHA.getResponse(id) → string` | 当前可用的一次性 response token；**没有就返回 `""`**（过期/答错/重置/销毁后一律空） |
| `EpoCHA.reset(id)` | 取消在算/在飞的活儿，清空 token、输入与隐藏字段，回到 `idle`（**不**自动领新题） |
| `EpoCHA.refresh(id)` | 等价于点「换一张」：取消旧的，领一道新题 |
| `EpoCHA.destroy(id)` | 摘事件监听、清计时器、terminate worker、`revokeObjectURL`、移除 shadow host 与隐藏字段、注销注册表 |
| `EpoCHA.state(id)` / `EpoCHA.ids()` | 只读观测（集成方调试 / 测试用） |
| `EpoCHA.util` | 纯函数（`leadingZeroBits`、`b64urlToBytes`、`lang`），不含任何密钥或状态 |

`options`：

| 键 | 默认 | 说明 |
|---|---|---|
| `apiBase` | **必填** | 验证服务基址（widget 只打它的 `/api/challenge`、`/api/redeem`） |
| `siteKey` | **必填** | 公开站点标识。形态不合法直接 `throw`（不静默降级） |
| `language` | `navigator.language` | `zh-CN` / `en`（其他值按前缀归一到这两档） |
| `fieldName` | `"epocha_response"` | 隐藏字段名；给 `""` 就**不**创建字段。非空时容器必须在目标 form 内，或用 `form` 关联，否则 `render()` 抛 `TypeError` |
| `form` | — | 目标 `<form>` 元素或其在**当前文档**里的 id。容器在 form 外时用它显式关联 |
| `onSuccess(token, id)` | — | 验证通过一次（**每个 token 只触发一次**） |
| `onExpired(id)` | — | token 过期：同时**已清空** token 与隐藏字段 |
| `onError({code,message,retryable,widgetId})` | — | 受控错误：`code` 是固定集合，`message` 是本地化文案，**不含服务端原文** |
| `workerUrl` | `""` | 同源 worker 文件（方案 B）。跨域 URL 直接 `throw` |
| `powTimeoutMs` | `20000` | 1s–120s。到点取消计算并进入 `error` |
| `powMaxTries` | `5e7` | 搜索预算上限（另受 `2^(bits+6)` 与服务端 nonce 上界 1e12 约束） |
| `fetchTimeoutMs` | `10000` | 单次 HTTP 超时（AbortController） |
| `maxAutoRetry` | `2` | **仅**对 `503 challenge-unavailable` 的自动重试次数（尊重 `Retry-After`），其他错误一律人工重试 |
| `showExperimentalNote` | `true` | 组件内的"实验性题型"标注；关掉它不等于这道题变得生产可用 |
| `accessibility` | `false` | 显式申请纯文本无障碍通道；服务端仍要求站点池包含 `accessible-choice-v1`，且 24h 限额 |
| `onState(state, id)` | — | 状态观测回调 |

### 2.1 题型与输入控件（ADR-024 / ADR-025）

widget 按服务端 `question.answer_type` 选择控件；接入方不需要、也不能指定题型：

| `answer_type` | 控件 | 提交串 | 边界 |
|---|---|---|---|
| `text` | 文本输入（`text-image-v1/v2/v3`） | 规范化后的 6/8 字符 | v3 的题面带 `tolerant:1`，组件会显示"允许 1 个字符认错"的说明 |
| `cells` | 图片上的可聚焦格子按钮（`shape-grid-v1`） | 1-based 序号排序去重后的逗号串（`3,7`） | 按钮位置只取自题面 `grid_*` 几何；几何越界即 fail closed |
| `pairs` | 两个数字输入（`shape-count-v1`） | `a,b` | 两个字段都有值才允许提交 |
| `angle` | 角度滑块 + 图片拖拽辅助线（Phase 1/2） | 0.1° 整数（`0..3599`） | 题面给 `angle_min/angle_max/angle_step`；有 `cue_x/cue_y/aim_len` 时显示可拖拽瞄准线，键盘仍可用滑块微调 |
| `point` | 图片点击/键盘移动（Phase 1 契约） | `x,y`，各 `0..999` | 坐标按图片宽高归一化；点击或方向键设置，不读取图片内容 |
| `timing` | 雪碧图动画 + “记录时机”（Phase 1 契约） | 毫秒 `0..60000` | 必须 `render_mode:"sprite_sheet"`；减少动态时 fail closed；时序题不提供暂停 |
| 任意 `sprite_sheet` 题型 | 暂停/继续、重播 | — | 仅非 timing 模式显示；时间轴由 Web Animations 驱动 |
| `media:"text"` + `options` | 文本选项按钮 | 选项字母 | 无障碍通道；不加载图片，键盘可直接选择 |

Widget 会上报只含 `total_ms/first_input_ms/event_count/pointer_events/key_events/focus_blur` 的摘要；
不采集原始坐标/按键内容。服务端只把它当弱证据，不改变显式判定。

Phase 4 的 `energy-window-v1` 复用 `timing` + `sprite_sheet`，`pipe-connect-v1` 复用 `cells`；两者都不新增前端输入模式。
Phase 4.5 的 `intersection-point-v1` 复用 `point`：点击层写入 `x,y`，方向键每次 10、Shift+方向键每次 1，
选中点有可视标记与 `aria-label` 播报；同样不新增输入模式。

当前 Phase 2 的 `parallax-code-v1` 使用 `text` + `render_mode:"sprite_sheet"`，并由播放条提供暂停/继续/重播；
`bank-shot-v1` 使用 `angle` + `cue_x/cue_y/aim_len`，图片上的拖拽层与滑块共享同一个角度值。

`cells` 的按钮用百分比定位，随图片缩放；`aria-pressed` 反映选中，Space/Enter 可切换，
再次点击取消。`pairs` 的标签取自题面的 `labels_zh/labels_en`，只进 `textContent`。
`angle`/`point`/`timing` 是 ADR-025 的 Phase 1 契约输入：**当前没有生产生成器登记**，
现有 6 个题型仍只用 `text`/`cells`/`pairs`。所有模式最终都写入同一个 `answer` 串并按
`/api/redeem` 提交；widget 不做判定，也不持有答案或物理碰撞终点。
主题型清单与 M1–M4 见 `docs/QUESTION-TYPES.md`。

## 3. 状态机

```
idle → loading → ready → solving → submitting → verified
         ↑          │         │         │           │
         │          ↓         ↓         ↓           ↓
         └──(refresh)      error     error      expired（token 已清空）
                                   （所有状态都可被 destroy() 打断 → destroyed）
```

行为保证（都有测试；浏览器侧那部分见 `docs/STAGE4A.md` §3）：

- **一个实例同一时刻只有一个有效任务**：每次领题 `gen++`；`gen` 不匹配的响应/计算结果一律丢弃。
  晚到的旧响应**不会**覆盖新题（实测断言：界面上显示的图 = 最后一次请求的图）。
- 重复点提交不会二次兑换（`submitting/solving/verified` 直接 return）。
- `verified` 之后 `onSuccess` 不再触发；`onExpired` 只在 token 真的作废时触发一次。
- 过期**清空 token**，不只是改 UI（`getResponse()` 返回 `""`，隐藏字段一起清空，`onExpired` 恰好一次）。
  这一条在浏览器里用 `response_ttl_sec=2` 的演示站点真跑过：等到服务端 TTL 到期后断言三者同时成立。
- 标签页后台挂起后恢复：立刻用 `Date.now()` 复核 deadline，过期就走过期路径。
- 客户端倒计时只服务体验；**服务端裁决才是最终依据**（`expires_in` 来自服务端；
  服务端没给就不设倒计时、不猜"固定两分钟"）。
- 503/限流/网络错误/PoW 超时/CSP 禁 Worker 都给出可理解状态 + 受控重试；
  **不静默降级成 PoW-only**，不无限自动刷新。

`onError` 的 `code` 集合：`network` `timeout` `rate-limited` `challenge-unavailable`
`invalid-challenge` `invalid-request` `expired` `worker-blocked` `worker-unsupported`
`worker-unavailable` `pow-timeout` `pow-exhausted` `image-unusable` `bad-response`
`interactive-unavailable` `motion-required` `http-<status>`。

## 4. 隐藏字段（以及 Shadow DOM 的那个坑）

**Shadow DOM 里的 `<input>` 不参与表单提交。** 所以字段建在容器的 **light DOM**：

```html
<div id="captcha">
  <input type="hidden" name="epocha_response" data-epocha-field="epocha-3">  <!-- light DOM -->
  <div data-epocha-widget="epocha-3">#shadow-root</div>
</div>
```

- 字段名可配（`fieldName`），值**只**放 response token。
- **字段必须属于目标 form**：容器在 form 内 ⇒ 字段建在容器里（跟着容器一起移动/移除）；
  容器在 form 外 ⇒ 必须用 `form` 选项关联，否则 `render()` 抛 `TypeError`（不静默造一个提交不到的字段，见 §1.1）。
- `reset` / `refresh` / 过期 / 答错 / `destroy` 都会清空或移除它。
- 多实例各写自己那个字段：同名字段落在不同 `<form>` 里互不覆盖（实测断言）。
- 上面这些不是推断：`/fixture` 页面用 `new FormData(form)` 与**原生 form POST** 双重验证过
  ——`FormData` 里的值与 `getResponse()` 一致，且服务端确实收到并写库（`docs/STAGE4B.md` §2）。

## 5. PoW 与 Worker

- 算法 `sha256-prefix`：`digest = SHA256(seed || 十进制 nonce 的 ASCII)`，通过条件为摘要前导零比特 ≥ `bits`。
  服务端以库中行为权威复核，客户端不能自报参数。
- **计算在 Dedicated Worker 里**，页面输入与按钮不被阻塞；worker 每 20000 次让出一次事件循环，
  因此 `cancel` 能被处理（同步死循环会吞掉取消消息）。
- 纯 JS SHA-256（不用 `crypto.subtle`）：每次摘要都跨异步边界时吞吐只有 2–4k 次/秒，
  `bits=18` 需要几十秒，只能靠降低服务端难度掩盖——那是红线。实测吞吐见 `docs/STAGE4A.md` §4。
- 一致性靠**共享固定向量**证明，不是"两端各自看起来通过"：
  `internal/pow/testdata/pow_vectors.json` 由 Go 实现生成并断言，Node 侧用 worker 的原始字节
  在 `vm` 里断言同一份文件（`make widget-checks`）。
- 两种 worker 加载方式，**至少实现一种**（本 widget 两种都实现了）：
  - **A（默认）**：脚本内嵌同一份 worker 源码，运行时建 Blob Worker。需要页面 CSP 允许 `worker-src blob:`。
  - **B**：接入方把 `v1.worker.js` 与页面**同源**部署，用 `workerUrl` 指定。`workerUrl` 只接受同源 URL，
    否则 `throw`（不接受远程代码地址）。两条路径跑的字节由防漂移测试钉住必须逐字节相同。
- CSP 禁 Worker 或浏览器无 Worker 时：`error` + `worker-blocked` / `worker-unsupported`，
  **绝不**回落到主线程"假装算完"，也不假装验证完成。
- 不用 `eval` / `new Function` / 动态 `import()`，不让服务端 challenge 指定可执行脚本 URL。

## 6. CSP 清单（生产接入需要的）

```
script-src  'self' https://captcha.example.com          # 或把 v1.js 自托管后只留 'self'
connect-src https://captcha.example.com                 # /api/challenge /api/redeem
img-src     data: 'self'                                # 题图是内联 data URL
worker-src  blob:                                       # 方案 A
            # 或 worker-src 'self' 并把 v1.worker.js 放到同源（方案 B）
```

- **不需要** `unsafe-eval`，也不需要 `unsafe-inline`（widget 的样式用构造式样式表
  `new CSSStyleSheet()` + `replaceSync` + `adoptedStyleSheets`，不受 `style-src` 约束）。
- 诚实边界：老浏览器（Safari < 16.4 等）没有构造式样式表时会回落到 `<style>` 元素，
  那时需要 `style-src 'unsafe-inline'`。这条不是"支持所有严格 CSP"的承诺。
- 不给任何 CORS 头：`<script src>` 跨域加载不需要它。若接入方启用 COEP，需要自托管脚本
  （或用带 `Cross-Origin-Resource-Policy: cross-origin` 的 CDN），本服务不设该头。
- 若你**不是**从自己的 epocha 源站加载，而是经第三方 CDN 分发：请自托管这份脚本或加
  `integrity` + `crossorigin`，否则 CDN/上游被攻陷就等于在你的 origin 上执行任意代码。
  同源自托管不需要 SRI（此时风险在服务器本身，而不在传输链路上）。
- `default-src 'none'` 的页面若用本仓库的示例后端，需要 `style-src 'self'` + `form-action 'self'`
  等（示例页面刻意不用内联 `<style>/<script>`，见 `cmd/exampleapp`）。

## 7. 实测踩到并钉住的实现细节

- **不用 `http.ServeContent`**：当系统没有 `/etc/mime.types` 时它会嗅探内容并把
  `Content-Type` 覆盖成 `text/plain`；配上 `nosniff`，浏览器直接拒绝执行脚本
  （`blocked due to MIME type ("text/plain")`）。装了 mime 表的开发机上单测却全绿，
  所以这条由 `web/widget/widget_test.go` + 浏览器闭环双重钉住。
- worker 的 `started` / `progress` **不是终态**：`_dispatch` 对非终态消息不摘除 job 登记。
  （曾经无条件 `delete`，于是 `started` 摘掉登记、真正的 `solved` 无处可寻，组件卡在 `solving` 到超时。）
- 隐藏字段必须在 light DOM（§4）。
- 领题成功后**立刻**建立 worker：环境不兼容（CSP）在用户输入答案之前就报出来。
- 坐标点击 / `scrollIntoView`：BiDi 的鼠标动作对视口外坐标报 `move target out of bounds`。

## 8. 无障碍（诚实说明）

已做：`<label for>` 绑定输入框；`role="status" aria-live="polite"` 播报；错误不只靠颜色
（`data-kind` + 文案，输入框 `aria-invalid`）；键盘可完成整题（Tab 到达输入框与按钮、Enter 提交）；
不自动抢焦点（除用户主动交互外不 `focus()`）；图片 `alt` 只描述用途**不含答案**。

**窄屏是真实测量，不是推断**（阶段 4B）：用 W3C BiDi `browsingContext.setViewport`
把 Firefox 147.0.2 的视口**真的**设成 375×667 与 414×896（先断言 `window.innerWidth`
与 `documentElement.clientWidth` 等于目标值，再量布局），检查文档无横向溢出、
组件盒不超右缘、图片完整且比例不变形、输入框与两个按钮在 shadow DOM 内**中心点可命中**、
错误文案不撑破状态区，并在 375 下用真实鼠标点击 + 键盘输入走完验证与业务提交。
这不等于真机移动端测试：`devicePixelRatio` 是被设进去的 2，不是某台设备的像素密度；
没有触屏手势、没有浏览器工具栏占用、没有低端 Android WebView 的排版差异。

**未做，也不声称已做**：

- 这是**纯视觉**题型。无法看清图片的用户本阶段**无法完成验证**。
  非视觉等价通道（音频或其他）尚未实现 —— 这是产品缺口，记录在 DESIGN §17。
- **PoW-only 不是等效替代**：它换掉的是"题目"这一层，防线少一层，安全含义不同，
  不能被称为无障碍等价方案。
- 未做完整 WCAG 合规验证（无辅助技术实测、无屏幕阅读器走查）。

## 9. widget 明确不做的事

- 不含 `secret` / 管理密钥 / pepper；不请求 `/api/admin/*`；不调用 `/siteverify`（那是服务端到服务端的）。
  公开请求一律 `credentials: 'omit'`。
- 不请求管理 API、不提供"跳过验证"分支、不返回答案，也不提供任何答案出口。
- 不引入 iframe（直接 Shadow DOM 挂载）。**Shadow DOM 不是安全边界**：它隔离样式与命名，
  同源脚本照样能读它的内容；不要把它当成"防篡改"。
- 不因为换了挂载方式就改来源语义：Origin 仍由服务端裁决。用 iframe 换 Origin 之后再拿
  客户端自报的父域名冒充可信来源，是明确禁止的做法。
- `?site_key=` 必须留在 URL 上（CORS 预检看不见请求体）。服务端另有规则：
  URL 与请求体同时给出 site_key 时**必须一致**，冲突即拒（见 DESIGN §10.3）。
