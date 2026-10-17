package api

import "testing"

func TestSiteverifySuccessShape(t *testing.T) {
	t.Skip("待阶段 1（DESIGN §17 ⑥）：成功响应 key 集合恰为 success/challenge_ts/hostname")
}

func TestSiteverifyFailureShape(t *testing.T) {
	t.Skip("待阶段 1：失败仍返回 HTTP 200 且 key 为 success/error-codes（连字符），不暴露内部原因")
}

func TestErrorCodesAreRecaptchaOnly(t *testing.T) {
	t.Skip("待阶段 1：错误码必须落在 §10.1 列举的 6 个 reCAPTCHA 族内，禁止自定义码外泄层次")
}

func TestBodySizeLimits(t *testing.T) {
	t.Skip("待阶段 2（DESIGN §14 ⑤）：公开端点 4KB、管理端 64KB 上限，超限 400")
}

func TestOriginWhitelist(t *testing.T) {
	t.Skip("待阶段 2：域名白名单不符 → bad-request；CORS 不得回显未校验的 Origin")
}

func TestRateLimit429(t *testing.T) {
	t.Skip("待阶段 2：超 IP+site_key 阈值返回 429 且带 Retry-After")
}

func TestNoSecretInLogs(t *testing.T) {
	t.Skip("待阶段 2：跑全流程后 grep 日志，secret/response 全文/answer 不得出现（make secret-scan）")
}
