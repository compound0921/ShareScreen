package remotectl

import (
	"log"

	"github.com/coder/websocket"

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

// maxPwFails 是单条连接上控制密码能连错几次。
//
// 令牌是 256 位随机值,猜不中,所以 /ws 上没有失败次数限流(见 auth.go)。
// 密码是人选的、可能很短,**重复猜是有意义的** —— 所以这条路上要有个闸。
//
// 8 次:手滑、大小写、记错一个字符,这么多次都足够纠正过来;再往后就是在
// 枚举了。断连而不是锁定:断掉的是一条连接,对方拿令牌重连即可重来,而
// 我们这边不维护任何跨连接的封禁状态 —— 那种状态既有内存增长问题,
// 又会在合法的那个同伴被误伤时很难自证。
//
// 明确一下这个计数**不**打算做到的事:它拦不住铁了心枚举的人(重连是免费的),
// 它挡的是"一个拿到了链接的人在密码框里乱试"。真正让人不敢硬猜的是另外两件:
// 第一层的令牌(没有它连不上来),以及每一次猜错都会在主机屏幕上敲一次门 ——
// 猜错走的是普通的"申请 → 待批准",主机看到那个弹窗就能拒绝、就能收回控制权。
const maxPwFails = 8

// handleCtl 处理控制权协商。
func (s *Server) handleCtl(sess *session, m clientMsg) {
	switch m.Act {
	case actRequest:
		pw := s.currentPassword()
		// 主机没设密码时 pw 是空串,passwordMatch 一律不匹配 ——
		// 于是这里自然退回"申请 → 主机批准"那条老路。
		authOK := pw != "" && passwordMatch(pw, m.Pw)

		var state string
		if authOK {
			state = s.control.grantIfFree(sess.id, sess.label)
		} else {
			state = s.control.request(sess.id, sess.label)
		}

		wrongPw := pw != "" && m.Pw != "" && !authOK

		if authOK {
			// 密码对了,失败计数就清零 —— 哪怕最后没轮到他(有人正在控制),
			// 那也是"密码没错",下一次不该接着上次数算。
			sess.pwFails = 0

			// 而且没和别人撞上 —— 直接就是控制者了,不经过批准。
			if state == stateGranted {
				s.grant(sess)
				log.Printf("远程控制:%s 控制密码通过,已自动批准", sess.label)
				s.notify()
				return
			}
		} else if wrongPw {
			// 只有"主机设了密码 + 对方确实填了"才算一次失败 ——
			// 完全没填是正常用法(等人批准),不该计入。
			sess.pwFails++
			if sess.pwFails >= maxPwFails {
				log.Printf("远程控制:%s 控制密码连续错误 %d 次,断开", sess.label, sess.pwFails)
				sess.write(serverMsg{Type: "err", Code: errAuth, Msg: "控制密码连续错误太多次"})
				sess.close()
				_ = sess.conn.Close(websocket.StatusPolicyViolation, "控制密码错误")
				return
			}
		}

		reason := stateReason(state)
		if state == statePending && wrongPw {
			// 这里是"排队等批准"之上的额外说明,不是拒绝 —— 让他知道
			// 密码填错了,可以改一个再试,也可以原地等主机点允许。
			reason = "控制密码不正确,可以改一个再试,或者等主机批准"
		}
		sess.write(serverMsg{Type: msgCtl, State: state, Reason: reason})
		if state == statePending {
			log.Printf("远程控制:%s 申请控制权,等待主机批准", sess.label)
		}
		s.notify()

	case actRelease:
		// 同一个动作,两种处境:在控制就是"归还",还在排队就是"取消申请"。
		// 观众那侧按钮上的字会跟着处境变,这里回的措辞也要跟着变 ——
		// 排队的人看到"已归还控制权"会以为自己刚才在控制。
		wasOwner, ok := s.control.release(sess.id)
		if !ok {
			return
		}
		reason := "已归还控制权"
		if !wasOwner {
			reason = "已取消申请"
			log.Printf("远程控制:%s 撤回了控制申请", sess.label)
		}
		sess.write(serverMsg{Type: msgCtl, State: stateIdle, Reason: reason})
		sess.drain()
		s.notify()
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
