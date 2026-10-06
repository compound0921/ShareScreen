// Package notify 在主机屏幕右下角弹一个带两个按钮的通知,并等用户做决定。
//
// 两个消费者共用一个窗口:远程控制的批准(观众申请之后,主机未必在浏览器
// 控制页前面,光靠页面上的提示会漏掉整个申请),以及音频采集设备的提示
// (采的那台一直没声音、而别处正在放)。两者都是"人在电脑前但不看那个
// 页面"时唯一能及时通知到他的方式。
//
// 窗口机制是同一套,内容由 Prompt 决定 —— 见 AskButtons。批准那条是
// Ask,语义(允许/拒绝)固定在 Choice 上。
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
// # 它是"抢焦点"的,但 Enter 什么也不做
//
// 弹窗会主动把前台抢过来(用户的选择)。这带来一个必须处理的风险:
// 抢焦点的那一刻,用户正在别处敲键盘 —— 一个本来打给别的窗口的回车
// 绝不能在这里产生效果。所以窗口过程里**硬性截获 VK_RETURN 并把它吃掉**,
// 不依赖默认按钮那套(那套只在焦点不在别的按钮上时才生效)。
//
// "吃掉"和"不管"是两回事:不管的话回车会落到对话框管理器手里,照样能
// 激活默认按钮。两道都要成立,少一道回车就又能用了。
//
// 键盘因此只能表达一件事:Esc 落在**左边那个按钮**上 —— 批准那边是拒绝,
// 音频提示那边是忽略,两个方向都不动任何东西。主色按钮只能鼠标点,
// keyAction 里根本没有指向它的取值。
//
// 同一个理由见 dialog_windows.go 里 ask() 的 MB_DEFBUTTON2。
package notify

import (
	"context"
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

// Prompt 是一次弹窗要显示的全部内容。
//
// 按钮固定两个:两个消费者(批准、音频提示)都只需要两个,而自绘窗口的
// 布局是按两个按钮写死的。为用不上的槽位做成按钮数组,等于把风险加到
// 最难测的那一半代码上(paint / 命中测试 / 布局),换不来任何东西。
type Prompt struct {
	Title string
	Body  string

	// Primary 是右下角那个主色按钮上的字,Secondary 是它左边那个灰底的。
	// 顺序对应 AskButtons 的返回值:0 = Primary,1 = Secondary。
	Primary   string
	Secondary string

	// Timeout > 0 时显示倒计时,文案由 Countdown 现算 —— 和 Request.Timeout
	// 一样,只是这里把文案也交出去,因为两边说的是不同的事
	//("自动拒绝" / "自动忽略")。
	Timeout   time.Duration
	Countdown func(time.Duration) string

	// StealFocus 决定要不要把前台抢过来。
	//
	// 抢焦点是有代价的 —— 那一刻用户正在别处敲键盘,所以 Enter 被硬性
	// 吃掉(见包注释)。当前两条提示都要抢:批准是一条"有人在等着"的
	// 请求,音频提示是一条不动它就没法处理的状态,两者被忽略过去的代价
	// 都比打断一次打字大。
	//
	// 它还有一个不那么明显的后果:**不抢的话键盘到不了这个窗口**,
	// Esc 就按不动了,两个按钮只能用鼠标点。窗口本来就是置顶的,
	// 显示和点击都不受影响,受影响的只有键盘。
	StealFocus bool

	// Height 是卡片高度(逻辑像素),0 表示用默认值。
	//
	// 正文长短差别很大:批准那边一两行就够,音频提示要说清两台设备的
	// 名字。正文区的高度是从按钮行反推的(见 create),所以高度一改,
	// 它自己就跟着变。
	Height int
}

// AskButtons 由平台文件实现:
//
//   - notify_windows.go —— 真正的右下角弹窗
//   - notify_other.go   —— 存根,直接返回 −1
//
// 签名(两边必须一致):AskButtons(ctx context.Context, p Prompt) int。
// 返回按钮下标(0 = Prompt.Primary,1 = Prompt.Secondary),**−1 表示没有
// 做决定**:ctx 结束、窗口没建起来、或者用户直接关掉了窗口(Alt+F4)。
//
// 它阻塞到用户选择或 ctx 结束,调用方必须是自己的 goroutine —— 它会
// 阻塞几十秒。

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

// Ask 弹右下角通知,请用户批准或拒绝一次远程控制申请。
//
// 返回 ChoiceNone 表示**没有做决定**:ctx 结束、窗口没建起来、或者用户
// 直接关掉了窗口。调用方遇到它什么都不要做 —— 申请会自己过期,那套逻辑
// 只在 remotectl 一处。
func Ask(ctx context.Context, req Request) Choice {
	title, body := Describe(req)
	return choiceForIndex(AskButtons(ctx, Prompt{
		Title:     title,
		Body:      body,
		Primary:   "允许",
		Secondary: "拒绝",
		Timeout:   req.Timeout,
		Countdown: CountdownText,
		// 批准是抢焦点的。代价和相应的防护见包注释里 Enter 那一段。
		StealFocus: true,
	}))
}

// choiceForIndex 把按钮下标翻译回批准语义。纯函数,便于测试。
//
// 下标和 Choice 的对应关系是这套泛化的接缝,单拎出来测:0 是右下角那个
// 主色按钮(允许),1 是它左边那个(拒绝),−1 是没做决定。
func choiceForIndex(i int) Choice {
	switch i {
	case 0:
		return ChoiceAllow
	case 1:
		return ChoiceDeny
	}
	return ChoiceNone
}
