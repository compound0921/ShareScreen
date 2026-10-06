package remotectl

import (
	"testing"
	"time"
)

// ---------- 控制权 ----------

func TestControlSingleController(t *testing.T) {
	var c control

	if got := c.request("a", "10.0.0.1"); got != statePending {
		t.Fatalf("第一个申请应当进入 pending,得到 %q", got)
	}
	// 第二个人在有人排队时应当直接被告知"忙",而不是跟着排队 ——
	// 排队会出现"申请成功却动不了"的错觉。
	if got := c.request("b", "10.0.0.2"); got != stateBusy {
		t.Fatalf("有人排队时第二个申请应当是 busy,得到 %q", got)
	}
	// 重复申请不应当重置计时
	if got := c.request("a", "10.0.0.1"); got != statePending {
		t.Fatalf("重复申请应当仍是 pending,得到 %q", got)
	}

	if !c.approve("a") {
		t.Fatal("批准 a 应当成功")
	}
	if got := c.holder(); got != "a" {
		t.Fatalf("控制者是 %q,期望 a", got)
	}
	// a 已经在控制了,重复申请应当直接得到 granted
	if got := c.request("a", "10.0.0.1"); got != stateGranted {
		t.Fatalf("控制者重复申请应当是 granted,得到 %q", got)
	}
	// 这时 b 再来申请只能是 busy
	if got := c.request("b", "10.0.0.2"); got != stateBusy {
		t.Fatalf("已被占用时应当是 busy,得到 %q", got)
	}
}

func TestControlApproveWrongID(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1")

	if c.approve("b") {
		t.Fatal("批准一个没在申请的会话不该成功")
	}
	if c.holder() != "" {
		t.Fatal("失败的批准不该改变控制者")
	}
	if !c.approve("a") {
		t.Fatal("批准 a 应当成功")
	}
}

func TestControlRevokeAndRelease(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1")
	c.approve("a")

	if c.release("b") {
		// b 不是控制者,归还应当无效
		t.Fatal("非控制者归还不该成功")
	}
	if c.holder() != "a" {
		t.Fatal("非控制者归还后控制者不该变")
	}

	if !c.release("a") {
		t.Fatal("控制者归还应成功")
	}
	if c.holder() != "" {
		t.Fatal("归还后不该还有控制者")
	}

	// 夺回在没人在控制时必须是空操作 —— 它是主机那边的刹车,不能报错
	if _, ok := c.revoke(); ok {
		t.Fatal("没人在控制时夺回不该报告成功")
	}
}

func TestControlRevoke(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1")
	c.approve("a")

	id, ok := c.revoke()
	if !ok || id != "a" {
		t.Fatalf("夺回应当返回 a,得到 %q/%v", id, ok)
	}
	if c.holder() != "" {
		t.Fatal("夺回后不该还有控制者")
	}
}

func TestControlDropOnDisconnect(t *testing.T) {
	var c control

	// 排队中的人断了:名额要放开,否则别人永远申请不了
	c.request("a", "10.0.0.1")
	if wasOwner := c.drop("a"); wasOwner {
		t.Fatal("还在排队的人断开不该被当成控制者")
	}
	if got := c.request("b", "10.0.0.2"); got != statePending {
		t.Fatalf("排队者断开后应当能重新申请,得到 %q", got)
	}

	// 控制者断了:控制权要释放
	c.approve("b")
	if wasOwner := c.drop("b"); !wasOwner {
		t.Fatal("控制者断开应当被识别为控制者")
	}
	if c.holder() != "" {
		t.Fatal("控制者断开后控制权应当释放")
	}
}

func TestControlExpire(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1")

	if _, ok := c.expire(time.Now()); ok {
		t.Fatal("刚提交的申请不该立刻过期")
	}

	id, ok := c.expire(time.Now().Add(PendingTimeout + time.Second))
	if !ok || id != "a" {
		t.Fatalf("超时后应当过期并返回 a,得到 %q/%v", id, ok)
	}
	// 过期之后名额要放开
	if got := c.request("b", "10.0.0.2"); got != statePending {
		t.Fatalf("过期后应当能重新申请,得到 %q", got)
	}
}

func TestControlSnapshot(t *testing.T) {
	var c control
	c.request("a", "192.168.1.7")

	owner, pending := c.snapshot()
	if owner != "" {
		t.Fatalf("还没批准,不该有控制者,得到 %q", owner)
	}
	if pending == nil || pending.RemoteIP != "192.168.1.7" {
		t.Fatalf("待批准信息不对: %+v", pending)
	}

	c.approve("a")
	owner, pending = c.snapshot()
	if owner != "192.168.1.7" {
		t.Fatalf("控制者应当是申请者的地址,得到 %q", owner)
	}
	if pending != nil {
		t.Fatalf("批准之后不该还有待批准项: %+v", pending)
	}
}

// ---------- 注入队列的顺序 ----------

