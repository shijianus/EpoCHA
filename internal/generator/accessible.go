package generator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// accessible-choice-v1：无障碍等价通道的纯文本单选题（ADR-029）。
//
// 它是**弱安全**题型：选项是文本数字，机器/脚本可以直接判断。准入靠显式 accessibility
// 请求、站点池显式包含、24h 限额与一次性 challenge；不得把它写成生产可用或抗 AI。
const (
	AccessibleChoiceID      = "accessible-choice-v1"
	accessibleChoiceVersion = "v1"
	accessibleChoiceLen     = 1
	accessibleChoiceOptions = 12
)

type AccessibleOption struct {
	ID      string `json:"id"`
	LabelZH string `json:"label_zh"`
	LabelEN string `json:"label_en"`
}

type AccessibleChoicePublic struct {
	Kind         string             `json:"kind"`
	AnswerType   string             `json:"answer_type"`
	MaxAnswerLen int                `json:"max_answer_len"`
	Length       int                `json:"length"`
	Charset      string             `json:"charset"`
	PromptZH     string             `json:"prompt_zh"`
	PromptEN     string             `json:"prompt_en"`
	Options      []AccessibleOption `json:"options"`
}

func (AccessibleChoicePublic) PublicKind() string { return "accessible-choice" }

type AccessibleChoice struct {
	macKey []byte
}

var _ Generator = AccessibleChoice{}

func NewAccessibleChoice(macKey []byte) (AccessibleChoice, error) {
	if len(macKey) < 32 {
		return AccessibleChoice{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	return AccessibleChoice{macKey: macKey}, nil
}

func (AccessibleChoice) ID() string      { return AccessibleChoiceID }
func (AccessibleChoice) Version() string { return accessibleChoiceVersion }
func (AccessibleChoice) Media() string   { return MediaText }

type accessibleChoiceSpec struct {
	Options []AccessibleOption
	Answer  string
}

func buildAccessibleChoice(entropy io.Reader) (accessibleChoiceSpec, []byte, error) {
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return accessibleChoiceSpec{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	target, err := drawInt(entropy, 0, int64(accessibleChoiceOptions-1))
	if err != nil {
		return accessibleChoiceSpec{}, nil, err
	}
	opts := make([]AccessibleOption, accessibleChoiceOptions)
	for i := 0; i < accessibleChoiceOptions; i++ {
		var n int64
		if i == int(target) {
			n, err = drawInt(entropy, 6, 49) // 偶数 12..98
			n *= 2
		} else {
			n, err = drawInt(entropy, 5, 49) // 奇数 11..99
			n = n*2 + 1
		}
		if err != nil {
			return accessibleChoiceSpec{}, nil, err
		}
		label := strconv.FormatInt(n, 10)
		opts[i] = AccessibleOption{ID: string(rune('A' + i)), LabelZH: label, LabelEN: label}
	}
	return accessibleChoiceSpec{Options: opts, Answer: string(rune('A' + target))}, nonce, nil
}

func (g AccessibleChoice) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	spec, nonce, err := buildAccessibleChoice(entropy)
	if err != nil {
		return Question{}, err
	}
	pub := AccessibleChoicePublic{
		Kind: "accessible-choice", AnswerType: AnswerTypeText,
		MaxAnswerLen: accessibleChoiceLen, Length: accessibleChoiceLen, Charset: "ABCDEFGHIJKL",
		PromptZH: "选择唯一一个偶数", PromptEN: "Select the only even number",
		Options: spec.Options,
	}
	return Question{
		Payload:  pub,
		Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, spec.Answer)),
	}, nil
}

func (g AccessibleChoice) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

func (AccessibleChoice) NormalizeAnswer(input string) (string, bool) {
	s := strings.ToUpper(trimASCII(input))
	if len(s) != 1 || s[0] < 'A' || s[0] >= 'A'+accessibleChoiceOptions {
		return "", false
	}
	return s, true
}

func (AccessibleChoice) SanitizePublic(raw []byte) (map[string]any, error) {
	var p AccessibleChoicePublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("generator: accessible-choice 题面 JSON: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("generator: accessible-choice 题面 JSON: %w", err)
	}
	switch {
	case p.Kind != "accessible-choice" || p.AnswerType != AnswerTypeText:
		return nil, fmt.Errorf("generator: accessible-choice 类型不合法")
	case p.MaxAnswerLen != accessibleChoiceLen || p.Length != accessibleChoiceLen || p.Charset != "ABCDEFGHIJKL":
		return nil, fmt.Errorf("generator: accessible-choice 答案形状不合法")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: accessible-choice 提示不合法")
	case len(p.Options) != accessibleChoiceOptions:
		return nil, fmt.Errorf("generator: accessible-choice 选项数不合法")
	}
	for i, o := range p.Options {
		want := string(rune('A' + i))
		if o.ID != want || o.LabelZH == "" || o.LabelEN == "" || len(o.LabelZH) > 32 || len(o.LabelEN) > 32 {
			return nil, fmt.Errorf("generator: accessible-choice 选项 %d 不合法", i)
		}
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "length", "charset",
			"prompt_zh", "prompt_en", "options":
		default:
			return nil, fmt.Errorf("generator: accessible-choice 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"length": p.Length, "charset": p.Charset,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN, "options": p.Options,
	}, nil
}

// AccessibleChoiceAnswerFrom 供 devseed/测试使用；难度不影响文本题答案。
func AccessibleChoiceAnswerFrom(entropy io.Reader, _ int) (string, error) {
	spec, _, err := buildAccessibleChoice(entropy)
	if err != nil {
		return "", err
	}
	return spec.Answer, nil
}
