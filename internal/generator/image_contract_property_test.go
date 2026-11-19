package generator

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"testing"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func TestTextImagePayloadDimensionsMatchPNG(t *testing.T) {
	key := bytes.Repeat([]byte{0xa7}, 32)
	profiles := []struct {
		name string
		new  func() (TextImage, error)
	}{
		{name: TextImageID, new: func() (TextImage, error) { return NewTextImage(key) }},
		{name: TextImageV2ID, new: func() (TextImage, error) { return NewTextImageV2(key) }},
		{name: TextImageV3ID, new: func() (TextImage, error) { return NewTextImageV3(key) }},
	}

	for _, profile := range profiles {
		g, err := profile.new()
		if err != nil {
			t.Fatalf("%s 构造失败: %v", profile.name, err)
		}
		for difficulty := 1; difficulty <= MaxDifficulties; difficulty++ {
			for sample := 0; sample < 3; sample++ {
				seed := []byte(profile.name + "/dimension-contract/" + string(rune('0'+difficulty)) + "/" + string(rune('a'+sample)))
				entropy, err := eval.NewDeterministic(seed)
				if err != nil {
					t.Fatal(err)
				}
				q, err := g.Generate(context.Background(), Params{Difficulty: difficulty}, entropy)
				if err != nil {
					t.Fatalf("%s d%d sample%d Generate: %v", profile.name, difficulty, sample, err)
				}

				payloadJSON, err := json.Marshal(q.Payload)
				if err != nil {
					t.Fatal(err)
				}
				var payload struct {
					ImageWidth  int `json:"image_width"`
					ImageHeight int `json:"image_height"`
				}
				if err := json.Unmarshal(payloadJSON, &payload); err != nil {
					t.Fatal(err)
				}
				cfg, err := png.DecodeConfig(bytes.NewReader(q.Asset))
				if err != nil {
					t.Fatalf("%s d%d sample%d PNG DecodeConfig: %v", profile.name, difficulty, sample, err)
				}
				if payload.ImageWidth != cfg.Width || payload.ImageHeight != cfg.Height {
					t.Fatalf("%s d%d sample%d payload=%dx%d PNG=%dx%d",
						profile.name, difficulty, sample, payload.ImageWidth, payload.ImageHeight, cfg.Width, cfg.Height)
				}
			}
		}
	}
}
