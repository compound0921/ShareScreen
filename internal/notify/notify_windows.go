//go:build windows

package notify

import (
	"context"
	"errors"
	"log"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 本程序没有自己的消息泵(GUI 子系统、无控制台),所以这个包自建窗口类、
// 自建消息循环,并 runtime.LockOSThread 把它钉在一条 OS 线程上。
//
// systray 也是在它自己那条锁定的线程上跑自己的循环的(systray.go:24 +
// nativeLoop),两条线程各一个消息泵是 Win32 的常规用法,互不干涉。
//
// # 整个界面是自绘的
//
// 没有用 STATIC / BUTTON 这些系统控件:它们画出来是带三维边框和系统主题
// 的样子,要扁平化就只能自己画。代价是 Tab 之类的键盘导航没有了 ——
// 与"不要用键盘确认"的方向一致。
//
// 实测踩到的坑:GetCurrentThreadId 在 kernel32 不在 user32,放错 dll 不是
// "找不到就跳过",LazyProc.Call 会直接 panic。

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procSetWindowRgn             = user32.NewProc("SetWindowRgn")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop         = user32.NewProc("BringWindowToTop")
	procAttachThreadInput        = user32.NewProc("AttachThreadInput")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procSystemParametersInfoW    = user32.NewProc("SystemParametersInfoW")
	procGetMonitorInfoW          = user32.NewProc("GetMonitorInfoW")
	procMonitorFromPoint         = user32.NewProc("MonitorFromPoint")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procGetDpiForSystem          = user32.NewProc("GetDpiForSystem")
	procLoadCursorW              = user32.NewProc("LoadCursorW")
	procSetCursor                = user32.NewProc("SetCursor")
	procIsWindow                 = user32.NewProc("IsWindow")
	procBeginPaint               = user32.NewProc("BeginPaint")
	procEndPaint                 = user32.NewProc("EndPaint")
	procInvalidateRect           = user32.NewProc("InvalidateRect")
	procTrackMouseEvent          = user32.NewProc("TrackMouseEvent")
	procSetTimer                 = user32.NewProc("SetTimer")
	procKillTimer                = user32.NewProc("KillTimer")
	procDrawTextW                = user32.NewProc("DrawTextW")

	procCreateFontW        = gdi32.NewProc("CreateFontW")
	procCreateSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	procCreatePen          = gdi32.NewProc("CreatePen")
	procCreateRoundRectRgn = gdi32.NewProc("CreateRoundRectRgn")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procGetStockObject     = gdi32.NewProc("GetStockObject")
	procRoundRect          = gdi32.NewProc("RoundRect")
	procSetTextColor       = gdi32.NewProc("SetTextColor")
	procSetBkMode          = gdi32.NewProc("SetBkMode")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	// GetCurrentThreadId 在 kernel32,不在 user32 —— 放错 dll 不是"找不到
	// 就跳过",LazyProc.Call 会直接 panic。实测踩到过。
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
)

// ── 常量 ──

const (
	wsPopup = 0x80000000

	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080 // 不进 Alt+Tab,也不占任务栏

	wmClose       = 0x0010
	wmPaint       = 0x000F
	wmEraseBkgnd  = 0x0014
	wmKeyDown     = 0x0100
	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmDestroy     = 0x0002
	wmMouseLeave  = 0x02A3
	wmTimer       = 0x0113

	// 倒计时刷新的定时器 ID,只在本窗口用,取值随意
	countdownTimerID  = 1
	countdownInterval = 1000 // 毫秒

	vkEscape = 0x1B
	vkReturn = 0x0D

	// HWND_TOPMOST 是 (HWND)-1,所有位都是 1
	hwndTopmost   = ^uintptr(0)
	swpShowWindow = 0x0040

	spiGetWorkArea = 0x0030

	monitorDefaultToNearest = 0x00000002

	idcArrow = 32512
	idcHand  = 32649

	fwBold   = 700
	fwNormal = 400

	psSolid = 0
	psNull  = 8

	nullBrush   = 5 // GetStockObject(NULL_BRUSH):只描边不填充
	transparent = 1 // SetBkMode:文字背景透明,否则每行后面拖一条底色

	dtLeft       = 0x0000
	dtCenter     = 0x0001
	dtVCenter    = 0x0004
	dtSingleLine = 0x0020
	dtWordBreak  = 0x0010
	// 正文里可能出现 & 之类的字符,不加这个会被当成助记符前缀吃掉
	dtNoPrefix = 0x8000

	tmeLeave = 0x00000002

	// 按钮索引
	btnNone  = -1
	btnAllow = 0
	btnDeny  = 1
)

