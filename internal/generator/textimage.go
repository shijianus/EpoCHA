package generator

// text-image-v1：固定六字符的视觉题，阶段 3B 的第一个图片生成器。
//
// **定位（不许漂移）**：这是"把 图片资产 → 传输 → 判分 这条工程链路跑通，并留下可复现评测入口"
// 的第一个视觉题型，**不是**已经验证过的抗 AI / 抗 OCR 方案：
//   - 图里可见的字符本来就要给答题者看，所以"图上有答案"是设计事实，不是泄露；
//     这层没有任何密码学保密性可言——被人眼或 OCR 读出就是被读出了。
//   - 抗机器能力只能来自 ADR-002 的 M1–M4 测量（见 DESIGN §8.5）。测量未完成前，
//     管理面一律 production_ready:false，文档与错误响应都不声称它抗自动解题。
//   - 加旋转、干扰线、噪声**不构成**安全性论据（红线 14）：它们只改变题面随机空间 M1。
//
// 与 arithmetic-baseline-v1 共用同一条判分形态（generator.go 的 answerVerifier/verifyAnswer，
// DESIGN §5.4）：答案明文不落库、不下发、不进日志；判分是"对提交值重算 HMAC 再常数时间比较"。
// 本轮**没有**为新题型发明新的密码学协议。

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	mrand "math/rand"
	"strings"
	"sync"

	xdraw "golang.org/x/image/draw"
	xfont "golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/f64"
	"golang.org/x/image/math/fixed"
)

const (
	TextImageID      = "text-image-v1"
	textImageVersion = "v1"

	// TextImageV2ID 是强形变版本。它不替换 v1：v1 仍是历史基线，v2 需要独立评测。
	TextImageV2ID      = "text-image-v2"
	textImageV2Version = "v2"

	// TextImageV3ID 是"抽象文字"版本（ADR-024）：v2 的逐字形变 + 字形层模糊/正弦位移/
	// 擦除点/低对比，并允许最多 1 个字符替换。它同样不替换 v1/v2。
	TextImageV3ID      = "text-image-v3"
	textImageV3Version = "v3"

	// TextImageLength 是**固定**答案长度。不做可变长度：长度本身会变成答案的一部分信息，
	// 归一化也要多一条"4..7 都算合法"的分支，而代价是 M3 变差（短答案的猜中率更高）。
	TextImageLength = 6
	// TextImageV2Length 是 v2 的固定长度：8 个字符把 M1 从 32^6 提到 32^8，
	// 同时给"字间重叠 + 旋转 + 剪切"留出更多可读空间（6 字符在强形变下太短，误读一个就整串错）。
	TextImageV2Length = 8
	// TextImageV3Length 回到 6：v3 有单字容差，但仍要保持答案空间足够大（32^6），
	// 同时给模糊/位移后的字形留出人眼可读的余地。
	TextImageV3Length = 6

	// TextImageCharset 是公开常量：32 个字符，剔除肉眼易混的 I/1 与 O/0（本集合无小写，
	// 所以也不再排除 l）。它是题面的一部分（进 payload），**不是秘密**。
	TextImageCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	// TextImageV2Charset 与 v1 相同；单独命名是为了让 v2 的公开题面与评测代码
	// 明确引用 v2 契约，而不是"碰巧继承了 v1 的常量"。
	TextImageV2Charset = TextImageCharset
	// TextImageV3Charset 与 v1/v2 相同；v3 的差异在渲染与容差，不在字符集。
	TextImageV3Charset = TextImageCharset

	// MimeTypePNG 必须与 store.MimeTypePNG 逐字节一致；交叉检查在 internal/pool 里有测试。
	MimeTypePNG = "image/png"

	// MaxImageAssetBytes 必须与 store.MaxAssetBytes 一致（同样是 internal/pool 的交叉测试）。
	// 生成器自己先挡一次：否则一张超标的图会在每次补题时被写库侧拒绝，
	// 池子永远是空的而运维只看得见"资源到顶"，诊断方向完全错。
	MaxImageAssetBytes = 65536

	// --- 渲染参数的硬上界 ---
	// 档位表里的每一个值都必须通过这些检查，越界即 ErrBadParams——**不是**"裁一下继续画"。
	// 这组常量的意义是让"资源有界"成为代码事实而不是文档承诺（DESIGN §13、AGENTS 硬性约束）。
	maxImageWidth, maxImageHeight = 640, 256
	// maxImagePixels 比"宽上限 × 高上限"更小，两个约束因此各自可达：
	// 如果它就等于两者的乘积，那它永远不可能是被违反的那一条，写出来只是给人看的装饰。
	maxImagePixels           = 120_000
	minFontSize, maxFontSize = 16.0, 96.0
	minDPI, maxDPI           = 72.0, 150.0
	maxRotationDeg           = 15.0
	maxBaselineJitter        = 12
	maxSpacingJitter         = 12
	maxInterferenceLines     = 8
	maxLineSegments          = 8
	// maxNoiseDivisor：噪声点数 ≤ 像素总数 / 它（即 ≤5%）。
	maxNoiseDivisor = 20
	// glyphEdgeMargin：字形包围盒与画布边缘必须保留的最小像素距离。
	// 有界重试的判定就用它；**字符被裁切这条路径在代码里不存在**。
	glyphEdgeMargin = 2
	// lineThickness：干扰线粗细（像素）。
	lineThickness = 2

	// layoutRetries 是"重排字形直到既不越界也不挤在一起"的**有界**次数。
	// 失败返回 ErrGenerateExhausted（可重试），绝不退化成"发一张看不懂的图"。
	layoutRetries = 128

	// jitterSeedBytes 是图形抖动 PRNG 的种子长度。
	jitterSeedBytes = 8

	// 8 位灰度下的三种墨量：纸亮、字暗、线居中。
	paperGray, inkGray, lineGray = uint8(240), uint8(28), uint8(120)
	// lineCrossGray 是 v2 的"穿过字形"线颜色：比背景线更深，但仍明显浅于字墨，
	// 用来切开 OCR 的连通域而不把字符涂掉。
	lineCrossGray = uint8(70)
	// noiseGraySpread：噪声点相对纸面向下的最大偏移（比纸暗一点，不伪装成字）。
	noiseGraySpread = 60

	// probeFontSize 只用于 NewTextImage 的"字体必须覆盖字符集"探测，与实际渲染档位无关。
	probeFontSize = 32.0
)

var (
	// ErrFontGlyphMissing 是内嵌字体缺少字符集里某个字形。它在**登记生成器时**就失败，
	// 服务不会带着一个"没人能答对"的题型启动（DESIGN §17 阶段 3B：字体缺失即启动失败）。
	ErrFontGlyphMissing = errors.New("generator: 内嵌字体缺字形")
	// ErrRenderTooLarge 是 PNG 编码结果超出单题资产上限。它是**配置/档位错误**而不是瞬时故障：
	// 同一档位重试多少次都会超，所以不能当成可重试的 Exhausted 处理。
	ErrRenderTooLarge = errors.New("generator: 渲染结果超出资产字节上限")
)

// ImageParams 是一道视觉题的**有界**渲染参数。导出它是因为两处都需要读懂档位：
// 评测 manifest 要写复现信息，管理面要解释"这一档为什么长这样"。
type ImageParams struct {
	Width, Height int
	FontSize      float64
	DPI           float64
	// MaxRotationDeg 是**单字**旋转角的绝对值上界（度）。0 = 不旋转。
	MaxRotationDeg float64
	// BaselineJitter 是基线上下抖动（±像素）；SpacingJitter 是字间额外间隙（0..N 像素）。
	// SpacingJitter 只加不减：把字符往一起挤是可读性问题，不是难度。
	BaselineJitter int
	SpacingJitter  int
	// Lines 是干扰折线条数，LineSegments 是每条的段数，Noise 是噪点个数。
	Lines        int
	LineSegments int
	Noise        int

	// Margin 是字形包围盒与画布边缘的最小距离（像素）；0 表示沿用 glyphEdgeMargin(2)。
	// v3 的模糊/正弦位移会把墨迹向外扩，因此必须用更大的边距，否则"不裁切"的几何保证失效。
	Margin int

	// style 只给 text-image-v2/v3 使用。零值必须与 v1 的渲染路径完全一致：
	// v1 的每个档位都不填它，于是 v1 的字节级形状不因这次扩展而改变。
	style textImageStyle
}

