package generator

// arithmetic-baseline-v1：整数四则运算题。
//
// **定位必须说清楚**：这个题型是用来验证"生成器接口 + 题池 + 单次作答生命周期"的工程基线，
// 不是生产级抗自动解题题型。题面是标准算术表达式，任何脚本都能直接求值，
// 因此这里**不**声称"答案不可从题面推导"；本层保证的只有三件事：
// 响应里没有额外的答案字段、没有私有 verifier、没有内部随机数。
// 也不得用"随机实例很多"论证安全性（ADR-002：M1 只影响同题复用，不影响求解难度）。

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
)

const (
	// ArithID 是含版本的稳定标识；sites.generator_id 存的就是它。
	ArithID      = "arithmetic-baseline-v1"
	arithVersion = "v1"

	// 数值边界：表达式里出现的每个中间值与最终答案都必须在 ±maxAnswerValue 内，
	// 且乘积在求值前就做溢出检查。有界是为了让判分可判定、题面长度可控。
	maxAnswerValue = 1_000_000_000

	maxTaskLen     = 64
	maxOperand     = 99
	maxDivisorCand = 9
	minDivisorCand = 2

	// genRetries 是"重抽直到满足边界"的**有界**次数。无界重试在小答案空间里
	// 会变成隐蔽忙循环，并让单次生成耗时不可预期。
	genRetries = 128

	// 提交答案的形态上限：符号 + 最多 12 位数字。
	maxAnswerDigits = 12
)

// ErrGenerateExhausted 是"重试次数用完仍不满足边界"。它是可重试的生成失败：
// 池 worker 按退避处理，绝不把半成品写进库。
var ErrGenerateExhausted = errors.New("generator: 生成重试耗尽")

// ArithPublic 是唯一的公开题面类型：**字段即白名单**。
// 答案、内部随机数、操作数生成顺序都不在这个结构体里——不是靠 json:"-" 抑制，是根本没有。
type ArithPublic struct {
	Task         string `json:"task"`           // 要求值的表达式，如 "12 + 7"、"(9 - 3) × 5"
	AnswerType   string `json:"answer_type"`    // "integer"
	MaxAnswerLen int    `json:"max_answer_len"` // 提交长度上限（含符号）
}

func (ArithPublic) PublicKind() string { return "arithmetic" }

// ArithmeticBaseline 实现 Generator。无可变状态，可并发调用。
type ArithmeticBaseline struct {
	// macKey 是按 purpose 派生的 K_q。verifier 只存它的输出，因此答案明文不必落库（红线 5）。
	macKey []byte
}

var _ Generator = ArithmeticBaseline{}

// NewArithmeticBaseline 构造生成器；macKey 必须是派生密钥且 ≥32 字节。
func NewArithmeticBaseline(macKey []byte) (ArithmeticBaseline, error) {
	if len(macKey) < 32 {
		return ArithmeticBaseline{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	return ArithmeticBaseline{macKey: macKey}, nil
}

func (ArithmeticBaseline) ID() string      { return ArithID }
func (ArithmeticBaseline) Version() string { return arithVersion }
func (ArithmeticBaseline) Media() string   { return "text" }

// difficultySpec 把 1..5 档映射成有界参数范围。范围是工程选择，不构成任何安全性论断。
type difficultySpec struct {
	minOperand, maxOperand int
	outerMul               bool // d4/d5 allow multiplying the two bracketed results
	allowMul, allowDiv     bool
}

var arithDifficulty = map[int]difficultySpec{
	1: {minOperand: 2, maxOperand: maxOperand},
	2: {minOperand: 2, maxOperand: maxOperand, allowMul: true},
	3: {minOperand: 2, maxOperand: maxOperand, allowMul: true, allowDiv: true},
	4: {minOperand: 2, maxOperand: maxOperand, allowMul: true, outerMul: true},
	5: {minOperand: 2, maxOperand: maxOperand, allowMul: true, allowDiv: true, outerMul: true},
}

// Generate 抽一道题。entropy 必须是调用方给的 CSPRNG 流（生产路径是 crypto/rand.Reader）。
//
// 四个操作数渲染成 "(a op b) op (c op d)"：结合顺序由括号确定——
// 优先级歧义会让"正确答案"变成两个。除法只在能整除时使用（除数取自当前值的真实因子），
// 因此不存在除零与余数含糊。
func (g ArithmeticBaseline) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	spec := arithDifficulty[p.Difficulty]

	for range genRetries {
		if err := ctx.Err(); err != nil {
			return Question{}, err
		}
		nonce := make([]byte, verifierNonceSz)
		if _, err := io.ReadFull(entropy, nonce); err != nil {
			return Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
		}
		acc, task, ok := buildExpr(entropy, spec)
		if !ok {
			continue
		}
		answer := strconv.FormatInt(acc, 10)
		return Question{
			Payload:  ArithPublic{Task: task, AnswerType: "integer", MaxAnswerLen: maxAnswerDigits + 2},
			Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, answer)),
		}, nil
	}
	return Question{}, ErrGenerateExhausted
}

