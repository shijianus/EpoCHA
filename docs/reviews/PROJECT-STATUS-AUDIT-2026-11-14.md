# EpoCHA 专案完整现状、开发完善度与证据审计报告

> **审计日期：2026-11-14。报告性质：当前工作树的工程／产品／安全效果审计，不是发布批准或安全认证。**
>
> **总体结论：专案已经超出原型阶段，具备相当完整的自托管验证服务工程骨架；但尚不能认定为“已完成、经过完整验证、可以承担生产 CAPTCHA 安全责任”的产品。** 工程闭环的成熟度明显高于题型抗自动化效果与正式发布证据的成熟度。
>
> 本轮独立测量中，7 种新题型共 **7,000 题，有 6,824 题被确定性程序解出（97.4857%）**；7 型的既定 Gate B 判定均为 **FAIL**。这不是密钥或 verifier 泄漏证明，而是公开题面本身足以被低成本求解的证据。
>
> 同一源码在隔离副本执行正式 `release-check --profile full` 的结论为 **INCOMPLETE / 退出码 3 / `release_ready:false`**，不是 PASS。Linux 真运行、数据库完整集成、`-race`、容器、真实浏览器和人工验收仍有关键缺口。
>
> **另有直接工程阻断：公共根路由漏挂交互模块，新7型标准部署会请求404（F-12）；刷新15分钟冷却跨计数窗口可提前清除，已在隔离测试复现2秒解除（F-02）。**

## 阅读导航