// 配色。ColorRef 是 0x00BBGGRR(蓝在最高字节),不是 RGB 顺序。
const (
	colBg          = 0x00FFFFFF // 白卡片
	colBorder      = 0x00E8E8E8
	colTitle       = 0x00202020
	colBody        = 0x00666666
	colDenyBg      = 0x00F2F2F2
	colDenyBgHover = 0x00E2E2E2
	colDenyFg      = 0x00333333
	colAllowBg     = 0x00F67D2D // RGB(45,125,246) 蓝
	colAllowHover  = 0x00DC691E // RGB(30,105,220)
	colAllowFg     = 0x00FFFFFF
)

// 逻辑尺寸(96 DPI 下的像素),实际用前都乘 dpi/96。
const (
	baseWidth   = 360
	baseHeight  = 136
	baseMargin  = 16
	basePad     = 18
	baseRadius  = 10
	baseBtnW    = 88
	baseBtnH    = 34
	baseBtnGap  = 10
	baseTitlePt = 14
	baseBodyPt  = 12
)

// ── Win32 结构体 ──

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

func (r rect) contains(x, y int32) bool {
	return x >= r.Left && x < r.Right && y >= r.Top && y < r.Bottom
}

type msg struct {
	Hwnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

type paintStruct struct {
	Hdc         uintptr
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type trackMouseEvent struct {
	CbSize      uint32
	DwFlags     uint32
	HwndTrack   windows.Handle
	DwHoverTime uint32
}

// ── 每个窗口的状态 ──

// popupState 挂在一块窗口上。
//
// 用一张按 HWND 索引的表,而不是把 Go 指针塞进 GWLP_USERDATA:GC 不扫描
// LONG_PTR,那样写要靠 runtime.KeepAlive 兜着,比这个功能本身还复杂。
type popupState struct {
	mu sync.Mutex

	hwnd windows.Handle
	w, h int32
	dpi  int // 逻辑尺寸换算用,建窗口时定下来

	title, body string
	allowR      rect
	denyR       rect
	bodyR       rect

	// deadline 是这份申请作废的时刻。倒计时按它现算,每秒重绘一次。
	// 零值表示不显示倒计时。
	deadline time.Time

	hover    int32
	pressed  int32
	tracking bool

	// decided 表示用户点过按钮或按过键。没有它就说明是超时/被取消/直接关窗。
	decided bool
	choice  Choice

	destroyed bool
	done      chan struct{}

	fontTitle windows.Handle
	fontBody  windows.Handle

	// prevFg 是弹窗出现前的前台窗口,关闭时把焦点还给它。
	prevFg windows.Handle
}

var (
	statesMu sync.Mutex
	states   = map[windows.Handle]*popupState{}
)

func lookupState(h windows.Handle) *popupState {
	statesMu.Lock()
	defer statesMu.Unlock()
	return states[h]
}

// decide 记下用户的选择并关窗。重复调用只有第一次算数。
func (st *popupState) decide(c Choice) {
	st.mu.Lock()
	if !st.decided {
		st.decided, st.choice = true, c
	}
	st.mu.Unlock()
	st.requestClose()
}

// requestClose 请求关窗。
//
// 跨线程只能 PostMessageW:DestroyWindow 必须由创建窗口的那条线程调用,
// SendMessageW 会阻塞到对方处理完(UI 线程正忙时就死锁了)。
//
// 整个判断加投递都在锁内。PostMessageW 不阻塞,所以不会和 WM_DESTROY
// 抢锁死锁;而放在锁外的话,检查完到投递之间窗口可能已经被销毁、
// 句柄甚至被系统回收给别的窗口 —— 那次投递就打到别人家去了。
func (st *popupState) requestClose() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.hwnd == 0 || st.destroyed {
		return
	}
	procPostMessageW.Call(uintptr(st.hwnd), wmClose, 0, 0)
}

// buttonAt 返回坐标落在哪个按钮上。
func (st *popupState) buttonAt(x, y int32) int32 {
	if st.allowR.contains(x, y) {
		return btnAllow
	}
	if st.denyR.contains(x, y) {
		return btnDeny
	}
	return btnNone
}

// setHover 更新悬停状态,变了才重绘。
func (st *popupState) setHover(idx int32) {
	st.mu.Lock()
	changed := st.hover != idx
	st.hover = idx
	st.mu.Unlock()
	if changed {
		st.invalidateButtons()
	}
}

func (st *popupState) invalidateButtons() {
	st.mu.Lock()
	a, d := st.allowR, st.denyR
	hwnd := st.hwnd
	st.mu.Unlock()
	if hwnd == 0 {
		return
	}
	procInvalidateRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&a)), 0)
	procInvalidateRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&d)), 0)
}

