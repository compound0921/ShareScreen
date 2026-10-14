package remotectl

import (
	"errors"
	"log"
)

// OpenResult 是一次"开启共享与远控"落地之后的结果。
//
// 两个布尔是分开的,因为它们是两件独立的事、也可能只成一半:远控没开
// 成(采集源不支持、端口被占)的时候共享照样能开始,对方能看画面、只是
// 不能操作。把这种情形压成一个 bool,就只能说"失败了",而那句话会让
// 人以为画面也看不成。
type OpenResult struct {
	// Sharing 表示共享已经开始(或者本来就在跑)。
	Sharing bool

	// Remote 表示远控已经打开。
	Remote bool

	// Err 非 nil 表示至少有一件事没做成。
	Err error
}

// handleOpen 处理"请求开启共享与远控"。
//
// 这是这个功能的主线:远控还没开的时候,对方能连上来就靠请求入口,而这里
// 是那个请求的入口。两条放行路径和 handleCtl 是同构的 —— 主机当场批准,
// 或者填对控制密码。同一个暗号、同一个失败计数器(passwordFail)。
func (s *Server) handleOpen(sess *session, m clientMsg) {
	switch m.Act {
	case actRequest:
		s.requestOpen(sess, m.Pw)

	case actRelease:
		// 撤回请求。和 handleCtl 的 release 一样,两件事合在一个动作里
		// 是因为观众那侧只有一个按钮。
		//
		// 这里不会撤回"控制权":能走到这个分支说明远控当时没开,那就不可能
		// 有人是控制者(见 control.release 的两种处境)。
		if _, ok := s.control.release(sess.id); !ok {
			return
		}
		sess.write(serverMsg{Type: msgOpen, State: stateIdle, Reason: "已取消请求"})
		s.notify()
	}
}

// requestOpen 受理一次开启请求。
func (s *Server) requestOpen(sess *session, pw string) {
	// 已经开着就是"请求已满足"。
	//
	// 这一条要放在最前面:页面刷新、断线重连、主机自己开完之后对方又点了
	// 一次,都会走到这里。没有它,一次已经完成的请求会再去敲一次主机的门。
	if s.alreadyOpen() {
		sess.write(serverMsg{Type: msgOpen, State: openOpened, Reason: "已经开着"})
		return
	}

	// 密码捷径。和 handleCtl 那一条是同一个暗号、同一个计数器。
	//
	// ⚠️ 这条路比控制那条**更重**:它一条消息就同时决定了"开始共享"和
	// "打开远控",主机全程不用动手。默认密码 123456 + 请求入口默认开合起来
	// 就是"链接发给谁,谁就能自己把这两样打开"——见 config 里那段取舍。
	wantPw := s.currentPassword()
	if authOK := wantPw != "" && passwordMatch(wantPw, pw); authOK {
		sess.pwFails = 0
		log.Printf("远程控制:%s 控制密码通过,自动放行开启请求", sess.label)
		s.openFor(sess)
		return
	} else if wantPw != "" && pw != "" {
		// 填错了。和 handleCtl 一样:这算一次失败,但不直接拒绝 ——
		// 落回"请主机批准"那条路,让主机看见这扇门被敲过。
		if s.passwordFail(sess) {
			return
		}
	}

	switch state := s.control.request(sess.id, sess.label, pendingOpen); state {
	case statePending:
		sess.write(serverMsg{Type: msgOpen, State: openPending})
		log.Printf("远程控制:%s 请求开启共享与远控,等待主机批准", sess.label)

	case stateGranted:
		// 他已经在控制这台电脑了 —— 那意味着远控本来就开着,缺的只是共享。
		// 没有需要主机点头的新东西,直接落地。重走一遍授权是无害的:
		// 客户端把 granted 当幂等状态处理。
		s.openFor(sess)
		return

	case stateBusy:
		// 待批名额被占着。措辞不分"控制申请"还是"开启请求":观众那侧
		// 只知道有人比他先一步,而主机那侧无论如何也只会看到一份待批。
		sess.write(serverMsg{Type: msgOpen, State: stateBusy, Reason: "已经有一份申请在等着处理了"})
	}

	s.notify()
}