// textImageStyle 是 v2 的附加形变参数。它们只改题面形状，不参与答案与判分。
//
// 为什么不复制一份渲染器：v1 与 v2 共用同一条"字形测量 → 布局 → 画布 → PNG"路径，
// 私有 verifier 也共用 generator.go 的那一条。复制会让"加一个档位"变成两处同时改，
// 而漏掉的往往是边界检查。
type textImageStyle struct {
	// RotationDeg > 0 时覆盖 ImageParams.MaxRotationDeg（v1 的上界仍是 15°）。
	RotationDeg float64
	// Shear 是水平剪切系数（x' = x + shear*y），负值表示反向。
	Shear float64
	// ScaleMin/ScaleMax 是单字缩放区间；1/1 表示不变。
	ScaleMin, ScaleMax float64
	// OverlapDivisor 越小，允许的字间重叠越大（v1 = 4）。
	OverlapDivisor float64
	// OverlapPx 是主动向左推的像素上界；0 = 不主动重叠。
	OverlapPx int
	// WaveAmplitude/WavePeriod 让基线按水平位置做正弦位移。
	WaveAmplitude int
	WavePeriod    float64
	// CrossLines 是在字形画完之后再画几条穿过字形的干扰线。
	CrossLines int
	// InkSpread 让每个字的墨色在 inkGray..inkGray+InkSpread 间随机。
	InkSpread int
	// Stroke 是加粗半径（像素）；0 = 不加粗。
	Stroke int

	// --- v3 的字形层后处理（ADR-024）---
	// 这些字段只改题面形状，不参与答案与判分；零值 = 不启用。
	// BlurPasses 是 3×3 加权模糊（核 1-2-1）的遍数，只模糊字形层。
	BlurPasses int
	// WarpAmp/WarpPeriod 是平滑正弦位移的幅度（像素）与周期（像素）。
	WarpAmp    int
	WarpPeriod float64
	// EraseDots 是字形包围盒并集内的稀疏擦除点数。
	EraseDots int
	// InkGray/PaperGray 是 v3 的墨色/纸色；0 表示沿用包级默认。
	InkGray   int
	PaperGray int
	// PaperTexture 是纸面低频纹理幅度（0..16）。
	PaperTexture int
}

// abstract 判断是否启用 v3 的字形层后处理。零值 style（v1/v2）一律走旧路径，
// 这是"v1/v2 的字节形状不被本次扩展改变"的代码依据。
func (s textImageStyle) abstract() bool {
	return s.BlurPasses > 0 || s.WarpAmp > 0 || s.EraseDots > 0 ||
		s.InkGray > 0 || s.PaperGray > 0 || s.PaperTexture > 0
}

func (s textImageStyle) validate() error {
	// v1 的五档全部使用零值 style；零值不是"缩放 0"，而是"不启用 v2 形变"。
	if s == (textImageStyle{}) {
		return nil
	}
	switch {
	case s.RotationDeg < 0 || s.RotationDeg > 45:
		return fmt.Errorf("%w: v2 旋转上界 %v° 越界（≤45）", ErrBadParams, s.RotationDeg)
	case s.Shear < -0.6 || s.Shear > 0.6:
		return fmt.Errorf("%w: v2 剪切 %v 越界（±0.6）", ErrBadParams, s.Shear)
	case s.ScaleMin < 0.5 || s.ScaleMax > 1.6 || s.ScaleMin > s.ScaleMax:
		return fmt.Errorf("%w: v2 缩放区间 %v..%v 越界", ErrBadParams, s.ScaleMin, s.ScaleMax)
	case s.OverlapDivisor != 0 && (s.OverlapDivisor < 1 || s.OverlapDivisor > 8):
		return fmt.Errorf("%w: v2 重叠除数 %v 越界（1..8）", ErrBadParams, s.OverlapDivisor)
	case s.OverlapPx < 0 || s.OverlapPx > 24:
		return fmt.Errorf("%w: v2 主动重叠 %d 越界（≤24）", ErrBadParams, s.OverlapPx)
	case s.WaveAmplitude < 0 || s.WaveAmplitude > 12:
		return fmt.Errorf("%w: v2 波浪幅度 %d 越界（≤12）", ErrBadParams, s.WaveAmplitude)
	case s.WavePeriod != 0 && (s.WavePeriod < 16 || s.WavePeriod > 200):
		return fmt.Errorf("%w: v2 波浪周期 %v 越界（16..200）", ErrBadParams, s.WavePeriod)
	case s.CrossLines < 0 || s.CrossLines > 6:
		return fmt.Errorf("%w: v2 穿越线 %d 越界（≤6）", ErrBadParams, s.CrossLines)
	case s.InkSpread < 0 || s.InkSpread > 120:
		return fmt.Errorf("%w: v2 墨色扩散 %d 越界（≤120）", ErrBadParams, s.InkSpread)
	case s.Stroke < 0 || s.Stroke > 3:
		return fmt.Errorf("%w: v2 加粗 %d 越界（≤3）", ErrBadParams, s.Stroke)
	case s.BlurPasses < 0 || s.BlurPasses > 2:
		return fmt.Errorf("%w: v3 模糊遍数 %d 越界（≤2）", ErrBadParams, s.BlurPasses)
	case s.WarpAmp < 0 || s.WarpAmp > 6:
		return fmt.Errorf("%w: v3 位移幅度 %d 越界（≤6）", ErrBadParams, s.WarpAmp)
	case s.WarpAmp > 0 && (s.WarpPeriod < 24 || s.WarpPeriod > 80):
		return fmt.Errorf("%w: v3 位移周期 %v 越界（24..80）", ErrBadParams, s.WarpPeriod)
	case s.WarpAmp == 0 && s.WarpPeriod != 0:
		return fmt.Errorf("%w: v3 未启用位移但给了周期 %v", ErrBadParams, s.WarpPeriod)
	case s.EraseDots < 0 || s.EraseDots > 32:
		return fmt.Errorf("%w: v3 擦除点 %d 越界（≤32）", ErrBadParams, s.EraseDots)
	case s.InkGray < 0 || s.InkGray > 255:
		return fmt.Errorf("%w: v3 墨色 %d 越界", ErrBadParams, s.InkGray)
	case s.PaperGray < 0 || s.PaperGray > 255:
		return fmt.Errorf("%w: v3 纸色 %d 越界", ErrBadParams, s.PaperGray)
	case s.InkGray > 0 && s.PaperGray > 0 && s.PaperGray-s.InkGray < 96:
		return fmt.Errorf("%w: v3 墨/纸对比 %d 过低（需要 ≥96）", ErrBadParams, s.PaperGray-s.InkGray)
	case s.PaperTexture < 0 || s.PaperTexture > 16:
		return fmt.Errorf("%w: v3 纸面纹理 %d 越界（≤16）", ErrBadParams, s.PaperTexture)
	}
	return nil
}

// textImageTiers 是**仅有的**五档。刻意不做参数笛卡尔积：几百个没测过的组合里，
// 每一档都要有人读过图、量过字节数、跑过评测，否则"配置灵活"只是把风险摊开。
//
// d1 是"干净档"：零旋转、零抖动、零干扰、零噪声。它有两个用途——人工可读性的对照条件，
// 以及字节级可复现的渲染（同一答案必然同一 PNG）。
var textImageTiers = map[int]ImageParams{
	1: {Width: 280, Height: 104, FontSize: 40, DPI: 72, MaxRotationDeg: 0, BaselineJitter: 0, SpacingJitter: 0, Lines: 0, LineSegments: 0, Noise: 0},
	2: {Width: 300, Height: 112, FontSize: 40, DPI: 72, MaxRotationDeg: 3, BaselineJitter: 2, SpacingJitter: 3, Lines: 1, LineSegments: 3, Noise: 150},
	3: {Width: 320, Height: 116, FontSize: 40, DPI: 72, MaxRotationDeg: 5, BaselineJitter: 3, SpacingJitter: 4, Lines: 2, LineSegments: 4, Noise: 300},
	4: {Width: 340, Height: 124, FontSize: 42, DPI: 72, MaxRotationDeg: 7, BaselineJitter: 4, SpacingJitter: 5, Lines: 3, LineSegments: 5, Noise: 450},
	5: {Width: 360, Height: 132, FontSize: 44, DPI: 72, MaxRotationDeg: 10, BaselineJitter: 5, SpacingJitter: 6, Lines: 4, LineSegments: 6, Noise: 600},
}

// TextImageParams 返回某个难度档的渲染参数。
func TextImageParams(difficulty int) (ImageParams, error) {
	p, ok := textImageTiers[difficulty]
	if !ok {
		return ImageParams{}, fmt.Errorf("%w: 难度 %d 没有对应的视觉档位", ErrBadParams, difficulty)
	}
	return p, nil
}

