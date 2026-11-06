package generator

import "testing"

func TestFrameSpecValidate(t *testing.T) {
	ok := FrameSpec{Mode: RenderModeSpriteSheet, Count: 12, Cols: 4, Rows: 3, FrameMS: 50, Loop: true}
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法 FrameSpec 被拒绝: %v", err)
	}
	if w, h, err := ok.SheetSize(120, 90); err != nil || w != 480 || h != 270 {
		t.Fatalf("SheetSize = (%d,%d,%v)，期望 (480,270,nil)", w, h, err)
	}

	cases := []struct {
		name string
		f    FrameSpec
	}{
		{"wrong mode", FrameSpec{Mode: "static", Count: 12, Cols: 4, Rows: 3, FrameMS: 50}},
		{"too few frames", FrameSpec{Mode: RenderModeSpriteSheet, Count: 1, Cols: 1, Rows: 1, FrameMS: 50}},
		{"too many frames", FrameSpec{Mode: RenderModeSpriteSheet, Count: 65, Cols: 8, Rows: 8, FrameMS: 50}},
		{"grid too small", FrameSpec{Mode: RenderModeSpriteSheet, Count: 9, Cols: 2, Rows: 4, FrameMS: 50}},
		{"cols too large", FrameSpec{Mode: RenderModeSpriteSheet, Count: 2, Cols: 9, Rows: 1, FrameMS: 50}},
		{"too fast", FrameSpec{Mode: RenderModeSpriteSheet, Count: 2, Cols: 2, Rows: 1, FrameMS: 19}},
		{"too slow", FrameSpec{Mode: RenderModeSpriteSheet, Count: 2, Cols: 2, Rows: 1, FrameMS: 501}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.f.Validate(); err == nil {
				t.Fatalf("%+v 应被拒绝", tc.f)
			}
		})
	}
}

func TestFrameSpecSheetSizeBounds(t *testing.T) {
	f := FrameSpec{Mode: RenderModeSpriteSheet, Count: 4, Cols: 2, Rows: 2, FrameMS: 50}
	if _, _, err := f.SheetSize(0, 100); err == nil {
		t.Fatal("零宽单帧应被拒绝")
	}
	if w, h, err := f.SheetSize(640, 640); err != nil || w != 1280 || h != 1280 {
		t.Fatalf("640×640 的 2×2 雪碧图应合法: (%d,%d,%v)", w, h, err)
	}
	tooBig := FrameSpec{Mode: RenderModeSpriteSheet, Count: 16, Cols: 4, Rows: 4, FrameMS: 50}
	if _, _, err := tooBig.SheetSize(640, 640); err == nil {
		t.Fatal("2560×2560 雪碧图应被拒绝")
	}
}
