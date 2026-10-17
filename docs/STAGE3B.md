# 阶段 3B：首个视觉生成器 + 图片资产接线 + 本地预览 + 可复现评测

> **定位（先看这段）**：这一阶段交付的是**图片题的工程链路**——渲染、存储、下发、判分、生命周期、
> 本地预览、离线评测脚手架。`text-image-v1` 是**实验性**题型，管理面里它带着
> `experimental:true` + `production_ready:false`。
>
> **本轮没有证明它抗 AI 或抗 OCR**：本机没有任何离线 OCR 工具（未获授权安装/下载模型），
> 人工可读性也**没有**真实参与者数据。所以本文件把结论分成四条独立记录（§7），
> 不允许把它们混成一句"全部通过"（ADR-002、ADR-006）。
>
> 设计依据：`docs/DESIGN.md` §5.5、§8.5、§10.1、§10.2、§13、§16、§17；决策记录 ADR-012。
> 上一阶段：`docs/STAGE3A.md`。

## 1. 渲染与资产数据流

```
pool worker（事务外）                     SQLite（权威）                 浏览器
─────────────────────────────────       ─────────────────────         ─────────────────
crypto/rand ─── 6 次均匀抽样 → 答案串    pool.payload_json  ← 公开题面（≤4096B）
              ├─ 16B nonce ────────────► pool.verifier_blob ← nonce‖HMAC(K(qver),…)（永不下发）
              └─ 8B 抖动种子              pool.asset_blob    ← PNG 字节（≤65536B，CHECK）
                     │                   pool.asset_mime    ← '' | 'image/png'（CHECK）
                     ▼
             math/rand（每次独立实例）
             逐字测量包围盒 → 布局（不越界/不重叠）→ 旋转合成到 image.Gray
             → png.Encode → 字节数自检
                     │
                     ▼
             InsertQuestion：行数上限 **与** 全局图片字节预算在同一条 SQL 内判定

/api/challenge（pow_question 站点）
  IssueChallengeWithQuestion：一个事务里插 challenge 行 + 条件领取一条 ready 题 + 回读该题
  （只有这一条路径把 asset_blob 读进 Go，且只读被领的那一行）
                     │
                     ▼
  questionDTO = SanitizePublic(payload_json) 白名单字段
              + generator / generator_version
              + media / image_mime / image_base64   ← 图片在这里内联下发
                     │
                     ▼
  浏览器：<img src="data:image/png;base64,…">  → 人读 → 提交答案 + PoW 解
                     │
                     ▼
/api/redeem  QuestionVerifierForChallenge（**只读 verifier 一个字段**）
             → NormalizeAnswer → Verify(HMAC 常数时间) → 同一事务终结
```

三条不显眼但决定正确性的设计选择：

1. **图片走 base64 内联，不新增 `/asset/<id>` 端点**（ADR-012）。DESIGN §10.1 草稿里的
   `GET /asset/<pool_id>/<sig>` **没有采用**：那会凭空造出一个需要防枚举、防跨 challenge 引用的面，
   而 3B 的题目是一次性的——图只该跟着它那张 challenge 出去一次。
2. **判分不读图片**。`QuestionVerifierForChallenge` 只 SELECT `verifier_blob`。
   于是"资产被清理/损坏"在结构上不可能影响验证（`TestTextImageGradingDoesNotNeedAsset` 把资产
   从库里清掉后仍兑换成功来钉这条）。
3. **资产与题目同一行、同生同死**。清理任务删 `pool` 行时 PNG 一起消失，不存在"孤儿文件"要扫，
   也不需要文件路径清理或对象存储。代价是库体积，所以**字节预算与行数上限是两条独立的边界**。

## 2. `text-image-v1` 的取值定义