// textImageV2Tiers 是 v2 的五档。与 v1 的关键差别：
//   - 8 字符、480×160；
//   - 单字旋转 10°..35°、剪切 0.15..0.55、缩放 0.90..1.30、主动重叠 4..12px；
//   - 波浪基线 + 穿过字形的干扰线 + 每字随机墨色 + 1..3px 加粗。
//
// 这些参数是**题面形状**，不是安全证明；它们只改变 M1。M4 仍须由 OCR 基线测量。
var textImageV2Tiers = map[int]ImageParams{
	1: {Width: 480, Height: 160, FontSize: 46, DPI: 72, BaselineJitter: 4, SpacingJitter: 6, Lines: 2, LineSegments: 4, Noise: 600,
		style: textImageStyle{RotationDeg: 10, Shear: 0.15, ScaleMin: 0.90, ScaleMax: 1.10, OverlapDivisor: 2.6, OverlapPx: 4, WaveAmplitude: 2, WavePeriod: 80, CrossLines: 1, InkSpread: 40, Stroke: 1}},
	2: {Width: 480, Height: 160, FontSize: 46, DPI: 72, BaselineJitter: 5, SpacingJitter: 7, Lines: 3, LineSegments: 5, Noise: 900,
		style: textImageStyle{RotationDeg: 18, Shear: 0.25, ScaleMin: 0.85, ScaleMax: 1.15, OverlapDivisor: 2.2, OverlapPx: 6, WaveAmplitude: 3, WavePeriod: 70, CrossLines: 2, InkSpread: 50, Stroke: 1}},
	3: {Width: 480, Height: 160, FontSize: 46, DPI: 72, BaselineJitter: 5, SpacingJitter: 7, Lines: 3, LineSegments: 6, Noise: 1000,
		style: textImageStyle{RotationDeg: 22, Shear: 0.30, ScaleMin: 0.85, ScaleMax: 1.15, OverlapDivisor: 2.8, OverlapPx: 3, WaveAmplitude: 3, WavePeriod: 65, CrossLines: 1, InkSpread: 50, Stroke: 1}},
	4: {Width: 480, Height: 160, FontSize: 46, DPI: 72, BaselineJitter: 7, SpacingJitter: 9, Lines: 5, LineSegments: 7, Noise: 1500,
		style: textImageStyle{RotationDeg: 30, Shear: 0.45, ScaleMin: 0.75, ScaleMax: 1.25, OverlapDivisor: 1.6, OverlapPx: 10, WaveAmplitude: 5, WavePeriod: 50, CrossLines: 3, InkSpread: 80, Stroke: 2}},
	5: {Width: 480, Height: 160, FontSize: 46, DPI: 72, BaselineJitter: 8, SpacingJitter: 10, Lines: 6, LineSegments: 8, Noise: 1800,
		style: textImageStyle{RotationDeg: 35, Shear: 0.55, ScaleMin: 0.70, ScaleMax: 1.30, OverlapDivisor: 1.4, OverlapPx: 12, WaveAmplitude: 6, WavePeriod: 40, CrossLines: 3, InkSpread: 100, Stroke: 3}},
}

// TextImageV2Params 返回 v2 某个难度档的渲染参数。
func TextImageV2Params(difficulty int) (ImageParams, error) {
	p, ok := textImageV2Tiers[difficulty]
	if !ok {
		return ImageParams{}, fmt.Errorf("%w: 难度 %d 没有对应的 v2 档位", ErrBadParams, difficulty)
	}
	return p, nil
}

// textImageV3Tiers 是"抽象文字"的五档（ADR-024）。与 v2 的取舍不同：
//   - 单字旋转只到 24°，不靠大角度旋转制造难度；
//   - 主要抽象手段是字形层加权模糊（1..2 遍 3×3 核 1-2-1）、正弦位移（1..3px）、
//     稀疏擦除点（4..20 个）、低对比（墨 44..52 / 纸 238..242）与纸面纹理；
//   - Margin 必须 ≥ WarpAmp + 2*BlurPasses + 2，否则位移会把墨迹推出画布；
//   - 6 字符 + 单字容差：人认错一个字符仍能通过，但答案空间仍是 32^6。
//
// 这些参数是**题面形状**，不是安全证明；M4 未测，production_ready 必须保持 false。
var textImageV3Tiers = map[int]ImageParams{
	1: {Width: 480, Height: 160, FontSize: 50, DPI: 72, BaselineJitter: 3, SpacingJitter: 4, Lines: 1, LineSegments: 4, Noise: 400, Margin: 8,
		style: textImageStyle{RotationDeg: 6, Shear: 0.06, ScaleMin: 0.94, ScaleMax: 1.06, OverlapDivisor: 6.0, OverlapPx: 0, WaveAmplitude: 1, WavePeriod: 90, CrossLines: 1, InkSpread: 24, Stroke: 0,
			BlurPasses: 1, WarpAmp: 1, WarpPeriod: 70, EraseDots: 4, InkGray: 44, PaperGray: 242, PaperTexture: 4}},
	2: {Width: 480, Height: 160, FontSize: 50, DPI: 72, BaselineJitter: 4, SpacingJitter: 5, Lines: 2, LineSegments: 5, Noise: 600, Margin: 9,
		style: textImageStyle{RotationDeg: 10, Shear: 0.10, ScaleMin: 0.93, ScaleMax: 1.07, OverlapDivisor: 6.0, OverlapPx: 0, WaveAmplitude: 1, WavePeriod: 85, CrossLines: 1, InkSpread: 28, Stroke: 0,
			BlurPasses: 1, WarpAmp: 2, WarpPeriod: 64, EraseDots: 8, InkGray: 46, PaperGray: 241, PaperTexture: 5}},
	3: {Width: 480, Height: 160, FontSize: 50, DPI: 72, BaselineJitter: 5, SpacingJitter: 6, Lines: 2, LineSegments: 5, Noise: 800, Margin: 10,
		style: textImageStyle{RotationDeg: 14, Shear: 0.14, ScaleMin: 0.92, ScaleMax: 1.08, OverlapDivisor: 5.0, OverlapPx: 0, WaveAmplitude: 2, WavePeriod: 80, CrossLines: 1, InkSpread: 32, Stroke: 0,
			BlurPasses: 1, WarpAmp: 2, WarpPeriod: 56, EraseDots: 12, InkGray: 48, PaperGray: 240, PaperTexture: 6}},
	4: {Width: 480, Height: 160, FontSize: 50, DPI: 72, BaselineJitter: 6, SpacingJitter: 7, Lines: 3, LineSegments: 6, Noise: 1000, Margin: 11,
		style: textImageStyle{RotationDeg: 18, Shear: 0.18, ScaleMin: 0.92, ScaleMax: 1.08, OverlapDivisor: 4.0, OverlapPx: 0, WaveAmplitude: 2, WavePeriod: 72, CrossLines: 2, InkSpread: 36, Stroke: 0,
			BlurPasses: 2, WarpAmp: 3, WarpPeriod: 48, EraseDots: 16, InkGray: 50, PaperGray: 239, PaperTexture: 7}},
	5: {Width: 480, Height: 160, FontSize: 50, DPI: 72, BaselineJitter: 7, SpacingJitter: 7, Lines: 3, LineSegments: 7, Noise: 1200, Margin: 12,
		style: textImageStyle{RotationDeg: 20, Shear: 0.20, ScaleMin: 0.92, ScaleMax: 1.08, OverlapDivisor: 3.5, OverlapPx: 0, WaveAmplitude: 2, WavePeriod: 64, CrossLines: 2, InkSpread: 40, Stroke: 1,
			BlurPasses: 2, WarpAmp: 3, WarpPeriod: 40, EraseDots: 20, InkGray: 52, PaperGray: 238, PaperTexture: 8}},
}

// TextImageV3Params 返回 v3 某个难度档的渲染参数。
func TextImageV3Params(difficulty int) (ImageParams, error) {
	p, ok := textImageV3Tiers[difficulty]
	if !ok {
		return ImageParams{}, fmt.Errorf("%w: 难度 %d 没有对应的 v3 档位", ErrBadParams, difficulty)
	}
	return p, nil
}

// textImageProfile 是同一套渲染器上的一组"题型身份"：id/version、答案长度、字符集与档位表。
// v1/v2/v3 只在 profile 上不同，判分路径共用；tolerance>0 时改用逐字 verifier（ADR-024）。
type textImageProfile struct {
	id      string
	version string
	length  int
	charset string
	tiers   map[int]ImageParams
	// tolerance 是允许的字符替换数。0 = 精确 HMAC（v1/v2）；1 = 逐字标签容差（v3）。
	tolerance int
}

var textImageV1Profile = textImageProfile{
	id: TextImageID, version: textImageVersion,
	length: TextImageLength, charset: TextImageCharset, tiers: textImageTiers,
}

var textImageV2Profile = textImageProfile{
	id: TextImageV2ID, version: textImageV2Version,
	length: TextImageV2Length, charset: TextImageV2Charset, tiers: textImageV2Tiers,
}

var textImageV3Profile = textImageProfile{
	id: TextImageV3ID, version: textImageV3Version,
	length: TextImageV3Length, charset: TextImageV3Charset, tiers: textImageV3Tiers,
	tolerance: 1,
}

