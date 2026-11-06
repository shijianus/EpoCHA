package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

func (h *harness) siteWithGenerator(t *testing.T, name, siteKey, genID string, difficulty int) (*store.Site, string) {
	t.Helper()
	site, secret, err := h.db.CreateSite(context.Background(), name, siteKey, store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion,
		GeneratorID: genID, Difficulty: difficulty,
		AllowMissingOrigin: true,
	})
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	return site, secret
}

func payloadMap(t *testing.T, payload any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func shapeGridFixture(t *testing.T, difficulty int, seed string) seededQuestion {
	t.Helper()
	g, err := generator.NewShapeGrid(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := "epocha-api-shapegrid-" + seed
	answer, err := generator.ShapeGridAnswerFrom(evalStream(t, stream))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty}, evalStream(t, stream))
	if err != nil {
		t.Fatalf("生成 shape-grid 夹具: %v", err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatalf("夹具答案 %q 与该图判分不符 ⇒ 熵流位置不一致", answer)
	}
	return seededQuestion{
		GeneratorID: generator.ShapeGridID,
		Payload:     payloadMap(t, q.Payload),
		Answer:      answer,
		Asset:       q.Asset,
	}
}

func shapeCountFixture(t *testing.T, difficulty int, seed string) seededQuestion {
	t.Helper()
	g, err := generator.NewShapeCount(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := "epocha-api-shapecount-" + seed
	answer, err := generator.ShapeCountAnswerFrom(evalStream(t, stream))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty}, evalStream(t, stream))
	if err != nil {
		t.Fatalf("生成 shape-count 夹具: %v", err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatalf("夹具答案 %q 与该图判分不符 ⇒ 熵流位置不一致", answer)
	}
	return seededQuestion{
		GeneratorID: generator.ShapeCountID,
		Payload:     payloadMap(t, q.Payload),
		Answer:      answer,
		Asset:       q.Asset,
	}
}

func textImageV3Fixture(t *testing.T, difficulty int, seed string) seededQuestion {
	t.Helper()
	g, err := generator.NewTextImageV3(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := "epocha-api-v3-" + seed
	answer, err := generator.TextImageV3AnswerFrom(evalStream(t, stream))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty}, evalStream(t, stream))
	if err != nil {
		t.Fatalf("生成 v3 夹具: %v", err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatalf("夹具答案 %q 与该图判分不符 ⇒ 熵流位置不一致", answer)
	}
	return seededQuestion{
		GeneratorID: generator.TextImageV3ID,
		Payload:     payloadMap(t, q.Payload),
		Answer:      answer,
		Asset:       q.Asset,
		Tolerance:   1,
	}
}

func questionOf(t *testing.T, r reply) map[string]any {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("签发未成功：%d %s", r.status, r.body)
	}
	q, ok := r.got["question"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 question：%s", r.body)
	}
	return q
}

func TestShapeGridChallengeAndRedeem(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.siteWithGenerator(t, "grid", "sk-grid", generator.ShapeGridID, 2)
	fx := shapeGridFixture(t, 2, "e2e")
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	r := h.challengeReply("sk-grid")
	q := questionOf(t, r)
	if q["kind"] != "shape-grid" || q["answer_type"] != "cells" || q["generator"] != generator.ShapeGridID {
		t.Fatalf("shape-grid DTO 形状不符: %v", q)
	}
	if num(q["grid_cols"]) != 3 || num(q["grid_rows"]) != 3 || num(q["grid_cell"]) != 100 {
		t.Fatalf("网格几何不符: %v", q)
	}
	if _, err := base64.StdEncoding.DecodeString(q["image_base64"].(string)); err != nil {
		t.Fatalf("image_base64 不是合法 base64: %v", err)
	}
	rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), fx.Answer)
	if rr.status != http.StatusOK {
		t.Fatalf("正确答案未通过兑换：%d %s", rr.status, rr.body)
	}
}

// TestShapeGridRedeemNormalizesOrderAndDuplicates 证明"点选顺序/重复点击"不会消耗唯一机会。
func TestShapeGridRedeemNormalizesOrderAndDuplicates(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.siteWithGenerator(t, "grid", "sk-grid", generator.ShapeGridID, 2)
	fx := shapeGridFixture(t, 2, "normalize")
	h.seedQuestions(t, site.Policy.Difficulty, fx)
	r := h.challengeReply("sk-grid")
	questionOf(t, r)
	parts := strings.Split(fx.Answer, ",")
	if len(parts) != 2 {
		t.Fatalf("夹具答案 %q 不是两个格子", fx.Answer)
	}
	// 乱序 + 重复同一格 + 逗号后空格：规范化后必须与正确答案相同。
	submitted := parts[1] + "," + parts[0] + "," + parts[0] + ", " + parts[1]
	rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), submitted)
	if rr.status != http.StatusOK {
		t.Fatalf("规范化后的提交未通过：%d %s", rr.status, rr.body)
	}
}