| 项 | 值 | 说明 |
|---|---|---|
| 长度 | **固定 6** | 可变长度会把"长度"变成答案信息的一部分，并让归一化多一条分支，代价是 M3 变差 |
| 字符集 | `ABCDEFGHJKLMNPQRSTUVWXYZ23456789`（32 个） | 剔除肉眼易混的 I/1 与 O/0；**它是公开常量**，进 payload，不是秘密 |
| 抽样 | `crypto/rand.Int` 逐字符均匀取 | 不用 `math/rand` 抽答案；抖动才用 `math/rand`（每次独立播种，红线 10） |
| 归一化 | 裁首尾 ASCII 空白 → 小写折成大写 → 长度必须恰好 6 → 每个字符必须在字符集内 | 长度不符/内部空白/非 ASCII/含 I·O·0·1 一律**畸形**（400 invalid-request，不消耗作答机会） |
| 接受域 | 1 个规范化串（容差 0） | 大小写折叠只增加"同一答案的写法数"，不改变接受域占答案空间的比例 |
| verifier | `nonce(16B) ‖ HMAC(K(qver), "epocha/v1\|qver\|" ‖ nonce ‖ 0x00 ‖ 答案)`，共 48B | 与 `arithmetic-baseline-v1` **同一条构造**（提到 `generator.go` 共用）；本轮没发明新密码学协议 |

渲染参数**全部有界**，且只有五档（`internal/generator/textimage.go` 的 `textImageTiers`）：

| 档 | 画布 | 字号 | 单字旋转 | 基线抖动 | 字间抖动 | 干扰线 | 噪点 |
|---|---|---|---|---|---|---|---|
| 1 | 280×104 | 40pt | 0° | 0 | 0 | 0 | 0 |
| 2 | 300×112 | 40pt | ≤3° | ±2px | 0..3px | 1 条×3 段 | 150 |
| 3 | 320×116 | 40pt | ≤5° | ±3px | 0..4px | 2 条×4 段 | 300 |
| 4 | 340×124 | 42pt | ≤7° | ±4px | 0..5px | 3 条×5 段 | 450 |
| 5 | 360×132 | 44pt | ≤10° | ±5px | 0..6px | 4 条×6 段 | 600 |

硬上界（独立于档位表，越界即 `ErrBadParams`，**不"夹紧后继续画"**）：宽 ≤640、高 ≤256、
像素总数 ≤120,000、字号 16..96pt、DPI 72..150、旋转 ≤15°、抖动 ≤12px、干扰线 ≤8 条×≤8 段、
噪点 ≤ 像素数的 5%、PNG 编码后 ≤65,536 字节。

**d1 是"干净档"**（零抖动），两个用途：人工可读性的对照条件，以及字节级可复现的渲染。

可读性保证是**几何的、被测试的**，不是"看着还行"：`layoutGlyphs` 先测量每个字形旋转后的包围盒，
再逐字排版，要求 (a) 包围盒完整落在画布内且离边 ≥2px，(b) 与前一字的水平重叠 ≤ 较窄者的 1/4；
做不到就返回错误，由**有界**重试（128 次）处理，最终失败是 `ErrGenerateExhausted`（可重试的生成失败），
**绝不会**发出一张被裁切或字符挤在一起的图。`TestTextImageLayoutGuarantees` 对 5 档 × 5 种极端字串 × 150 次
抖动逐一验证，并断言失败率 <5%。

## 3. 配置与运行命令

新增环境变量（全部有校验，非法值拒绝启动而不是静默夹紧）：

| 变量 | 默认 | 含义 |
|---|---|---|
| `EPOCHA_POOL_IMAGE_LOW_WATERMARK` | 8 | 图片桶低于此值才补题。**不继承**算术桶的 50 |
| `EPOCHA_POOL_IMAGE_HIGH_WATERMARK` | 24 | 图片桶补到此值停止。几十题的量级，不是数万行 |
| `EPOCHA_POOL_IMAGE_MAX_TOTAL_BYTES` | 4194304（4 MiB） | **全局**图片字节预算（ready/assigned/terminal 全算），上界 1 GiB |
| `EPOCHA_POOL_IMAGE_YIELD_MS` | 200 | 补图片题时每次让步时长（算术桶是 100ms）——渲染贵一个量级 |
| `EPOCHA_GENERATORS` | 空=全部 | 题型白名单。写错一个名字**拒绝启动**（静默忽略会让运维以为"已关掉"） |

代码内固定值（不开放为配置，避免配置面变成攻击面）：单题 PNG ≤65,536B（与 `0004` 的 CHECK 同值，
`TestImageConstantsMatchStore` 交叉检查）、MIME 只允许 `image/png`、布局重试 128 次。

