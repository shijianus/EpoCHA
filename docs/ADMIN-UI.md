# 管理控制台 M1（阶段 5C 第一批）

交付范围：**登录 → 站点列表 → 创建站点 → 编辑站点策略与 Origin → 启用/停用**。
代码在 `web/adminui/`（内嵌静态资源）与 `internal/admin`（挂载点），后端只有一处最小扩展（§5）。

这不是一份"计划要做"的文档：下面每条都有对应的测试或门禁判据，未做到的写在 §8 里，不混进通过项。

---

## 1. 挂载与网络边界

| 面 | 位置 | 说明 |
| --- | --- | --- |
| 页面与资源 | 管理监听器 `GET /admin`、`/admin/`、`/admin/app.js`、`/admin/app.css` | 与 `/api/admin/*` **同一个** Server、同一个监听器 |
| 公开 API | 公共监听器 | 没有这些路由，也没有管理 API 的路由 |

三条要点：

1. **挂载点本身就是边界。** 公共监听器上 `/admin` 与 `/api/admin/*` 都是 404（不是 401——
   401 意味着"路由挂上了、只差鉴权"，那是另一种暴露）。判据在
   `cmd/server/main_test.go:TestAdminUISitsOnlyOnAdminListener`（两个真实监听器）与
   `docs/scripts/admin-ui-check.sh` 第 4 步（curl 端到端）。
2. **没有为了浏览器方便做过任何放宽**：不发布管理端口、不把 `EPOCHA_ADMIN_LISTEN` 改成
   `0.0.0.0`、不加 CORS 头（管理面对跨源脚本本来就不该可达）、不加未鉴权的管理代理路由、
   不改已验收的 Docker 网络边界（`deploy/docker/compose.yml` 未动）。
3. 页面壳**可以**在未登录时取到（它是静态资源，不含任何数据）；真实门禁仍然是
   每个 `/api/admin/*` 请求头里的 Bearer 校验。"能取到页面"不等于"能读到数据"，
   这条由 `internal/admin/ui_test.go:TestAdminUIMountedOnAdminHandler` 钉住。

响应头（三个资源一致）：`Content-Security-Policy` 无 `unsafe-inline`/`unsafe-eval`、
含 `frame-ancestors 'none'`、`default-src 'none'`、`connect-src 'self'`、`form-action 'none'`、
`base-uri 'none'`；另有 `Cache-Control: no-store`、`X-Content-Type-Options: nosniff`、
`X-Frame-Options: DENY`、`Cross-Origin-Opener-Policy/Resource-Policy: same-origin`。
`no-store` 是刻意的：一份过期的 `app.js` 会让运维看到与后端不符的字段，省字节不值得这个歧义。

资源名是**白名单常量**（`web/adminui/adminui.go`），不存在按请求参数拼路径，
因此目录穿越在结构上不可能；仍探了一遍（`adminui_test.go:TestUnknownSubpathsAreNotFound`）。

## 2. 凭据模型（以及它不保证什么）

- 管理凭据只活在 `app.js` 闭包的 `session.key` 里：**不写** localStorage / sessionStorage /
  IndexedDB / Cookie，不进 URL 与 hash，不进 console，不进任何配置对象。
- 提交登录时**先清空输入框**，成败都不留原文；401 一律清状态并回登录面。
- 退出 = 丢弃凭据引用 + 清空视图 DOM 与草稿 + 作废题型缓存；重新进来必须重新输入。
- 错误分族，不笼统："凭据无效"（401）、"请求过于频繁"（429）、
  "无法连接管理 API（这不是凭据问题）"（fetch 失败）、"请求被服务端拒绝：字段不符合边界"（400）、
  "与现有记录冲突"（409）、"服务端内部错误，本次改动没有生效"（500）。
  403/5xx 两支在当前实现里**无法真实触发**（§8）。
- **不声称能保证物理擦除 JavaScript 内存。** 这里防的是"凭据被写进可被他人读取的持久介质"，
  不是"进程内存里不存在历史副本"。
- 页面不拼接任何服务端字符串到 `innerHTML`：只有 `textContent` 与 `createTextNode`
  （`app.js` 的 `el()` 助手没有 innerHTML 分支）。错误响应的 `error` 字段只作为文本拼进一句
  固定话术，其余响应体不落地成 HTML。

## 3. 视图与真实 API 的对应

