// Package generator 定义可插拔题型生成器的契约（DESIGN §8.1）。
//
// 本文件只有契约，没有实现：具体题型（arith-v1、text-warp-v1、cjk-v1、rotate-v1）
// 在阶段 3 落地。契约即跨阶段边界，改动需同步更新 DESIGN §8.2 的反枚举论证。
package generator

import (
	"context"
	"io"
)

// Generator 是单个题型的生成与校验器。
//
// 实现必须满足（否则阶段 3 的属性测试会失败）：
//   - Answer.Canonical 只存服务端，落库前摘要化；Challenge.Payload 中不得出现答案的任何形式。
//   - 答案空间下限 ≥2^10，或与其他层（PoW、尝试次数上限）联合后达到该下限。
//   - 决定答案的随机量（数值、正确选项位置、旋转角）必须直接取自 entropy（CSPRNG）；
//     仅图形抖动可以用由 entropy 播种的 math/rand，且每次调用新建、不得共享。
type Generator interface {
	// ID 是题型稳定标识，写入 pool.generator_id，例如 "text-warp-v1"。
	ID() string

	// Media 返回 "text" 或 "image"，决定 widget 渲染分支。
	Media() string

	// Generate 产出一道新题。entropy 是调用方提供的 crypto/rand 流，
	// 每次调用独立、用完即弃。同一实例被并发调用，实现不得持有可变状态。
	Generate(ctx context.Context, p Params, entropy io.Reader) (Challenge, error)

	// Verify 比较客户端答案。in 已由调用方截断到协议上限，
	// 实现必须用常数时间比较，且不做模糊匹配（容差靠分桶）。
	Verify(ans Answer, in string) bool
}

// Params 是站点级生成配置。
type Params struct {
	Difficulty int    // 1..5，映射到题目内部参数
	Lang       string // "zh-CN" | "en"
}

// Challenge 是生成结果：下发部分与保密部分严格分离。
type Challenge struct {
	Payload  any    // JSON 序列化后作为 pool.payload_json 下发客户端
	Answer   Answer // 仅服务端，落库前 HMAC
	Asset    []byte // 图片字节，可为 nil；经签名 URL 单次取回
	MimeType string // Asset 的 MIME，Asset 非空时必填
}

// Answer 是规范化后的离散答案，作为 HMAC 输入与比较基准。
type Answer struct {
	Canonical string
}