// validate 检查全部渲染边界。任何一项越界都拒绝，绝不"就地夹紧后继续画"：
// 静默夹紧会让配置与实际形状不一致，而实际形状才是被评测的那个。
func (p ImageParams) validate() error {
	switch {
	case p.Width < 40 || p.Width > maxImageWidth:
		return fmt.Errorf("%w: 宽 %d 越界（≤%d）", ErrBadParams, p.Width, maxImageWidth)
	case p.Height < 40 || p.Height > maxImageHeight:
		return fmt.Errorf("%w: 高 %d 越界（≤%d）", ErrBadParams, p.Height, maxImageHeight)
	case p.Width*p.Height > maxImagePixels:
		return fmt.Errorf("%w: 像素总数 %d 越界（≤%d）", ErrBadParams, p.Width*p.Height, maxImagePixels)
	case p.FontSize < minFontSize || p.FontSize > maxFontSize:
		return fmt.Errorf("%w: 字号 %v 越界（%v..%v）", ErrBadParams, p.FontSize, minFontSize, maxFontSize)
	case p.DPI < minDPI || p.DPI > maxDPI:
		return fmt.Errorf("%w: DPI %v 越界（%v..%v）", ErrBadParams, p.DPI, minDPI, maxDPI)
	case p.MaxRotationDeg < 0 || p.MaxRotationDeg > maxRotationDeg:
		return fmt.Errorf("%w: 旋转上界 %v° 越界（≤%v）", ErrBadParams, p.MaxRotationDeg, maxRotationDeg)
	case p.BaselineJitter < 0 || p.BaselineJitter > maxBaselineJitter:
		return fmt.Errorf("%w: 基线抖动 %d 越界（≤%d）", ErrBadParams, p.BaselineJitter, maxBaselineJitter)
	case p.SpacingJitter < 0 || p.SpacingJitter > maxSpacingJitter:
		return fmt.Errorf("%w: 字间抖动 %d 越界（≤%d）", ErrBadParams, p.SpacingJitter, maxSpacingJitter)
	case p.Lines < 0 || p.Lines > maxInterferenceLines:
		return fmt.Errorf("%w: 干扰线条数 %d 越界（≤%d）", ErrBadParams, p.Lines, maxInterferenceLines)
	case p.LineSegments < 0 || p.LineSegments > maxLineSegments:
		return fmt.Errorf("%w: 干扰线段数 %d 越界（≤%d）", ErrBadParams, p.LineSegments, maxLineSegments)
	case p.Noise < 0 || p.Noise > p.Width*p.Height/maxNoiseDivisor:
		return fmt.Errorf("%w: 噪声点数 %d 超过像素总数的 1/%d", ErrBadParams, p.Noise, maxNoiseDivisor)
	case p.Margin < 0 || p.Margin > 24:
		return fmt.Errorf("%w: 字形边距 %d 越界（≤24）", ErrBadParams, p.Margin)
	}
	if err := p.style.validate(); err != nil {
		return err
	}
	// 后处理会把墨迹向外扩：边距必须覆盖位移 + 模糊半径，否则"不裁切"的几何保证失效。
	if p.style.abstract() && p.margin() < p.style.WarpAmp+2*p.style.BlurPasses+2 {
		return fmt.Errorf("%w: 边距 %d 小于位移 %d + 模糊 %d 所需的余量",
			ErrBadParams, p.margin(), p.style.WarpAmp, p.style.BlurPasses)
	}
	// 字形必须放得下：字号 × 6 个字符至少要有可辨认的宽度，否则布局重试会变成忙循环。
	if p.Width <= (glyphEdgeMargin+1)*2 {
		return fmt.Errorf("%w: 宽 %d 放不下任何字形", ErrBadParams, p.Width)
	}
	return nil
}

// margin 返回实际使用的字形边距。0 必须解释成 glyphEdgeMargin(2)，
// 这样 v1/v2 的全部档位（都不填 Margin）保持历史行为不变。
func (p ImageParams) margin() int {
	if p.Margin > 0 {
		return p.Margin
	}
	return glyphEdgeMargin
}

// TextImagePublic 是视觉题的**唯一**公开题面类型：字段即白名单。
// 答案、抖动种子、字形位置、判分材料都不在这个类型里（不是靠 json:"-" 抑制，是根本没有）。
//
// 六个字段全是"怎么输入"与"这张图多大"，没有一个是"答案是什么"：
// Charset 是公开常量，ImageWidth/Height 让客户端可以校验收到的图。
type TextImagePublic struct {
	Length       int    `json:"length"`
	Charset      string `json:"charset"`
	AnswerType   string `json:"answer_type"`
	MaxAnswerLen int    `json:"max_answer_len"`
	ImageWidth   int    `json:"image_width"`
	ImageHeight  int    `json:"image_height"`
	// Tolerant 是允许的字符替换数：0/缺省 = 精确匹配（v1/v2），1 = v3。
	// 它必须与 profile 一致，否则 SanitizePublic 报错（防止库里的旧题面被新实现换一套判分口径）。
	Tolerant int `json:"tolerant,omitempty"`
}

func (TextImagePublic) PublicKind() string { return "text-image" }

// TextImage 实现 Generator。不可变字段，可并发调用（字体在包级缓存里且只读）。
type TextImage struct {
	macKey  []byte
	profile textImageProfile
}

var _ Generator = TextImage{}

// 字体只解析一次：一次 sfnt 解析是毫秒级，每题重算是纯浪费。
//
// 并发依据是**读过的真实注释**（x/image@v0.36.0）：
//   - opentype.Font = sfnt.Font，"不带 *sfnt.Buffer 参数的方法一律可并发"；NewFace 属于这一类；
//   - "A Face is not safe to use concurrently" ⇒ 每次 Generate 自己 NewFace、自己 Close。
var (
	textImageFontOnce sync.Once
	textImageFont     *opentype.Font
	textImageFontErr  error
)

func loadGoRegular() (*opentype.Font, error) {
	textImageFontOnce.Do(func() {
		textImageFont, textImageFontErr = opentype.Parse(goregular.TTF)
	})
	return textImageFont, textImageFontErr
}

// NewTextImage 构造 v1 视觉生成器，并在**登记阶段**就把两类事故挡在启动时：
//  1. 内嵌字体必须覆盖整个字符集（缺字形 ⇒ 题面永远答不对，而响应看起来一切正常）；
//  2. 五档渲染参数必须全部通过边界自检（档位表写错 ⇒ 现在失败，而不是第一次补题时）。
func NewTextImage(macKey []byte) (TextImage, error) {
	return newTextImage(macKey, textImageV1Profile)
}

// NewTextImageV2 构造强形变 v2。它与 v1 共用判分路径，但独立档位、独立 ID，
// 因此管理面与题池可以把"v2 是否可用"单独观测，不会把 v1 的历史结论带过去。
func NewTextImageV2(macKey []byte) (TextImage, error) {
	return newTextImage(macKey, textImageV2Profile)
}

// NewTextImageV3 构造抽象文字 v3：v2 的形变 + 字形层后处理 + 单字容差。
// 它使用逐字 HMAC verifier（ADR-024），与 v1/v2 的 48B 精确 verifier 是两条构造。
func NewTextImageV3(macKey []byte) (TextImage, error) {
	return newTextImage(macKey, textImageV3Profile)
}

