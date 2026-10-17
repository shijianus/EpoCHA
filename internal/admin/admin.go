// Package admin 是管理面：站点与 Key CRUD、题型权重、题池状态、统计、密钥轮换（DESIGN §10.2）。
//
// 高危面处理：Bearer admin_key 与站点 secret_key 同样只存带 pepper 的 HMAC 摘要；
// 默认只监听 localhost，反向代理层再叠一次来源限制；所有写操作落 audit 表；
// 列表接口分页上限 500，排序字段走白名单。
// 鉴权失败次数进入内存锁定计数（与 risk 的令牌桶分开实现，避免互相影响）。
// 实现于阶段 2（API）/ 阶段 5（页面）。
package admin
