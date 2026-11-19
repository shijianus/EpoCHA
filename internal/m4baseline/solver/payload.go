package solver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
)

// publicDoc 是浏览器能看到的全部内容。Raw 里的键就是 /api/challenge 的 question 对象，
// 包含白名单字段、generator/generator_version、media/image_mime/image_base64。
type publicDoc struct {
	raw  map[string]any
	img  *image.Gray
	w, h int
}

func loadPublic(publicJSON []byte) (*publicDoc, error) {
	var raw map[string]any
	dec := json.NewDecoder(bytes.NewReader(publicJSON))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("public DTO 不是 JSON 对象: %w", err)
	}
	if media, _ := raw["media"].(string); media != "image" {
		return nil, fmt.Errorf("media=%q，本攻击基线只覆盖 image 题型", media)
	}
	if mime, _ := raw["image_mime"].(string); mime != "image/png" {
		return nil, fmt.Errorf("image_mime=%q，本攻击基线只覆盖 PNG", mime)
	}
	b64, _ := raw["image_base64"].(string)
	if b64 == "" {
		return nil, fmt.Errorf("缺少 image_base64")
	}
	asset, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("image_base64 解码失败: %w", err)
	}
	img, err := png.Decode(bytes.NewReader(asset))
	if err != nil {
		return nil, fmt.Errorf("PNG 解码失败: %w", err)
	}
	g := toGray(img)
	w, h := g.Bounds().Dx(), g.Bounds().Dy()
	iw, okW := docInt(raw, "image_width")
	ih, okH := docInt(raw, "image_height")
	if !okW || !okH {
		return nil, fmt.Errorf("缺少 image_width/image_height")
	}
	// 雪碧图的 image_width/height 是**单帧**尺寸；整张图的尺寸由 frame_cols/frame_rows
	// 推导。普通图片则两者必须与 PNG 一致。若这里放宽检查，坏资产会被攻击器读成
	// 完全不同的几何，测出来的数字就没有意义。
	if mode, _ := raw["render_mode"].(string); mode == "sprite_sheet" {
		fc, okC := docInt(raw, "frame_cols")
		fr, okR := docInt(raw, "frame_rows")
		if !okC || !okR || fc < 1 || fr < 1 || iw*fc != w || ih*fr != h {
			return nil, fmt.Errorf("雪碧图 frame 几何与 PNG 尺寸不一致")
		}
	} else if iw != w || ih != h {
		return nil, fmt.Errorf("image_width/height 与 PNG 尺寸不一致")
	}
	return &publicDoc{raw: raw, img: g, w: w, h: h}, nil
}

func docInt(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func docString(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func docObjects(m map[string]any, key string) ([]map[string]any, bool) {
	v, ok := m[key]
	if !ok {
		return nil, false
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		out = append(out, obj)
	}
	return out, true
}

func requireInt(d *publicDoc, keys ...string) ([]int, error) {
	out := make([]int, len(keys))
	for i, key := range keys {
		v, ok := docInt(d.raw, key)
		if !ok {
			return nil, fmt.Errorf("缺少字段 %s", key)
		}
		out[i] = v
	}
	return out, nil
}
