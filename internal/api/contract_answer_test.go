package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func contractFixture(t *testing.T, h *harness, genID, answer string) seededQuestion {
	t.Helper()
	g, err := h.gens.Lookup(genID)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: 3}, evalStream(t, "contract-"+genID))
	if err != nil {
		t.Fatalf("生成契约夹具 %s: %v", genID, err)
	}
	return seededQuestion{
		GeneratorID: genID,
		Payload:     payloadMap(t, q.Payload),
		Answer:      answer,
		Asset:       q.Asset,
		Verifier:    q.Verifier,
	}
}

func TestContractAnswerTypesChallengeAndRedeem(t *testing.T) {
	cases := []struct {
		name, genID, siteKey, answerType, answer string
	}{
		{"angle", contractAngleID, "sk-angle", generator.AnswerTypeAngle, contractAngleAnswer},
		{"point", contractPointID, "sk-point", generator.AnswerTypePoint, contractPointAnswer},
		{"timing", contractTimingID, "sk-timing", generator.AnswerTypeTiming, contractTimingAnswer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			site, _ := h.siteWithGenerator(t, tc.name, tc.siteKey, tc.genID, 3)
			fx := contractFixture(t, h, tc.genID, tc.answer)
			h.seedQuestions(t, site.Policy.Difficulty, fx)

			r := h.challengeReply(tc.siteKey)
			q := questionOf(t, r)
			if q["answer_type"] != tc.answerType {
				t.Fatalf("answer_type = %v，期望 %s", q["answer_type"], tc.answerType)
			}
			switch tc.genID {
			case contractAngleID:
				if num(q["angle_max"]) != 1800 || num(q["angle_step"]) != 1 {
					t.Fatalf("angle 题面字段不符: %v", q)
				}
			case contractTimingID:
				if q["render_mode"] != generator.RenderModeSpriteSheet || num(q["frame_count"]) != 4 {
					t.Fatalf("timing 题面字段不符: %v", q)
				}
			}
			rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), tc.answer)
			if rr.status != http.StatusOK {
				t.Fatalf("正确答案未通过兑换：%d %s", rr.status, rr.body)
			}
			if rr.str("response") == "" {
				t.Fatal("成功响应缺少 response token")
			}
		})
	}
}

func TestContractAngleRejectsOutsideToleranceAndConsumesAttempt(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.siteWithGenerator(t, "angle", "sk-angle-bad", contractAngleID, 3)
	fx := contractFixture(t, h, contractAngleID, contractAngleAnswer)
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	r := h.challengeReply("sk-angle-bad")
	questionOf(t, r)
	nonce := h.solvePow(t, r)
	if rr := h.redeemAnswer(r.str("token"), nonce, "901"); rr.status == http.StatusOK {
		t.Fatal("容差外的角度不应通过")
	}
	// 一次有效尝试后 challenge 已终结；正确答案也不能复用同一张 token。
	if rr := h.redeemAnswer(r.str("token"), nonce, contractAngleAnswer); rr.status == http.StatusOK {
		t.Fatal("已消费的 challenge 不应再次通过")
	}
}
