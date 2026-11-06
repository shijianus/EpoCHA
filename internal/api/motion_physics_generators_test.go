package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func parallaxFixture(t *testing.T, difficulty int, seed string) seededQuestion {
	t.Helper()
	g, err := generator.NewParallax(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := "epocha-api-parallax-" + seed
	answer, err := generator.ParallaxAnswerFrom(evalStream(t, stream), difficulty)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty}, evalStream(t, stream))
	if err != nil {
		t.Fatalf("生成 parallax 夹具: %v", err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatalf("夹具答案 %q 与该题判分不符", answer)
	}
	return seededQuestion{
		GeneratorID: generator.ParallaxID, Payload: payloadMap(t, q.Payload),
		Answer: answer, Asset: q.Asset, Verifier: q.Verifier,
	}
}

func bankShotFixture(t *testing.T, difficulty int, seed string) seededQuestion {
	t.Helper()
	g, err := generator.NewBankShot(tokenQuestionKey(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := "epocha-api-bankshot-" + seed
	answer, err := generator.BankShotAnswerFrom(evalStream(t, stream), difficulty)
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: difficulty}, evalStream(t, stream))
	if err != nil {
		t.Fatalf("生成 bank-shot 夹具: %v", err)
	}
	if !g.Verify(q.Verifier, answer) {
		t.Fatalf("夹具答案 %q 与该题判分不符", answer)
	}
	return seededQuestion{
		GeneratorID: generator.BankShotID, Payload: payloadMap(t, q.Payload),
		Answer: answer, Asset: q.Asset, Verifier: q.Verifier,
	}
}

func TestParallaxChallengeRedeemAndReplay(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.siteWithGenerator(t, "parallax", "sk-parallax", generator.ParallaxID, 2)
	fx := parallaxFixture(t, 2, "replay")
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	r := h.challengeReply("sk-parallax")
	q := questionOf(t, r)
	if q["kind"] != "parallax-code" || q["answer_type"] != "text" ||
		q["render_mode"] != "sprite_sheet" || num(q["frame_count"]) < 2 {
		t.Fatalf("parallax DTO 形状不符: %v", q)
	}
	nonce := h.solvePow(t, r)
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status != http.StatusOK {
		t.Fatalf("正确答案未通过：%d %s", rr.status, rr.body)
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("同一 challenge 重放不应通过")
	}
}

func TestParallaxWrongCodeConsumesAttempt(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.siteWithGenerator(t, "parallax", "sk-parallax-bad", generator.ParallaxID, 2)
	fx := parallaxFixture(t, 2, "wrong")
	h.seedQuestions(t, site.Policy.Difficulty, fx)
	r := h.challengeReply("sk-parallax-bad")
	questionOf(t, r)
	nonce := h.solvePow(t, r)
	wrong := "0000"
	if fx.Answer == wrong {
		wrong = "1111"
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, wrong); rr.status == http.StatusOK {
		t.Fatal("错误代码不应通过")
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("答错后 challenge 已终结，正确答案也不应复用同一 token")
	}
}

func TestBankShotChallengeRedeemAndReplay(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.siteWithGenerator(t, "bank", "sk-bank", generator.BankShotID, 2)
	fx := bankShotFixture(t, 2, "replay")
	h.seedQuestions(t, site.Policy.Difficulty, fx)

	r := h.challengeReply("sk-bank")
	q := questionOf(t, r)
	if q["kind"] != "bank-shot" || q["answer_type"] != "angle" ||
		num(q["min_bounces"]) != 1 || num(q["cue_x"]) <= 0 || num(q["aim_len"]) <= 0 {
		t.Fatalf("bank-shot DTO 形状不符: %v", q)
	}
	nonce := h.solvePow(t, r)
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status != http.StatusOK {
		t.Fatalf("正确角度未通过：%d %s", rr.status, rr.body)
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("同一 challenge 重放不应通过")
	}
}

func TestBankShotWrongAngleConsumesAttempt(t *testing.T) {
	h := newHarness(t, nil)
	site, _ := h.siteWithGenerator(t, "bank", "sk-bank-bad", generator.BankShotID, 3)
	fx := bankShotFixture(t, 3, "wrong")
	h.seedQuestions(t, site.Policy.Difficulty, fx)
	r := h.challengeReply("sk-bank-bad")
	questionOf(t, r)
	nonce := h.solvePow(t, r)
	a, err := strconv.Atoi(fx.Answer)
	if err != nil {
		t.Fatal(err)
	}
	wrong := strconv.Itoa((a + 900) % 3600)
	if rr := h.redeemAnswer(r.str("token"), nonce, wrong); rr.status == http.StatusOK {
		t.Fatal("容差外角度不应通过")
	}
	if rr := h.redeemAnswer(r.str("token"), nonce, fx.Answer); rr.status == http.StatusOK {
		t.Fatal("答错后 challenge 已终结，正确角度也不应复用同一 token")
	}
}
