# STAGE4A.md — 阶段 4A：widget 初版、业务接入示例、浏览器闭环（2026-10-18）

范围：原生 JS widget `v1.js` + PoW worker `v1.worker.js` + 示例业务后端 `cmd/exampleapp`
+ 真实浏览器交互验收。**不做**新题型、风险打分、自动淘汰、完整后台、CF Worker、公网部署。

---

## 1. 交付物与职责

| 路径 | 职责 |
|---|---|
| `web/widget/v1.js` | 生产 widget（零依赖、单一全局 `EpoCHA`、Shadow DOM 挂载、中英文、状态机、取消与超时、隐藏字段） |
| `web/widget/v1.worker.js` | PoW worker：纯 JS SHA-256 + 可取消的分块搜索。**同一份字节**既作 Blob 源也作同源文件（防漂移由测试钉住） |
| `web/widget/widget.go` | `go:embed` + 白名单资源名 + 版本化 URL 的只读 handler（不走 `ServeContent`，见 §6） |
| `internal/api/api.go` | 公共端口注册 `GET /widget/v1.js`、`GET /widget/v1.worker.js` |
| `internal/api/handlers.go` | `site_key` 一致性规则：URL 与请求体/token 不一致即拒（预检与实际同源同站） |
| `cmd/exampleapp/` | 开发专用示例业务后端：留言写入受 `/siteverify` 保护；CSP 四档；secret 只在进程内 |
| `internal/pow/testdata/pow_vectors.json` | JS↔Go **共享**固定向量（摘要、前导零、第一个解、被拒格式） |
| `docs/scripts/browser/` | 零安装的 BiDi 客户端 + 浏览器交互测试 + 向量测试 |
| `docs/scripts/stage4a.sh` | 阶段 4A 门禁（含 widget/示例/浏览器/回归，逐项只跑一次） |
| `docs/scripts/ocr-baseline.sh` | 把"退出码分层"变成可执行事实（见 §5） |

## 2. 怎么跑起来

```bash
make build
export EPOCHA_MASTER_KEY=$(openssl rand -base64 32) EPOCHA_PEPPER=$(openssl rand -base64 32) \
       EPOCHA_ADMIN_KEY=ak_$(openssl rand -hex 32)
bin/epocha --db /tmp/e4a.db --listen 127.0.0.1:8080 --admin-listen 127.0.0.1:8081

# 建一个演示站点（widget 需要 pow_question + 视觉题型 + 允许示例来源）
curl -sX POST http://127.0.0.1:8081/api/admin/sites \
  -H "Authorization: Bearer $EPOCHA_ADMIN_KEY" -H 'Content-Type: application/json' -d '{
  "name":"example","site_key":"pk_demo4a","pow_bits":12,"verify_mode":"pow_question",
  "generator_id":"text-image-v1","difficulty":2,
  "origins":["http://127.0.0.1:8091"],"allow_missing_origin":false}'
# → 记下返回的 secret（只出现一次）

go run ./cmd/exampleapp --listen 127.0.0.1:8091 --api http://127.0.0.1:8080 \
  --site-key pk_demo4a --site-secret '<上一步的 secret>' --csp blob
# → 浏览器打开 http://127.0.0.1:8091/
```

浏览器里要做的事（`make stage4a-check` 已把这些**自动化**跑过，人再做一遍是复核）：

1. 打开页面 → 组件自动领题（受服务端限流与题池容量约束）。
2. 看图、输入 6 个字符（大小写不敏感；`Tab` 可到输入框与按钮，`Enter` 提交）。
3. 提交 → 状态区进入"正在本地计算"，**浏览器自己算 PoW**（在 Dedicated Worker 里，页面不卡）。
4. 组件拿到一次性 `response` → `getResponse()` / 隐藏字段自动填好。
5. 点「提交留言」→ 示例后端带自己的 secret 调 `/siteverify`，只有 `success:true` 才写库；状态区显示"留言已写入"。
6. 再点「用同一个 token 再提交一次」→ 必然被拒（`epocha-duplicate-response`），列表不增加。
7. 不做人机验证直接点「提交留言」→ 被拒（`epocha-missing-response`）：这就是"前端 `onSuccess` 不是授权"。
8. 换一张 / 过期 / CSP 禁 Worker 的档位（`--csp no-worker`）→ 组件给出明确错误状态，不假装通过。

