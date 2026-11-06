package generator

import "fmt"

// 多帧渲染契约（ADR-025 / ADR-028）。Phase 1 用一张 PNG 雪碧图承载多帧，
// 因此不需要改动 pool 的单资产形态；帧按行优先排列。
const (
	RenderModeStatic      = "static"
	RenderModeSpriteSheet = "sprite_sheet"

	MinFrameCount = 2
	MaxFrameCount = 64
	MinFrameMS    = 20
	MaxFrameMS    = 500
	MaxFrameGrid  = 8
	MaxSheetSide  = 2048
)

// FrameSpec 是雪碧图元数据。它是公开字段的子集；答案与时间窗不在里面。
type FrameSpec struct {
	Mode    string `json:"render_mode"`
	Count   int    `json:"frame_count"`
	Cols    int    `json:"frame_cols"`
	Rows    int    `json:"frame_rows"`
	FrameMS int    `json:"frame_ms"`
	Loop    bool   `json:"frame_loop"`
}

// Validate 只检查形状；单帧尺寸由 SheetSize 检查，因为 FrameSpec 不含像素尺寸。
func (f FrameSpec) Validate() error {
	if f.Mode != RenderModeSpriteSheet {
		return fmt.Errorf("%w: frame mode=%q", ErrBadParams, f.Mode)
	}
	if f.Count < MinFrameCount || f.Count > MaxFrameCount {
		return fmt.Errorf("%w: frame_count=%d", ErrBadParams, f.Count)
	}
	if f.Cols < 1 || f.Cols > MaxFrameGrid || f.Rows < 1 || f.Rows > MaxFrameGrid {
		return fmt.Errorf("%w: frame grid=%dx%d", ErrBadParams, f.Cols, f.Rows)
	}
	if f.Cols*f.Rows < f.Count {
		return fmt.Errorf("%w: frame grid %dx%d 容不下 %d 帧", ErrBadParams, f.Cols, f.Rows, f.Count)
	}
	if f.FrameMS < MinFrameMS || f.FrameMS > MaxFrameMS {
		return fmt.Errorf("%w: frame_ms=%d", ErrBadParams, f.FrameMS)
	}
	return nil
}

// SheetSize 返回整张雪碧图的显示尺寸并检查边界。
// 单帧尺寸沿用现有图片画布上界（1..640），整张图不超过 MaxSheetSide×MaxSheetSide。
func (f FrameSpec) SheetSize(frameW, frameH int) (int, int, error) {
	if frameW < 1 || frameW > 640 || frameH < 1 || frameH > 640 {
		return 0, 0, fmt.Errorf("%w: frame size=%dx%d", ErrBadParams, frameW, frameH)
	}
	if f.Cols < 1 || f.Rows < 1 {
		return 0, 0, fmt.Errorf("%w: frame grid=%dx%d", ErrBadParams, f.Cols, f.Rows)
	}
	w, h := frameW*f.Cols, frameH*f.Rows
	if w > MaxSheetSide || h > MaxSheetSide {
		return 0, 0, fmt.Errorf("%w: sheet size=%dx%d", ErrBadParams, w, h)
	}
	return w, h, nil
}
