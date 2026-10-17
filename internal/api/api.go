// Package api 是公开 HTTP 层：路由、输入校验、限流中间件、reCAPTCHA 兼容错误码。
//
// 边界（DESIGN §10.1）：/api/challenge、/api/redeem、/siteverify、/asset/*、/widget/v1.js。
// 本包只做协议与校验，业务判断一律下沉到 pow/token/pool/risk/store。
// 实现顺序：阶段 1 打通三端点最小闭环，阶段 2 加限流与域名白名单。
package api
