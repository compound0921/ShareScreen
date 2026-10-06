package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"sharescreen/internal/notify"
	"sharescreen/internal/remotectl"
)

// ── 测试替身 ──

// askCall 是一次弹窗调用。测试直接抓住它,才能控制"用户什么时候点"。
type askCall struct {
	req   notify.Request
	ctx   context.Context
	reply chan notify.Choice
}

type fakePopup struct {
	mu       sync.Mutex
	calls    []*askCall
	approved []string
	denied   []string

	// status 是 status() 返回的东西 —— 权威状态的替身。
	status remotectl.Status
}

func newFakePopup() *fakePopup { return &fakePopup{} }

func (f *fakePopup) ask(ctx context.Context, req notify.Request) notify.Choice {
	c := &askCall{req: req, ctx: ctx, reply: make(chan notify.Choice, 1)}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()

	select {
	case choice := <-c.reply:
		return choice
	case <-ctx.Done():
		return notify.ChoiceNone
	}
}

func (f *fakePopup) approve(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved = append(f.approved, id)
	return true
}

func (f *fakePopup) deny(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied = append(f.denied, id)
	return true
}

func (f *fakePopup) controller() *popupController {
	return &popupController{
		ask:     f.ask,
		approve: f.approve,
		deny:    f.deny,
		status:  func() remotectl.Status { f.mu.Lock(); defer f.mu.Unlock(); return f.status },
	}
}

// setPending 让"权威状态"变成有/没有一份申请。
func (f *fakePopup) setPending(id, ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "" {
		f.status.Pending = nil
		return
	}
	f.status.Pending = &remotectl.Pending{
		ID:       id,
		RemoteIP: ip,
		At:       time.Now().Format(time.RFC3339),
	}
}

func (f *fakePopup) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePopup) call(i int) *askCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

func (f *fakePopup) counts() (approved, denied int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.approved), len(f.denied)
}

// waitFor 轮询等一个条件。弹窗是异步起 goroutine 的,断言前必须等它到位 ——
// 直接断言会变成"有时候过有时候不过"。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等不到:%s", what)
}

// statusOf 把 fakePopup 的权威状态包成 remotectl.Status 交给 onChange。
func statusOf(f *fakePopup) remotectl.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// ── 测试 ──

// 有申请就弹一次,内容带上对方地址。
func TestPopupOpensOnPending(t *testing.T) {
	f := newFakePopup()
	p := f.controller()
	f.setPending("s1", "192.168.3.163")

	p.onChange(statusOf(f))

	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })
	if got := f.call(0).req.IP; got != "192.168.3.163" {
		t.Errorf("弹窗里的地址 = %q,想要 192.168.3.163", got)
	}
	if got := f.call(0).req.Timeout; got <= 0 || got > remotectl.PendingTimeout {
		t.Errorf("剩余时间 = %v,应当落在 (0, %v] 之间", got, remotectl.PendingTimeout)
	}
}

// 同一份申请重复回调**只弹一次**。
//
// 这条是整套"盯跃迁、不是盯回调"设计存在的全部理由:OnChange 在每一次
// 状态变化时都触发,批准之后还会再来一串。照着回调弹窗的话,主人会被
// 同一个申请反复打扰。
func TestPopupDoesNotReopenForSamePending(t *testing.T) {
	f := newFakePopup()
	p := f.controller()
	f.setPending("s1", "192.168.3.163")

	for i := 0; i < 5; i++ {
		p.onChange(statusOf(f))
	}

	waitFor(t, "弹窗被调用", func() bool { return f.callCount() >= 1 })
	// 再等一会儿,确认没有第二、第三次
	time.Sleep(50 * time.Millisecond)
	if n := f.callCount(); n != 1 {
		t.Errorf("同一份申请弹了 %d 次,应当只有 1 次", n)
	}
}

// 申请没了(批准/拒绝/过期/对方断开),弹窗要被取消。
func TestPopupClosesWhenPendingCleared(t *testing.T) {
	f := newFakePopup()
	p := f.controller()
	f.setPending("s1", "192.168.3.163")
	p.onChange(statusOf(f))
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })

	f.setPending("", "")
	p.onChange(statusOf(f))

	waitFor(t, "弹窗的 ctx 被取消", func() bool {
		return f.call(0).ctx.Err() != nil
	})
}

// 点"允许"就调 Approve,而且不能顺手也调 Deny。
func TestPopupAllowCallsApprove(t *testing.T) {
	f := newFakePopup()
	p := f.controller()
	f.setPending("s1", "192.168.3.163")
	p.onChange(statusOf(f))
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })

	f.call(0).reply <- notify.ChoiceAllow

	waitFor(t, "Approve 被调用", func() bool {
		a, _ := f.counts()
		return a == 1
	})
	if approved, denied := f.counts(); approved != 1 || denied != 0 {
		t.Errorf("approve=%d deny=%d,想要 1 和 0", approved, denied)
	}
	if got := f.approved[0]; got != "s1" {
		t.Errorf("批准的是 %q,想要 s1", got)
	}
}

