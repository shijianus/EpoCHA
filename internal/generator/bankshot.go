package generator

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	mrand "math/rand"
	"strconv"
)

// bank-shot-v1：一次反弹的物理直觉题（ADR-025/ADR-028）。
//
// 公开题面只有台面图、母球位置、角度范围与瞄准线长度；障碍几何与正确角度都不下发。
// 服务端用 TSV 对量化角度做容差判定，verifier 里没有答案明文。
const (
	BankShotID         = "bank-shot-v1"
	bankShotVersion    = "v1"
	bankShotWidth      = 640
	bankShotHeight     = 360
	bankShotMaxAnswer  = 4
	bankShotMinBounces = 1
	bankShotAimLen     = 110
	bankShotBallR      = 9
)

type bankShotTier struct {
	tolerance    int // decidegrees
	pocketRadius int
	obstacleW    int
	obstacleH    int
}

// 有效角度 3600 个桶；d1 的接受域 41 个桶 ⇒ 单次盲猜 1.14%，3 次 3.4%（<10%）。
var bankShotTiers = map[int]bankShotTier{
	1: {tolerance: 20, pocketRadius: 20, obstacleW: 150, obstacleH: 42},
	2: {tolerance: 15, pocketRadius: 19, obstacleW: 145, obstacleH: 40},
	3: {tolerance: 12, pocketRadius: 18, obstacleW: 140, obstacleH: 38},
	4: {tolerance: 10, pocketRadius: 17, obstacleW: 135, obstacleH: 36},
	5: {tolerance: 8, pocketRadius: 16, obstacleW: 130, obstacleH: 34},
}

// BankShotTolerance 返回某难度档的角度容差（0.1°）。供评测/文档复算 M2/M3。
func BankShotTolerance(difficulty int) (int, error) {
	t, ok := bankShotTiers[difficulty]
	if !ok {
		return 0, fmt.Errorf("%w: bank-shot 缺少难度 %d 的档位", ErrBadParams, difficulty)
	}
	return t.tolerance, nil
}

// BankShotPublic 是唯一公开题面：字段即白名单。
type BankShotPublic struct {
	Kind         string `json:"kind"`
	AnswerType   string `json:"answer_type"`
	MaxAnswerLen int    `json:"max_answer_len"`
	ImageWidth   int    `json:"image_width"`
	ImageHeight  int    `json:"image_height"`
	PromptZH     string `json:"prompt_zh"`
	PromptEN     string `json:"prompt_en"`
	AngleMin     int    `json:"angle_min"`
	AngleMax     int    `json:"angle_max"`
	AngleStep    int    `json:"angle_step"`
	MinBounces   int    `json:"min_bounces"`
	CueX         int    `json:"cue_x"`
	CueY         int    `json:"cue_y"`
	AimLen       int    `json:"aim_len"`
}

func (BankShotPublic) PublicKind() string { return "bank-shot" }

// BankShot 实现 Generator；无可变状态，可并发调用。
type BankShot struct {
	macKey []byte
}

var _ Generator = BankShot{}

func NewBankShot(macKey []byte) (BankShot, error) {
	if len(macKey) < 32 {
		return BankShot{}, fmt.Errorf("%w: verifier 密钥短于 32 字节", ErrBadParams)
	}
	for d := 1; d <= MaxDifficulties; d++ {
		if _, ok := bankShotTiers[d]; !ok {
			return BankShot{}, fmt.Errorf("%w: bank-shot 缺少难度 %d 的档位", ErrBadParams, d)
		}
	}
	return BankShot{macKey: macKey}, nil
}

func (BankShot) ID() string      { return BankShotID }
func (BankShot) Version() string { return bankShotVersion }
func (BankShot) Media() string   { return MediaImage }

type bankShotScene struct {
	CueX, CueY       int
	TargetX, TargetY int
	PocketR          int
	Obstacle         image.Rectangle
	Angle            int
	Tolerance        int
	VisualSeed       int64
}

