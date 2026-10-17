package generator

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// 契约级测试：阶段 0/1 只能验证类型可编译、接口形状稳定，以及泄露守卫本身有效。
// 具体题型在阶段 3 复用这些辅助函数（DESIGN §8.2 的 M1–M4 与统一约束）。
func TestContractCompiles(t *testing.T) {
	var g Generator = stub{}
	if g.ID() != "stub-v1" || g.Media() != "text" {
		t.Fatalf("接口形状与 DESIGN §8.1 不一致")
	}
	c, err := g.Generate(t.Context(), Params{Difficulty: 1, Lang: "en"}, strings.NewReader("seed"))
	if err != nil {
		t.Fatalf("stub Generate: %v", err)
	}
	if !g.Verify(c.Verifier, "42") {
		t.Fatal("stub Verify 应接受正确答案")
	}
	if g.Verify(c.Verifier, "43") {
		t.Fatal("stub Verify 应拒绝错误答案")
	}
	assertNoAnswerLeak(t, c.Payload, "42")
}

// findAnswerLeak 检查公开 payload 是否含答案的任何可识别形式（ADR-003 的硬约束）。
func findAnswerLeak(payload any, answer string) (string, bool) {
	if answer == "" {
		return "", false
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "marshal payload: " + err.Error(), true
	}
	if strings.Contains(strings.ToLower(string(b)), strings.ToLower(answer)) {
		return "payload 含答案 " + answer, true
	}
	return "", false
}

func assertNoAnswerLeak(t *testing.T, payload any, answer string) {
	t.Helper()
	if why, leaked := findAnswerLeak(payload, answer); leaked {
		t.Fatalf("答案泄露：%s", why)
	}
}

func TestAnswerLeakGuard(t *testing.T) {
	if _, leaked := findAnswerLeak(map[string]any{"prompt": "答案是 42"}, "42"); !leaked {
		t.Fatal("payload 含答案时必须检出")
	}
	if _, leaked := findAnswerLeak(map[string]any{"prompt": "1+1=?"}, "42"); leaked {
		t.Fatal("正常 payload 被误判")
	}
	if _, leaked := findAnswerLeak(map[string]any{"a": 1}, ""); leaked {
		t.Fatal("空答案不应触发")
	}
}

func TestArithMetrics(t *testing.T) {
	t.Skip("待阶段 3（DESIGN §8.2）：给出 M1 实例空间、M2 答案空间与容差、M3 猜测通过率（n=3 累计）、M4 实测尝试成本")
}

func TestTextWarpMetrics(t *testing.T) {
	t.Skip("待阶段 3：M1–M4 四指标；另需本地 OCR/多模态基线通过率测量，用以替换 DESIGN §4 的 60–90% 估算")
}

func TestCJKMetrics(t *testing.T) {
	t.Skip("待阶段 3：四选项题型的 M3 是主要弱点（单次 25%），必须给出 n=3 累计通过率与语料再分发许可（R3）")
}

func TestRotateMetrics(t *testing.T) {
	t.Skip("待阶段 3：连续角→接受域的折算必须实测；DESIGN 已把旧的「粗24×精6=144」降级为未验证估算，不得再当作安全性论据")
}

func TestGenerateConsumesEntropy(t *testing.T) {
	t.Skip("待阶段 3：答案参数必须取自 entropy；不消费 entropy 或消费后仍可预测的判失败")
}

func TestVerifierNeverInPayload(t *testing.T) {
	t.Skip("待阶段 3：四种题型各生成 500 题，断言 Verifier 内容与可推导种子均不出现在 Payload")
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
		Payload:  map[string]any{"prompt": "1+1=?"},
		Verifier: Verifier("42"),
	}, nil
}

func (stub) Verify(v Verifier, in string) bool {
	return subtle.ConstantTimeCompare(v, []byte(in)) == 1
}
