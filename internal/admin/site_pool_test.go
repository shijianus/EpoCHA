package admin

import (
	"net/http"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func TestSiteGeneratorPoolRoundTrip(t *testing.T) {
	x := newHarness(t, nil)
	r := x.do(http.MethodPost, "/api/admin/sites", map[string]any{
		"name": "pool-admin", "site_key": "pk_pool_admin",
		"verify_mode": "pow_question", "generator_id": generator.ArithID,
		"generator_pool": []string{generator.ArithID}, "difficulty": 2, "pow_bits": 14,
	})
	if r.status != http.StatusCreated {
		t.Fatalf("创建失败: %d %s", r.status, r.body)
	}
	site, _ := r.got["site"].(map[string]any)
	pool, _ := site["generator_pool"].([]any)
	if len(pool) != 1 || pool[0] != generator.ArithID {
		t.Fatalf("创建响应 pool = %v", site["generator_pool"])
	}
	id := siteID(r)
	// 旧版单字段表单只带 generator_id/难度；不得清空已配置的池。
	oldForm := x.do(http.MethodPut, pathSite(id), map[string]any{
		"generator_id": generator.ArithID, "difficulty": 3,
	})
	if oldForm.status != http.StatusOK {
		t.Fatalf("旧版单字段更新失败: %d %s", oldForm.status, oldForm.body)
	}
	site, _ = oldForm.got["site"].(map[string]any)
	if pool, _ := site["generator_pool"].([]any); len(pool) != 1 || pool[0] != generator.ArithID {
		t.Fatalf("单字段更新清空了 pool: %v", site["generator_pool"])
	}
	upd := x.do(http.MethodPut, pathSite(id), map[string]any{
		"generator_pool": []string{},
	})
	if upd.status != http.StatusOK {
		t.Fatalf("清空 pool 失败: %d %s", upd.status, upd.body)
	}
	site, _ = upd.got["site"].(map[string]any)
	if pool, _ := site["generator_pool"].([]any); len(pool) != 0 {
		t.Fatalf("清空后 pool = %v", site["generator_pool"])
	}
}
