package input

import (
	"strconv"
	"strings"
)

// 这里自己定义虚拟键码,而不是用 golang.org/x/sys/windows 里那套 VK_*
// 常量,原因是 x/sys/windows 只能在 Windows 上编译,而这个映射表是纯
// 数据、应当能被任意平台上的测试完整覆盖。取值就是 Windows 虚拟键码表
// (docs.microsoft.com/windows/win32/inputdev/virtual-key-codes)。
const (
	vkBack           = 0x08
	vkTab            = 0x09
	vkClear          = 0x0C
	vkReturn         = 0x0D
	vkShift          = 0x10
	vkControl        = 0x11
	vkMenu           = 0x12
	vkPause          = 0x13
	vkCapital        = 0x14
	vkEscape         = 0x1B
	vkSpace          = 0x20
	vkPrior          = 0x21
	vkNext           = 0x22
	vkEnd            = 0x23
	vkHome           = 0x24
	vkLeft           = 0x25
	vkUp             = 0x26
	vkRight          = 0x27
	vkDown           = 0x28
	vkPrint          = 0x2A
	vkSnapshot       = 0x2C
	vkInsert         = 0x2D
	vkDelete         = 0x2E
	vkHelp           = 0x2F
	vkLWin           = 0x5B
	vkRWin           = 0x5C
	vkApps           = 0x5D
	vkNumPad0        = 0x60
	vkMultiply       = 0x6A
	vkAdd            = 0x6B
	vkSubtract       = 0x6D
	vkDecimal        = 0x6E
	vkDivide         = 0x6F
	vkF1             = 0x70
	vkNumLock        = 0x90
	vkScroll         = 0x91
	vkLShift         = 0xA0
	vkRShift         = 0xA1
	vkLControl       = 0xA2
	vkRControl       = 0xA3
	vkLMenu          = 0xA4
	vkRMenu          = 0xA5
	vkBrowserBack    = 0xA6
	vkBrowserForward = 0xA7
	vkBrowserRefresh = 0xA8
	vkVolumeMute     = 0xAD
	vkVolumeDown     = 0xAE
	vkVolumeUp       = 0xAF
	vkMediaNextTrack = 0xB0
	vkMediaPrevTrack = 0xB1
	vkMediaStop      = 0xB2
	vkMediaPlayPause = 0xB3
	vkOEM1           = 0xBA
	vkOEMPlus        = 0xBB
	vkOEMComma       = 0xBC
	vkOEMMinus       = 0xBD
	vkOEMPeriod      = 0xBE
	vkOEM2           = 0xBF
	vkOEM3           = 0xC0
	vkOEM4           = 0xDB
	vkOEM5           = 0xDC
	vkOEM6           = 0xDD
	vkOEM7           = 0xDE
	vkOEM102         = 0xE2
	vkABNTC1         = 0xC1 // 巴西 ABNT 键盘上多出来的那个键
)

// keyCode 是映射表的一项。extended 对应 Win32 的 KEYEVENTF_EXTENDEDKEY。
//
// 扩展标记区分的是那些扫描码与小键盘重合一组的键(方向键、Home/End、
// Insert/Delete、右 Ctrl、右 Alt、两个 Windows 键、小键盘回车和除号)。
// 用虚拟键码注入时,不设这个标记大多数程序也能正确响应,但少数按左右
// 区分修饰键的程序(游戏居多)会分不出来 —— 所以照实标上。
type keyCode struct {
	vk       uint16
	extended bool
}

