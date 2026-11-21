package admin

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

// TestAccessibleOnlyChannelValidation 钉住通道可达性校验（ADR-034 §7）：
// 意外形状（pow_question + generator_id 单选 accessible-only + 空池）必须 400；
// 显式 pool 的 accessibility-only 与混合池仍是合法用法；编辑不得把合法站点改成意外形状。
func TestAccessibleOnlyChannelValidation(t *testing.T) {
	x := newHarness(t, nil)
	// harness 默认只登记 arithmetic；无障碍校验需要 accessible-choice 同样在册。
	acc, err := generator.NewAccessibleChoice(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := x.gens.Register(acc); err != nil {
		t.Fatal(err)
	}

	// 1) 意外形状：pow_question + generator_id=accessible-choice + 空池 ⇒ 400。
	r := x.do(http.MethodPost, "/api/admin/sites", map[string]any{
		"name": "accidental", "site_key": "pk-accidental",
		"verify_mode": "pow_question", "generator_id": generator.AccessibleChoiceID,
		"difficulty": 2,
	})
	if r.status != http.StatusBadRequest {
		t.Fatalf("隐式 accessible-only 单选应 400：%d %s", r.status, r.body)
	}

	// 2) 合法用法：显式 pool 的 accessibility-only 站点 ⇒ 201（不能一刀切禁用）。
	ok := x.do(http.MethodPost, "/api/admin/sites", map[string]any{
		"name": "deliberate", "site_key": "pk-deliberate",
		"verify_mode": "pow_question", "generator_id": generator.AccessibleChoiceID,
		"generator_pool": []string{generator.AccessibleChoiceID},
		"difficulty":     2,
	})
	if ok.status != http.StatusCreated {
		t.Fatalf("显式 pool 的 accessibility-only 站点必须可用：%d %s", ok.status, ok.body)
	}
	id := siteID(ok)

	// 3) 混合池（普通 + 无障碍）也是合法用法 ⇒ 201。
	mixed := x.do(http.MethodPost, "/api/admin/sites", map[string]any{
		"name": "mixed", "site_key": "pk-mixed",
		"verify_mode": "pow_question", "generator_id": generator.ArithID,
		"generator_pool": []string{generator.ArithID, generator.AccessibleChoiceID},
		"difficulty":     2,
	})
	if mixed.status != http.StatusCreated {
		t.Fatalf("混合池站点必须可用：%d %s", mixed.status, mixed.body)
	}

	// 4) 编辑把合法站点改成意外形状 ⇒ 400（fail-closed，错误码族不变）。
	bad := x.do(http.MethodPut, pathSite(id), map[string]any{
		"generator_pool": []string{},
	})
	if bad.status != http.StatusBadRequest {
		t.Fatalf("把显式池清空成意外形状应 400：%d %s", bad.status, bad.body)
	}
}

// TestGeneratorsExposeChannelFacts 钉住 /api/admin/generators 的通道元数据：
// accessible/motion 来自 dispatch 静态画像（与签发硬约束同一份），管理台据此展示通道限制。
func TestGeneratorsExposeChannelFacts(t *testing.T) {
	x := newHarness(t, nil)
	accGen, err := generator.NewAccessibleChoice(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := x.gens.Register(accGen); err != nil {
		t.Fatal(err)
	}
	r := x.do(http.MethodGet, "/api/admin/generators", nil)
	if r.status != http.StatusOK {
		t.Fatalf("generators: %d %s", r.status, r.body)
	}
	gens, _ := r.got["generators"].([]any)
	if len(gens) == 0 {
		t.Fatal("generators 清单为空")
	}
	found := map[string]map[string]any{}
	for _, item := range gens {
		g, _ := item.(map[string]any)
		id, _ := g["id"].(string)
		found[id] = g
	}
	acc, ok := found[generator.AccessibleChoiceID]
	if !ok {
		t.Fatal("无障碍专用题型未出现在清单")
	}
	if acc["accessible"] != true {
		t.Fatalf("accessible-choice-v1 的 accessible 元数据 = %v，期望 true", acc["accessible"])
	}
	arith, ok := found[generator.ArithID]
	if !ok {
		t.Fatal("arithmetic 未出现在清单")
	}
	if arith["accessible"] != false {
		t.Fatalf("arithmetic 的 accessible 元数据 = %v，期望 false", arith["accessible"])
	}
}
