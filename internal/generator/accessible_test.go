package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func accessibleStream(t *testing.T, seed string) io.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAccessibleChoiceGenerateAndVerify(t *testing.T) {
	g, err := NewAccessibleChoice(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := AccessibleChoiceAnswerFrom(accessibleStream(t, "a11y-answer"), 3)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 3}, accessibleStream(t, "a11y-answer"))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q.Verifier, spec) {
		t.Fatalf("答案 %q 未通过", spec)
	}
	if g.Media() != MediaText || q.Asset != nil {
		t.Fatal("accessible-choice 必须是纯文本、无资产")
	}
	pub := q.Payload.(AccessibleChoicePublic)
	if len(pub.Options) != accessibleChoiceOptions {
		t.Fatalf("选项数 = %d", len(pub.Options))
	}
	seen := map[string]bool{}
	even := 0
	for _, o := range pub.Options {
		if seen[o.ID] || o.LabelZH != o.LabelEN {
			t.Fatalf("选项不合法: %+v", o)
		}
		seen[o.ID] = true
		n, err := strconv.Atoi(o.LabelZH)
		if err != nil {
			t.Fatalf("选项不是数字: %q", o.LabelZH)
		}
		if n%2 == 0 {
			even++
		}
	}
	if even != 1 {
		t.Fatalf("偶数选项数 = %d，期望 1", even)
	}
	wrong := "A"
	if spec == "A" {
		wrong = "B"
	}
	if g.Verify(q.Verifier, wrong) {
		t.Fatal("错误选项不应通过")
	}
}

func TestAccessibleChoiceNormalizeAndSanitize(t *testing.T) {
	g := AccessibleChoice{}
	if got, ok := g.NormalizeAnswer(" c "); !ok || got != "C" {
		t.Fatalf("NormalizeAnswer = (%q,%v)", got, ok)
	}
	for _, bad := range []string{"", "AA", "M", "1", "A B"} {
		if _, ok := g.NormalizeAnswer(bad); ok {
			t.Fatalf("畸形答案 %q 应拒绝", bad)
		}
	}
	g2, _ := NewAccessibleChoice(bytes.Repeat([]byte{0x62}, 32))
	q, err := g2.Generate(context.Background(), Params{Difficulty: 1}, accessibleStream(t, "a11y-shape"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(q.Payload)
	if _, err := g2.SanitizePublic(raw); err != nil {
		t.Fatalf("合法题面被拒: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["answer"] = "A"
	bad, _ := json.Marshal(m)
	if _, err := g2.SanitizePublic(bad); err == nil {
		t.Fatal("额外 answer 应被拒绝")
	}
}