| 视图 | 用的端点 | 页面显示的事实 |
| --- | --- | --- |
| 凭据入口 | `GET /api/admin/whoami` | 成功后显示公开 `key_id`，不显示凭据原文 |
| 站点列表 | `GET /api/admin/sites?limit=20&after_id=N` | 名称 / `site_key` / 验证模式 / 题型 / 状态徽标 / 版本；游标分页（后端只有前进游标，上一页由页面记栈） |
| 创建站点 | `POST /api/admin/sites` | 名称、`site_key`（留空由服务生成 `pk_…`）、`verify_mode`、`generator_id`、难度档、PoW bits；201 结果里**一次性**展示 secret 明文 |
| 站点详情 | `GET/PUT /api/admin/sites/{id}`、`PUT /api/admin/sites/{id}/origins` | 概览（ID/`site_key`/状态/版本/创建时间）+ 策略表单 + Origin 列表 + 凭据视图（只有 `key_id` 与时间） |
| 启停 | `POST /api/admin/sites/{id}/enable|disable` | 二次确认里点名站点名称、`site_key`、当前版本 |
| 题型下拉 | `GET /api/admin/generators` | `experimental` 与 `production_ready` 两个徽标 + 服务端 `note` 原文 |

列表与详情的空状态、失败状态、重试都是真实分支（加载中 / 空列表 / 失败 + 重试按钮），
**没有**任何 QPS、成功率、趋势图——那些数据本身还不存在，画出来就是编造。

## 4. 保存路径的具体行为

- **只发改动过的字段。** `PUT /api/admin/sites/{id}` 的部分更新语义（nil = 不改）直接落到
  请求体上，避免"顺手把没动的字段按页面默认值写回去"。
- **保存后重新读取。** `confirmSaved()` 再发一次 `GET`，逐字段与 PUT 的期望值比对；
  不一致就报"已保存但复读结果不一致"，而不是把 200 当成落库成功。
- **防重复提交。** 请求在途时提交按钮禁用；`if (submit.disabled) return` 挡住同一 tick 的二次触发。
- **未保存修改的守卫。** 详情页挂着"策略"与"来源"两张表单，脏标记**分部记账**
  （`setDirty(part, …)`）——只有一根布尔量的话，保存策略会把"来源还没存"这件事一起抹掉。
  有脏数据时切页会弹确认横幅，且横幅是**叠加**在表单之上而不是重建内容：
  重建会让"留下继续编辑"拿回一张空表单，等于用提示这个动作把修改抹掉了。
  Origin 的未保存编辑另有草稿（`originsDraft`，按站点 id 记），保存策略重画详情页时不会丢。
  关标签/刷新另有 `beforeunload`。
- 两个端点的失败是**分别报告**的：策略 PUT 失败 ⇒ 什么都没写；策略成功而来源 PUT 失败 ⇒
  文案点名"来源保存失败"，并且复读后的页面显示的就是库里真实状态。

## 5. 后端最小扩展：站点名称可改

需求要求详情页能改名称，而 `PUT /api/admin/sites/{id}` 原本不含 `name`。改动限定在：

- `store.SiteUpdate` 增加 `Name *string`（nil = 不改），`UpdateSite` 的 UPDATE 语句带上 `name=?`，
  校验复用创建路径同一个 `validateName`（`printableOK` + `MaxNameLen=64`）；
  撞 `UNIQUE` 映射为 `ErrConflict` ⇒ HTTP 409。
- `updateSiteRequest` 增加 `Name *string`。严格解析（`DisallowUnknownFields`）保持不变，
  所以请求里出现 `site_key` 仍是 400——**`site_key` 不可改**是刻意的：它是签发绑定与
  来源判定的锚点，改它等于换站点，会让业务后端写死的值静默失效。

理由与代价记在 ADR-019。名称**不参与任何判定**（账本按 `site_id` 外键、下发按 `site_key`），
所以改它不是"立即生效的安全动作"：**不提升站点版本**，不使任何已签发的题目或 token 失效。
这条由 `store`（`TestUpdateSiteName`）与 API（`TestSiteNameUpdateViaAPI`）两层测试钉住，
包括"nil 不改名"、"失败改名不影响任何一方"、"重启后仍在"。

## 6. 启停与实验题型的口径

- 启停都会提升站点版本（`SetSiteEnabled` 只在状态**变化**时 +1），因此
  **停用前签发、尚未消费的 token 在重新启用后也不会复活**（DESIGN §5.3）。
  确认横幅里就写这句话，不缩成"确定吗？"。
- `DELETE /api/admin/sites/{id}` 是软吊销（停用 + 提升版本），不删行——控制台的 M1
  没有暴露"删除站点"按钮：硬删除会拆掉防重放账本，而这件事不该在页面上被一键触发。
- 题型下拉旁边显示的是服务端给回的 `experimental` / `production_ready` 两个布尔值
  （`arithmetic-baseline-v1` 与 `text-image-v1` **都是** `experimental:true` +
  `production_ready:false`），加上 `note` 原文。页面上没有任何一处把它们说成已验证的生产防线
  （红线 14、DESIGN §8.4）。

