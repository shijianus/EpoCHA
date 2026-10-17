// Package generator 定义可插拔题型生成器的契约（DESIGN §8.1、ADR-002/003）。
//
// 本文件只有契约，没有实现：具体题型（arith-v1、text-warp-v1、cjk-v1、rotate-v1）在阶段 3 落地。
// 契约是跨阶段边界，改动需同步 DESIGN §8.2 的 M1–M4 指标论证。
package generator

import (
	"context"
	"io"
)

// Generator 是单个题型的生成与校验器。
//
// 实现必须满足（阶段 3 的属性测试逐条覆盖）：
//   - Payload 与 Verifier 严格分离：答案、可推导答案的种子、判分秘密材料都只能待在 Verifier 里。
//   - 安全性按 DESIGN §8.2 的四个指标分别论证（M1 题面随机空间、M2 答案空间与接受容差、
//     M3 随机猜测通过率、M4 每次尝试成本）；不得用"随机字体/噪声/CSPRNG"代替论证。
//   - 决定答案的随机量（数值、正确选项位置、旋转角）必须直接取自 entropy（CSPRNG）；
//     仅图形抖动可由 entropy 播种的 math/rand 承担，且每次调用新建、不得共享实例。
type Generator interface {
	// ID 是题型稳定标识，写入 pool.generator_id，例如 "text-warp-v1"。
	ID() string

	// Media 返回 "text" 或 "image"，决定 widget 渲染分支。
	Media() string

	// Generate 产出一道新题。entropy 是调用方提供的 crypto/rand 流，每次调用独立、用完即弃。
	// 同一实例被并发调用，实现不得持有可变状态。
	Generate(ctx context.Context, p Params, entropy io.Reader) (Challenge, error)

	// Verify 判分。input 已由调用方截断到协议上限。
	// 比较方式由题型决定：可离散化的题型用常数时间精确比较，
	// 需要容差的题型（角度、区域）自行实现判定，但不得为此把答案放进 Payload。
	Verify(v Verifier, input string) bool
}

// Params 是站点级生成配置。
type Params struct {
	Difficulty int    // 1..5，映射到题目内部参数
	Lang       string // "zh-CN" | "en"
}

// Challenge 是生成结果：公开下发部分与私有判分部分严格分离（ADR-003）。
type Challenge struct {
	Payload  any      // JSON 序列化后作为 pool.payload_json 下发客户端
	Verifier Verifier // 服务端私有判分数据，永不下发
	Asset    []byte   // 图片字节，可为 nil；经签名 URL 单次取回
	MimeType string   // Asset 的 MIME，Asset 非空时必填
}

// Verifier 是服务端私有判分数据的载体。
// 形态由每个题型自定义（摘要、参数、私有结构皆可）；阶段 3 定稿时若需要类型安全的接口，
// 由各题型自行持有解析逻辑，本层只负责"它是字节，且不出服务端"。
type Verifier []byte