// 点"拒绝"就调 Deny。
func TestPopupDenyCallsDeny(t *testing.T) {
	f := newFakePopup()
	p := f.controller()
	f.setPending("s1", "192.168.3.163")
	p.onChange(statusOf(f))
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })

	f.call(0).reply <- notify.ChoiceDeny

	waitFor(t, "Deny 被调用", func() bool {
		_, d := f.counts()
		return d == 1
	})
	if approved, denied := f.counts(); approved != 0 || denied != 1 {
		t.Errorf("approve=%d deny=%d,想要 0 和 1", approved, denied)
	}
}

// 超时(ChoiceNone)**不发任何决定**。
//
// 这是用户明确选的行为:窗口自己消失,申请交给 remotectl 的过期循环作废。
// 回归成"没点也算拒绝",观众那边收到的原因就变了("主机拒绝了"而不是
// "主机没有应答"),而那是两件不同的事。
func TestPopupTimeoutSendsNoDecision(t *testing.T) {
	f := newFakePopup()
	p := f.controller()
	f.setPending("s1", "192.168.3.163")
	p.onChange(statusOf(f))
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })

	f.call(0).reply <- notify.ChoiceNone

	// run 是异步的,没有可等的信号 —— 给足时间,再断言两边都没被调用。
	time.Sleep(100 * time.Millisecond)
	if approved, denied := f.counts(); approved != 0 || denied != 0 {
		t.Errorf("没有决定却发了决定:approve=%d deny=%d", approved, denied)
	}
}

// 陈旧的"没有申请了"不能关掉刚弹出来的窗。
//
// OnChange 没有全局串行化:两条 readPump 协程加一个过期定时器都可能调它,
// 各自先取一份 Status 再进来。一条在那份申请出现**之前**取到的、内容是
// "没有申请"的状态,如果晚到一步,就会把窗关掉 —— 而此后不会再有任何
// 回调来提醒主人。
func TestPopupIgnoresStaleStatus(t *testing.T) {
	f := newFakePopup()
	p := f.controller()

	// 权威状态说:有一份申请。
	f.setPending("s1", "192.168.3.163")
	// 但回调带来的是一份"没有申请"的旧快照。
	p.onChange(remotectl.Status{})

	time.Sleep(50 * time.Millisecond)
	if n := f.callCount(); n != 0 {
		t.Errorf("过期的快照不该弹窗,却弹了 %d 次", n)
	}

	// 反过来:窗已经开着,一条过期的"没有申请了"不能把它关掉
	p.onChange(statusOf(f))
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })
	p.onChange(remotectl.Status{})

	time.Sleep(50 * time.Millisecond)
	if err := f.call(0).ctx.Err(); err != nil {
		t.Errorf("过期的快照把正在显示的弹窗关掉了:%v", err)
	}
}

// 前一份申请还在等,又来了新的一份:关掉旧的,弹新的。
func TestPopupReplacesOnNewPending(t *testing.T) {
	f := newFakePopup()
	p := f.controller()
	f.setPending("s1", "192.168.3.163")
	p.onChange(statusOf(f))
	waitFor(t, "弹窗被调用", func() bool { return f.callCount() == 1 })

	f.setPending("s2", "192.168.3.9")
	p.onChange(statusOf(f))

	waitFor(t, "第二个弹窗", func() bool { return f.callCount() == 2 })
	if got := f.call(1).req.IP; got != "192.168.3.9" {
		t.Errorf("第二个弹窗的地址 = %q,想要 192.168.3.9", got)
	}
	waitFor(t, "第一个弹窗被取消", func() bool {
		return f.call(0).ctx.Err() != nil
	})
}

// remaining 直接反映 PendingTimeout,不能是另抄的一个数字。
func TestRemaining(t *testing.T) {
	// 刚发生的申请:剩余时间应当接近整个超时窗口
	at := time.Now().Format(time.RFC3339)
	if got := remaining(at); got <= 0 || got > remotectl.PendingTimeout {
		t.Errorf("刚申请时 remaining = %v,应当在 (0, %v]", got, remotectl.PendingTimeout)
	}

	// 已经超过超时窗口:夹到 0,不能是负数(界面上会显示成负秒)
	old := time.Now().Add(-remotectl.PendingTimeout - time.Minute).Format(time.RFC3339)
	if got := remaining(old); got != 0 {
		t.Errorf("早就过期的申请 remaining = %v,想要 0", got)
	}

	// 解析不了就不显示倒计时
	if got := remaining("不是时间"); got != 0 {
		t.Errorf("时间解析失败时 remaining = %v,想要 0", got)
	}
}