// ── 窗口类 ──

const className = "ShareScreenControlRequest"

var registerOnce struct {
	sync.Once
	err error
}

func registerClass() error {
	registerOnce.Do(func() {
		hInst, _, _ := procGetModuleHandleW.Call(0)
		cursor, _, _ := procLoadCursorW.Call(0, uintptr(idcArrow))

		wc := wndClassExW{
			CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
			LpfnWndProc:   syscall.NewCallback(wndProc),
			HInstance:     windows.Handle(hInst),
			HCursor:       windows.Handle(cursor),
			HbrBackground: 0, // 全部自绘,不给背景刷子,免得先被刷一遍再被盖掉
			LpszClassName: windows.StringToUTF16Ptr(className),
		}
		if ret, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
			registerOnce.err = err
		}
	})
	return registerOnce.err
}

// ── 对外入口 ──

// Ask 弹出右下角通知并阻塞,直到用户选择或 ctx 结束。
//
// 见 notify.go 的包注释:返回 ChoiceNone 表示没有决定,调用方什么都不做。
func Ask(ctx context.Context, req Request) Choice {
	// 窗口只能在创建它的线程上被销毁,消息循环也必须固定在同一条线程上。
	// LockOSThread 之后这条线程就专属于这个弹窗,goroutine 退出时线程一起
	// 结束 —— 不会把一条锁定的线程漏在进程里。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := registerClass(); err != nil {
		log.Printf("远控弹窗:注册窗口类失败,这次不弹了: %v", err)
		return ChoiceNone
	}

	title, body := Describe(req)
	st := &popupState{done: make(chan struct{}), hover: btnNone, pressed: btnNone}

	if err := st.create(title, body, req.Timeout); err != nil {
		log.Printf("远控弹窗:创建窗口失败,这次不弹了: %v", err)
		return ChoiceNone
	}

	statesMu.Lock()
	states[st.hwnd] = st
	statesMu.Unlock()

	// ctx 结束就关窗。这条 goroutine 只做一件事:投一个 WM_CLOSE。
	go func() {
		select {
		case <-ctx.Done():
			st.requestClose()
		case <-st.done:
		}
	}()

	st.messageLoop()

	close(st.done)
	statesMu.Lock()
	delete(states, st.hwnd)
	statesMu.Unlock()

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.decided {
		return st.choice
	}
	return ChoiceNone
}

