package remotectl

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"sharescreen/internal/input"
	"sharescreen/internal/screen"
)

// Injector 是把输入真正打到系统上的那层。定成接口是为了让队列顺序、
// 控制权这些逻辑能脱离 Windows 来测 —— 真机测试里"点错位置"和"根本
// 没点到"完全分不清,而这正是单测要覆盖的部分。
type Injector interface {
	MoveNorm(area screen.Rect, nx, ny float64) error
	ButtonEvent(b input.Button, down bool) error
	Wheel(dx, dy int) error
	KeyEvent(vk uint16, extended, down bool) error
	TypeText(s string) error
	ElevatedForeground() bool

	// HostActive 报告本机用户此刻在不在操作这台机器(1 秒内有他自己的输入)。
	//
	// 本机用户优先:他在用的时候注入一律让路,免得两边抢同一个鼠标。
	// 实现见 internal/input 的 HostActive —— 那里要把"注入的输入"从
	// "用户自己的输入"里分出来,不能直接拿 GetLastInputInfo 当空闲判断。
	HostActive() bool
}

// osInjector 是 Injector 的真实实现,直接转发给 internal/input。
type osInjector struct{}

func (osInjector) MoveNorm(area screen.Rect, nx, ny float64) error {
	return input.MoveNorm(area, nx, ny)
}
func (osInjector) ButtonEvent(b input.Button, down bool) error {
	return input.ButtonEvent(b, down)
}
func (osInjector) Wheel(dx, dy int) error { return input.Wheel(dx, dy) }
func (osInjector) KeyEvent(vk uint16, extended, down bool) error {
	return input.KeyEvent(vk, extended, down)
}
func (osInjector) TypeText(s string) error  { return input.TypeText(s) }
func (osInjector) ElevatedForeground() bool { return input.ElevatedForeground() }
func (osInjector) HostActive() bool         { return input.HostActive() }

// 注入队列的容量和超时。
const (
	// maxQueue 是单个会话积压的事件数上限。超过说明主机注入跟不上,
	// 再堆下去只会让操作延迟越来越大,不如断开。
	maxQueue = 256

	// rateLimit 是单个会话每秒允许的输入事件数。
	//
	// 观众端已经把移动合并到每帧一次(约 60/秒),正常操作远低于这个值。
	// 撞上说明不是手在动 —— 更可能是页面里的循环出了 bug,或者是有人
	// 在拿它当压测工具。
	rateLimit = 500

	// rateWindow 是上面那个计数的窗口长度。
	rateWindow = time.Second

	// hostCheckInterval 是"主机在不在操作"的兜底检查间隔。
	//
	// 事件进来时那次检查只覆盖"观众正在动"的情况。观众按住不放(拖动、
	// 或者按着 Ctrl 等下一个键)的时候没有任何事件流进来,而主机一样需要
	// 拿回自己的鼠标 —— 那个按钮是已经按下去的,主机一动就变成拖拽。
	// 250ms 是"主机察觉不到"和"别白跑"之间的折中。
	hostCheckInterval = 250 * time.Millisecond

	// writeTimeout 是往观众端写一条消息的上限。
	writeTimeout = 5 * time.Second

	// injectErrInterval 是同一类注入错误最多多久报给观众一次。
	//
	// 必须节流:鼠标一移动就是几十条事件,而失败原因通常是同一个
	// (比如前台窗口提权),不节流的话观众端一秒能收到几百条一样的
	// 报错,把真正的信息淹掉。
	injectErrInterval = 3 * time.Second
)

// injKind 是注入队列里的事件类型。
type injKind int

const (
	injMove injKind = iota
	injButton
	injWheel
	injKey
	injText
)

// injEvent 是队列里的一个待注入事件。
//
// 用一个大结构而不是接口:事件就五种,字段加起来也不多,而接口会带来
// 每个事件一次堆分配 —— 鼠标移动每秒六十次,这个分配量没必要。
type injEvent struct {
	kind injKind

	x, y     float64
	button   input.Button
	down     bool
	dx, dy   int
	vk       uint16
	extended bool
	text     string
}

