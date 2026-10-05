//go:build windows

package clipboard

import (
	"errors"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
	procGlobalSize   = kernel32.NewProc("GlobalSize")
	procGlobalFree   = kernel32.NewProc("GlobalFree")

	// 搬运内存用,见下面 copyToGlobal / copyFromGlobal 的说明。
	procRtlMoveMemory = kernel32.NewProc("RtlMoveMemory")
)

const (
	// cfUnicodeText 是"以 NUL 结尾的 UTF-16 串"这种剪贴板格式。
	//
	// 不用 CF_TEXT(ANSI 版):那种格式的编码取决于当前代码页,中文
	// 经过它的时候会变成乱码。
	cfUnicodeText = 13

	gmemMoveable = 0x0002

	// maxGlobalBytes 是愿意从剪贴板里读的字节数上限。比 MaxText 大一倍
	// 是给 UTF-16 留的余量(每个字符两字节)。
	maxGlobalBytes = MaxText * 2

	// 抢剪贴板的重试参数。
	//
	// OpenClipboard 会失败 —— 别的进程正占着它是常态(用户开着某个程序的
	// 复制对话框,或者另一个剪贴板工具也在轮询)。这不是故障,退让一下
	// 重试,还不行就跳过这一轮。
	openAttempts = 10
	openBackoff  = 20 * time.Millisecond
)

// sequence 返回系统给剪贴板的序列号:内容一变它就加一。
//
// 用它判断"变没变",而不是每次读一遍内容去比对:剪贴板里可能躺着几 MB
// 的东西,而这里每 800 毫秒要问一次。这个调用只返回一个整数。
func sequence() uint32 {
	v, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(v)
}

// readText 读出剪贴板里的文本。调用方必须已经持有 Sync 的锁。
func readText() (string, error) {
	release, err := open()
	if err != nil {
		return "", err
	}
	defer release()

	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		// 剪贴板是空的,或者里面不是文本(图片、文件)。两者都不是错误。
		return "", nil
	}
	return copyFromGlobal(h)
}

// writeText 把文本放进剪贴板。调用方必须已经持有 Sync 的锁。
func writeText(text string) error {
	// UTF16FromString 会自己补上结尾的 NUL,正是 SetClipboardData 要的。
	units, err := windows.UTF16FromString(text)
	if err != nil {
		// 文本里含 NUL 字符。剪贴板文本没法表达它,直接拒绝。
		return err
	}

	release, err := open()
	if err != nil {
		return err
	}
	defer release()

	// EmptyClipboard 是必须的:SetClipboardData 只负责加一种格式,
	// 不清空的话旧内容会作为**另一个格式**留在剪贴板里,别人粘贴出来的
	// 还是旧的东西。
	procEmptyClipboard.Call()

	h, _, allocErr := procGlobalAlloc.Call(gmemMoveable, uintptr(len(units)*2))
	if h == 0 {
		return errors.New("剪贴板内存分配失败: " + allocErr.Error())
	}

	if err := copyToGlobal(h, units); err != nil {
		procGlobalFree.Call(h)
		return err
	}

	if r, _, callErr := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		// 失败时这块内存还归我们,要自己释放。
		//
		// 成功时**绝不能**释放:所有权已经交给系统了。释放它会让剪贴板里
		// 留下一个悬空指针 —— 表现为粘贴出来是乱码或者别的程序的残留数据,
		// 而且完全没法归因到这一行。
		procGlobalFree.Call(h)
		return errors.New("写入剪贴板失败: " + callErr.Error())
	}
	return nil
}

// ---------- 内存搬运 ----------
//
// 这里刻意绕开了 (*uint16)(unsafe.Pointer(ptr)) 这种写法。
//
// 那种写法是对的(这块内存来自 GlobalAlloc,不归 Go 的 GC 管,不会被搬走),
// 但 `go vet` 的 unsafeptr 检查会报 "possible misuse of unsafe.Pointer",
// 而本项目一直在保持 `go vet ./...` 干净(各平台桩文件就是为了这个才存在的)。
//
// 换成的写法是:Windows 那边的地址自始至终只是一个 uintptr,作为参数传给
// RtlMoveMemory;参与转换的只有 Go 自己那一边的指针,方向是
// unsafe.Pointer → uintptr —— 这个方向是允许的,syscall 就是这么用的,
// vet 也不会报。

// copyToGlobal 把 Go 的 UTF-16 切片写进一块 Windows 全局内存。
func copyToGlobal(h uintptr, units []uint16) error {
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return errors.New("剪贴板内存锁定失败")
	}
	n := uintptr(len(units) * 2)
	procRtlMoveMemory.Call(ptr, uintptr(unsafe.Pointer(&units[0])), n)
	procGlobalUnlock.Call(h)

	runtime.KeepAlive(units)
	return nil
}

// copyFromGlobal 把 Windows 全局内存里的 UTF-16 串读成 Go 字符串。
func copyFromGlobal(h uintptr) (string, error) {
	size, _, _ := procGlobalSize.Call(h)
	if size == 0 || size > maxGlobalBytes {
		// 0 表示拿不到大小;过大说明这不是一段文本(图片之类)。
		return "", nil
	}

	buf := make([]uint16, size/2)
	if len(buf) == 0 {
		return "", nil
	}

	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return "", errors.New("剪贴板内存锁定失败")
	}
	procRtlMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), ptr, size)
	procGlobalUnlock.Call(h)

	// UTF16ToString 在第一个 NUL 处截断,正好是 CF_UNICODETEXT 的约定。
	// GlobalSize 给的是整块内存的大小,里面可能有没写满的尾巴。
	s := windows.UTF16ToString(buf)

	runtime.KeepAlive(buf)
	return s, nil
}

// open 打开剪贴板,返回一个关闭函数。
func open() (func(), error) {
	for i := 0; i < openAttempts; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return func() { procCloseClipboard.Call() }, nil
		}
		if i < openAttempts-1 {
			time.Sleep(openBackoff)
		}
	}
	return nil, errors.New("剪贴板被别的程序占用")
}
