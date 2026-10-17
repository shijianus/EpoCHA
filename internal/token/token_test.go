package token

import "testing"

func TestSignVerifyRoundTrip(t *testing.T) {
	t.Skip("待阶段 1（DESIGN §17 ②）：签发→校验通过，payload 字段与 §6 一致")
}

func TestTamperedTokenRejected(t *testing.T) {
	t.Skip("待阶段 1：逐字节翻转 payload/tag/kid 任一位置均返回拒绝；含截断与超长（>4KB）")
}

func TestExpiredAndWrongSiteRejected(t *testing.T) {
	t.Skip("待阶段 1（DESIGN §17 ③）：exp 过期、sid 与请求 site_key 不符均拒")
}

func TestKidRotationDualActive(t *testing.T) {
	t.Skip("待阶段 2：previous kid 在 TTL 内仍可验证，新 token 用 current kid")
}

func TestPurposeKeysNotInterchangeable(t *testing.T) {
	t.Skip("待阶段 1：chal 密钥签的 token 不能被 resp 验证（HKDF 用途分域）")
}
