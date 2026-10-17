package pool

import "testing"

func TestRefillBelowLowWatermark(t *testing.T) {
	t.Skip("待阶段 3（DESIGN §17）：注入低水位，断言补至 2*low 后停止")
}

func TestConcurrentClaimNoDuplicateDelivery(t *testing.T) {
	t.Skip("待阶段 3：-race 下并发领题，同一 pool.id 不得被两个 session 领取")
}

func TestExhaustedPoolFailsClosed(t *testing.T) {
	t.Skip("待阶段 3：连续 pool_miss 超阈值返回 429，不得降级为无题目放行")
}

func TestBurnAfterUse(t *testing.T) {
	t.Skip("待阶段 3：consumed 与过期行被清理任务物理删除（用后即焚），保留期不超过 TTL")
}