```bash
make build            # bin/epocha（含字体，实测 10,813,732 字节）
make check            # fmt-check + vet + race + size
make stage3b-check    # = check + secret-scan + stage1/2/3a-check + docs/scripts/stage3b.sh（22 项）
make eval-samples     # 生成 dev 与 holdout 样本集到 eval-out/（含 ground truth，已 gitignore）
make ocr-baseline     # 跑离线 OCR 基线；本机无工具时以退出码 3 报"未测"
```

## 4. 本地预览（不能只靠 curl 的那部分）

一条命令启动（服务与题池已在别处跑起来的前提下）：

```bash
go run ./cmd/preview \
  --api http://127.0.0.1:8080 \
  --admin http://127.0.0.1:8081 \
  --admin-key "$EPOCHA_ADMIN_KEY" \
  --difficulty 2 --pow-bits 12
# 打印：epocha 预览：用浏览器打开 http://127.0.0.1:8090/
```

它做什么：用管理 API **创建自己的演示站点**（`pow_question` + `text-image-v1` + 指定难度 +
来源白名单=它自己的 loopback 来源），把站点 secret 只留在自己进程内，然后服务一个原生单页。
已有站点时可用 `--site-key` + `--site-secret` 跳过建站（此时需自行保证来源白名单含预览来源）。

浏览器里的操作顺序：

1. 打开页面 → **自动领第一题**（每次都是新 challenge，受服务端限流与题池容量约束；旧题不回池）。
2. 看图，在输入框里敲 6 个字符（大小写不敏感；`Tab` 到输入框、`Enter` 提交，全程可键盘完成）。
3. 点「提交答案」或按 Enter → 页面**在本地用 WebCrypto 暴力求解 PoW**（状态区显示进度，
   服务端不代算），然后 `POST /api/redeem`。
4. 拿到一次性 `response` 后，页面把它 `POST` 给**同源**的 `/demo/verify`；演示后端带着自己的
   secret 调 `/siteverify`，只把 `success` / `error-codes` / `hostname` / `challenge_ts` 回给浏览器。
5. 状态区给出结果：成功（绿）、答案错（红，并说明这张题已作废）、暂时给不出题（503，提示稍后重试）。
6. 「取一张新题（换题）」= 一次全新 challenge。
7. 展开「盲测模式」→ 选题数与难度档 → 开始：逐题作答，**输入前不显示标准答案**，
   结束后打印完成率 / 错误率 / 换题次数 / 平均 PoW 耗时。

安全边界（都有测试）：

- 默认且**强制** loopback：`--listen 0.0.0.0:…` 直接拒绝启动（`TestListenLoopbackIsCheckNotDefault`）。
- secret 不进 HTML、JS、页面配置或任何响应（`TestSecretNeverReachesBrowser` 按值扫）。
- 浏览器**不能**指定验证目标：`/demo/verify` 只接受一个 `response` 字段，多带 `target`/`url` 一律 400，
  且被拒的请求一次都不会触达上游（`TestVerifyTargetIsNotBrowserControlled`）。
- 没有"跳过验证/返回答案/改任意站点配置"的后门；它唯一额外的能力是用管理凭据建**自己的**演示站点。
- 页面与 verify 响应都带 `Cache-Control: no-store`，且**不设任何 CORS 头**（同源）。

**这不是正式 widget**（阶段 4）：不承诺完整跨站点 SDK API，也未做完整无障碍合规验证。

## 5. 评测方法（脚手架已交付，效果结论未测）

### 5.1 离线样本

```bash
go run ./cmd/evalgen --set dev     --count 60  --difficulty 3 --out eval-out
go run ./cmd/evalgen --set holdout --count 100 --difficulty 3 --out eval-out
```

- 输出目录 `eval-out/<set>-d<档>-n<条>-<UTC时间>/`：`sample-0001.png`…（文件名只有序号，
  **不编码答案**）+ `ground-truth.jsonl`（答案表）+ `manifest.json`。
- `manifest.json` 记：生成器 id/version、样本集名、难度档、条数、seed、**完整档位参数**、
  `go version`、`GOOS/GOARCH`、创建时间，以及一句"本目录含答案表，不得入库/外发"的警告。
- 熵源：`internal/eval` 的 HMAC-CTR 确定性流，**只用于评测与测试**。有测试钉住
  `cmd/server` 的依赖图里没有 `internal/eval`（`stage3b.sh` 第 1 项），所以生产生成路径不可能拿到固定种子。