func (st *popupState) messageLoop() {
	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(ret) {
		case 0: // WM_QUIT
			return
		case -1:
			// 出错。**必须跳出** —— 当返回值是 -1 而循环继续,
			// 它会立刻返回 -1 再来一次,变成一个吃满一个核的死循环。
			st.requestClose()
			return
		}

		// 键盘先过我们这一关,必须在 TranslateMessage/DispatchMessage 之前。
		switch keyChoice(m.Message, m.WParam) {
		case keyDeny:
			st.decide(ChoiceDeny)
			continue
		case keyConsume:
			continue
		}

		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// keyAction 是一条键盘消息该怎么处理。
type keyAction int

const (
	keyIgnore  keyAction = iota // 交回系统
	keyConsume                  // 吃掉,但不产生任何选择
	keyDeny                     // 拒绝
)

// keyChoice 决定这条键盘消息怎么处理。
//
// # Enter 必须被吃掉,而且不能产生任何选择
//
// 这个弹窗是抢焦点弹出来的 —— 用户那个回车几乎肯定是打给别的窗口的,
// 不能让它在这里产生任何效果。
//
// 注意"什么效果都不产生"不等于"不管它":只要不截获,回车就会落到
// 对话框管理器手里去激活默认按钮。所以这里**显式吃掉**它,而且按钮
// 也不是系统控件、没有默认按钮这回事 —— 两道都要成立,少一道回车就
// 又能用了。
//
// Esc 是另一回事:它不是"确认"键,而且方向是安全的(拒绝)。窗口没有
// 标题栏也没有关闭按钮,Esc 是唯一的"我不想管这件事"。Alt+F4 走
// WM_CLOSE,那条路不记选择,等于超时。
func keyChoice(message uint32, wparam uintptr) keyAction {
	if message != wmKeyDown {
		return keyIgnore
	}
	switch uint16(wparam & 0xFFFF) {
	case vkReturn:
		return keyConsume
	case vkEscape:
		return keyDeny
	}
	return keyIgnore
}

// ── 窗口过程 ──

func wndProc(hwnd windows.Handle, message uint32, wparam, lparam uintptr) uintptr {
	st := lookupState(hwnd)
	if st == nil {
		// 窗口还没登记(创建过程中就会来几条消息),照默认处理。
		ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wparam, lparam)
		return ret
	}

	switch message {
	case wmEraseBkgnd:
		// 整个客户区都在 WM_PAINT 里画完,别再刷一遍背景 ——
		// 刷了再盖会闪。
		return 1

	case wmPaint:
		st.paint()
		return 0

	case wmMouseMove:
		x, y := int32(lparam&0xFFFF), int32((lparam>>16)&0xFFFF)
		st.mu.Lock()
		tracking := st.tracking
		st.mu.Unlock()
		if !tracking {
			tme := trackMouseEvent{
				CbSize:    uint32(unsafe.Sizeof(trackMouseEvent{})),
				DwFlags:   tmeLeave,
				HwndTrack: hwnd,
			}
			procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
			st.mu.Lock()
			st.tracking = true
			st.mu.Unlock()
		}
		idx := st.buttonAt(x, y)
		st.setHover(idx)
		// 悬停在按钮上时给个手型,这是"这里能点"的唯一提示 ——
		// 扁平样式没有凸起可以暗示。
		if idx != btnNone {
			hand, _, _ := procLoadCursorW.Call(0, uintptr(idcHand))
			procSetCursor.Call(hand)
		}
		return 0

	case wmMouseLeave:
		st.mu.Lock()
		st.tracking = false
		st.mu.Unlock()
		st.setHover(btnNone)
		return 0

	case wmLButtonDown:
		x, y := int32(lparam&0xFFFF), int32((lparam>>16)&0xFFFF)
		idx := st.buttonAt(x, y)
		st.mu.Lock()
		st.pressed = idx
		st.mu.Unlock()
		return 0

	case wmLButtonUp:
		x, y := int32(lparam&0xFFFF), int32((lparam>>16)&0xFFFF)
		st.mu.Lock()
		pressed := st.pressed
		st.pressed = btnNone
		st.mu.Unlock()

		// 按下和抬起要落在同一个按钮上才算点击 —— 和系统按钮一致:
		// 按下之后拖出去再松手是取消。
		if idx := st.buttonAt(x, y); idx != btnNone && idx == pressed {
			switch idx {
			case btnAllow:
				st.decide(ChoiceAllow)
			case btnDeny:
				st.decide(ChoiceDeny)
			}
		}
		return 0

	case wmTimer:
		// 倒计时每秒重绘一次。只刷正文那一块,整张卡重绘会闪。
		st.mu.Lock()
		br, has := st.bodyR, !st.deadline.IsZero()
		st.mu.Unlock()
		if has {
			procInvalidateRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&br)), 0)
		}
		return 0

	case wmClose:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0

	case wmDestroy:
		st.mu.Lock()
		st.destroyed = true
		fontTitle, fontBody := st.fontTitle, st.fontBody
		prev := st.prevFg
		st.mu.Unlock()

		procKillTimer.Call(uintptr(hwnd), countdownTimerID)

		if fontTitle != 0 {
			procDeleteObject.Call(uintptr(fontTitle))
		}
		if fontBody != 0 {
			procDeleteObject.Call(uintptr(fontBody))
		}

		// 把焦点还给弹窗出现之前的那个窗口。它可能已经被关了,
		// 所以要用 IsWindow 挡一下过期句柄。
		if prev != 0 {
			if ok, _, _ := procIsWindow.Call(uintptr(prev)); ok != 0 {
				procSetForegroundWindow.Call(uintptr(prev))
			}
		}

		procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wparam, lparam)
	return ret
}

// ── 绘制 ──

