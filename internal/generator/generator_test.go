package generator

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// 契约级测试：阶段 0 只能验证类型可编译、接口形状稳定。
// stub 的存在是让 answer-leak 守卫自身被测试；具体题型在阶段 3 复用这些辅助函数。
func TestContractCompiles(t *testing.T) {
	var g Generator = stub{}
	if g.ID() != "stub-v1" || g.Media() != "text" {
		t.Fatalf("接口形状与 DESIGN §8.1 不一致")
	}
	c, err := g.Generate(t.Context(), Params{Difficulty: 1, Lang: "en"}, strings.NewReader("seed"))
	if err != nil {
		t.Fatalf("stub Generate: %v", err)
	}
	if !g.Verify(c.Answer, "42") {
		t.Fatal("stub Verify 应接受固定答案")
	}
	if g.Verify(c.Answer, "43") {
		t.Fatal("stub Verify 应拒绝错误答案")
	}
	assertNoAnswer(t, c.Payload, c.Answer)
}

// findAnswerLeak 检查 payload 序列化后是否含答案的任何规范化形式（DESIGN §8.2 统一约束）。
func findAnswerLeak(payload any, ans Answer) (string, bool) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "marshal payload: " + err.Error(), true
	}
	s := strings.ToLower(string(b))
	if ans.Canonical != "" && strings.Contains(s, strings.ToLower(ans.Canonical)) {
		return "payload 含答案 " + ans.Canonical, true
	}
	return "", false
}

func assertNoAnswer(t *testing.T, payload any, ans Answer) {
	t.Helper()
	if why, leaked := findAnswerLeak(payload, ans); leaked {
		t.Fatalf("答案泄露： %s", why)
	}
}

func TestAnswerLeakGuardDetectsLeak(t *testing.T) {
	if _, leaked := findAnswerLeak(map[string]any{"prompt": "答案是 42"}, Answer{Canonical: "42"}); !leaked {
		t.Fatal("payload 含答案时必须检出")
	}
	if _, leaked := findAnswerLeak(map[string]any{"prompt": "1+1=?"}, Answer{Canonical: "42"}); leaked {
		t.Fatal("正常 payload 被误判")
	}
	if _, leaked := findAnswerLeak(map[string]any{"a": 1}, Answer{}); leaked {
		t.Fatal("空答案不应触发")
	}
}

func TestArithAnswerSpaceLowerBound(t *testing.T) {
	t.Skip("待阶段 3（DESIGN §8.2）：模板×参数实例数 ≥2^10，且 500 题随机 payload 无答案泄露")
}

func TestTextWarpAnswerSpaceLowerBound(t *testing.T) {
	t.Skip("待阶段 3：长度 5–7、去易混字符集、字体随机指派生效；同一文本经 bloom 去重不复用")
}

func TestCJKOptionRandomization(t *testing.T) {
	t.Skip("待阶段 3：正确项位置随机 + 干扰项来自语料；语料再分发许可证登记在 docs/NOTICE.md")
}

func TestRotateAngleBucketing(t *testing.T) {
	t.Skip("待阶段 3：连续角→离散桶（粗 24 × 精 6），Verify 只做精确匹配不做容差")
}

func TestGenerateConsumesEntropy(t *testing.T) {
	t.Skip("待阶段 3：答案参数必须取自 entropy；不消费 entropy 或消费后仍产出可预测题目的实现判失败")
}

type stub struct{}

func (stub) ID() string    { return "stub-v1" }
func (stub) Media() string { return "text" }

func (stub) Generate(_ context.Context, _ Params, entropy io.Reader) (Challenge, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(entropy, buf); err != nil {
		return Challenge{}, err
	}
	return Challenge{
		Payload: map[string]any{"prompt": "1+1=?"},
		Answer:  Answer{Canonical: "42"},
	}, nil
}

func (stub) Verify(ans Answer, in string) bool {
	return subtle.ConstantTimeCompare([]byte(ans.Canonical), []byte(in)) == 1
}
