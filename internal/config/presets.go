package config

import "fmt"

// Resolution 是一个分辨率选项。Width/Height 为 0 表示"原始分辨率,不缩放"。
type Resolution struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Label  string `json:"label"`
}

// 相对桌面尺寸的缩放比例。0 表示原始分辨率。
var presetScales = []float64{0, 0.75, 0.5, 0.375}

// ResolutionPresets 按桌面宽高比生成分辨率选项。
//
// 跟随宽高比而不是写死 16:9 —— 本机桌面是 2560×1600(16:10),套用 1280×720
// 会同时产生拉伸和上下黑边。程序在启动时读取真实桌面尺寸来生成列表。
//
// 桌面尺寸探测失败时(desktopW/H 为 0),退回到通用的一组 16:9 预设。
func ResolutionPresets(desktopW, desktopH int) []Resolution {
	if desktopW <= 0 || desktopH <= 0 {
		return []Resolution{
			{0, 0, "原始分辨率"},
			{1920, 1080, "1920 × 1080"},
			{1280, 720, "1280 × 720"},
			{854, 480, "854 × 480"},
		}
	}

	var out []Resolution
	seen := map[[2]int]bool{}

	for _, s := range presetScales {
		var w, h int
		if s == 0 {
			w, h = desktopW, desktopH
		} else {
			w = int(float64(desktopW) * s)
			h = int(float64(desktopH) * s)
		}
		// yuv420p 要求宽高为偶数
		w -= w % 2
		h -= h % 2
		if w < 16 || h < 16 {
			continue
		}

		key := [2]int{w, h}
		if seen[key] {
			continue
		}
		seen[key] = true

		label := fmt.Sprintf("%d × %d", w, h)
		if s == 0 {
			label = fmt.Sprintf("原始 %d × %d(不缩放)", w, h)
		}
		out = append(out, Resolution{Width: w, Height: h, Label: label})
	}

	// 把"原始"作为第一项之外的默认推荐项,顺序保持从高到低即可
	return out
}