// Verify 判分：对提交值重算摘要再常数时间比较（可离散化题型的允许做法，ADR-003）。
// 需要容差的题型（角度、区域）自行定义判定方式，不得为此把答案放进 Payload。
func (g ArithmeticBaseline) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

// NormalizeAnswer 定义"什么算一个合法答案串"：
//   - 裁掉首尾 ASCII 空白；内部空白一律畸形；
//   - 允许单个前导 '+' 或 '-'；
//   - 只接受 ASCII 十进制数字（小数点、千分位、下划线、全角/阿拉伯数字一律畸形）；
//   - 前导零按规范化处理（"007" → "7"，"-000" → "0"）：格式差异不该消耗掉唯一一次作答机会；
//   - 长度上限 maxAnswerDigits，超出即畸形（因此"整数溢出"走畸形分支，不会被当成答错，
//     也不会因为解析失败而 panic 或静默回绕）。
//
// ok=false 表示**畸形**而不是"答错"：调用方必须按请求形态错误处理，不做终态化。
func (ArithmeticBaseline) NormalizeAnswer(input string) (string, bool) {
	s := trimASCII(input)
	if s == "" || len(s) > maxAnswerDigits+2 {
		return "", false
	}
	neg := false
	body := s
	switch body[0] {
	case '+':
		body = body[1:]
	case '-':
		neg = true
		body = body[1:]
	}
	if body == "" || len(body) > maxAnswerDigits {
		return "", false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return "", false
		}
	}
	trimmed := strings.TrimLeft(body, "0")
	if trimmed == "" {
		return "0", true
	}
	if neg {
		return "-" + trimmed, true
	}
	return trimmed, true
}

