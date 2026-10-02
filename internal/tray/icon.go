package tray

import "sharescreen/internal/icon"

// trayIcon 返回托盘图标的 ICO 数据。
//
// 图形本身定义在 internal/icon 里,和 exe 自身的图标共用同一份 —— 分成两处
// 各画各的,迟早会改了一个忘了另一个。
//
// 这里只挑托盘用得上的小尺寸:Windows 会按当前 DPI 从里面挑最合适的一个,
// 100% 缩放下是 16,高 DPI 下会取 20/24/32。
func trayIcon() []byte {
	data, err := icon.ICO([]int{16, 20, 24, 32})
	if err != nil {
		// PNG 编码一个内存里的图像不会失败,真失败了也只是没有图标,
		// 不值得让托盘起不来。
		return nil
	}
	return data
}
