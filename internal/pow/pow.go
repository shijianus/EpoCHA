// Package pow 实现 sha256-prefix 工作量证明（DESIGN §7）。
package pow

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
)

// Algo 是协议标识，写进 challenge 响应，客户端据此选择实现。
const Algo = "sha256-prefix"

// 协议常量集中在此，避免调用方凭记忆写数字。
const (
	seedLen       = 16
	MinBits       = 12 // 配置与 DB CHECK 的下界
	MaxBits       = 24
	MaxNonceLen   = 16
	maxNonceValue = int64(1e12)
)

var (
	ErrInvalidBits  = errors.New("pow: bits 超出允许范围")
	ErrInvalidSeed  = errors.New("pow: seed 长度不符")
	ErrInvalidNonce = errors.New("pow: 客户端解格式不合法")
)

// Params 是一次 challenge 的 PoW 参数。Bits 与 Seed 都由服务端决定并被 token 签名覆盖。
type Params struct {
	Algo string
	Bits uint8
	Seed []byte
}

// NewParams 生成新的挑战参数。seed 每个 challenge 独立，因此解不可跨 challenge 复用。
func NewParams(bits uint8) (Params, error) {
	if bits < MinBits || bits > MaxBits {
		return Params{}, fmt.Errorf("%w: %d", ErrInvalidBits, bits)
	}
	seed := make([]byte, seedLen)
	if _, err := rand.Read(seed); err != nil {
		return Params{}, fmt.Errorf("pow: 生成 seed: %w", err)
	}
	return Params{Algo: Algo, Bits: bits, Seed: seed}, nil
}

// Verify 检查客户端解。格式校验先于哈希计算，避免把未截断的任意字符串送进哈希。
func (p Params) Verify(clientNonce string) (bool, error) {
	if p.Algo != Algo {
		return false, fmt.Errorf("pow: 未知算法 %q", p.Algo)
	}
	if len(p.Seed) != seedLen {
		return false, ErrInvalidSeed
	}
	if !validNonce(clientNonce) {
		return false, ErrInvalidNonce
	}
	h := sha256.Sum256(append(append([]byte{}, p.Seed...), clientNonce...))
	return LeadingZeroBits(h[:]) >= int(p.Bits), nil
}

// validNonce 只接受 1..16 位十进制且 ≤1e12 的字符串。
// 严格格式把"客户端可提交任意字节"的可能性关掉，也让哈希输入长度有上界。
func validNonce(s string) bool {
	if len(s) == 0 || len(s) > MaxNonceLen {
		return false
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return false
	}
	for _, r := range []byte(s) {
		if r < '0' || r > '9' {
			return false
		}
	}
	return n.IsUint64() && n.Int64() <= maxNonceValue
}

// LeadingZeroBits 返回前导零的比特数（按字节序，高位优先）。
func LeadingZeroBits(b []byte) int {
	n := 0
	for _, c := range b {
		if c == 0 {
			n += 8
			continue
		}
		for mask := byte(0x80); mask&c == 0; mask >>= 1 {
			n++
		}
		return n
	}
	return n
}
