package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"sharescreen/internal/audio"
	"sharescreen/internal/notify"
)

// 音频采集设备的提示。
//
// 回环采集抓的是"某台渲染设备自己的混音",不是"系统在响什么"。笔记本接了
// HDMI 之后系统常把默认播放设备切到显示器那一路(那里没有声音),真正的
// 输出还在扬声器上 —— 两者一错开,观众那边就是有画面、一点声音没有,而
// 程序对此完全沉默。这里补上那一句:采的那台一直没声音、而别处正在放,
// 就在右下角问一句要不要换过去。
//
// 判定用两路信号,都是现成的:
//
//   - "我们采的这台没声音" —— 回环数据包里的静音标志(见 audio.audibility);
//   - "别处在放" —— 每台端点的峰值电平(见 audio.Levels)。
//
// 阈值全部按"宁可少报"挑:弹错了就是在用户正打字的时候糊一个置顶窗口
// 在他脸上。

const (
	audioPollInterval = 2 * time.Second

	// 采集设备要连续静音这么久才算"不对劲"。要盖过自然的空档 ——
	// 一段没声音的镜头、两首歌之间。
	audioSilenceFor = 10 * time.Second

	// 别处的峰值超过它才算"在放"。1e-3 比"暂停的流留下的残留"(约 1e-9)
	// 高好几个数量级,又低于正常内容的电平。
	audioPeakEps = 1e-3

	// 连着几次采样都要活跃 —— 一声系统提示音不该把弹窗勾出来。
	audioActiveStreak = 2

	// 弹窗关掉之后的冷却。切换过去之后如果那台也没声音,不该马上又弹一次。
	audioCooldown = 30 * time.Second

	// 这条提示的卡片要比默认(136)高不少:正文要说清两台设备的名字,
	// 光那两个名字就要占掉四行左右。正文区的高度是从按钮行反推的,
	// 具体能放几行见 notify 的 create。
	audioHintHeight = 240

	// 设备名截到多少个字符。这个数字和上面的高度是一对 —— 放不下就会
	// 被顶出卡片,而卡片放不下时不会报错,只会看起来是坏的。
	// 「扬声器 (Realtek(R) Audio)」是 24 个字符,所以留到 28。
	audioNameMax = 28
)

// 按钮下标,对应 Prompt 里 Primary/Secondary 的顺序。
const (
	audioBtnSwitch = 0 // 主色按钮「切换」
	audioBtnIgnore = 1 // 灰底按钮「忽略」—— Esc 也落在它上面
)

// audioHint 是这台提示的判定状态机。
//
// 依赖全部可注入,好让判定逻辑脱离 Win32、COM 和真实采集来测 ——
// 和 popup.go 里那个远控批准弹窗同一个形状,也是同一个理由。
type audioHint struct {
	capture   func() (deviceID, name string, silentFor time.Duration, ok bool)
	levels    func() ([]audio.Level, error)
	running   func() bool
	remoteOut func() bool // 有远控申请在等着批准
	prompt    func(context.Context, notify.Prompt) int
	switchTo  func(string) error
	now       func() time.Time

	mu sync.Mutex

	// streak 是"别处"连续活跃了几次采样。
	streak int

	// offered 表示用户对这次共享做过决定了(忽略或切换),不再问。
	// 推流停下来就复位 —— 见 tick 开头。
	offered bool

	// until 之前不弹。弹窗一关就冷却一段时间。
	until time.Time

	// showing 表示弹窗正开着;cancel 用来在条件自己消失时把它关掉。
	showing bool
	cancel  context.CancelFunc
}

// startAudioHint 起一根轮询协程,跟着 ctx 走 —— 和 srv.startAddressPoller
// 同一个生命周期模型:控制页关掉、程序退出时一起停。
//
// 不先跑一遍再进 ticker(地址轮询那边会先跑一遍):第一次判定本来就要
// 等满 10 秒静音,提前跑一次没有任何意义。
func startAudioHint(ctx context.Context, h *audioHint) {
	if h.capture == nil || h.prompt == nil {
		return
	}
	go func() {
		t := time.NewTicker(audioPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				h.stopShowing()
				return
			case <-t.C:
				h.tick()
			}
		}
	}()
}