- **dev 与 holdout 分开**：`holdout` 的 seed 写死在代码里且不允许覆盖
  （`--seed` 给了别的值直接报错）。看完验收集结果再回头调参、然后拿同一集当独立效果证明，
  测的就不是泛化能力而是"我还记得答案"。
- 诚实边界：字节级复现只在同一 Go 版本上有保证（`math/rand` 与 `crypto/rand.Int` 的内部算法
  都不是稳定性承诺），所以**已生成的 PNG 文件才是样本**，seed 只是"为什么是这些文件"的说明。

### 5.2 随机猜测（M3）

- 字符集 32、长度 6 ⇒ 完整串 `32^6 = 1,073,741,824` 种，一次猜中 `p = 9.31e-10`。
- 归一化**不改变**接受域：大小写折叠只是让同一正确答案有多种写法，服务端把它们都映射到同一个
  规范化串，接受域仍是 1 个串（容差 0）。
- n 次**独立新挑战**的累计成功是 `1-(1-p)^n`；n=3 ⇒ `2.79e-9`。`n·p` 只是上界，不是通用精确值。
- 与"同一题多次尝试"是两回事：`pow_question` 下一道题只有一次作答机会（ADR-011 #3），
  所以攻击者要拿到 n 次机会必须领 n 道题，每次都要重付 PoW 与题池成本。
- **字符组合数不是抗 OCR 的论据**（ADR-002）：M3 量的是"不看任何求解能力的下限"，
  对多模态模型与 OCR 的通过率只能由 M4 的实测回答，而 M4 本轮未测。

### 5.3 OCR 基线：**未执行（阻塞）**

- 环境探测结果：`tesseract` / `gocr` / `cuneiform` 均不存在，Python 无 PIL/pytesseract/easyocr。
  按约定**没有**擅自安装系统包或下载模型。
- 已交付的是适配器与格式，不是结论：
  ```bash
  go run ./cmd/evalocr --groundtruth eval-out/holdout-…/ground-truth.jsonl \
    --ocr-cmd tesseract --ocr-arg '{image}' --ocr-arg - --ocr-arg stdout
  ```
  输入 = 样本目录 + ground truth；输出 = `predictions.jsonl`
  （`{file, text, ms, error}`）+ 指标报告。执行方式是 `exec.Command` 直接 argv（不经 shell），
  每张一个超时；`--ocr-cmd` 不存在时打印阻塞原因并以**退出码 3** 退出。
- 指标至少包含：整串正确率（**两个分母**：`StrictAccuracy = 正确/全部样本`、
  `ScoredAccuracy = 正确/成功跑完的样本`）、样本量、工具与参数、缺测数、工具失败数、
  单张耗时 min/median/mean/max、字符级正确率。
  两个分母同时报是刻意的：只报后者会把"大面积 OCR 崩溃"包装成"题目很难"。
- 评分器与"工具失败 ≠ 答错"这条用假 OCR 脚本做过测试（`cmd/evalocr/main_test.go`），
  覆盖答对/答错/非零退出/空输出/超时/缺文件/越界文件名/重复预测。
  **没有**用伪造的预测数据声称 OCR 通过，也**没有**把同一生成器的私有答案喂给"OCR"当输入。
- 未做（需另行授权）：上传样本到任何外部多模态 API。

### 5.4 人工可读性：**未测**

预览页的盲测模式已交付（不显示答案、统计完成率/错误率/换题次数/耗时）。
本轮**没有真实参与者数据**，agent 不代填冒充用户。要拿到数据：跑起预览 → 找若干参与者各做 10 题
→ 把页面打印的汇总与参与者条件（设备、屏幕、是否戴眼镜、视力矫正情况）一起记进本文件。

## 6. 资源实测（2026-10-18，本机 linux/arm64 4 核可见，go1.24.5，`GOTOOLCHAIN=local`）

测量形状与数字来源都要写明，否则数字没有意思。

