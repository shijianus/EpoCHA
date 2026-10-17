// Package store 负责 SQLite 打开、PRAGMA、迁移与查询（DESIGN §5）。
//
// 固定配置：WAL + synchronous(NORMAL) + busy_timeout(5000) + foreign_keys(ON)，
// 写者单连接串行（db.SetMaxOpenConns(1)）——这是"原子领题/双花防护"的前提，不是性能妥协。
// 一次性消费的唯一合法写法：BEGIN IMMEDIATE + UPDATE ... WHERE status='ready' AND
// RowsAffected==1；任何绕过该模式的读取-判断-写回都是 bug。
// 答案与站点密钥只存带 pepper 的 HMAC 摘要。实现于阶段 1（打开与迁移）/ 阶段 2（schema）。
package store