func newTextImage(macKey []byte, profile textImageProfile) (TextImage, error) {
	if len(macKey) < 32 {
		return TextImage{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	if profile.tolerance < 0 || profile.tolerance > 1 {
		return TextImage{}, fmt.Errorf("%w: 容差 %d 越界（只支持 0/1）", ErrBadParams, profile.tolerance)
	}
	font, err := loadGoRegular()
	if err != nil {
		return TextImage{}, fmt.Errorf("generator: 内嵌字体解析失败: %w", err)
	}
	face, err := opentype.NewFace(font, &opentype.FaceOptions{
		Size: probeFontSize, DPI: minDPI, Hinting: xfont.HintingFull,
	})
	if err != nil {
		return TextImage{}, fmt.Errorf("generator: 内嵌字体 face: %w", err)
	}
	defer func() { _ = face.Close() }()
	for _, r := range profile.charset {
		dr, _, _, _, ok := face.Glyph(fixed.P(0, 0), r)
		if !ok || dr.Empty() {
			return TextImage{}, fmt.Errorf("%w: %q", ErrFontGlyphMissing, r)
		}
	}
	for d := 1; d <= MaxDifficulties; d++ {
		p, ok := profile.tiers[d]
		if !ok {
			return TextImage{}, fmt.Errorf("%w: %s 缺少难度 %d 的档位", ErrBadParams, profile.id, d)
		}
		if err := p.validate(); err != nil {
			return TextImage{}, err
		}
	}
	return TextImage{macKey: macKey, profile: profile}, nil
}

// effectiveProfile 让零值 TextImage 继续表现为 v1。
//
// 这不是"兼容垫片"：评测适配器与若干测试把 TextImage 当纯 NormalizeAnswer 用，
// 零值没有 profile。把它解释成 v1 是唯一不改变既有判分口径的选择；新代码必须
// 通过 NewTextImage / NewTextImageV2 明确选择 profile。
func (g TextImage) effectiveProfile() textImageProfile {
	if g.profile.id == "" {
		return textImageV1Profile
	}
	return g.profile
}

func (g TextImage) ID() string      { return g.effectiveProfile().id }
func (g TextImage) Version() string { return g.effectiveProfile().version }
func (TextImage) Media() string     { return MediaImage }

// TextImageAnswerFrom 从熵流里按**与生产 Generate 完全相同的抽样顺序**取出答案。
//
// 它存在的唯一理由是离线评测：样本必须来自生产分布，否则"评的是另一套东西"。
// 它**不构成答案预言机**：不读库、不读 token、不接受 verifier，只消费调用方自己持有的熵流；
// 生产路径的熵来自 crypto/rand，没有人能提前知道下一段流。
// 一致性由 TestTextImageAnswerFromMatchesGenerate 钉住（同一流取出的答案 == Generate 判分用的答案）。
func TextImageAnswerFrom(entropy io.Reader) (string, error) {
	return textImageAnswerFrom(textImageV1Profile, entropy)
}

// TextImageV2AnswerFrom 是 v2 的离线评测入口；与生产 Generate 使用同一 profile 和抽样顺序。
func TextImageV2AnswerFrom(entropy io.Reader) (string, error) {
	return textImageAnswerFrom(textImageV2Profile, entropy)
}

// TextImageV3AnswerFrom 是 v3 的离线评测入口；与生产 Generate 使用同一 profile 和抽样顺序。
func TextImageV3AnswerFrom(entropy io.Reader) (string, error) {
	return textImageAnswerFrom(textImageV3Profile, entropy)
}

func textImageAnswerFrom(profile textImageProfile, entropy io.Reader) (string, error) {
	out := make([]byte, 0, profile.length)
	for range profile.length {
		i, err := drawInt(entropy, 0, int64(len(profile.charset))-1)
		if err != nil {
			return "", err
		}
		out = append(out, profile.charset[i])
	}
	return string(out), nil
}

// Generate 出一道视觉题。答案由**返回的 Question 之外**的部分（verifier 的 HMAC）承担判分，
// 因此答案串在这里生成后就被丢弃（红线 5）。包内要拿答案走 generateWithAnswer。
func (g TextImage) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	_, q, err := g.generateWithAnswer(ctx, p, entropy)
	return q, err
}

func (g TextImage) generateWithAnswer(ctx context.Context, p Params, entropy io.Reader) (string, Question, error) {
	g.profile = g.effectiveProfile()
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return "", Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	ip, ok := g.profile.tiers[p.Difficulty]
	if !ok {
		return "", Question{}, fmt.Errorf("%w: 难度 %d 没有对应的 %s 档位", ErrBadParams, p.Difficulty, g.profile.id)
	}
	if err := ip.validate(); err != nil {
		return "", Question{}, err
	}
	if err := ctx.Err(); err != nil {
		return "", Question{}, err
	}

	answer, err := textImageAnswerFrom(g.profile, entropy)
	if err != nil {
		return "", Question{}, err
	}
	nonce := make([]byte, verifierNonceSz)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return "", Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	// 图形抖动（旋转角、基线位移、干扰线、噪点位置）用**每次独立播种**的 math/rand（红线 10）：
	// 它不参与答案与判分，但必须每次不同，否则同答案的题面在字节上完全一样，
	// 那会把"同一题面重复出现"变成常态。种子本身来自调用方给的 CSPRNG 流。
	seed := make([]byte, jitterSeedBytes)
	if _, err := io.ReadFull(entropy, seed); err != nil {
		return "", Question{}, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	jitter := mrand.New(mrand.NewSource(int64(binary.BigEndian.Uint64(seed))))

	font, err := loadGoRegular()
	if err != nil {
		return "", Question{}, fmt.Errorf("generator: 内嵌字体解析失败: %w", err)
	}
	face, err := opentype.NewFace(font, &opentype.FaceOptions{
		Size: ip.FontSize, DPI: ip.DPI, Hinting: xfont.HintingFull,
	})
	if err != nil {
		return "", Question{}, fmt.Errorf("generator: face: %w", err)
	}
	defer func() { _ = face.Close() }()

	// 布局重试：一次失败的具体原因是"旋转后的包围盒出界"或"挤在一起超过 1/4"。
	// 重试只重画抖动（不重抽答案！），所以答案与 verifier 在最外层确定一次。
	var lastLayoutErr error
	for range layoutRetries {
		if err := ctx.Err(); err != nil {
			return "", Question{}, err
		}
		placed, err := layoutGlyphs(face, answer, ip, jitter)
		if err != nil {
			lastLayoutErr = err
			continue
		}
		asset, err := renderCanvas(ip, placed, jitter)
		if err != nil {
			if errors.Is(err, ErrRenderTooLarge) {
				// 档位决定的结果：重试不会变小，直接上抛（并把档位写进错误类别，便于诊断）
				return "", Question{}, err
			}
			lastLayoutErr = err
			continue
		}
		q := Question{
			Payload: TextImagePublic{
				Length:       g.profile.length,
				Charset:      g.profile.charset,
				AnswerType:   "text",
				MaxAnswerLen: g.profile.length,
				ImageWidth:   ip.Width,
				ImageHeight:  ip.Height,
				Tolerant:     g.profile.tolerance,
			},
			Verifier: PrivateVerifier(textImageVerifier(g.macKey, nonce, answer, g.profile.tolerance)),
			Asset:    asset,
			MimeType: MimeTypePNG,
		}
		return answer, q, nil
	}
	if lastLayoutErr != nil && !errors.Is(lastLayoutErr, ErrGenerateExhausted) {
		return "", Question{}, fmt.Errorf("%w: %v", ErrGenerateExhausted, lastLayoutErr)
	}
	return "", Question{}, ErrGenerateExhausted
}

// --- 字形布局 ---

// placedGlyph 是一个已经定好位置与旋转角的字符。
//
// tile 与 dr **同一个坐标空间**（tile 的 Min 就是 dr 的 Min），所以"tile 坐标 → 画布坐标"
// 在未旋转时是恒等映射，旋转矩阵只需要绕 dr 中心转 —— 少一处容易写错的偏移换算。
type placedGlyph struct {
	ch    rune
	dot   image.Point
	angle float64 // 弧度
	shear float64 // 水平剪切
	sx    float64 // 水平缩放
	sy    float64 // 垂直缩放
	base  image.Rectangle
	dr    image.Rectangle
	tile  *image.RGBA
	// alpha 是字形 mask 的独立副本，只给 v3 的字形层后处理用（v1/v2 为 nil）。
	// 不能留到下一次 face.Glyph 调用之后再取：mask 会被下一次调用改写。
	alpha *image.Alpha
	box   image.Rectangle // 变换后在画布上的包围盒（用于不越界判定）
	ink   uint8
}

// layoutGlyphs 逐字定位。返回的每个 placed.box 一定满足：
//   - 完整落在画布内且离边缘 ≥ glyphEdgeMargin（**字符被裁切这条路径不存在**）；
//   - 与前一个字的水平重叠 ≤ 较窄者的 1/4（**字符挤在一起这条路径不存在**）。
//
// 做不到就返回错误，由外层**有界**重试处理。
//
// 旋转角在**测量时抽一次**并带到绘制阶段：布局保证是针对这个角算出来的，
// 如果绘制时再抽一个角，"保证不裁切"就变成了"大概不裁切"。
func layoutGlyphs(face xfont.Face, answer string, ip ImageParams, jitter *mrand.Rand) ([]placedGlyph, error) {
	// 第一遍：以 dot=(0,0) 测量每个字形的变换后包围盒与前进宽度（不取 mask）。
	type probe struct {
		rel           image.Rectangle // 按本字变换算出的包围盒（相对 dot）
		adv           int
		angle         float64
		shear, sx, sy float64
	}
	var probes []probe
	for _, r := range answer {
		dr, _, _, advance, ok := face.Glyph(fixed.P(0, 0), r)
		if !ok || dr.Empty() {
			return nil, fmt.Errorf("%w: %q", ErrFontGlyphMissing, r)
		}
		angle, shear, sx, sy := glyphTransform(ip, jitter)
		rel := transformRect(dr, angle, shear, sx, sy)
		adv := int(math.Round(float64(advance.Round()) * sx))
		probes = append(probes, probe{rel: rel, adv: adv, angle: angle, shear: shear, sx: sx, sy: sy})
	}

	// 起点与基线**由测到的几何算出来**，而不是抄一个"看着居中"的公式：
	// 字形的左部衬垫可以是负数（旋转还会再撑开包围盒），固定起点会让首字偶尔压到边缘，
	// 那种偶发失败在 128 次重试里表现为"5% 的补题白做"。
	// 竖直方向把基线放到"允许区间的正中"，于是上下两侧各留一半余量给基线抖动。
	edge := ip.margin()
	upExtent, downExtent, leftExtent := 0, 0, 0
	for _, p := range probes {
		upExtent = max(upExtent, -p.rel.Min.Y)
		downExtent = max(downExtent, p.rel.Max.Y)
		leftExtent = max(leftExtent, -p.rel.Min.X)
	}
	pen := edge + leftExtent
	baselineLo, baselineHi := edge+upExtent, ip.Height-edge-downExtent
	if baselineHi < baselineLo {
		return nil, fmt.Errorf("generator: 画布高 %d 放不下字形升降部（需要 %d..%d）",
			ip.Height, baselineLo, baselineHi)
	}
	baseline := (baselineLo + baselineHi) / 2
	// v3 的 6 个字符在 480px 宽画布上左对齐会留下大半张空白，既难看也把字形挤在
	// 干扰线最密的一侧。只对 v3 做近似居中（v1/v2 的布局与字节形状保持不变）。
	// 估算宽度加上字间抖动上界，宁可靠左一点也不要让末字压到右边缘。
	if ip.style.abstract() {
		total := 0
		for _, p := range probes {
			total += p.adv
		}
		if start := (ip.Width - total - len(probes)*ip.SpacingJitter) / 2; start > pen {
			pen = start
		}
	}

	// 第二遍：从左到右排，必要时**强制**推开（推开的量不受 SpacingJitter 上界约束，
	// 但整体仍受画布宽度约束——放不下就重试）。
	out := make([]placedGlyph, 0, len(probes))
	for i, r := range []rune(answer) {
		p := probes[i]
		dx := 0
		if ip.SpacingJitter > 0 {
			dx = jitter.Intn(ip.SpacingJitter + 1)
		}
		// v2 主动向左推：字间重叠是"切不开"的来源，而不是靠随机间距碰运气。
		if ip.style.OverlapPx > 0 {
			dx -= jitter.Intn(ip.style.OverlapPx + 1)
		}
		dy := 0
		if ip.BaselineJitter > 0 {
			dy = jitter.Intn(2*ip.BaselineJitter+1) - ip.BaselineJitter
		}
		x := pen + dx
		wave := waveOffset(ip.style, x)
		box := p.rel.Add(image.Pt(x, baseline+dy+wave))
		if len(out) > 0 {
			prev := out[len(out)-1].box
			// 允许的重叠量由档位决定：v1 是较窄字形宽度的 1/4，v2 可以到 1/1.4。
			narrow := min(prev.Dx(), box.Dx())
			allowed := overlapAllowed(narrow, ip.style.OverlapDivisor)
			if need := prev.Max.X - allowed - box.Min.X; need > 0 {
				x += need
				wave = waveOffset(ip.style, x)
				box = p.rel.Add(image.Pt(x, baseline+dy+wave))
			}
		}
		if box.Min.X < edge || box.Max.X > ip.Width-edge ||
			box.Min.Y < edge || box.Max.Y > ip.Height-edge {
			return nil, fmt.Errorf("generator: 字形 %q 放不进 %dx%d（box=%v）", r, ip.Width, ip.Height, box)
		}
		out = append(out, placedGlyph{
			ch: r, dot: image.Pt(x, baseline+dy+wave), angle: p.angle,
			shear: p.shear, sx: p.sx, sy: p.sy, base: p.rel, box: box,
		})
		pen = x + p.adv
	}

	// 第三遍：按最终 dot 取 mask 并画进 tile（mask 会被下一次 Glyph 调用改写，所以立刻画掉）。
	for i := range out {
		g := &out[i]
		dr, mask, maskp, _, ok := face.Glyph(fixed.P(g.dot.X, g.dot.Y), g.ch)
		if !ok || dr.Empty() {
			return nil, fmt.Errorf("%w: %q", ErrFontGlyphMissing, g.ch)
		}
		// 绘制必须与测量一致：把测量得到的相对包围盒平移到最终 dot，应当等于这次算出的包围盒。
		// 不相等说明 face 的行为与这里的假设不同 —— 那时"不越界/不重叠"的保证已经失效，
		// 宁可整次布局失败（外层有界重试，最终 ErrGenerateExhausted），也不发一张可能被裁切的图。
		got := transformRect(dr, g.angle, g.shear, g.sx, g.sy)
		if got != g.base.Add(g.dot) || got != g.box {
			return nil, fmt.Errorf("generator: 字形 %q 绘制包围盒 %v 与测量 %v 不一致", g.ch, got, g.box)
		}
		g.dr = dr
		g.ink = inkGray
		if ip.style.InkSpread > 0 {
			g.ink = uint8(clampInt(int(inkGray)+jitter.Intn(ip.style.InkSpread+1), 0, 255))
		}
		// tile 以**原点**建图：实测 golang.org/x/image/draw 的 Transform 只按 sr 的
		// 尺寸取源、不采纳 sr.Min（identity 矩阵下会把 (10,20) 的块画到 (10,10)）。
		// 因此平移量必须显式写进矩阵，源矩形一律从 (0,0) 开始。
		g.tile = image.NewRGBA(image.Rectangle{Max: dr.Size()})
		xdraw.DrawMask(g.tile, g.tile.Bounds(), image.NewUniform(color.Gray{Y: g.ink}),
			image.Point{}, mask, maskp, xdraw.Over)
		if ip.style.abstract() {
			// 独立副本：mask 是 face 的内部缓冲，下一次 Glyph 调用就会改写它。
			g.alpha = image.NewAlpha(g.tile.Bounds())
			xdraw.Draw(g.alpha, g.alpha.Bounds(), mask, maskp, xdraw.Src)
		}
	}
	return out, nil
}

// glyphTransform 在档位/风格的上界内抽一次单字变换。
// v1 的 style 是零值，因此这里只消费一次 Float64（与旧 angleRad 完全一致），
// 不改变 v1 的确定性熵流顺序。
func glyphTransform(ip ImageParams, jitter *mrand.Rand) (angle, shear, sx, sy float64) {
	maxDeg := ip.MaxRotationDeg
	if ip.style.RotationDeg > 0 {
		maxDeg = ip.style.RotationDeg
	}
	angle = angleRadDeg(maxDeg, jitter)
	if ip.style.Shear != 0 {
		shear = (jitter.Float64()*2 - 1) * ip.style.Shear
	}
	sx, sy = 1, 1
	if ip.style.ScaleMax > 0 {
		lo, hi := ip.style.ScaleMin, ip.style.ScaleMax
		sx = lo + jitter.Float64()*(hi-lo)
		sy = lo + jitter.Float64()*(hi-lo)
	}
	return angle, shear, sx, sy
}

func angleRadDeg(maxDeg float64, jitter *mrand.Rand) float64 {
	if maxDeg <= 0 {
		return 0
	}
	deg := (jitter.Float64()*2 - 1) * maxDeg
	return deg * math.Pi / 180
}

func waveOffset(style textImageStyle, x int) int {
	if style.WaveAmplitude <= 0 || style.WavePeriod <= 0 {
		return 0
	}
	return int(math.Round(math.Sin(float64(x)/style.WavePeriod*2*math.Pi) * float64(style.WaveAmplitude)))
}

func overlapAllowed(narrow int, divisor float64) int {
	if divisor <= 0 {
		divisor = 4
	}
	return int(float64(narrow) / divisor)
}

// transformRect 返回矩形绕自身中心做"缩放 → 剪切 → 旋转"后的整数包围盒。
// v1 的 scale=1、shear=0 时与旧的 rotateRect 逐位一致。
func transformRect(r image.Rectangle, angle, shear, sx, sy float64) image.Rectangle {
	cx := float64(r.Min.X) + float64(r.Dx())/2
	cy := float64(r.Min.Y) + float64(r.Dy())/2
	m := glyphAbout(cx, cy, angle, shear, sx, sy, image.Pt(0, 0))
	corners := []image.Point{r.Min, image.Pt(r.Max.X, r.Min.Y), r.Max, image.Pt(r.Min.X, r.Max.Y)}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range corners {
		fx, fy := applyAff3(m, float64(p.X), float64(p.Y))
		minX = math.Min(minX, fx)
		minY = math.Min(minY, fy)
		maxX = math.Max(maxX, fx)
		maxY = math.Max(maxY, fy)
	}
	return image.Rect(int(math.Floor(minX)), int(math.Floor(minY)),
		int(math.Ceil(maxX))+1, int(math.Ceil(maxY))+1)
}

// glyphAbout 构造 src→dst 仿射矩阵：先把源点相对 tile 中心缩放/剪切，再平移 t，
// 最后绕 (cx,cy) 旋转。
//
// 与旧 rotationAbout 的关系：scale=1、shear=0 时逐位相等（有 v1 测试钉住）。
// golang.org/x/image/draw 的 Transformer 明确 m 把**源坐标**映到**目标坐标**。
func glyphAbout(cx, cy, angle, shear, sx, sy float64, t image.Point) f64.Aff3 {
	sin, cos := math.Sincos(angle)
	tx, ty := float64(t.X), float64(t.Y)
	return f64.Aff3{
		cos * sx,
		cos*shear*sy - sin*sy,
		cx + cos*tx - sin*ty - cos*sx*cx - (cos*shear-sin)*sy*cy,
		sin * sx,
		sin*shear*sy + cos*sy,
		cy + sin*tx + cos*ty - sin*sx*cx - (sin*shear+cos)*sy*cy,
	}
}

// rotateRect / rotationAbout 保留为 v1 的兼容包装；新代码用 transformRect / glyphAbout。
func rotateRect(r image.Rectangle, angle float64) image.Rectangle {
	return transformRect(r, angle, 0, 1, 1)
}

func rotationAbout(cx, cy, angle float64, t image.Point) f64.Aff3 {
	return glyphAbout(cx, cy, angle, 0, 1, 1, t)
}

func applyAff3(m f64.Aff3, x, y float64) (float64, float64) {
	return m[0]*x + m[1]*y + m[2], m[3]*x + m[4]*y + m[5]
}

// --- 画布 ---

// renderCanvas 画纸面、字形（带旋转）、干扰线、噪点，返回 PNG 字节。
//
// 画布用 **image.Gray**（单通道）而不是 RGBA：同一尺寸的裸数据小 4 倍，PNG 体积也随之下降，
// 而视觉题不需要颜色。抖动与抗识别都不依赖色彩。
//
// v1/v2 的 style 是零值，走下面这条**原样保留**的路径；v3 走 renderAbstractCanvas。
// 分支条件只由 style 决定，所以"v1/v2 的字节形状不变"是代码结构上的事实，不靠测试碰巧覆盖。
func renderCanvas(ip ImageParams, glyphs []placedGlyph, jitter *mrand.Rand) ([]byte, error) {
	if ip.style.abstract() {
		return renderAbstractCanvas(ip, glyphs, jitter)
	}

	img := image.NewGray(image.Rect(0, 0, ip.Width, ip.Height))
	xdraw.Draw(img, img.Bounds(), image.NewUniform(color.Gray{Y: paperGray}), image.Point{}, xdraw.Src)

	for _, g := range glyphs {
		// 旋转中心取 tile 自身的中心（源从原点建图），平移量就是字形在画布上的位置。
		cx := float64(g.tile.Bounds().Dx()) / 2
		cy := float64(g.tile.Bounds().Dy()) / 2
		drawGlyph := func(offset image.Point) {
			m := glyphAbout(cx, cy, g.angle, g.shear, g.sx, g.sy, g.dr.Min.Add(offset))
			// ApproxBiLinear：升采样用双线性、降采样用 4 邻域混合。旋转以升采样为主，
			// 可读性优先的质量足够，而 CatmullRom 慢一档；单字 tile 只有几十乘几十像素。
			xdraw.ApproxBiLinear.Transform(img, m, g.tile, g.tile.Bounds(), xdraw.Over, nil)
		}
		if ip.style.Stroke <= 0 {
			drawGlyph(image.Pt(0, 0))
			continue
		}
		// 加粗用 5 个方向的叠加，而不是逐像素膨胀：像素量与边缘质量都可控。
		drawGlyph(image.Pt(0, 0))
		for _, o := range []image.Point{
			{X: -ip.style.Stroke}, {X: ip.style.Stroke},
			{Y: -ip.style.Stroke}, {Y: ip.style.Stroke},
		} {
			drawGlyph(o)
		}
	}

	drawRandomLines(img, ip, jitter, ip.Lines, lineGray)
	// v2 的穿越线画在字形之后：它确实切断字符连通域，而不是只做背景纹理。
	drawRandomLines(img, ip, jitter, ip.style.CrossLines, lineCrossGray)

	for range ip.Noise {
		x, y := jitter.Intn(ip.Width), jitter.Intn(ip.Height)
		v := int(paperGray) - jitter.Intn(noiseGraySpread+1)
		img.SetGray(x, y, color.Gray{Y: uint8(clampInt(v, 0, 255))})
	}
	return encodeGrayPNG(img)
}

// minInkRetainPct 是 v3 后处理允许保留的最小字墨覆盖率（百分比）。
// 它证明的是"字没有被模糊/擦除吃掉"，不是"人能读出来"——后者仍须人工验收。
const minInkRetainPct = 55

// renderAbstractCanvas 是 v3 的渲染路径：字形先画进独立的 Alpha 层，
// 在层上做正弦位移、加权模糊、稀疏擦除，再按墨色合成到纸面。
//
// 为什么不直接在全画布上做模糊/位移：那会把干扰线与纸面纹理一起糊掉，
// "只模糊字"和"糊整张图"是两种视觉结果；后者还会让背景线变成灰雾，降低可读性。
func renderAbstractCanvas(ip ImageParams, glyphs []placedGlyph, jitter *mrand.Rand) ([]byte, error) {
	layer := image.NewAlpha(image.Rect(0, 0, ip.Width, ip.Height))
	for _, g := range glyphs {
		if g.alpha == nil {
			continue
		}
		cx := float64(g.alpha.Bounds().Dx()) / 2
		cy := float64(g.alpha.Bounds().Dy()) / 2
		drawGlyph := func(offset image.Point) {
			m := glyphAbout(cx, cy, g.angle, g.shear, g.sx, g.sy, g.dr.Min.Add(offset))
			xdraw.ApproxBiLinear.Transform(layer, m, g.alpha, g.alpha.Bounds(), xdraw.Over, nil)
		}
		drawGlyph(image.Pt(0, 0))
		for i := 0; i < ip.style.Stroke; i++ {
			for _, o := range []image.Point{{X: -i - 1}, {X: i + 1}, {Y: -i - 1}, {Y: i + 1}} {
				drawGlyph(o)
			}
		}
	}

	origInk := alphaSum(layer)
	if origInk == 0 {
		return nil, fmt.Errorf("%w: 字形层为空", ErrGenerateExhausted)
	}
	layer = warpAlpha(layer, ip.style.WarpAmp, ip.style.WarpPeriod, jitter)
	for i := 0; i < ip.style.BlurPasses; i++ {
		layer = blurAlpha3x3(layer)
	}
	eraseAlphaDots(layer, glyphs, ip.style.EraseDots, jitter)
	if kept := alphaSum(layer); kept*100 < origInk*minInkRetainPct {
		return nil, fmt.Errorf("%w: 字墨覆盖率 %d%% 低于 %d%%",
			ErrGenerateExhausted, kept*100/origInk, minInkRetainPct)
	}

	img := image.NewGray(image.Rect(0, 0, ip.Width, ip.Height))
	paper := uint8(paperGray)
	if ip.style.PaperGray > 0 {
		paper = uint8(ip.style.PaperGray)
	}
	fillPaperTexture(img, paper, ip.style.PaperTexture, jitter)
	drawRandomLines(img, ip, jitter, ip.Lines, lineGray)
	for range ip.Noise {
		x, y := jitter.Intn(ip.Width), jitter.Intn(ip.Height)
		v := int(paper) - jitter.Intn(noiseGraySpread+1)
		img.SetGray(x, y, color.Gray{Y: uint8(clampInt(v, 0, 255))})
	}

	ink := uint8(inkGray)
	if ip.style.InkGray > 0 {
		ink = uint8(ip.style.InkGray)
	}
	xdraw.DrawMask(img, img.Bounds(), image.NewUniform(color.Gray{Y: ink}), image.Point{},
		layer, image.Point{}, xdraw.Over)
	// 穿越线最后画：它切断字形的连通域，是 v3 与 v2 共有的"反切片"手段。
	drawRandomLines(img, ip, jitter, ip.style.CrossLines, lineCrossGray)
	return encodeGrayPNG(img)
}

func encodeGrayPNG(img *image.Gray) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("generator: PNG 编码: %w", err)
	}
	out := buf.Bytes()
	if len(out) > MaxImageAssetBytes {
		return nil, fmt.Errorf("%w: %d 字节（上限 %d）", ErrRenderTooLarge, len(out), MaxImageAssetBytes)
	}
	return out, nil
}

