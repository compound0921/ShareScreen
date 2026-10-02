//go:build windows

// Package screen 探测桌面尺寸。
//
// 用途:分辨率预设要跟随桌面宽高比(16:10 的桌面套 1280×720 会同时拉伸和加黑边),
// 所以程序启动时必须知道真实桌面尺寸。
package screen

import "golang.org/x/sys/windows"

const (
	smCXScreen = 0
	smCYScreen = 1
)

var (
	user32                 = windows.NewLazySystemDLL("user32.dll")
	procGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	procSetProcessDPIAware = user32.NewProc("SetProcessDPIAware")
)

// SetDPIAware 声明进程为 DPI 感知。
//
// 不声明的话,GetSystemMetrics 返回的是被系统 DPI 缩放过的逻辑尺寸
// (比如 150% 缩放下会返回实际值的 2/3),宽高比推算会跟着出错。
func SetDPIAware() {
	// 返回值表示调用前是否已经是 DPI 感知的,这里不关心
	_, _, _ = procSetProcessDPIAware.Call()
}

// Size 返回主显示器的分辨率(物理像素)。
func Size() (w, h int) {
	cx, _, _ := procGetSystemMetrics.Call(uintptr(smCXScreen))
	cy, _, _ := procGetSystemMetrics.Call(uintptr(smCYScreen))
	return int(int32(cx)), int(int32(cy))
}