`cmd/preview`（阶段 3B 的开发预览）保留，但它**不是**"正式接入已完成"的证据：
它没有走生产 widget 脚本、没有跨域脚本加载、没有 CSP 矩阵。

## 3. 验收状态（三态口径：PASS / BLOCKED / NOT_RUN）

工程项只写 PASS/FAIL；外部条件不足写 BLOCKED；本轮没做写 NOT_RUN。**不存在"PASS：某项未测"**。

| 分类 | 状态 | 内容 |
|---|---|---|
| 后端/组件工程测试 | **PASS** | `go test -race` 全绿；`internal/api` 新增 site_key 一致性 3 个测试；`web/widget` 6 个测试（含两份 worker 拷贝防漂移、危险构造扫描）；`cmd/exampleapp` 13 个测试（含 fail-closed、超时、写入失败不缓存成功）；`internal/pow` 共享向量测试 |
| 跨实现一致性 | **PASS** | `node --test` 用 worker 的原始字节在 `vm` 里断言与 Go 生成的同一份向量一致（7 项） |
| 浏览器交互测试 | **PASS** | 真实 Firefox + WebDriver BiDi（零第三方驱动）：闭环、答错终结、一次性重放、多实例与隐藏字段、晚到响应、reset/destroy、计算超时预算、高难度取消、CSP 禁 Worker、键盘-only、请求面、凭据面 |
| 移动端 375px 无横向溢出 | **BLOCKED** | Firefox BiDi 没有可用的视口设置命令；node:test 以 `skip` 表达（不是通过）。需人工在真机或 DevTools 响应式模式下确认 |
| OCR 效果测量 | **BLOCKED** | 沿用 3B：本机无离线 OCR、未获授权安装/下载 ⇒ M4 未知 |
| 人工可读性 | **NOT_RUN** | 4A 没有新的人工参与数据；盲测入口仍在 `cmd/preview` |
| 生产适用性 | **未论证** | `text-image-v1` 仍 `experimental:true` / `production_ready:false`；widget 的存在不改变这一点 |

未做且**不假装**的：非视觉等价通道（音频等）本阶段只有视觉通道；PoW-only 降级不是等效替代。
iframe 方案未采用（直接 Shadow DOM 挂载），因此也不存在"换 Origin 后自报父域名"那条路。

## 4. 实测数字（本机 linux/arm64、go1.24.5、Firefox 147.0.2 headless、Node v22.21.1，2026-10-18）

| 项 | 实测 | 说明 |
|---|---|---|
| widget 体积 | `v1.js` 原文 48,412 B / **gzip 16,687 B**；与 worker 文件合计 gzip 21,121 B | 预算 30KB [目标]，由 `make widget-size` 守护 |
| 浏览器 SHA-256 吞吐 | **422,833 次/秒**（200,000 次耗时 473 ms） | Dedicated Worker 内的纯 JS 实现；`crypto.subtle` 逐次摘要的形态在预览里只有 2–4k 次/秒量级，那才是选它的原因 |
| bits=12 求解 | 451 次 / **4–6 ms** | 单个 challenge 的样本，不是分布结论 |
| bits=12 提交→verified 墙钟 | **103–145 ms** | 含 PoW + 一次 localhost 往返 + 判分事务 |
| bits=20 提交→verified 墙钟 | 四次采样 **309 / 1,551 / 1,862 / 4,064 ms** | 第一个解出现在第几次是几何分布，加上门禁并行时机器负载不同 ⇒ 只当量级证据：bits=20 在浏览器端是**秒级到十秒级**，不能随便往上抬。最后那次数值同时是"别把单次采样当常量"的例子 |
| 服务二进制 | 10,944,804 B（3B 是 10,813,732 B） | +128 KB 是 widget 与 worker 两份内嵌文本 |
| 示例后端进程 | VmRSS / VmHWM / Threads 见 `make stage4a-check` 输出 | 功能闭环形状，**不是**稳态；RSS 断言只用 100MB 目标线兜底 |