func TestShapeCountChallengeAndRedeem(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.siteWithGenerator(t, "count", "sk-count", generator.ShapeCountID, 3)
	fx := shapeCountFixture(t, 3, "e2e")
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	r := h.challengeReply("sk-count")
	q := questionOf(t, r)
	if q["kind"] != "shape-count" || q["answer_type"] != "pairs" || q["generator"] != generator.ShapeCountID {
		t.Fatalf("shape-count DTO 形状不符: %v", q)
	}
	labels, ok := q["labels_zh"].([]any)
	if !ok || len(labels) != 2 {
		t.Fatalf("labels_zh 形状不符: %#v", q["labels_zh"])
	}
	parts := strings.Split(fx.Answer, ",")
	if len(parts) != 2 {
		t.Fatalf("夹具答案 %q 不是两个计数", fx.Answer)
	}
	rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), parts[0]+", "+parts[1])
	if rr.status != http.StatusOK {
		t.Fatalf("正确计数未通过兑换：%d %s", rr.status, rr.body)
	}
}

func TestTextImageV3TolerantRedeem(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.siteWithGenerator(t, "v3", "sk-v3", generator.TextImageV3ID, 2)
	fx := textImageV3Fixture(t, 2, "tolerant")
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	r := h.challengeReply("sk-v3")
	q := questionOf(t, r)
	if q["kind"] != "text-image" || num(q["length"]) != 6 || num(q["tolerant"]) != 1 {
		t.Fatalf("v3 DTO 形状不符: %v", q)
	}
	one := corruptOne(fx.Answer)
	if one == fx.Answer {
		t.Fatal("单字替换夹具没有变化")
	}
	rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), one)
	if rr.status != http.StatusOK {
		t.Fatalf("单字替换应通过（容差 1）：%d %s", rr.status, rr.body)
	}

	// 两个字符错误必须失败，并且该 challenge 被终结。
	fx2 := textImageV3Fixture(t, 2, "tolerant-two")
	h.seedQuestions(t, site.Policy.Difficulty, fx2)
	r2 := h.challengeReply("sk-v3")
	questionOf(t, r2)
	rr2 := h.redeemAnswer(r2.str("token"), h.solvePow(t, r2), corruptTwoChars(fx2.Answer))
	if rr2.status != http.StatusBadRequest {
		t.Fatalf("两个字符错误应被拒：%d %s", rr2.status, rr2.body)
	}
}

func TestExtensionQuestionsDoNotLeakPrivateMaterial(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct {
		name  string
		genID string
		fx    seededQuestion
	}{
		{"shape-grid", generator.ShapeGridID, shapeGridFixture(t, 1, "leak")},
		{"shape-count", generator.ShapeCountID, shapeCountFixture(t, 1, "leak")},
		{"text-image-v3", generator.TextImageV3ID, textImageV3Fixture(t, 1, "leak")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			site, _ := h.siteWithGenerator(t, tc.name, "sk-"+tc.name, tc.genID, 1)
			h.seedQuestions(t, site.Policy.Difficulty, tc.fx)
			r := h.challengeReply("sk-" + tc.name)
			questionOf(t, r)
			body := []byte(r.body)
			if bytes.Contains(body, []byte(tc.fx.Answer)) {
				t.Fatalf("%s 响应里出现了答案串 %q", tc.name, tc.fx.Answer)
			}
			// verifier 不直接下发；这里按库里的那一段字节做按值扫描。
			v := h.verifierOf(t, r.str("token"))
			if len(v) == 0 {
				t.Fatal("读回的 verifier 为空")
			}
			if bytes.Contains(body, v) {
				t.Fatalf("%s 响应里出现了私有 verifier 字节", tc.name)
			}
		})
	}
}

func corruptOne(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

func corruptTwoChars(s string) string {
	if len(s) < 2 {
		return s
	}
	b := []byte(s)
	for i := 0; i < 2; i++ {
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
	}
	return string(b)
}
