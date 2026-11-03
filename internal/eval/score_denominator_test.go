package eval

import "testing"

func TestScoreUnknownPredictionsDoNotSubtractKnownDenominator(t *testing.T) {
	ground := []Sample{{File: "a.png", Answer: "ABCDEF"}, {File: "b.png", Answer: "GHIJKL"}, {File: "c.png", Answer: "MNOPQR"}, {File: "d.png", Answer: "STUVWX"}}
	predictions := []Prediction{
		{File: "a.png", Text: "ABCDEF"},
		{File: "b.png", Text: "GHIJKL"},
		{File: "c.png", Err: "timeout"},
		{File: "d.png", Text: "bad"},
		{File: "unknown.png", Text: "ABCDEF"},
	}
	m, err := Score(ground, predictions, func(s string) (string, bool) { return s, len(s) == 6 })
	if err != nil {
		t.Fatal(err)
	}
	if m.Predicted != 4 || m.Unknown != 1 || m.ToolFailures != 1 || m.Malformed != 1 || m.Exact != 2 {
		t.Fatalf("unexpected counts: %+v", m)
	}
	if m.ScoredAccuracy != 2.0/3.0 || m.StrictAccuracy != 0.5 {
		t.Fatalf("unknown prediction changed known denominator: %+v", m)
	}
	m, err = Score(ground[:2], []Prediction{{File: "a.png", Text: "ABCDEF"}, {File: "b.png", Text: "GHIJKL"}, {File: "unknown.png", Text: "ABCDEF"}}, nil)
	if err != nil || m.ScoredAccuracy != 1 {
		t.Fatalf("accuracy must remain 100 percent: metrics=%+v err=%v", m, err)
	}
}
