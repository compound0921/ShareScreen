package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"sharescreen/internal/audio"
	"sharescreen/internal/notify"
)

// ── 测试替身 ──

// hintCall 是一次弹窗调用。测试直接抓住它,才能控制"用户什么时候点"。
type hintCall struct {
	p     notify.Prompt
	ctx   context.Context
	reply chan int
}

type fakeHint struct {
	mu sync.Mutex

	// 采集设备这一侧
	devID, devName string
	silent         time.Duration
	haveCapture    bool

	// 别处那一侧
	levels    []audio.Level
	levelsErr error

	running       bool
	remotePending bool

	// clock 是假时钟。只有冷却窗口用到它 —— 静音时长是 capture 直接
	// 报出来的,不需要真等。
	clock time.Time

	calls     []*hintCall
	switched  []string
	switchErr error
}

func newFakeHint() *fakeHint {
	return &fakeHint{clock: time.Now()}
}

func (f *fakeHint) hint() *audioHint {
	return &audioHint{
		capture: func() (string, string, time.Duration, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.devID, f.devName, f.silent, f.haveCapture
		},
		levels: func() ([]audio.Level, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.levels, f.levelsErr
		},
		running: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.running
		},
		remoteOut: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.remotePending
		},
		prompt: func(ctx context.Context, p notify.Prompt) int {
			c := &hintCall{p: p, ctx: ctx, reply: make(chan int, 1)}
			f.mu.Lock()
			f.calls = append(f.calls, c)
			f.mu.Unlock()

			select {
			case idx := <-c.reply:
				return idx
			case <-ctx.Done():
				return -1
			}
		},
		switchTo: func(id string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.switched = append(f.switched, id)
			return f.switchErr
		},
		now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.clock
		},
	}
}

// quietHint 把场景摆成"采的是 A、A 一直没声音、B 在放"。
func (f *fakeHint) quietHint() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
	f.haveCapture = true
	f.devID, f.devName = "A", "扬声器 (Realtek(R) Audio)"
	f.silent = audioSilenceFor + time.Second
	f.levels = []audio.Level{
		{ID: "A", Name: "扬声器 (Realtek(R) Audio)", Peak: 0},
		{ID: "B", Name: "NVIDIA High Definition Audio", Peak: 0.5},
	}
}

func (f *fakeHint) set(apply func(*fakeHint)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apply(f)
}

// ticks 走 n 个轮询周期。每次把假时钟往前推一格 —— 冷却窗口靠它过期。
func (f *fakeHint) ticks(h *audioHint, n int) {
	for i := 0; i < n; i++ {
		f.set(func(f *fakeHint) { f.clock = f.clock.Add(audioPollInterval) })
		h.tick()
	}
}

func (f *fakeHint) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeHint) call(i int) *hintCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

func (f *fakeHint) switchedTo() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.switched...)
}

// waitClosed 等弹窗那根 goroutine 收尾。
func waitClosed(t *testing.T, h *audioHint) {
	t.Helper()
	waitFor(t, "弹窗关闭", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return !h.showing
	})
}

// noPopup 断言弹窗**一次都没弹**(或者一共弹了 want 次之后停住)。
//
// 只看 f.callCount() 是不够的,而且会时过时不过:弹窗是 tick 里起一根
// goroutine 去调的,登记那次调用是那根 goroutine 的第一步 —— 直接断言
// 会读到"还没来得及登记"的中间态,于是真正的 bug 也能蒙混过去。
//
// showing 是同步置位的(在 tick → show 里,起 goroutine 之前),所以它
// 是"这一刻到底有没有弹"的确定答案。
func noPopup(t *testing.T, f *fakeHint, h *audioHint, want int) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := f.callCount(); n != want {
			t.Fatalf("弹窗被调用了 %d 次,想要 %d 次", n, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.mu.Lock()
	showing := h.showing
	h.mu.Unlock()
	if showing {
		t.Fatalf("弹窗开着 —— 一共弹了 %d 次,想要 %d 次", f.callCount(), want)
	}
}

// ── 测试 ──

// 采的那台静音够久、别处持续在放 → 弹一次。
func TestHintOpensWhenCapturedIsSilentAndOtherPlays(t *testing.T) {
	f := newFakeHint()
	f.quietHint()
	h := f.hint()

	f.ticks(h, 1)
	if n := f.callCount(); n != 0 {
		t.Fatalf("第一次采样就弹了 %d 次 —— 一次活跃还不够", n)
	}

	f.ticks(h, 1)
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })

	body := f.call(0).p.Body
	for _, want := range []string{"Realtek", "NVIDIA"} {
		if !strings.Contains(body, want) {
			t.Errorf("正文里没有提到 %q:%q", want, body)
		}
	}
}

