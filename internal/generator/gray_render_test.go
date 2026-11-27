package generator

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"testing"
)

func TestFillGrayRectMatchesReference(t *testing.T) {
	// Nonzero origins and subimages exercise both stride and clipping, including
	// bytes outside the destination view that must remain untouched.
	for _, bounds := range []image.Rectangle{image.Rect(0, 0, 40, 30), image.Rect(-20, 10, 20, 40)} {
		for _, sub := range []bool{false, true} {
			for _, value := range []byte{0, 1, 127, 255} {
				for _, rect := range []image.Rectangle{
					bounds, bounds.Inset(4), image.Rect(-100, -100, 100, 100),
					image.Rect(5, 5, 6, 6), image.Rect(0, 0, 0, 0),
					image.Rect(100, 100, 110, 110), image.Rect(-10, 15, 12, 24),
				} {
					got, want := image.NewGray(bounds), image.NewGray(bounds)
					for i := range got.Pix {
						got.Pix[i] = byte(i % 251)
					}
					copy(want.Pix, got.Pix)
					g, w := got, want
					if sub {
						g = got.SubImage(bounds.Inset(3)).(*image.Gray)
						w = want.SubImage(bounds.Inset(3)).(*image.Gray)
					}
					fillGrayRect(g, rect.Min.X, rect.Min.Y, rect.Dx(), rect.Dy(), value)
					draw.Draw(w, rect.Intersect(w.Bounds()), image.NewUniform(color.Gray{Y: value}), image.Point{}, draw.Src)
					if !bytes.Equal(got.Pix, want.Pix) {
						t.Fatalf("pixel mismatch bounds=%v sub=%v rect=%v value=%d", bounds, sub, rect, value)
					}
				}
			}
		}
	}
}

func TestCopyGrayFrameMatchesReference(t *testing.T) {
	for _, at := range []image.Point{{}, {X: -30, Y: -20}, {X: 10, Y: 8}, {X: 90, Y: 90}} {
		src := image.NewGray(image.Rect(-4, -8, 22, 18))
		for i := range src.Pix {
			src.Pix[i] = byte(i % 251)
		}
		src = src.SubImage(src.Bounds().Inset(3)).(*image.Gray)
		before := append([]byte(nil), src.Pix...)
		got, want := image.NewGray(image.Rect(-10, -12, 40, 30)), image.NewGray(image.Rect(-10, -12, 40, 30))
		g, w := got.SubImage(got.Bounds().Inset(3)).(*image.Gray), want.SubImage(want.Bounds().Inset(3)).(*image.Gray)
		copyGrayFrame(g, at, src)
		draw.Draw(w, image.Rectangle{Min: at, Max: at.Add(src.Bounds().Size())}, src, src.Bounds().Min, draw.Src)
		if !bytes.Equal(got.Pix, want.Pix) || !bytes.Equal(src.Pix, before) {
			t.Fatalf("copy differs from reference or changed source: offset=%v", at)
		}
	}
}
