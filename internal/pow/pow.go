// Package pow 负责工作量证明的参数签发与验证（DESIGN §7）。
//
// 协议 sha256-prefix：SHA256(seed || "<client_nonce>") 前 bits 位为 0。
// seed 与 session nonce 一一绑定，使 PoW 解无法跨站点或跨题目摊销。
// 验证成本固定为 1 次哈希；memory-hard 协议位为 v2 预留（默认关闭，因验证侧同样变贵）。
// 实现于阶段 1。
package pow
