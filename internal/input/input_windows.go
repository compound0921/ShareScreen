//go:build windows

package input

import (
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"sharescreen/internal/screen"
)

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procSendInput     = user32.NewProc("SendInput")
	procGetForeground = user32.NewProc("GetForegroundWindow")

	procGetLastInputInfo = user32.NewProc("GetLastInputInfo")
	procGetTickCount     = kernel32.NewProc("GetTickCount")
)

// ---------- 判断主机自己在不在操作 ----------

// lastInputInfo 对应 Win32 的 LASTINPUTINFO。
//
// dwTime 是**最后一次输入事件的 tick**(毫秒),用的是 GetTickCount 那口钟。
// 注意它把注入的输入也算在内 —— SendInput 会重置这个空闲计时器,这正是
// 下面要绕开的东西。
type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// hostIdleAfterMillis 是"主机在操作"的判定窗口。
//
// 主机最后一次**不是我们发的**输入落在这个窗口之内,就认为他正在用这台
// 机器,注入一律让路;超过这个窗口没动静才算空闲。1 秒是"手停下来了"和
// "人只是停了一下"之间的折中。
const hostIdleAfterMillis = 1000

var (
	// ourInjectTick 是我们最后一次成功注入的时刻。
	ourInjectTick atomic.Uint32
	// foreignInputTick 是最近一次**不是我们干的**输入的时刻。
	foreignInputTick atomic.Uint32
)

func tick() uint32 {
	t, _, _ := procGetTickCount.Call()
	return uint32(t)
}

// sampleForeignInput 看一眼最近这次输入是不是我们干的,是别人就记下来。
//
// **必须在每次注入之前调**,理由见 HostActive 里那段。
func sampleForeignInput() {
	li := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if ok, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li))); ok == 0 {
		return // 拿不到当没有,宁可让注入继续也不要凭一个失败的去拦它
	}

	// 比我们上次注入还晚 → 中间有人动过,而那个人不可能是我们,只能是
	// 本机用户。
	//
	// 相等时**不算**外来输入,而且这里要的正是"大于"这个方向:SendInput
	// 记下的时刻和我们随后读到的 tick 之间可能差一毫秒(正好跨过 tick
	// 边界),取大于才不会把自己的事件认成主机的。反过来漏判主机一次输入
	// 的代价小得多 —— 他的手还在动,下一次采样照样抓得到。
	if int32(li.dwTime-ourInjectTick.Load()) > 0 {
		foreignInputTick.Store(li.dwTime)
	}
}

// HostActive 报告本机用户此刻在不在操作这台机器。
//
// # 为什么要绕这么一圈
//
// GetLastInputInfo 报的是"本会话最后一次输入",而它**把注入的输入也算进去**
// (SendInput 会重置那个空闲计时器)。直接拿它当"主人在不在"用,主机的鼠标
// 只要被观众带着动过一下,就会永远显得"刚操作过" —— 而我们要的恰恰是分清
// 哪次是观众发过来的、哪次是本机用户自己动的。
//
// 办法是搭上"我们知道自己什么时候注入过"这件事:在每次注入**之前**看一眼
// GetLastInputInfo,那个时刻要是晚于我们上次注入的时刻,中间那次输入就
// 只能是本机用户的。看早了没关系(下次注入前还会看),看晚了才会漏 ——
// 所以是注入前看,不是注入后看。
//
// 从没注入过的时候(ourInjectTick 为 0),任何输入都算本机用户的,这也是对的:
// 我们还没动过手,系统里那些输入没有别的来源。
//
// 拿不到信息时返回 false —— 宁可让注入继续,也不要凭一个失败的去拦它。
func HostActive() bool {
	sampleForeignInput()

	last := foreignInputTick.Load()
	if last == 0 {
		return false // 还没观察到任何输入,当他没在操作
	}
	// 用有符号差值比大小:tick 是 32 位、约 49.7 天绕一圈,直接比大小会
	// 在绕圈那一刻整整错一次。
	return int32(tick()-last) < hostIdleAfterMillis
}

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

	// 动手之前先看一眼有没有人来过。顺序不能反:发送之后再采样,刚发出去的
	// 那批事件会把"最后一次输入"刷成我们自己的,主机夹在中间的操作就被
	// 盖掉了。见 HostActive。
	sampleForeignInput()

	n, _, err := procSendInput.Call(
		uintptr(len(events)),
		uintptr(unsafe.Pointer(&events[0])),
		unsafe.Sizeof(input{}),
	)
	if int(n) != len(events) {
		return fmt.Errorf("注入 %d 个输入事件,系统只收下了 %d 个:%s",
			len(events), n, lastErrorText(err))
	}

	// 记下我们动手的时刻:下一次采样时,晚于它的输入就不是我们发的了。
	ourInjectTick.Store(tick())
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