func alphaSum(img *image.Alpha) uint64 {
	var sum uint64
	for _, v := range img.Pix {
		sum += uint64(v)
	}
	return sum
}

// warpAlpha 对字形层做平滑正弦位移：sx = x + amp·sin(2πy/period + φx)，
// sy = y + 0.6·amp·sin(2πx/(1.35·period) + φy)。两个方向周期不同，
// 避免位移场退化成一条整齐的波浪（那反而更容易被线性校正）。
func warpAlpha(src *image.Alpha, amp int, period float64, rng *mrand.Rand) *image.Alpha {
	if amp <= 0 || period <= 0 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	sxTab := make([]float64, w)
	syTab := make([]float64, h)
	phaseX, phaseY := rng.Float64()*2*math.Pi, rng.Float64()*2*math.Pi
	for x := range sxTab {
		sxTab[x] = math.Sin(2*math.Pi*float64(x)/(period*1.35) + phaseY)
	}
	for y := range syTab {
		syTab[y] = math.Sin(2*math.Pi*float64(y)/period + phaseX)
	}
	dst := image.NewAlpha(b)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx := float64(x) + float64(amp)*syTab[y]
			dy := float64(y) + 0.6*float64(amp)*sxTab[x]
			dst.SetAlpha(x, y, color.Alpha{A: sampleAlphaBilinear(src, dx, dy)})
		}
	}
	return dst
}

