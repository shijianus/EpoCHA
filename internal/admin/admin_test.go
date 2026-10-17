package admin

import "testing"

func TestBearerAuthMatrix(t *testing.T) {
	t.Skip("待阶段 2（DESIGN §10.2）：缺失/错误/已吊销 admin_key → 401/403，且不区分文案")
}

func TestSecretKeyShownOnceOnly(t *testing.T) {
	t.Skip("待阶段 2：创建站点响应含明文 secret；后续任何接口不再返回，库中仅 secret_mac")
}

func TestAuditCoversAllWrites(t *testing.T) {
	t.Skip("待阶段 2：每个写操作落 audit（actor/action/target），只读接口不落")
}

func TestListPaginationAndSortWhitelist(t *testing.T) {
	t.Skip("待阶段 5：分页上限 500；非法 sort 字段回落默认值，不得参与 SQL 构造")
}

func TestWeightAutoDemotionRule(t *testing.T) {
	t.Skip("待阶段 5（DESIGN §17 阶段 5）：suspect 率 >15% 且样本 ≥200 → weight 减半，连续两轮 → enabled=0")
}
