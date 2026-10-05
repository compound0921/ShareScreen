// Package input 把观众端的鼠标和键盘事件注入到本机。
//
// 这是整个项目里唯一会让远端直接操作你这台机器的代码,所以几条硬约束
// 写在这里,实现时不要绕开:
//
//   - **注入失败必须报出来。** 但要注意,SendInput 的返回值和
//     GetLastError 都**不能**用来说明 UIPI 拦截:当前台窗口以管理员
//     身份运行时,注入会被系统悄悄丢弃,而 SendInput 照样返回成功。
//     唯一可靠的判断办法是事先看一眼前台窗口的完整性级别,见
//     ElevatedForeground。
//   - **安全桌面上的注入必然失效**:Ctrl+Alt+Del、UAC 同意框、锁屏。
//     这是 Windows 的设计,不是缺陷,也不该试图绕过。
//   - **不能有 cgo**(见 docs/架构设计.md §5.5),只能用
//     golang.org/x/sys/windows 的纯 Go 调用。
//
// # 坐标是怎么算的
//
// 观众看到的画面来自某个采集源,它对应屏幕上一块确定的矩形(见
// ffmpeg.CaptureRect)。归一化坐标先落回这块矩形得到屏幕像素,再折算成
// SendInput 的绝对坐标空间。
//
// 绝对坐标的解释有两种:默认按**主显示器**铺满 0~65535,带上
// VIRTUALDESK 时按**整个虚拟桌面**铺满。这里一律走 VIRTUALDESK,因为
// "先算像素、再按虚拟桌面折算"对两种采集范围都成立;而"直接按主显示器
// 折算"只在采集范围恰好等于主屏时才对,而且它会悄悄忽略多屏。
// 单显示器时两条路算出来的数是完全一样的,所以这么做没有额外代价。
package input

import (
	"errors"
	"math"
	"unicode/utf16"
)

// ErrUnsupported 表示当前平台没有输入注入能力。
var ErrUnsupported = errors.New("input: 仅支持 Windows")

// Button 是鼠标按键。
type Button uint32

const (
	BtnLeft Button = iota
	BtnMiddle
	BtnRight
	BtnX1
	BtnX2
)

// normPixel 把 [0,1] 的归一化坐标换算成一块矩形里的像素坐标。
//
// 分母用 size-1 而不是 size:归一化的 1.0 要落在**最后一个像素**上,
// 而不是这块区域右边缘之外一格。用 size 当分母会让画面最右一列和
// 最下一行永远点不到 —— 表现为鼠标贴着边就"过不去"。
func normPixel(n float64, origin, size int) int {
	if size <= 1 {
		return origin
	}
	if math.IsNaN(n) {
		n = 0
	}
	// 越界的值直接夹住,而不是拒绝 —— 观众那边的鼠标位置计算难免有
	// 一两个像素的误差,为此丢一个事件不值得。
	n = math.Min(math.Max(n, 0), 1)

	p := origin + int(math.Round(n*float64(size-1)))
	if p > origin+size-1 {
		p = origin + size - 1
	}
	return p
}

// unicodeUnits 把文本拆成 UTF-16 码元序列 —— 也就是 KEYEVENTF_UNICODE
// 要逐个送出的那些值。
//
// 单独拎出来是因为它有个容易忽略的点:非 BMP 字符(emoji、部分生僻字)
// 在 UTF-16 里是两个码元,必须按代理对送两条事件。按 rune 直接取低 16 位
// 的话,那些字符到了主机上会变成两个问号。
func unicodeUnits(s string) []uint16 {
	return utf16.Encode([]rune(s))
}

// absCoord 把屏幕像素坐标折算成 SendInput 绝对坐标空间的取值。
//
// origin/size 是**虚拟桌面**的原点和尺寸。折算出来的取值区间是闭区间
// [0,65535],所以分母同样是 size-1。
//
// 虚拟桌面只有一块显示器时,结果就是"按主显示器归一化"的那个值 ——
// 两者在数学上是同一个式子。
func absCoord(pixel, origin, size int) int32 {
	if size <= 1 {
		return 0
	}
	v := int64(pixel-origin) * 65535 / int64(size-1)
	if v < 0 {
		v = 0
	}
	if v > 65535 {
		v = 65535
	}
	return int32(v)
}
