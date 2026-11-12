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
	AnswerTypeText       = "text"
	AnswerTypeInteger    = "integer"
	AnswerTypeCells      = "cells"
	AnswerTypePairs      = "pairs"
	AnswerTypeAngle      = "angle"
	AnswerTypePoint      = "point"
	AnswerTypeTiming     = "timing"
	AnswerTypeMasks      = "masks"
	AnswerTypePath       = "path"
	AnswerTypeSelect     = "select"
	AnswerTypeOrder      = "order"
	AnswerTypeSequence   = "sequence"
	AnswerTypePlacements = "placements"
)

// VersionPrefix 是新增离散编码的固定版本前缀。新题型只使用 v1；不兼容的格式变更必须换新
// answer_type 或新 generator version，不能悄悄让旧 verifier 被新解释读取（DESIGN §8.4）。
const VersionPrefix = "v1:"

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

// --- 2026-11-12 新增的离散交互编码（ADR-030）---
//
// 这些编码只服务新题型；共同规则是：固定版本前缀 "v1:"、固定字段顺序/分隔符、
// 只接受 ASCII、无内部空白、无浮点、长度与范围先验证再使用。任何一条不满足都返回
// ok=false（畸形），由 API 按请求形态错误处理，不消耗唯一作答机会。

func cutVersionPrefix(input string) (string, bool) {
	s := trimASCII(input)
	if len(s) < len(VersionPrefix) {
		return "", false
	}
	// 版本字母大小写不敏感（规范输出永远是 v1:），但分隔符与数字必须逐字节匹配；
	// 这样 "V1:" 与 "v1:" 是同一版本，而 "v2:" / "v1 :" 仍然是不同/非法版本。
	if (s[0] != 'v' && s[0] != 'V') || s[1] != '1' || s[2] != ':' {
		return "", false
	}
	return s[len(VersionPrefix):], true
}

func asciiHexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// NormalizeMasks 把"每个格子一个 4-bit edge mask"的答案规范成 v1:<hex...>。
// 仅接受恰好 cells 个 ASCII 十六进制字符；大写折成小写（同一数值的另一种写法）。
// 它不检查连通性/边界——那是题型 verifier 的语义，不是编码层的事。
func NormalizeMasks(input string, cells int) (string, bool) {
	if cells < 1 || cells > 64 {
		return "", false
	}
	body, ok := cutVersionPrefix(input)
	if !ok || len(body) != cells {
		return "", false
	}
	out := make([]byte, 0, len(VersionPrefix)+cells)
	out = append(out, VersionPrefix...)
	for i := 0; i < len(body); i++ {
		v, ok := asciiHexNibble(body[i])
		if !ok {
			return "", false
		}
		const hex = "0123456789abcdef"
		out = append(out, hex[v])
	}
	return string(out), true
}

// NormalizePath 把格子索引序列规范成 v1:NN.NN...（每段固定两位十进制、原顺序保留）。
// 不检查相邻性/重复语义之外的题型规则；重复索引直接算畸形（序列题不允许重复格）。
func NormalizePath(input string, cells, minLen, maxLen int) (string, bool) {
	if cells < 1 || cells > 100 || minLen < 1 || maxLen < minLen || maxLen > 99 {
		return "", false
	}
	body, ok := cutVersionPrefix(input)
	if !ok || body == "" {
		return "", false
	}
	parts := strings.Split(body, ".")
	if len(parts) < minLen || len(parts) > maxLen {
		return "", false
	}
	seen := make([]bool, cells)
	out := make([]string, len(parts))
	for i, part := range parts {
		if len(part) != 2 || part[0] < '0' || part[0] > '9' || part[1] < '0' || part[1] > '9' {
			return "", false
		}
		n := int(part[0]-'0')*10 + int(part[1]-'0')
		if n >= cells || seen[n] {
			return "", false
		}
		seen[n] = true
		out[i] = part
	}
	return VersionPrefix + strings.Join(out, "."), true
}

// NormalizeSelect 把"36 选 1"这类单选规范成 v1:<index>（无前导零）。
func NormalizeSelect(input string, cells int) (string, bool) {
	if cells < 1 || cells > 1000 {
		return "", false
	}
	body, ok := cutVersionPrefix(input)
	if !ok || body == "" || len(body) > 4 {
		return "", false
	}
	if len(body) > 1 && body[0] == '0' {
		return "", false
	}
	n := 0
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return "", false
		}
		n = n*10 + int(body[i]-'0')
	}
	if n >= cells {
		return "", false
	}
	return VersionPrefix + strconv.Itoa(n), true
}

