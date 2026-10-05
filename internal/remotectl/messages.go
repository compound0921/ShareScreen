package remotectl

import (
	"log"

	"sharescreen/internal/clipboard"
	"sharescreen/internal/input"
)

// dispatch 把一条解析好的消息送到对应的分支。
func (s *Server) dispatch(sess *session, m clientMsg) {
	switch m.Type {
	case msgCtl:
		s.handleCtl(sess, m)
	case msgIn:
		s.handleInput(sess, m)
	case msgClip:
		s.handleClipboard(sess, m)
	default:
		// 不认识的消息直接忽略。观看页缓存在浏览器里,观众那侧的版本
		// 可能比主机新 —— 发来几个不认识的消息是正常的,不该报错。
	}
}

// handleCtl 处理控制权协商。
func (s *Server) handleCtl(sess *session, m clientMsg) {
	switch m.Act {
	case actRequest:
		state := s.control.request(sess.id, sess.label)
		sess.write(serverMsg{Type: msgCtl, State: state, Reason: stateReason(state)})
		if state == statePending {
			log.Printf("远程控制:%s 申请控制权,等待主机批准", sess.label)
		}
		s.notify()

	case actRelease:
		if s.control.release(sess.id) {
			sess.write(serverMsg{Type: msgCtl, State: stateIdle, Reason: "已归还控制权"})
			sess.drain()
			s.notify()
		}
	}
}

func stateReason(state string) string {
	if state == stateBusy {
		return "已经有人在控制这台电脑了"
	}
	return ""
}

// handleInput 处理一条输入事件。
//
// 不是控制者的事件在这里就被丢掉。必须拦在这一层:放到注入那边再拦的话,
// 队列里会混进别人的操作,而"谁按的"在那个位置已经分不出来了。
func (s *Server) handleInput(sess *session, m clientMsg) {
	if s.control.holder() != sess.id {
		return
	}

	switch m.Kind {
	case kindMove:
		s.pushEvent(sess, injEvent{kind: injMove, x: m.X, y: m.Y})

	case kindButton:
		if m.B == nil || m.Down == nil {
			return
		}
		b, ok := buttonOf(*m.B)
		if !ok {
			return
		}
		s.pushEvent(sess, injEvent{kind: injButton, button: b, down: *m.Down})

	case kindWheel:
		s.pushEvent(sess, injEvent{kind: injWheel, dx: m.DX, dy: m.DY})

	case kindKey:
		if m.Down == nil {
			return
		}
		vk, ext, ok := input.CodeToVK(m.Code)
		if !ok {
			// 认不出来的键就忽略,绝不猜一个 —— 猜错会在主机上打出
			// 用户根本没按过的字符。
			return
		}
		s.pushEvent(sess, injEvent{kind: injKey, vk: vk, extended: ext, down: *m.Down})

	case kindText:
		if m.S == "" || len(m.S) > maxTextBytes {
			return
		}
		s.pushEvent(sess, injEvent{kind: injText, text: m.S})
	}
}

// handleClipboard 把控制者送来的文本写进主机剪贴板。
//
// 两个门槛:必须是控制者(纯观看的人不该能改主机的剪贴板),而且用户
// 得开过剪贴板同步。后一条是单独的开关 —— 剪贴板会把手边的密码、
// 验证码之类的东西送出去,这个决定该由用户单独做一次。
func (s *Server) handleClipboard(sess *session, m clientMsg) {
	clip := s.clipSync()
	if clip == nil {
		return
	}
	if s.control.holder() != sess.id {
		return
	}
	if m.S == "" || len(m.S) > clipboard.MaxText {
		return
	}
	// 写失败大多是因为别的进程正占着剪贴板。不报错给观众 ——
	// 那是常态,而且过一会儿他自己再复制一次就好了。
	_ = clip.Write(m.S)
}

// pushEvent 排入一个事件;队列满了就断开这个会话。
//
// 断开必须发生在锁**外面**(session.push 返回之后)—— sess.close 自己
// 要拿同一把锁,在 push 里面调用它就是自锁。
func (s *Server) pushEvent(sess *session, ev injEvent) {
	if !sess.push(ev) {
		log.Printf("远程控制:%s 的输入积压过多,断开", sess.label)
		s.dropSlow(sess)
	}
}

// buttonOf 把浏览器报的按键编号映射成鼠标按键。
//
// 编号沿用 MouseEvent.button:0 左、1 中、2 右,3/4 是两个侧键。
func buttonOf(n int) (input.Button, bool) {
	switch n {
	case 0:
		return input.BtnLeft, true
	case 1:
		return input.BtnMiddle, true
	case 2:
		return input.BtnRight, true
	case 3:
		return input.BtnX1, true
	case 4:
		return input.BtnX2, true
	}
	return 0, false
}
