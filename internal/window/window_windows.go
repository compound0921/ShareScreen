//go:build windows

// Package window 枚举当前桌面上可见的顶层窗口。
//
// 用途:采集单个窗口时,用户不必再去猜窗口标题里的一段文字 ——
// 直接从列表里选。
//
// 这里只做列表,不做缩略图。缩略图需要对每个窗口各截一帧,Windows 上
// 唯一干净的做法是 Windows Graphics Capture,而现在的采集路径是
// ffmpeg 的 ddagrab / gdigrab,拿不到那种能力。列表已经解决了
// "得手打标题"这个主要痛点。
package window

import (
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procIsIconic                 = user32.NewProc("IsIconic")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW     = user32.NewProc("GetWindowTextLengthW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowLongPtrW        = user32.NewProc("GetWindowLongPtrW")
)

const (
	gwlExStyle     = -20
	wsExToolWindow = 0x00000080
)

// Window 是一个可采集的顶层窗口。
type Window struct {
	Title     string `json:"title"`
	Process   string `json:"process"`   // 进程可执行文件名,如 chrome.exe
	PID       uint32 `json:"pid"`
	Minimized bool   `json:"minimized"` // 最小化的窗口采不到内容
}

// List 返回当前可见的顶层窗口,按进程名 + 标题排序。
//
// 过滤掉了不可见窗口、没有标题的窗口,以及工具窗口(WS_EX_TOOLWINDOW)
// —— 后者是各种程序创建的消息窗口、托盘辅助窗口,数量多且对用户无意义。
func List() ([]Window, error) {
	var out []Window

	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if visible, _, _ := procIsWindowVisible.Call(hwnd); visible == 0 {
			return 1 // 继续枚举
		}
		if isToolWindow(hwnd) {
			return 1
		}
		title := windowText(hwnd)
		if strings.TrimSpace(title) == "" {
			return 1
		}

		var pid uint32
		procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))

		iconic, _, _ := procIsIconic.Call(hwnd)

		out = append(out, Window{
			Title:     title,
			Process:   processName(pid),
			PID:       pid,
			Minimized: iconic != 0,
		})
		return 1
	})

	if err := windows.EnumWindows(cb, nil); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Process != out[j].Process {
			return out[i].Process < out[j].Process
		}
		return out[i].Title < out[j].Title
	})
	return out, nil
}

func isToolWindow(hwnd uintptr) bool {
	// GWL_EXSTYLE 是 -20。不能直接把负常量转成 uintptr(常量转换会溢出报错),
	// 要先落到一个有符号变量上,再转 —— 变量转换是允许的,按位回绕即可。
	idx := int32(gwlExStyle)

	// GetWindowLongPtrW 只在 64 位 user32 上导出。本程序只发布 amd64,
	// 所以这里不做 32 位的兼容分支。
	ex, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(idx))
	return ex&wsExToolWindow != 0
}

func windowText(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf)
}

// processName 通过 PID 取可执行文件名。取不到时返回空串 ——
// 这不是致命错误,界面上少显示一列而已。
func processName(pid uint32) string {
	if pid == 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	full := windows.UTF16ToString(buf[:size])
	if i := strings.LastIndexAny(full, `\/`); i >= 0 {
		full = full[i+1:]
	}
	return full
}