// 别处只响了一次(一声系统提示音),不该弹。
//
// 这条是"持续性"那个条件的全部意义:只看某一刻的峰值,Windows 随便一个
// 通知音就能把置顶窗口糊到用户脸上。
func TestHintNeedsSustainedOtherActivity(t *testing.T) {
	f := newFakeHint()
	f.quietHint()
	h := f.hint()

	// 第一轮活跃,第二轮就安静了
	f.ticks(h, 1)
	f.set(func(f *fakeHint) { f.levels[1].Peak = 0 })
	f.ticks(h, 5)

	noPopup(t, f, h, 0)
}

// 静音时长不够,再多次采样也不弹。
func TestHintNeedsSustainedSilence(t *testing.T) {
	f := newFakeHint()
	f.quietHint()
	h := f.hint()

	f.set(func(f *fakeHint) { f.silent = 9 * time.Second })
	f.ticks(h, 10)

	noPopup(t, f, h, 0)
}

// 没有音频(没开、没起来)什么都不说 —— 那不是"静音",是"没有音频"。
func TestHintSilentForStreamWithoutAudio(t *testing.T) {
	f := newFakeHint()
	f.quietHint()
	f.set(func(f *fakeHint) { f.haveCapture = false })
	h := f.hint()

	f.ticks(h, 10)

	noPopup(t, f, h, 0)
}

// 点「忽略」→ 这次推流剩下的时间不再问;停止共享再开始,可以重新问。
func TestHintIgnoreSuppressesUntilStreamRestarts(t *testing.T) {
	f := newFakeHint()
	f.quietHint()
	h := f.hint()

	f.ticks(h, 2)
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })

	f.call(0).reply <- audioBtnIgnore
	waitClosed(t, h)

	f.ticks(h, 20)
	noPopup(t, f, h, 1)
	if got := f.switchedTo(); len(got) != 0 {
		t.Errorf("忽略不该换设备,却换了 %v", got)
	}

	// 停止共享 → 复位 → 再开始,应当能重新问
	f.set(func(f *fakeHint) { f.running = false })
	f.ticks(h, 1)
	f.set(func(f *fakeHint) { f.running = true })

	f.ticks(h, 5)
	waitFor(t, "重新开始共享后能再问", func() bool { return f.callCount() == 2 })
}

// 点「切换」→ 换到那台在放的设备上,并且不再问。
func TestHintSwitchCallsSwitchTo(t *testing.T) {
	f := newFakeHint()
	f.quietHint()
	h := f.hint()

	f.ticks(h, 2)
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })

	f.call(0).reply <- audioBtnSwitch
	waitFor(t, "切换被调用", func() bool { return len(f.switchedTo()) == 1 })

	if got := f.switchedTo()[0]; got != "B" {
		t.Errorf("换到的是 %q,想要 B", got)
	}
	if got := f.call(0).p.Primary; got != "切换" {
		t.Errorf("主色按钮上的字是 %q,想要「切换」", got)
	}
	if got := f.call(0).p.Secondary; got != "忽略" {
		t.Errorf("第二个按钮上的字是 %q,想要「忽略」", got)
	}
	// 用户的明确选择:抢焦点。它同时决定了键盘能不能到这个窗口 ——
	// 不抢的话 Esc 根本到不了,「忽略」就只能鼠标点。
	if !f.call(0).p.StealFocus {
		t.Error("这条提示应当把前台抢过来")
	}
	if f.call(0).p.Timeout != 0 || f.call(0).p.Countdown != nil {
		t.Error("这条提示不该有倒计时 —— 它背后是一个持续存在的状态")
	}
}

// 条件自己消失(采集设备又出声了)→ 窗口被关掉,而且**不算用户做过决定**:
// 条件再次成立时还应该再问一次。
func TestHintCancelsWhenConditionClearsWithoutDeciding(t *testing.T) {
	f := newFakeHint()
	f.quietHint()
	h := f.hint()

	f.ticks(h, 2)
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })

	// 采到声音了
	f.set(func(f *fakeHint) { f.silent = 0 })
	f.ticks(h, 1)
	waitFor(t, "弹窗被取消", func() bool { return f.call(0).ctx.Err() != nil })
	waitClosed(t, h)

	if got := f.switchedTo(); len(got) != 0 {
		t.Errorf("被取消不该换设备,却换了 %v", got)
	}

	// 又安静下来 → 应当能再问一次(冷却过后)
	f.set(func(f *fakeHint) { f.silent = audioSilenceFor + time.Second })
	f.set(func(f *fakeHint) { f.clock = f.clock.Add(audioCooldown + time.Second) })
	f.ticks(h, 5)

	waitFor(t, "条件再次成立时能再问", func() bool { return f.callCount() == 2 })
}

