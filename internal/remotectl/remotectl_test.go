package remotectl

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"sharescreen/internal/input"
	"sharescreen/internal/screen"
)

// ---------- 控制权 ----------

func TestControlSingleController(t *testing.T) {
	var c control

	if got := c.request("a", "10.0.0.1", pendingControl); got != statePending {
		t.Fatalf("第一个申请应当进入 pending,得到 %q", got)
	}
	// 第二个人在有人排队时应当直接被告知"忙",而不是跟着排队 ——
	// 排队会出现"申请成功却动不了"的错觉。
	if got := c.request("b", "10.0.0.2", pendingControl); got != stateBusy {
		t.Fatalf("有人排队时第二个申请应当是 busy,得到 %q", got)
	}
	// 重复申请不应当重置计时
	if got := c.request("a", "10.0.0.1", pendingControl); got != statePending {
		t.Fatalf("重复申请应当仍是 pending,得到 %q", got)
	}

	if _, ok := c.approve("a"); !ok {
		t.Fatal("批准 a 应当成功")
	}
	if got := c.holder(); got != "a" {
		t.Fatalf("控制者是 %q,期望 a", got)
	}
	// a 已经在控制了,重复申请应当直接得到 granted
	if got := c.request("a", "10.0.0.1", pendingControl); got != stateGranted {
		t.Fatalf("控制者重复申请应当是 granted,得到 %q", got)
	}
	// 这时 b 再来申请只能是 busy
	if got := c.request("b", "10.0.0.2", pendingControl); got != stateBusy {
		t.Fatalf("已被占用时应当是 busy,得到 %q", got)
	}
}

func TestControlApproveWrongID(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1", pendingControl)

	if _, ok := c.approve("b"); ok {
		t.Fatal("批准一个没在申请的会话不该成功")
	}
	if c.holder() != "" {
		t.Fatal("失败的批准不该改变控制者")
	}
	if _, ok := c.approve("a"); !ok {
		t.Fatal("批准 a 应当成功")
	}
}

