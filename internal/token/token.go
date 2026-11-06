// Package token 负责签名令牌的签发与校验（DESIGN §6、ADR-004/007）。
package token

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// 两个用途。密钥按 purpose+kid 派生，因此 chal token 永远不能当 resp token 用。
const (
	PurposeChallenge = "chal"
	PurposeResponse  = "resp"
)

const (
	prefix          = "v1"
	domain          = "epocha/v1|"
	maxTokenBytes   = 4096
	maxPayloadBytes = 2048
	clockSkew       = 5 * time.Second
)

var (
	ErrMalformed   = errors.New("token: 格式或签名不合法")
	ErrExpired     = errors.New("token: 已过期")
	ErrUnknownKid  = errors.New("token: 未知密钥标识")
	ErrUnusableKey = errors.New("token: 密钥材料不合法")
)

// Claims 是签名覆盖的全部安全相关参数。客户端无法改动难度、TTL 或站点绑定。
type Claims struct {
	Typ string `json:"typ"`
	Kid string `json:"kid"`
	Sid int64  `json:"sid"`
	// Svid 是签发时的站点版本。停用/重新启用站点会提升它，于是"停用前签发的未消费 token
	// 在站点重新启用后也不会复活"这条性质由签名保证，而不依赖库里还留着那行状态。
	// 阶段 1 的 token 没有这个字段（解出为 0）→ 一律拒绝：升级后最多 1 小时内的旧 token 失效，
	// 这是有意的一次性影响，账本里"已消费"的状态完整保留（DESIGN §5.3、ADR-010）。
	Svid int64  `json:"svid"`
	CID  string `json:"cid"`
	RID  string `json:"rid,omitempty"`
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
	Bits uint8  `json:"bits,omitempty"`
	Seed string `json:"seed,omitempty"`
	// Mode 与 Gen 是**验证策略的签发快照**（阶段 3A）：pow_only / pow_question。
	// 它们进签名，所以客户端不能拿一张 pow_question 的 token 去走"只交 PoW"的路径，
	// 也不能换 generator；再配合库中行的交叉核对（DESIGN §6），策略就不是客户端可控量。
	// 注意：mode 为空是**非法**的，不允许"缺字段=按旧语义放行"这种回落。
	Mode string `json:"mode"`
	Gen  string `json:"gen,omitempty"`
	// Sess 是调用方的弱会话标识，仅用于刷新/遥测状态聚合；它不是凭据，也不是信任根。
	Sess string `json:"sess,omitempty"`
}

// Signer 持有一把当前主密钥与可选的一把上一把主密钥（kid 轮换期间双活）。
type Signer struct {
	byKid map[string][]byte
	cur   string
}

// NewSigner 用主密钥（及可选的旧主密钥）构造签名器。密钥长度必须 ≥32 字节。
func NewSigner(master, previous []byte) (*Signer, error) {
	s := &Signer{byKid: map[string][]byte{}}
	cur, err := register(s.byKid, master)
	if err != nil {
		return nil, fmt.Errorf("token: 当前主密钥: %w", err)
	}
	s.cur = cur
	if len(previous) > 0 {
		if _, err := register(s.byKid, previous); err != nil {
			return nil, fmt.Errorf("token: 旧主密钥: %w", err)
		}
	}
	return s, nil
}

func register(m map[string][]byte, master []byte) (string, error) {
	if len(master) < 32 {
		return "", ErrUnusableKey
	}
	kid := Kid(master)
	if _, dup := m[kid]; dup {
		return "", fmt.Errorf("%w: 新旧主密钥相同", ErrUnusableKey)
	}
	m[kid] = master
	return kid, nil
}

// Kid 是主密钥的稳定指纹，用于轮换期间识别签发方。
func Kid(master []byte) string {
	sum := hmacSHA256(master, domain+"kid")
	return fmt.Sprintf("%x", sum[:8])
}

// CurrentKid 返回当前主密钥的 kid。
func (s *Signer) CurrentKid() string { return s.cur }

// Sign 生成 <prefix>.<kid>.<b64url(payload)>.<b64url(tag)>。
// payload 用固定字段顺序的 JSON 序列化一次后按原字节签名，验证方不对原始字节重编码。
func (s *Signer) Sign(purpose string, c Claims, now time.Time) (string, error) {
	if purpose != PurposeChallenge && purpose != PurposeResponse {
		return "", fmt.Errorf("%w: 未知用途 %q", ErrUnusableKey, purpose)
	}
	if c.Sid <= 0 || c.CID == "" {
		return "", fmt.Errorf("%w: 缺少站点或 challenge 绑定", ErrUnusableKey)
	}
	if c.Svid <= 0 {
		return "", fmt.Errorf("%w: 缺少站点版本绑定", ErrUnusableKey)
	}
	if purpose == PurposeResponse && c.RID == "" {
		return "", fmt.Errorf("%w: response 令牌缺少 rid", ErrUnusableKey)
	}
	if purpose == PurposeChallenge && c.Bits == 0 {
		return "", fmt.Errorf("%w: challenge 令牌缺少难度", ErrUnusableKey)
	}
	if purpose == PurposeChallenge {
		switch c.Mode {
		case "":
			return "", fmt.Errorf("%w: challenge 令牌缺少验证模式", ErrUnusableKey)
		case "pow_question":
			if c.Gen == "" {
				return "", fmt.Errorf("%w: pow_question 令牌缺少生成器绑定", ErrUnusableKey)
			}
		case "pow_only":
			if c.Gen != "" {
				return "", fmt.Errorf("%w: pow_only 令牌不该带生成器", ErrUnusableKey)
			}
		default:
			return "", fmt.Errorf("%w: 未知验证模式 %q", ErrUnusableKey, c.Mode)
		}
	}
	c.Typ = purpose
	c.Kid = s.cur
	c.Iat = now.Unix()
	body, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("token: 序列化: %w", err)
	}
	if len(body) > maxPayloadBytes {
		return "", fmt.Errorf("%w: payload 超长", ErrUnusableKey)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	tag := base64.RawURLEncoding.EncodeToString(tagBytes(s.byKid[s.cur], purpose, payload))
	return prefix + "." + c.Kid + "." + payload + "." + tag, nil
}

