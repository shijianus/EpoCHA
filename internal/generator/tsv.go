package generator

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
)

// TSV（tag-set verifier）是交互式题型的连续容差构造（ADR-028）。
//
// 线格式：
//
//	magic "ETSV"(4B) || version(1B) || nonce(16B) || count(uint16 BE) || count × tag(32B)
//
// 每个 tag 绑定 feature_id 与量化后的 value；答案明文与可推导种子都不进 verifier。
// 校验只能"测试候选是否命中集合"，没有读回答案的接口。
const (
	TSVMaxTags   = 127
	tsvHeaderLen = 4 + 1 + 16 + 2
	TSVMaxBytes  = tsvHeaderLen + TSVMaxTags*sha256.Size // 4087，留 9B 给未来头部扩展

	tsvMagic     = "ETSV"
	tsvVersion   = byte(1)
	tsvMACDomain = "epocha/v1|qver-tsv|"
)

type tsvEntry struct {
	featureID uint16
	value     uint32
	tag       [sha256.Size]byte
}

// TSVBuilder 生成一个不可变的 verifier 字节串。key 与 nonce 在构造时复制，
// 调用方之后修改原切片不会改变已加入的标签。
type TSVBuilder struct {
	key     []byte
	nonce   [16]byte
	entries []tsvEntry
	seen    map[uint64]struct{}
}

// NewTSVBuilder 要求 key ≥32B、nonce 恰好 16B；短密钥/错长度一律拒绝（fail closed）。
func NewTSVBuilder(key, nonce []byte) (*TSVBuilder, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("%w: TSV 密钥短于 32 字节", ErrBadParams)
	}
	if len(nonce) != 16 {
		return nil, fmt.Errorf("%w: TSV nonce 必须是 16 字节", ErrBadParams)
	}
	b := &TSVBuilder{
		key:  append([]byte(nil), key...),
		seen: make(map[uint64]struct{}),
	}
	copy(b.nonce[:], nonce)
	return b, nil
}

// Add 加入一个接受值。同一个 (featureID,value) 重复加入是编程错误：
// 它会悄悄浪费标签预算，且不会扩大接受域。
func (b *TSVBuilder) Add(featureID uint16, value uint32) error {
	if b == nil {
		return fmt.Errorf("%w: nil TSV builder", ErrBadParams)
	}
	if len(b.key) < 32 || b.seen == nil {
		// 零值 TSVBuilder 没有密钥/去重表；拒绝而不是用空密钥算出一个"能过"的标签。
		return fmt.Errorf("%w: TSV builder 未初始化", ErrBadParams)
	}
	k := uint64(featureID)<<32 | uint64(value)
	if _, dup := b.seen[k]; dup {
		return fmt.Errorf("%w: TSV 重复标签 feature=%d value=%d", ErrBadParams, featureID, value)
	}
	if len(b.entries) >= TSVMaxTags {
		return fmt.Errorf("%w: TSV 标签数超过 %d", ErrBadParams, TSVMaxTags)
	}
	b.entries = append(b.entries, tsvEntry{
		featureID: featureID,
		value:     value,
		tag:       tsvTag(b.key, b.nonce[:], featureID, value),
	})
	b.seen[k] = struct{}{}
	return nil
}

// Len 返回已加入的标签数。
func (b *TSVBuilder) Len() int {
	if b == nil {
		return 0
	}
	return len(b.entries)
}

// Build 生成最终 verifier。空集合拒绝生成：一个"什么都不接受"的 verifier 是配置事故，
// 不是合法的 fail-closed 题。
func (b *TSVBuilder) Build() ([]byte, error) {
	if b == nil || len(b.entries) == 0 {
		return nil, fmt.Errorf("%w: TSV 至少需要一个标签", ErrBadParams)
	}
	if len(b.entries) > TSVMaxTags {
		return nil, fmt.Errorf("%w: TSV 标签数超过 %d", ErrBadParams, TSVMaxTags)
	}
	out := make([]byte, tsvHeaderLen+len(b.entries)*sha256.Size)
	copy(out[0:4], tsvMagic)
	out[4] = tsvVersion
	copy(out[5:21], b.nonce[:])
	binary.BigEndian.PutUint16(out[21:23], uint16(len(b.entries)))
	for i, e := range b.entries {
		copy(out[tsvHeaderLen+i*sha256.Size:], e.tag[:])
	}
	return out, nil
}

func tsvTag(key, nonce []byte, featureID uint16, value uint32) [sha256.Size]byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(tsvMACDomain))
	m.Write(nonce)
	var buf [6]byte
	binary.BigEndian.PutUint16(buf[0:2], featureID)
	binary.BigEndian.PutUint32(buf[2:6], value)
	m.Write(buf[:])
	var out [sha256.Size]byte
	copy(out[:], m.Sum(nil))
	return out
}

// parseTSV 只接受精确长度、正确 magic/version、1..TSVMaxTags 的 count。
// 截断、尾部多余字节、count 被改写都会被拒绝，而不是"读多少算多少"。
func parseTSV(blob []byte) (nonce []byte, tags []byte, count int, ok bool) {
	if len(blob) < tsvHeaderLen || string(blob[0:4]) != tsvMagic || blob[4] != tsvVersion {
		return nil, nil, 0, false
	}
	n := int(binary.BigEndian.Uint16(blob[21:23]))
	if n < 1 || n > TSVMaxTags {
		return nil, nil, 0, false
	}
	if len(blob) != tsvHeaderLen+n*sha256.Size {
		return nil, nil, 0, false
	}
	return blob[5:21], blob[tsvHeaderLen:], n, true
}

// TSVContains 判断候选是否命中集合。所有标签都比较完，不提前返回；
// 候选 tag 与每个 tag 使用常数时间比较（红线 2）。
func TSVContains(blob, key []byte, featureID uint16, value uint32) bool {
	if len(key) < 32 {
		return false
	}
	nonce, tags, count, ok := parseTSV(blob)
	if !ok {
		return false
	}
	cand := tsvTag(key, nonce, featureID, value)
	match := 0
	for i := 0; i < count; i++ {
		match |= subtle.ConstantTimeCompare(cand[:], tags[i*sha256.Size:(i+1)*sha256.Size])
	}
	return match == 1
}

// TSVVerifyAny 解析一次，对多个候选做"任一命中"判断。用于点选接受域（3×3 邻格）
// 等场景；仍然把所有候选与所有 tag 比较完。
func TSVVerifyAny(blob, key []byte, featureID uint16, values ...uint32) bool {
	if len(key) < 32 || len(values) == 0 {
		return false
	}
	nonce, tags, count, ok := parseTSV(blob)
	if !ok {
		return false
	}
	match := 0
	for _, value := range values {
		cand := tsvTag(key, nonce, featureID, value)
		for i := 0; i < count; i++ {
			match |= subtle.ConstantTimeCompare(cand[:], tags[i*sha256.Size:(i+1)*sha256.Size])
		}
	}
	return match == 1
}

// TSVCount 返回标签数；畸形 blob 返回 false。供测试与观测用，不暴露接受域内容。
func TSVCount(blob []byte) (int, bool) {
	_, _, count, ok := parseTSV(blob)
	return count, ok
}
