package generator

// text-image-v1 的测试。重点是三件只在"图片题"上才存在的新风险：
//   1. 渲染必须有界且**可读**（不裁切、不挤在一起）——用几何断言证明，不靠肉眼看一批；
//   2. 资产与公开 DTO 不得携带私有材料（答案串、种子、verifier）；
//   3. 生成/渲染失败必须是可诊断的失败，而不是"发一张空白或半成品题"。
//
// 测试熵源用 eval.NewDeterministic：**只用于测试与评测**。生产路径的熵由 pool 传 crypto/rand，
// 生成器自己没有任何"固定种子"回落分支。

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	mrand "math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/shijianus/EpoCHA/internal/eval"
)

const testMACKeyLen = 32

// --- 夹具 ---

func newTestTextImage(t *testing.T) TextImage {
	t.Helper()
	g, err := NewTextImage(bytes.Repeat([]byte{0x11}, testMACKeyLen))
	if err != nil {
		t.Fatalf("NewTextImage: %v", err)
	}
	return g
}

func detStream(t *testing.T, seed string) io.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// genOne 生成一题并同时拿到它的答案（包内私有出口，测试专用）。
func genOne(t *testing.T, g TextImage, difficulty int, seed string) (string, Question) {
	t.Helper()
	answer, q, err := g.generateWithAnswer(context.Background(), Params{Difficulty: difficulty}, detStream(t, "epocha-gen-"+seed))
	if err != nil {
		t.Fatalf("生成失败（难度 %d，seed %s）: %v", difficulty, seed, err)
	}
	return answer, q
}

