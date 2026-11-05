# Cloady 测试部署与 API 接入

状态：**amd64 测试部署，不是正式生产发布。** 正式支持范围仍是 Linux arm64；本页记录的是
`jian-shi/jpn1` production 的 Cloady 测试实例、API 契约和已知边界。

## 1. 当前入口

| 项 | 值 |
|---|---|
| Cloady workspace | `jian-shi` |
| App / env / region | `jpn1` / `production` / `jpn1` |
| 镜像版本 | `epocha cloady-2026-11-05 (commit 6db855675e7b6ccaca9e78a8719365ba598b9671, built 2026-11-05T00:00:00Z)` |
| 公共 API / 页面基址 | `https://jpn1-ub5s1o5q-jian-shi.cloady.io` |
| 只读测试页 | `https://jpn1-ub5s1o5q-jian-shi.cloady.io/demo?site_key=pk_jpn1_v2_20261105` |
| 测试站点 site_key（公开） | `pk_jpn1_v2_20261105` |
| 测试站点 secret | 本机 `tmp/cloady-site-v2-secret-final3.txt`（仅当前用户 NTFS ACL；`tmp/` 已 gitignore；**不要提交、不要贴聊天**） |
| 题型 / 难度 | `text-image-v2` / 1（8 字符，480×160） |
| 健康检查 | `/healthz`、`/readyz`、`/widget/v1.js` 均为 200 |
| 管理面 | 不发布；公网 `/api/admin/*` 为 404 |

`/demo` 只在 `EPOCHA_DEMO_PAGE=1` 时挂载，只调用公开 challenge/redeem；它不读取 secret、
不调用 `/siteverify`、不写业务数据。它**不是**业务接入示例，接入见 §3。

Cloady 边缘偶发 TLS/连接重置（实测本机 `/demo` 200、VPS 出口偶发 EOF）；这是平台入口的
瞬时故障，不是应用 5xx。遇到时重试即可，不能把一次 000/EOF 记成验证失败。

## 2. API 闭环

### 2.1 领题

```bash
curl -sS -X POST "$BASE/api/challenge" \
  -H 'Content-Type: application/json' \
  -H "Origin: $BASE" \
  -d '{"site_key":"pk_jpn1_v2_20261105"}'
```

响应（节选）：

```json
{
  "token": "<challenge token>",
  "expires_in": 180,
  "pow": {"algo":"sha256-prefix","seed":"<base64>","bits":14},
  "mode": "pow_question",
  "question": {
    "kind": "text-image",
    "generator": "text-image-v2",
    "generator_version": "v2",
    "length": 8,
    "charset": "ABCDEFGHJKLMNPQRSTUVWXYZ23456789",
    "answer_type": "text",
    "max_answer_len": 8,
    "media": "image",
    "image_mime": "image/png",
    "image_width": 480,
    "image_height": 160,
    "image_base64": "<PNG base64>"
  }
}
```

规则：
- PoW：`SHA-256(seed_bytes || decimal_nonce)` 的前导零比特 ≥ `pow.bits`；nonce 是 1..16 位十进制且 ≤1e12。
- 答案按**水平从左到右**读（v2 的字会上下错位）；服务端大小写不敏感、裁首尾 ASCII 空白，内部空白/字符集外/长度不是 8 判畸形。
- 站点同时允许精确 Origin（当前页面域名）和缺失 Origin（服务端到服务端调用）；生产站点不要照抄 `allow_missing_origin=true`。

### 2.2 兑换

```bash
curl -sS -X POST "$BASE/api/redeem?site_key=pk_jpn1_v2_20261105" \
  -H 'Content-Type: application/json' \
  -H "Origin: $BASE" \
  -d '{"token":"<challenge token>","pow_nonce":"<nonce>","answer":"<8 chars>"}'
```

成功：`200 {"response":"<一次性 response token>","expires_in":120}`。
答错/过期/重复兑换：`400 {"error-codes":["invalid-challenge"]}`（有限码集，不透露哪一层失败）。

### 2.3 服务端验证（唯一授权点）

```bash
curl -sS -X POST "$BASE/siteverify" \
  --data-urlencode "secret=$EPOCHA_SITE_SECRET" \
  --data-urlencode "response=$RESPONSE"
# 首次：{"success":true,"challenge_ts":"...","hostname":"...","error-codes":null}
# 重放：{"success":false,"error-codes":["timeout-or-duplicate"]}
```

`secret` 只在业务后端使用，绝不进浏览器、HTML、JS、页面配置或日志。浏览器里的
`onSuccess` 只是 UI 信号，不是授权。

## 3. widget 接入

业务页面只放公开 `site_key`：

```html
<div id="captcha"></div>
<script src="https://jpn1-ub5s1o5q-jian-shi.cloady.io/widget/v1.js" defer></script>
<script>
  document.addEventListener("DOMContentLoaded", function () {
    EpoCHA.render(document.getElementById("captcha"), {
      apiBase: "https://jpn1-ub5s1o5q-jian-shi.cloady.io",
      siteKey: "pk_jpn1_v2_20261105",
      language: "zh-CN",
      fieldName: "epocha_response" // 或 "" + 自己读 getResponse
    });
  });
</script>
```

业务后端收到 `epocha_response` 后调用 `/siteverify`，只有 `success:true` 才执行受保护操作。
不要用 `onSuccess` 做授权，也不要把 secret 下发到浏览器。

## 4. 部署方式与边界

- 构建上下文由 `deploy/cloady/` 的 Dockerfile + 仓库源码组成；不要在 Cloady 构建上下文里放
  `cmd/exampleapp`、`internal/eval`、文档、评测素材或密钥。
- Cloady 的 upload 应用**不允许原地改 source**：`PATCH /apps/jpn1` 对 upload source 返回
  `400 Only a git app's repository, branch or root directory can be changed.` 更换代码需要删除并
  重建应用。这会丢失容器内 SQLite，站点/凭据必须重新播种；当前测试部署**没有持久卷**。
- Cloady 的 POST API 对裸 `curl` 有边缘 WAF 拦截；需要带浏览器式 `User-Agent`、`Origin`、
  `Referer`。这不是 API 权限问题；token 本身可用。
- 当前实例是 amd64 测试部署；项目正式支持范围是 Linux arm64。Cloady 公网 HTTPS 是平台入口，
  不是项目自有域名/证书/反代验收。
- `production_ready:false` 不变；`text-image-v2` 的 OCR 数字见 ADR-023，不能外推成抗 AI。

## 5. 本轮实测收据

| 项 | 结果 |
|---|---|
| Cloady app status | `running` |
| `/healthz` / `/readyz` / `/widget/v1.js` | 200 / 200 / 200 |
| `/demo?...`（headless Chromium） | 页面标题与 widget 均加载，组件状态 `ready`，8 字符标题/占位符一致 |
| `/api/challenge` | 200，`text-image-v2`，8 字符，480×160 |
| `/api/redeem` | 200，返回 response token |
| `/siteverify` 首次 | `success:true`，hostname 正确 |
| `/siteverify` 重放 | `success:false`，`timeout-or-duplicate` |
| v1 OCR holdout d1/d3/d5 | 95% / 31% / 3%（tesseract 5.3.4 / eng 4.1.0，100 张/档） |
| v2 OCR holdout d1/d2/d3/d5 | 0% / 0% / 0% / 0%（同一工具与分母） |

OCR 结论只对“该固定工具、固定参数、固定 holdout、该机器”成立；人工可读性、辅助技术、
真机移动端、抗多模态与目标机攻击成本仍为 **NOT_RUN**。
