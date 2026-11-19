package m4baseline

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"math"
	"os/exec"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func TestPublicDTOOnlyContainsPublicFields(t *testing.T) {
	g, err := generator.NewTileRotation(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	q, err := g.Generate(context.Background(), generator.Params{Difficulty: 3, Lang: "zh-CN"}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dto, payloadLen, err := publicDTO(g, q)
	if err != nil {
		t.Fatal(err)
	}
	if payloadLen <= 0 {
		t.Fatal("payload length should be > 0")
	}
	var doc map[string]any
	if err := json.Unmarshal(dto, &doc); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"verifier", "private", "answer", "seed", "private_verifier"} {
		if _, ok := doc[banned]; ok {
			t.Fatalf("public DTO leaks %q", banned)
		}
	}
	for _, required := range []string{"generator", "generator_version", "media", "image_mime", "image_base64", "initial_masks"} {
		if _, ok := doc[required]; !ok {
			t.Fatalf("public DTO missing %q", required)
		}
	}
	if bytes.Contains(dto, q.Verifier) {
		t.Fatal("public DTO contains verifier bytes")
	}
}

func TestStats(t *testing.T) {
	lo, hi := wilson95(0, 0)
	if lo != 0 || hi != 1 {
		t.Fatalf("wilson95(0,0)=%v,%v want 0,1", lo, hi)
	}
	lo, hi = wilson95(5, 10)
	if math.Abs(lo-0.2366) > 0.005 || math.Abs(hi-0.7634) > 0.005 {
		t.Fatalf("wilson95(5,10)=%v,%v", lo, hi)
	}
	if got := percentile([]float64{1, 2, 3, 4, 5}, 50); got != 3 {
		t.Fatalf("median=%v want 3", got)
	}
	if got := percentile(nil, 95); got != 0 {
		t.Fatalf("empty percentile=%v want 0", got)
	}
	if math.Abs(randomGuessThree(1.0/64)-0.0461) > 0.001 {
		t.Fatalf("randomGuessThree(1/64)=%v", randomGuessThree(1.0/64))
	}
}

func TestGuessSingle(t *testing.T) {
	cases := map[string]float64{
		generator.TileRotationID:       1.0 / 64.0,
		generator.SymbolPathID:         1.0 / 4008.0,
		generator.CrowdFindID:          1.0 / 36.0,
		generator.VisualRankID:         1.0 / 120.0,
		generator.ProgressiveTargetID:  1.0 / 729.0,
		generator.ShellTrackID:         1.0 / 64.0,
		generator.SilhouetteAssemblyID: 1.0 / 120.0,
	}
	for id, want := range cases {
		got, _ := guessSingle(id)
		if math.Abs(got-want) > 1e-12 {
			t.Fatalf("%s guess=%v want %v", id, got, want)
		}
	}
}

// TestServerDoesNotLinkM4Baseline 钉住"评测工具不进 runtime binary"。
func TestServerDoesNotLinkM4Baseline(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not in PATH")
	}
	cmd := exec.Command(goBin, "list", "-deps", "./cmd/server")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/server: %v", err)
	}
	deps := string(out)
	if strings.Contains(deps, "github.com/shijianus/EpoCHA/internal/m4baseline") {
		t.Fatalf("cmd/server links the M4 attack tool:\n%s", deps)
	}
}
