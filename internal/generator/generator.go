// Package generator 定义可插拔题型生成器的契约与实现（DESIGN §8.1、§8.4，ADR-002/003/011）。
//
// 隔离措施是**类型层面**的，不靠 json:"-":
//   - 公开题面必须是实现 PublicPayload 的具名结构体，生成器不会把私有字段放进来；
//   - 私有判分数据是独立的 PrivateVerifier（一段字节），落进 pool.verifier_blob；
//   - 公开 API 只能通过 SanitizePublic 拿到"白名单字段"的 map，任何多出来的键都会让这次出网失败。
//
// 阶段 3A 只有 arithmetic-baseline-v1：它是**工程基线**（用来验证生成器接口、题池与提交生命周期），
// 不是生产抗自动解题题型。公开算术表达式可以被程序直接求值，因此这里绝不声称"答案不可从题面推导"。
package generator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
)

// 协议边界。
const MaxDifficulties = 5

// 媒体类别。题池 worker 用它决定水位与字节预算这一组资源参数（DESIGN §8.5），
// API 用它决定 DTO 是否必须带图片——所以它是**契约值**，不是描述文字。
const (
	MediaText  = "text"
	MediaImage = "image"
)

var (
	ErrUnknownGenerator = errors.New("generator: 未注册的题型")
	ErrBadParams        = errors.New("generator: 生成参数不合法")
	ErrOutOfEntropy     = errors.New("generator: 熵源不足")
)

// PublicPayload 是可下发的题面。实现者必须是**只含公开字段**的具名结构体。
type PublicPayload interface {
	// PublicKind 是出网 DTO 的题型标识，写入响应的 kind 字段。
	PublicKind() string
}

// PrivateVerifier 是服务端私有判分数据：永不下发、永不写日志。
// 形态由题型自定（ADR-003）：可摘要、可参数、可私有结构；这里只规定"它是字节且不出门"。
type PrivateVerifier []byte

// Question 是一次生成的结果：公开部分与私有部分严格分离。
type Question struct {
	Payload  PublicPayload   // 序列化后作为 pool.payload_json 下发
	Verifier PrivateVerifier // 序列化后作为 pool.verifier_blob，永不下发
	Asset    []byte          // 图片字节，可为 nil（阶段 3B 才用）
	MimeType string          // Asset 非空时必填
}

// Params 是一次生成的输入。
//
// 3A 没有按 lang 或按站点分桶：arithmetic-baseline-v1 与语言无关，
// 题目内容也只由 (generator, version, difficulty) 决定，所以这里只有难度档。
type Params struct {
	Difficulty int    // 1..MaxDifficulties
	Lang       string // "zh-CN" | "en"（与语言无关的题型必须忽略它）
}

// Generator 是单个题型的生成与判分器。
//
// 实现必须满足：
//   - 决定答案的随机量必须直接来自 entropy（CSPRNG）；不得用 math/rand 抽答案参数
//     （图形抖动可以在阶段 3B 用 entropy 播种的独立 math/rand 实例，且不得共享）。
//   - 同一实例被并发调用：不得持有可变状态。
//   - 答案、可推导答案的种子、判分秘密材料只能待在 Verifier 里。
//   - 安全性按 DESIGN §8.2 的 M1–M4 分别论证；"随机实例多"、"用了 CSPRNG"都不算论证。
type Generator interface {
	ID() string      // 含版本的稳定标识，例如 "arithmetic-baseline-v1"
	Version() string // 协议版本 "v1"：判分兼容性的判定依据
	Media() string   // "text" | "image"

	Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error)

	// Verify 判分。input 必须是 NormalizeAnswer 之后的规范形式；实现内部用常数时间比较。
	// 需要容差的题型（角度、区域）自行定义判定方式，但不得为此把答案放进 Payload。
	Verify(v PrivateVerifier, input string) bool

	// NormalizeAnswer 把用户提交串规范成可判分形式。
	// ok=false 表示**畸形**（不是"答错"）：调用方必须按请求形态错误处理，
	// 不能消耗掉这张 challenge 唯一的作答机会。
	NormalizeAnswer(input string) (canonical string, ok bool)

	// SanitizePublic 把库里存的题面 JSON 归一成"只含允许字段"的出网对象。
	// 这是公开 API 拿到题面的唯一途径：出现任何非白名单键都**报错**（而不是带着它出门，
	// 也不是悄悄裁掉后继续下发），调用方据此作废这张 challenge。
	SanitizePublic(raw []byte) (map[string]any, error)
}

