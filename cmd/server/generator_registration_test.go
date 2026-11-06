package main

import (
	"bytes"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func TestKnownGeneratorsIncludeTextImageV2(t *testing.T) {
	gens, err := knownGenerators(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatalf("knownGenerators: %v", err)
	}
	seen := map[string]bool{}
	for _, g := range gens {
		seen[g.ID()] = true
	}
	for _, id := range []string{
		generator.ArithID, generator.TextImageID, generator.TextImageV2ID, generator.TextImageV3ID,
		generator.ShapeGridID, generator.ShapeCountID,
		generator.ParallaxID, generator.BankShotID, generator.AccessibleChoiceID,
	} {
		if !seen[id] {
			t.Fatalf("knownGenerators 未登记 %q（已登记：%v）", id, seen)
		}
	}
}
