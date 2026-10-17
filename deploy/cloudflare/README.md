# deploy/cloudflare

阶段 5 交付物：`worker.js` + `wrangler.toml`（免费版）。

**只做无状态的事**（DESIGN §12、D8）：widget 与素材的静态缓存、PoW challenge 参数的 HMAC 签名下发。
**绝不做**：nonce 防重放、题池、统计写入 —— 免费 KV 只有 1000 写/天、D1 10 万行/天、
Durable Objects 需要付费计划、单次调用 CPU 上限 10ms。

边缘签发模式是弱一致折衷：token 无 nonce 记录，一次性完全依赖 redeem 时源站原子消费。
只适合低价值表单；严格一次性请用源站签发。降级：Worker 回源超时 2s → 返回
`{"error":"edge_unavailable"}`，widget 切 `fallback_base` 直连源站；源站不可用则验证失败
（安全优先，绝不静默放行）。