// TestQueueKeepsOrder 是这个功能里最要紧的一条测试。
//
// "鼠标移到按钮上、然后点击"必须按这个顺序到达,否则点中的是上一个位置
// 的东西 —— 在真机上表现为"时不时点错",极难复现也极难归因。
func TestQueueKeepsOrder(t *testing.T) {
	s := newSession("s1", "10.0.0.1", nil, &Server{})

	s.push(injEvent{kind: injMove, x: 0.1, y: 0.1})
	s.push(injEvent{kind: injButton, button: 0, down: true})
	s.push(injEvent{kind: injMove, x: 0.9, y: 0.9})
	s.push(injEvent{kind: injButton, button: 0, down: false})

	want := []struct {
		kind injKind
		x, y float64
	}{
		{injMove, 0.1, 0.1}, // 点击前的那次移动不能被后来的移动吃掉
		{injButton, 0, 0},
		{injMove, 0.9, 0.9},
		{injButton, 0, 0},
	}

	for i, w := range want {
		ev, ok := s.pop()
		if !ok {
			t.Fatalf("第 %d 个事件不见了", i)
		}
		if ev.kind != w.kind {
			t.Fatalf("第 %d 个事件类型是 %d,期望 %d", i, ev.kind, w.kind)
		}
		if ev.kind == injMove && (ev.x != w.x || ev.y != w.y) {
			t.Fatalf("第 %d 个移动是 (%v,%v),期望 (%v,%v)", i, ev.x, ev.y, w.x, w.y)
		}
	}
	if _, ok := s.pop(); ok {
		t.Fatal("队列里不该还有事件")
	}
}

// TestQueueCoalescesConsecutiveMoves 确认连续的移动会被合并成最后一个。
func TestQueueCoalescesConsecutiveMoves(t *testing.T) {
	s := newSession("s1", "10.0.0.1", nil, &Server{})

	for i := 0; i < 50; i++ {
		s.push(injEvent{kind: injMove, x: float64(i) / 50})
	}

	if n := len(s.queue); n != 1 {
		t.Fatalf("50 次连续移动应当合并成 1 个事件,实际 %d", n)
	}
	ev, _ := s.pop()
	if ev.x != 49.0/50 {
		t.Fatalf("合并后应当保留最后一次移动,得到 %v", ev.x)
	}
}

// TestQueueDoesNotDropDiscreteEvents 确认队列满时丢掉的是移动,而不是
// 按下/抬起 —— 丢掉一个"抬起"会让主机的鼠标键永远卡在按下状态。
func TestQueueDoesNotDropDiscreteEvents(t *testing.T) {
	s := newSession("s1", "10.0.0.1", nil, &Server{})

	for i := 0; i < maxQueue; i++ {
		if !s.push(injEvent{kind: injKey, vk: 0x41, down: true}) {
			t.Fatalf("第 %d 个离散事件不该被拒绝", i)
		}
	}
	// 队列满了。再来一个离散事件应当报告失败(交给上层断开),而不是
	// 悄悄丢掉其中一个按键事件。
	if s.push(injEvent{kind: injKey, vk: 0x41, down: false}) {
		t.Fatal("队列满时离散事件应当报告失败")
	}
}

// TestQueueClosedRejects 确认关闭后的会话不再收事件。
func TestQueueClosedRejects(t *testing.T) {
	s := newSession("s1", "10.0.0.1", nil, &Server{})
	s.close()

	if !s.push(injEvent{kind: injMove, x: 0.5}) {
		t.Fatal("已关闭的会话应当安静地丢弃事件,而不是报告积压")
	}
	if n := len(s.queue); n != 0 {
		t.Fatalf("已关闭的会话不该再收事件,实际 %d", n)
	}
}

// TestDrain 确认夺回控制权之后,排队中的事件会被清掉。
//
// 不清的话,夺权前的那几次点击还会打出去 —— 等于没夺成。
func TestDrain(t *testing.T) {
	s := newSession("s1", "10.0.0.1", nil, &Server{})
	s.push(injEvent{kind: injButton, button: 0, down: true})
	s.push(injEvent{kind: injButton, button: 0, down: false})

	s.drain()

	if _, ok := s.pop(); ok {
		t.Fatal("清空之后不该还能取到事件")
	}
}

// ---------- 令牌 ----------

func TestTokenMatch(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	if len(tok) < 32 {
		t.Fatalf("令牌太短: %d 个字符", len(tok))
	}
	if !tokenMatch(tok, tok) {
		t.Fatal("同一个令牌应当匹配")
	}
	if tokenMatch(tok, tok+"x") {
		t.Fatal("不同的令牌不该匹配")
	}
	// 空令牌必须一律拒绝:配置里令牌为空时(还没开启过),不能让
	// 一个空 ?t= 就通过。
	if tokenMatch("", "") || tokenMatch("", tok) || tokenMatch(tok, "") {
		t.Fatal("空令牌不该匹配任何东西")
	}
}

func TestNewTokenUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("生成令牌失败: %v", err)
		}
		if seen[tok] {
			t.Fatal("生成了重复的令牌")
		}
		seen[tok] = true
	}
}

// ---------- 按键编号 ----------

func TestButtonOf(t *testing.T) {
	for i, want := range []string{"左", "中", "右"} {
		if _, ok := buttonOf(i); !ok {
			t.Fatalf("按键 %d(%s)应当能映射", i, want)
		}
	}
	if _, ok := buttonOf(9); ok {
		t.Fatal("未知按键编号不该映射成功")
	}
	if _, ok := buttonOf(-1); ok {
		t.Fatal("负数按键编号不该映射成功")
	}
}