// session 是一个连上来的观众。
type session struct {
	id    string
	label string // 给主机看的标识,通常是对方 IP
	conn  *websocket.Conn

	srv *Server

	// inMu 保护 queue,并和 closed 一起决定还能不能收事件。
	inMu   sync.Mutex
	queue  []injEvent
	closed bool

	// notify 只是"队列里有东西了"的唤醒信号,容量 1 —— 它不承载事件,
	// 所以丢多少次都不影响正确性。
	notify chan struct{}
	done   chan struct{}

	// 下面这一组记的是"有哪些键鼠是我们按下去的、还没松开",以及"当前
	// 是不是在给主机让路"。
	//
	// 只在注入协程上读写(apply、noteHeld、releaseHeld、suspend/resume
	// 全在那一条上),所以不加锁 —— 和 pwFails 同一个路数。
	//
	// 为什么要记:观众按住左键拖拽的时候主机一动鼠标,注入停下,而那个
	// 左键是**我们已经按下去了**的。不替它补一个抬起,主机接下来自己点的
	// 每一下都会变成拖拽。
	heldButtons    uint32          // 按 input.Button 编号的位
	heldKeys       map[uint16]bool // vk → 是不是扩展键(抬起时要原样带回去)
	inputSuspended bool

	// pwFails 是这条连接上控制密码连错的次数,见 messages.go 的 maxPwFails。
	//
	// 不加锁:它只在 readPump 这一个 goroutine 上读写(dispatch 是同步调用,
	// 所有 clientMsg 都从那个循环里出来)。加了锁反而会让人以为还有别的
	// 写入方。
	pwFails int

	// writeMu 串行化对 conn 的写。WebSocket 允许一个读和一个写并发,
	// 但不允许两个写并发。
	writeMu sync.Mutex

	errMu     sync.Mutex
	lastErr   string
	lastErrAt time.Time
}

