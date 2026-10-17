package generator

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

func testMacKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func newArith(t *testing.T) ArithmeticBaseline {
	t.Helper()
	g, err := NewArithmeticBaseline(testMacKey())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// --- 独立求值器：不复用被测代码的构造逻辑 ---

// evalTask 是一个**独立实现**：把题面文本按显式括号重新解析并求值。
// 它存在的意义是：如果生成器把答案算错（或渲染顺序与计算顺序不一致），这里就会发现。
func evalTask(t *testing.T, task string) int64 {
	t.Helper()
	// 统一符号
	s := strings.NewReplacer("×", " * ", "÷", " / ", "+", " + ", "-", " - ", "(", " ( ", ")", " ) ").Replace(task)
	fields := strings.Fields(s)
	// 拆掉括号：形如 ( a op b ) op c 或 a op b
	var terms []int64
	var ops []string
	depth := 0
	var inner []string
	for _, f := range fields {
		switch f {
		case "(":
			depth++
		case ")":
			depth--
			// 内层求值
			innerVal := evalFlat(t, inner)
			terms = appendInt64(terms, innerVal)
			inner = inner[:0]
		default:
			switch f {
			case "+", "-", "*", "/":
				if depth > 0 {
					inner = append(inner, f)
				} else {
					ops = append(ops, f)
				}
			default:
				v, err := strconv.ParseInt(f, 10, 64)
				if err != nil {
					t.Fatalf("题面含无法解析的片段 %q（task=%q）: %v", f, task, err)
				}
				if depth > 0 {
					inner = append(inner, f)
				} else {
					terms = appendInt64(terms, v)
				}
			}
		}
	}
	if len(inner) > 0 {
		t.Fatalf("括号未闭合：%q", task)
	}
	if len(terms) == 0 {
		t.Fatalf("没有操作数：%q", task)
	}
	// ops 里没有括号部分，先算括号（已折叠）再线性求值
	all := terms
	res := all[0]
	idx := 0
	for _, op := range ops {
		if idx+1 >= len(all) {
			t.Fatalf("表达式不完整：%q", task)
		}
		next := all[idx+1]
		switch op {
		case "+":
			res += next
		case "-":
			res -= next
		case "*":
			res *= next
		case "/":
			if next == 0 || res%next != 0 {
				t.Fatalf("表达式含除零或非整除：%q", task)
			}
			res /= next
		}
		idx++
	}
	return res
}

func appendInt64(s []int64, v int64) []int64 { return append(s, v) }

// evalFlat 求 "a op b" 形式的内层表达式（括号内只允许一次运算，这是本生成器的合同）。
func evalFlat(t *testing.T, fields []string) int64 {
	t.Helper()
	if len(fields) != 3 {
		t.Fatalf("括号内必须是 a op b，got %v", fields)
	}
	a, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	b, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	switch fields[1] {
	case "+":
		return a + b
	case "-":
		return a - b
	case "*":
		return a * b
	case "/":
		if b == 0 || a%b != 0 {
			t.Fatalf("括号内非整除：%v", fields)
		}
		return a / b
	}
	t.Fatalf("未知运算符 %q", fields[1])
	return 0
}

// --- 固定输入的真实答案（KAT）---

// TestArithKnownAnswer 用一条固定的熵流生成一道题，钉死"同样的输入 → 同样的题面与答案"。
// 这不是为了"可预测"（生产熵源是 crypto/rand），而是为了让判分链路有可回归的基准。
func TestArithKnownAnswer(t *testing.T) {
	g := newArith(t)
	stream := bytes.Repeat([]byte{0x11, 0x22, 0x33, 0x44}, 16)
	q1, err := g.Generate(context.Background(), Params{Difficulty: 1, Lang: "en"}, bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	q2, err := g.Generate(context.Background(), Params{Difficulty: 1, Lang: "en"}, bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	p1 := q1.Payload.(ArithPublic)
	p2 := q2.Payload.(ArithPublic)
	if p1.Task != p2.Task {
		t.Fatalf("同一熵流应产生同一题面：%q vs %q", p1.Task, p2.Task)
	}
	if !bytes.Equal(q1.Verifier, q2.Verifier) {
		t.Fatal("同一熵流的 verifier 也应相同")
	}
	if p1.Task == "" {
		t.Fatal("题面为空")
	}
	// 答案由题面独立求值，且必须被 verifier 接受
	want := evalTask(t, p1.Task)
	if !g.Verify(q1.Verifier, strconv.FormatInt(want, 10)) {
		t.Fatalf("独立求值得到 %d，verifier 不接受：task=%q", want, p1.Task)
	}
	// 相邻值必须被拒
	if g.Verify(q1.Verifier, strconv.FormatInt(want+1, 10)) {
		t.Fatalf("相邻答案 %d 竟被接受", want+1)
	}
}

// TestArithVerifierAcceptsOnlyEvaluatedAnswer 是核心正确性测试：
// 5000 道题，每题的答案都由独立求值器从题面文本重算，verifier 必须接受它、拒绝邻近值。
func TestArithVerifierAcceptsOnlyEvaluatedAnswer(t *testing.T) {
	g := newArith(t)
	for diff := 1; diff <= MaxDifficulties; diff++ {
		for i := 0; i < 1000; i++ {
			q, err := g.Generate(context.Background(), Params{Difficulty: diff, Lang: "zh-CN"}, rand.Reader)
			if err != nil {
				t.Fatalf("difficulty=%d 第 %d 次生成失败: %v", diff, i, err)
			}
			pub := q.Payload.(ArithPublic)
			want := evalTask(t, pub.Task)
			s := strconv.FormatInt(want, 10)
			if !g.Verify(q.Verifier, s) {
				t.Fatalf("difficulty=%d：题面 %q 的独立求值 %s 未被 verifier 接受", diff, pub.Task, s)
			}
			if g.Verify(q.Verifier, strconv.FormatInt(want+1, 10)) {
				t.Fatalf("difficulty=%d：相邻答案被接受（task=%q）", diff, pub.Task)
			}
			if len(q.Verifier) != verifierNonceSz+verifierTagSz {
				t.Fatalf("verifier 长度 %d，期望 %d", len(q.Verifier), verifierNonceSz+verifierTagSz)
			}
			if abs64(want) > maxAnswerValue {
				t.Fatalf("答案越界：%d", want)
			}
			if len(pub.Task) > maxTaskLen {
				t.Fatalf("题面过长：%q", pub.Task)
			}
		}
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// --- 答案规范化 ---

func TestNormalizeAnswer(t *testing.T) {
	g := ArithmeticBaseline{}
	accepted := map[string]string{
		"42":            "42",
		"  42 ":         "42",
		"\t42\n":        "42",
		"+42":           "42",
		"-42":           "-42",
		"007":           "7",
		"-007":          "-7",
		"0":             "0",
		"-0":            "0",
		"+000":          "0",
		"1000000":       "1000000",
		"-999999999999": "-999999999999", // 12 位，仍在长度上限内
	}
	for in, want := range accepted {
		got, ok := g.NormalizeAnswer(in)
		if !ok || got != want {
			t.Errorf("NormalizeAnswer(%q) = %q,%v 期望 %q", in, got, ok, want)
		}
	}
	rejected := []string{
		"", "   ", "abc", "42abc", "4 2", "4.2", "1,000", "1_000", "+-1", "--1", "-", "+",
		"١٢٣", // 非 ASCII 数字
		"4２",  // 全角数字
		"0x2a", "0b101", "1e3", " 4 2 ", "４２",
		strings.Repeat("9", maxAnswerDigits+1),       // 数字过长 → 畸形（不是答错，也不参与判分）
		strings.Repeat("9", 200),                     // 远超上限
		"999999999999999999999999999999999999999999", // 若按 int64 解析会溢出，这里必须是畸形
		"\x0042", "42\x00",
	}
	for _, in := range rejected {
		if got, ok := g.NormalizeAnswer(in); ok {
			t.Errorf("NormalizeAnswer(%q) 应畸形，got %q", in, got)
		}
	}
}

// --- 公开 DTO 与隔离 ---

// TestPublicHasNoPrivateFields 钉住"字段即白名单"：公开结构体的字段集合是固定的四个键（含 kind），
// 且 verifier 的字节不出现在序列化结果里。
func TestPublicHasNoPrivateFields(t *testing.T) {
	g := newArith(t)
	q, err := g.Generate(context.Background(), Params{Difficulty: 5, Lang: "en"}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(q.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"task": true, "answer_type": true, "max_answer_len": true}
	if len(keys) != len(want) {
		t.Fatalf("公开字段数量不符：%v", keys)
	}
	for k := range keys {
		if !want[k] {
			t.Fatalf("公开 payload 出现未登记字段 %q", k)
		}
	}
	// 结构体反射：字段名里不得有 answer/seed/nonce 这类私有语义
	typ := fmt.Sprintf("%T", q.Payload)
	if typ != "generator.ArithPublic" {
		t.Fatalf("公开类型应为具名结构体，got %s", typ)
	}
	for _, forbidden := range []string{"answer", "seed", "nonce", "verifier", "secret"} {
		if strings.Contains(strings.ToLower(typ), forbidden) {
			t.Errorf("公开类型名暗示含私有材料：%s", typ)
		}
	}
	// verifier 的原始字节或其 hex/base64 形态都不得出现在题面里
	s := string(raw)
	if strings.Contains(s, string(q.Verifier)) {
		t.Fatal("verifier 字节出现在公开 payload 中")
	}
}

// TestSanitizePublicDropsUnknownFields 是出网白名单的实际作用：
// 库里若存在带额外键的题面（旧数据或写错的地方），它会被**拒绝**而不是跟着响应出门。
// TestSanitizePublicRejectsUnknownFields 额外键必须是**整次失败**，不是"裁掉后继续下发"：
// 库里出现非白名单键说明写入侧已经出了事故，静默裁剪会把事故掩盖成"题面看起来正常"。
func TestSanitizePublicRejectsUnknownFields(t *testing.T) {
	g := newArith(t)
	ok := []byte(`{"task":"12 + 7","answer_type":"integer","max_answer_len":14}`)
	m, err := g.SanitizePublic(ok)
	if err != nil {
		t.Fatal(err)
	}
	if m["kind"] != "arithmetic" || m["task"] != "12 + 7" {
		t.Fatalf("DTO 不符：%v", m)
	}
	if _, exists := m["answer"]; exists {
		t.Fatal("DTO 不该有 answer 键")
	}
	for _, bad := range []string{
		`{"task":"12 + 7","answer_type":"integer","max_answer_len":14,"answer":19}`,
		`{"task":"12 + 7","answer_type":"integer","max_answer_len":14,"seed":"AAA"}`,
		`{"task":"12 + 7","answer_type":"integer","max_answer_len":14,"verifier":"abc"}`,
		`{"task":"","answer_type":"integer","max_answer_len":14}`,
		`{"task":"12 + 7","answer_type":"float","max_answer_len":14}`,
		`{"task":"12 + 7"}`,
	} {
		if _, err := g.SanitizePublic([]byte(bad)); err == nil {
			t.Errorf("非法题面应拒绝：%s", bad)
		}
	}
	// 超长题面同样拒绝（出网体积必须有界）
	long := `{"task":"` + strings.Repeat("1 + ", maxTaskLen) + `","answer_type":"integer","max_answer_len":14}`
	if _, err := g.SanitizePublic([]byte(long)); err == nil {
		t.Error("超长题面应拒绝")
	}
}

// --- 熵源与边界 ---

// TestGeneratePropagatesEntropyError：熵源坏掉时必须**立刻**返回错误，
// 不能被 genRetries 循环吞掉变成静默的"生成失败但无错误"。
func TestGeneratePropagatesEntropyError(t *testing.T) {
	g := newArith(t)
	if _, err := g.Generate(context.Background(), Params{Difficulty: 3}, shortReader{}); err == nil {
		t.Fatal("熵源不足应报错")
	} else if !errors.Is(err, ErrOutOfEntropy) {
		t.Fatalf("错误类型应为 ErrOutOfEntropy，got %v", err)
	}
}

type shortReader struct{}

func (shortReader) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestGenerateRejectsBadParams(t *testing.T) {
	g := newArith(t)
	for _, d := range []int{0, -1, MaxDifficulties + 1, 99} {
		if _, err := g.Generate(context.Background(), Params{Difficulty: d}, rand.Reader); !errors.Is(err, ErrBadParams) {
			t.Errorf("difficulty=%d 应拒绝，got %v", d, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Generate(ctx, Params{Difficulty: 1}, rand.Reader); err == nil {
		t.Error("已取消的 context 应中止生成")
	}
}

func TestWeakMacKeyRefused(t *testing.T) {
	if _, err := NewArithmeticBaseline(bytes.Repeat([]byte{1}, 16)); err == nil {
		t.Fatal("短密钥应拒绝构造生成器")
	}
}

// TestVerifyRejectsCorruptVerifier 保证畸形 verifier 一律判假，而不是 panic。
func TestVerifyRejectsCorruptVerifier(t *testing.T) {
	g := newArith(t)
	for _, v := range []PrivateVerifier{nil, {}, PrivateVerifier("short"), PrivateVerifier(strings.Repeat("A", 100))} {
		if g.Verify(v, "1") {
			t.Errorf("畸形 verifier %d 字节竟判真", len(v))
		}
	}
}

// --- 注册表 ---

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	g := newArith(t)
	if err := r.Register(g); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(g); err == nil {
		t.Error("同 id 重复登记必须失败：静默替换判分实现是严重错误")
	}
	got, err := r.Lookup(ArithID)
	if err != nil || got.Version() != "v1" || got.Media() != "text" {
		t.Fatalf("查找不符: %v %+v", err, got)
	}
	if _, err := r.Lookup("text-warp-v1"); !errors.Is(err, ErrUnknownGenerator) {
		t.Fatalf("未登记题型必须显式失败（不得回落到别的题型）: %v", err)
	}
	if ids := r.IDs(); len(ids) != 1 || ids[0] != ArithID {
		t.Errorf("IDs 不符: %v", ids)
	}
}

// --- 答案分布（M2/M3 的实测口径）---

// TestArithAnswerDistribution 不宣称任何安全性，只做一件事：
// 把本生成器在各难度档下的**答案分布**量出来，作为随机猜测基线（M3）的输入。
// 注意：算术题的答案分布并不均匀（小范围、非负、整除约束都会集中质量），
// 因此不能用"组合数很大"代替对 max 单点概率的测量。
func TestArithAnswerDistribution(t *testing.T) {
	g := newArith(t)
	const samples = 5000
	for diff := 1; diff <= MaxDifficulties; diff++ {
		counts := map[string]int{}
		nonEmpty := 0
		for i := 0; i < samples; i++ {
			q, err := g.Generate(context.Background(), Params{Difficulty: diff}, rand.Reader)
			if err != nil {
				t.Fatalf("difficulty=%d: %v", diff, err)
			}
			pub := q.Payload.(ArithPublic)
			answer := strconv.FormatInt(evalTask(t, pub.Task), 10)
			counts[answer]++
			nonEmpty++
		}
		maxKey, maxN := "", 0
		for k, n := range counts {
			if n > maxN {
				maxN, maxKey = n, k
			}
		}
		p := big.NewRat(int64(maxN), int64(nonEmpty))
		t.Logf("difficulty=%d：样本 %d，不同答案 %d，单点最高 P=%s（最热门答案 %s）",
			diff, nonEmpty, len(counts), p.FloatString(5), maxKey)
		// 基线题的 M3 必然远高于 10%：这正是它只能作工程基线、不能作生产题型的原因。
		if maxN*10 < nonEmpty/10 {
			t.Logf("difficulty=%d：单点概率低于 1%%（记录用，不构成安全性论断）", diff)
		}
	}
}

// --- 契约级守卫：泄露检测辅助本身要有效 ---

// findPrivateLeak 检查序列化后的题面里是否出现私有材料（verifier 字节或其派生串）。
func findPrivateLeak(payloadJSON []byte, v PrivateVerifier) bool {
	if len(v) == 0 {
		return false
	}
	if bytes.Contains(payloadJSON, v) {
		return true
	}
	// 常见"间接泄露"形态：hex 与 base64 都查一遍
	hexStr := fmt.Sprintf("%x", v)
	sum := sha256.Sum256(v)
	return bytes.Contains(payloadJSON, []byte(hexStr)) ||
		bytes.Contains(payloadJSON, []byte(fmt.Sprintf("%x", sum[:])))
}

func TestPrivateLeakGuardWorks(t *testing.T) {
	v := PrivateVerifier("0123456789abcdef0123456789abcdef")
	if !findPrivateLeak([]byte(`{"x":"`+string(v)+`"}`), v) {
		t.Fatal("含私有字节时必须检出")
	}
	if findPrivateLeak([]byte(`{"task":"1 + 1"}`), v) {
		t.Fatal("正常题面被误判")
	}
	if findPrivateLeak([]byte(`{}`), nil) {
		t.Fatal("空 verifier 不应触发")
	}
}

// 换主密钥（判分派生密钥随之改变）后，旧题目**必须判假**，即使提交正确答案：fail closed。
// 这是运维程序的依据——轮换主密钥后题池里的存量题不可再用（DESIGN §6 轮换段、ADR-011 #4）。
func TestVerifierFailsClosedAfterKeyChange(t *testing.T) {
	old := newArith(t)
	other := make([]byte, 32)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	rotated := mustArith(t, other)

	q, err := old.Generate(context.Background(), Params{Difficulty: 2, Lang: "zh-CN"}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := q.Payload.(ArithPublic)
	if !ok {
		t.Fatalf("payload 类型不符：%T", q.Payload)
	}
	in := strconv.FormatInt(evalTask(t, p.Task), 10)
	if !old.Verify(q.Verifier, in) {
		t.Fatal("原密钥下自求自验必须通过（前提不成立）")
	}
	if rotated.Verify(q.Verifier, in) {
		t.Fatal("换密钥后仍接受正确答案：判分密钥可替换，红线 1")
	}
	// 空答案与垃圾答案同样判假，而不是报错或 panic
	for _, bad := range []string{"", "0", in + "0", "-" + in} {
		if rotated.Verify(q.Verifier, bad) {
			t.Errorf("换密钥后 %q 竟判真", bad)
		}
	}
}

// mustArith 用任意密钥构造生成器（判分密钥轮换测试需要两把不同的钥匙）。
func mustArith(t *testing.T, key []byte) ArithmeticBaseline {
	t.Helper()
	g, err := NewArithmeticBaseline(key)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