// Registry 是按 id 索引的生成器表。
type Registry struct {
	mu        sync.RWMutex
	byID      map[string]Generator
	lifecycle func(string) (bool, bool)
}

func NewRegistry() *Registry { return &Registry{byID: map[string]Generator{}} }

// Register 登记一个生成器。同 id 重复登记是编程错误：它会静默替换判分实现。
func (r *Registry) Register(g Generator) error {
	if g == nil || g.ID() == "" {
		return ErrBadParams
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lifecycle != nil && !productionEligible(r.lifecycle, g.ID()) {
		return ErrProductionProfile
	}
	if _, dup := r.byID[g.ID()]; dup {
		return errors.New("generator: 同 id 重复登记 " + g.ID())
	}
	r.byID[g.ID()] = g
	return nil
}

// Lookup 按 id 取生成器。未登记一律报错——**不允许**回落到"随机一个可用题型"，
// 那会把站点声明的策略换成别的策略。
func (r *Registry) Lookup(id string) (Generator, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lifecycle != nil && !productionEligible(r.lifecycle, id) {
		return nil, ErrProductionProfile
	}
	g, ok := r.byID[id]
	if !ok {
		return nil, ErrUnknownGenerator
	}
	return g, nil
}

// IDs 返回已登记题型 id（管理面校验与观测用）。
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		if r.lifecycle != nil && !productionEligible(r.lifecycle, id) {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// --- 共用的判分形态 ---

// 可离散化题型（3A 的算术、3B 的视觉字符）共用**同一条** verifier 构造：
//
//	verifier = nonce(16B) || HMAC(K(qver), "epocha/v1|qver|" || nonce || 0x00 || 规范化答案)
//
// 之所以放在一个实现里而不是每个题型抄一遍：这是安全关键构造，两份拷贝会各自漂移，
// 而"库里的旧 verifier 被新解释读成另一批答案"是静默的正确性事故（DESIGN §5.4、ADR-011 #4）。
// 需要连续容差的题型（角度、区域）不适用这条路径，按 ADR-003 自行定义判定方式。
const (
	verifierNonceSz = 16
	verifierTagSz   = sha256.Size

	// AnswerVerifierLen 是共用 verifier 的字节长度（48）。库侧的 MaxVerifierBytes=4096 是上限
	// （ADR-028；交互题用 TSV，见 tsv.go），这里有测试按 48 钉住实际形态，
	// 改长度必须同时改迁移注释与 DESIGN §5.4。
	AnswerVerifierLen = verifierNonceSz + verifierTagSz

	// answerMACDomain 是域分隔前缀。它与站点 secret 摘要串、token 派生串、管理凭据串都不同，
	// 因此这几类 HMAC 不可互换（DESIGN §6）。
	answerMACDomain = "epocha/v1|qver|"
)

// answerTag 算答案的 HMAC 标签。
func answerTag(macKey, nonce []byte, canonicalAnswer string) []byte {
	m := hmac.New(sha256.New, macKey)
	m.Write([]byte(answerMACDomain))
	m.Write(nonce)
	m.Write([]byte{0})
	m.Write([]byte(canonicalAnswer))
	return m.Sum(nil)
}

// answerVerifier 拼出私有判分数据：nonce || tag。答案明文到此为止，不进库、不出门。
func answerVerifier(macKey, nonce []byte, canonicalAnswer string) []byte {
	tag := answerTag(macKey, nonce, canonicalAnswer)
	out := make([]byte, 0, AnswerVerifierLen)
	out = append(out, nonce...)
	return append(out, tag...)
}

// verifyAnswer 对候选答案重算同一条 HMAC 再常数时间比较。
// **没有任何**读回答案的接口：这是"答案不落库明文"能被证明的结构原因。
func verifyAnswer(macKey []byte, v PrivateVerifier, canonicalAnswer string) bool {
	if len(v) != AnswerVerifierLen {
		return false
	}
	want := answerTag(macKey, v[:verifierNonceSz], canonicalAnswer)
	return subtle.ConstantTimeCompare(want, v[verifierNonceSz:]) == 1
}

// --- 逐字容差 verifier（ADR-024，仅 text-image-v3 使用）---
//
// 形态：nonce(16B) || tag_0 || … || tag_{L-1}，每标签 32B。
// tag_i = HMAC(K(qver), "epocha/v1|qver-char|" || nonce || 0x00 || byte(i) || char_i)
//
// 为什么要单独一条构造：v1/v2 的精确 verifier 只能回答"整串是否相等"，
// 无法在不读回答案的前提下接受"有一个字符认错了"。逐字标签让 Verify 能数出
// 不匹配的位置数；接受域因此扩大到"精确答案 + 任一位置替换"。
// 边界：拿到 qver 密钥的人本来就能离线枚举 verifier（ADR-011 的残余风险）；
// 逐字标签把有密钥时的离线枚举从 32^L 降到 L×32，不改变无密钥时的安全边界。
const (
	charTagSz     = sha256.Size
	charMACDomain = "epocha/v1|qver-char|"
)

// charTag 算第 pos 个字符的 HMAC 标签。位置与字符之间写入 0x00 分隔，
// 避免把"位置字节 + 字符字节"当成一个可拼接的整体。
func charTag(macKey, nonce []byte, pos int, ch byte) []byte {
	m := hmac.New(sha256.New, macKey)
	m.Write([]byte(charMACDomain))
	m.Write(nonce)
	m.Write([]byte{0})
	m.Write([]byte{byte(pos)})
	m.Write([]byte{0})
	m.Write([]byte{ch})
	return m.Sum(nil)
}

// charVerifier 拼出逐字 verifier。答案明文到此为止。
func charVerifier(macKey, nonce []byte, canonicalAnswer string) []byte {
	out := make([]byte, 0, verifierNonceSz+len(canonicalAnswer)*charTagSz)
	out = append(out, nonce...)
	for i := 0; i < len(canonicalAnswer); i++ {
		out = append(out, charTag(macKey, nonce, i, canonicalAnswer[i])...)
	}
	return out
}

// verifyChars 逐位重算标签并累计不匹配数；所有位置都比较完，不提前返回。
// tolerance < 0 或 verifier 长度与答案长度不一致时一律判假（fail closed）。
func verifyChars(macKey []byte, v PrivateVerifier, canonicalAnswer string, tolerance int) bool {
	if tolerance < 0 || len(v) != verifierNonceSz+len(canonicalAnswer)*charTagSz {
		return false
	}
	nonce := v[:verifierNonceSz]
	mismatches := 0
	for i := 0; i < len(canonicalAnswer); i++ {
		want := charTag(macKey, nonce, i, canonicalAnswer[i])
		got := v[verifierNonceSz+i*charTagSz : verifierNonceSz+(i+1)*charTagSz]
		eq := subtle.ConstantTimeCompare(want, got)
		mismatches += 1 - eq
	}
	return mismatches <= tolerance
}

// --- 共享的输入规范化工具 ---

// trimASCII 只裁 ASCII 空白：Unicode 空白与不可见字符属于"畸形"而不是"可宽容的空格"，
// 宽容它们会让同一个答案有多种提交形式，给判分逻辑添噪声。
func trimASCII(s string) string {
	return strings.Trim(s, " \t\r\n\v\f")
}
