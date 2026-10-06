package main

import (
	"context"
	"sync"
	"time"

	"sharescreen/internal/notify"
	"sharescreen/internal/remotectl"
)

// popupController 维护主机端的"有人申请控制"弹窗。
//
// # 为什么需要它
//
// remotectl 的 OnChange 在**每一次**主机可见的状态变化时都会触发:有人连上、
// 申请、批准、拒绝、收回控制权、断开、开关远控、申请过期…… 不是只在"申请
// 出现"时触发。照着回调就弹窗的话,同一份申请会被重复弹好几次。
//
// 所以这里只盯 Pending 的**跃迁**:同一个 ID 不重开,ID 消失就关掉。
type popupController struct {
	// 四个依赖都可以替换,好让状态跃迁逻辑脱离 Win32 和真实会话来测。
	// 生产值在 main.go 里装配。
	ask     func(context.Context, notify.Request) notify.Choice
	approve func(string) bool
	deny    func(string) bool
	status  func() remotectl.Status

	mu     sync.Mutex
	curID  string             // 正在弹窗的申请 ID;"" 表示没有弹窗
	cancel context.CancelFunc // 关掉当前弹窗
}

// onChange 是唯一入口,由 remotectl.Options.OnChange 驱动。
//
// **必须是非阻塞的。** 它跑在观众的 WebSocket 读协程上(messages.go:34)
// 也跑在申请过期的定时器协程上(server.go:511),在这里等用户点按钮
// 会把那条连接和过期检查一起卡住。所以真正的等待在 run() 里那个
// 独立的 goroutine 上。
func (p *popupController) onChange(st remotectl.Status) {
	id, ip := "", ""
	var timeout time.Duration
	if st.Pending != nil {
		id, ip = st.Pending.ID, st.Pending.RemoteIP
		timeout = remaining(st.Pending.At)
	}
	p.sync(id, ip, timeout)
}

// remaining 算这份申请还剩多久作废。
//
// 用的是 remotectl.PendingTimeout 而不是另抄一个 60 秒:弹窗上写着"还剩
// 多少秒",那个数字必须和真正的作废时刻一致,两处各记一份早晚会走散。
// 解析失败或已经超时就返回 0,调用方不显示倒计时(而不是显示一个负数)。
func remaining(at string) time.Duration {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return 0
	}
	left := remotectl.PendingTimeout - time.Since(t)
	if left < 0 {
		return 0
	}
	return left
}

func (p *popupController) sync(id, ip string, timeout time.Duration) {
	// 陈旧回调防护。OnChange 没有全局串行化 —— 两条 readPump 协程加一个
	// 过期定时器都可能调它,而且都是各自先取一份 Status 再进来。所以手里
	// 这个 st 未必是最新的,动手之前先问一次权威状态。
	//
	// 不挡的话有两种坏结果:已经处理掉的申请又被弹一次窗;以及刚弹出来的
	// 窗被一条过期的"没有申请了"关掉 —— 后者更糟,用户会整个错过这次申请,
	// 而且此后不会再有任何回调来提醒他。
	//
	// ⚠️ 这里依赖 remotectl 的一个不变量:**notify() 从不在持有 s.mu 时被调用**
	// (Approve/Deny/removeSession/handleWS 都是先解锁再 notify)。也就是说
	// Status() 可以从 OnChange 里安全调用。哪天有人在 s.mu 里调 notify,
	// 这里会立刻自死锁(Go 的 mutex 不可重入)—— 改那边的时候记得看这里。
	if p.status != nil {
		if cur := p.status().Pending; pendingID(cur) != id {
			return
		}
	}

	p.mu.Lock()

	if id == p.curID {
		// 同一份申请。绝大多数回调都是这种(批准之后还会有一串)。
		p.mu.Unlock()
		return
	}

	prev := p.cancel
	p.cancel, p.curID = nil, id

	if id == "" || p.ask == nil {
		p.mu.Unlock()
		if prev != nil {
			prev() // 关旧窗。不在锁内调,省得和窗口那边的锁纠缠
		}
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.mu.Unlock()

	if prev != nil {
		prev()
	}
	go p.run(ctx, id, ip, timeout)
}

// run 等用户做决定,然后把它交回去。
func (p *popupController) run(ctx context.Context, id, ip string, timeout time.Duration) {
	choice := p.ask(ctx, notify.Request{IP: ip, Timeout: timeout})

	switch choice {
	case notify.ChoiceAllow:
		p.approve(id)
	case notify.ChoiceDeny:
		p.deny(id)
	case notify.ChoiceNone:
		// 超时、被取消、或窗口没建起来 —— 什么都不发。
		// 申请会自己过期,过期逻辑只在 remotectl 一处。
	}

	// 清掉自己那条记录,否则同一个 ID 再也不会弹窗。
	//
	// 要比一下:如果等待期间这份申请已经作废、又来了新的一份,curID 早就
	// 换成新的了,这里不能把后来者的记录抹掉。
	p.mu.Lock()
	if p.curID == id {
		p.curID = ""
	}
	p.mu.Unlock()
}

func pendingID(p *remotectl.Pending) string {
	if p == nil {
		return ""
	}
	return p.ID
}
