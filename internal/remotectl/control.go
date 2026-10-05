package remotectl

import (
	"sync"
	"time"
)

// pendingTimeout 是一份控制申请在主机点头之前能挂多久。
//
// 超时自动作废,而不是一直悬着:主机不在电脑前时,申请挂在那里既占着
// 名额(别人也申请不了),又让观众一直等一个不会来的批准。到点告诉
// 观众"没人应答",他还可以再试一次。
const pendingTimeout = 60 * time.Second

// waiter 是一个会话在控制权上的状态。
type waiter struct {
	id    string
	label string // 给主机看的标识,通常是对方的 IP
	at    time.Time
}

func describe(w *waiter) *Pending {
	if w == nil {
		return nil
	}
	return &Pending{ID: w.id, RemoteIP: w.label, At: w.at.Format(time.RFC3339)}
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

// request 记录一次控制申请,返回应当回给这个会话的状态。
func (c *control) request(id, label string) string {
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

	c.pending = &waiter{id: id, label: label, at: time.Now()}
	return statePending
}

// approve 把控制权交给正在等批准的那个会话。
func (c *control) approve(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil || c.pending.id != id {
		return false
	}
	c.owner = c.pending
	c.pending = nil
	return true
}

// deny 拒绝一份申请。主机点"拒绝"和申请超时都走这里。
func (c *control) deny(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil || c.pending.id != id {
		return false
	}
	c.pending = nil
	return true
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

// release 是观众主动归还控制权。
func (c *control) release(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.owner == nil || c.owner.id != id {
		return false
	}
	c.owner = nil
	return true
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

// expire 清掉已经等了太久的申请,返回它的会话 ID。
func (c *control) expire(now time.Time) (id string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil || now.Sub(c.pending.at) < pendingTimeout {
		return "", false
	}
	id = c.pending.id
	c.pending = nil
	return id, true
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
