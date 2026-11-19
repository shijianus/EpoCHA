package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

func imageContractTestGenerators(t *testing.T) (*generator.Registry, generator.TextImage, generator.Parallax, generator.ShellTrack) {
	t.Helper()
	key := make([]byte, 32)
	registry := generator.NewRegistry()
	text, err := generator.NewTextImage(key)
	if err != nil {
		t.Fatal(err)
	}
	parallax, err := generator.NewParallax(key)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := generator.NewShellTrack(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []generator.Generator{text, parallax, shell} {
		if err := registry.Register(g); err != nil {
			t.Fatal(err)
		}
	}
	return registry, text, parallax, shell
}

func imageContractGenerate(t *testing.T, g generator.Generator, difficulty int, seed string) *store.PoolQuestion {
	t.Helper()
	entropy, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty}, entropy)
	if err != nil {
		t.Fatalf("Generate(%s, d%d): %v", g.ID(), difficulty, err)
	}
	payload, err := json.Marshal(q.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return &store.PoolQuestion{
		GeneratorID: g.ID(), Version: g.Version(), Difficulty: difficulty,
		PayloadJSON: string(payload), Verifier: q.Verifier,
		Asset: q.Asset, AssetMime: q.MimeType,
	}
}

func imageContractSetPayloadInt(t *testing.T, q *store.PoolQuestion, key string, value int) *store.PoolQuestion {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(q.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	payload[key] = b
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	copy := *q
	copy.PayloadJSON = string(encoded)
	return &copy
}

func TestQuestionDTOImageDimensionsMatchPNG(t *testing.T) {
	registry, text, parallax, shell := imageContractTestGenerators(t)
	server := &Server{gens: registry}

	textBase := imageContractGenerate(t, text, 1, "dto-text-base")
	textOtherTier := imageContractGenerate(t, text, 5, "dto-text-other-tier")
	parallaxBase := imageContractGenerate(t, parallax, 1, "dto-parallax-base")
	shellBase := imageContractGenerate(t, shell, 1, "dto-shell-base")

	t.Run("static real generator output is accepted", func(t *testing.T) {
		dto, err := server.questionDTO(textBase)
		if err != nil {
			t.Fatalf("生成器产出的静态 PNG 应通过 DTO 尺寸校验: %v", err)
		}
		if dto["image_width"] != 280 || dto["image_height"] != 104 {
			t.Fatalf("text-image-v1 d1 声明尺寸 = %v×%v，期望 280×104", dto["image_width"], dto["image_height"])
		}
	})

	t.Run("sprite sheet real generator output uses frame dimensions", func(t *testing.T) {
		for _, q := range []*store.PoolQuestion{parallaxBase, shellBase} {
			dto, err := server.questionDTO(q)
			if err != nil {
				t.Fatalf("合法雪碧图 %s 不应把单帧声明尺寸误与整张 PNG 比较: %v", q.GeneratorID, err)
			}
			if dto["render_mode"] != generator.RenderModeSpriteSheet {
				t.Fatalf("%s render_mode = %v", q.GeneratorID, dto["render_mode"])
			}
		}
	})

	tests := []struct {
		name string
		q    *store.PoolQuestion
	}{
		{
			name: "static metadata width differs from PNG",
			q:    imageContractSetPayloadInt(t, textBase, "image_width", 281),
		},
		{
			name: "static asset from another real tier differs from metadata",
			q: func() *store.PoolQuestion {
				copy := *textBase
				copy.Asset = textOtherTier.Asset
				return &copy
			}(),
		},
		{
			name: "sprite sheet asset differs from frame grid metadata",
			q: func() *store.PoolQuestion {
				copy := *parallaxBase
				copy.Asset = shellBase.Asset
				return &copy
			}(),
		},
		{
			name: "malformed PNG asset is rejected",
			q: func() *store.PoolQuestion {
				copy := *textBase
				copy.Asset = []byte("not a PNG")
				return &copy
			}(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if dto, err := server.questionDTO(tc.q); err == nil {
				t.Fatalf("尺寸不一致的图片资产不得出网；DTO=%v", dto)
			}
		})
	}
}

func TestQuestionDTOBadPNGMetadataFailsClosed(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.imageSiteWithSecret(t, "bad-image-contract", "sk-bad-image-contract", 1)
	fixture := imageFixture(t, 1, "bad-image-contract")
	ids := h.seedQuestions(t, site.Policy.Difficulty, fixture)
	if len(ids) != 1 {
		t.Fatalf("seedQuestions returned %d ids, want 1", len(ids))
	}

	const verifierSentinel = "BAD-PNG-VERIFIER-SENTINEL-54f0a8d9"
	maintenance := rawSQLite(t, h.dbPath)
	if _, err := maintenance.ExecContext(context.Background(),
		`UPDATE pool SET asset_blob=?, verifier_blob=? WHERE id=?`,
		[]byte("not a PNG"), []byte(verifierSentinel), ids[0]); err != nil {
		t.Fatal(err)
	}

	r := h.challengeReply("sk-bad-image-contract")
	if r.status != http.StatusServiceUnavailable || !r.hasCode(CodeChallengeUnavailable) {
		t.Fatalf("坏 PNG 应返回 challenge-unavailable 503：%d %s", r.status, r.body)
	}
	if _, ok := r.got["token"]; ok {
		t.Fatalf("坏 PNG 题不应签发 token：%s", r.body)
	}
	if _, ok := r.got["pow"]; ok {
		t.Fatalf("坏 PNG 题不应返回 PoW：%s", r.body)
	}
	if strings.Contains(r.body, verifierSentinel) || strings.Contains(h.logs.String(), verifierSentinel) {
		t.Fatal("坏 PNG 路径泄露了题目 verifier")
	}
	if n := h.activeChallenges(t); n != 0 {
		t.Fatalf("坏 PNG 不应留下 challenge，issued=%d", n)
	}
	buckets, err := h.db.PoolStats(context.Background(), h.clock)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range buckets {
		if b.GeneratorID == generator.TextImageID && (b.Ready != 0 || b.Terminal != 1) {
			t.Fatalf("坏 PNG 应隔离为唯一 terminal 题：ready=%d terminal=%d", b.Ready, b.Terminal)
		}
	}
}

func TestQuestionDTOImageMetadataIsStrictlyInteger(t *testing.T) {
	registry, text, _, _ := imageContractTestGenerators(t)
	server := &Server{gens: registry}
	q := imageContractGenerate(t, text, 1, "dto-text-fractional")
	var payload map[string]any
	if err := json.Unmarshal([]byte(q.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	payload["image_width"] = 280.5
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	q.PayloadJSON = string(b)
	if dto, err := server.questionDTO(q); err == nil {
		t.Fatalf("非整数图片宽度不得通过题面白名单/尺寸契约；DTO=%v", dto)
	}
}

func Example_questionDTOImageDimensionContract() {
	fmt.Println("static: PNG = image_width × image_height")
	fmt.Println("sprite_sheet: PNG = image_width × frame_cols, image_height × frame_rows")
	// Output:
	// static: PNG = image_width × image_height
	// sprite_sheet: PNG = image_width × frame_cols, image_height × frame_rows
}
