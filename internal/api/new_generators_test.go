package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

// Wave 1（ADR-030）新题型的 API 闭环：DTO 形状、正确答案、一次性消费、错误答案终结。
// fixture 的 verifier 直接用生成器产物，因为生成器的线格式已在 internal/generator 里被
// 独立测试覆盖；这里证明的是"登记 → 领题 → redeem → 一次性"这条链路。

func wave1Fixture(t *testing.T, genID, seed string) seededQuestion {
	t.Helper()
	stream := "epocha-api-wave1-" + genID + "-" + seed
	var (
		g      generator.Generator
		answer string
		err    error
	)
	switch genID {
	case generator.TileRotationID:
		g, err = generator.NewTileRotation(tokenQuestionKey(t))
		if err == nil {
			answer, err = generator.TileRotationAnswerFrom(evalStream(t, stream))
		}
	case generator.SymbolPathID:
		g, err = generator.NewSymbolPath(tokenQuestionKey(t))
		if err == nil {
			answer, err = generator.SymbolPathAnswerFrom(evalStream(t, stream))
		}
	case generator.CrowdFindID:
		g, err = generator.NewCrowdFind(tokenQuestionKey(t))
		if err == nil {
			answer, err = generator.CrowdFindAnswerFrom(evalStream(t, stream))
		}
	case generator.VisualRankID:
		g, err = generator.NewVisualRank(tokenQuestionKey(t))
		if err == nil {
			answer, err = generator.VisualRankAnswerFrom(evalStream(t, stream))
		}
	default:
		t.Fatalf("未知 Wave 1 题型 %q", genID)
	}
	if err != nil {
		t.Fatalf("%s 生成夹具: %v", genID, err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: 2}, evalStream(t, stream))
	if err != nil {
		t.Fatalf("%s 生成题目: %v", genID, err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatalf("%s 夹具答案 %q 与 verifier 不匹配", genID, answer)
	}
	return seededQuestion{
		GeneratorID: genID, Payload: payloadMap(t, q.Payload), Answer: answer,
		Asset: q.Asset, Verifier: q.Verifier,
	}
}

func wrongWave1Answer(genID, answer string) string {
	switch genID {
	case generator.TileRotationID:
		if answer == "v1:000000000" {
			return "v1:111111111"
		}
		return "v1:000000000"
	case generator.SymbolPathID:
		wrong := "v1:00.01.02.08.09.15"
		if wrong == answer {
			return "v1:00.01.02.08.14.20"
		}
		return wrong
	case generator.CrowdFindID:
		var n int
		if _, err := fmt.Sscanf(answer, "v1:%d", &n); err != nil {
			return "v1:0"
		}
		return "v1:" + strconv.Itoa((n+1)%36)
	case generator.VisualRankID:
		if answer == "v1:A.B.C.D.E" {
			return "v1:E.D.C.B.A"
		}
		return "v1:A.B.C.D.E"
	}
	return "v1:"
}

func TestWave1GeneratorsChallengeAndRedeem(t *testing.T) {
	cases := []struct {
		id, siteKey, answerType string
	}{
		{generator.TileRotationID, "sk-tile", "masks"},
		{generator.SymbolPathID, "sk-symbol", "path"},
		{generator.CrowdFindID, "sk-crowd", "select"},
		{generator.VisualRankID, "sk-rank", "order"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			h := newHarness(t, nil)
			site, _ := h.siteWithGenerator(t, tc.id, tc.siteKey, tc.id, 2)
			fx := wave1Fixture(t, tc.id, "correct")
			h.seedQuestions(t, site.Policy.Difficulty, fx)

			r := h.challengeReply(tc.siteKey)
			q := questionOf(t, r)
			if q["answer_type"] != tc.answerType || q["generator"] != tc.id {
				t.Fatalf("DTO 形状不符: %v", q)
			}
			if q["media"] != "image" || q["image_base64"] == "" {
				t.Fatalf("Wave 1 题型必须是内联图片: %v", q)
			}
			for _, forbidden := range []string{"answer", "verifier", "seed", "target_index", "target_masks"} {
				if _, ok := q[forbidden]; ok {
					t.Fatalf("公开 DTO 含禁用字段 %q: %v", forbidden, q)
				}
			}
			rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), fx.Answer)
			if rr.status != http.StatusOK {
				t.Fatalf("正确答案未通过兑换：%d %s", rr.status, rr.body)
			}
			if rr.str("response") == "" {
				t.Fatalf("兑换成功但没有 response token: %s", rr.body)
			}
			again := h.redeemAnswer(r.str("token"), h.solvePow(t, r), fx.Answer)
			if again.status == http.StatusOK {
				t.Fatalf("同一 challenge 第二次兑换竟然成功: %s", again.body)
			}
		})
	}
}

func TestWave1GeneratorsWrongAnswerTerminates(t *testing.T) {
	cases := []struct {
		id, siteKey string
	}{
		{generator.TileRotationID, "sk-tile-wrong"},
		{generator.SymbolPathID, "sk-symbol-wrong"},
		{generator.CrowdFindID, "sk-crowd-wrong"},
		{generator.VisualRankID, "sk-rank-wrong"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			h := newHarness(t, nil)
			site, _ := h.siteWithGenerator(t, tc.id+"-wrong", tc.siteKey, tc.id, 2)
			fx := wave1Fixture(t, tc.id, "wrong")
			h.seedQuestions(t, site.Policy.Difficulty, fx)

			r := h.challengeReply(tc.siteKey)
			questionOf(t, r)
			wrong := wrongWave1Answer(tc.id, fx.Answer)
			if wrong == fx.Answer {
				t.Fatalf("测试夹具没有产生错误答案")
			}
			rr := h.redeemAnswer(r.str("token"), h.solvePow(t, r), wrong)
			if rr.status == http.StatusOK {
				t.Fatalf("错误答案竟然通过: %s", rr.body)
			}
			// 错误答案已提交终态：再提交正确答案仍必须失败。
			again := h.redeemAnswer(r.str("token"), h.solvePow(t, r), fx.Answer)
			if again.status == http.StatusOK {
				t.Fatalf("错误答案后第二次兑换竟然成功: %s", again.body)
			}
		})
	}
}
