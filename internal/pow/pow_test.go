package pow

import "testing"

func TestVerifyKnownVector(t *testing.T) {
	t.Skip("待阶段 1（DESIGN §17 ①）：固定 seed/bits/nonce 的 true/false 向量，含 bits 边界 12/24")
}

func TestSeedBoundToNonce(t *testing.T) {
	t.Skip("待阶段 1：同一 PoW 解跨 session/跨站点必须失败，防止打码农场摊销算力")
}

func TestSolveTimeLowerBound(t *testing.T) {
	t.Skip("待阶段 1：redeem 时延低于理论下限 20% 时置 suspect 信号（DESIGN §7）")
}