| 项 | 实测 | 条件 |
|---|---|---|
| 生成+PNG 编码耗时 | **1.81ms（d1）→ 3.91ms（d5）/题** | `BenchmarkTextImageGenerate -benchtime 60x`，纯生成（不含写库） |
| 生成+编码+写库+落盘 | **≈6ms/题**（20 题 136ms） | `stage3b.sh` d5；含 4 次进程启动开销，所以高于上一行 |
| 单题 PNG 字节 | d1 **1,126–2,557**（均值 1,911）；d5 **5,648–6,902**（均值 6,315，中位 6,286） | d1 取 benchmark 60 样本，d5 取 `evalgen` 60 样本；上限 65,536（CHECK） |
| base64 后响应体 | 一张 d5 题的 `image_base64` 长 **7,964** 字符（原图 5,971B） | `/api/challenge` 实测；整个响应约 8KB |
| 图片桶填满后的库体积 | 主文件 4,096 B + `-wal` 2,855,192 B；`wal_checkpoint(TRUNCATE)` 后 **278,528 B** | 新库 + 2 站点 + ≥20 条 ready 图片题（约 151KB PNG 净载荷）+ 40 条算术题 + 20 次闭环账本。**写入进行中只 stat 主文件会得到假数**（3A 踩过，这里两个数都报） |
| 真实服务进程驻留 | **VmRSS 18,776 kB / VmHWM 18,904 kB / Threads 10** | `cmd/server` 二进制（非测试进程），形状 = 视觉桶 ≥20 条 ready + 20 次混合闭环（10 次算术 + 10 次 pow_only，**0 失败**） |
| 二进制体积 | **10,813,732 B（10.31 MiB）** | `-trimpath -ldflags "-s -w"`；3A 是 9,896,228 B ⇒ 本阶段 +0.87 MiB（x/image 光栅路径 + 内嵌 148,672B 字体） |

**这些数字的含义边界**：

- 18.5 MB 仍是**功能闭环形状**的采样，不是长期运行/真实流量下的稳态，也没压过"图片桶满载 × 多档位"。
  它比 3A 的 15.1 MB 高 3.4 MB，方向与"多了字体解析 + 光栅缓冲 + 20 条 PNG"一致，
  但**没有做逐项归因**（没测过字体缓存占多少、page cache 占多少）。
- 不能拿 3A 的 15.1 MB 冒充图片题实测，也不能拿本表的 18.5 MB 宣布 §13 的 `<100MB` "已达标"。
- 每行图片题的**净**磁盘开销本轮没有单独隔离出来测（需要同一库的 checkpoint 前后差值 × 固定条数），
  表里给的是"填满视觉桶后的总体积"。
- 表格之后用完全相同的形状复跑了两次 `make stage3b-check`（22 项仍全绿、退出码 0）：
  第一次 **VmRSS 18,892 kB / VmHWM 18,892 kB / Threads 9**、20 条视觉题 132 ms、`-wal` 2,855,192 B；
  第二次 **VmRSS 18,712 kB / VmHWM 19,108 kB / Threads 9**、134 ms、`-wal` 2,851,072 B。
  与上表差在 0.1–0.4 MB 量级、线程数差 1（取决于闭环结束时后台 goroutine 的存活时刻）⇒
  这类数字要按"同一形状内的运行间抖动"读，不要当成可复现到个位数的常量。真正的常量只有
  PNG 上限 65,536 B、字符集 32、长度 6 这类设计值。

## 7. 四条结论要分开读

| # | 结论 | 状态 | 证据 |
|---|---|---|---|
| 1 | **工程闭环已验证** | ✅ 通过 | `make check`（fmt/vet/race/size）+ `make secret-scan` + `stage1/2/3a-check` 各 10 项 + `stage3b-check` 22 项，全部真实跑绿（退出码 0） |
| 2 | **OCR 实测** | ❌ **未执行（阻塞）** | 本机无离线 OCR 工具且未获授权安装；已交付适配器 + 格式 + 有测试的评分器；`--ocr-cmd` 缺失时退出码 3 = 未测，不是 0% |
| 3 | **人工可读性** | ❌ **未测** | 盲测页面已交付，无真实参与者数据。agent 目视检查过 d1/d5 样图与浏览器截图（可读），但这**不是**人工实测 |
| 4 | **生产适用性** | ❌ **未证明** | 管理面 `production_ready:false` + `experimental:true`；M4（每次尝试成本）完全未测，M3 只是下限不是能力 |

真实浏览器验证做到哪一步（以及没做到哪一步）：

