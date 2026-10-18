# STAGE4B.md — 阶段 4B：接入收尾、真实窄屏、离线 OCR 基线、人工盲测入口（2026-10-18）

范围：把 4A 留下的三处"能用但没被真正验证"补成真实证据，并交付一个能跑的 OCR 基线。
**不做**新题型、风险打分、自动淘汰、后台页面、CF 边缘、公网部署，也不动 `production_ready:false`。

上一轮（4A）的报告状态已过时：本轮进入时 HEAD 是 `1812688`，`stage-4a-done` 固定在 `bd0f314`（未移动）。

---

## 1. 交付物与职责

| 路径 | 职责 |
|---|---|
| `cmd/exampleapp` `/fixture` + `/fixture-init.js` + `POST /guestbook-form` | 文档 §1 最小接入示例的**可运行版本**：容器在 form 内、两个脚本都 defer、初始化走外部同源文件、原生表单 POST |
| `web/widget/v1.js`（`form` 选项） | 隐藏字段的归属规则：容器不在目标 form 内就必须显式关联，否则 `render()` 抛 `TypeError`，不静默留孤儿字段 |
| `docs/scripts/browser/bidi.mjs`（`setViewport` / `viewportMetrics` / `subscribeEvents`） | W3C `browsingContext.setViewport` 的真实客户端实现 + 读回浏览器自报度量 |
| `docs/scripts/browser/widget_browser.test.mjs` | 21 条浏览器交互；新增 4 条窄屏（真实视口）与 4 条 fixture/字段归属用例，**0 条 skip** |
| `internal/eval` + `cmd/evalocr`（`Normalizer`） | 评分归一化改为注入 `(string, bool)`，默认直接取生产 `generator.TextImage.NormalizeAnswer`；畸形输出单独计 `Malformed` 且留在分母里 |
| `docs/scripts/ocr/{Dockerfile,ocr1.sh,run-baseline.sh}` | 隔离 OCR 容器（官方 ubuntu + 官方仓库 tesseract）、单图调用器、dev 调参 + dev/holdout 正式测量 |
| `cmd/blindtest` + `docs/scripts/blindtest.sh` | 一条命令启动的人工可读性盲测入口（临时密钥/库/端口，导出无凭据 CSV/JSON） |
| `docs/scripts/stage4b.sh` + `make stage4b-check` | 阶段 4B 门禁（含容器隔离实证与评分口径回归守卫） |

## 2. 接入示例：两类只在浏览器里现形的错误

4A 的文档示例有两种错法，都不是文字问题：

1. **脚本顺序竞争**：跨域 `<script src=…/v1.js" defer>` 后面跟一个**同步**内联脚本立刻 `EpoCHA.render`。
   `defer` 的脚本晚于同步脚本执行，于是首屏偶发 `EpoCHA is not defined`；开发机缓存热了就复现不出来。
   ⇒ 现在两个脚本**都带 defer**（deferred 脚本按文档顺序执行，这是唯一确定的保证），
   且初始化放在**外部同源文件**里，严格 CSP 不需要 `unsafe-inline`。
2. **容器在 form 外**：Shadow DOM 的坑修好之后（字段落在 light DOM），字段仍然可能不属于目标表单——
   页面显示"验证通过"，POST 里根本没有 token。
   ⇒ widget 现在要求：`fieldName` 非空 ⇒ 容器必须在某 `<form>` 内，或用 `form` 选项关联到
   当前文档里的真实 form；两者都不满足就 `render()` 抛 `TypeError` 且不留下 input。

验证方式不是 grep 文档字符串，而是浏览器真加载 + 真提交（`docs/scripts/browser/widget_browser.test.mjs`）：

- `文档最小示例 fixture：deferred 脚本顺序确定…` —— 容器确实在 `#order` 内、字段 `form.id == "order"`、
  加载期无 `ReferenceError`（订阅 `log.entryAdded` 取证）。
