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

	// TextImageLength 是**固定**答案长度。不做可变长度：长度本身会变成答案的一部分信息，
	// 归一化也要多一条"4..7 都算合法"的分支，而代价是 M3 变差（短答案的猜中率更高）。
	TextImageLength = 6

	// TextImageCharset 是公开常量：32 个字符，剔除肉眼易混的 I/1 与 O/0（本集合无小写，
	// 所以也不再排除 l）。它是题面的一部分（进 payload），**不是秘密**。
	TextImageCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

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
	}
	// 字形必须放得下：字号 × 6 个字符至少要有可辨认的宽度，否则布局重试会变成忙循环。
	if p.Width <= (glyphEdgeMargin+1)*2 {
		return fmt.Errorf("%w: 宽 %d 放不下任何字形", ErrBadParams, p.Width)
	}
	return nil
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
}

func (TextImagePublic) PublicKind() string { return "text-image" }

// TextImage 实现 Generator。无可变字段，可并发调用（字体在包级缓存里且只读）。
type TextImage struct {
	macKey []byte
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

// NewTextImage 构造视觉生成器，并在**登记阶段**就把两类事故挡在启动时：
//  1. 内嵌字体必须覆盖整个字符集（缺字形 ⇒ 题面永远答不对，而响应看起来一切正常）；
//  2. 五档渲染参数必须全部通过边界自检（档位表写错 ⇒ 现在失败，而不是第一次补题时）。
func NewTextImage(macKey []byte) (TextImage, error) {
	if len(macKey) < 32 {
		return TextImage{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
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
	for _, r := range TextImageCharset {
		dr, _, _, _, ok := face.Glyph(fixed.P(0, 0), r)
		if !ok || dr.Empty() {
			return TextImage{}, fmt.Errorf("%w: %q", ErrFontGlyphMissing, r)
		}
	}
	for d := 1; d <= MaxDifficulties; d++ {
		p, err := TextImageParams(d)
		if err != nil {
			return TextImage{}, err
		}
		if err := p.validate(); err != nil {
			return TextImage{}, err
		}
	}
	return TextImage{macKey: macKey}, nil
}

func (TextImage) ID() string      { return TextImageID }
func (TextImage) Version() string { return textImageVersion }
func (TextImage) Media() string   { return MediaImage }

// TextImageAnswerFrom 从熵流里按**与生产 Generate 完全相同的抽样顺序**取出答案。
//
// 它存在的唯一理由是离线评测：样本必须来自生产分布，否则"评的是另一套东西"。
// 它**不构成答案预言机**：不读库、不读 token、不接受 verifier，只消费调用方自己持有的熵流；
// 生产路径的熵来自 crypto/rand，没有人能提前知道下一段流。
// 一致性由 TestTextImageAnswerFromMatchesGenerate 钉住（同一流取出的答案 == Generate 判分用的答案）。
func TextImageAnswerFrom(entropy io.Reader) (string, error) {
	out := make([]byte, 0, TextImageLength)
	for range TextImageLength {
		i, err := drawInt(entropy, 0, int64(len(TextImageCharset))-1)
		if err != nil {
			return "", err
		}
		out = append(out, TextImageCharset[i])
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
	if p.Difficulty < 1 || p.Difficulty > MaxDifficulties {
		return "", Question{}, fmt.Errorf("%w: difficulty=%d", ErrBadParams, p.Difficulty)
	}
	ip, err := TextImageParams(p.Difficulty)
	if err != nil {
		return "", Question{}, err
	}
	if err := ip.validate(); err != nil {
		return "", Question{}, err
	}
	if err := ctx.Err(); err != nil {
		return "", Question{}, err
	}

	answer, err := TextImageAnswerFrom(entropy)
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
				Length:       TextImageLength,
				Charset:      TextImageCharset,
				AnswerType:   "text",
				MaxAnswerLen: TextImageLength,
				ImageWidth:   ip.Width,
				ImageHeight:  ip.Height,
			},
			Verifier: PrivateVerifier(answerVerifier(g.macKey, nonce, answer)),
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
	base  image.Rectangle
	dr    image.Rectangle
	tile  *image.RGBA
	box   image.Rectangle // 旋转后在画布上的包围盒（用于不越界判定）
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
	// 第一遍：以 dot=(0,0) 测量每个字形的旋转后包围盒与前进宽度（不取 mask）。
	type probe struct {
		rel   image.Rectangle // 按本字旋转角算出的包围盒（相对 dot）
		adv   int
		angle float64
	}
	var probes []probe
	for _, r := range answer {
		dr, _, _, advance, ok := face.Glyph(fixed.P(0, 0), r)
		if !ok || dr.Empty() {
			return nil, fmt.Errorf("%w: %q", ErrFontGlyphMissing, r)
		}
		angle := angleRad(ip, jitter)
		probes = append(probes, probe{rel: rotateRect(dr, angle), adv: advance.Round(), angle: angle})
	}

	// 起点与基线**由测到的几何算出来**，而不是抄一个"看着居中"的公式：
	// 字形的左部衬垫可以是负数（旋转还会再撑开包围盒），固定起点会让首字偶尔压到边缘，
	// 那种偶发失败在 128 次重试里表现为"5% 的补题白做"。
	// 竖直方向把基线放到"允许区间的正中"，于是上下两侧各留一半余量给基线抖动。
	upExtent, downExtent, leftExtent := 0, 0, 0
	for _, p := range probes {
		upExtent = max(upExtent, -p.rel.Min.Y)
		downExtent = max(downExtent, p.rel.Max.Y)
		leftExtent = max(leftExtent, -p.rel.Min.X)
	}
	pen := glyphEdgeMargin + leftExtent
	baselineLo, baselineHi := glyphEdgeMargin+upExtent, ip.Height-glyphEdgeMargin-downExtent
	if baselineHi < baselineLo {
		return nil, fmt.Errorf("generator: 画布高 %d 放不下字形升降部（需要 %d..%d）",
			ip.Height, baselineLo, baselineHi)
	}
	baseline := (baselineLo + baselineHi) / 2

	// 第二遍：从左到右排，必要时**强制**推开（推开的量不受 SpacingJitter 上界约束，
	// 但整体仍受画布宽度约束——放不下就重试）。
	out := make([]placedGlyph, 0, len(probes))
	for i, r := range []rune(answer) {
		p := probes[i]
		dx := 0
		if ip.SpacingJitter > 0 {
			dx = jitter.Intn(ip.SpacingJitter + 1)
		}
		dy := 0
		if ip.BaselineJitter > 0 {
			dy = jitter.Intn(2*ip.BaselineJitter+1) - ip.BaselineJitter
		}
		x := pen + dx
		box := p.rel.Add(image.Pt(x, baseline+dy))
		if len(out) > 0 {
			prev := out[len(out)-1].box
			// 允许的重叠量：较窄字形宽度的 1/4，向下取整。
			narrow := min(prev.Dx(), box.Dx())
			allowed := narrow / 4
			if need := prev.Max.X - allowed - box.Min.X; need > 0 {
				x += need
				box = p.rel.Add(image.Pt(x, baseline+dy))
			}
		}
		if box.Min.X < glyphEdgeMargin || box.Max.X > ip.Width-glyphEdgeMargin ||
			box.Min.Y < glyphEdgeMargin || box.Max.Y > ip.Height-glyphEdgeMargin {
			return nil, fmt.Errorf("generator: 字形 %q 放不进 %dx%d（box=%v）", r, ip.Width, ip.Height, box)
		}
		out = append(out, placedGlyph{
			ch: r, dot: image.Pt(x, baseline+dy), angle: p.angle, base: p.rel, box: box,
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
		got := rotateRect(dr, g.angle)
		if got != g.base.Add(g.dot) || got != g.box {
			return nil, fmt.Errorf("generator: 字形 %q 绘制包围盒 %v 与测量 %v 不一致", g.ch, got, g.box)
		}
		g.dr = dr
		// tile 以**原点**建图：实测 golang.org/x/image/draw 的 Transform 只按 sr 的
		// 尺寸取源、不采纳 sr.Min（identity 矩阵下会把 (10,20) 的块画到 (10,10)）。
		// 因此平移量必须显式写进矩阵，源矩形一律从 (0,0) 开始。
		g.tile = image.NewRGBA(image.Rectangle{Max: dr.Size()})
		xdraw.DrawMask(g.tile, g.tile.Bounds(), image.NewUniform(color.Gray{Y: inkGray}),
			image.Point{}, mask, maskp, xdraw.Over)
	}
	return out, nil
}

// angleRad 在档位的上界内取一个旋转角（弧度）。上界为 0 时恒等。
func angleRad(ip ImageParams, jitter *mrand.Rand) float64 {
	if ip.MaxRotationDeg <= 0 {
		return 0
	}
	deg := (jitter.Float64()*2 - 1) * ip.MaxRotationDeg
	return deg * math.Pi / 180
}

// rotateRect 返回矩形绕自身中心旋转后的**整数**包围盒（向外取整，宁可保守）。
func rotateRect(r image.Rectangle, angle float64) image.Rectangle {
	cx := float64(r.Min.X) + float64(r.Dx())/2
	cy := float64(r.Min.Y) + float64(r.Dy())/2
	m := rotationAbout(cx, cy, angle, image.Pt(0, 0))
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

// rotationAbout 构造 src→dst 仿射矩阵：先把源点平移 t，再绕 (cx,cy) 旋转 angle。
//
// golang.org/x/image/draw 的 Transformer 明确 m 把**源坐标**映到**目标坐标**
// （"(sx,sy) → (m00*sx + m01*sy + m02, m10*sx + m11*sy + m12)"）。字形 tile 从原点建图，
// 所以"tile 在画布上的未旋转位置"必须作为 t 传进来：
//
//	dst = c + R*(src + t − c)
//
// angle=0 时退化为纯平移 (t)，有测试钉住。
func rotationAbout(cx, cy, angle float64, t image.Point) f64.Aff3 {
	sin, cos := math.Sincos(angle)
	dx, dy := float64(t.X), float64(t.Y)
	return f64.Aff3{
		cos, -sin, cx + cos*(dx-cx) - sin*(dy-cy),
		sin, cos, cy + sin*(dx-cx) + cos*(dy-cy),
	}
}

func applyAff3(m f64.Aff3, x, y float64) (float64, float64) {
	return m[0]*x + m[1]*y + m[2], m[3]*x + m[4]*y + m[5]
}

// --- 画布 ---

// renderCanvas 画纸面、字形（带旋转）、干扰线、噪点，返回 PNG 字节。
//
// 画布用 **image.Gray**（单通道）而不是 RGBA：同一尺寸的裸数据小 4 倍，PNG 体积也随之下降，
// 而视觉题不需要颜色。抖动与抗识别都不依赖色彩。
func renderCanvas(ip ImageParams, glyphs []placedGlyph, jitter *mrand.Rand) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, ip.Width, ip.Height))
	xdraw.Draw(img, img.Bounds(), image.NewUniform(color.Gray{Y: paperGray}), image.Point{}, xdraw.Src)

	for _, g := range glyphs {
		// 旋转中心取 tile 自身的中心（源从原点建图），平移量就是字形在画布上的位置。
		cx := float64(g.tile.Bounds().Dx()) / 2
		cy := float64(g.tile.Bounds().Dy()) / 2
		m := rotationAbout(cx, cy, g.angle, g.dr.Min)
		// ApproxBiLinear：升采样用双线性、降采样用 4 邻域混合。旋转以升采样为主，
		// 可读性优先的质量足够，而 CatmullRom 慢一档；单字 tile 只有几十乘几十像素。
		xdraw.ApproxBiLinear.Transform(img, m, g.tile, g.tile.Bounds(), xdraw.Over, nil)
	}

	for range ip.Lines {
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
			drawSegment(img, pts[i], pts[i+1], lineGray)
		}
	}

	for range ip.Noise {
		x, y := jitter.Intn(ip.Width), jitter.Intn(ip.Height)
		v := int(paperGray) - jitter.Intn(noiseGraySpread+1)
		img.SetGray(x, y, color.Gray{Y: uint8(clampInt(v, 0, 255))})
	}

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
func (TextImage) NormalizeAnswer(input string) (string, bool) {
	s := trimASCII(input)
	if len(s) != TextImageLength {
		return "", false
	}
	out := make([]byte, 0, TextImageLength)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if strings.IndexByte(TextImageCharset, c) < 0 {
			return "", false
		}
		out = append(out, c)
	}
	return string(out), true
}

// Verify 判分：与算术基线**同一条** HMAC 路径（generator.go），容差为零——
// 字符序列要么等于要么不等于，没有"差不多"的余量，因此 M2 的接受域就是那一个串。
func (g TextImage) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	return verifyAnswer(g.macKey, v, canon)
}

// SanitizePublic 是题面出网的唯一通道：白名单之外**任何**键都让整次调用失败。
// 这里刻意不回显长度之外的任何东西：图片本身走 pool.asset_blob，不在 payload 里
// （一条 4KB 的 CHECK 装不下图，也不该装）。
func (TextImage) SanitizePublic(raw []byte) (map[string]any, error) {
	var p TextImagePublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch {
	case p.Length != TextImageLength:
		return nil, fmt.Errorf("generator: 题面 length=%d 与固定长度 %d 不符", p.Length, TextImageLength)
	case p.Charset != TextImageCharset:
		return nil, errors.New("generator: 题面 charset 与公开字符集不符")
	case p.AnswerType != "text":
		return nil, fmt.Errorf("generator: 题面 answer_type=%q 不合法", p.AnswerType)
	case p.MaxAnswerLen < 1 || p.MaxAnswerLen > 64:
		return nil, fmt.Errorf("generator: 题面 max_answer_len=%d 越界", p.MaxAnswerLen)
	}
	if p.ImageWidth < 40 || p.ImageWidth > maxImageWidth ||
		p.ImageHeight < 40 || p.ImageHeight > maxImageHeight ||
		p.ImageWidth*p.ImageHeight > maxImagePixels {
		return nil, fmt.Errorf("generator: 题面图像尺寸 %dx%d 越界", p.ImageWidth, p.ImageHeight)
	}
	for k := range generic {
		switch k {
		case "length", "charset", "answer_type", "max_answer_len", "image_width", "image_height":
		default:
			return nil, fmt.Errorf("generator: 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind":           p.PublicKind(),
		"length":         p.Length,
		"charset":        p.Charset,
		"answer_type":    p.AnswerType,
		"max_answer_len": p.MaxAnswerLen,
		"image_width":    p.ImageWidth,
		"image_height":   p.ImageHeight,
	}, nil
}
