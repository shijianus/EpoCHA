// Package token 负责签名令牌的签发与校验（DESIGN §6）。
//
// 线格式 v1：<ver>.<kid>.<b64url(payload_json)>.<b64url(tag)>
// tag = HMAC-SHA256(K_purpose, "epocha:v1:" + b64url(payload))，
// K_purpose 由主密钥经 HKDF-SHA256 按用途（chal / resp / site）派生，不落库。
//
// 不变量（阶段 1 的测试必须逐条覆盖）：
//   - 任何字节被篡改 → 拒绝；比较一律 crypto/subtle.ConstantTimeCompare。
//   - payload 必须携带 exp 与站点绑定 sid，两者与请求上下文不符即拒。
//   - 解析前先检查总长度上限（4KB），payload 段禁止未知字段。
//   - kid 支持双活（current + previous）以完成无缝轮换。
package token
