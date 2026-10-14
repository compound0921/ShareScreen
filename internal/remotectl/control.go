package remotectl

import (
	"sync"
	"time"
)

// PendingTimeout 是一份控制申请在主机点头之前能挂多久。
//
// 超时自动作废,而不是一直悬着:主机不在电脑前时,申请挂在那里既占着
// 名额(别人也申请不了),又让观众一直等一个不会来的批准。到点告诉
// 观众"没人应答",他还可以再试一次。
//
// 导出是因为主机端的批准弹窗要拿它算"还剩多少秒",而那段提示必须和
// 真正的过期时刻一致 —— 另抄一个 60 秒过去,两边迟早会走散。
const PendingTimeout = 60 * time.Second

// waiter 是一个会话在控制权上的状态。
type waiter struct {
	id    string
	label string // 给主机看的标识,通常是对方的 IP
	at    time.Time
	kind  string // pendingControl 或 pendingOpen,见 server.go
}

func describe(w *waiter) *Pending {
	if w == nil {
		return nil
	}
	return &Pending{
		ID:       w.id,
		Kind:     w.kind,
		RemoteIP: w.label,
		At:       w.at.Format(time.RFC3339),
	}
}

// control 是控制权仲裁。
//
// 规则只有一条:同一时刻最多一个会话能操作主机。其余的人照常看画面,
// 但他们的事件在进入注入队列之前就会被丢掉 —— 拦早一点是必要的,
// 否则主机的鼠标会在几个人的操作之间来回跳。
//
// 这里的取舍是"宁可拒绝,不要排队":已经有申请在等的时候,后来者直接
// 收到"忙"。排队看着更公平,但主机批准时批的是队列里的某一个人,观众
// 会看到自己"申请成功"却还是动不了,反而更难解释。
type control struct {
	mu sync.Mutex

	owner   *waiter // 非 nil 表示有人在控制
	pending *waiter // 非 nil 表示有申请在等批准
}

// request 记录一次申请(要控制权,或者要开启),返回应当回给这个会话的状态。
//
// kind 只影响"主机点了允许之后该做什么",不影响排队规则:
//
//   - 两个 kind 共享同一个 owner 和同一个 pending 名额。有人正在申请控制时,
//     另一个人的"请求开启"同样得到 busy。理由和下面那段一样 —— 排队会出现
//     "申请成功却还是动不了"的错觉;而且两种申请挤在一个名额里,主机那一侧
//     无论如何也只会看到一份待批,不用去解释"为什么有两个弹窗"。
//   - 唯一按 kind 区分的是重复申请:同一个会话重复发同一种申请不该重置计时。
//     跨 kind 的重复(先申请控制、又请求开启)走的是普通的新申请,如果它
//     自己已经占着那个名额,也应当原样返回 pending —— 这里用 id 比较就够了,
//     因为名额只有一个,而占着它的正是他自己。
func (c *control) request(id, label, kind string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.owner != nil {
		if c.owner.id == id {
			return stateGranted // 重复申请,本来就是他在控制
		}
		return stateBusy
	}
	if c.pending != nil {
		if c.pending.id == id {
			return statePending // 已经申请过了,别重置计时
		}
		return stateBusy
	}

	c.pending = &waiter{id: id, label: label, at: time.Now(), kind: kind}
	return statePending
}

// grantIfFree 在不和任何人冲突的前提下直接把控制权交给 id。
//
// 这是"控制密码"那条捷径:填对密码的观众不必等主机点批准。冲突判定和
// request 完全一致 —— 有人正在控制、或者别人已经在排队等批准,一律回 busy:
//
//   - **不能夺权。** 密码决定"我要不要麻烦主机点一下",不决定"我能不能把
//     别人正在用的控制权抢过来"。那件事只有主机的"收回控制权"能做。
//   - **不能插队。** 别人已经在等主机批准了,这时候放一个知道密码的人先进去,
//     那个排队的人会看到自己的申请凭空作废,而主机刚准备点"允许"。
//
// 唯独一种情况特殊:排队的就是自己。观众先不知道密码、点了申请,又想起来
// 密码补填一次 —— 那就把他从队列里升格,而不是回一句 busy 让他重新排队。
func (c *control) grantIfFree(id, label string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.owner != nil {
		if c.owner.id == id {
			return stateGranted // 本来就是他在控制
		}
		return stateBusy
	}
	if c.pending != nil {
		if c.pending.id != id {
			return stateBusy
		}
		// 自己在排队 —— 升格为控制者,等价于 approve 走了一遍。
		c.owner = c.pending
		c.pending = nil
		return stateGranted
	}

	c.owner = &waiter{id: id, label: label, at: time.Now(), kind: pendingControl}
	return stateGranted
}