// SanitizePublic 是题面出网的唯一通道：反序列化进白名单结构体，再逐键比对原始键集合，
// **任何**非白名单键都让整次调用失败。选"报错"而不是"静默丢弃"是因为：库里出现额外键
// 意味着写入侧已经出了事故，把它悄悄裁掉继续下发会掩盖事故现场（而这张题的 challenge 仍应作废）。
func (ArithmeticBaseline) SanitizePublic(raw []byte) (map[string]any, error) {
	var p ArithPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Task == "" || len(p.Task) > maxTaskLen || p.AnswerType != "integer" || p.MaxAnswerLen <= 0 {
		return nil, errors.New("generator: 题面形状不合法")
	}
	for k := range generic {
		switch k {
		case "task", "answer_type", "max_answer_len":
		default:
			return nil, fmt.Errorf("generator: 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind":           p.PublicKind(),
		"task":           p.Task,
		"answer_type":    p.AnswerType,
		"max_answer_len": p.MaxAnswerLen,
	}, nil
}

// --- 表达式构造 ---

// buildExpr 生成表达式文本与答案。任何边界不满足（溢出、不能整除、长度超限）都返回 ok=false，
// 由 Generate 的**有界**重试循环处理——这里不放无界循环，也不靠"重画某个数"补救，
// 因为补救会改变算子与答案的联合分布，而我需要如实报告那个分布。
func buildExpr(entropy io.Reader, spec difficultySpec) (answer int64, task string, ok bool) {
	left, leftTask, ok := buildArithPair(entropy, spec)
	if !ok {
		return 0, "", false
	}
	right, rightTask, ok := buildArithPair(entropy, spec)
	if !ok {
		return 0, "", false
	}
	op, err := drawOp(entropy, difficultySpec{allowMul: spec.outerMul})
	if err != nil {
		return 0, "", false
	}
	answer, ok = boundedArith(left, right, op)
	if !ok {
		return 0, "", false
	}
	task = "(" + leftTask + ") " + opSymbol(op) + " (" + rightTask + ")"
	return answer, task, len(task) <= maxTaskLen
}

func buildArithPair(entropy io.Reader, spec difficultySpec) (int64, string, bool) {
	a, err := drawInt(entropy, int64(spec.minOperand), int64(spec.maxOperand))
	if err != nil {
		return 0, "", false
	}
	op, err := drawOp(entropy, spec)
	if err != nil {
		return 0, "", false
	}
	lo, hi := int64(spec.minOperand), int64(spec.maxOperand)
	if op == '/' {
		lo, hi = minDivisorCand, maxDivisorCand
	}
	b, err := drawInt(entropy, lo, hi)
	if err != nil {
		return 0, "", false
	}
	value, ok := boundedArith(a, b, op)
	if !ok {
		return 0, "", false
	}
	return value, fmt.Sprintf("%d %s %d", a, opSymbol(op), b), true
}

func boundedArith(a, b int64, op byte) (int64, bool) {
	if a < -maxAnswerValue || a > maxAnswerValue || b < -maxAnswerValue || b > maxAnswerValue {
		return 0, false
	}
	var value int64
	switch op {
	case '+':
		value = a + b
	case '-':
		value = a - b
	case 'x':
		// Bracketed results can be negative; guard both signs before multiplication.
		aa, bb := a, b
		if aa < 0 {
			aa = -aa
		}
		if bb < 0 {
			bb = -bb
		}
		if bb != 0 && aa > maxAnswerValue/bb {
			return 0, false
		}
		value = a * b
	case '/':
		if b < minDivisorCand || b > maxDivisorCand || a%b != 0 {
			return 0, false
		}
		value = a / b
	default:
		return 0, false
	}
	return value, value >= -maxAnswerValue && value <= maxAnswerValue
}

func drawOp(r io.Reader, spec difficultySpec) (byte, error) {
	ops := []byte{'+', '-'}
	if spec.allowMul {
		ops = append(ops, 'x')
	}
	if spec.allowDiv {
		ops = append(ops, '/')
	}
	i, err := drawInt(r, 0, int64(len(ops)-1))
	if err != nil {
		return 0, err
	}
	return ops[i], nil
}

func opSymbol(op byte) string {
	switch op {
	case 'x':
		return "×"
	case '/':
		return "÷"
	default:
		return string(op)
	}
}

// drawInt 在 [lo,hi] 上均匀取值。用标准库 crypto/rand.Int：它自己处理拒绝采样，
// 因此这里不需要手写"取模偏置"的逻辑（手写容易写出不均匀分布）。
// entropy 是测试可注入的流，生产传 crypto/rand.Reader。
func drawInt(entropy io.Reader, lo, hi int64) (int64, error) {
	if hi < lo {
		return 0, ErrBadParams
	}
	n, err := crand.Int(entropy, big.NewInt(hi-lo+1))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	return lo + n.Int64(), nil
}
