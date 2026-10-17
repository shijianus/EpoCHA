// Package eval 是**开发期**的评测底座：固定测试熵源、样本清单、评分器。
//
// 它不在服务的依赖图里（cmd/server 不 import 它，有测试钉住这一点），原因是这里有两样
// 东西绝不能出现在生产路径上：
//   - 确定性熵源：它把"下一次生成什么"变成可复现的，而生产生成必须来自 CSPRNG；
//   - ground truth：答案表。生产 HTTP API 没有任何答案出口（DESIGN §8.5）。
//
// 只有标准库：这个包被两个开发命令行工具与测试共享，不该拖进任何渲染或存储依赖。
package eval

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// deterministicReader 是 HMAC-SHA256 计数器模式的确定性字节流：
//
//	block(i) = HMAC-SHA256(seed, uint64LE(i))
//
// 为什么用它而不是 math/rand 的种子：生成路径要求的是 io.Reader，而且生产侧用
// crypto/rand.Int 的**拒绝采样**消费熵；用一个字节流源，评测与生产消费的是同一个接口，
// 抽样顺序也一致（换成 math/rand 就等于评的是另一套东西）。
//
// 它是**可复现**工具，不是密码学安全的伪随机数发生器，不得用于任何密钥或答案的生产。
type deterministicReader struct {
	seed    []byte
	counter uint64
	buf     []byte
}

// NewDeterministic 返回一个由 seed 完全决定的无限字节流。
// seed 为空时拒绝：一个静默的固定种子会让"以为是随机"变成"其实是常量"。
func NewDeterministic(seed []byte) (io.Reader, error) {
	if len(seed) < 8 {
		return nil, fmt.Errorf("eval: 测试熵源 seed 短于 8 字节")
	}
	out := make([]byte, len(seed))
	copy(out, seed)
	return &deterministicReader{seed: out}, nil
}

func (d *deterministicReader) Read(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if len(d.buf) == 0 {
			var ctr [8]byte
			binary.LittleEndian.PutUint64(ctr[:], d.counter)
			d.counter++
			m := hmac.New(sha256.New, d.seed)
			m.Write(ctr[:])
			d.buf = m.Sum(nil)
		}
		n := copy(p, d.buf)
		d.buf = d.buf[n:]
		p = p[n:]
	}
	return total, nil
}