// 有远控申请在等批准时不弹:两个窗口算在右下角同一组坐标上,会完全重叠。
func TestHintWaitsForRemoteApprovalPopup(t *testing.T) {
	f := newFakeHint()
	f.quietHint()
	f.set(func(f *fakeHint) { f.remotePending = true })
	h := f.hint()

	f.ticks(h, 10)
	noPopup(t, f, h, 0)

	// 批准处理完 → 下一轮就该弹了
	f.set(func(f *fakeHint) { f.remotePending = false })
	f.ticks(h, 5)
	waitFor(t, "远控弹窗消失后能弹", func() bool { return f.callCount() == 1 })
}

// 正在采的那台即使报着高电平也不选它 —— 那是我们自己。
func TestHintNeverSwitchesToItself(t *testing.T) {
	f := newFakeHint()
	f.quietHint()
	f.set(func(f *fakeHint) { f.levels[0].Peak = 0.9 }) // A 就是采的那台
	h := f.hint()

	f.ticks(h, 10)

	// B 还在放(0.5),所以仍然该弹 —— 但绝不会切到 A 上
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })
	f.call(0).reply <- audioBtnSwitch
	waitFor(t, "切换被调用", func() bool { return len(f.switchedTo()) == 1 })

	if got := f.switchedTo()[0]; got == "A" {
		t.Errorf("换到了正在采的那台 %q", got)
	}
}

// 探不到设备别弹 —— 那是"不知道",不是"别处在放"。
func TestHintDoesNothingWhenProbingFails(t *testing.T) {
	f := newFakeHint()
	f.quietHint()
	f.set(func(f *fakeHint) { f.levelsErr = context.DeadlineExceeded })
	h := f.hint()

	f.ticks(h, 10)
	noPopup(t, f, h, 0)
}

// 措辞:两台设备的名字都要出现,而且不能有"秒" —— 这条提示没有倒计时。
func TestAudioHintPromptWording(t *testing.T) {
	p := audioHintPrompt("扬声器 (Realtek(R) Audio)", "NVIDIA High Definition Audio")

	if p.Title == "" {
		t.Error("标题不能为空")
	}
	for _, want := range []string{"Realtek", "NVIDIA"} {
		if !strings.Contains(p.Body, want) {
			t.Errorf("正文里没有提到 %q:%q", want, p.Body)
		}
	}
	// 正文里可以出现"黑几秒"这种时长描述,但不能出现"N 秒后…"那种
	// 会一秒一秒变的倒计时 —— 那是由 Prompt.Countdown 现算的,
	// 烤进正文就会永远停在弹出那一刻。
	if strings.Contains(p.Body, "秒后") {
		t.Errorf("正文里烤进了倒计时:%q", p.Body)
	}
	if !strings.Contains(p.Body, "黑") {
		t.Errorf("正文该说明切换要重启采集、画面会黑几秒:%q", p.Body)
	}
	if p.Height <= 0 {
		t.Error("这条提示的正文比默认长,必须指定一个更高的卡片")
	}
}

// 设备名要截短:卡片高度是固定的,超长名字会把正文顶出去。
func TestShortName(t *testing.T) {
	if got := shortName("扬声器"); got != "扬声器" {
		t.Errorf("短名字应当原样保留,却变成 %q", got)
	}
	if got := shortName(""); got == "" {
		t.Error("名字取不到时也要给个说法,不能是空串")
	}

	long := "扬声器 (Realtek(R) Audio) 很长很长的尾巴"
	got := shortName(long)

	if n := len([]rune(got)); n > audioNameMax {
		t.Errorf("截完还有 %d 个字符,卡片放不下:%q", n, got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("超长的名字应当带省略号:%q", got)
	}
	// 从中间截:两端都要留着 —— 设备名往往前缀一样,靠后缀区分。
	if !strings.HasPrefix(got, "扬声器 (") {
		t.Errorf("前缀被截掉了:%q", got)
	}
	if !strings.HasSuffix(got, "尾巴") {
		t.Errorf("后缀被截掉了 —— 那才是有区分度的一半:%q", got)
	}

	// 真实设备名(24 个字符)必须原样留下,不能被截 —— 截了用户就认不出
	// 是哪台设备,而这条提示的全部价值就是让他知道要换到哪台去。
	real := "扬声器 (Realtek(R) Audio)"
	if got := shortName(real); got != real {
		t.Errorf("常见的真实设备名被截了:%q,想要 %q", got, real)
	}
}