- ✅ 已用 `firefox --headless --screenshot` 验证：预览页在真实浏览器里加载、脚本执行、
  CORS 预检 `OPTIONS 204` 后 `POST /api/challenge 200`、状态区进入"正在领取新题…"；
  并且把**真实 API 返回的 `image_base64`** 交给 Firefox 渲染，肉眼可读（d5 的 `V62LYD`，
  声明尺寸 360×132 与实际解码一致）。
- ✅ 这一步抓到一个真 bug：页面最初没在 URL 上带 `site_key`，而 JSON 请求会触发预检、
  预检只能从查询参数认站点 → 浏览器拿到 `OPTIONS 403`。curl 测不出来。
- ❌ **未**在浏览器里完成"输入答案 → 提交 → 成功"这一击：本环境没有浏览器驱动
  （无 geckodriver/playwright），`--screenshot` 不能点击或输入。
  这一段是**人工验证项**，请按 §4 的步骤 1–7 亲自走一遍，不要声称它已自动化通过。

## 8. 已知安全与无障碍限制（诚实清单）

**安全**

1. 图片里可见的字符**本来就要给答题者看**：这层没有密码学保密性，被读出就是被读出。
   本轮保证的只有——答案明文不落库、不下发（作为文本）、不进日志；PNG 无 `tEXt/iTXt/zTXt` 元数据块；
   公开 DTO 只有白名单字段；`verifier` 是 48B 摘要且判分读不到答案。
2. `M4 = 未测` ⇒ **不得**声称抗 AI/抗 OCR。加旋转、干扰线、噪声只改变 M1，不构成安全性论据（红线 14）。
3. 主密钥轮换后存量图片题的 verifier 在新密钥下必然判假（fail closed，沿用 3A 语义与运维程序：
   轮换前先清池）。
4. 图片题把"每次尝试成本"从 CPU 侧搬到了**磁盘与响应体**：因此字节预算是安全相关的资源边界，
   不是性能调优项。它只在写入侧原子判定，预检查只是省一次白烧的渲染。
5. 预览后端持站点 secret：它只监听 loopback、只接受一个字段、验证目标由服务端配置固定。
   它仍是**开发工具**，不要放到生产拓扑里。

**无障碍**

1. `text-image-v1` 是**纯视觉**题：对无法看图或低视力用户，这道题本质上不可完成。
   本轮**没有**音频或文本等价通道（DESIGN §11 的非视觉等价通道属阶段 4）。
2. **不能把 PoW-only 自动降级称为等效无障碍方案**：它换掉的是"题目这一层"，防线少了一层，
   安全语义不同。预览页上这段文字与 `alt` 描述都按这个口径写。
3. 页面做到的是：`label` 关联、键盘可完成、`aria-live` 播报、`alt` 只描述用途不含答案、
   错误信息明确、移动视口适配。**未做**：屏幕阅读器实测、完整 WCAG 检查、题型的非视觉替代。

## 9. 与 3A 的兼容性（必须继续成立的性质）

复用且**未改动**语义：原子领题（条件更新 + 影响行数 + partial UNIQUE）、`assigned` 永不回 `ready`、
一次有效作答终结 challenge、错误答案提交终态（不返回 error）、重启持久化、站点版本失效、
response 一次性消费、`pow_only` 与 `arithmetic-baseline-v1` 两条旧路径。

`0004` 是纯增量迁移：旧行 `asset_blob` 为 NULL、`asset_mime` 为 `''`，
`TestMigration0004FromStage3ADatabase` 用一个只有 `0001..0003` 的库验证升级后旧题仍可领取与终结、
二次打开幂等。算术题的 DTO 字段集合**逐字节未变**（`TestThreeVerificationPathsCoexist` 断言键集合），
图片字段只在 `media=image` 时出现。

## 10. 命令索引

```bash
make stage3b-check                      # 端到端 22 项（含 check / secret-scan / stage1/2/3a）
go test -count=1 ./...                  # 单元与集成
go test -race -count=1 ./...
go test -run XXX -bench BenchmarkTextImageGenerate -benchtime 60x ./internal/generator/   # §6 的生成耗时与 PNG 分布
go run ./cmd/evalgen --set dev --count 60 --difficulty 5 --out eval-out                   # §5.1
go run ./cmd/evalocr --groundtruth eval-out/dev-*/ground-truth.jsonl                      # §5.3（本机阻塞）
go run ./cmd/preview --api … --admin … --admin-key …                                      # §4
```