// paint 把整张卡片画出来。没有子控件,所有东西都在这里画。
func (st *popupState) paint() {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(uintptr(st.hwnd), uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(uintptr(st.hwnd), uintptr(unsafe.Pointer(&ps)))

	st.mu.Lock()
	w, h := st.w, st.h
	title, body := st.title, st.body
	allowR, denyR := st.allowR, st.denyR
	hover, pressed := st.hover, st.pressed
	fontTitle, fontBody := st.fontTitle, st.fontBody
	deadline := st.deadline
	st.mu.Unlock()

	radius := int32(st.scale(baseRadius))

	// 卡片本体 + 1px 描边。窗口有圆角区域(见 create),这里画的圆角
	// 和那个区域对齐,所以四个角是干净的。
	fillRound(hdc, rect{0, 0, w, h}, colBg, radius)
	strokeRound(hdc, rect{0, 0, w, h}, colBorder, radius)

	pad := int32(st.scale(basePad))
	drawText(hdc, title, rect{pad, pad - 2, w - pad, pad + 20},
		fontTitle, colTitle, dtLeft|dtNoPrefix)

	// 正文是两行:固定的"来自谁",加上每秒都在变的倒计时。
	//
	// 倒计时在这里现算,不存进状态 —— 它每秒都变,存下来就得再安排一次
	// 写入;直接按 deadline 算,重绘多少次都是对的。
	if !deadline.IsZero() {
		body += "\n" + CountdownText(time.Until(deadline))
	}
	drawText(hdc, body, rect{pad, pad + 24, w - pad, denyR.Top - 8},
		fontBody, colBody, dtLeft|dtWordBreak|dtNoPrefix)

	drawButton(hdc, denyR, "拒绝", colDenyBg, colDenyBgHover, colDenyFg,
		fontBody, hover == btnDeny, pressed == btnDeny, int32(st.scale(7)))
	drawButton(hdc, allowR, "允许", colAllowBg, colAllowHover, colAllowFg,
		fontBody, hover == btnAllow, pressed == btnAllow, int32(st.scale(7)))
}

func drawButton(hdc uintptr, r rect, label string, bg, bgHover, fg uint32,
	font windows.Handle, hover, pressed bool, radius int32) {

	switch {
	case pressed:
		// 按下时压暗一档,给出触感
		bg = bgHover
	case hover:
		bg = bgHover
	}
	fillRound(hdc, r, bg, radius)

	// 文字水平垂直居中。用 DT_CENTER|DT_VCENTER|DT_SINGLELINE 那套需要
	// 配合 DT_ 的组合,这里直接算一个内缩矩形再居中更省事。
	inner := rect{r.Left, r.Top - 1, r.Right, r.Bottom - 1}
	drawText(hdc, label, inner, font, fg,
		dtCenter|dtVCenter|dtSingleLine|dtNoPrefix)
}

// fillRound 用纯色填一个圆角矩形,不描边。
func fillRound(hdc uintptr, r rect, color uint32, radius int32) {
	brush, _, _ := procCreateSolidBrush.Call(uintptr(color))
	// RoundRect 一定会用当前的笔描边,给它一支"不存在"的笔就不画边
	pen, _, _ := procCreatePen.Call(psNull, 0, 0)
	oldB, _, _ := procSelectObject.Call(hdc, brush)
	oldP, _, _ := procSelectObject.Call(hdc, pen)

	procRoundRect.Call(hdc, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right), uintptr(r.Bottom),
		uintptr(radius*2), uintptr(radius*2))

	procSelectObject.Call(hdc, oldB)
	procSelectObject.Call(hdc, oldP)
	procDeleteObject.Call(brush)
	procDeleteObject.Call(pen)
}

// strokeRound 只描 1px 边,不填充。
func strokeRound(hdc uintptr, r rect, color uint32, radius int32) {
	pen, _, _ := procCreatePen.Call(psSolid, 1, uintptr(color))
	hollow, _, _ := procGetStockObject.Call(nullBrush)
	oldP, _, _ := procSelectObject.Call(hdc, pen)
	oldB, _, _ := procSelectObject.Call(hdc, hollow)

	procRoundRect.Call(hdc, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-1), uintptr(r.Bottom-1),
		uintptr(radius*2), uintptr(radius*2))

	procSelectObject.Call(hdc, oldP)
	procSelectObject.Call(hdc, oldB)
	procDeleteObject.Call(pen)
}