// Verify 校验签名、用途、过期与签发密钥，并返回 claims。
// 调用方仍需自行做站点一致性与一次性消费（那是 store 的职责）。
func (s *Signer) Verify(purpose string, tok string, now time.Time) (Claims, error) {
	var zero Claims
	if len(tok) == 0 || len(tok) > maxTokenBytes {
		return zero, ErrMalformed
	}
	parts := splitToken(tok)
	if parts == nil {
		return zero, ErrMalformed
	}
	if parts[0] != prefix {
		return zero, ErrMalformed
	}
	master, ok := s.byKid[parts[1]]
	if !ok {
		return zero, ErrUnknownKid
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(body) > maxPayloadBytes {
		return zero, ErrMalformed
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return zero, ErrMalformed
	}
	// 拒绝非规范 base64：同一 tag 的多种编码写法不能都算有效。
	if base64.RawURLEncoding.EncodeToString(got) != parts[3] {
		return zero, ErrMalformed
	}
	// tag 比较必须常数时间：它是唯一的密钥探测面。
	if subtle.ConstantTimeCompare(got, tagBytes(master, purpose, parts[2])) != 1 {
		return zero, ErrMalformed
	}

	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(body), maxPayloadBytes))
	dec.DisallowUnknownFields()
	var c Claims
	if err := dec.Decode(&c); err != nil {
		return zero, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if c.Typ != purpose || c.Kid != parts[1] || c.CID == "" || c.Sid <= 0 || c.Svid <= 0 {
		return zero, ErrMalformed
	}
	if purpose == PurposeResponse && c.RID == "" {
		return zero, ErrMalformed
	}
	if purpose == PurposeChallenge {
		if c.Bits == 0 || c.Seed == "" {
			return zero, ErrMalformed
		}
		// 缺 mode 的旧 token 一律拒绝：绝不把"字段缺失"解释成"按旧策略放行"
		switch c.Mode {
		case "pow_only":
			if c.Gen != "" {
				return zero, ErrMalformed
			}
		case "pow_question":
			if c.Gen == "" {
				return zero, ErrMalformed
			}
		default:
			return zero, ErrMalformed
		}
	}
	if c.Exp <= 0 || c.Iat <= 0 {
		return zero, ErrMalformed
	}
	if now.Unix() >= c.Exp {
		return zero, ErrExpired
	}
	if c.Iat > now.Add(clockSkew).Unix() {
		return zero, ErrMalformed
	}
	return c, nil
}

// splitToken 要求恰好 4 段且各段非空；不符合返回 nil。
func splitToken(tok string) []string {
	parts := make([]string, 0, 4)
	rest := tok
	for i := 0; i < 4; i++ {
		if i == 3 {
			if rest == "" {
				return nil
			}
			parts = append(parts, rest)
			return parts
		}
		idx := indexByte(rest, '.')
		if idx <= 0 {
			return nil
		}
		parts = append(parts, rest[:idx])
		rest = rest[idx+1:]
	}
	return nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// tagBytes 计算 tag 的原始字节。purpose 同时进入派生密钥与消息，构成双重域分隔。
func tagBytes(master []byte, purpose, payload string) []byte {
	key := deriveKey(master, purpose, Kid(master))
	return hmacSHA256(key, domain+purpose+"|"+payload)
}

// computeTag 是 tagBytes 的 base64url 形式，供测试与调试构造 token。
func computeTag(master []byte, purpose, payload string) string {
	return base64.RawURLEncoding.EncodeToString(tagBytes(master, purpose, payload))
}

// DerivePurpose 派生一把**用途密钥**：HMAC(master, "epocha/v1|<purpose>|<kid>")。
// 与 token 内部派生完全同一手法，因此题目 verifier 的密钥与 chal/resp 签名密钥互不可用
// ——拿到一个推不出另一个，也不会出现"同一个密钥既签 token 又算答案摘要"。
func DerivePurpose(master []byte, purpose string) []byte {
	if len(master) < 32 || purpose == "" {
		return nil
	}
	return deriveKey(master, purpose, Kid(master))
}

// deriveKey 是带域分隔的 HMAC 派生（DESIGN §6）：每个用途一把固定长度密钥，
// 域分隔串含版本、purpose 与 kid，因此不存在跨用途或跨主密钥的密钥复用。
func deriveKey(master []byte, purpose, kid string) []byte {
	return hmacSHA256(master, domain+purpose+"|"+kid)
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
