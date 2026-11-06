package generator

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 通用答案编码（ADR-025）。现有题型继续使用自己的 NormalizeAnswer；
// 这里的函数只定义交互式底座新增的三种原语，避免各题型各写一份且边界漂移。
const (
	AnswerTypeText    = "text"
	AnswerTypeInteger = "integer"
	AnswerTypeCells   = "cells"
	AnswerTypePairs   = "pairs"
	AnswerTypeAngle   = "angle"
	AnswerTypePoint   = "point"
	AnswerTypeTiming  = "timing"
)

const (
	AngleMin  = 0
	AngleMax  = 3599 // 0.1° 量化；360.0° 用 0 表达
	PointMin  = 0
	PointMax  = 999
	TimingMin = 0
	TimingMax = 60000
)

// NormalizeAngle 把角度规范成 0..3599 的十进制整数串（ADR-025）。
// 只接受 ASCII 数字；小数、符号、内部空白、全角数字都算畸形——
// 畸形不消耗作答机会，但也不能被"宽容解析"成另一个角度。
func NormalizeAngle(input string) (string, bool) {
	s := trimASCII(input)
	if s == "" || len(s) > 4 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < AngleMin || n > AngleMax {
		return "", false
	}
	return strconv.Itoa(n), true
}

// NormalizePoint 把点选规范成 "x,y"（各 0..999，按图宽/高的千分位）。
// 允许逗号两侧的 ASCII 空格；字段内部空白、符号、小数、第三个字段都算畸形。
func NormalizePoint(input string) (string, bool) {
	s := trimASCII(input)
	if s == "" || len(s) > 11 { // "999 , 999" 的最坏长度
		return "", false
	}
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return "", false
	}
	vals := [2]int{}
	for i, part := range parts {
		field := trimASCII(part)
		if field == "" || len(field) > 3 {
			return "", false
		}
		for j := 0; j < len(field); j++ {
			if field[j] < '0' || field[j] > '9' {
				return "", false
			}
		}
		n, err := strconv.Atoi(field)
		if err != nil || n < PointMin || n > PointMax {
			return "", false
		}
		vals[i] = n
	}
	return strconv.Itoa(vals[0]) + "," + strconv.Itoa(vals[1]), true
}

// NormalizeTiming 把时序答案规范成 0..60000 的毫秒整数串。
// 循环动画的"相位"语义由题型在 verifier 内解释；这里只固定编码边界。
func NormalizeTiming(input string) (string, bool) {
	s := trimASCII(input)
	if s == "" || len(s) > 5 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < TimingMin || n > TimingMax {
		return "", false
	}
	return strconv.Itoa(n), true
}

// NormalizeCellSet 是 cells 答案的通用规范化：1-based 序号、逗号分隔、排序去重。
// 只把"同一集合的另一种写法"折叠掉，不扩大接受域；空/越界/非十进制是畸形。
func NormalizeCellSet(input string, maxCells, maxLen int) (string, bool) {
	if maxCells < 1 || maxLen < 1 {
		return "", false
	}
	s := trimASCII(input)
	if s == "" || len(s) > maxLen {
		return "", false
	}
	parts := strings.Split(s, ",")
	if len(parts) == 0 || len(parts) > maxCells {
		return "", false
	}
	seen := make([]bool, maxCells+1)
	nums := make([]int, 0, len(parts))
	for _, raw := range parts {
		p := trimASCII(raw)
		if p == "" || len(p) > 2 {
			return "", false
		}
		n := 0
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return "", false
			}
			n = n*10 + int(p[i]-'0')
		}
		if n < 1 || n > maxCells {
			return "", false
		}
		if !seen[n] {
			seen[n] = true
			nums = append(nums, n)
		}
	}
	if len(nums) == 0 {
		return "", false
	}
	sort.Ints(nums)
	out := make([]string, len(nums))
	for i, n := range nums {
		out[i] = strconv.Itoa(n)
	}
	return strings.Join(out, ","), true
}

// NormalizePrimitiveAnswer 按通用原语类型分派；未知类型返回 false。
// text/integer/cells/pairs 仍由各自生成器实现（它们有题型专属规则），不在这里抢解释权。
func NormalizePrimitiveAnswer(answerType, input string) (string, bool) {
	switch answerType {
	case AnswerTypeAngle:
		return NormalizeAngle(input)
	case AnswerTypePoint:
		return NormalizePoint(input)
	case AnswerTypeTiming:
		return NormalizeTiming(input)
	default:
		return "", false
	}
}

// FormatAngle / FormatPoint / FormatTiming 供生成器在构造私有 verifier 前量化候选值。
// 它们与 Normalize 使用同一组边界，避免"生成时接受、判分时拒绝"的静默不一致。
func FormatAngle(decidegrees int) (string, bool) {
	if decidegrees < AngleMin || decidegrees > AngleMax {
		return "", false
	}
	return strconv.Itoa(decidegrees), true
}

func FormatPoint(x, y int) (string, bool) {
	if x < PointMin || x > PointMax || y < PointMin || y > PointMax {
		return "", false
	}
	return strconv.Itoa(x) + "," + strconv.Itoa(y), true
}

func FormatTiming(ms int) (string, bool) {
	if ms < TimingMin || ms > TimingMax {
		return "", false
	}
	return strconv.Itoa(ms), true
}

// KnownAnswerType 只覆盖已定义契约的类型；path/order/polygon 是 ADR-025 的保留项，未实现，
// 因此这里不把它们列成"已知"，避免调用方误以为可以下发。
func KnownAnswerType(answerType string) bool {
	switch answerType {
	case AnswerTypeText, AnswerTypeInteger, AnswerTypeCells, AnswerTypePairs,
		AnswerTypeAngle, AnswerTypePoint, AnswerTypeTiming:
		return true
	default:
		return false
	}
}

// ValidateAnswerType 是给测试/配置校验用的显式错误版本。
func ValidateAnswerType(answerType string) error {
	if KnownAnswerType(answerType) {
		return nil
	}
	return fmt.Errorf("%w: 未定义的 answer_type %q", ErrBadParams, answerType)
}