func TestControlRevokeAndRelease(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1", pendingControl)
	c.approve("a")

	if _, ok := c.release("b"); ok {
		// b 既不在控制也没排队,放弃应当无效
		t.Fatal("没占任何位置的会话放弃不该成功")
	}
	if c.holder() != "a" {
		t.Fatal("别人的放弃后控制者不该变")
	}

	wasOwner, ok := c.release("a")
	if !ok || !wasOwner {
		t.Fatal("控制者归还应成功,并报告自己是控制者")
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
	c.request("a", "10.0.0.1", pendingControl)
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
	c.request("a", "10.0.0.1", pendingControl)
	if wasOwner := c.drop("a"); wasOwner {
		t.Fatal("还在排队的人断开不该被当成控制者")
	}
	if got := c.request("b", "10.0.0.2", pendingControl); got != statePending {
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
	c.request("a", "10.0.0.1", pendingControl)

	if _, _, ok := c.expire(time.Now()); ok {
		t.Fatal("刚提交的申请不该立刻过期")
	}

	id, _, ok := c.expire(time.Now().Add(PendingTimeout + time.Second))
	if !ok || id != "a" {
		t.Fatalf("超时后应当过期并返回 a,得到 %q/%v", id, ok)
	}
	// 过期之后名额要放开
	if got := c.request("b", "10.0.0.2", pendingControl); got != statePending {
		t.Fatalf("过期后应当能重新申请,得到 %q", got)
	}
}

func TestControlSnapshot(t *testing.T) {
	var c control
	c.request("a", "192.168.1.7", pendingControl)

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

// TestControlCancelPending 是"取消申请":排队的人主动撤掉。
//
// 这条以前是坏的 —— release 只认控制者,排队的人点那个按钮,服务端什么
// 也不做,而界面上那个按钮就在他眼前。它必须真的把名额放开,否则别人
// (以及他自己)在超时之前都申请不了。
func TestControlCancelPending(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1", pendingControl)

	wasOwner, ok := c.release("a")
	if !ok {
		t.Fatal("排队者撤回申请应当成功")
	}
	if wasOwner {
		t.Fatal("排队的人不是控制者,不该被当成归还控制权")
	}
	if _, pending := c.snapshot(); pending != nil {
		t.Fatalf("撤回之后不该还挂在待批准里: %+v", pending)
	}
	// 名额要放开:别人现在能申请了
	if got := c.request("b", "10.0.0.2", pendingControl); got != statePending {
		t.Fatalf("撤回之后名额应当放开,得到 %q", got)
	}
}

// 撤回的是自己的那份,不能顺手把别人正在等的申请也撤掉。
func TestControlCancelPendingDoesNotTouchOthers(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1", pendingControl)

	if _, ok := c.release("b"); ok {
		t.Fatal("没排队的人放弃不该成功")
	}
	owner, pending := c.snapshot()
	if owner != "" || pending == nil || pending.RemoteIP != "10.0.0.1" {
		t.Fatalf("a 的申请不该被动到: owner=%q pending=%+v", owner, pending)
	}
}

// ---------- 观看页的配置接口 ----------

// TestConfigEndpointReportsLANHosts 钉住 /rc/config 里的 lan 那一条。
//
// 观看页靠它判断"我是不是该换到局域网地址重开"。这个字段一旦悄悄没了,
// 表现是**没有任何报错、延迟默默变回绕公网**—— 因为页面会安安静静地回到
// "用 location.hostname"那条老路。所以它值得一条测试。
func TestConfigEndpointReportsLANHosts(t *testing.T) {
	s := New(Options{
		WebRTCPort: 8889,
		StreamPath: "live",
		LANHosts:   []string{"192.168.3.236", "10.126.126.1"},
		Rotate:     func() int { return 180 },
	})

	rec := httptest.NewRecorder()
	s.handleConfig(rec, httptest.NewRequest(http.MethodGet, "/rc/config", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d,期望 200", rec.Code)
	}
	var got struct {
		WHEPPort int      `json:"whepPort"`
		Path     string   `json:"path"`
		LAN      []string `json:"lan"`
		Rotate   int      `json:"rotate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析失败: %v(%s)", err, rec.Body.String())
	}
	if got.WHEPPort != 8889 || got.Path != "live" {
		t.Fatalf("端口/路径不对: %+v", got)
	}
	if len(got.LAN) != 2 || got.LAN[0] != "192.168.3.236" {
		t.Fatalf("局域网地址表不对(顺序也有含义,第一个是优先探的): %+v", got.LAN)
	}
	// 观看页靠它决定要不要把画面转回来。取不到就是画面倒着,而没有任何报错。
	if got.Rotate != 180 {
		t.Fatalf("旋转角不对: %d", got.Rotate)
	}
}

// Rotate 没配时下发 0,而不是让接口崩 —— 页面对 0 的处理就是"不转"。
func TestConfigEndpointRotateDefaultsZero(t *testing.T) {
	s := New(Options{})
	rec := httptest.NewRecorder()
	s.handleConfig(rec, httptest.NewRequest(http.MethodGet, "/rc/config", nil))

	var got struct {
		Rotate int `json:"rotate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Rotate != 0 {
		t.Fatalf("没配的时候应当是 0,得到 %d", got.Rotate)
	}
}

// 探测不到局域网地址(纯内网、离线)时这一项是空的,而不是让接口报错 ——
// 页面那边 `cfg.lan || []` 吃得住空值,但吃不接口出错。
func TestConfigEndpointLANEmpty(t *testing.T) {
	s := New(Options{})
	rec := httptest.NewRecorder()
	s.handleConfig(rec, httptest.NewRequest(http.MethodGet, "/rc/config", nil))

	var got struct {
		LAN []string `json:"lan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析失败: %v(%s)", err, rec.Body.String())
	}
	if len(got.LAN) != 0 {
		t.Fatalf("没有局域网地址时应当是空的,得到 %+v", got.LAN)
	}
}

// ---------- 控制密码(自动批准) ----------

// TestGrantIfFree 覆盖密码通道的冲突判定:没人占的时候直接给,有人占着
// 的时候和普通申请一样是 busy —— 密码是"不用麻烦主机点一下",不是夺权。
func TestGrantIfFree(t *testing.T) {
	var c control

	if got := c.grantIfFree("a", "10.0.0.1"); got != stateGranted {
		t.Fatalf("没人控制时应当直接授予,得到 %q", got)
	}
	if c.holder() != "a" {
		t.Fatalf("控制者是 %q,期望 a", c.holder())
	}
	// 别人正在控制:密码再对也不能把人顶下来
	if got := c.grantIfFree("b", "10.0.0.2"); got != stateBusy {
		t.Fatalf("已经有人在控制时应当是 busy,得到 %q", got)
	}
	if c.holder() != "a" {
		t.Fatalf("busy 不该改变控制者,现在是 %q", c.holder())
	}
	// 控制者自己再点一次是幂等的
	if got := c.grantIfFree("a", "10.0.0.1"); got != stateGranted {
		t.Fatalf("控制者重复申请应当是 granted,得到 %q", got)
	}
}

// TestGrantIfFreeWithPending 是这条通道最容易写错的一处:别人已经在排队
// 等主机批准时不能插队,而**排队的本人**补上密码应当升格,不是被告知 busy。
func TestGrantIfFreeWithPending(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1", pendingControl) // a 先申请了,在等主机点允许

	if got := c.grantIfFree("b", "10.0.0.2"); got != stateBusy {
		t.Fatalf("别人在排队时不该插队,得到 %q", got)
	}

	// a 又想起来了密码,补填一次
	if got := c.grantIfFree("a", "10.0.0.1"); got != stateGranted {
		t.Fatalf("排队者补上密码应当升格为控制者,得到 %q", got)
	}
	owner, pending := c.snapshot()
	if owner != "10.0.0.1" {
		t.Fatalf("控制者应当是 a 的地址,得到 %q", owner)
	}
	if pending != nil {
		t.Fatalf("升格之后不该还挂在待批准列表里: %+v", pending)
	}
}

func TestPasswordMatch(t *testing.T) {
	if !passwordMatch("hunter2", "hunter2") {
		t.Fatal("同一个密码应当匹配")
	}
	if passwordMatch("hunter2", "hunter3") {
		t.Fatal("不同的密码不该匹配")
	}
	// 主机**没设**密码时(空串),一个空输入绝不能被当成匹配 ——
	// 那等于所有人自动获批,等于第二层门槛没了。
	if passwordMatch("", "") || passwordMatch("", "hunter2") || passwordMatch("hunter2", "") {
		t.Fatal("空密码不该匹配任何东西")
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

// ---------- 请求开启共享与远控 ----------

// 两种申请共用同一份待批名额:控制申请在等的时候,开启请求得到 busy,
// 反之亦然。
//
// 这条不是为了省一个字段,是为了主机的界面:无论如何都只有一份待批,
// 弹窗、控制页那一块、托盘提示就都不必去解释"为什么有两份申请在等"。
func TestKindSharesOnePendingSlot(t *testing.T) {
	var c control

	if got := c.request("a", "10.0.0.1", pendingControl); got != statePending {
		t.Fatalf("控制申请应当进入 pending,得到 %q", got)
	}
	if got := c.request("b", "10.0.0.2", pendingOpen); got != stateBusy {
		t.Fatalf("名额被控制申请占着时,开启请求应当是 busy,得到 %q", got)
	}

	var c2 control
	if got := c2.request("a", "10.0.0.1", pendingOpen); got != statePending {
		t.Fatalf("开启请求应当进入 pending,得到 %q", got)
	}
	if got := c2.request("b", "10.0.0.2", pendingControl); got != stateBusy {
		t.Fatalf("名额被开启请求占着时,控制申请应当是 busy,得到 %q", got)
	}
}

// kind 要能穿过快照、批准、拒绝、过期这四道 —— 主机的"允许"该做什么、
// 拒绝和超时该回什么话,全看它。
func TestKindSurvivesSnapshot(t *testing.T) {
	var c control
	c.request("a", "192.168.1.7", pendingOpen)

	_, pending := c.snapshot()
	if pending == nil || pending.Kind != pendingOpen {
		t.Fatalf("快照里应当带上 kind=open,得到 %+v", pending)
	}
}

func TestKindSurvivesDeny(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1", pendingOpen)

	kind, ok := c.deny("a")
	if !ok || kind != pendingOpen {
		t.Fatalf("deny 应当返回 open/true,得到 %q/%v", kind, ok)
	}
	// 拒绝之后名额要放开
	if got := c.request("b", "10.0.0.2", pendingControl); got != statePending {
		t.Fatalf("拒绝之后应当能重新申请,得到 %q", got)
	}
}

func TestKindSurvivesExpire(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1", pendingOpen)

	if _, _, ok := c.expire(time.Now()); ok {
		t.Fatal("刚提交的申请不该立刻过期")
	}
	id, kind, ok := c.expire(time.Now().Add(PendingTimeout + time.Second))
	if !ok || id != "a" || kind != pendingOpen {
		t.Fatalf("过期应当带出 a/open,得到 %q/%q/%v", id, kind, ok)
	}
}

// 批准一份**开启请求**不该顺手把控制权交出去。
//
// 开启请求批准之后还要去把共享和远控真正打开,而那一步会失败。先授权
// 再开启的话,失败之后会留下"界面说你正在控制、实际按什么都没反应";
// 更要紧的是输入那条路只看"你是不是控制者"(见 handleInput),所以那个
// 状态会真的让输入送到主机上。
func TestApproveOpenDoesNotGrantControl(t *testing.T) {
	var c control
	c.request("a", "10.0.0.1", pendingOpen)

	kind, ok := c.approve("a")
	if !ok || kind != pendingOpen {
		t.Fatalf("approve 应当返回 open/true,得到 %q/%v", kind, ok)
	}
	if c.holder() != "" {
		t.Fatalf("开启请求批准之后不该有控制者,现在是 %q", c.holder())
	}
	if _, pending := c.snapshot(); pending != nil {
		t.Fatalf("批准之后名额要放掉,否则弹窗关不掉: %+v", pending)
	}

	// 对照:批准一份**控制申请**当然要交出控制权。
	var c2 control
	c2.request("b", "10.0.0.2", pendingControl)
	if kind, ok := c2.approve("b"); !ok || kind != pendingControl {
		t.Fatalf("approve 应当返回 control/true,得到 %q/%v", kind, ok)
	}
	if c2.holder() != "b" {
		t.Fatalf("控制申请批准之后控制者应当是 b,得到 %q", c2.holder())
	}
}

// 远控没开的时候,即使有人是控制者,注入事件也必须被丢掉。
//
// 这在以前是不可能出现的状态(没开远控就没有会话,没有会话就不可能有
// 控制者)。请求入口让它成了常态,而它是"先开启、后授权"那个顺序的最后
// 一道防线 —— 顺序被写反时,这里就是唯一拦住注入的地方。
func TestHandleInputDroppedWhenRemoteDisabled(t *testing.T) {
	var s Server
	s.sessions = map[string]*session{}
	sess := newSession("s1", "10.0.0.1", nil, &s)
	s.sessions[sess.id] = sess

	// 让他成为控制者,但**不**打开远控授权。
	if got := s.control.grantIfFree(sess.id, sess.label); got != stateGranted {
		t.Fatalf("准备阶段应当直接拿到控制权,得到 %q", got)
	}

	s.handleInput(sess, clientMsg{Type: msgIn, Kind: kindMove, X: 0.5, Y: 0.5})
	if _, ok := sess.pop(); ok {
		t.Fatal("远控没开的时候,注入事件不该入队")
	}

	// 授权打开之后,同一条事件要能进去 —— 否则上面那条测试可能只是
	// 因为事件本身构造成了进不去。
	s.enabled = true
	s.handleInput(sess, clientMsg{Type: msgIn, Kind: kindMove, X: 0.5, Y: 0.5})
	if _, ok := sess.pop(); !ok {
		t.Fatal("远控开启之后,控制者的移动事件应当入队")
	}
}

// 填对控制密码的开启请求不弹窗,直接落地 —— 和"申请控制"那条捷径同一个
// 暗号,主机全程不用动手。
func TestRequestOpenWithPasswordSkipsHost(t *testing.T) {
	called := make(chan struct{}, 1)
	s := New(Options{
		OnApproveOpen: func() OpenResult {
			called <- struct{}{}
			return OpenResult{Sharing: true, Remote: true}
		},
	})
	s.Configure(8090, "tok", "hunter2", false)
	sess := newSession("s1", "10.0.0.1", nil, s)

	s.handleOpen(sess, clientMsg{Type: msgOpen, Act: actRequest, Pw: "hunter2"})

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("填对密码应当直接放行,不经过主机批准")
	}
	if _, pending := s.control.snapshot(); pending != nil {
		t.Fatalf("密码那条路不该留下待批项: %+v", pending)
	}
}

// 密码填错**不**直接拒绝,落回"请主机批准"那条路 —— 主机要能看见这扇门
// 被敲过,这和 handleCtl 里那句注释是同一个考虑。
func TestRequestOpenWrongPasswordFallsBackToHost(t *testing.T) {
	s := New(Options{})
	s.Configure(8090, "tok", "hunter2", false)
	sess := newSession("s1", "10.0.0.1", nil, s)

	s.handleOpen(sess, clientMsg{Type: msgOpen, Act: actRequest, Pw: "nope"})

	_, pending := s.control.snapshot()
	if pending == nil || pending.Kind != pendingOpen {
		t.Fatalf("密码错了应当留下一条待主机批准的开启请求,得到 %+v", pending)
	}
	if sess.pwFails != 1 {
		t.Fatalf("密码填错要计数,得到 %d", sess.pwFails)
	}
}

// 两条路共用同一个失败计数器。
//
// 各记一份的话,交替着填就等于把 maxPwFails 翻倍 —— 而那个数字是"闸",
// 不是一个显示用的计数。
func TestPasswordFailCounterShared(t *testing.T) {
	s := New(Options{})
	s.Configure(8090, "tok", "hunter2", false)
	s.sessions = map[string]*session{}
	sess := newSession("s1", "10.0.0.1", nil, s)
	s.sessions[sess.id] = sess

	// 控制申请那条路要先过 isEnabled 那道护栏,否则它会直接回"远控未开启"
	// 而不去数密码。
	s.enabled = true

	s.handleCtl(sess, clientMsg{Type: msgCtl, Act: actRequest, Pw: "nope"})
	s.handleOpen(sess, clientMsg{Type: msgOpen, Act: actRequest, Pw: "nope"})

	if sess.pwFails != 2 {
		t.Fatalf("两条路应当共用同一个计数器,期望 2,得到 %d", sess.pwFails)
	}
}

// 主机批准开启请求之后,落地动作被调用,并且发起者拿到控制权。
func TestApproveOpenRunsActionAndGrants(t *testing.T) {
	done := make(chan struct{}, 1)
	s := New(Options{
		OnApproveOpen: func() OpenResult {
			done <- struct{}{}
			return OpenResult{Sharing: true, Remote: true}
		},
	})
	s.sessions = map[string]*session{}
	sess := newSession("s1", "10.0.0.1", nil, s)
	s.sessions[sess.id] = sess

	if got := s.control.request(sess.id, sess.label, pendingOpen); got != statePending {
		t.Fatalf("准备阶段应当是 pending,得到 %q", got)
	}
	if !s.Approve(sess.id) {
		t.Fatal("批准应当成功")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("批准之后应当去落地开启动作")
	}

	deadline := time.Now().Add(2 * time.Second)
	for s.control.holder() != sess.id {
		if time.Now().After(deadline) {
			t.Fatalf("开启成功之后控制权应当给发起者,现在是 %q", s.control.holder())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 落地失败时:不留下控制权,而且给观众的是 failed 而不是 denied ——
// 那不是"不同意",是"同意了但没做成",页面上该不该重试完全不同。
func TestApproveOpenFailureGrantsNothing(t *testing.T) {
	s := New(Options{
		OnApproveOpen: func() OpenResult {
			return OpenResult{Err: errors.New("端口被占用")}
		},
	})
	s.sessions = map[string]*session{}
	sess := newSession("s1", "10.0.0.1", nil, s)
	s.sessions[sess.id] = sess

	s.control.request(sess.id, sess.label, pendingOpen)
	if !s.Approve(sess.id) {
		t.Fatal("批准应当成功")
	}

	deadline := time.Now().Add(2 * time.Second)
	for s.control.holder() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.control.holder() != "" {
		t.Fatalf("落地失败不该留下控制权,现在控制者是 %q", s.control.holder())
	}
}

// ---------- 本机用户优先 ----------

// fakeInjector 实现 Injector:只记下被调了什么,并且能随时切"主机在不在
// 操作键鼠"。全部经锁访问,好让 -race 下读它也不出事。
type fakeInjector struct {
	mu    sync.Mutex
	busy  bool
	calls []string
}

func (f *fakeInjector) setHostBusy(on bool) {
	f.mu.Lock()
	f.busy = on
	f.mu.Unlock()
}

func (f *fakeInjector) HostActive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy
}

func (f *fakeInjector) log(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeInjector) logged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeInjector) MoveNorm(screen.Rect, float64, float64) error {
	f.log("move")
	return nil
}

func (f *fakeInjector) ButtonEvent(b input.Button, down bool) error {
	dir := "up"
	if down {
		dir = "down"
	}
	f.log(btnName(b) + " " + dir)
	return nil
}

func (f *fakeInjector) Wheel(int, int) error {
	f.log("wheel")
	return nil
}

func (f *fakeInjector) KeyEvent(vk uint16, _, down bool) error {
	dir := "up"
	if down {
		dir = "down"
	}
	f.log(fmt.Sprintf("key %d %s", vk, dir))
	return nil
}

func (f *fakeInjector) TypeText(string) error {
	f.log("text")
	return nil
}

func (f *fakeInjector) ElevatedForeground() bool { return false }

func btnName(b input.Button) string {
	switch b {
	case input.BtnLeft:
		return "left"
	case input.BtnMiddle:
		return "middle"
	case input.BtnRight:
		return "right"
	case input.BtnX1:
		return "x1"
	case input.BtnX2:
		return "x2"
	}
	return "?"
}

// waitInjectLog 等到注入记录正好是这几条(顺序和内容都要对上)。
func waitInjectLog(t *testing.T, f *fakeInjector, want ...string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Equal(f.logged(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("注入记录 = %v,想要 %v", f.logged(), want)
}

// 主机自己在用键鼠的时候注入让路,而且**按着的东西要补一个抬起**。
//
// 后半句是这个功能里最容易漏、后果最讨厌的一处:观众按住左键拖拽,主机一动
// 鼠标,注入停下,而那个左键是已经按下去的了。不替它补抬起,主机接下来自己
// 点的每一下都会变成拖拽 —— 那会被记成"这个软件把我的鼠标弄坏了"。
func TestInjectYieldsToHostAndReleases(t *testing.T) {
	f := &fakeInjector{}
	srv := &Server{inject: f, sessions: map[string]*session{}}
	sess := newSession("s1", "10.0.0.1", nil, srv)
	srv.sessions[sess.id] = sess
	if st := srv.control.grantIfFree(sess.id, sess.label); st != stateGranted {
		t.Fatalf("准备阶段应当拿到控制权,得到 %q", st)
	}

	go sess.injectLoop()
	defer sess.close()

	// 主机没在操作:正常注入,并记下我们按下了左键。
	sess.push(injEvent{kind: injButton, button: input.BtnLeft, down: true})
	waitInjectLog(t, f, "left down")

	// 主机开始用鼠标:这个右键不该被注入(注入让路),而那个还按着的左键
	// 要补一个抬起。
	f.setHostBusy(true)
	sess.push(injEvent{kind: injButton, button: input.BtnRight, down: true})
	waitInjectLog(t, f, "left down", "left up")

	// 主机停手:恢复注入。
	f.setHostBusy(false)
	sess.push(injEvent{kind: injButton, button: input.BtnRight, down: true})
	waitInjectLog(t, f, "left down", "left up", "right down")
}

// 观众按住不放的时候也要让路 —— 这条走的是定时检查那条路。
//
// 没有它的话:观众按着左键不动(拖动时很常见),主机一动鼠标,那个左键就
// 一直按在主机那儿,他点的每一下都变成拖拽。而这期间**没有任何事件**流进
// 注入协程,"事件进来时检查"永远等不到。
func TestInjectYieldsWhileViewerHoldsStill(t *testing.T) {
	f := &fakeInjector{}
	srv := &Server{inject: f, sessions: map[string]*session{}}
	sess := newSession("s1", "10.0.0.1", nil, srv)
	srv.sessions[sess.id] = sess
	srv.control.grantIfFree(sess.id, sess.label)

	go sess.injectLoop()
	defer sess.close()

	sess.push(injEvent{kind: injButton, button: input.BtnLeft, down: true})
	waitInjectLog(t, f, "left down")

	// 主机开始操作,而观众这边什么都不再发了。
	f.setHostBusy(true)
	waitInjectLog(t, f, "left down", "left up")
}

// 键盘也一样:主机在用的时候注入让路,按着的修饰键要松开。
//
// 松不掉的那个后果比鼠标更隐蔽 —— 主机的 Ctrl 一直被按着,后面他按什么都
// 变成组合键,而屏幕上没有任何东西显示这一点。
func TestInjectReleasesHeldKeysToHost(t *testing.T) {
	f := &fakeInjector{}
	srv := &Server{inject: f, sessions: map[string]*session{}}
	sess := newSession("s1", "10.0.0.1", nil, srv)
	srv.sessions[sess.id] = sess
	srv.control.grantIfFree(sess.id, sess.label)

	go sess.injectLoop()
	defer sess.close()

	const vkCtrl, vkA = 0x11, 0x41

	sess.push(injEvent{kind: injKey, vk: vkCtrl, down: true})
	waitInjectLog(t, f, "key 17 down")

	// 让路期间推一个**别的**键:要是它被注进去了,日志里出现的会是
	// "key 65 down",和下面那条抬起的期望值对不上 —— 这样这条测试才真的
	// 在验"补了抬起",而不是碰巧被队列里那个抬起事件蒙对。
	f.setHostBusy(true)
	sess.push(injEvent{kind: injKey, vk: vkA, down: true})
	waitInjectLog(t, f, "key 17 down", "key 17 up")
}