// drawText 画一段文字。
func drawText(hdc uintptr, s string, r rect, font windows.Handle, color uint32, flags uint32) {
	if s == "" || font == 0 {
		return
	}
	u := windows.StringToUTF16(s)
	n := len(u) - 1 // 末尾那个 NUL 不算进长度
	if n <= 0 {
		return
	}

	procSelectObject.Call(hdc, uintptr(font))
	procSetBkMode.Call(hdc, transparent)
	procSetTextColor.Call(hdc, uintptr(color))
	procDrawTextW.Call(hdc,
		uintptr(unsafe.Pointer(&u[0])),
		uintptr(n),
		uintptr(unsafe.Pointer(&r)),
		uintptr(flags))
	// 上面把 &u[0] 转成了 uintptr,GC 就看不到这个引用了 ——
	// 不留这一句,这段文字有可能在绘制途中被回收掉。
	runtime.KeepAlive(u)
}

// ── 创建与布局 ──

func (st *popupState) create(title, body string, timeout time.Duration) error {
	dpi := systemDPI()
	scale := func(v int) int { return v * dpi / 96 }

	work, err := workArea()
	if err != nil {
		return err
	}

	w, h := scale(baseWidth), scale(baseHeight)
	margin := scale(baseMargin)
	pad := scale(basePad)
	btnW, btnH, btnGap := scale(baseBtnW), scale(baseBtnH), scale(baseBtnGap)

	x := int(work.Right) - w - margin
	y := int(work.Bottom) - h - margin

	// 弹窗出现前的前台窗口,关闭时要还回去。拿不到(锁屏、安全桌面)就跳过。
	prevFg, _, _ := procGetForegroundWindow.Call()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	cls := windows.StringToUTF16Ptr(className)
	winTitle := windows.StringToUTF16Ptr(title)
	hwnd, _, callErr := procCreateWindowExW.Call(
		wsExTopmost|wsExToolWindow,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(winTitle)),
		wsPopup, // 没有 WS_BORDER:边框自己画,系统那个是三维的
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		0, 0, hInst, 0,
	)
	runtime.KeepAlive(cls)
	runtime.KeepAlive(winTitle)
	if hwnd == 0 {
		return callErr
	}

	btnY := int32(h - pad - btnH)
	allowL := int32(w - pad - btnW)

	st.mu.Lock()
	st.hwnd = windows.Handle(hwnd)
	st.dpi = dpi
	st.w, st.h = int32(w), int32(h)
	st.title, st.body = title, body
	st.allowR = rect{allowL, btnY, allowL + int32(btnW), btnY + int32(btnH)}
	st.denyR = rect{allowL - int32(btnGap+btnW), btnY, allowL - int32(btnGap), btnY + int32(btnH)}
	// 倒计时每秒重绘这一块,单独记下来 —— 整张卡重绘会闪。
	st.bodyR = rect{int32(pad), int32(pad + 24), int32(w - pad), st.denyR.Top - 8}
	if timeout > 0 {
		st.deadline = time.Now().Add(timeout)
	}
	st.fontTitle = createFont(scale(baseTitlePt), fwBold)
	st.fontBody = createFont(scale(baseBodyPt), fwNormal)
	st.prevFg = windows.Handle(prevFg)
	st.mu.Unlock()

	// 圆角窗口区域。没有它四个角会露出方形底 —— 客户区是方的,
	// 我们只在里面画了圆角卡片。
	region, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(w+1), uintptr(h+1),
		uintptr(scale(baseRadius)*2), uintptr(scale(baseRadius)*2))
	if region != 0 {
		procSetWindowRgn.Call(hwnd, region, 1)
	}

	procSetWindowPos.Call(hwnd, hwndTopmost,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h), swpShowWindow)

	// 倒计时每秒重绘一次。没有倒计时就不必让它空转。
	if !st.deadline.IsZero() {
		procSetTimer.Call(hwnd, countdownTimerID, countdownInterval, 0)
	}

	// 抢前台。必须在显示之后做,否则窗口还没成为可见的顶层窗口,
	// SetForegroundWindow 不认。详见 stealFocus。
	stealFocus(st.hwnd, prevFg)

	// 光标初始停在"拒绝"上 —— 最安全的那个。只是视觉提示,
	// 不影响点击(点击按坐标判定)。
	st.setHover(btnDeny)
	return nil
}