// tick 看一次现场,必要时弹窗。
func (h *audioHint) tick() {
	now := h.now()

	// 没在共享:整台复位。下一次开始共享可以重新问一次 —— 这是用户选的
	// 语义("忽略"只管这一次推流)。
	if !h.running() {
		h.stopShowing()
		h.mu.Lock()
		h.offered, h.streak, h.until = false, 0, time.Time{}
		h.mu.Unlock()
		return
	}

	deviceID, name, silentFor, ok := h.capture()
	// ok 为假是"这次共享压根没有音频"(没开、没起来、起流失败),
	// 不是"音频静音了"。那种情况下什么都不该说。
	if !ok || silentFor < audioSilenceFor {
		h.stopShowing()
		h.mu.Lock()
		h.streak = 0
		h.mu.Unlock()
		return
	}

	levels, err := h.levels()
	if err != nil {
		// 探不到就别猜。不动 streak —— 下一轮大概率就好了。
		h.stopShowing()
		return
	}
	other, active := audio.PickActive(deviceID, levels, audioPeakEps)

	h.mu.Lock()
	if !active {
		h.streak = 0
		h.mu.Unlock()
		h.stopShowing()
		return
	}
	h.streak++
	skip := h.offered ||
		now.Before(h.until) ||
		h.streak < audioActiveStreak ||
		h.showing
	h.mu.Unlock()

	if skip {
		return
	}

	// 远控批准更要紧:两个窗口都算在右下角同一组坐标上,会完全重叠。
	// 等它处理完再说 —— 条件还在的话下一轮照样能弹。
	if h.remoteOut() {
		return
	}

	h.show(name, other)
}

// show 起一个 goroutine 去弹窗。
//
// 必须异步:prompt 阻塞到用户做决定,而 tick 跑在轮询协程上,后面还有
// 下一轮要看。这也是"一次推流只弹一次"能成立的地方 —— 见 offered。
func (h *audioHint) show(capturedName string, other audio.Level) {
	h.mu.Lock()
	if h.showing { // 已经开着一个了,别叠第二个
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.showing, h.cancel = true, cancel
	h.mu.Unlock()

	go func() {
		idx := h.prompt(ctx, audioHintPrompt(capturedName, other.Name))
		cancel() // 窗口已经关了,把 ctx 的资源放掉

		h.mu.Lock()
		h.showing, h.cancel = false, nil
		h.until = h.now().Add(audioCooldown)
		if idx >= 0 {
			// 用户真的按了一个按钮。本次推流不再问 —— 这条是必须的:
			// 故意把某个程序分到另一台设备上的人会被每十秒问一次。
			h.offered = true
		}
		h.mu.Unlock()

		// 条件自己消失时窗口被我们取消,那是 −1,和"窗口建不起来"
		// 走同一条路:不当作决定,只冷却一下,下次条件成立还能再问。
		if idx == audioBtnSwitch {
			if err := h.switchTo(other.ID); err != nil {
				log.Printf("音频设备提示:切换失败: %v", err)
			}
		}
	}()
}

// stopShowing 关掉正在显示的弹窗(如果开着)。
//
// 条件自己消失时用它 —— 采集设备又出声了、别处也安静了、或者推流停了。
// 那**不算**用户做过决定:他可能压根没看见,下次条件成立时还应该再问。
func (h *audioHint) stopShowing() {
	h.mu.Lock()
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// audioHintPrompt 组装那条提示。
//
// 纯函数,好测措辞 —— 和 notify.Describe 同一个理由:窗口那一半在真机上
// 很难断言,而措辞是用户唯一读到的东西。
func audioHintPrompt(capturedName, otherName string) notify.Prompt {
	return notify.Prompt{
		Title: "检测到其他活跃音频设备",
		Body: fmt.Sprintf(
			"当前采集的「%s」没有声音,而「%s」正在播放。\n换过去会重启采集,画面黑几秒。",
			shortName(capturedName), shortName(otherName)),

		// 主色按钮是「切换」:弹出这条就是为了修好它,「忽略」是"别烦我"。
		// Esc 落在「忽略」上(见 notify.keyChoice),方向是安全的。
		Primary:   "切换",
		Secondary: "忽略",

		Height: audioHintHeight,

		// 抢焦点(用户选的):这条提示不动就没法处理,站在最前面才不会被
		// 忽略过去。代价和相应的防护见 notify 包注释里 Enter 那一段 ——
		// 抢焦点的那一刻用户正在别处敲键盘,所以回车被硬性吃掉。
		//
		// 抢焦点也顺带让 Esc 能用:不抢的话键盘根本到不了这个窗口,
		// 「忽略」就只能鼠标点。
		StealFocus: true,

		// 没有倒计时:它背后是一个持续存在的状态,不会自己结束。窗口
		// 一直挂着,直到用户点它,或者那个状态消失。
	}
}

// shortName 把设备名截短到能放进卡片。
//
// 设备名可以很长(「扬声器 (Realtek(R) Audio)」「NVIDIA High Definition
// Audio」),而卡片高度是个固定常量 —— 不截的话名字能把正文顶出卡片,
// 看起来就是坏了。
//
// 从**中间**截:设备名往往前缀一样、后缀才区分得开
// (「扬声器 (Realtek(R) Audio)」和「扬声器 (USB Audio)」)。
// 只影响显示,切换时用的仍然是完整的设备 ID。
func shortName(name string) string {
	if name == "" {
		return "未知设备"
	}
	r := []rune(name)
	if len(r) <= audioNameMax {
		return name
	}
	// 头部多留一点(名字的主干在前面),尾部留 7 个字符够放 "Audio)" 这种
	// 区分度最高的后缀。
	const tail = 7
	head := audioNameMax - tail - 1 // −1 是省略号自己
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}
