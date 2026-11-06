package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func energyWindowFixture(t *testing.T, difficulty int, seed string) seededQuestion {
	t.Helper()
	g, err := generator.NewEnergyWindow(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := "epocha-api-energy-" + seed
	answer, err := generator.EnergyWindowAnswerFrom(evalStream(t, stream), difficulty)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty}, evalStream(t, stream))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatalf("energy 夹具答案 %q 不匹配", answer)
	}
	return seededQuestion{
		GeneratorID: generator.EnergyWindowID, Payload: payloadMap(t, q.Payload),
		Answer: answer, Asset: q.Asset, Verifier: q.Verifier,
	}
}

func pipeConnectFixture(t *testing.T, difficulty int, seed string) seededQuestion {
	t.Helper()
	g, err := generator.NewPipeConnect(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := "epocha-api-pipe-" + seed
	answer, err := generator.PipeConnectAnswerFrom(evalStream(t, stream), difficulty)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty}, evalStream(t, stream))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatalf("pipe 夹具答案 %q 不匹配", answer)
	}
	return seededQuestion{
		GeneratorID: generator.PipeConnectID, Payload: payloadMap(t, q.Payload),
		Answer: answer, Asset: q.Asset, Verifier: q.Verifier,
	}
}

func intersectionPointFixture(t *testing.T, difficulty int, seed string) seededQuestion {
	t.Helper()
	g, err := generator.NewIntersectionPoint(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := "epocha-api-intersection-" + seed
	answer, err := generator.IntersectionPointAnswerFrom(evalStream(t, stream), difficulty)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty}, evalStream(t, stream))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatalf("intersection 夹具答案 %q 不匹配", answer)
	}
	return seededQuestion{
		GeneratorID: generator.IntersectionPointID, Payload: payloadMap(t, q.Payload),
		Answer: answer, Asset: q.Asset, Verifier: q.Verifier,
	}
}

func TestEnergyWindowChallengeRedeemAndReplay(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "energy", "sk-energy", []string{generator.EnergyWindowID}, 2)
	fx := energyWindowFixture(t, 2, "replay")
	h.seedQuestions(t, 2, fx)
	r := h.challengeReply("sk-energy")
	q := questionOf(t, r)
	if q["answer_type"] != "timing" || q["render_mode"] != "sprite_sheet" || num(q["frame_count"]) != 36 {
		t.Fatalf("energy DTO 形状不符: %v", q)
	}
	nonce := h.solvePow(t, r)
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status != http.StatusOK {
		t.Fatalf("正确时机未通过: %d %s", rr.status, rr.body)
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("energy challenge 重放不应通过")
	}
}

func TestEnergyWindowWrongTimingConsumesAttempt(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "energy", "sk-energy-bad", []string{generator.EnergyWindowID}, 2)
	fx := energyWindowFixture(t, 2, "wrong")
	h.seedQuestions(t, 2, fx)
	r := h.challengeReply("sk-energy-bad")
	questionOf(t, r)
	nonce := h.solvePow(t, r)
	target, _ := strconv.Atoi(fx.Answer)
	// 偏差 1200ms 远超容差；若取模后落到 <200ms，NormalizeAnswer 会按"零思考抢答"
	// 判成畸形（不消耗作答机会），所以再挪开一段。
	wrongMS := (target + 1200) % 2880 // 与 energy-window-v1 的 36×80ms 周期一致
	if wrongMS < 200 {
		wrongMS += 400
	}
	wrong := strconv.Itoa(wrongMS)
	if rr := h.redeemAnswer(r.str("token"), nonce, wrong); rr.status == http.StatusOK {
		t.Fatal("窗口外时机不应通过")
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("答错后 challenge 已终结")
	}
}

func TestPipeConnectChallengeRedeemAndReplay(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "pipe", "sk-pipe", []string{generator.PipeConnectID}, 2)
	fx := pipeConnectFixture(t, 2, "replay")
	h.seedQuestions(t, 2, fx)
	r := h.challengeReply("sk-pipe")
	q := questionOf(t, r)
	if q["answer_type"] != "cells" || num(q["grid_cols"]) != 4 || num(q["grid_rows"]) != 4 {
		t.Fatalf("pipe DTO 形状不符: %v", q)
	}
	nonce := h.solvePow(t, r)
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status != http.StatusOK {
		t.Fatalf("正确节点未通过: %d %s", rr.status, rr.body)
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("pipe challenge 重放不应通过")
	}
}

func TestPipeConnectWrongSetConsumesAttempt(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "pipe", "sk-pipe-bad", []string{generator.PipeConnectID}, 2)
	fx := pipeConnectFixture(t, 2, "wrong")
	h.seedQuestions(t, 2, fx)
	r := h.challengeReply("sk-pipe-bad")
	questionOf(t, r)
	nonce := h.solvePow(t, r)
	wrong := "1,2"
	if fx.Answer == wrong {
		wrong = "15,16"
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, wrong); rr.status == http.StatusOK {
		t.Fatal("错误节点集合不应通过")
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("答错后 challenge 已终结")
	}
}

func TestIntersectionPointChallengeRedeemAndReplay(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "intersection", "sk-intersection", []string{generator.IntersectionPointID}, 3)
	fx := intersectionPointFixture(t, 3, "replay")
	h.seedQuestions(t, 3, fx)
	r := h.challengeReply("sk-intersection")
	q := questionOf(t, r)
	if q["answer_type"] != "point" || num(q["image_width"]) != 640 || num(q["image_height"]) != 360 {
		t.Fatalf("intersection DTO 形状不符: %v", q)
	}
	nonce := h.solvePow(t, r)
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status != http.StatusOK {
		t.Fatalf("正确交点未通过: %d %s", rr.status, rr.body)
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("intersection challenge 重放不应通过")
	}
}

func TestIntersectionPointWrongPointConsumesAttempt(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "intersection", "sk-intersection-bad", []string{generator.IntersectionPointID}, 3)
	fx := intersectionPointFixture(t, 3, "wrong")
	h.seedQuestions(t, 3, fx)
	r := h.challengeReply("sk-intersection-bad")
	questionOf(t, r)
	nonce := h.solvePow(t, r)
	wrong := "0,0"
	if fx.Answer == wrong {
		wrong = "999,999"
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, wrong); rr.status == http.StatusOK {
		t.Fatal("远离交点的点击不应通过")
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("答错后 challenge 已终结")
	}
}
