package generator

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func tsvTestKey() []byte   { return bytes.Repeat([]byte{0x33}, 32) }
func tsvTestNonce() []byte { return bytes.Repeat([]byte{0x44}, 16) }

func TestTSVBuildContainsAndCount(t *testing.T) {
	b, err := NewTSVBuilder(tsvTestKey(), tsvTestNonce())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []uint32{10, 11, 12} {
		if err := b.Add(0, v); err != nil {
			t.Fatalf("Add(%d): %v", v, err)
		}
	}
	blob, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) != tsvHeaderLen+3*32 {
		t.Fatalf("长度 = %d，期望 %d", len(blob), tsvHeaderLen+3*32)
	}
	if n, ok := TSVCount(blob); !ok || n != 3 {
		t.Fatalf("TSVCount = (%d,%v)，期望 (3,true)", n, ok)
	}
	for _, v := range []uint32{10, 11, 12} {
		if !TSVContains(blob, tsvTestKey(), 0, v) {
			t.Fatalf("value=%d 应命中", v)
		}
	}
	if TSVContains(blob, tsvTestKey(), 0, 13) {
		t.Fatal("value=13 不应命中")
	}
	if TSVContains(blob, tsvTestKey(), 1, 10) {
		t.Fatal("不同 feature_id 不应命中")
	}
}

func TestTSVVerifyAny(t *testing.T) {
	b, _ := NewTSVBuilder(tsvTestKey(), tsvTestNonce())
	_ = b.Add(7, 100)
	blob, _ := b.Build()
	if !TSVVerifyAny(blob, tsvTestKey(), 7, 1, 99, 100) {
		t.Fatal("候选 100 应命中")
	}
	if TSVVerifyAny(blob, tsvTestKey(), 7, 1, 99) {
		t.Fatal("候选全不命中时应拒绝")
	}
	if TSVVerifyAny(blob, tsvTestKey(), 7) {
		t.Fatal("空候选应拒绝")
	}
}

func TestTSVBuilderRejectsBadInputs(t *testing.T) {
	if _, err := NewTSVBuilder(bytes.Repeat([]byte{1}, 31), tsvTestNonce()); err == nil {
		t.Fatal("短密钥应被拒绝")
	}
	if _, err := NewTSVBuilder(tsvTestKey(), make([]byte, 15)); err == nil {
		t.Fatal("15B nonce 应被拒绝")
	}
	if _, err := NewTSVBuilder(tsvTestKey(), make([]byte, 17)); err == nil {
		t.Fatal("17B nonce 应被拒绝")
	}
	empty, _ := NewTSVBuilder(tsvTestKey(), tsvTestNonce())
	if _, err := empty.Build(); err == nil {
		t.Fatal("空 TSV 应被拒绝")
	}
	var zero TSVBuilder
	if err := zero.Add(0, 1); err == nil {
		t.Fatal("零值 TSVBuilder 应拒绝 Add")
	}
	b, _ := NewTSVBuilder(tsvTestKey(), tsvTestNonce())
	if err := b.Add(0, 1); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(0, 1); err == nil {
		t.Fatal("重复 (feature,value) 应被拒绝")
	}
}

func TestTSVTagBoundary(t *testing.T) {
	b, _ := NewTSVBuilder(tsvTestKey(), tsvTestNonce())
	for i := 0; i < TSVMaxTags; i++ {
		if err := b.Add(0, uint32(i)); err != nil {
			t.Fatalf("第 %d 个标签: %v", i+1, err)
		}
	}
	blob, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) != TSVMaxBytes {
		t.Fatalf("满额 TSV 长度 = %d，期望 %d", len(blob), TSVMaxBytes)
	}
	if TSVMaxBytes > 4096 {
		t.Fatalf("TSVMaxBytes=%d 超过 MaxVerifierBytes 预算 4096", TSVMaxBytes)
	}
	if err := b.Add(0, uint32(TSVMaxTags)); err == nil {
		t.Fatal("第 128 个标签应被拒绝")
	}
}

func TestTSVTamperAndMalformedRejected(t *testing.T) {
	b, _ := NewTSVBuilder(tsvTestKey(), tsvTestNonce())
	_ = b.Add(0, 7)
	blob, _ := b.Build()
	key := tsvTestKey()

	if !TSVContains(blob, key, 0, 7) {
		t.Fatal("基线应命中")
	}
	// 错误密钥
	if TSVContains(blob, bytes.Repeat([]byte{0x55}, 32), 0, 7) {
		t.Fatal("错误密钥不应命中")
	}
	// 短密钥
	if TSVContains(blob, key[:31], 0, 7) {
		t.Fatal("短密钥不应命中")
	}
	// nonce 篡改
	nonceBad := append([]byte(nil), blob...)
	nonceBad[5] ^= 0xff
	if TSVContains(nonceBad, key, 0, 7) {
		t.Fatal("nonce 篡改后不应命中")
	}
	// magic / version 篡改
	magicBad := append([]byte(nil), blob...)
	magicBad[0] = 'X'
	if TSVContains(magicBad, key, 0, 7) {
		t.Fatal("magic 篡改后不应命中")
	}
	versionBad := append([]byte(nil), blob...)
	versionBad[4] = 2
	if TSVContains(versionBad, key, 0, 7) {
		t.Fatal("version 篡改后不应命中")
	}
	// count 篡改（长度不匹配）
	countBad := append([]byte(nil), blob...)
	binary.BigEndian.PutUint16(countBad[21:23], 2)
	if TSVContains(countBad, key, 0, 7) {
		t.Fatal("count 篡改后不应命中")
	}
	// tag 篡改
	tagBad := append([]byte(nil), blob...)
	tagBad[tsvHeaderLen] ^= 0xff
	if TSVContains(tagBad, key, 0, 7) {
		t.Fatal("tag 篡改后不应命中")
	}
	// 截断 / 尾部多余字节
	if TSVContains(blob[:len(blob)-1], key, 0, 7) {
		t.Fatal("截断后不应命中")
	}
	if TSVContains(append(append([]byte(nil), blob...), 0), key, 0, 7) {
		t.Fatal("尾部多余字节不应命中")
	}
	// count=0 的手工头
	zero := append([]byte(nil), blob[:tsvHeaderLen]...)
	binary.BigEndian.PutUint16(zero[21:23], 0)
	if TSVContains(zero, key, 0, 7) {
		t.Fatal("count=0 应被拒绝")
	}
}

func TestTSVBuilderCopiesKey(t *testing.T) {
	key := tsvTestKey()
	b, _ := NewTSVBuilder(key, tsvTestNonce())
	_ = b.Add(0, 5)
	blob, _ := b.Build()
	key[0] ^= 0xff // builder 必须已复制
	if !TSVContains(blob, tsvTestKey(), 0, 5) {
		t.Fatal("builder 应复制密钥，原 key 被改后仍应命中")
	}
	if TSVContains(blob, key, 0, 5) {
		t.Fatal("用被改过的 key 不应命中")
	}
}