- `文档最小示例 fixture：FormData 带上 token，原生 form POST 真写入…` ——
  `new FormData(form).get('epocha_response') === EpoCHA.getResponse(id)`，字段集合恰为
  `{text, epocha_response}`；点原生提交按钮触发**真实导航**，结果页 `data-status="201"` 且留言数 +1；
  同一页面不做验证直接提交 ⇒ `403 epocha-missing-response`（`onSuccess` 不是授权）。
- `隐藏字段的三种归属…` —— form 外无关联必须抛 `TypeError` 且零孤儿字段；显式 `form` 关联把字段送进目标 form
  且容器可以在 form 外；`fieldName:''` 一个字段都不建。
- `fixture：reset 从 FormData 里拿掉 token，destroy 移除字段本身`。

顺带修掉一个真实误拒：Firefox 在原生 form POST 上会发 **`Origin: null`**（不透明来源的表示），
而 `sameSite` 把 `"null"` 当 URL 解析得到空 Host ⇒ 每次真实表单提交都被当成跨站拒掉。
现在 `null` 回落到 `Sec-Fetch-Site` 判定（同样是浏览器强制、脚本改不了的头），
并有 Go 测试覆盖 `null + same-origin` 放行 / `null + cross-site` 拒 / 真跨站 Origin 仍拒。

表单路径与 JSON 路径共用同一个 `commitProtected`：两份实现在"什么算已授权"上不可能漂移；
表单版同样走**字段白名单**（多带 `target`/`secret`/`api_base` 一律 400 且不调用验证服务），
否则"浏览器指定验证目标"会从 POST 这条路绕回来。

## 3. 窄屏：真实视口，以及一个被纠正的旧结论

**旧结论是错的。** 4A 记的是"Firefox BiDi 不提供视口设置命令 ⇒ 375px BLOCKED"。
实际情况：测试只试过 `browsingContext.resize` 与 `emulation.setViewport`，这两个命令**不存在**于
W3C WebDriver BiDi；规范里的 `browsingContext.setViewport` 在 Firefox 147.0.2 上真实生效。

能力实测（原始请求/响应都记下来了）：

| 请求参数 | 结果 | 页面自报度量 |
|---|---|---|
| `session.new` 能力声明 | `setWindowRect: true`，`browserVersion: 147.0.2` | — |
| 基线（不调 setViewport） | — | `innerWidth=1366 clientWidth=1366 innerHeight=682 devicePixelRatio=1` |
| `{viewport:{width:375,height:667}, devicePixelRatio:2}` | `result={}`（成功） | `innerWidth=375 clientWidth=375 innerHeight=667 devicePixelRatio=2` |
| `{viewport:{width:414,height:896}}` | 成功 | `innerWidth=414 clientWidth=414 innerHeight=896` |

所以窄屏 4 条是**执行并断言**的用例（不是 skip）：先断言 `window.innerWidth` 与
`documentElement.clientWidth` 真的等于目标值，再量——

- 文档与 `body` 无横向溢出（`scrollWidth ≤ innerWidth + 1`）
- 组件盒不超视口右缘、不塌成零尺寸
- 题目图片完整可见（`rendered.width ≤ vw`）、比例不变形（`natural` 与 `rendered` 的宽高比差 < 2%）、
  没被压到不可读的尺寸
- 输入框与两个按钮**中心点可命中**：命中测试在 `shadowRoot.elementFromPoint` 里做
  （`document.elementFromPoint` 会把 shadow 内容重定向成宿主，那样"可命中"就退化成"页面上有个块"）
- 错误文案不撑破布局：用 CSP `worker-src 'none'` 那一档逼出真实 `error` 长文案再量
- 375 下用**真实鼠标点击 + 键盘输入**走完验证与业务提交
- 414×896 档回归，并确认 `:host{max-width:360px}` 让它不被拉宽到 414

**这不等于真机移动端测试**：`devicePixelRatio` 是被设进去的 2，不是某台设备的像素密度；
没有触屏手势、没有浏览器工具栏占位、没有低端 Android WebView 的排版差异。这些留在人工验证项里。

## 4. 离线 OCR 基线（第一次有真实数字）

### 4.1 工具与隔离

本机没有任何 OCR 可执行文件，但有 Docker ⇒ 按本轮授权**构建一个隔离评测容器**，
不碰宿主系统、不装新浏览器框架、不提权。

