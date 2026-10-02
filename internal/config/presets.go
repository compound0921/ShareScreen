package config

import "fmt"

// Resolution 是一个分辨率选项。Width/Height 为 0 表示"原始分辨率,不缩放"。
type Resolution struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Label  string `json:"label"`
}

// 相对桌面尺寸的缩放比例。原始分辨率("不缩放")单独作为第一项构造,
// 不放在这个列表里 —— 它的 Width/Height 必须是 0,不是桌面尺寸。
var presetScales = []float64{0.75, 0.5, 0.375}

// ResolutionPresets 按桌面宽高比生成分辨率选项。
//
// 跟随宽高比而不是写死 16:9 —— 16:10 的桌面套用 1280×720 会同时产生拉伸
// 和上下黑边。程序在启动时读取真实桌面尺寸来生成列表。
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

	// ★ 第一项是"不缩放",它的 Width/Height 必须是 0,不能填桌面尺寸。
	//
	// 填了桌面尺寸(比如 2560×1600)参数构造器会认为这是一次真实缩放,
	// 于是走 hwdownload 把帧从显存拷回内存再缩放 —— 即使缩放比例是 1:1,
	// 那次拷贝照样发生,每帧十几 MB、60fps 下接近 1 GB/s。零拷贝路径
	// 就这么被无声地丢掉了,而界面上显示的仍然是"原始分辨率,不缩放"。
	//
	// 这个错误不会报错,只会让 CPU 占用莫名其妙地高 —— 所以在这里挡住它。
	out := []Resolution{{
		Width:  0,
		Height: 0,
		Label:  fmt.Sprintf("原始 %d × %d(不缩放)", desktopW, desktopH),
	}}

	seen := map[[2]int]bool{{desktopW, desktopH}: true} // 避免下面再生成一个等尺寸的档位

	for _, s := range presetScales {
		w := int(float64(desktopW) * s)
		h := int(float64(desktopH) * s)

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

		out = append(out, Resolution{
			Width:  w,
			Height: h,
			Label:  fmt.Sprintf("%d × %d", w, h),
		})
	}

	return out
}