- [1. 给无上下文复查者的说明](#1-给无上下文复查者的说明)
- [2. 版本、环境与证据方法](#2-版本环境与证据方法)
- [3. 整体完善度评价](#3-整体完善度评价)
- [4. 当前实际架构与开发完成面](#4-当前实际架构与开发完成面)
- [5. 本轮实测结果](#5-本轮实测结果)
- [6. 题型与机器攻击测量](#6-题型与机器攻击测量)
- [7. 发现、缺口与优先级](#7-发现缺口与优先级)
- [8. 安全保证与测试映射](#8-安全保证与测试映射)
- [9. 发布、部署与运维判断](#9-发布部署与运维判断)
- [10. 建议的后续开发顺序](#10-建议的后续开发顺序)
- [11. 独立复核与重跑步骤](#11-独立复核与重跑步骤)
- [12. 限制、不应推导的结论与最终意见](#12-限制不应推导的结论与最终意见)

---

## 1. 给无上下文复查者的说明

### 1.1 这是什么专案

EpoCHA 是自托管人机验证服务：生产后端为 Go，权威业务状态在 SQLite；浏览器 widget 使用无第三方运行时依赖的原生 JavaScript。基本流程为：

1. 接入方服务器持有站点 secret，浏览器只持有公开 site key。
2. 浏览器领取带签名的 challenge，完成 PoW，并在站点要求时完成一道题。
3. `/api/redeem` 校验签名、站点绑定、有效期、PoW 与答案，成功后签发一次性 response token。
4. 接入方后端调用 `/siteverify`；只有服务端验证成功才执行受保护的业务动作。
5. SQLite 原子状态转移承担一次性消费；管理 API 在独立监听器上提供站点、来源、凭据与题池管理。

它采用 reCAPTCHA **风格**的 `/siteverify` 字段与响应，不是对所有 reCAPTCHA SDK 的无条件兼容实现。

### 1.2 先区分四件事

| 层次 | 本报告中意味着什么 | 不意味着什么 |
|---|---|---|
| 实现存在 | 阅读了真实实现及调用路径 | 已运行、无缺陷或已被用户验收 |
| 工程测试 PASS | 明确命令实际执行、退出码为 0，且没有把失败／跳过算通过 | CAPTCHA 难以被机器破解 |
| 效果测量 | 在特定样本、攻击器、参数、主机上测得结果 | 全部题型、全部难度、目标服务器或真实人群的结论 |
| 发布验收 | 支持的平台、身份、候选版本与完整门禁全部满足 | 历史标签、交叉编译或一张截图可以替代 |

本文使用：**PASS / FAIL** 表示已执行工程项的结果；**BLOCKED** 表示外部条件不满足；**NOT_RUN** 表示本轮未执行。`INCOMPLETE` 是发布门禁的聚合结果，不会转换成 PASS。

### 1.3 文档权威顺序

项目约定要求冲突时以 `docs/DECISIONS.md` 的 ADR 为准；实际代码和本轮原始证据用于确认“是否已经落地”。不要只读 `AGENTS.md` 的旧进度摘要。

**已经确认的文档漂移：** `AGENTS.md`、`docs/QUESTION-TYPES.md:43` 仍保留“站点单生成器、风险打分／加权多题型尚未实现”等旧描述，但 ADR-026／027、0006 迁移、`internal/dispatch` 和 API 的实际路径已经实现站点生成器池、风险带、刷新惩罚和弱遥测。应把它们标为“已实现、尚有缺陷和验收边界”，而不是继续写“完全不存在”。详见第 4、7 节。

## 2. 版本、环境与证据方法

### 2.1 审计对象不是一个干净发布提交

| 项目 | 本轮事实 |
|---|---|
| 专案根目录 | `D:\Users\Smart Kevin\OneDrive\Desktop\epocha` |
| 分支 | `fix/release-preparation-2026-11-02` |
| HEAD | `b5506989075c82927ead14cda0e71a8f9f401803` |
| 审计前已有修改 | `Makefile`、`docs/DESIGN.md` |
| 审计前已有未跟踪开发 | `cmd/m4baseline/`、`internal/m4baseline/`、2026-11-12 M4 报告及其 CSV／JSON 证据 |
| 本轮动作 | 新增本报告、审计脚本、日志、收据和证据；没有修改产品功能、测试或部署源码 |
| Git／远端 | 未 commit、未 tag、未 push、未 fetch、未调用远端核验；不声称远端保存了本轮内容 |

起止源码清单包含 **284 个 Git 可见文件**（含既有未跟踪 M4 开发，不含本次审计输出）。两次 SHA-256 清单指纹完全相同：

```text
19b05422750a5df45969040b3c734c5e901149335d951296d825a439abf82adb
```

清单算法与逐文件哈希在 `audit-evidence/2026-11-14/{audit_runner.py,source-initial.json,source-final.json}`。这是本报告的工作树身份；**HEAD 一项不足以代表未提交的 M4 工具。** 发布工具的 source hash 使用另一种序列化格式，故值不同；逐文件对应相同，不能把不同算法的指纹误判为源码漂移。

`source-initial.status.txt` 在创建审计收集器后采集，因此其中也出现最早的审计文件；判断原有修改时应以清单的 `excluded_audit_outputs` 和本节范围为准。

### 2.2 实际本机环境

- Windows 11／`win32`／amd64；不是专案正式支持的 Linux/arm64 运行环境。
- PATH 默认 `go` 为 **go1.24.2**，不符合 `go.mod` 的 **1.27.3**。本轮改用**已经安装**的绝对路径 Go 1.27.3，未升级或下载工具链。
- Node **v24.14.0**；Python 3.14.3 只用于审计收据、统计与本地文件处理，不是生产依赖。
- 全部 Go 命令固定 `GOTOOLCHAIN=local`、`GOPROXY=off`、`GOSUMDB=off`、`GOENV=off`、`GOWORK=off`；默认加 `-mod=readonly`。
- Docker 客户端存在，但本地 Linux Engine 管道不可连接；未启动 Docker、未创建容器、未拉镜像。
- 没有可用 GNU Make、gcc 或 WSL Linux 发行版；标准安装位置和 PATH 未找到 Firefox。
- 没有授权外部部署／在线攻击／模型调用；本轮没有在线扫描、VPS 操作、安装依赖或调用外部 AI。

Go 绝对路径：

```text
C:\Users\Smart Kevin\go\pkg\mod\golang.org\toolchain@v0.0.1-go1.27.3.windows-amd64\bin\go.exe
```

上述条件只描述本机，不是 VPS 的事实。原始输出见 `toolchain`、`go-env`、`docker-capability`、`wsl-capability`、`make-capability` 与 race 收据。

### 2.3 证据链结构

本报告相邻目录：`docs/reviews/audit-evidence/2026-11-14/`。

核心文件：

| 文件／目录 | 用途 |
|---|---|
| `RESULTS-final.json` | 65 份命令收据的索引、Go／Node／M4 的独立汇总；优先读它，不以较早 `RESULTS.json` 为最终版本 |
| `*.receipt.json` | 精确 argv、cwd、时间、真实退出码、stdout／stderr 字节数与 SHA-256；部分专用脚本的字段稍有差异 |
| `*.stdout.log`、`*.stderr.log` | 完整原始输出；保留失败，不用 `head`／`tail` 隐藏失败方向 |
| `source-initial.json`、`source-final.json` | 被审计文件及逐文件 SHA-256，验证用户原有工作未变 |
| `isolated-source-manifest.json` | 字节相同的隔离副本来源与本地 clone 命令；不访问网络、不创建提交 |
| `isolated-release-gate/` | 隔离源码运行 full 门禁的完整 summary、Node TAP 与人工项 NOT_RUN 记录 |
| `native-coverage.out` | 17 个可运行包的原始覆盖率 profile；不是全后端覆盖率 |
| `linux-test-compilation-summary.json` | 28 包清单中 25 个有测试包的 Linux/arm64 编译结果；明确 `executed:false` |
| `cross-build-manifest.json` | server／ops 的字节数、SHA-256、目标与未执行说明 |
| `m4-replication/` | 本轮独立 7,000 题的逐题 CSV 与 summary JSON |
| `risk_cooldown_reproduction.go.txt` | 只在隔离副本添加的缺陷回归测试源码，未改变正式测试 |
| `risk-cooldown-reproduction.*` | 上述测试的 FAIL 原始输出与退出码 |
| `static-findings-evidence.json` | 模块父子路由、加载超时、sprite缩放、目标条几何与无障碍额度的静态证据定位 |
| `go-cpu-metric-definition.txt` | 安装的Go1.27.3原始metric定义摘录，用于核实CPU字段口径 |
| `SHA256SUMS.json`、`verify_evidence.py` | 随报告交付的文件完整性清单与离线核验器 |

**证据强度：** 有原始输出、版本与逐文件哈希，可检查命令结果和内部一致性；没有签名、远端时间戳或第三方见证，因此**不提供不可抵赖性、来源真实性或防同权限恶意篡改保证**。

### 2.4 为什么既有一次 FAIL 又有一次 INCOMPLETE

1. 原工作区直接跑 `go test ./...`／`go vet ./...`，发现被 Git 忽略的旧 `.go` 工装也会被 Go 扫描。特别是 `release-artifacts/c04-c08-20261102T142021Z-6471/red-audit.go` 等出现 `undefined: DB`。Git 忽略不代表 Go 忽略。
2. 原树 full 门禁正确拒绝 `go-package-inventory`。期间本轮审计仍在写新证据，导致原树第一次 `source-stability` 也未成立；**这是本次并行采证方式造成的，不能归罪产品缺陷**。
3. 在新的临时本地 clone 内覆入相同 284 文件，逐字节核对，排除原树 ignored 工装；不删除／清理原工作区。
4. 在该稳定副本串行运行 full：`go-package-inventory` 与 `source-stability` 均 PASS，最终 **INCOMPLETE / 3**，真实平台和人工条件仍阻塞。
5. 第一轮构建收据集成测试也因审计输出并行变化失败；同一源码隔离重跑该集成测试 **PASS / 0**。失败输出保留，不把它伪装成产品回归。

隔离副本仍含既有未提交 M4 开发，因此仍是 dirty；它是**干净于遗留工装的审计副本，不是干净 Git 发布候选**。

## 3. 整体完善度评价

### 3.1 我的总评

**定位：有较强安全工程意识的实验性验证平台／可继续试部署的工程底座，而不是已经完成的生产级 CAPTCHA 产品。**

优点不在“题型多”，而在一次性消费、站点绑定、凭据生命周期、资源上限、备份恢复边界与验收失败分类已有实质设计和测试。主要短板也不在“缺更多题型”，而在：

1. 已新增题型的真实抗自动化效果大幅落后于工程实现速度。
2. 当前版本缺少受支持平台上的完整、绑定候选身份的重新验收。
3. 无障碍／真实用户体验与新交互题型浏览器实测不足。
4. 文档和里程碑摘要互相漂移，容易让后来者高估或低估实际能力。
5. 个别风险控制与无障碍额度路径有可复核缺陷。

### 3.2 不给虚假的“完成百分比”

没有稳定、经批准的需求分母，直接写“完成 90%”会把不同维度混在一起。本报告采用分维度判断：

| 维度 | 完善度评价 | 本轮证据／保留意见 |
|---|---|---|
| 核心服务结构与接口 | **较成熟的工程实现** | handler、配置、token、PoW、存储、题池与管理面已齐；Linux 完整运行仍 BLOCKED |
| 一次性与凭据安全设计 | **基础扎实，但本轮未完成全回归** | 有原子消费、同事务、提交后成功、撤销持久化测试；不以静态阅读代替实际数据库测试 |
| 题库工程可扩展性 | **已形成体系** | 19 个注册生成器、12 类交互答题类型、白名单 DTO、容差 verifier、资源预算 |
| 题型安全效果 | **不合格／尚未证明** | 新 7 型 97.49% 被解；其他题型不能因未测而认为安全 |
| 风险调度 | **已实现的规则型基础能力** | 多题型池、静态加权、风险带、弱遥测已接入；跨窗冷却缺陷、客户端 session 兼容边界 |
| Widget 主链路 | **工程基础完整，新题型验收不足** | 生命周期和向量工装 PASS；本轮真实浏览器交互 BLOCKED |
| 管理产品面 | **站点主流程可用，平台功能不完整** | 凭据入口／站点列表／创建／详情；统计、淘汰、审计／轮换专用页面未齐 |
| 运维与恢复工具 | **能力较完整，正式演练不足** | backup／verify／restore／运行锁／readyz 已有；当前版本目标机恢复接管、满载、长跑未复验 |
| 自动化质量与证据 | **明显优于一般原型，但不完整** | 可运行子集、Node、交叉编译、短 fuzz 有证据；race／Linux 集成／人工项不能通过 |
| 正式发布准备度 | **未达到** | 隔离 full `INCOMPLETE/3`，dirty 非正式候选，不能批准上线 |

### 3.3 当前可以做与不应做

**可以：** 继续本地研发、开展受控且有回退方案的测试部署、把 PoW／一次性验证工程作为实验底座、收集真人可用性与攻击成本数据。

**不应：** 宣称抗 AI／抗 OCR、承诺适用于高价值生产防护、把 19 个 experimental 生成器全部加入真实用户默认池、以本报告或历史阶段标签代替新候选 full 验收。

## 4. 当前实际架构与开发完成面

### 4.1 后端与持久化

- 单 Go 服务；`cmd/server` 负责配置、DB、独立监听器、后台题池 worker、清理及优雅关闭。
- SQLite 是防重放账本、站点、凭据、题池与审计的权威；已有 **0001–0006 六条迁移**。不要继续按早期“三条／四条迁移”描述现状。
- `/api/challenge`、`/api/redeem`、`/siteverify`、`/healthz`、`/readyz` 主接口存在；`readyz` 包含 DB 写路径可用性，而非只测 TCP。
- HMAC-SHA256 token 使用按 purpose／kid 分域的派生密钥；challenge／response／qver 分离；库中参数为权威。
- 一次性消费路径采用条件更新、检查影响行数、同事务提交；错误答案的终态提交与畸形输入不消费机会分离。
- 来源与代理信任是显式配置；限流有状态条目上限；公开错误限制在既定错误族。
- 图片以公开 DTO 的 base64 下发，不依赖公开裸媒体／答案调试接口；判分不需要读取图片资产。

**重要限制：** 本轮 Windows 无法原生编译 `internal/store/lock.go` 的 `syscall.Flock`，因此 store／api／admin 等关键集成测试未实际运行。Linux 交叉 `vet` 和测试编译不补足运行证据。

### 4.2 题池与生成器基础设施

已有后台预生成、低／高水位迟滞、有界批次、CPU 让步、指数退避、全局行数／图片字节预算、按题型版本和难度精确领题。公开 payload 与私有 verifier 分离；新连续输入使用固定上限的 Tag-Set Verifier（TSV），不是落库明文答案。

主要上限从当前代码／ADR-028 应读为：payload **8,192 B**、verifier **4,096 B**、单题资产 **65,536 B**；不是所有早期文档中的 4 KiB／512 B 旧值。规模与满载资源仍应在目标机实测。

### 4.3 风险带、多题型池与遥测：已经落地，但不要夸大

实际调用链：`internal/api/handlers.go:282–378`、`internal/risk/phase3.go:246–295`、`internal/dispatch/dispatch.go`、0006 迁移。

- 站点 `generator_pool` 最多 8 个；空池沿用兼容的单 `generator_id`。
- `BandForScore` 将 0–100 规则分划为 low／medium／high／critical。
- 调度按静态题型画像的 base × riskFit × deviceFit 加权，使用安全随机；先确认 ready 候选，不隐式降级成 PoW-only。
- 刷新带来难度与 PoW 地板、冷却；失败和有限遥测摘要影响下一次调度。
- 遥测是弱证据，不进入当前题 `Verify`；正确答案不会因为这些摘要单独变错。
- 客户端没有 session 时保留旧行为，**不启用刷新地板**；session 也可由客户端更换。不要把它写成不可绕过的服务端身份／反机器人证明。
- 未接入外部 IP 信誉、ASN／Tor 名单或机器学习模型；静态权重不是基于真实成功率的自动学习。
- 未完成自动分级淘汰／自动降权与实际人群阈值标定。

### 4.4 无障碍：有弱安全文本路径，不等于完成可达性

`accessible-choice-v1`、显式 accessibility 请求、站点池 opt-in 与 source+site 的 24h 名额闸门已经存在（ADR-029）。这纠正了旧清单中“没有任何无障碍生成器”的绝对说法。

但它是 **12 选 1 文本工程通道**：可脚本求解、随机单次 1/12，不能称等效生产防护；没有音频等通用替代、没有辅助技术真实验收。高风险过滤、名额过早消费问题和 source 共享也影响可用性。总体仍是“基础路径已做，完整无障碍验收未完成”。

### 4.5 Widget

- `EpoCHA.render/getResponse/reset/refresh/destroy`、中英文、Shadow DOM、light DOM hidden response 字段、任务取消／换代、过期清 token、CSP worker 两路径、多实例隔离等已存在。
- 核心 `v1.js`、PoW `v1.worker.js` 与按需同源 `v1.interactive.js`；worker 内嵌／外部源码一致性有测试。
- Widget代码接受 **12 类** answer_type：`text`、`cells`、`pairs`、`angle`、`point`、`timing`、`masks`、`path`、`select`、`order`、`sequence`、`placements`。算术基线API另有`integer`，但当前widget白名单并不接受它，不能宣称所有19型都能直接用生产widget完成。
- **新7型生产接入有确定挂载缺陷：** 子资源handler包含交互模块，但公共根mux没有注册其URL，导致正常单二进制部署取模块404（F-12）。因此这里的“渲染器已实现”不等于“生产公共URL可完整使用”。
- 未实现或未验收：通用 `fallback_base`、真实辅助技术／真机、全部新交互题型跨浏览器完整闭环。
- `cmd/preview` 是自包含的 text 答案页，非 text 题型会拒绝；“在白名单里”不等于“已提供交互预览”。

### 4.6 管理面

站点／凭据 API、来源策略、启停、TTL／限流／模式／题型、生成器列表、只读题池和审计基础能力已存在。静态管理壳仅挂在管理监听器上；API 仍各自鉴权。

管理 UI 覆盖凭据验证、站点列表／分页、创建与 secret 一次性展示、站点详情与策略保存回读。它不是完整运营管理平台：统计趋势、基于真实数据的淘汰面板、专用审计／凭据轮换页面仍不完整。不要用虚构 QPS、成功率或空导航补齐观感。

### 4.7 交付与工具

已有 Docker／Compose、反代示例、文件型密钥、运行目录锁、`cmd/ops` 的 backup／verify／restore／check／admin／admin-recover、部署预检、构建收据与持续演练证据脚本。另有开发期 preview／eval／loadtest／blindtest／M4 工具，不应被链接进生产 server。

当前源码清单为 **181 个 Go 文件，其中 101 个 `_test.go` 文件**；静态计数 **680 个 `Test...` 函数、8 个 `Fuzz...` 函数**。这是维护规模和测试存在性，**不是已执行数量或覆盖充分性的证明**。

## 5. 本轮实测结果

### 5.1 总表

下表的 PASS 都有实际输出；所有路径以 `docs/reviews/audit-evidence/2026-11-14/` 为基准。

| 检查 | 实际结果 | 原始码／证据 | 如何解释 |
|---|---|---|---|
| 原工作区 `go test -json -count=1 ./...` | **FAIL** | rc **1**；`go-test-full.*` | Windows flock 不可编译，且 ignored 工装有额外编译错误 |
| 原工作区 `go vet ./...` | **FAIL** | rc **1**；`go-vet-full.*` | 同上，不能宣称全量 vet 绿 |
| 原工作区 full 发布门禁 | **FAIL** | rc **1**；`release-full.*` | 正确拒绝源码清单污染；source-stability 另受并行采证影响 |
| 明确 17 个 Windows 可运行包测试＋coverage | **PASS** | rc **0**；`go-native-coverage.*`、`native-coverage.out` | 298 个顶层 Test、8 个顶层 fuzz seed 容器，含子测试共 **408 个 pass 事件**；0 test fail／skip |
| 同 17 包原生 `go vet` | **PASS** | rc **0**；`native-vet.*` | 明确子集，不是全部后端 |
| 隔离源码 `go test -json -count=1 ./...` | **BLOCKED（运行命令非零）** | rc **1**；`isolated-go-full.*` | 28 包中 17 pass，8 个依赖 flock 的测试包编译失败；3 包无测试。只有平台编译阻塞，不再有遗留工装错误 |
| 隔离 Linux/arm64 `go vet ./...` | **PASS** | rc **0**；`isolated-go-vet-linux-full.*` | Linux 目标静态检查，不运行服务 |
| Linux/arm64 测试二进制逐包编译 | **PASS（仅编译）** | 25 个 rc **0**；`linux-test-compilation-summary.json` | 28 包中25包有测试、3包无测试；**没有执行任何 Linux 测试** |
| `go test -race -count=1 ./...` | **BLOCKED** | rc **2**；`go-race-full.*` | Go 报 race 需要 cgo |
| 显式 `CGO_ENABLED=1` 的 pow／token race 能力验证 | **BLOCKED** | rc **1**；`go-race-with-cgo.*` | 明确报缺 gcc，未完成 race；生产 CGO=0 约束未改变 |
| 隔离 full 发布门禁 | **INCOMPLETE** | rc **3**；`isolated-release-full.*`、`isolated-release-gate/summary.json` | **15 PASS / 19 BLOCKED / 8 NOT_RUN / 0 FAIL**；`release_ready:false` |
| full 内 Node 四组测试 | **PASS** | 26＋50＋22＋57 = **155**；见对应 TAP | 0 fail／skip／todo／cancelled；不是浏览器 GUI |
| 构建收据真实集成，隔离复跑 | **PASS** | rc **0**；`isolated-build-receipt-integration.*` | 真交叉构建、原始字节校验、独立目录、篡改拒绝；不是镜像运行 |
| 8 个 fuzz target，各 5 秒／2 workers | **PASS（短时）** | 各 rc **0**；`fuzz-*.log` | 合计约 **1,164,052 次执行**；只针对答案规范化，不是完整攻击面 fuzz |
| stage5b 分类自测 | **PASS** | rc **0**；`stage5b-classifier-gitbash.*` | 9 条场景，验证分类逻辑；不是 stage5b 部署通过 |
| `go mod verify` | **PASS** | rc **0**；`go-module-verify.*` | 本地 module cache 完整性；不是最新漏洞情报或许可证审计 |
| JavaScript／shell 语法 | **PASS** | full summary：23 个 JS、17 个 shell | 语法正确不保证行为正确 |
| Go 格式／diff 空白检查 | **PASS** | full：181 个 Go 文件、`diff-check` | 没有格式化用户代码 |
| M4 独立 7,000 题 | **测量完成；7 型效果 FAIL** | 工具 rc **0**；`m4-replication.*` | 工具成功完成测量不等于题型合格 |
| 刷新冷却补充回归 | **FAIL（已复现缺陷）** | rc **1**；`risk-cooldown-reproduction.*` | 新增测试只在隔离副本；未混入正式测试数量 |
| Docker／Linux 服务／真实浏览器／人工／TLS | **BLOCKED／NOT_RUN** | full summary 及环境收据 | 本报告未完成这些验收 |

**包装层注意：** `audit_runner.py` 成功记录命令时自身可能退出 0，即使被测命令退出 1／2／3；应读取收据 `raw_exit_code`，不是工具包装的“任务完成”。Go／make／脚本各自的退出码也不能互相冒充。本轮 GNU Make 不可用，因此没有重新宣称实测 make 的折叠行为。

一次多包 `go test -c -o 同一目录` 首次因 `cmd/preview` 与 `web/preview`、`cmd/m4baseline` 与 `internal/m4baseline` 输出名冲突失败；后改用逐包唯一文件名，25 包均成功。这个失败是审计编译命令的命名问题，不是产品不能编译；原始 `go-test-compile-linux-project.stderr.log` 仍保留。

### 5.2 可运行 Go 子集的覆盖率

以下只反映本机可运行包的语句覆盖，不代表安全充分性：

| 包 | 语句覆盖率 |
|---|---:|
| `internal/pow` | 96.9% |
| `internal/token` | 93.4% |
| `internal/risk` | 84.2% |
| `internal/dispatch` | 80.3% |
| `internal/generator` | 80.1% |
| `internal/eval` | 97.5% |
| `web/widget` 的 Go 资源／handler | 85.2% |
| `web/adminui` 的 Go 资源／handler | 91.3% |
| `internal/m4baseline` | 16.1% |
| `internal/m4baseline/solver` | 28.0% |
| `cmd/devclient` | 18.4% |
| `cmd/loadtest` | 80.7% |
| `cmd/exampleapp`／`cmd/preview` | 各 66.0% |
| `cmd/evalocr` | 58.3% |
| `cmd/blindtest` | 86.3% |

新 M4 工具覆盖低，特别需要补输入／退出码／空白名单／失败路径的测试。主程序、store、api、admin、config、pool 的本轮运行覆盖率**未知**，不能拿上述百分比补齐。Go 资源 handler 的覆盖率也不是 JavaScript 分支覆盖率。

### 5.3 二进制与 widget 预算

**[本轮实测]** Linux/arm64、CGO=0、`-trimpath -ldflags='-s -w'`：

| 产物 | 字节 | SHA-256 | 状态 |
|---|---:|---|---|
| server | **12,255,392** | `d50e3c55fb2abbe416c5ed208e1f67326ba1d533367c8d3039f9ee234392b3e0` | 低于35 MiB；未执行 |
| ops | **10,289,312** | `cc11426d24532b13a7e743011d034d2590d5ea671edd542c48ee2149c2bf9c0e` | 低于35 MiB；未执行 |

这是审计构建，不含正式候选完整 version ldflags；不能替代构建收据或当作已经部署的镜像。`go version -m` 元数据收据另存；server 依赖清单另存。没有量得目标机稳态 RSS／p99／WAL 增长。

Widget gzip(level 9)：

| 文件 | 原始字节 | gzip 字节 | 预算与余量 |
|---|---:|---:|---|
| `v1.js` | 110,465 | **28,968** | `<30,720`；余1,752 B |
| `v1.interactive.js` | 48,751 | **10,311** | `<30,720` |
| 核心＋交互模块 | — | **39,279** | `<40,960`；余1,681 B（约4.1%） |

预算本轮 PASS，但总余量很小。不要继续以旧 16–22 KB 的历史 widget 数字描述当前交付；也不要把模块仍有30 KiB单文件余量误读为总预算还有那么多。

## 6. 题型与机器攻击测量

### 6.1 当前19型清单与可认定范围

所有注册题型均保持 **`experimental:true`／`production_ready:false`**。本报告没有改变默认站点、权重、难度或配置。

| 题型 | 输入／定位 | 当前可认定的效果状态 |
|---|---|---|
| `arithmetic-baseline-v1` | integer／公开算术 | 工程基线，可直接脚本求值；不是防机器题 |
| `text-image-v1` | text／6字符图 | 有历史单工具 OCR 测量；本轮 OCR NOT_RUN |
| `text-image-v2` | text／8字符形变 | 有历史记录；本轮不重验、不外推到v3 |
| `text-image-v3` | text／6字符、容1次替换 | 本轮效果 NOT_RUN；strict OCR评分不等于生产容差接受率 |
| `shape-grid-v1` | cells／3×3图形 | M4／真人 NOT_RUN；negative polarity不是安全证明 |
| `shape-count-v1` | pairs／两类计数 | M4／真人 NOT_RUN |
| `parallax-code-v1` | text／多帧数字 | M4／真人／跨引擎动画 NOT_RUN |
| `bank-shot-v1` | angle／反弹角度 | M4／真人 NOT_RUN；容差用TSV |
| `accessible-choice-v1` | text／12选1 | 弱安全、显式请求、24h闸门；不是完整等价无障碍认证 |
| `energy-window-v1` | timing／记录时间 | M4／真人／真实时序 NOT_RUN |
| `pipe-connect-v1` | cells／断点 | legacy能力存在；完整逐格edge-mask整改仍BLOCKED，不能借新题型冒充完成 |
| `intersection-point-v1` | point／交点 | M4／真人 NOT_RUN；量化邻域容差 |
| `tile-rotation-v1` | masks | 本轮 Gate B **FAIL** |
| `symbol-path-v1` | path | 本轮 Gate B **FAIL**；另有目标条几何问题 |
| `crowd-find-v1` | select | 本轮 Gate B **FAIL**；视觉可区分性需进一步约束 |
| `visual-rank-v1` | order | 本轮 Gate B **FAIL** |
| `progressive-target-v1` | sequence | 本轮 Gate B **FAIL** |
| `shell-track-v1` | sequence／33帧 | 本轮 Gate B **FAIL**；动画不是保密边界 |
| `silhouette-assembly-v1` | placements | 本轮 Gate B **FAIL** |

### 6.2 本轮独立复测方法

- 本轮重新构建现有未提交 `cmd/m4baseline`；不复用旧 exe，不修改 solver，不调参。
- 7型各 **1,000** 个新 challenge，`difficulty=3`／`lang=zh-CN`／`crypto/rand.Reader`；共7,000。
- harness 可调用生成器和持有 verifier，但 solver 的函数输入只有白名单公开 DTO＋公开 PNG base64。
- `internal/m4baseline/run.go:193,207–240,301–322`：真实 Generate → SanitizePublic → solver → NormalizeAnswer → 生产 Verify。私有判分资料不交给 solver。
- 它是**本地公开题面求解评测**，不是实际 HTTP 领题／PoW／redeem／siteverify E2E。成功率指答案能被生产题型判分器接受。
- 统计脚本另行检查 CSV 索引0..999无缺号／重复、分母、成功数、生成重试和 Wilson 95% CI。原有11月12日和本轮两份 CSV／JSON 的汇总均一致。
- 不是预注册 holdout 或双盲：solver 来自仓库既有开发，可能看过早期 dev 样本。本轮是新随机样本上的独立重跑，不是全新攻击队伍。
- 参考主机 i7-14650HX、24 logical processors、约31.71 GiB RAM、Go1.27.3；不是1C1G目标机。
- 时间：**2026-11-14T05:22:08Z–05:24:34Z**，约146.7秒。复测期间本机另有构建／审计进程，时延不是受控空闲机基准，应视为参考wall测量。

### 6.3 本轮结果（新的随机样本）

| 题型 | 生产Verify接受／总数 | 接受率 | 独立重算Wilson95% CI | solver p95 wall | 既有Gate B结论 |
|---|---:|---:|---|---:|---|
| tile-rotation | 1000/1000 | 100.0% | 99.617%–100% | 0.698 ms | FAIL |
| symbol-path | 996/1000 | 99.6% | 98.976%–99.844% | 7.254 ms | FAIL |
| crowd-find | 828/1000 | 82.8% | 80.337%–85.012% | 4.407 ms | FAIL |
| visual-rank | 1000/1000 | 100.0% | 99.617%–100% | 11.294 ms | FAIL |
| progressive-target | 1000/1000 | 100.0% | 99.617%–100% | 6.123 ms | FAIL |
| shell-track | 1000/1000 | 100.0% | 99.617%–100% | 8.699 ms | FAIL |
| silhouette-assembly | 1000/1000 | 100.0% | 99.617%–100% | 1.582 ms | FAIL |
| **合计** | **6824/7000** | **97.4857%** | **97.092%–97.827%** | 不跨型合并p95 | **7 FAIL / 0 PASS** |

所有题型均无DTO／PNG解析失败；symbol-path有4次放弃，其余未成功来自crowd-find答错；生成未出现重试。失败／放弃均留在分母。旧证据为6836/7000（97.6571%），**不是本轮数字**；两次随机波动不改变7型全部失败的方向。

### 6.4 可以下的结论

- 这7型已经存在具体、可运行、从公开信息出发的非AI求解器；不是“理论上可能”。
- `tile-rotation` 公开 `initial_masks` 即完整初始棋盘，旋转约束可程序化求解，不需要OCR。
- 多步、动画、图形、唯一解、CSPRNG、HMAC、题面空间大，都没有阻止本轮程序求解。
- 新题型保留实验状态和不自动加入站点是正确的风险隔离；保持该状态并不消除已测得的效果失败。

### 6.5 不能从本轮下的结论

- 不能说整个服务在“不算PoW成本”的同时被无成本攻破；PoW、网络、限流、IP／站点约束的总成本没有包含。
- 不能说剩余12型安全或不安全已全部实测；它们不在本轮新攻击测量内。
- 不能把参考主机毫秒数／RSS当目标VPS预算，也不能称新7型经真人可读性、辅助技术或真机验证。
- 不能把 `cmd/m4baseline` rc0解释为效果PASS。rc0仅表示测量按工具定义完成，判定在JSON的 `verdict`。
- **CPU字段存在口径缺陷，不能作为准确CPU秒证据。** 详见F-04；本表因此优先列wall。成功数与公开可解事实不依赖CPU字段成立。

## 7. 发现、缺口与优先级

### 分级说明

- **P0：上线阻断／发布前必须作出明确处置。** 不等于远程RCE或数据库被绕过。
- **P1：应优先修正的真实缺陷或重大验收缺口。**
- **P2：维护性、证据与体验问题，影响后续可靠推进。**

下列项区分“动态已复现”“静态路径确认”“既定范围缺口”。没有把未经证实的可疑点写成漏洞。

### F-01 · P0 · 新7型无法承担预期的第二层人机区分效果

**证据强度：动态复测，确定。** 第6节6824/7000、7 FAIL；`m4-replication/`、`RESULTS-final.json`。

**影响：** 若把它们当作主要CAPTCHA防线，自动化客户端只需公开题面与简单程序即可高概率给出被生产Verify接受的答案。PoW仍有成本，但不能据此把题型层效果失败写成通过。

**建议：** 冻结扩题速度与生产启用；优先做攻击—设计—真人成本的闭环。每型独立决定保留／重设计／移除候选，**本轮不删除任何文件或题型**。只加噪声、延迟或更多轮次不足以证明改进；重新跑冻结攻击器和独立holdout，另测用户负担。

**关闭条件：** 明确威胁模型和可接受攻击成功率／单位成本；公开题面baseline与更强攻击评测均有可重复数据；没有证据前继续`production_ready:false`。

### F-02 · P1 · 15分钟刷新冷却被10分钟窗口切换提前清除

**证据强度：静态＋隔离回归动态复现，确定。**

- `internal/risk/phase3.go:72–79`：bucket改变时直接新建状态，丢失`cooldownUntil`。
- 同文件`:95–103`：第6次给3秒、第10次给15分钟冷却。
- `internal/api/handlers.go:291–295`：先Assess／Observe，再按返回Cooldown判断；没有先保留旧deadline。
- ADR-026 `docs/DECISIONS.md:1157–1162` 明确≥10次冷却15分钟。
- 回归时钟Unix599→601，只过2秒，Observe前RetryAfter=14m58s，Observe后Cooldown与RetryAfter均0；新断言FAIL rc1。
- 现有`TestRefreshTrackerStateMachine`在`:51–55`只断言跨窗分数／地板重置，漏掉冷却必须保留的断言。

**影响：** 有session的客户端可在窗口边界比预期早恢复签发；削弱既定刷新惩罚，但不等于签名／一次性账本被绕过。

**建议：** 先补跨窗、临界点、容量淘汰与重试不续期的测试，再让未到期deadline独立于计数桶重置保留。此报告未改代码。

### F-03 · P1 · 无障碍24h名额在实际签发前消费

**证据强度：静态调用路径确认；完整HTTP动态复现BLOCKED。**

`internal/api/handlers.go:322–345`检查配置后即调用`access.Allow(accessKey)`；之后`:353–360`可能因没有ready题而503，`:362–372`可能调度失败，`:411–422`可能因存量上限／池竞争／DB错误失败。`internal/risk/phase3.go:326`将语义写为“一次签发占名额”，但实际可能“一次失败申请占名额”。

**影响：** 正常无障碍用户在空池或瞬时错误后，下一次请求可能被拒至24h窗口结束；source共享时同站其他用户也可能受影响。不得把source闸门当真人身份。

**建议：** 先写“池空→补池→重试仍可取得第一次签发”的API负向回归，以及竞争签发／取消／提交失败测试；采用明确可回滚预留或只在成功签发后提交额度的原子设计，避免简单后移造成并发超领。Linux实跑前不宣称修复验证。

### F-04 · P1 · M4 CPU字段不是准确进程CPU时间，p95回退判据也不足

**证据强度：真实实现＋安装工具链原始定义核验，确定。**

- `internal/m4baseline/host.go:32–41`用`runtime/metrics`的`/cpu/classes/user:cpu-seconds`，却将其称为“进程累计用户态CPU”。
- 安装的Go1.27.3 `src/runtime/metrics/description.go:168–173`明确这是**估计的Go用户代码CPU时间，不能直接与system CPU测量比较**。它不是`GetProcessTimes`／`getrusage`的实际进程用户CPU计数。
- `run.go:436–450`以整个样本CPU sum≥1ms决定使用CPU p95，不能证明逐题p95有有效分辨率；本轮多个型p95 CPU=0但仍可能标`cpu`。
- 旧M4报告`:99–101`关于Windows 15.6ms进程CPU粒度的解释没有被该实现证实。

**影响：** 成功率／可解性仍成立，但“p95准确CPU秒”“每成功一次精确CPU成本”证据不足；不能作为严谨容量或收费成本结论。

**建议：** 使用可核验的OS进程CPU测量或批量solver-only测量，分离生成器／GC／harness；显式记录metric名称、估计性质、精度、测量失败和fallback依据。先补测试，不用CPU=0当无成本。当前wall数据只作为参考主机的本地solver耗时，不包含PoW／网络。

### F-05 · P1 · 当前候选完整发布与目标机运行证据尚未闭合

**证据强度：本轮full门禁＋仓库明确NOT_RUN，确定缺口。**

隔离full为INCOMPLETE/3，正式范围仍B路径／Linux arm64／UID:GID1000:1000；本轮Windows子集、交叉编译、旧标签均不能替代。详见第5、9节。

**建议：** 对冻结候选在独立正式环境完成race、全量Go、stage4b／5a／5b／admin-ui、浏览器、恢复接管与资源演练。不要为得到绿灯降低门禁或把未运行项改PASS。

### F-06 · P2 · 状态文档冲突，容易让复查者得出错误结论

**证据强度：文档与代码对照，确定。**

- `AGENTS.md`尚未实现清单／目录缺少dispatch，与ADR-026和实际API冲突。
- `docs/QUESTION-TYPES.md:43`仍称单生成器；`:23–29,173–179`将新7型M4写未测，而既有未提交M4报告与本轮已测失败。历史文档可保留日期语境，但当前目录应指向最新测量。
- DESIGN §17保留早期“风险层不存在”“移动端BLOCKED”“OCR未跑”等阶段叙述，后续ADR已经更新；缺少醒目的当前状态索引。
- `docs/AI-BRIEFING.md`自身是11月9日快照，不包含11月12日完整扩题与本轮结果，不能作为永远最新。

**建议：** 建立单一“当前能力／验收状态矩阵”，逐条链接实现、ADR、测试、最近证据、未测边界；历史章节明确“当时状态，非当前结论”。本轮只在新报告澄清，不擅自改写用户文档或AGENTS。

### F-07 · P2 · ignored工装污染全量Go扫描；审计输出也应与构建输入隔离

**证据强度：原树FAIL＋隔离恢复，确定工作区问题。**

`go test ./...`不遵守`.gitignore`；原树旧`release-artifacts`与`tmp`工装进入包扫描。release-check的`checkGoInventory`正确失败拒绝，不是门禁fail-open。隔离相同源码后该项PASS。

**建议：** 证据／临时Go工装放模块外，或使用明确独立模块／Go忽略命名规则；正式测试从隔离固定snapshot执行。现有文件删除需用户授权，不使用`git clean`处理。本轮保留全部旧文件。

### F-08 · P2 · 新M4工装尚欠防覆写、完整来源绑定与主路径测试

**证据强度：实现与本轮覆盖率，确定。**

- `cmd/m4baseline/main.go:39,113–124`直接运行默认指向`docs/reviews/evidence`，输出以`O_TRUNC`覆写；Makefile时间戳默认虽较安全，直接CLI仍可覆盖旧证据。
- 原summary只有HEAD／branch／dirty，未绑定M4未提交源文件的完整hash；本轮以外层源码清单补足。
- `internal/m4baseline`16.1%、solver28.0%，`cmd/m4baseline`无自身测试；成功复测不能覆盖错误／空输入／中断／输出失败的语义。
- CSV只有统计与备注，不存逐题公开题面hash／资产；不能从旧CSV恢复某个失败PNG。旧报告若声称`docs/reviews/evidence`已有具体图像，需先验证文件存在；该原目录本轮仅见CSV与JSON。

**建议：** 新目录／独占创建、显式overwrite需用户决定、写完原子交付、记录source／binary／schema hash；保留受控公开题面证据或其hash，禁止私有verifier进入产物。用新目录保留每次测量，不能以覆盖方式“更新历史”。

### F-09 · P1 · 交互模块加载没有超时，可能长时间卡在不可刷新的loading

**证据强度：静态控制流确认；真实浏览器挂起复现NOT_RUN。**

`web/widget/v1.js:765–790`只靠script.onload／onerror结束共享Promise，没有超时或取消；`:1833–1835`进入loading并禁用提交／刷新；`:2014–2030`等待模块后才proceed，而题目deadline的定时器在proceed末尾`:2007–2008`才装上。若资源连接一直pending而不触发load/error，页面保持前台且无visibility/focus事件时，没有独立计时器把它带回可重试状态。API自身requestTimeout不覆盖这次script加载；页面宿主仍可调用公共reset／refresh API，但用户可见按钮不能自救。

**建议：** 给共享模块加载设置有界超时、清理脚本／计时器与可重试状态；在等待模块时也保护challenge deadline。补“永不settle的模块请求”“TTL到期”“destroy”“多实例等待后重试”测试，并在真实浏览器验证。

### F-10 · P1 · sprite窄容器缩宽不缩背景，存在裁切与点选几何错配

**证据强度：CSS／渲染路径静态确认；真实窄屏布局复现BLOCKED。**

`web/widget/v1.js:594,621`为frameview设`max-width:100%`；`:1578–1582,1597–1600`使用原始px宽高／background-size／偏移。`web/widget/v1.interactive.js:802–806,836–840`的shell也使用固定px背景，而`:818–819`覆盖按钮按容器百分比放置。容器比原始帧窄时，div宽度被约束、背景和高度不同比缩放，右侧图像可能被裁，按钮位置也不再对应画面。它主要影响sprite题，不应因旧text-image窄屏PASS而外推通过。

**建议：** 用明确等比frame viewport与随实际显示尺寸一致的背景位置／点击映射，覆盖320／375px及更窄宿主容器；检查每帧右边缘与命中位置，不只检查页面无横向滚动。先真实浏览器复现后修复，未实跑前不写已验收。

### F-11 · P1 · symbol-path长度8的目标条超出画布

**证据强度：渲染公式与常量确认；本轮没有渲染截图／真人验收。**

`internal/generator/symbolpath.go:31–42`画布宽320、目标长度6–8；`:656–664`用40px卡片＋4px间隔。长度8时总宽`8×40＋7×4=348px`，居中起点`(320−348)/2=−14px`，两端各有14px落在画布之外。公开`target_h=48`（`:39,394`）与实际卡片高40也不一致。这是确定的几何缺陷，不能当混淆或安全特性；本轮不把旧报告“已有图像证据”的说法当作事实。

**建议：** 先补6／7／8长度目标条包围盒全部落在画布内的断言，再调整尺寸／间距或画布；明确payload几何到底是卡片尺寸还是保留区。核验所有glyph在最小尺寸可读，最后做真人与浏览器验收。唯一DFS答案不证明目标条可见。

### F-12 · P1 · 公共根路由漏挂交互模块，阻断新7型生产widget

**证据强度：父子mux完整路径静态确认，确定；真实Linux HTTP复现BLOCKED。这是当前最直接的产品接入缺陷之一。**

- `web/widget/widget.go:16,24–27,71–73`确实内嵌`v1.interactive.js`，并在widget子handler注册`GET /widget/v1.interactive.js`。
- **但是**`internal/api/api.go:137–139`公共根mux仅转发`widget.URLJS`和`widget.URLWorker`，没有转发`URLInteractive`，也没有widget目录前缀路由。子handler有路径不代表父mux会把该请求送进去。
- `web/widget/v1.js:756–768,2011–2030`在新7型领取后请求同源交互脚本；标准单二进制部署没有额外静态服务器补路由时，请求落在公共根mux的404，进入`interactive-unavailable`。
- `web/widget/widget_test.go`中的模块资源测试只测子handler；`docs/scripts/stage4a.sh:149`只探旧主脚本／worker；`widget_lifecycle.test.mjs`的模块用例通过vm直接注入并手动触发onload，绕过真实生产路由。因此“Node50 PASS＋资源测试PASS”仍漏掉此缺陷。

**影响：** 7个依赖新模块的题型虽然能生成、单元测试能渲染，用户经生产公共URL使用widget却无法加载渲染器。只修路由也不会改善F-01的抗自动化结果，这两类问题必须分别关闭。

**建议：** 先加从`api.Server.Handler()`真实公共根入口请求3个widget资源的回归（状态码／MIME／cache／nosniff／body），确认模块公开路由缺失时先红；然后最小挂载白名单第三资源，保持管理边界不变。补真实浏览器领新型→加载模块→输入→redeem→siteverify的集成闭环。当前报告不改代码、不声称已修复。

## 8. 安全保证与测试映射

本节说明哪些已有设计应当保留，以及哪些测试在本轮没跑。**测试存在性不等于运行通过**。

| 保证／能力 | 代码与既有测试定位 | 本轮状态 |
|---|---|---|
| PoW边界、错误解、共享JS／Go向量 | `internal/pow/{pow_test.go,vectors_test.go}`；`docs/scripts/browser/pow_vectors.test.mjs` | 原生Go／Node PASS；真实浏览器PoW NOT_RUN |
| token篡改、过期、purpose／kid／站点绑定 | `internal/token/token_test.go`、API契约／stage测试 | token包PASS；完整API BLOCKED |
| challenge／response原子一次性 | `internal/store/{store_test.go,pool_flow_test.go,ledger_regression_test.go}`；API stage tests | 代码审查／Linux编译；实际DB运行BLOCKED |
| 双DB句柄、过期快照／竞争领题 | `TestStaleSnapshotClaimFailsClosed`等store测试；DESIGN §17对应说明 | 本轮运行BLOCKED |
| 错误答案提交终态，畸形输入不消费 | `TestRedeemAttemptWrongAnswerCommits`、`TestConcurrentRedeemMixedAnswers`、`TestWrongAnswerTerminatesChallenge`、`TestAnswerIsBoundToItsOwnChallenge` | 测试存在／编译；本轮运行BLOCKED |
| secret校验先于消费、撤销不复活 | `internal/{api,admin,store}` credential／stage2／ledger回归 | 本轮完整运行BLOCKED |
| 管理写入与审计同事务 | `audit_transaction_test.go`、`audited_actor_regression_test.go`等 | 代码审查／编译；本轮运行BLOCKED |
| 图片／公开DTO不含私有字段、PNG元数据限制 | `internal/generator` leak／asset测试与API stage3b | generator PASS；API按值扫描BLOCKED |
| TSV容差与格式边界 | `internal/generator/{tsv_test.go,answer_encoding_test.go}`等 | 原生PASS＋短fuzz；不代表真实人容差适当 |
| 刷新／遥测／可达性限额 | `internal/risk/phase3_test.go`、`internal/api/phase3_test.go` | risk原测试PASS；新增跨窗回归FAIL；API BLOCKED |
| 管理静态壳／监听器隔离 | `web/adminui/adminui_test.go`、`internal/admin/ui_test.go`、server listener测试 | 资源包PASS；服务监听器运行BLOCKED |
| widget晚到响应／多实例／destroy／过期 | `widget_lifecycle.test.mjs`、`widget_browser.test.mjs` | Node工装PASS；真实浏览器NOT_RUN/BLOCKED |
| 备份／恢复保守失效／凭据唯一副本保留 | `internal/store/{deploy_test.go,restore_regression_test.go}`、`cmd/ops/main_test.go` | 代码与编译；本轮Linux实际运行BLOCKED |
| 运行锁跨进程／服务生命周期 | `internal/store/lock_test.go`、`cmd/server/lock_test.go`、ops真实子进程测试 | 编译PASS，运行BLOCKED；不能用Windows stub代替 |
| 生产依赖隔离 | server-dependencies原始JSON；`production-dependency-check.json` | Linux目标257依赖包，禁止的eval／M4／开发命令0个，widget存在，CgoFiles0；这是依赖图检查，不是运行验收 |

### 8.1 跳过与没有测试的范围

- 两个已有未来阶段占位：`TestStatsSortWhitelist`、`TestWeightAutoDemotionRule`，定义在`internal/admin/admin_test.go:754–759`附近；表示统计排序／分级淘汰未实现，不计PASS。
- 权限／只读目录测试有环境性`Skipf`；须在真实非root Linux身份下跑，不可用Windows权限行为或root拒绝工装替代。
- M4中的`go not in PATH`可跳过依赖隔离检查；本轮正确Go路径已提供，原生子集无test skip。
- `cmd/evalgen`、`cmd/m4baseline`、`web/preview`本轮包清单为无自身test文件；不把package-level“no test files”记作运行成功。
- 8个5秒fuzz只提升规范化入口的短时覆盖，不能替代长期fuzz、HTTP协议fuzz、数据库故障注入、race或依赖安全审查。

### 8.2 本轮没有发现或没有证据证明的事

没有把“未发现”写成“保证不存在”。当前审计未给出可验证的签名伪造、secret直接泄漏、SQL注入或原子消费绕过新复现；也**没有**完成足以排除这些类别的全动态审计。可疑但未核实的驱动／平台行为没有计为发现。

## 9. 发布、部署与运维判断

### 9.1 已有的正确边界

- 正式支持范围仍是 **B路径：宿主CGO=0静态构建＋named build context装配；Linux/arm64；UID:GID1000:1000**。amd64测试／A路径／其他UID不自动扩大范围。
- 生产Docker边界包括非root、只读根、cap drop、资源上限、公共口loopback／管理口不发布等；本轮只确认配置和已有验收设计，没有实际inspect新容器。
- `flock`是建议锁，只约束遵守规则的进程；不能抵御同等权限绕过代码直接开库。
- backup使用一致性快照，不是直接copy WAL模式主DB；backup／verify允许在线，不应误加互斥锁。
- restore保守失效旧challenge／response／凭据／站点并要求新master；旧快照不能保持快照之后的消费／撤销事实。
- manifest checksum＋integrity_check只提供相应完整性检查，不提供备份来源认证或保密性。
- plain Docker／Compose的unhealthy不会自动重启；restart策略围绕进程退出，不应引入无条件kill探针。

### 9.2 必须承认的未闭合项

1. **当前候选的完整Linux arm64 full与race未完成。** 本轮本机不具备条件；旧stage标签不跟随新HEAD自动重新验收。
2. **恢复后整体目录接管**等当前Linux非root演练仍有文档NOT_RUN（`BACKUP-RESTORE.md:174–181,377–380`、`DEPLOYMENT.md:334–338`）。
3. **掉电持久性模型**有明确边界：WAL／`synchronous=NORMAL`的断电／系统崩溃窗口不能与正常进程重启等价（ADR-022）。一次性业务对掉电后旧状态回退的容忍度需要业务明确接受或调整设计；本轮没有模拟断电。
4. **目标机1C1G稳态容量**：没有本轮持续真实流量、满图片预算、DB/WAL增长、GC／p99／OOM实测，不能凭12MB二进制或历史功能闭环RSS宣布预算达标。
5. **公网TLS／反代日志／备份自动化**：本轮未验证公网证书、各层日志无凭据、定时异机备份、轮转、告警和RTO/RPO。相关职责仍需运维落实。
6. **真实浏览器与人工项**：新7型、动画、键盘、辅助技术、真机、剪贴板、403/5xx分支等不能用Node DOM工装或截图代替。

### 9.3 CI不是不存在，但配置文件不等于已经跑绿

`.github/workflows/ci.yml`存在engineering矩阵；full部分明确为专用self-hosted环境／手动触发，并注明runner尚未配置的边界（`:91–99`附近）。本轮没有联网读取Actions运行历史，**远端CI实际执行状态未知**。不能写“没有CI”，也不能写“CI已自动证明发布可用”。

### 9.4 发布决定

**本报告不批准正式发布、不打完成标签、不承认stage-3／4／5整体完成。** 现有`stage-3a-done`、`3b`、`4a`、`4b`、`5a`、`5b`是历史范围的里程碑，不代表新题型、风险调度、当前恢复实现和当前候选全部重验。

## 10. 建议的后续开发顺序

### 10.1 最高优先级：先收敛，不再用题型数量衡量进度

| 顺序 | 工作 | 应产出的证据／关闭标准 |
|---|---|---|
| 1 | 将本报告与新7型FAIL作为当前风险事实；保留experimental，不推广默认池 | 当前状态矩阵与攻击器／源码／样本身份可追溯；不删历史证据 |
| 2 | 先补测试修F-12模块挂载、F-02冷却、F-03名额生命周期及题面几何／加载缺陷 | 红→绿日志；父mux与真实浏览器、边界／并发／提交失败测试；全量race后才宣称完成 |
| 3 | 修M4度量／防覆写／来源绑定与CLI测试 | 新旧metric区分、错误出口、空输入拒绝、重复目录不覆写、source hash验证 |
| 4 | 准备冻结候选和受支持的独立Linux环境 | 工具／UID／平台／源码／镜像／二进制全绑定；不得偷偷降门槛 |
| 5 | 执行full与真实浏览器／恢复／容量演练 | 完整原始输出，所有required都有非空证据；BLOCKED仍不通过 |
| 6 | 做真实用户／无障碍验证，与攻击成本共同决定重设计 | 人成功率、耗时、放弃率、辅助技术结果与机器成本同时具备 |
| 7 | 再补管理产品面与运营能力 | 只显示真实数据；审计／轮换／池观测／告警／备份演练可操作 |
| 8 | 最后考虑CF边缘、多节点或新外部依赖 | 独立ADR／明确授权／能力测试；不为解决题型效果而先堆架构 |

### 10.2 对题型研发的具体建议

- 每型建立M1随机空间、M2接受域、M3最优猜测／条件分布、M4实际攻击成本的同页卡片，明确哪些是公式、哪些是估算、哪些是真测。
- 随机猜测模型不能替代会读payload／几何的攻击器。公开结构化状态需要供交互使用时，应直接承认它可被程序读取，而不是称“没有answer字段所以安全”。
- 不以“隐藏initial_masks”或“再加噪声”直接宣布修复；image-only也可能低成本恢复mask，必须重新测。
- 唯一私有答案不等于唯一可见答案。裁切／遮挡／不可见差异要先解决；不可读不是安全特性。
- 先冻结solver和参数，再采集新holdout；记录调参集与正式集的界线。对真人难度不确定的题型，不应以机器0%当成功。
- 评估端到端攻击成本时另计PoW、网络、限流与每站点策略，不能混进单题图像solver时延。
- 自动淘汰仍须遵循候选告警＋人工确认／多信号规则，不能只凭“通过率高”自动下线。

### 10.3 对维护和交接的建议

- 当前能力矩阵至少包含：功能、API／文件、ADR、测试名、最近运行版本、状态、证据路径、未覆盖范围。
- 把实验、正式支持、可选、历史测量写成结构化字段，而不是散在几十份报告的叙述中。
- 临时工装不要落进主Go模块的`./...`扫描范围；构建与证据输出目录不要在运行中改变被hash的输入集。
- 以构建收据保存版本元组与原始二进制；checksum要保留但不冒充签名。备份认证／加密若要新增必须另行设计授权。
- 本轮未擅自commit／push；后续按项目规则测试→审查提交内容→commit→经授权push→核对远端OID，不能把本地报告当远端已备份。

## 11. 独立复核与重跑步骤

### 11.1 最快的离线复核路线

1. 读本报告第1、2、5、6、7节，避免把“命令完成”和“效果通过”混淆。
2. 运行相邻证据目录的`verify_evidence.py`，校验交付文件hash、收据输出hash、起止源码一致、M4索引与成功率。
3. 读`RESULTS-final.json`与`isolated-release-gate/summary.json`，确认full为INCOMPLETE/3，15/19/8各状态，并核对required项目。
4. 任取一份PASS与一份FAIL收据，按其精确argv／环境／cwd查原始日志；不要只看本报告摘录。
5. 核对`risk_cooldown_reproduction.go.txt`，在独立副本加入测试，运行指定单测；应观察当前实现失败。
6. 对照7型CSV逐题统计与summary；独立重跑应用新目录，不覆盖11月12日或本轮证据。
7. 若需要真正发布判断，转到受支持Linux环境运行第11.3节，而不是在Windows添加flock stub后冒充支持通过。

### 11.2 本轮Windows可复现的明确测试子集

在专案根、已安装的正确Go1.27.3和离线module cache环境中执行。以下示意`GO`需设为上文绝对路径，**不要允许工具链隐式下载**：

```bash
export GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOENV=off GOWORK=off
"$GO" test -json -count=1 \
  ./internal/generator ./internal/dispatch ./internal/eval \
  ./internal/pow ./internal/token ./internal/risk ./internal/version \
  ./internal/m4baseline ./internal/m4baseline/solver \
  ./web/widget ./web/adminui ./cmd/exampleapp ./cmd/preview \
  ./cmd/evalocr ./cmd/blindtest ./cmd/devclient ./cmd/loadtest
```

这条命令不是全量。完整精确命令、coverage参数、原始码在对应receipt；报告没有要求复查者执行不透明脚本才能相信结论。

M4应先build二进制，再使用**全新唯一目录**运行，直接读JSON verdict：

```bash
"$GO" build -trimpath -o /YOUR/NEW/OUTPUT/m4baseline.exe ./cmd/m4baseline
/YOUR/NEW/OUTPUT/m4baseline.exe --challenges 1000 --difficulty 3 \
  --lang zh-CN --out-dir /YOUR/NEW/OUTPUT/m4-run
```

上面`/YOUR/NEW/OUTPUT`是**操作占位路径，不是现存证据**，须替换；不要直接运行CLI默认覆写历史证据。新样本是随机的，单次成功数可波动，不能要求字节与本次CSV相同。代码哈希和判分规则应一致。

### 11.3 正式Linux复验清单（本轮未运行）

在B路径／Linux arm64／UID:GID1000:1000、专用空Docker环境、Go1.27.3／Node24／GNU Make／gcc／Firefox及既有stage工具全部具备、且候选干净后：

1. 保存候选commit、源码清单、工具链、平台、身份、Docker清单；核验工作树与构建输入。
2. 跑`go test -race -count=1 ./...`及项目full入口，**保留真正的JSON和脚本退出码**。
3. 跑所需stage4b／5a／5b／admin-ui与真实浏览器；由full计划决定，避免手工重复统计同一套测试充数。
4. 补新7型真实交互／键盘／窄视口／reduced-motion／各浏览器动画；人工项依模板由真人提供，不由AI代填。
5. 运行受控恢复接管、已消费token重放、撤销／停用不复活、备份／恢复故障边界、进程／容器互斥。
6. 在1C1G与实际配置／图片满载／持续流量下量RSS、p99、DB/WAL增长、清理与OOM；另行评估掉电语义。
7. 检查定时异机备份、恢复时间、监控告警、证书与反代各层日志；涉及外部调用／发布／远端变更先授权。
8. 所有required PASS且有证据后才讨论正式完成／发布，不能移动既有标签替代新修复提交。

本清单是复验建议，**不是执行证据**。

## 12. 限制、不应推导的结论与最终意见

### 12.1 本轮边界

- 覆盖整个专案的核心模块与交付链路，辅以四条只读专项审查；**不是对每行代码的形式化证明**。
- 本轮没有在Linux运行服务，没有真正访问SQLite消费路径／容器／外部VPS，没有真实浏览器GUI或人工用户测试。
- 关键后端风险以代码／已有测试／交叉编译作证；运行保证明确保持BLOCKED。
- 没有在线查依赖漏洞库或许可证新事实；本地`go mod verify`只能证明cache一致，不能宣布“无CVE”。
- 没有读取真实密钥文件，也没有对全部git历史做新的敏感信息扫描；本轮新增文件的复核不能替代全量secret-scan。
- M4只测试新7型、d3、一台参考主机和一组已开发solver；不能替代全部难度、其他题型、真人或完整业务攻防。
- 为复现F-02在隔离副本新增了一个测试文件；该副本从那一步开始不再是原始无附加测试的副本。此前full与build收据运行在加测试**之前**，原工作树生产源码始终未改。
- 临时隔离副本与交叉构建产物留在本机便于复查，未自动删除用户／历史文件；随报告的小型证据包不包含那些大型测试二进制，但保留命令和hash。

### 12.2 最终意见

**保留并继续发展这套工程底座是有价值的；现在应把开发重心从“继续增加验证码玩法”转向“用可复核攻击与真人数据筛选少数题型、修正风险／可达性缺陷、完成当前候选的Linux正式验收”。**

从当前实况看：

- **工程实现：已相当完整，不是空壳。**
- **产品范围：尚有明确缺项，不是全阶段完成。**
- **题型防机器效果：新7型已实测不合格，其他未测不能算合格。**
- **测试证据：本轮已尽量在可用环境补全并保留失败，但“完整测试全部通过”这一条件尚未满足。**
- **发布结论：不批准；继续实验与受控验证，保留`experimental:true`／`production_ready:false`。**

没有证据的地方明确留空，比把“工具在、代码在、历史绿过”写成当前验收完成更有价值。