| 项 | 值 |
|---|---|
| 基础镜像 | `ubuntu:24.04` @ `sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac`（linux/arm64） |
| OCR | tesseract-ocr **5.3.4-1build5** arm64（leptonica-1.82.0，Found NEON） |
| 英文模型 | `tesseract-ocr-eng` **1:4.1.0-2**，`eng.traineddata` 4,113,088 B，md5 `d1be414fbb296b3ad777bfca655e194e` |
| 镜像体积 | 272 MB（构建期下载基础镜像 + apt 包；识别阶段无网络） |
| 运行约束 | `--network none`、`--read-only`、`--cap-drop ALL`、`--security-opt no-new-privileges`、样本目录 `:ro` 挂载、不挂 docker.sock、不 `--privileged` |
| 答案隔离 | `ground-truth.jsonl` **从不**挂进容器；评测编排先把 PNG 复制进一个**只含图片**的暂存目录 |

隔离不是靠注释声明的：`make stage4b-check` 里有三条实证——容器内 `/in` 只列得出图片、
`--network none` 下解析外部主机名必须失败、容器内 `CapEff=0000000000000000`。

生产服务与生产镜像**不含** tesseract（`go list -deps ./cmd/server` 也证明服务二进制不链接
`evalgen/evalocr/blindtest/devseed`）。

### 4.2 口径：先修评分器，再谈数字

`evalocr` 原先自带一个 `upper` 归一化：删掉所有空白再大写。问题是生产端
`generator.TextImage.NormalizeAnswer` 对**含内部空白的串判为畸形**（`400 invalid-request`，连判分都不做）。
用宽容版归一化测出来的"通过率"是服务端永远不会给的值，而且虚高多少取决于 OCR 的输出习惯。

现在默认档就是生产函数本身，并有测试逐串断言两者一致；`upper` 档被**删除**（用它直接报错）。
畸形输出单独计入 `Metrics.Malformed`，且**留在分母里**。这条由 `TestScoreCountsMalformedSeparately`
与门禁第二步（构造 4 张：1 对 / 1 畸形 / 2 工具失败 ⇒ 严格通过率必须是 25.00%，不是宽容口径的 50%）钉住。

### 4.3 调参：只看 dev

`evalgen` 的 dev 集（每档 40 张，确定性 seed），严格通过率 = 正确 / **全部样本**：

| 配置 | d1 | d3 | d5 |
|---|---|---|---|
| psm=7 oem=3 + 字符白名单 | 97.50% | 27.50% | 0.00% |
| **psm=6 oem=3 + 字符白名单** ← 选定 | **97.50%** | **37.50%** | **0.00%** |
| psm=7 oem=1 + 字符白名单 | 97.50% | 27.50% | 0.00% |
| psm=7 oem=1 无白名单 | 95.00% | 7.50% | 0.00% |

选**最强**的一组而不是随便一组：基线要回答"一台 commodity OCR 最多能读成什么样"。
注意白名单是**帮忙**的（把输出空间限制到题目字符集），所以带白名单反而更强——
拿"不加白名单的 7.5%"去说"OCR 读不出来"就是自欺。

holdout 集在这个阶段完全没有参与参数选择。

### 4.4 正式结果（dev 与 holdout 各 3 档 × 100 张，固定配置 psm=6 oem=3 + 白名单）

`psm=6`：整块版面自动切行；`oem=3`：默认引擎；白名单 = `ABCDEFGHJKLMNPQRSTUVWXYZ23456789`（32 字符）。
预处理：**无**（不做缩放/二值化/去噪声，直接用 `evalgen` 产出的 PNG）。

| 档位 | 样本 | 严格通过率（分母=全部样本） | Wilson 95% CI | 答错 | 畸形 | 工具失败 | 字符级* |
|---|---|---|---|---|---|---|---|
| d1（低） | 100 | **96.00%** | [90.16%, 98.43%] | 1 | 3 | 0 | 99.83% |
| d3（中） | 100 | **31.00%** | [22.78%, 40.63%] | 16 | 52 | 1 | 93.62% |
| d5（高） | 100 | **3.00%** | [1.03%, 8.45%] | 6 | 81 | 10 | 85.19% |