// codeTable 把浏览器的 KeyboardEvent.code 映射成虚拟键码。
//
// 字母、数字、功能键、小键盘数字是连续区间,在 CodeToVK 里算,不列进
// 这张表 —— 26 + 10 + 24 + 10 行纯抄写的映射只会让表更难读。
var codeTable = map[string]keyCode{
	"Backspace":  {vkBack, false},
	"Tab":        {vkTab, false},
	"Enter":      {vkReturn, false},
	"Escape":     {vkEscape, false},
	"Space":      {vkSpace, false},
	"Clear":      {vkClear, false},
	"Help":       {vkHelp, false},
	"CapsLock":   {vkCapital, false},
	"ScrollLock": {vkScroll, false},
	"Pause":      {vkPause, false},

	// 导航键组:扫描码与小键盘共用,必须带扩展标记
	"PrintScreen": {vkSnapshot, true},
	"Insert":      {vkInsert, true},
	"Delete":      {vkDelete, true},
	"Home":        {vkHome, true},
	"End":         {vkEnd, true},
	"PageUp":      {vkPrior, true},
	"PageDown":    {vkNext, true},
	"ArrowUp":     {vkUp, true},
	"ArrowDown":   {vkDown, true},
	"ArrowLeft":   {vkLeft, true},
	"ArrowRight":  {vkRight, true},

	// 修饰键:左右分开,否则主机上按不出"右 Ctrl"
	"ShiftLeft":    {vkLShift, false},
	"ShiftRight":   {vkRShift, false},
	"ControlLeft":  {vkLControl, false},
	"ControlRight": {vkRControl, true},
	"AltLeft":      {vkLMenu, false},
	"AltRight":     {vkRMenu, true},
	"MetaLeft":     {vkLWin, true},
	"MetaRight":    {vkRWin, true},
	"ContextMenu":  {vkApps, true},

	// 标点。名字是 OEM 编号,不是字符 —— 同一个物理键在不同布局上
	// 打出不同字符,这正是我们要的:观众按的是位置,主机按自己的布局解释。
	"Backquote":     {vkOEM3, false},
	"Minus":         {vkOEMMinus, false},
	"Equal":         {vkOEMPlus, false},
	"BracketLeft":   {vkOEM4, false},
	"BracketRight":  {vkOEM6, false},
	"Backslash":     {vkOEM5, false},
	"Semicolon":     {vkOEM1, false},
	"Quote":         {vkOEM7, false},
	"Comma":         {vkOEMComma, false},
	"Period":        {vkOEMPeriod, false},
	"Slash":         {vkOEM2, false},
	"IntlBackslash": {vkOEM102, false},
	"IntlRo":        {vkABNTC1, false},
	"IntlYen":       {vkOEM5, false},

	// 小键盘
	"NumLock":        {vkNumLock, true},
	"NumpadEnter":    {vkReturn, true},
	"NumpadDivide":   {vkDivide, true},
	"NumpadMultiply": {vkMultiply, false},
	"NumpadSubtract": {vkSubtract, false},
	"NumpadAdd":      {vkAdd, false},
	"NumpadDecimal":  {vkDecimal, false},

	// 多媒体键:远程控制里"帮我按下静音"是很常见的诉求
	"AudioVolumeMute":    {vkVolumeMute, true},
	"AudioVolumeDown":    {vkVolumeDown, true},
	"AudioVolumeUp":      {vkVolumeUp, true},
	"MediaPlayPause":     {vkMediaPlayPause, true},
	"MediaTrackNext":     {vkMediaNextTrack, true},
	"MediaTrackPrevious": {vkMediaPrevTrack, true},
	"MediaStop":          {vkMediaStop, true},
	"BrowserBack":        {vkBrowserBack, true},
	"BrowserForward":     {vkBrowserForward, true},
	"BrowserRefresh":     {vkBrowserRefresh, true},
}

// CodeToVK 把浏览器的 KeyboardEvent.code 映射成 Windows 虚拟键码。
//
// 用 code 而不是 key:code 表示键盘上那个**物理位置**,与观众的键盘布局
// 无关。观众用德语键盘按下 Z 键的位置,主机收到的就是主机布局里同一个
// 位置对应的键。key 带着观众自己的布局信息,送到一台布局不同的机器上
// 会变成另一个键 —— 快捷键还好,输入文本就完全错位了。
//
// 返回 false 表示这个 code 不在表里。调用方应当忽略它,而不是猜一个键。
func CodeToVK(code string) (vk uint16, extended bool, ok bool) {
	switch {
	// KeyA..KeyZ → 虚拟键码恰好就是 ASCII 大写
	case len(code) == 4 && strings.HasPrefix(code, "Key"):
		if c := code[3]; c >= 'A' && c <= 'Z' {
			return uint16(c), false, true
		}

	// Digit0..Digit9 → 恰好就是 ASCII 数字
	case len(code) == 6 && strings.HasPrefix(code, "Digit"):
		if c := code[5]; c >= '0' && c <= '9' {
			return uint16(c), false, true
		}

	// F1..F24 → 0x70 起连续
	case len(code) >= 2 && len(code) <= 3 && code[0] == 'F':
		if n, err := strconv.Atoi(code[1:]); err == nil && n >= 1 && n <= 24 {
			return uint16(vkF1 + n - 1), false, true
		}

	// Numpad0..Numpad9 → 0x60 起连续
	case strings.HasPrefix(code, "Numpad") && len(code) == 7:
		if c := code[6]; c >= '0' && c <= '9' {
			return uint16(vkNumPad0 + int(c-'0')), false, true
		}
	}

	e, hit := codeTable[code]
	if !hit {
		return 0, false, false
	}
	return e.vk, e.extended, true
}