func sampleAlphaBilinear(img *image.Alpha, x, y float64) uint8 {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := x-float64(x0), y-float64(y0)
	b := img.Bounds()
	at := func(xx, yy int) float64 {
		if xx < b.Min.X || xx >= b.Max.X || yy < b.Min.Y || yy >= b.Max.Y {
			return 0
		}
		return float64(img.AlphaAt(xx, yy).A)
	}
	top := at(x0, y0)*(1-fx) + at(x0+1, y0)*fx
	bottom := at(x0, y0+1)*(1-fx) + at(x0+1, y0+1)*fx
	return uint8(clampInt(int(top*(1-fy)+bottom*fy+0.5), 0, 255))
}

// blurAlpha3x3 是 3×3 加权模糊（核 1-2-1，近似高斯）。不用 9 格等权盒式模糊：
// 等权把 4px 笔画的中轴也拉掉一截，低对比下会直接变成"看不清"；加权核保留中心，
// 只软化边缘——同样降低 OCR 的边沿信息，但让人眼仍有可辨认的笔画主体。
var blurKernel3x3 = [3][3]int{
	{1, 2, 1},
	{2, 4, 2},
	{1, 2, 1},
}

func blurAlpha3x3(src *image.Alpha) *image.Alpha {
	b := src.Bounds()
	dst := image.NewAlpha(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			sum, weight := 0, 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					xx, yy := x+dx, y+dy
					if xx < b.Min.X || xx >= b.Max.X || yy < b.Min.Y || yy >= b.Max.Y {
						continue
					}
					w := blurKernel3x3[dy+1][dx+1]
					sum += w * int(src.AlphaAt(xx, yy).A)
					weight += w
				}
			}
			if weight > 0 {
				dst.SetAlpha(x, y, color.Alpha{A: uint8(sum / weight)})
			}
		}
	}
	return dst
}

