//go:build windows

// Package screen 探测桌面尺寸。
//
// 用途:分辨率预设要跟随桌面宽高比(16:10 的桌面套 1280×720 会同时拉伸和加黑边),
// 所以程序启动时必须知道真实桌面尺寸。
package screen

import "golang.org/x/sys/windows"

const (
	smCXScreen        = 0
	smCYScreen        = 1
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
	smCMonitors       = 80
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
	return metric(smCXScreen), metric(smCYScreen)
}

// VirtualRect 返回整个虚拟桌面(所有显示器的最小包围盒)。
//
// 远程控制要用它把屏幕像素坐标折算成 SendInput 的绝对坐标 —— 那是
// 按整个虚拟桌面归一化的,不是按主显示器。副屏摆在主屏左边时这里
// 的 X 是负数。
func VirtualRect() Rect {
	return Rect{
		X: metric(smXVirtualScreen),
		Y: metric(smYVirtualScreen),
		W: metric(smCXVirtualScreen),
		H: metric(smCYVirtualScreen),
	}
}

// MonitorCount 返回显示器数量。
func MonitorCount() int {
	return metric(smCMonitors)
}

// metric 读一项系统度量。
//
// GetSystemMetrics 返回的是 int,虚拟屏幕的坐标和尺寸都可以是负数,
// 所以这里必须按有符号数解释 —— 当成无符号读会把 -1920 变成一个
// 天文数字。
func metric(index int) int {
	v, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(int32(v))
}