// buildBankShot 是 Generate 与 BankShotAnswerFrom 的唯一抽样路径。
// 场景重试有界；失败返回 ErrGenerateExhausted，绝不发一张没有可行解的图。
func buildBankShot(entropy io.Reader, difficulty int) (bankShotScene, []byte, error) {
	if difficulty < 1 || difficulty > MaxDifficulties {
		return bankShotScene{}, nil, fmt.Errorf("%w: difficulty=%d", ErrBadParams, difficulty)
	}
	tier, ok := bankShotTiers[difficulty]
	if !ok {
		return bankShotScene{}, nil, fmt.Errorf("%w: difficulty=%d", ErrBadParams, difficulty)
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return bankShotScene{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	seedBytes := make([]byte, 8)
	if _, err := io.ReadFull(entropy, seedBytes); err != nil {
		return bankShotScene{}, nil, fmt.Errorf("%w: %v", ErrOutOfEntropy, err)
	}
	var seed int64
	for _, b := range seedBytes {
		seed = seed<<8 | int64(b)
	}

	for range 64 {
		cx, err := drawInt(entropy, 80, 160)
		if err != nil {
			return bankShotScene{}, nil, err
		}
		cy, err := drawInt(entropy, 280, 325)
		if err != nil {
			return bankShotScene{}, nil, err
		}
		tx, err := drawInt(entropy, 570, 622)
		if err != nil {
			return bankShotScene{}, nil, err
		}
		ty, err := drawInt(entropy, 60, 165)
		if err != nil {
			return bankShotScene{}, nil, err
		}
		cueX, cueY, targetX, targetY := int(cx), int(cy), int(tx), int(ty)
		bounceX := cueX + int(math.Round(float64(targetX-cueX)*float64(cueY)/float64(cueY+targetY)))
		if bounceX < 40 || bounceX > 600 || absInt(bounceX-cueX) < 40 || absInt(targetX-bounceX) < 40 {
			continue
		}
		angle := angleDecidegrees(cueX, cueY, targetX, targetY)
		// 障碍放在若干候选位置；选第一个既不挡两段路径、也不压住球/袋的位置。
		for _, c := range [][2]int{{500, 315}, {430, 315}, {520, 245}, {320, 210}, {380, 250}, {260, 170}, {440, 190}, {300, 275}} {
			hw, hh := tier.obstacleW/2, tier.obstacleH/2
			obs := image.Rect(c[0]-hw, c[1]-hh,
				c[0]+(tier.obstacleW-hw), c[1]+(tier.obstacleH-hh)).Intersect(image.Rect(20, 20, bankShotWidth-20, bankShotHeight-20))
			if obs.Dx() < tier.obstacleW || obs.Dy() < tier.obstacleH {
				continue
			}
			if image.Pt(cueX, cueY).In(obs.Inset(-bankShotBallR-2)) ||
				image.Pt(targetX, targetY).In(obs.Inset(-tier.pocketRadius-2)) {
				continue
			}
			if segmentRectIntersects(cueX, cueY, bounceX, 0, obs.Inset(-bankShotBallR-2)) ||
				segmentRectIntersects(bounceX, 0, targetX, targetY, obs.Inset(-bankShotBallR-2)) {
				continue
			}
			return bankShotScene{
				CueX: cueX, CueY: cueY, TargetX: targetX, TargetY: targetY,
				PocketR: tier.pocketRadius, Obstacle: obs,
				Angle: angle, Tolerance: tier.tolerance, VisualSeed: seed,
			}, nonce, nil
		}
	}
	return bankShotScene{}, nil, fmt.Errorf("%w: bank-shot 未找到无遮挡场景", ErrGenerateExhausted)
}

// BankShotAnswerFrom 按与 Generate 相同的熵顺序取出正确角度，供 devseed/离线评测使用。
func BankShotAnswerFrom(entropy io.Reader, difficulty int) (string, error) {
	s, _, err := buildBankShot(entropy, difficulty)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(s.Angle), nil
}

func (g BankShot) Generate(ctx context.Context, p Params, entropy io.Reader) (Question, error) {
	if err := ctx.Err(); err != nil {
		return Question{}, err
	}
	scene, nonce, err := buildBankShot(entropy, p.Difficulty)
	if err != nil {
		return Question{}, err
	}
	tags, err := NewTSVBuilder(g.macKey, nonce)
	if err != nil {
		return Question{}, err
	}
	for _, a := range acceptedAngles(scene.Angle, scene.Tolerance) {
		if err := tags.Add(0, uint32(a)); err != nil {
			return Question{}, err
		}
	}
	ver, err := tags.Build()
	if err != nil {
		return Question{}, err
	}
	asset, err := renderBankShot(scene)
	if err != nil {
		return Question{}, err
	}
	pub := BankShotPublic{
		Kind: "bank-shot", AnswerType: AnswerTypeAngle, MaxAnswerLen: bankShotMaxAnswer,
		ImageWidth: bankShotWidth, ImageHeight: bankShotHeight,
		PromptZH: "拖动瞄准线，让球经一次反弹落入标记袋",
		PromptEN: "Aim the cue ball so it banks once into the marked pocket",
		AngleMin: AngleMin, AngleMax: AngleMax, AngleStep: 1, MinBounces: bankShotMinBounces,
		CueX: scene.CueX, CueY: scene.CueY, AimLen: bankShotAimLen,
	}
	return Question{
		Payload: pub, Verifier: PrivateVerifier(ver),
		Asset: asset, MimeType: MimeTypePNG,
	}, nil
}

func (g BankShot) Verify(v PrivateVerifier, input string) bool {
	canon, ok := g.NormalizeAnswer(input)
	if !ok {
		return false
	}
	angle, err := strconv.Atoi(canon)
	if err != nil {
		return false
	}
	return TSVContains(v, g.macKey, 0, uint32(angle))
}

func (BankShot) NormalizeAnswer(input string) (string, bool) {
	return NormalizeAngle(input)
}

func (BankShot) SanitizePublic(raw []byte) (map[string]any, error) {
	var p BankShotPublic
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("generator: bank-shot 题面 JSON: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("generator: bank-shot 题面 JSON: %w", err)
	}
	switch {
	case p.Kind != "bank-shot" || p.AnswerType != AnswerTypeAngle:
		return nil, fmt.Errorf("generator: bank-shot 题面类型不合法")
	case p.MaxAnswerLen != bankShotMaxAnswer:
		return nil, fmt.Errorf("generator: bank-shot 答案长度不合法")
	case p.ImageWidth != bankShotWidth || p.ImageHeight != bankShotHeight:
		return nil, fmt.Errorf("generator: bank-shot 图像尺寸不合法")
	case p.AngleMin != AngleMin || p.AngleMax != AngleMax || p.AngleStep != 1:
		return nil, fmt.Errorf("generator: bank-shot 角度范围不合法")
	case p.MinBounces != bankShotMinBounces:
		return nil, fmt.Errorf("generator: bank-shot 反弹次数不合法")
	case p.CueX < 0 || p.CueX >= bankShotWidth || p.CueY < 0 || p.CueY >= bankShotHeight:
		return nil, fmt.Errorf("generator: bank-shot 母球位置越界")
	case p.AimLen < 40 || p.AimLen > 300:
		return nil, fmt.Errorf("generator: bank-shot 瞄准线长度越界")
	case p.PromptZH == "" || len(p.PromptZH) > 200 || p.PromptEN == "" || len(p.PromptEN) > 200:
		return nil, fmt.Errorf("generator: bank-shot 提示不合法")
	}
	for k := range generic {
		switch k {
		case "kind", "answer_type", "max_answer_len", "image_width", "image_height",
			"prompt_zh", "prompt_en", "angle_min", "angle_max", "angle_step",
			"min_bounces", "cue_x", "cue_y", "aim_len":
		default:
			return nil, fmt.Errorf("generator: bank-shot 题面含非白名单字段 %q", k)
		}
	}
	return map[string]any{
		"kind": p.Kind, "answer_type": p.AnswerType, "max_answer_len": p.MaxAnswerLen,
		"image_width": p.ImageWidth, "image_height": p.ImageHeight,
		"prompt_zh": p.PromptZH, "prompt_en": p.PromptEN,
		"angle_min": p.AngleMin, "angle_max": p.AngleMax, "angle_step": p.AngleStep,
		"min_bounces": p.MinBounces, "cue_x": p.CueX, "cue_y": p.CueY, "aim_len": p.AimLen,
	}, nil
}

func acceptedAngles(target, tolerance int) []int {
	out := make([]int, 0, 2*tolerance+1)
	for d := -tolerance; d <= tolerance; d++ {
		out = append(out, (target+d+3600)%3600)
	}
	return out
}

func angleDecidegrees(cx, cy, tx, ty int) int {
	// 以屏幕坐标定义角度（y 向下）；镜像目标在 y=0 上方的反射路径。
	dx := float64(tx - cx)
	dy := float64(-ty - cy)
	deg := math.Atan2(dy, dx) * 180 / math.Pi
	if deg < 0 {
		deg += 360
	}
	return int(math.Round(deg*10)) % 3600
}

func segmentRectIntersects(x0, y0, x1, y1 int, r image.Rectangle) bool {
	// Liang–Barsky：用参数化线段与矩形四条边求交，避免采样漏掉薄障碍。
	xmin, xmax := float64(r.Min.X), float64(r.Max.X)
	ymin, ymax := float64(r.Min.Y), float64(r.Max.Y)
	dx, dy := float64(x1-x0), float64(y1-y0)
	t0, t1 := 0.0, 1.0
	clip := func(p, q float64) bool {
		if p == 0 {
			return q >= 0
		}
		t := q / p
		if p < 0 {
			if t > t1 {
				return false
			}
			if t > t0 {
				t0 = t
			}
		} else {
			if t < t0 {
				return false
			}
			if t < t1 {
				t1 = t
			}
		}
		return true
	}
	if !clip(-dx, float64(x0)-xmin) || !clip(dx, xmax-float64(x0)) ||
		!clip(-dy, float64(y0)-ymin) || !clip(dy, ymax-float64(y0)) {
		return false
	}
	return t0 <= t1
}

func renderBankShot(s bankShotScene) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, bankShotWidth, bankShotHeight))
	fillGrayRect(img, 0, 0, bankShotWidth, bankShotHeight, 18)
	// 台面与库边
	fillGrayRect(img, 8, 8, bankShotWidth-16, bankShotHeight-16, 28)
	fillGrayRect(img, 8, 8, bankShotWidth-16, 2, 72)
	fillGrayRect(img, 8, bankShotHeight-10, bankShotWidth-16, 2, 72)
	fillGrayRect(img, 8, 8, 2, bankShotHeight-16, 72)
	fillGrayRect(img, bankShotWidth-10, 8, 2, bankShotHeight-16, 72)
	for x := 40; x < bankShotWidth; x += 40 {
		fillGrayRect(img, x, 20, 1, bankShotHeight-40, 34)
	}
	for y := 40; y < bankShotHeight; y += 40 {
		fillGrayRect(img, 20, y, bankShotWidth-40, 1, 34)
	}
	// 干扰袋
	for _, p := range []image.Point{{24, 24}, {320, 24}, {616, 24}, {24, 180}, {24, 336}, {320, 336}} {
		fillCircle(img, p.X, p.Y, 13, 6)
		strokeCircle(img, p.X, p.Y, 13, 58)
	}
	// 目标袋：亮环 + 深洞
	fillCircle(img, s.TargetX, s.TargetY, s.PocketR, 6)
	strokeCircle(img, s.TargetX, s.TargetY, s.PocketR, 245)
	strokeCircle(img, s.TargetX, s.TargetY, s.PocketR-4, 120)
	// 障碍
	fillGrayRect(img, s.Obstacle.Min.X, s.Obstacle.Min.Y, s.Obstacle.Dx(), s.Obstacle.Dy(), 92)
	strokeGrayRect(img, s.Obstacle, 160)
	// 母球
	fillCircle(img, s.CueX, s.CueY, bankShotBallR, 242)
	strokeCircle(img, s.CueX, s.CueY, bankShotBallR, 12)
	fillCircle(img, s.CueX-3, s.CueY-3, 2, 255)
	// 轻噪点只落在背景；不改变障碍/球的边界。
	rng := mrand.New(mrand.NewSource(s.VisualSeed))
	for i := 0; i < 48; i++ {
		x, y := rng.Intn(bankShotWidth), rng.Intn(bankShotHeight)
		if img.GrayAt(x, y).Y <= 40 {
			img.SetGray(x, y, color.Gray{Y: uint8(22 + rng.Intn(14))})
		}
	}
	return encodeGrayPNG(img)
}

func fillCircle(dst *image.Gray, cx, cy, r int, v uint8) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
				fillGrayRect(dst, x, y, 1, 1, v)
			}
		}
	}
}

func strokeCircle(dst *image.Gray, cx, cy, r int, v uint8) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			d := (x-cx)*(x-cx) + (y-cy)*(y-cy)
			if d <= r*r && d >= (r-1)*(r-1) {
				fillGrayRect(dst, x, y, 1, 1, v)
			}
		}
	}
}

func strokeGrayRect(dst *image.Gray, r image.Rectangle, v uint8) {
	fillGrayRect(dst, r.Min.X, r.Min.Y, r.Dx(), 1, v)
	fillGrayRect(dst, r.Min.X, r.Max.Y-1, r.Dx(), 1, v)
	fillGrayRect(dst, r.Min.X, r.Min.Y, 1, r.Dy(), v)
	fillGrayRect(dst, r.Max.X-1, r.Min.Y, 1, r.Dy(), v)
}
