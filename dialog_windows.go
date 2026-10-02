//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procMessageBoxW = user32.NewProc("MessageBoxW")
	// GetConsoleWindow 在 kernel32 里,不在 user32 —— 放错会 panic。
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
)

const (
	mbOK            = 0x00000000
	mbIconError     = 0x00000010
	mbSetForeground = 0x00010000
	mbTopmost       = 0x00040000
)

// showFatalDialog 用模态对话框把启动失败的原因告诉用户。
//
// 只在进程没有控制台时弹。双击 exe 启动(构建时带 -H=windowsgui)正是这种
// 情况:错误只落在日志文件里,而没人会为了一次"闪退"去翻日志。从终端启动时
// stderr 已经能看见,再弹个模态框只会挡住脚本。
func showFatalDialog(title, msg string) {
	// 这条路径上不能再抛异常 —— 那会把真正要报的错盖掉。
	// 正常系统上这两个过程一定存在,但"报错时再崩一次"的代价太大。
	if procGetConsoleWindow.Find() != nil || procMessageBoxW.Find() != nil {
		return
	}
	if hasConsole() {
		return
	}
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	m, err := windows.UTF16PtrFromString(msg)
	if err != nil {
		return
	}
	procMessageBoxW.Call(
		0, // 无父窗口
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		mbOK|mbIconError|mbSetForeground|mbTopmost)
}

// hasConsole 报告当前进程是否挂着控制台窗口。
func hasConsole() bool {
	h, _, _ := procGetConsoleWindow.Call()
	return h != 0
}