## 7. 本地演示

```bash
make admin-demo        # 或 bash docs/scripts/admin-demo.sh
```

它只做本轮的事：编译到临时目录 → `mktemp -d` 的**空库** → **运行时随机**的
master/pepper/admin 三个 0600 文件 → 公共与管理两个监听器都绑 `127.0.0.1`
（默认 8411/8401 之外的 8401/8402）→ 打印浏览器地址与管理凭据文件路径。

- 管理凭据**不由网页自动获取**，也不打印内容；终端只告诉你文件位置和你自己该怎么看
  （`cat $WORK/keys/admin`）。
- 启动前检查固定端口空闲；被占用就中止——连到遗留进程会让"看到的数据"不属于本轮。
- `Ctrl-C` 只清理本轮创建的临时目录，不碰仓库的 `bin/`、`.env`、`keys/`、既有数据库。
- 根文件系统剩余空间不足 400MB 且 `/dev/shm` 可写时，本轮构建缓存与临时目录搬到 tmpfs
  并打印原因；不删除任何既有文件、不改全局配置。
- 演示用的是 `EPOCHA_POOL_ENABLED=false` 的空库实例，**不是**部署初始化流程；
  真实部署仍按 `docs/DEPLOYMENT.md` §11 的启动配方。

## 8. 验收与未做

门禁：

```bash
bash docs/scripts/admin-ui-check.sh    # 结论看最后一行 RESULT=
```

退出码三态：0 = 全 PASS；1 = 有 FAIL；3 = 无失败但存在 BLOCKED/NOT_RUN。
`make admin-ui-check` 只能报 0 或 2，真码看脚本打印。

已实际执行（两项都由真实进程/真实浏览器跑过，不是静态断言）：

- Go：`./web/adminui`（白名单、ETag=内容哈希、安全头、未知子路径 404、非 GET 拒绝、
  HEAD 带长度不带正文、资源里无凭据形状）、`./internal/admin`（挂载 + 未鉴权 API 仍 401 +
  发出的字节 == 内嵌字节 + 名称更新的 API 矩阵）、`./cmd/server`（两个真实监听器的边界）、
  `-race -count=1`。
- 浏览器（Firefox 147.0.2 BiDi + Node 内置 WebSocket，零第三方依赖）：错误 key 的 401 文案、
  正确 key 登录后显示 `key_id`、创建站点并拿到一次性 secret、进详情取不回 secret、
  改名称/难度/PoW/限流/Origin 后保存并与服务端复读对账、越界值被服务端拒（400）、
  停用/启用的二次确认与真实版本变化、题型结论显示、退出后视图清空、
  375×667 真实视口下控件可命中且无横向溢出、连续失败后的 429 文案不被说成凭据问题、
  服务真停掉后的"无法连接管理 API"文案。
  凭据不进存储的判据是页面里读 `localStorage`/`sessionStorage`/`document.cookie`/
  `indexedDB.databases()` 四面**全为空**。

明确未做到 / 未覆盖：

| 项 | 状态 | 原因 |
| --- | --- | --- |
| 403 与 5xx 两支错误文案 | **NOT_RUN** | 当前管理 API 不产生这两种响应（鉴权只有 401/429；写入错误是 400/404/409）。分支在代码里存在，但没有能真实触发它的端点 |
| 剪贴板写入 | **NOT_RUN**（测的是回落路径） | headless Firefox 不给剪贴板权限；页面在该情况下走"整段选中 + 提示手动复制"，测的是这条回落 |
| 真机移动端 | **NOT_RUN** | 375×667 是 BiDi 设进去的真实视口（`innerWidth`/`clientWidth` 实测相等），不等于真实设备与真实 DPR |
| 键盘 Tab 序列逐站点核对 | **NOT_RUN** | 表单有 label、焦点管理有（横幅出现时聚焦第一个按钮、切页后聚焦标题），但没做完整的 Tab 序与读屏播报核对；辅助技术实测仍是阶段 4 起就欠着的人工项 |
| 题池观测 / 审计 / 凭据轮换的管理页面 | **未交付** | M1 范围只有站点主流程。`/api/admin/pool`、`/api/admin/audit`、`secrets/rotate` 等端点存在，页面没有对应视图（导航里也就没有指向空页的标签） |
| 多管理员 / 会话 / RBAC | **不做** | 本轮明确不引入账户体系与 Cookie 会话，也不改现有管理密钥协议 |
| 风险评分、题型权重、自动淘汰、新题型、公网部署 | **不做** | 不在里程碑内 |