这些数字的含义边界：都是**同一形状内的单次采样**，没有多设备、没有真实网络 RTT、没有后台节流场景；
不能当容量规划输入。bits=20 那条只是"生产难度下浏览器端要花秒级"的量级证据。

## 5. 退出码分层（本轮按要求逐层实测）

| 层 | 命令 | 实测退出码 |
|---|---|---|
| 二进制 | `bin/evalocr --groundtruth …`（无 OCR 工具） | **3** |
| `go run` | `go run ./cmd/evalocr …` | **1**（`go run` 把任何非 0 折叠成 1，真实码只在 stderr 的 `exit status 3`） |
| shell 脚本 | `bash docs/scripts/ocr-baseline.sh` | **3**（先 build 再跑二进制，就是为了不被折叠） |
| Make | `make ocr-baseline` | **2**（make 把任何配方失败折叠成 2） |

⇒ 需要机器可读的"3 = BLOCKED"时只能走二进制/脚本那条路径；`stage3b.sh` 现在**按各层真实形状**断言。

同一件事在 JS 测试上也测了：`node --test <file>` 退出码 0/1（没有"未测"语义），
所以"未执行"必须由 `t.skip` 表达，并由 `stage4a.sh` 换算成 BLOCKED 计数；
`make` 那一层仍然只会给 0 或 2。

门禁的嵌套问题也一并修掉：`make stage4a-check` 只跑 `docs/scripts/stage4a.sh`，
脚本内部把 `fmt-check / vet / race / size / secret-scan / widget-size / stage1 / stage2 / stage3a / stage3b`
**各跑一次**并逐条打印退出码——同一套 `-race` 不再被跑四遍。

## 6. 本轮由"真实浏览器"抓到、curl 层永远看不到的缺陷

1. **`http.ServeContent` 把脚本的 Content-Type 覆盖成 `text/plain`**：系统缺 `/etc/mime.types` 时它按内容嗅探。
   配 `nosniff` 后浏览器直接拒绝执行（`blocked due to MIME type`）。改为自己写响应头与字节。
2. **示例页面的内联 `<style>` / `<script>` 被严格 CSP 挡下**：页面"就是不动"，但没有任何 HTTP 层症状。
   改成同源 `/app.css` + `/app.js`，配置走 `<script type="application/json">` 数据块（不被执行，不受 `script-src` 约束）。
3. **worker 的 `started` 消息被当成终态**：`_dispatch` 无条件 `delete pending[job]`，于是
   `started` 摘掉登记、真正的 `solved` 无处可寻，组件卡在 `solving` 直到超时。
4. **视口外坐标点击**：BiDi 报 `move target out of bounds`；点击前先 `scrollIntoView`（产品无关，测试自身的正确性）。
5. **示例应用端口冲突导致测试打到遗留进程**：宕机路径测试会连到**已在运行**的实例并"因为错误的原因通过"。
   门禁现在开头就检查固定端口是否空闲。

## 7. 与既有阶段的兼容

- 阶段 1/2/3A/3B 的验收脚本全部重跑，退出码 0（见 §3 表格与 `make stage4a-check` 输出）。
- 站点默认仍是 `pow_only`；`arithmetic-baseline-v1` 与 `pow_only` 两条旧路径回归通过。
- widget 只使用既有的 `/api/challenge`、`/api/redeem` 契约与服务端给出的 `expires_in`；
  **没有**为前端新增"答案"或"自报参数"的出口，也**没有**放宽任何一条安全红线。
- 唯一新增的公开面是两条只读静态路由；`site_key` 一致性是**收紧**（原来允许"预检一站、操作另一站"的歧义）。
