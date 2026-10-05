//go:build windows

package input

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"sharescreen/internal/screen"
)

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	procSendInput     = user32.NewProc("SendInput")
	procGetForeground = user32.NewProc("GetForegroundWindow")
)

// INPUT 的事件类型。
const (
	inputMouse    = 0
	inputKeyboard = 1
)

// 鼠标事件标志(winuser.h 里的 MOUSEEVENTF_*)。
//
// 显式写成 uint32:这些值要直接进 MOUSEINPUT.dwFlags,不写类型的话
// 它们是无类型常量,和 ButtonEvent 里那个泛型辅助函数凑一起会被推成 int。
const (
	mouseMove        uint32 = 0x0001
	mouseLeftDown    uint32 = 0x0002
	mouseLeftUp      uint32 = 0x0004
	mouseRightDown   uint32 = 0x0008
	mouseRightUp     uint32 = 0x0010
	mouseMiddleDown  uint32 = 0x0020
	mouseMiddleUp    uint32 = 0x0040
	mouseXDown       uint32 = 0x0080
	mouseXUp         uint32 = 0x0100
	mouseWheel       uint32 = 0x0800
	mouseHWheel      uint32 = 0x1000
	mouseVirtualDesk uint32 = 0x4000
	mouseAbsolute    uint32 = 0x8000
)

// 键盘事件标志(KEYEVENTF_*)。
const (
	keyExtended = 0x0001
	keyUp       = 0x0002
	keyUnicode  = 0x0004
)

// mouseInput 对应 Win32 的 MOUSEINPUT,amd64 下是 32 字节。
//
// 最后那个字段是指针宽度,所以它要 8 字节对齐 —— 前五个字段占 20 字节,
// Go 会自动补 4 字节到偏移 24,和 C 的布局一致。
type mouseInput struct {
	dx          int32
	dy          int32
	mouseData   uint32
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

// keybdInput 对应 Win32 的 KEYBDINPUT,amd64 下是 24 字节。
type keybdInput struct {
	wVk         uint16
	wScan       uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

// input 对应 Win32 的 INPUT:一个事件类型 + 一个联合体。
//
// 联合体的尺寸取三个成员里最大的 MOUSEINPUT(32 字节),对齐要求也由它
// 决定(8)。所以类型字段后面必须显式补 4 字节 —— 少了它联合体就从偏移 4
// 开始,dwExtraInfo 会整体错位 4 字节,注入的事件会被系统当成垃圾。
//
// 这个坑本项目在音频那边踩过一次(docs/架构设计.md §5.5:WAVEFORMATEX
// 被 Go 自动补齐到 20 字节,导致格式识别永远失败),所以这里不靠"应该
// 没事",而是用下面几条编译期断言把尺寸钉死 —— 尺寸对不上就直接编不过。
type input struct {
	typ  uint32
	_    uint32
	data [32]byte
}

var (
	_ [unsafe.Sizeof(mouseInput{})]byte = [32]byte{}
	_ [unsafe.Sizeof(keybdInput{})]byte = [24]byte{}
	_ [unsafe.Sizeof(input{})]byte      = [40]byte{}
)

func mouseEvent(mi mouseInput) input {
	in := input{typ: inputMouse}
	*(*mouseInput)(unsafe.Pointer(&in.data[0])) = mi
	return in
}

func keyEvent(ki keybdInput) input {
	in := input{typ: inputKeyboard}
	*(*keybdInput)(unsafe.Pointer(&in.data[0])) = ki
	return in
}

// send 把一组事件交给系统。
//
// 一次调用可以带多个事件,它们之间不会被别的输入插队 —— 这一点对
// "先移动到按钮上、再按下"很关键,分成两次调用就可能被别的程序插进来。
func send(events []input) error {
	if len(events) == 0 {
		return nil
	}
	n, _, err := procSendInput.Call(
		uintptr(len(events)),
		uintptr(unsafe.Pointer(&events[0])),
		unsafe.Sizeof(input{}),
	)
	if int(n) != len(events) {
		return fmt.Errorf("注入 %d 个输入事件,系统只收下了 %d 个:%s",
			len(events), n, lastErrorText(err))
	}
	return nil
}

// lastErrorText 把 GetLastError 的返回值翻译成人话。
//
// 注意它**说明不了 UIPI**:被 UIPI 拦下的输入是静默丢弃的,SendInput
// 照样报告成功。想提前知道"现在点不动",只能靠 ElevatedForeground。
func lastErrorText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case 0:
			return "系统没有报告错误码"
		case windows.ERROR_ACCESS_DENIED:
			return "被系统拒绝(前台窗口可能以管理员身份运行)"
		}
		return errno.Error()
	}
	return err.Error()
}

// MoveNorm 把归一化坐标移动到采集矩形 area 上对应的屏幕位置。
//
// nx/ny 取值 [0,1],(0,0) 是画面左上角、(1,1) 是右下角。画面缩放不影响
// 归一化坐标的含义,所以分辨率档位随便改都不需要动这里。
func MoveNorm(area screen.Rect, nx, ny float64) error {
	if area.W <= 0 || area.H <= 0 {
		return fmt.Errorf("采集区域无效(%d×%d)", area.W, area.H)
	}
	desk := screen.VirtualRect()
	if desk.W <= 0 || desk.H <= 0 {
		return errors.New("拿不到虚拟桌面尺寸")
	}

	px := normPixel(nx, area.X, area.W)
	py := normPixel(ny, area.Y, area.H)

	return send([]input{mouseEvent(mouseInput{
		dx:      absCoord(px, desk.X, desk.W),
		dy:      absCoord(py, desk.Y, desk.H),
		dwFlags: mouseMove | mouseAbsolute | mouseVirtualDesk,
	})})
}