// scale 按窗口 DPI 把逻辑尺寸换成物理像素。
func (st *popupState) scale(v int) int {
	if st.dpi <= 0 {
		return v
	}
	return v * st.dpi / 96
}

// stealFocus 把前台抢过来。
//
// SetForegroundWindow 在前台不属于本进程时会被系统直接拒绝 —— 这是有意
// 的设计,防止后台程序乱抢。可行的绕法是:临时把自己和当前前台线程的输入
// 队列接在一起,这时我们就算"前台的一部分",抢过来再断开。
//
// 锁屏/安全桌面上拿不到前台窗口(prev == 0),AttachThreadInput 也会因为
// 完整性级别而失败 —— 那种情况就只让它以 TOPMOST 显示出来,不报错。
// 这正是 input_windows.go 里 UIPI 那一节的同类限制。
func stealFocus(hwnd windows.Handle, prev uintptr) {
	if prev != 0 {
		fgTid, _, _ := procGetWindowThreadProcessID.Call(prev, 0)
		myTid, _, _ := procGetCurrentThreadID.Call()
		if fgTid != 0 && fgTid != myTid {
			procAttachThreadInput.Call(myTid, fgTid, 1)
			procSetForegroundWindow.Call(uintptr(hwnd))
			procBringWindowToTop.Call(uintptr(hwnd))
			procAttachThreadInput.Call(myTid, fgTid, 0)
			return
		}
	}
	procSetForegroundWindow.Call(uintptr(hwnd))
	procBringWindowToTop.Call(uintptr(hwnd))
}

// createFont 建一支字体。返回 0 表示系统会用默认字体,不影响可用性。
//
// 指定 "Microsoft YaHei UI":不指定的话中文可能落到点阵回退字体上,
// 高 DPI 下很难看。
func createFont(heightPt, weight int) windows.Handle {
	face := windows.StringToUTF16Ptr("Microsoft YaHei UI")
	h, _, _ := procCreateFontW.Call(
		uintptr(^uintptr(heightPt-1)), // 负值 = 字符高度(不是单元格高度)
		0, 0, 0,
		uintptr(weight),
		0, 0, 0,
		1, // DEFAULT_CHARSET
		0, 0,
		5, // CLEARTYPE_QUALITY
		0,
		uintptr(unsafe.Pointer(face)),
	)
	runtime.KeepAlive(face)
	return windows.Handle(h)
}

// systemDPI 返回主显示器的 DPI。
//
// 进程是系统 DPI 感知的(screen.SetDPIAware),所以拿到的就是物理像素的
// 换算基准。GetDpiForSystem 是 Win10 1607 才有的,取不到就按 96 算 ——
// 那样弹窗会偏小,但不会崩。
//
// 局限:混 DPI 的多显示器下,弹窗按主屏比例渲染。它出现在哪块屏上就以
// 哪块屏的比例渲染(GetDpiForWindow)在这里做不到 —— 尺寸得在创建之前
// 定下来,而那时候还没有窗口。
func systemDPI() int {
	if err := procGetDpiForSystem.Find(); err != nil {
		return 96
	}
	dpi, _, _ := procGetDpiForSystem.Call()
	if int32(dpi) <= 0 {
		return 96
	}
	return int(dpi)
}

// workArea 返回鼠标所在那块显示器的**工作区**(已经排除任务栏)。
//
// 用鼠标所在的那块而不是主屏:人一般看着有鼠标的那块屏。
func workArea() (rect, error) {
	var pt point
	if ok, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); ok != 0 {
		// MonitorFromPoint 的 POINT 是**按值传递的结构体**,x86-64 下整个
		// 8 字节打包进一个寄存器:低 32 位是 x,高 32 位是 y。
		// 当成两个独立参数传是错的 —— 那样 x 会占满整个寄存器。
		packed := uintptr(uint32(pt.X)) | uintptr(uint32(pt.Y))<<32
		mon, _, _ := procMonitorFromPoint.Call(packed, monitorDefaultToNearest)
		if mon != 0 {
			mi := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
			if ok, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); ok != 0 {
				return mi.RcWork, nil
			}
		}
	}

	// 兜底:主屏工作区。
	var r rect
	if ok, _, _ := procSystemParametersInfoW.Call(
		spiGetWorkArea, 0, uintptr(unsafe.Pointer(&r)), 0); ok != 0 {
		return r, nil
	}
	return rect{}, errors.New("拿不到屏幕工作区")
}
