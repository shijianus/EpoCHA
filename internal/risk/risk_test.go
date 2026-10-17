package risk

import "testing"

func TestTokenBucketBoundaries(t *testing.T) {
	t.Skip("待阶段 2（DESIGN §9）：满/空/恢复速率边界，含跨窗口重置")
}

func TestIPLRUHardCapBoundsMemory(t *testing.T) {
	t.Skip("待阶段 2：注入 10 万个随机 IP 后 map 项数 ≤65536 且 RSS 不上涨（1GB 机器的生存条件）")
}

func TestIPMaskingStableAndSalted(t *testing.T) {
	t.Skip("待阶段 2：同 /24（v4）或 /64（v6）同日得到同一 ip_mac，跨日不同；DB 中无原始 IP")
}

func TestBehaviorSignalsCannotSolelyDeny(t *testing.T) {
	t.Skip("待阶段 3（DESIGN §9）：仅凭 widget 自检信号（可伪造）不得把 score 推过 deny 阈值")
}

func TestTrustProxyHeaderOnlyWhenEnabled(t *testing.T) {
	t.Skip("待阶段 2：未开启 trust-proxy 时 CF-Connecting-IP/XFF 一律忽略，防伪造")
}