\* 字符级正确率只统计"合法且与答案等长"的输出，**不能**当整题通过率读：d5 的 85.19% 说的是
"平均每 6 个字符读对 5 个多"，而服务端要的是整串精确匹配（容差 0），所以那一档的接受率是 3%。

dev 侧同一配置的对照：d1 97.00% / d3 35.00% / d5 1.00% —— 与 holdout 相差 ≤4 个百分点，
说明这个测量不是"挑出来的样本"。

完整流水线单张耗时（含 `docker run` 启动 + tesseract 识别 + 评分）：
d1 min 459 / median 600 / mean 606 / max 962 ms；d3 median 546 ms；d5 median 578 ms（n=90）。
**这是这台开发机上的流水线吞吐，不是攻击者的边际成本**：真实攻击会复用进程、并行、并且不计评分开销。

### 4.5 这些数字允许说到什么程度

允许：「在该固定工具（tesseract 5.3.4 + eng 4.1.0 模型）、该固定参数、该样本集、该机器下的基线表现」。

**特别要说清的一条负面结论**：低档（difficulty 1）**基本挡不住通用 OCR**——100 张里 96 张被整串读对。
对照随机猜测的单题概率 `32^-6 ≈ 9.31e-10`（100 张里猜中≥1 的期望约 9.3e-8），
96% 与 3% 都不可能来自猜测，只能来自实际读出字符。
⇒ 生产默认不应落在低档；`text-image-v1` 的 `experimental:true` / `production_ready:false` 不因
高档数字好看而改变。

不允许（都没测过，也不据此起结论）：抗其它 OCR、抗多模态大模型、抗人+工具组合、
真实攻击成本 M4、跨设备/跨字体渲染差异、以及任何形式的"生产安全认证"。
本阶段没有做自动淘汰，也没有因为哪个数字改变任何生产配置。

## 5. 人工可读性盲测入口

一条命令：

```bash
make blindtest            # = bash docs/scripts/blindtest.sh
# 终端会打印：盲测入口：http://127.0.0.1:8491/
```

它拉起并只清理自己创建的东西：新 `mktemp` 目录（SQLite 库、题目图、答案表、记录文件）、
运行时随机生成的 master key / pepper / admin key / 站点 secret、三个演示站点
（difficulty 1/3/5，`pow_bits=12`——盲测要量的是可读性，不该被计算成本干扰）、
每档注入 `batch+4` 道题、`cmd/blindtest` 记录服务（仅 loopback）。
开头会检查：环境里已有 `EPOCHA_*` 就直接拒绝启动（不与现有配置混用），端口被占就拒绝启动
（否则真人做到一半发现连的是上一轮的遗留实例）。

页面契约：一次一题、每档固定 20 题、**答题前不显示标准答案**、不记录用户输入的字符。
三段耗时按 widget 的公开观测面拆开：

- 答题 = `ready → solving`（进入 solving 的时刻近似"按下提交"）
- PoW = `solving → submitting`
- 兑换 = `submitting → 终态`

诚实标注：**兑换那一段无法再拆成"网络 RTT"与"服务端处理时间"**——那需要在生产服务端插桩，
本工具不做，也不假装能分。另外 JS 定时器精度受浏览器后台节流影响，所以这些数是量级证据。

结果分类：`verified / wrong / timeout / error / unreadable`，其中
"这题我看不清"是用户主动点的 `unreadable`，**不混进** `error`（那会被读成组件故障）。
导出 `/export.csv` 与 `/export.json` 带会话号，字段只有档位、序号、结果、三段耗时、换题次数、时间戳；
不含 secret、不含 site_key、不含输入原文（由 `TestExportCarriesNoCredentialsOrInput` 断言）。

本轮实测（不依赖真人）：三个档位逐档 `POST /api/challenge` 均 200 且响应含题图；
页面配置解析正确；退出后端口 8491 与临时目录都被清干净。