// ButtonEvent 按下或抬起一个鼠标按键。
func ButtonEvent(b Button, down bool) error {
	var (
		flag uint32
		data uint32
	)
	switch b {
	case BtnLeft:
		flag = pick(down, mouseLeftDown, mouseLeftUp)
	case BtnMiddle:
		flag = pick(down, mouseMiddleDown, mouseMiddleUp)
	case BtnRight:
		flag = pick(down, mouseRightDown, mouseRightUp)
	case BtnX1:
		flag, data = pick(down, mouseXDown, mouseXUp), 1
	case BtnX2:
		flag, data = pick(down, mouseXDown, mouseXUp), 2
	default:
		return fmt.Errorf("未知鼠标按键: %d", b)
	}

	return send([]input{mouseEvent(mouseInput{mouseData: data, dwFlags: flag})})
}

// Wheel 滚轮。dy 为纵向、dx 为横向,单位是 Windows 的 WHEEL_DELTA(120):
// 向上/向右为正。
func Wheel(dx, dy int) error {
	if dx == 0 && dy == 0 {
		return nil
	}
	// 纵向和横向是两个独立事件,一次调用送出去,避免被别处插队。
	var events []input
	if dy != 0 {
		events = append(events, mouseEvent(mouseInput{
			mouseData: uint32(int32(dy)),
			dwFlags:   mouseWheel,
		}))
	}
	if dx != 0 {
		events = append(events, mouseEvent(mouseInput{
			mouseData: uint32(int32(dx)),
			dwFlags:   mouseHWheel,
		}))
	}
	return send(events)
}

// KeyEvent 按下或抬起一个虚拟键。
//
// 用于快捷键和那些不产生文本的键(回车、退格、方向键、功能键)。
// 要"打字"请用 TypeText。
func KeyEvent(vk uint16, extended, down bool) error {
	flags := uint32(0)
	if extended {
		flags |= keyExtended
	}
	if down {
		flags |= keyUp
	}
	return send([]input{keyEvent(keybdInput{wVk: vk, dwFlags: flags})})
}

// TypeText 把一段文本原样打进当前前台窗口。
//
// 走 Unicode 事件而不是虚拟键码,是因为观众用中文输入法打出来的词在主机
// 上根本没有对应的按键序列;而且虚拟键码会被主机的键盘布局再解释一遍,
// 布局不同就打字错位。Unicode 事件同时绕开了这两个问题。
//
// 代价是它不产生虚拟键,所以 Ctrl+C 这类组合键用它按不出来 —— 那种情况
// 由 KeyEvent 负责。发送端据此分派:带 Ctrl/Alt/Win 的走按键,能产生文本
// 的走这里(见 webui 的 rc.js)。
func TypeText(s string) error {
	var events []input
	for _, u := range unicodeUnits(s) {
		events = append(events,
			keyEvent(keybdInput{wScan: u, dwFlags: keyUnicode}),
			keyEvent(keybdInput{wScan: u, dwFlags: keyUnicode | keyUp}),
		)
	}

	// 一次 SendInput 塞几百个事件没有好处,分批送。分出来的缝隙可能被
	// 本机真实的键盘输入插进来,但"打字"本来就允许这个。
	const batch = 32
	for i := 0; i < len(events); i += batch {
		end := min(i+batch, len(events))
		if err := send(events[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// ElevatedForeground 报告前台窗口的完整性级别是不是**高于我们自己**。
//
// 为什么需要它:UIPI 会静默丢弃来自低完整性进程的注入事件,SendInput 的
// 返回值和 GetLastError **都不会**报错。也就是说,当前台是一个以管理员
// 身份运行的程序时,观众那边表现为"点什么都没反应,但连接是好的"。
// 只有提前看一眼完整性级别,才能把这件事说清楚。
//
// 判断条件不是"前台窗口提权了",而是"前台提权了**而我们没有**"。本程序
// 自己也以管理员运行时(实测:从提权的终端里启动就是这种情况),注入不会
// 被拦,这时候报警就是误报 —— 而会误报的警告等于没有警告。
//
// 拿不到信息时返回 false —— 宁可不说,也不要误报。
func ElevatedForeground() bool {
	// 自己就是提权的,前台再高也拦不住我们
	if windows.GetCurrentProcessToken().IsElevated() {
		return false
	}

	hwnd, _, _ := procGetForeground.Call()
	if hwnd == 0 {
		return false
	}

	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(windows.HWND(hwnd), &pid); err != nil || pid == 0 {
		return false
	}

	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()

	return token.IsElevated()
}

// pick 在按下/抬起两种取值里挑一个。
func pick[T any](down bool, ifDown, ifUp T) T {
	if down {
		return ifDown
	}
	return ifUp
}
