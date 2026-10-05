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
	for {
		select {
		case <-s.done:
			return
		case <-s.notify:
		}

		for {
			ev, ok := s.pop()
			if !ok {
				break
			}
			// 取事件之前可能已经被收回控制权了,再确认一次 ——
			// 队列里可能还压着夺权之前塞进来的几个事件。
			if s.srv.control.holder() != s.id {
				continue
			}
			if err := s.apply(ev); err != nil {
				s.reportInjectError(err)
			}
		}
	}
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
func (s *session) write(m serverMsg) {
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