// alreadyOpen 报告"共享和远控都已经开着"。
func (s *Server) alreadyOpen() bool {
	return s.isEnabled() && s.sharing()
}

// openFor 落地一次开启,并把结果告诉发起者。
//
// 两条路都会走到它(主机点允许、填对密码),而且都**不带任何先决的授权**:
// 控制权在这里、且在开启成功之后才判,见下面那段顺序说明。
//
// 跑在独立协程里:里面要绑端口、写配置(网络 I/O),而两条调用路径都压在
// 别人的读协程上 —— 主机批准那一条还挂在弹窗的等待协程上,堵住它就等于
// 把下一个弹窗也卡住。
func (s *Server) openFor(sess *session) {
	go func() {
		res := s.runOpen()
		if res.Err != nil {
			// 失败时不需要收拾控制权:它从来就没交出去过(control.approve
			// 对开启请求只收回名额)。这正是那里要分两种 kind 的原因。
			s.noteOpenFailure(res.Err)
			log.Printf("远程控制:开启失败: %v", res.Err)
			sess.write(serverMsg{Type: msgOpen, State: openFailed, Reason: openFailReason(res)})
			s.notify()
			return
		}

		// ⚠️ 顺序:**先开启、后授权**。
		//
		// handleInput 只看"你是不是控制者",不看远控开没开 —— 而请求入口
		// 让"端口在监听、远控没开"成了常态。反过来写,在"已经授权、还没
		// 开启"的那段窗口里,注入的事件会被真的送到主机的鼠标键盘上。
		//
		// 授权放在这里(而不是在批准那一侧)就是为此:runOpen 已经返回,
		// 远控确实开着,这时候给控制权才是安全的。handleInput 里那道
		// isEnabled 护栏是第二道,留着是因为这是安全属性,不该只靠一处
		// 代码的顺序来保证。
		switch s.control.grantIfFree(sess.id, sess.label) {
		case stateGranted:
			sess.write(serverMsg{Type: msgOpen, State: openOpened, Reason: "已开启"})
			s.grant(sess)
		default:
			// 开是开起来了,但控制权这一格没轮到他(有人正在控制,或者
			// 别人在排队等批准)。这不影响他看画面。
			sess.write(serverMsg{
				Type:   msgOpen,
				State:  openOpened,
				Reason: "已开启,但当前有人在控制",
			})
		}
		s.notify()
	}()
}

// runOpen 落地一次开启,并且串行化。
//
// 已经开着就直接返回成功,不再调回调 —— 这既是幂等,也顺手挡住了
// "两个请求同时通过"时把同一件事做两遍。
func (s *Server) runOpen() OpenResult {
	s.openMu.Lock()
	defer s.openMu.Unlock()

	if s.alreadyOpen() {
		return OpenResult{Sharing: true, Remote: true}
	}
	if s.opts.OnApproveOpen == nil {
		return OpenResult{Err: errors.New("这一版不支持通过请求开启")}
	}
	return s.opts.OnApproveOpen()
}

// noteOpenFailure 把落地失败记进状态,让控制页和托盘看得见。
func (s *Server) noteOpenFailure(err error) {
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
}

// openFailReason 把落地结果翻成观众能读懂的一句话。
//
// "共享起来了但远控没开"这一种要单独说:那时候画面是能看的,只是不能操作。
// 笼统地回一句"失败了"会让人以为整个没成,然后就不看了。
func openFailReason(res OpenResult) string {
	if res.Sharing {
		return "共享已开始,但远程控制没能打开:" + openErrText(res.Err) + "。可以先看画面。"
	}
	return "没能开启:" + openErrText(res.Err)
}

func openErrText(err error) string {
	if err == nil {
		return "原因不明"
	}
	return err.Error()
}