// NormalizeOrder 把 n 张卡的排列规范成 v1:A.B.C...（字母 ID、原顺序保留、不允许重复）。
func NormalizeOrder(input string, n int) (string, bool) {
	if n < 1 || n > 26 {
		return "", false
	}
	body, ok := cutVersionPrefix(input)
	if !ok || body == "" {
		return "", false
	}
	parts := strings.Split(body, ".")
	if len(parts) != n {
		return "", false
	}
	seen := [26]bool{}
	out := make([]string, n)
	for i, part := range parts {
		if len(part) != 1 {
			return "", false
		}
		c := part[0]
		if c >= 'a' && c <= 'z' {
			c = c - 'a' + 'A'
		}
		if c < 'A' || c > 'Z' {
			return "", false
		}
		idx := int(c - 'A')
		if seen[idx] {
			return "", false
		}
		seen[idx] = true
		out[i] = string(c)
	}
	return VersionPrefix + strings.Join(out, "."), true
}

// NormalizeSequence 把 n 个 0..maxVal 的离散选择规范成 v1:a.b.c（每段一位十进制）。
// 用于"三轮各选一格/一个容器"这类顺序敏感的答案；重复值合法（每轮独立）。
func NormalizeSequence(input string, n, maxVal int) (string, bool) {
	if n < 1 || n > 9 || maxVal < 0 || maxVal > 9 {
		return "", false
	}
	body, ok := cutVersionPrefix(input)
	if !ok || body == "" {
		return "", false
	}
	parts := strings.Split(body, ".")
	if len(parts) != n {
		return "", false
	}
	out := make([]string, n)
	for i, part := range parts {
		if len(part) != 1 || part[0] < '0' || part[0] > '9' {
			return "", false
		}
		if int(part[0]-'0') > maxVal {
			return "", false
		}
		out[i] = part
	}
	return VersionPrefix + strings.Join(out, "."), true
}

// NormalizePlacements 把 slot→(piece,rotation) 映射规范成 v1:id:rot,id:rot,...。
// 字段顺序是 slot 序号（0-based 隐式），id 必须是 0..pieces-1 且不重复，rotation 是 0..3。
func NormalizePlacements(input string, slots, pieces int) (string, bool) {
	if slots < 1 || slots > 9 || pieces < 1 || pieces > 9 || slots > pieces {
		return "", false
	}
	body, ok := cutVersionPrefix(input)
	if !ok || body == "" {
		return "", false
	}
	parts := strings.Split(body, ",")
	if len(parts) != slots {
		return "", false
	}
	seen := make([]bool, pieces)
	out := make([]string, len(parts))
	for i, part := range parts {
		fields := strings.Split(part, ":")
		if len(fields) != 2 || len(fields[0]) != 1 || len(fields[1]) != 1 {
			return "", false
		}
		if fields[0][0] < '0' || fields[0][0] > '9' || fields[1][0] < '0' || fields[1][0] > '3' {
			return "", false
		}
		id := int(fields[0][0] - '0')
		if id >= pieces || seen[id] {
			return "", false
		}
		seen[id] = true
		out[i] = fields[0] + ":" + fields[1]
	}
	return VersionPrefix + strings.Join(out, ","), true
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

// KnownAnswerType 只覆盖已定义契约的类型；path/order/polygon 是 ADR-025 的保留项，
// 2026-11-12 起 path/order 以及新增的 masks/select/sequence/placements 已有自己的
// 严格编码（见上方 Normalize*）；polygon 仍未实现，不在这里列成可用。
func KnownAnswerType(answerType string) bool {
	switch answerType {
	case AnswerTypeText, AnswerTypeInteger, AnswerTypeCells, AnswerTypePairs,
		AnswerTypeAngle, AnswerTypePoint, AnswerTypeTiming,
		AnswerTypeMasks, AnswerTypePath, AnswerTypeSelect, AnswerTypeOrder,
		AnswerTypeSequence, AnswerTypePlacements:
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
