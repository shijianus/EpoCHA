# deploy/cloudflare

**状态（2026-11-02）：未实现，阶段 5 之后才评估，不是当前部署包。** 本目录只有说明，
没有 `worker.js` 或 `wrangler.toml`。不得根据旧设计草案把边缘签发、边缘缓存或自动切源记为已交付。

| 能力 | 当前事实 | 后续验收条件 |
|---|---|---|
| Worker / Durable Objects | 未实现；权威验证节点仍为 VPS 上的 Go + SQLite | 按 DESIGN §12、ADR-001 先评估成本与信任根；若决定实施，先证明边缘原子领题/消费及故障语义 |
| 边缘 challenge 签发、widget/素材缓存 | 未实现；当前 challenge 由源站签发并落账，图片随 payload 内联，没有裸媒体端点 | 明确签名材料、来源绑定、缓存边界及唯一权威；不能把动态验证端点当静态缓存 |
| widget `fallback_base` | 未实现；当前失败重试不是跨 endpoint 的容灾切换 | 配置可信备用 endpoint、同一权威状态、超时与有界重试；全部失败仍 fail closed，不生成假 token |
| Nginx 前置 Cloudflare 信任配置 | `../docker/nginx.conf.example` 只有默认关闭的注释模板；不等于交付 Worker 或验收了 CDN | 另行授权并实测防火墙、官方网段与真实连接来源；不得仅凭客户端 `CF-Connecting-IP` 信任身份 |

**平台事实与工程选择分开**：ADR-001 已更正“必须付费”的旧断言——免费计划可以使用 SQLite-backed Durable Objects。
本文不重发旧配额/CPU 数字；实际选型时重新核对当日官方文档并记录日期，
2026-11-02 本轮没有联网核验。源站持有防重放账本是本项目的工程选择，不是平台不能持状态。

旧文中的“Worker 2s 超时 → edge_unavailable → fallback_base”是待评估目标，不是现有调用链。
边缘仍须遵守失败关闭，不能将无落账签发或静默 PoW-only 降级当作已批准实现。
这些待办保留在 [`DEPLOYMENT-READINESS-AUDIT` §6](../../docs/reviews/DEPLOYMENT-READINESS-AUDIT-2026-11-02.md) 的原功能台账，
此次冻结不取消原需求，也不宣称完整产品上线。