// seedN 造一个**至少 8 字节**的测试熵种子（eval 拒绝短种子：短种子会静默退化成常量流）。
// answerFromSeed 与 genOne 用**同一个**熵流标识取答案：
// 这条一致性是"离线评测的 ground truth 就是生产题目的答案"的全部依据。
func answerFromSeed(t *testing.T, seed string) string {
	t.Helper()
	a, err := TextImageAnswerFrom(detStream(t, "epocha-gen-"+seed))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func seedN(prefix string, i int) string {
	return "epocha-" + prefix + "-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
}

// --- 形状与判分闭环 ---

func TestTextImageGenerateShape(t *testing.T) {
	g := newTestTextImage(t)
	for diff := 1; diff <= MaxDifficulties; diff++ {
		want, err := TextImageParams(diff)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 12; i++ {
			answer, q := genOne(t, g, diff, seedN("shape", i*7+diff))
			if len(answer) != TextImageLength {
				t.Fatalf("答案长度 %d ≠ %d", len(answer), TextImageLength)
			}
			if q.MimeType != MimeTypePNG {
				t.Fatalf("MimeType = %q", q.MimeType)
			}
			if len(q.Verifier) != AnswerVerifierLen {
				t.Fatalf("verifier 长度 %d，期望 %d", len(q.Verifier), AnswerVerifierLen)
			}
			img, err := png.Decode(bytes.NewReader(q.Asset))
			if err != nil {
				t.Fatalf("难度 %d 的资产不是合法 PNG: %v", diff, err)
			}
			if b := img.Bounds(); b.Dx() != want.Width || b.Dy() != want.Height {
				t.Fatalf("难度 %d 图像尺寸 %v 与档位 %dx%d 不符", diff, b, want.Width, want.Height)
			}
			if len(q.Asset) > MaxImageAssetBytes {
				t.Fatalf("难度 %d PNG %d 字节，越界", diff, len(q.Asset))
			}
			if !g.Verify(q.Verifier, answer) {
				t.Fatalf("难度 %d 正确答案未通过判分", diff)
			}
			if !g.Verify(q.Verifier, strings.ToLower(answer)) {
				t.Fatalf("难度 %d 小写提交未通过判分（大小写不敏感是声明的行为）", diff)
			}
			if g.Verify(q.Verifier, corruptFirst(answer)) {
				t.Fatalf("难度 %d 改了字符竟通过判分", diff)
			}
		}
	}
}

func corruptFirst(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

// TestTextImageTier1InkStaysInsideGlyphBoxes 是"图片里确实是这串字符"的**几何**证据。
//
// d1 档不旋转、不抖动、无干扰线、无噪声，布局完全由答案决定。于是：
//   - 断言 (a) 每个字形包围盒内都有墨；
//   - 断言 (b) 包围盒之外没有任何暗像素。
//
// 两条合起来等于证明"画上去的笔迹只可能来自这 6 个字形"。
//
// 这不是 OCR 验证：它不能证明"人看得出这是 A"，只能证明渲染路径没有把预期外的东西混进题面。
// 跨实现的独立验证需要 OCR，而本环境没有离线 OCR（DESIGN §8.5 的未测项）。
func TestTextImageTier1InkStaysInsideGlyphBoxes(t *testing.T) {
	g := newTestTextImage(t)
	ip, err := TextImageParams(1)
	if err != nil {
		t.Fatal(err)
	}
	f := openProbeFace(t, ip)
	defer func() { _ = f.Close() }()

	for i := 0; i < 20; i++ {
		answer, q := genOne(t, g, 1, seedN("ink", i))
		placed, err := layoutGlyphs(f, answer, ip, mrand.New(mrand.NewSource(0)))
		if err != nil {
			t.Fatalf("d1 档布局应总能成功: %v", err)
		}
		img, err := png.Decode(bytes.NewReader(q.Asset))
		if err != nil {
			t.Fatal(err)
		}
		gray, ok := img.(*image.Gray)
		if !ok {
			t.Fatalf("画布应是 *image.Gray，got %T", img)
		}
		inkPerBox := make([]int, len(placed))
		leaked := 0
		for y := 0; y < ip.Height; y++ {
			for x := 0; x < ip.Width; x++ {
				if gray.GrayAt(x, y).Y >= 128 {
					continue // 纸面与浅色噪声之外的"有墨"判定：d1 无噪声，只有抗锯齿灰边
				}
				hit := -1
				for gi, p := range placed {
					if insideBox(p.box, x, y) {
						hit = gi
						break
					}
				}
				if hit < 0 {
					if leaked++; leaked < 5 {
						t.Errorf("答案 %q：(%d,%d) 有暗像素但在任何字形包围盒之外", answer, x, y)
					}
					continue
				}
				inkPerBox[hit]++
			}
		}
		if leaked > 0 {
			t.Fatalf("答案 %q 有 %d 个框外暗像素：题面混进了预期外的笔迹", answer, leaked)
		}
		for gi := range placed {
			if inkPerBox[gi] < 20 {
				t.Fatalf("答案 %q 的第 %d 个字形几乎没有墨（%d 像素）", answer, gi, inkPerBox[gi])
			}
		}
	}
}

// TestTextImageLayoutGuarantees 直接压在布局函数上：每个档位、大量抖动样本都必须满足
// 不越界与不严重重叠。这是"字符不能被裁切、严重重叠或移出可见区域"这条要求的落点。
func TestTextImageLayoutGuarantees(t *testing.T) {
	answers := []string{"MWMMMM", "234567", "ZKAPWM", "WZWZWZ", "HJKLMN"}
	for diff := 1; diff <= MaxDifficulties; diff++ {
		ip, err := TextImageParams(diff)
		if err != nil {
			t.Fatal(err)
		}
		f := openProbeFace(t, ip)
		var failed, checked int
		for a, answer := range answers {
			for s := 0; s < 150; s++ {
				placed, err := layoutGlyphs(f, answer, ip, mrand.New(mrand.NewSource(int64(s*7919+a+1))))
				if err != nil {
					// 布局失败只允许是"放不下"或"绘制与测量不一致"，两者都由外层有界重试处理
					if !strings.Contains(err.Error(), "放不进") && !strings.Contains(err.Error(), "不一致") {
						t.Fatalf("难度 %d 布局返回预期外的错误：%v", diff, err)
					}
					failed++
					continue
				}
				checked++
				if len(placed) != TextImageLength {
					t.Fatalf("难度 %d 字形数 %d ≠ %d", diff, len(placed), TextImageLength)
				}
				for i, p := range placed {
					if p.box.Min.X < glyphEdgeMargin || p.box.Max.X > ip.Width-glyphEdgeMargin ||
						p.box.Min.Y < glyphEdgeMargin || p.box.Max.Y > ip.Height-glyphEdgeMargin {
						t.Fatalf("难度 %d 第 %d 个字形越界 box=%v 画布 %dx%d", diff, i, p.box, ip.Width, ip.Height)
					}
					if p.tile == nil || p.tile.Bounds().Size() != p.dr.Size() {
						t.Fatalf("难度 %d 第 %d 个字形 tile 与 dr 不在同一坐标空间", diff, i)
					}
					if i > 0 {
						prev := placed[i-1].box
						if overlap := prev.Max.X - p.box.Min.X; overlap > min(prev.Dx(), p.box.Dx())/4 {
							t.Fatalf("难度 %d 第 %d 个字形与前一个字水平重叠 %d，超过较窄者 1/4", diff, i, overlap)
						}
					}
				}
			}
		}
		_ = f.Close()
		// 布局重试必须**极少**失败：否则默认档位会在生产里频繁返回 ErrGenerateExhausted
		if failed > checked/20 {
			t.Fatalf("难度 %d 布局失败率过高：%d/%d", diff, failed, failed+checked)
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// insideBox 是画布像素是否落在字形包围盒内（半开区间，与 image.Rectangle 的语义一致）。
func insideBox(r image.Rectangle, x, y int) bool {
	return r.Min.X <= x && x < r.Max.X && r.Min.Y <= y && y < r.Max.Y
}

func openProbeFace(t *testing.T, ip ImageParams) font.Face {
	t.Helper()
	f, err := loadGoRegular()
	if err != nil {
		t.Fatalf("字体解析: %v", err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: ip.FontSize, DPI: ip.DPI, Hinting: font.HintingFull})
	if err != nil {
		t.Fatal(err)
	}
	return face
}

// --- 字符集与抽样均匀性（M3 论证的前提） ---

func TestTextImageCharsetSamplingUniform(t *testing.T) {
	const draws = 40_000
	r := detStream(t, "charset-uniformity")
	counts := map[byte]int{}
	for n := 0; n < draws; n++ {
		a, err := TextImageAnswerFrom(r)
		if err != nil {
			t.Fatal(err)
		}
		if len(a) != TextImageLength {
			t.Fatalf("长度 %d", len(a))
		}
		for i := 0; i < len(a); i++ {
			if strings.IndexByte(TextImageCharset, a[i]) < 0 {
				t.Fatalf("抽到了字符集外的 %q", a[i])
			}
			counts[a[i]]++
		}
	}
	if len(counts) != len(TextImageCharset) {
		t.Fatalf("只出现 %d/%d 个字符：字符集没被完整覆盖", len(counts), len(TextImageCharset))
	}
	wantPer := float64(draws*TextImageLength) / float64(len(TextImageCharset))
	for _, c := range TextImageCharset {
		got := float64(counts[byte(c)])
		// ±20% 是"能抓住明显不均、又不会因随机抖动偶发失败"的宽界
		if got < wantPer*0.8 || got > wantPer*1.2 {
			t.Fatalf("字符 %q 出现 %.0f 次，期望 ≈%.0f（±20%%）", c, got, wantPer)
		}
	}
	for _, c := range "IO01" {
		if strings.ContainsRune(TextImageCharset, c) {
			t.Fatalf("字符集不应含易混字符 %q", c)
		}
	}
}

func TestTextImageAnswerFromMatchesGenerate(t *testing.T) {
	g := newTestTextImage(t)
	for i := 0; i < 60; i++ {
		seed := seedN("match", i)
		want := answerFromSeed(t, seed)
		answer, q := genOne(t, g, (i%MaxDifficulties)+1, seed)
		if answer != want {
			t.Fatalf("第 %d 次：TextImageAnswerFrom=%q 与生成实际用的答案 %q 不符 ⇒ 评测 ground truth 与生产分布脱钩", i, want, answer)
		}
		if !g.Verify(q.Verifier, want) {
			t.Fatalf("第 %d 次：由熵流推出的答案未通过判分", i)
		}
	}
}

// --- 归一化与判分 ---

func TestTextImageNormalizeAnswer(t *testing.T) {
	g := newTestTextImage(t)
	accept := []struct{ in, want string }{
		{"ABCD23", "ABCD23"},
		{"abcd23", "ABCD23"},   // 大小写不敏感
		{"  ABCD23", "ABCD23"}, // 首尾空白是输入习惯，不该消耗唯一一次作答机会
		{"ABCD23 \t\r\n", "ABCD23"},
		{"A2B3C4", "A2B3C4"},
	}
	for _, tc := range accept {
		got, ok := g.NormalizeAnswer(tc.in)
		if !ok || got != tc.want {
			t.Fatalf("NormalizeAnswer(%q) = (%q,%v)，期望 (%q,true)", tc.in, got, ok, tc.want)
		}
	}
	reject := []string{
		"", " ", "\t",
		"ABC", "ABCD234", strings.Repeat("A", 7), // 长度不符（长度本身是信息）
		"AB CD23", "AB\tCD23", "AB\nCD23", // 内部空白
		"ABCD2I", "ABCD2O", "ABCD21", "ABCD20", "abcdi0", // 被剔除的易混字符
		"١٢٣٤٥٦", "ＡＢＣＤ２３", "ABCD2٣", "ABCDÉF", // 非 ASCII
		"ABCD2.", "ABCD2-", "ABCD2_", "-ABCD2", "ABCD+2", // 结尾/开头的垃圾字符
		"ABCD2\n\n",
	}
	for _, in := range reject {
		if got, ok := g.NormalizeAnswer(in); ok {
			t.Fatalf("NormalizeAnswer(%q) 竟接受并给出 %q", in, got)
		}
	}
	// 归一化必须是幂等的：判分路径会先归一化一次，Verify 内部再归一化一次
	for _, tc := range accept {
		once, ok := g.NormalizeAnswer(tc.in)
		if !ok {
			t.Fatal(tc.in)
		}
		twice, ok := g.NormalizeAnswer(once)
		if !ok || twice != once {
			t.Fatalf("NormalizeAnswer 不幂等：%q → %q → %q", tc.in, once, twice)
		}
	}
}

func TestTextImageVerifyRejectsTamperedVerifier(t *testing.T) {
	g := newTestTextImage(t)
	answer, q := genOne(t, g, 2, "tamper")
	if !g.Verify(q.Verifier, answer) {
		t.Fatal("基线：正确答案应通过")
	}
	if g.Verify(q.Verifier[:AnswerVerifierLen-1], answer) {
		t.Fatal("截短的 verifier 竟通过")
	}
	if g.Verify(append(append([]byte{}, q.Verifier...), 'x'), answer) {
		t.Fatal("加长的 verifier 竟通过")
	}
	if g.Verify(PrivateVerifier(strings.Repeat("k", AnswerVerifierLen)), answer) {
		t.Fatal("随机 verifier 竟通过")
	}
	if g.Verify(q.Verifier, "ABC") || g.Verify(q.Verifier, "ABCDEF1") || g.Verify(q.Verifier, "") {
		t.Fatal("畸形输入竟通过判分")
	}
}

// TestTextImageAnswerIsNotGlobalToken 钉住"答案不是跨题口令"，并且**不依赖随机抽题**：
// 另一道题的 verifier 由测试自己构造，答案串确定不同。
func TestTextImageAnswerIsNotGlobalToken(t *testing.T) {
	g := newTestTextImage(t)
	key := bytes.Repeat([]byte{0x11}, testMACKeyLen)
	answerA, qA := genOne(t, g, 3, "not-global")

	other := "AAAAAA"
	if answerA == other {
		other = "BBBBBB" // 受控地换一个是别的串：不需要"随机抽到不同答案"这个运气
	}
	nonceB := bytes.Repeat([]byte{0x22}, verifierNonceSz)
	vB := PrivateVerifier(answerVerifier(key, nonceB, other))

	if !g.Verify(vB, other) {
		t.Fatal("受控 verifier 应接受它自己的答案")
	}
	if g.Verify(vB, answerA) {
		t.Fatalf("A 题的答案 %q 竟通过了 B 题（答案 %q）", answerA, other)
	}
	if g.Verify(qA.Verifier, other) {
		t.Fatalf("B 的答案 %q 竟通过了 A 题（答案 %q）", other, answerA)
	}
	if bytes.Equal(qA.Verifier, []byte(vB)) {
		t.Fatal("两个 verifier 相同：每题 nonce 没起作用")
	}
}

// --- 私有材料不出门 ---

func TestTextImagePrivateFieldsNeverLeave(t *testing.T) {
	g := newTestTextImage(t)
	for i := 0; i < 40; i++ {
		answer, q := genOne(t, g, 3, seedN("leak", i))
		payload := mustMarshal(t, q.Payload)
		for name, hay := range map[string][]byte{"payload": payload, "asset": q.Asset, "verifier": q.Verifier} {
			// verifier 里含 HMAC 输出，逐字节比较答案串没有意义，但它**不得等于**答案串
			if string(hay) == answer {
				t.Fatalf("答案 %q 原样出现在 %s 材料里", answer, name)
			}
		}
		if bytes.Contains(payload, []byte(answer)) || bytes.Contains(q.Asset, []byte(answer)) {
			t.Fatalf("答案 %q 出现在公开题面或图片字节里", answer)
		}
		fields, err := g.SanitizePublic(payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"answer", "seed", "verifier", "nonce", "jitter", "asset", "positions", "pool_id"} {
			if _, ok := fields[k]; ok {
				t.Fatalf("出网 DTO 含私有字段 %q", k)
			}
		}
		for k := range fields {
			switch k {
			case "kind", "length", "charset", "answer_type", "max_answer_len", "image_width", "image_height":
			default:
				t.Fatalf("DTO 出现未登记键 %q", k)
			}
		}
	}
}

func TestTextImageSanitizePublicKeyWhitelist(t *testing.T) {
	g := newTestTextImage(t)
	ok := mustMarshal(t, TextImagePublic{
		Length: TextImageLength, Charset: TextImageCharset, AnswerType: "text",
		MaxAnswerLen: TextImageLength, ImageWidth: 280, ImageHeight: 104,
	})
	if _, err := g.SanitizePublic(ok); err != nil {
		t.Fatalf("合法题面被拒: %v", err)
	}
	withExtra := append(ok[:len(ok)-1], []byte(`,"answer":"ABCDEF"}`)...)
	if _, err := g.SanitizePublic(withExtra); err == nil {
		t.Fatal("额外键必须整次报错（fail closed），不能静默丢弃后继续下发")
	}
	for name, doc := range map[string]string{
		"长度不符":   `{"length":5,"charset":"` + TextImageCharset + `","answer_type":"text","max_answer_len":6,"image_width":280,"image_height":104}`,
		"字符集不符":  `{"length":6,"charset":"ABC","answer_type":"text","max_answer_len":6,"image_width":280,"image_height":104}`,
		"答案类型不符": `{"length":6,"charset":"` + TextImageCharset + `","answer_type":"integer","max_answer_len":6,"image_width":280,"image_height":104}`,
		"尺寸越界":   `{"length":6,"charset":"` + TextImageCharset + `","answer_type":"text","max_answer_len":6,"image_width":7000,"image_height":104}`,
		"缺字段":    `{"length":6}`,
		"输入长度越界": `{"length":6,"charset":"` + TextImageCharset + `","answer_type":"text","max_answer_len":0,"image_width":280,"image_height":104}`,
	} {
		if _, err := g.SanitizePublic([]byte(doc)); err == nil {
			t.Fatalf("%s：应被拒绝", name)
		}
	}
}

// TestTextImageAssetHasNoTextChunks：PNG 元数据不得携带答案、文件名或任何私有串。
func TestTextImageAssetHasNoTextChunks(t *testing.T) {
	g := newTestTextImage(t)
	_, q := genOne(t, g, 1, "chunks")
	seen := map[string]int{}
	p := q.Asset
	if !bytes.HasPrefix(p, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("缺少 PNG 签名")
	}
	p = p[8:]
	for len(p) > 8 {
		typ := string(p[4:8])
		n := int(p[0])<<24 | int(p[1])<<16 | int(p[2])<<8 | int(p[3])
		seen[typ]++
		p = p[8+n+4:]
	}
	for _, typ := range []string{"tEXt", "iTXt", "zTXt"} {
		if seen[typ] > 0 {
			t.Fatalf("PNG 含文本元数据块 %s：答案或种子可能借此出门", typ)
		}
	}
	if seen["IHDR"] != 1 || seen["IDAT"] == 0 || seen["IEND"] != 1 {
		t.Fatalf("PNG 块结构异常：%v", seen)
	}
}

// --- 参数边界与失败形态 ---

func TestTextImageParamsBounds(t *testing.T) {
	base, err := TextImageParams(3)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]ImageParams{
		"宽越界":    with(base, func(p *ImageParams) { p.Width = maxImageWidth + 1 }),
		"高越界":    with(base, func(p *ImageParams) { p.Height = maxImageHeight + 1 }),
		"像素总数越界": with(base, func(p *ImageParams) { p.Width, p.Height = maxImageWidth, 200 }),
		"字号过大":   with(base, func(p *ImageParams) { p.FontSize = maxFontSize + 0.1 }),
		"字号过小":   with(base, func(p *ImageParams) { p.FontSize = minFontSize - 0.1 }),
		"DPI 越界": with(base, func(p *ImageParams) { p.DPI = maxDPI + 1 }),
		"旋转越界":   with(base, func(p *ImageParams) { p.MaxRotationDeg = maxRotationDeg + 0.1 }),
		"旋转为负":   with(base, func(p *ImageParams) { p.MaxRotationDeg = -1 }),
		"基线抖动越界": with(base, func(p *ImageParams) { p.BaselineJitter = maxBaselineJitter + 1 }),
		"字间抖动越界": with(base, func(p *ImageParams) { p.SpacingJitter = maxSpacingJitter + 1 }),
		"干扰线越界":  with(base, func(p *ImageParams) { p.Lines = maxInterferenceLines + 1 }),
		"干扰线段越界": with(base, func(p *ImageParams) { p.LineSegments = maxLineSegments + 1 }),
		"噪声越界":   with(base, func(p *ImageParams) { p.Noise = p.Width*p.Height/maxNoiseDivisor + 1 }),
		"噪声为负":   with(base, func(p *ImageParams) { p.Noise = -1 }),
	} {
		if err := p.validate(); err == nil {
			t.Fatalf("%s：越界参数未被拒：%+v", name, p)
		}
	}
	// 恰好等于上界必须**合法**：上界写成"严格小于"会让所有档位看起来都非法，
	// 而那种错误在只有拒绝表的测试里是测不出来的。
	edge := base
	edge.Width, edge.Height = 600, 200
	edge.FontSize, edge.DPI = maxFontSize, minDPI
	edge.MaxRotationDeg = maxRotationDeg
	edge.BaselineJitter, edge.SpacingJitter = maxBaselineJitter, maxSpacingJitter
	edge.Lines, edge.LineSegments = maxInterferenceLines, maxLineSegments
	edge.Noise = edge.Width * edge.Height / maxNoiseDivisor
	if err := edge.validate(); err != nil {
		t.Fatalf("边界值本身应合法: %v", err)
	}
	// 每档都必须合法且互不相同
	seen := map[ImageParams]int{}
	for d := 1; d <= MaxDifficulties; d++ {
		p, err := TextImageParams(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.validate(); err != nil {
			t.Fatalf("档位 %d 自检失败: %v", d, err)
		}
		seen[p]++
	}
	if len(seen) != MaxDifficulties {
		t.Fatal("五个档位必须彼此不同，否则难度档只是一个名字")
	}
	for _, d := range []int{0, -1, MaxDifficulties + 1, 99} {
		if _, err := TextImageParams(d); !errors.Is(err, ErrBadParams) {
			t.Fatalf("难度 %d 应返回 ErrBadParams，got %v", d, err)
		}
	}
}

func with(p ImageParams, f func(*ImageParams)) ImageParams {
	q := p
	f(&q)
	return q
}

func TestTextImageGenerateRejectsBadDifficulty(t *testing.T) {
	g := newTestTextImage(t)
	for _, d := range []int{0, -1, MaxDifficulties + 1} {
		if _, err := g.Generate(context.Background(), Params{Difficulty: d}, detStream(t, "epocha-test-bad-diff")); !errors.Is(err, ErrBadParams) {
			t.Fatalf("难度 %d 应被拒，got %v", d, err)
		}
	}
}

// TestTextImageEntropyFailureIsBounded 复现 3A 踩过的事故形态：坏熵源必须变成**有界结果**，
// 不能把生成器变成不可取消的忙循环。三条各自独立：
//   - 短熵流：读到 EOF ⇒ ErrOutOfEntropy（不是半成品、不是挂死）；
//   - 报错的熵源：错误原样上抛（折叠成"重试耗尽"会把诊断方向带偏）；
//   - 常量熵流：**必须返回**。Go 的 crypto/rand.Int 在 max 为 2 的幂时用掩码而不是拒绝采样，
//     所以常量流给出的是"一个合法但退化的答案"，而不是自旋 —— 这一点必须被测试钉住，
//     否则未来的实现换成拒绝采样式写法就会在这里挂死（算术题型正是那样挂死的）。
func TestTextImageEntropyFailureIsBounded(t *testing.T) {
	g := newTestTextImage(t)
	short := io.LimitReader(detStream(t, "epocha-test-short-entropy"), 8)
	if _, err := g.Generate(context.Background(), Params{Difficulty: 2}, short); !errors.Is(err, ErrOutOfEntropy) {
		t.Fatalf("8 字节熵流应耗尽，got %v", err)
	}
	failing := errorReader{err: errors.New("entropy went bang")}
	if _, err := g.Generate(context.Background(), Params{Difficulty: 2}, failing); err == nil ||
		!strings.Contains(err.Error(), "bang") {
		t.Fatalf("熵源错误应上抛，got %v", err)
	}
	done := make(chan error, 1)
	go func() {
		constant := io.LimitReader(strings.NewReader(strings.Repeat("\xff", 512)), 512)
		_, err := g.Generate(context.Background(), Params{Difficulty: 3}, constant)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("常量熵源把生成变成了不可终止的循环")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Generate(ctx, Params{Difficulty: 2}, detStream(t, "epocha-test-canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消应返回 context.Canceled，got %v", err)
	}
}

type errorReader struct{ err error }

func (e errorReader) Read([]byte) (int, error) { return 0, e.err }

func TestTextImageDeterministicAssets(t *testing.T) {
	g := newTestTextImage(t)
	a1, q1 := genOne(t, g, 4, "repro")
	a2, q2 := genOne(t, g, 4, "repro")
	if a1 != a2 || !bytes.Equal(q1.Asset, q2.Asset) {
		t.Fatal("同一熵流两次生成的答案或图像不一致 ⇒ 评测样本无法复现")
	}
	a3, q3 := genOne(t, g, 4, "other")
	if bytes.Equal(q1.Asset, q3.Asset) {
		t.Fatal("不同熵流竟生成完全相同的图像（图形抖动没起作用）")
	}
	if !g.Verify(q3.Verifier, a3) {
		t.Fatal("另一条流的判分失败")
	}
	// d1 档没有抖动：同一熵流必须逐字节同一张图（可读性对照条件依赖这一点）
	_, c1 := genOne(t, g, 1, "clean-a")
	_, c2 := genOne(t, g, 1, "clean-a")
	if !bytes.Equal(c1.Asset, c2.Asset) {
		t.Fatal("d1 档同一熵流竟生成不同图像")
	}
	if bytes.Equal(c1.Asset, q1.Asset) {
		t.Fatal("不同档位的图像竟逐字节相同：档位没有作用到渲染")
	}
}

// TestTextImageConcurrentGenerate 在 -race 下并发调用同一实例。
// 风险点是包级缓存的字体：Font 的无 Buffer 方法可并发（读过的注释），Face 必须每次新建。
func TestTextImageConcurrentGenerate(t *testing.T) {
	g := newTestTextImage(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 6; j++ {
				q, err := g.Generate(context.Background(), Params{Difficulty: (j % MaxDifficulties) + 1},
					detStream(t, seedN("conc", i*5+j)))
				if err != nil {
					errs <- err
					return
				}
				if len(q.Asset) == 0 || q.MimeType != MimeTypePNG {
					errs <- errors.New("并发调用产出了空资产或错误 MIME")
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestNewTextImageRejects(t *testing.T) {
	if _, err := NewTextImage(bytes.Repeat([]byte{1}, 16)); !errors.Is(err, ErrBadParams) {
		t.Fatal("短密钥必须被拒")
	}
	if _, err := NewTextImage(bytes.Repeat([]byte{1}, testMACKeyLen)); err != nil {
		t.Fatalf("合法密钥应构造成功: %v", err)
	}
	// 字体必须覆盖整个字符集（构造期就检查过，这里再独立验一次：
	// 缺字形会画成 .notdef，题面永远答不对而所有响应看起来都正常）
	f, err := loadGoRegular()
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: probeFontSize, DPI: minDPI, Hinting: font.HintingFull})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = face.Close() }()
	for _, r := range TextImageCharset {
		dr, _, _, _, ok := face.Glyph(fixed.P(0, 0), r)
		if !ok || dr.Empty() {
			t.Fatalf("内嵌字体缺 %q 字形", r)
		}
	}
}

// TestArithVerifierFormatUnchanged 是 3B 把判分构造提到共享函数后的回归护栏：
// 算术题的 verifier 字节必须仍然**逐字节等于**文档里那条公式的输出。
// 测试自己用 crypto/hmac 重算一遍（不借用被测代码），所以域分隔串、拼接顺序、
// nonce 位置、答案长度任何一处漂移都会变红——那类漂移的后果是升级后旧题静默判假。
func TestArithVerifierFormatUnchanged(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	g, err := NewArithmeticBaseline(key)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), Params{Difficulty: 3}, detStream(t, "epocha-test-arith-format"))
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Verifier) != AnswerVerifierLen {
		t.Fatalf("算术 verifier 长度 %d", len(q.Verifier))
	}
	nonce := q.Verifier[:verifierNonceSz]
	answer := evalArithText(t, q.Payload.(ArithPublic).Task)

	m := hmac.New(sha256.New, key)
	m.Write([]byte("epocha/v1|qver|"))
	m.Write(nonce)
	m.Write([]byte{0})
	m.Write([]byte(answer))
	want := append(append([]byte{}, nonce...), m.Sum(nil)...)
	if !bytes.Equal(want, []byte(q.Verifier)) {
		t.Fatal("重构后的 verifier 字节与文档线格式不一致（升级会让存量题静默判假）")
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatal("重构后算术题的正确答案不再通过判分")
	}
}

func evalArithText(t *testing.T, task string) string {
	t.Helper()
	num := func(s string) int64 {
		var v int64
		for _, c := range s {
			if c < '0' || c > '9' {
				t.Fatalf("题面含非数字 %q（在 %q 里）", c, s)
			}
			v = v*10 + int64(c-'0')
		}
		return v
	}
	apply := func(a int64, op string, b int64) int64 {
		switch op {
		case "+":
			return a + b
		case "-":
			return a - b
		case "\u00d7":
			return a * b
		case "\u00f7":
			if b == 0 || a%b != 0 {
				t.Fatalf("题面含非整除除法 %d %s %d", a, op, b)
			}
			return a / b
		}
		t.Fatalf("未知运算符 %q", op)
		return 0
	}
	s := task
	if strings.HasPrefix(s, "(") {
		end := strings.Index(s, ")")
		inner := strings.Fields(s[1:end])
		v := apply(num(inner[0]), inner[1], num(inner[2]))
		rest := strings.Fields(s[end+1:])
		return itoa(apply(v, rest[0], num(rest[1])))
	}
	f := strings.Fields(s)
	return itoa(apply(num(f[0]), f[1], num(f[2])))
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	digits := []byte{}
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// --- 供 STAGE3B.md 引用的实测数字（go test -run XXX -bench BenchmarkTextImage -benchtime 20x） ---

func BenchmarkTextImageGenerate(b *testing.B) {
	g, err := NewTextImage(bytes.Repeat([]byte{0x11}, testMACKeyLen))
	if err != nil {
		b.Fatal(err)
	}
	for diff := 1; diff <= MaxDifficulties; diff++ {
		b.Run(itoa(int64(diff)), func(b *testing.B) {
			var (
				minB, maxB, sumB = 1 << 30, 0, 0
			)
			r, _ := eval.NewDeterministic([]byte("bench-text-image"))
			b.ReportMetric(float64(diff), "tier")
			for i := 0; i < b.N; i++ {
				q, err := g.Generate(context.Background(), Params{Difficulty: diff}, r)
				if err != nil {
					b.Fatal(err)
				}
				n := len(q.Asset)
				sumB += n
				if n < minB {
					minB = n
				}
				if n > maxB {
					maxB = n
				}
			}
			b.ReportMetric(float64(minB), "png_min_B")
			b.ReportMetric(float64(maxB), "png_max_B")
			b.ReportMetric(float64(sumB)/float64(b.N), "png_avg_B")
		})
	}
}

// TestDrawSegmentTerminatesOnSteepLines 钉住本轮实测的挂死回归：
// 陡线（dy≫dx）与水平线都必须在一个有界的循环次数内画完。
// 超时保护靠 go test 的 -timeout，所以这里用极小的画布保证它本该瞬间返回。
func TestDrawSegmentTerminatesOnSteepLines(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 40, 40))
	cases := [][4]int{
		{0, 0, 0, 39},  // 垂直
		{0, 0, 39, 0},  // 水平
		{0, 0, 1, 39},  // 陡线：越过终点的死循环就发生在这类斜率上
		{0, 0, 39, 1},  // 浅线
		{39, 39, 0, 0}, // 反向对角
		{5, 5, 5, 5},   // 零长度
	}
	for _, c := range cases {
		done := make(chan struct{})
		go func() {
			drawSegment(img, image.Pt(c[0], c[1]), image.Pt(c[2], c[3]), lineGray)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("drawSegment(%v→%v) 未返回：线段绘制必须有界", c[:2], c[2:])
		}
	}
}
