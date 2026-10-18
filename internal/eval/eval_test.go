package eval

import (
	"io"
	"testing"
)

func TestDeterministicReaderIsReproducibleAndDistinct(t *testing.T) {
	read := func(seed string, n int) []byte {
		r, err := NewDeterministic([]byte(seed))
		if err != nil {
			t.Fatalf("NewDeterministic: %v", err)
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	a := read("epocha-eval-seed-1", 200)
	b := read("epocha-eval-seed-1", 200)
	c := read("epocha-eval-seed-2", 200)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("同一 seed 第 %d 字节不同：确定性不成立", i)
		}
	}
	same := 0
	for i := range a {
		if a[i] == c[i] {
			same++
		}
	}
	if same > 4 {
		t.Fatalf("不同 seed 的输出有 %d/200 字节相同，不像独立流", same)
	}
	// 无限流：读超过一个摘要块（32B）不该 EOF
	r, _ := NewDeterministic([]byte("epocha-eval-seed-3"))
	buf := make([]byte, 4096)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("长读应可用: %v", err)
	}
}

func TestDeterministicReaderRejectsShortSeed(t *testing.T) {
	// 空/短 seed 会静默退化成一个常量流：必须拒绝，而不是"随便给个默认"。
	for _, s := range []string{"", "abc", "1234567"} {
		if _, err := NewDeterministic([]byte(s)); err == nil {
			t.Fatalf("seed %q 应被拒绝", s)
		}
	}
}

func TestScoreCountsCorrectWrongAndToolFailuresSeparately(t *testing.T) {
	ground := []Sample{
		{File: "0001.png", Answer: "ABCDEF"},
		{File: "0002.png", Answer: "GHIJKM"},
		{File: "0003.png", Answer: "NOPQRS"},
		{File: "0004.png", Answer: "234567"},
	}
	pred := []Prediction{
		{File: "0001.png", Text: "ABCDEF", MS: 10},             // 对
		{File: "0002.png", Text: "GHIJKN", MS: 20},             // 错 1 字符（等长）
		{File: "0003.png", Text: "", MS: 5000, Err: "timeout"}, // 工具失败
	}
	m, err := Score(ground, pred, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 4 || m.Predicted != 3 {
		t.Fatalf("Total/Predicted = %d/%d", m.Total, m.Predicted)
	}
	if m.Exact != 1 || m.Mismatched != 1 || m.ToolFailures != 1 {
		t.Fatalf("Exact/Mismatched/ToolFailures = %d/%d/%d", m.Exact, m.Mismatched, m.ToolFailures)
	}
	if m.Missing != 1 {
		t.Fatalf("缺测应为 1（0004 无预测），got %d", m.Missing)
	}
	// 两个分母同时给出，且数值可手算复核
	if m.StrictAccuracy != 0.25 {
		t.Fatalf("StrictAccuracy = %v，期望 1/4", m.StrictAccuracy)
	}
	if m.ScoredAccuracy != 0.5 {
		t.Fatalf("ScoredAccuracy = %v，期望 1/2（剔除工具失败）", m.ScoredAccuracy)
	}
	// 字符级：参与比较的是 0001(6/6) 与 0002(5/6) ⇒ 11/12
	if m.CharAccuracy < 0.916 || m.CharAccuracy > 0.917 {
		t.Fatalf("CharAccuracy = %v，期望 ≈0.9167", m.CharAccuracy)
	}
	if m.MSCount != 2 || m.MSSum != 30 || m.MSMin != 10 || m.MSMax != 20 || m.MSMean != 15 || m.MSMedian != 15 {
		t.Fatalf("耗时汇总不符：%+v", m)
	}
}

func TestScoreUnknownFilesAndDuplicates(t *testing.T) {
	ground := []Sample{{File: "a.png", Answer: "AAA"}}
	m, err := Score(ground, []Prediction{{File: "zzz.png", Text: "AAA"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Unknown != 1 || m.Predicted != 0 || m.ScoredAccuracy != 0 || m.StrictAccuracy != 0 {
		t.Fatalf("未知文件不得进入任何分母：%+v", m)
	}
	if m.Missing != 1 {
		t.Fatalf("Missing = %d", m.Missing)
	}
	if _, err := Score(ground, []Prediction{{File: "a.png", Text: "AAA"}, {File: "a.png", Text: "AAA"}}, nil); err == nil {
		t.Fatal("重复预测必须报错：静默丢样本会让样本量变小而报告看起来正常")
	}
	if _, err := Score([]Sample{{File: "a.png", Answer: "A"}, {File: "a.png", Answer: "B"}}, nil, nil); err == nil {
		t.Fatal("ground truth 重复文件名必须报错")
	}
}

func TestScoreAppliesNormalizer(t *testing.T) {
	ground := []Sample{{File: "a.png", Answer: "ABC123"}}
	pred := []Prediction{{File: "a.png", Text: " abc123 "}}
	if m, _ := Score(ground, pred, nil); m.Exact != 0 {
		t.Fatal("不归一化时大小写与空白应算不一致")
	}
	// 故意用一个"宽容"的注入实现，证明评分器只按调用方给的口径计数。
	// （真实题型用的是 generator 的 NormalizeAnswer；cmd/evalocr 里有断言证明两者同源。）
	norm := func(s string) (string, bool) {
		out := []byte{}
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			if c != ' ' {
				out = append(out, c)
			}
		}
		return string(out), true
	}
	if m, _ := Score(ground, pred, norm); m.Exact != 1 {
		t.Fatalf("归一化后应判为一致：%+v", m)
	}
	// 注入实现判"畸形"时：既不算 Exact，也不算 Mismatched，而是单独的 Malformed，
	// 且**留在分母里**（剔除它会虚高通过率）。
	// 长度≠6 就畸形——这样 ground truth（6 位）仍然合法，只有带空白的预测被判畸形，
	// 这正是"评分器拒绝为坏样本集静默兜底"的边界。
	reject := func(s string) (string, bool) {
		if len(s) != 6 {
			return "", false
		}
		return s, true
	}
	m, err := Score(ground, pred, reject)
	if err != nil {
		t.Fatal(err)
	}
	if m.Exact != 0 || m.Mismatched != 0 || m.Malformed != 1 {
		t.Fatalf("畸形应单独计数：%+v", m)
	}
	if m.Predicted != 1 || m.ScoredAccuracy != 0 {
		t.Fatalf("畸形不该被剔出分母：%+v", m)
	}
	// 字符级正确率只统计"合法且等长"的输出，畸形串不能靠长度巧合蹭分。
	if m.CharAccuracy != 0 {
		t.Fatalf("畸形输出不应参与字符级统计：%v", m.CharAccuracy)
	}
}

func TestScoreEmptyInputsAreZeroNotOne(t *testing.T) {
	m, err := Score(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 0 || m.StrictAccuracy != 0 || m.ScoredAccuracy != 0 || m.CharAccuracy != 0 || m.MSCount != 0 {
		t.Fatalf("空输入必须全零，不能出现 1.0 或 NaN：%+v", m)
	}
}