// approve 处理一次"主机点了允许":把待批名额交出去,并告诉调用方等的是
// 哪一种申请(主机点完允许之后要做什么,完全取决于它)。
//
// **两种申请在这里的处理不一样,这个差异是整个功能的安全要点:**
//
//   - 控制申请:名额直接变成控制权。
//   - 开启请求:名额只是被收走 —— pending 清空,于是主机的弹窗关掉、别人
//     也能接着排队;但控制权**先不给**。开启请求接下来还要去把共享和远控
//     真正打开,而那一步会失败。失败时不能留下一个"界面说你正在控制、
//     实际按什么都没反应"的状态,更不能先把控制权交出去再谈开启 ——
//     输入那条路上只看"你是不是控制者"(见 handleInput)。
//     控制权由调用方在开启**成功之后**用 grantIfFree 补上。
func (c *control) approve(id string) (kind string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil || c.pending.id != id {
		return "", false
	}
	kind = c.pending.kind
	if kind == pendingControl {
		c.owner = c.pending
	}
	c.pending = nil
	return kind, true
}

// deny 拒绝一份申请。主机点"拒绝"和申请超时都走这里。
//
// 同样要带出 kind —— 回给观众的措辞按申请种类分。
func (c *control) deny(id string) (kind string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil || c.pending.id != id {
		return "", false
	}
	kind = c.pending.kind
	c.pending = nil
	return kind, true
}

// revoke 把控制权收回来,不管现在是谁在控制。
//
// 这是主机那边的"刹车",必须永远可用 —— 所以它不返回失败:没人在控制
// 时它就是个空操作。
func (c *control) revoke() (id string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.owner == nil {
		return "", false
	}
	id = c.owner.id
	c.owner = nil
	return id, true
}

// release 是观众主动放弃:正在控制就交回控制权,还在排队就撤掉申请。
//
// 两件事合在一个方法里,因为观众那一侧只有**一个**按钮 —— 它必须永远可用、
// 点了永远有回应。分成两个动作的话,"取消申请"那条路会收到一句"你不是控制者"
// 之类的拒绝,而拒绝里没有任何他能做的事:他就是要撤掉那份申请。
//
// 返回值:wasOwner 说明撤掉的是控制权还是排队(回给观众的措辞不一样),
// ok 为假表示这个会话两样都不占 —— 那才是真的无事可做。
func (c *control) release(id string) (wasOwner, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.owner != nil && c.owner.id == id {
		c.owner = nil
		return true, true
	}
	if c.pending != nil && c.pending.id == id {
		c.pending = nil
		return false, true
	}
	return false, false
}

// drop 在连接断开时清理这个会话占的位置。
//
// 返回它原本是不是控制者 —— 是的话调用方不用再通知谁了(人已经走了),
// 但要更新界面状态。
func (c *control) drop(id string) (wasOwner bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.owner != nil && c.owner.id == id {
		c.owner = nil
		wasOwner = true
	}
	if c.pending != nil && c.pending.id == id {
		c.pending = nil
	}
	return wasOwner
}

// expire 清掉已经等了太久的申请,返回它的会话 ID 和种类。
func (c *control) expire(now time.Time) (id, kind string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil || now.Sub(c.pending.at) < PendingTimeout {
		return "", "", false
	}
	id, kind = c.pending.id, c.pending.kind
	c.pending = nil
	return id, kind, true
}

// holder 返回当前控制者的会话 ID,没有则返回空串。
func (c *control) holder() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.owner == nil {
		return ""
	}
	return c.owner.id
}

// snapshot 返回给控制页看的控制权状态。
func (c *control) snapshot() (owner string, pending *Pending) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.owner != nil {
		owner = c.owner.label
	}
	return owner, describe(c.pending)
}

// clear 清空全部控制权状态。关闭远控时用。
func (c *control) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.owner = nil
	c.pending = nil
}