func newSession(id, label string, conn *websocket.Conn, srv *Server) *session {
	return &session{
		id:     id,
		label:  label,
		conn:   conn,
		srv:    srv,
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// ---------- 事件队列 ----------

// push 把一个事件排进注入队列,返回 false 表示队列已满、这个会话该断了。
//
// **顺序在这里是硬要求。** 移动和离散事件(按下/抬起、滚轮、按键)必须
// 走同一条队列,否则"先把鼠标移到按钮上、再点击"会变成"先点击、再移
// 过去",点中的是上一个位置的东西。
//
// 合并只发生在**队尾那个还没被取走的移动**上:观众的鼠标已经移到别处,
// 中间那些位置没有意义。被覆盖的是末尾元素,剩下事件的相对顺序不变,
// 所以"移动 → 点击"的先后仍然成立。
func (s *session) push(ev injEvent) bool {
	s.inMu.Lock()
	defer s.inMu.Unlock()

	if s.closed {
		return true // 已经关了,悄悄丢掉就行
	}

	if ev.kind == injMove {
		if n := len(s.queue); n > 0 && s.queue[n-1].kind == injMove {
			s.queue[n-1] = ev
			return true // 唤醒信号已经发出去了,不用再发
		}
	} else if len(s.queue) >= maxQueue {
		s.closed = true
		return false
	}

	s.queue = append(s.queue, ev)
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return true
}

func (s *session) pop() (injEvent, bool) {
	s.inMu.Lock()
	defer s.inMu.Unlock()

	if len(s.queue) == 0 {
		return injEvent{}, false
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	return ev, true
}

// close 停止收事件并唤醒注入协程,让它退出。
func (s *session) close() {
	s.inMu.Lock()
	already := s.closed
	s.closed = true
	s.inMu.Unlock()

	if !already {
		close(s.done)
	}
}

// ---------- 注入 ----------

// injectLoop 是这个会话的注入协程。
//
// 每个会话一个,但同一时刻只有一个会话能拿到控制权,所以实际上只有
// 控制者这条在干活 —— 别人的事件在 readPump 里就被丢掉了。
func (s *session) injectLoop() {
	// 兜底检查,见 hostCheckInterval 那段。
	hostTick := time.NewTicker(hostCheckInterval)
	defer hostTick.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-hostTick.C:
			// 只在控制权还在自己手上时才有意义。谁都轮不到的时候注入本来
			// 就不发,让路状态也就无从谈起。
			if s.srv.control.holder() == s.id {
				if s.srv.inject.HostActive() {
					s.suspendInput()
				} else {
					s.resumeInput()
				}
			}
		case <-s.notify:
		}

		for {
			ev, ok := s.pop()
			if !ok {
				break
			}
			// 取事件之前可能已经被收回控制权了,再确认一次 ——
			// 队列里可能还压着夺权之前塞进来的几个事件。
			//
			// 顺带把按着的东西放开:控制权没了,而观众是按着左键被夺权的
			// (拖拽中途被收回、连接断了),那个左键会一直按在主机那里 ——
			// 无论接下来是谁在控制,主机自己的鼠标都是坏的。
			if s.srv.control.holder() != s.id {
				s.releaseHeld()
				s.inputSuspended = false
				continue
			}
			// 本机用户优先:他在用这台机器的时候,注入一律让路。
			//
			// 丢弃而不是排队:攒着等主机停下来再一次性倒出去,那是在
			// 重放一段没人记得的操作(而且最长要等一秒),比直接丢掉糟
			// 得多。丢在这里也**不会**积压 —— 循环照样把队列掏空,
			// 只是不往系统里送。
			if s.srv.inject.HostActive() {
				s.suspendInput()
				continue
			}
			s.resumeInput()

			if err := s.apply(ev); err != nil {
				s.reportInjectError(err)
				continue
			}
			// 送出去了才记 —— 注入失败时系统那边并没有按下什么。
			s.noteHeld(ev)
		}
	}
}

// noteHeld 记下刚刚注入成功的按键状态。只在注入协程上调用。
func (s *session) noteHeld(ev injEvent) {
	switch ev.kind {
	case injButton:
		bit := uint32(1) << uint(ev.button)
		if ev.down {
			s.heldButtons |= bit
		} else {
			s.heldButtons &^= bit
		}
	case injKey:
		if !ev.down {
			delete(s.heldKeys, ev.vk)
			return
		}
		if s.heldKeys == nil {
			s.heldKeys = map[uint16]bool{}
		}
		s.heldKeys[ev.vk] = ev.extended
	}
}

// releaseHeld 把还按着的键鼠补一个抬起。
//
// 配合 suspendInput 用:让路之前必须先把手里按着的东西放下,否则那一下就
// 一直按在主机那儿 —— 主机自己点的每一下都会变成拖拽(或者是按住 Ctrl)。
func (s *session) releaseHeld() {
	// 失败不管:这里能失败的原因基本只有 UIPI(前台窗口是管理员),而那件
	// 事已经由 ElevatedForeground 在控制页上报着了。每个键报一次只会把
	// 真正的原因淹掉。
	for b := input.BtnLeft; b <= input.BtnX2; b++ {
		if s.heldButtons&(1<<uint(b)) == 0 {
			continue
		}
		_ = s.srv.inject.ButtonEvent(b, false)
	}
	s.heldButtons = 0

	for vk, ext := range s.heldKeys {
		_ = s.srv.inject.KeyEvent(vk, ext, false)
	}
	s.heldKeys = nil
}

// suspendInput 让路:松开按着的东西,并且只通知一次。
//
// 幂等 —— 注入协程是"丢掉一个事件就调一次",而松开和通知只该发生在进入
// 暂停的那一刻。主机一直在操作的时候,这条消息每次输入都要发的话,那比
// 不通知还烦人。
func (s *session) suspendInput() {
	if s.inputSuspended {
		return
	}
	s.inputSuspended = true
	s.releaseHeld()
	// 告诉观众为什么点了没反应。不说的话,这就是个彻头彻尾的谜。
	s.write(serverMsg{Type: msgHold, State: holdOn})
}

// resumeInput 主机停手了,恢复注入。同样是幂等的,只在恢复那一刻通知一次。
func (s *session) resumeInput() {
	if !s.inputSuspended {
		return
	}
	s.inputSuspended = false
	s.write(serverMsg{Type: msgHold, State: holdOff})
}

func (s *session) apply(ev injEvent) error {
	switch ev.kind {
	case injMove:
		area, ok := s.srv.captureRect()
		if !ok {
			return errors.New("当前采集源不支持远程控制")
		}
		return s.srv.inject.MoveNorm(area, ev.x, ev.y)
	case injButton:
		return s.srv.inject.ButtonEvent(ev.button, ev.down)
	case injWheel:
		return s.srv.inject.Wheel(ev.dx, ev.dy)
	case injKey:
		return s.srv.inject.KeyEvent(ev.vk, ev.extended, ev.down)
	case injText:
		return s.srv.inject.TypeText(ev.text)
	}
	return nil
}

// reportInjectError 把注入失败告诉观众,但同一类错误做节流。
func (s *session) reportInjectError(err error) {
	msg := err.Error()

	s.errMu.Lock()
	if msg == s.lastErr && time.Since(s.lastErrAt) < injectErrInterval {
		s.errMu.Unlock()
		return
	}
	s.lastErr = msg
	s.lastErrAt = time.Now()
	s.errMu.Unlock()

	s.write(serverMsg{Type: "err", Code: errInject, Msg: msg})
}

// ---------- 收发 ----------

// write 往观众端写一条消息。
//
// conn 为 nil 时什么都不做。nil 是测试里用的替身(见 newSession 的调用点),
// 不是运行时状态 —— 真实会话一定带着连接。有了这一句,那些只关心"状态机
// 怎么走"的测试不必为了凑一条 WebSocket 而把整条链路架起来。
func (s *session) write(m serverMsg) {
	if s.conn == nil {
		return
	}

	data, err := json.Marshal(m)
	if err != nil {
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	_ = s.conn.Write(ctx, websocket.MessageText, data)
}
