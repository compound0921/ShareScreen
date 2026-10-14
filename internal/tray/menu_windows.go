//go:build windows

package tray

import (
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// afterMenuClosed 在右键菜单收起之后补一条 WM_NULL。
//
// # 为什么非补不可
//
// TrackPopupMenu 有一对固定搭配:调用之前把菜单属主窗口置为前台
// (SetForegroundWindow),等它返回之后**再往那个窗口 PostMessage 一条
// WM_NULL**。后一半不是收尾清理,是必需的 —— 少了它,系统那边"前台已经
// 换人"这件事就收不了尾,后果是菜单的下一次弹出直接失败。
//
// 现象正好是"有时候右键打不开菜单",而且有明确的规律:上一次菜单是被点
// 空白处关掉的(不是选了某一项),接下来那次就打不开。这也解释了为什么它
// 时好时坏 —— 选中的那几次一切正常,点空白关掉的那几次才留下这个尾巴。
// (出处:MS KB Q135788;TrackPopupMenu 文档 remarks 里那条 "post a WM_NULL
// message to the window that owns the menu to force a task switch"。)
//
// systray 只做了前一半(它自己的 ShowMenu 里有 SetForegroundWindow),
// 后一半没做。它没给我们插手 ShowMenu 内部的机会,所以托盘那边改成自己接
// 右键(见 tray.go 的 SetOnRClick),在它返回之后再补这一步。
//
// 这个搭配在"显示隐藏的图标"那个浮出层里尤其要紧:浮出层是 explorer 的
// 窗口,前台本来就在别人手里,状态更容易收不了尾。
func afterMenuClosed() {
	hwnd := systrayWindow()
	if hwnd == 0 {
		return
	}
	procPostMessageW.Call(uintptr(hwnd), wmNull, 0, 0)
}

// systrayClassName 是 systray 建自己的消息窗口时用的窗口类名。
//
// 这是 energye/systray 的私有实现细节 —— 它没把窗口句柄暴露出来,而上面
// 要往那个窗口投消息,只能按类名去找。哪天它改名了,这里就找不到窗口,
// afterMenuClosed 变成空操作,退回升级之前的行为:不会更糟,只是这个补丁
// 失效。
const systrayClassName = "SystrayClass"

// 窗口消息。WM_NULL 的数值是 0。
const wmNull = 0x0000

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procGetCurrentProcessID      = kernel32.NewProc("GetCurrentProcessID")
)

// 托盘窗口句柄。
//
// 它是进程级的、建出来之后到进程结束都不会变,所以找到一次就记住。右键是
// 高频操作,每次重新枚举所有顶层窗口没必要;更要紧的是 EnumWindows 的回调
// 只能注册有限个(syscall.NewCallback 的槽位用完就没了),在热路径上反复
// 注册会把它们耗光。
//
// 没找到时不缓存 —— 那多半是时机问题(枚举发生在窗口建出来之前),下次
// 右键再找一次是对的。
var (
	systrayWndMu    sync.Mutex
	systrayWndCache windows.Handle
)

// 这两个是给回调用的传递参数。只在 systrayWindow 的锁内、同一条线程上读写。
var (
	enumTargetPID uint32
	enumFound     uintptr
)

// 回调只注册这一次。
var enumWindowsCallback = syscall.NewCallback(enumProc)

// enumProc 是 EnumWindows 的回调,返回 0 表示停下。
func enumProc(hwnd uintptr, _ uintptr) uintptr {
	var name [64]uint16
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
	if n == 0 || windows.UTF16ToString(name[:n]) != systrayClassName {
		return 1 // 不是它,继续下一个
	}

	// 光认类名不够:窗口类名在整个桌面范围内是共享的,任何一个用 systray
	// 的程序(它上游那几个 fork 也一样)都注册了 "SystrayClass"。只按类名
	// 找会抓到别人家的窗口,那样 WM_NULL 就投到别人那里去了。所以连进程号
	// 一起核对。
	var pid uint32
	procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid != enumTargetPID {
		return 1
	}

	enumFound = hwnd
	return 0
}

// systrayWindow 找到 systray 自己建的那个隐藏消息窗口。
//
// 找不到返回 0。systray 的窗口是隐藏的,但 EnumWindows 照样枚举得到它 ——
// 它只跳过子窗口和销毁中的窗口,不管可见性。
func systrayWindow() windows.Handle {
	systrayWndMu.Lock()
	defer systrayWndMu.Unlock()

	if systrayWndCache != 0 {
		return systrayWndCache
	}

	pid, _, _ := procGetCurrentProcessID.Call()
	if pid == 0 {
		return 0
	}

	// 回调是同步跑在这个线程上的,所以下面两行和它之间不会有别的调用插进来。
	enumTargetPID, enumFound = uint32(pid), 0
	procEnumWindows.Call(enumWindowsCallback, 0)

	systrayWndCache = windows.Handle(enumFound)
	return systrayWndCache
}