**这个入口第一版是坏的，而且是"真加载页面"才发现的。** 我最初只验了"能领题 + 参数解析 + 导出字段"，
就把入口记成已验证；随后在真实 375px 视口里加载它做截图取证时，看到的现象是
页面正常显示、档位按钮能点、**但组件永远不出现**（进度停在 `0 / 20`），
`EpoCHA is not defined` —— 因为 `pageHTML` 里**根本没有引入 widget 脚本的 `<script src>`**。
修复见后续 commit（`{{WIDGET_TAG}}` 注入 + deferred 顺序），并补了三道守卫：

- Go 测试断言页面里存在 `defer` 的 `/widget/v1.js` 标签、且排在 `/app.js` **之前**；
- `blindtest.sh` 启动后从页面里取出该 URL，真实 `curl` 一次并核对 200 / `text/javascript` / 内容特征；
- 修复后在真实浏览器里复验：`{hasEpoCHA:true, states:["ready"], item:"1"}`，
  截图 750×1334 设备像素 = 375×667 CSS px @ DPR 2（视口确实是 375，不是缩了个容器）。

教训写在这里而不是只写在 commit 里：**"入口可用"的断言必须由加载它的执行环境给出**。
逐档领题成功只证明服务端与题库就绪，不证明页面能把组件挂起来。

**人工可读性本身仍是 NOT_RUN**：agent 不代填，需要真人做题。

## 6. 验收状态（三态，不混用）

| 分类 | 状态 | 依据 |
|---|---|---|
| 4B 工程测试（blindtest / eval / evalocr / exampleapp / widget / api） | **PASS** | `go test -count=1` 各包通过；整套 `-race` 在 stage4a.sh 里跑一次 |
| OCR 评分口径回归守卫 | **PASS** | 含内部空白的输出按畸形计入分母 ⇒ 严格通过率 25.00%（宽容口径会给出 50%） |
| OCR 容器隔离实证 | **PASS** | `/in` 只见图片；`--network none` 下解析外部主机失败；容器内 `CapEff=0` |
| OCR 基线流水线 | **PASS** | dev + holdout 各 3 档跑通，样本数逐档核对 |
| 浏览器交互（含窄屏与 fixture） | **PASS** | 21 条声明 = 21 pass / 0 skip，逐条按标题核对 |
| 真实窄视口 375 / 414 布局与流程 | **PASS** | 见 §3，断言 innerWidth/clientWidth 后再量 |
| 真机移动端（触屏、DPR、WebView 差异） | **NOT_RUN** | 本轮无设备；Emulated 视口不等于真机 |
| OCR **效果数字** | 已测量，见 §4 | 不并入工程 PASS 计数 |
| 人工可读性（真人做题） | **NOT_RUN** | 需要人；入口见 §5 |
| 非视觉等价通道 / 完整无障碍合规 | **NOT_RUN** | 本阶段仍只有视觉通道 |
| 生产适用性 | **未论证** | `text-image-v1` 仍 `experimental:true` / `production_ready:false`；§4.4 的低档结论是**反对**投产的证据 |

## 7. 退出码

`make stage4b-check` 一次跑完并逐条记录；`docs/scripts/ocr/run-baseline.sh` 走"先 build 再跑二进制"，
所以 `evalocr` 的 3（BLOCKED）不会被 `go run` 折叠成 1、也不会被 `make` 折叠成 2（同 4A 的 §5）。

## 8. 与既有阶段的关系

- 阶段 1/2/3A/3B/4A 的验收由 `stage4a.sh` 各跑一次（`make stage4b-check` 只调它一次，不重复 `-race`）。
- 后端行为**没有放宽**：新增的都是收紧（表单字段白名单、`Origin: null` 的正确处理仍拒真跨站）。
- 唯一新增的公开面是 exampleapp 的 `/fixture` 与 `/guestbook-form`（开发期进程，默认 loopback）；
  `cmd/server` 不链接 `cmd/blindtest`（`go list -deps` 证明）。
- widget 体积：`v1.js` 原文 50,784 B / gzip **17,704 B**（预算 30KB 内，`make widget-size` 守护）。
