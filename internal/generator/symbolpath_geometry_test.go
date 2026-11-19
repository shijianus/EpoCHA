package generator

import (
	"bytes"
	"image/color"
	"image/png"
	"testing"
)

func TestSymbolPathTargetLengthsStayInsideCanvas(t *testing.T) {
	for _, n := range []int{6, 7, 8} {
		spec := symbolPathSpec{Target: make([]int, n), Path: make([]int, n)}
		for i := range spec.Target {
			spec.Target[i] = i % symbolGlyphCount
		}
		asset, err := renderSymbolPath(spec)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(asset))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != SymbolPathWidth || img.Bounds().Dy() != SymbolPathHeight {
			t.Fatal("asset dimensions differ from public payload")
		}
		// A border on the canvas edge means a target card or its outline was clipped.
		for y := symbolPathTargetY; y < symbolPathTargetY+symbolPathTargetH; y++ {
			for _, x := range []int{0, SymbolPathWidth - 1} {
				if got := color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y; got != 238 {
					t.Fatalf("target length %d clipped at (%d,%d): gray=%d", n, x, y, got)
				}
			}
		}
	}
}
