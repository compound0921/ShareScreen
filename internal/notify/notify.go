// Package notify 在主机屏幕右下角弹一个带两个按钮的通知,并等用户做决定。
//
// 只服务于远程控制的批准:观众申请控制之后,主机未必在浏览器控制页前面,
// 光靠页面上的提示会漏掉整个申请。弹窗是"人在电脑前但不看那个页面"时
// 唯一能及时通知到他的方式。
//
// # 为什么是自己建窗口
//
// 三条现成的路都走不通:
//
//   - github.com/energye/systray 没有任何通知/气泡 API(整个模块里
//     NIF_INFO 从未被设置过);
//   - Windows 的气泡通知本身不支持按钮,而且 Win10 以后被系统转成 toast,
//     点击回调还不一定触发;
//   - WinRT toast 要 AUMID + 开始菜单快捷方式 + 注册 COM 激活器,
//     为一个批准框不值当。
//
// 所以自己 RegisterClassExW + 消息循环,和本程序其他地方处理 Win32 的
// 方式一致(不引 CGO、不引 GUI 工具包)。
//
// # 它是"抢焦点"的,但 Enter 是拒绝
//
// 弹窗会主动把前台抢过来(用户的选择)。这带来一个必须处理的风险:
// 抢焦点的那一刻,用户正在别处敲键盘 —— 如果 Enter 是"允许",一个本来
// 打给别的窗口的回车就把机器交了出去。所以窗口过程里**硬性截获 VK_RETURN
// 并映射成拒绝**,不依赖默认按钮那套(那套只在焦点不在别的按钮上时才生效)。
// 允许必须鼠标点,或者 Alt+Y。
//
// 同一个理由见 dialog_windows.go 里 ask() 的 MB_DEFBUTTON2。
package notify

import (
	"fmt"
	"time"
)

// Choice 是用户对弹窗的回应。
type Choice int

const (
	// ChoiceNone 表示**没有做决定**:ctx 结束、窗口没建起来、或者用户
	// 直接关掉了窗口。
	//
	// 调用方遇到它什么都不要做 —— 申请会自己过期。这一条是刻意的:
	// 过期逻辑只在 remotectl 一处,弹窗不替它做决定。
	ChoiceNone Choice = iota
	ChoiceAllow
	ChoiceDeny
)

// Request 是弹窗要显示的内容。
type Request struct {
	// IP 是申请方的地址,用于显示。remotectl 那边已经把端口去掉了。
	IP string

	// Timeout 是这份申请还剩多久作废,用来生成提示语。
	// 0 表示不显示剩余时间。
	Timeout time.Duration
}

// Ask 由平台文件实现:
//
//   - notify_windows.go —— 真正的右下角弹窗
//   - notify_other.go   —— 存根,直接返回 ChoiceNone
//
// 签名(两边必须一致):Ask(ctx context.Context, req Request) Choice。
// 它阻塞到用户选择或 ctx 结束;返回 ChoiceNone 表示没有决定,调用方
// 什么都不用做 —— 超时只让窗口消失,申请由 remotectl 的过期循环作废。
// 调用方必须是自己的 goroutine,它会阻塞几十秒。

// Describe 把请求翻译成弹窗上的标题和**不随时间变化**的那部分正文。
//
// 单独拆成纯函数是为了能测:窗口那一半在真实机器上很难断言,而措辞是
// 用户唯一读到的东西。
//
// 倒计时不在这里 —— 它每秒都要变,必须由窗口按剩余时间现算(见
// CountdownText)。烤进正文的话,显示的就是弹窗弹出那一刻的秒数,
// 之后一直不动。
func Describe(req Request) (title, body string) {
	title = "有人申请控制这台电脑"

	if req.IP == "" {
		body = "对方没有留下地址。"
	} else {
		body = fmt.Sprintf("来自 %s。", req.IP)
	}
	return title, body
}

// CountdownText 返回倒计时那一行。
//
// 向上取整:剩 59.4 秒时说"60 秒"才是对的,截断的话弹窗一出来就显示
// 59,看着像已经过了一秒。
//
// 说的是"自动拒绝"而不是"作废":超时之后申请在控制权状态机里确实是
// denied,观众那边收到的也是拒绝 —— 只是原因写的是"主机没有应答"而不是
// "主机拒绝了这次申请",两件事在协议上是分得开的。
//
// 注意这只是**文案**。弹窗自己在超时时仍然不发送任何决定,作废是由
// remotectl 的过期循环做的(见 notify.go 的包注释),那条不变。
func CountdownText(remaining time.Duration) string {
	if remaining <= 0 {
		// 申请的作废由 remotectl 的过期循环做,它有 5 秒的粒度,所以
		// 倒计时归零之后弹窗还会挂一小会儿。这里说"即将",而不是继续
		// 显示"0 秒后自动拒绝"。
		return "即将自动拒绝。"
	}
	secs := int((remaining + time.Second - 1) / time.Second) // 向上取整
	return fmt.Sprintf("%d 秒后自动拒绝。", secs)
}