// eraseAlphaDots 在字形包围盒内点掉少量像素。点只落在字形上，不会在空白处
// 造出"看起来像笔画"的孤立点（那会把题面变成噪声题）。
func eraseAlphaDots(layer *image.Alpha, glyphs []placedGlyph, count int, rng *mrand.Rand) {
	if count <= 0 || len(glyphs) == 0 {
		return
	}
	b := layer.Bounds()
	for i := 0; i < count; i++ {
		g := glyphs[rng.Intn(len(glyphs))]
		box := g.box.Intersect(b)
		if box.Empty() {
			continue
		}
		x := box.Min.X + rng.Intn(box.Dx())
		y := box.Min.Y + rng.Intn(box.Dy())
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				if dx*dx+dy*dy > 2 {
					continue
				}
				xx, yy := x+dx, y+dy
				if xx >= b.Min.X && xx < b.Max.X && yy >= b.Min.Y && yy < b.Max.Y {
					layer.SetAlpha(xx, yy, color.Alpha{A: 0})
				}
			}
		}
	}
}

func fillPaperTexture(img *image.Gray, paper uint8, amp int, rng *mrand.Rand) {
	b := img.Bounds()
	if amp <= 0 {
		xdraw.Draw(img, b, image.NewUniform(color.Gray{Y: paper}), image.Point{}, xdraw.Src)
		return
	}
	w, h := b.Dx(), b.Dy()
	tx := make([]float64, w)
	ty := make([]float64, h)
	periodX, periodY := 80+rng.Float64()*60, 50+rng.Float64()*40
	phaseX, phaseY := rng.Float64()*2*math.Pi, rng.Float64()*2*math.Pi
	for x := range tx {
		tx[x] = math.Sin(2*math.Pi*float64(x)/periodX + phaseX)
	}
	for y := range ty {
		ty[y] = math.Sin(2*math.Pi*float64(y)/periodY + phaseY)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := int(paper) + int(float64(amp)*tx[x]*ty[y])
			img.SetGray(b.Min.X+x, b.Min.Y+y, color.Gray{Y: uint8(clampInt(v, 0, 255))})
		}
	}
}

// drawRandomLines 画 count 条有界折线。v1 的 count=ip.Lines、颜色 lineGray，
// 调用顺序与旧代码相同；v2 用同一个函数画穿越线，避免两份容易漂移的折线生成器。
func drawRandomLines(img *image.Gray, ip ImageParams, jitter *mrand.Rand, count int, v uint8) {
	for range count {
		pts := make([]image.Point, 0, ip.LineSegments+1)
		x, y := jitter.Intn(ip.Width), jitter.Intn(ip.Height)
		pts = append(pts, image.Pt(x, y))
		for range ip.LineSegments {
			// 每段位移有界（±1/3 画布），保证折线整体还落在图内、且不会退化成一个点
			dx := jitter.Intn(ip.Width/3) - ip.Width/6
			dy := jitter.Intn(ip.Height/3) - ip.Height/6
			x, y = clampInt(x+dx, 0, ip.Width-1), clampInt(y+dy, 0, ip.Height-1)
			pts = append(pts, image.Pt(x, y))
		}
		for i := 0; i+1 < len(pts); i++ {
			drawSegment(img, pts[i], pts[i+1], v)
		}
	}
}

// drawSegment 画一条粗细 lineThickness 的线段。
//
// 用参数化采样而不是 Bresenham：后者的两种常见符号约定（err=dx−dy 配 e2>−dy，
// 与 err=dx+dy 配 dy 为负）很容易混用其中一种而写成"陡线越过终点后永不相等"的**死循环**
// —— 本轮实测就挂在这里。采样式的循环次数有显式上界 max(|dx|,|dy|)+1，
// 终止性一眼可见，而干扰线不需要像素级最优的直线质量。
// 自己画而不引 x/image/vector 的栅格化器：十几行、零依赖，且所有像素访问都走
// setGrayBounded（越界不会 panic）。
func drawSegment(img *image.Gray, p0, p1 image.Point, v uint8) {
	steps := max(absInt(p1.X-p0.X), absInt(p1.Y-p0.Y))
	if steps == 0 {
		setGrayBounded(img, p0.X, p0.Y, v)
		return
	}
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x := int(math.Round(float64(p0.X) + t*float64(p1.X-p0.X)))
		y := int(math.Round(float64(p0.Y) + t*float64(p1.Y-p0.Y)))
		for d := 0; d < lineThickness; d++ {
			setGrayBounded(img, x, y+d, v)
		}
	}
}

func setGrayBounded(img *image.Gray, x, y int, v uint8) {
	if x < img.Bounds().Min.X || x >= img.Bounds().Max.X ||
		y < img.Bounds().Min.Y || y >= img.Bounds().Max.Y {
		return
	}
	img.SetGray(x, y, color.Gray{Y: v})
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// --- 输入规范与判分 ---

// NormalizeAnswer 定义"什么算一个合法答案串"：
//   - 裁首尾 ASCII 空白；含内部空白即畸形；
//   - **大小写不敏感**：字符集只有大写字母，把小写折成大写只是"同一答案的另一种写法"，
//     不该因此消耗掉唯一一次作答机会；
//   - 长度必须**恰好** TextImageLength：变长会把长度变成答案信息的一部分；
//   - 每个字符必须在字符集内：I/O/0/1 与任何非 ASCII（含全角字母、不可见字符）一律**畸形**。
//
// "畸形"与"答错"是两种结论（ADR-011 #3）：前者不终态化（400 invalid-request），
// 后者终结该 challenge。字符集是公开常量，所以"你输入的 O 不在字符集里"不泄露任何答案信息。
func (g TextImage) NormalizeAnswer(input string) (string, bool) {
	g.profile = g.effectiveProfile()
	s := trimASCII(input)
	if len(s) != g.profile.length {
		return "", false
	}
	out := make([]byte, 0, g.profile.length)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if strings.IndexByte(g.profile.charset, c) < 0 {
			return "", false
		}
		out = append(out, c)
	}
	return string(out), true
}

// textImageVerifier 按 profile 选择 verifier 构造：
// tolerance=0 ⇒ 48B 精确 HMAC（v1/v2）；tolerance>0 ⇒ 逐字标签（v3，ADR-024）。
// 这里不能"统一用逐字标签再要求 0 不匹配"：那会改变 v1/v2 的历史 verifier 形态与长度。
func textImageVerifier(macKey, nonce []byte, answer string, tolerance int) []byte {
	if tolerance > 0 {
		return charVerifier(macKey, nonce, answer)
	}
	return answerVerifier(macKey, nonce, answer)
}

// Verify 判分：v1/v2 与算术基线同一条 HMAC 路径（容差为零）；v3 用逐字标签，
// 允许最多 profile.tolerance 个字符替换。
func (g TextImage) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	g.profile = g.effectiveProfile()
	if g.profile.tolerance > 0 {
		return verifyChars(g.macKey, v, canon, g.profile.tolerance)
	}
	return verifyAnswer(g.macKey, v, canon)
}

// SanitizePublic 是题面出网的唯一通道：白名单之外**任何**键都让整次调用失败。
// 这里刻意不回显长度之外的任何东西：图片本身走 pool.asset_blob，不在 payload 里
// （一条 4KB 的 CHECK 装不下图，也不该装）。
func (g TextImage) SanitizePublic(raw []byte) (map[string]any, error) {
	g.profile = g.effectiveProfile()
	var p TextImagePublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch {
	case p.Length != g.profile.length:
		return nil, fmt.Errorf("generator: 题面 length=%d 与固定长度 %d 不符", p.Length, g.profile.length)
	case p.Charset != g.profile.charset:
		return nil, errors.New("generator: 题面 charset 与公开字符集不符")
	case p.AnswerType != "text":
		return nil, fmt.Errorf("generator: 题面 answer_type=%q 不合法", p.AnswerType)
	case p.MaxAnswerLen < 1 || p.MaxAnswerLen > 64:
		return nil, fmt.Errorf("generator: 题面 max_answer_len=%d 越界", p.MaxAnswerLen)
	case p.Tolerant != g.profile.tolerance:
		// 题面声明的容差必须与判分 profile 一致：否则库里的旧题面会被换一套判分口径。
		return nil, fmt.Errorf("generator: 题面 tolerant=%d 与 profile 的 %d 不符", p.Tolerant, g.profile.tolerance)
	}
	if p.ImageWidth < 40 || p.ImageWidth > maxImageWidth ||
		p.ImageHeight < 40 || p.ImageHeight > maxImageHeight ||
		p.ImageWidth*p.ImageHeight > maxImagePixels {
		return nil, fmt.Errorf("generator: 题面图像尺寸 %dx%d 越界", p.ImageWidth, p.ImageHeight)
	}
	for k := range generic {
		switch k {
		case "length", "charset", "answer_type", "max_answer_len", "image_width", "image_height", "tolerant":
		default:
			return nil, fmt.Errorf("generator: 题面含非白名单字段 %q", k)
		}
	}
	out := map[string]any{
		"kind":           p.PublicKind(),
		"length":         p.Length,
		"charset":        p.Charset,
		"answer_type":    p.AnswerType,
		"max_answer_len": p.MaxAnswerLen,
		"image_width":    p.ImageWidth,
		"image_height":   p.ImageHeight,
	}
	if p.Tolerant > 0 {
		out["tolerant"] = p.Tolerant
	}
	return out, nil
}
