package remotectl

// 这个文件是观看页和主机之间那条 WebSocket 上的全部消息定义。
// 两边各有一份实现(Go 和 rc.js),改这里就要同时改那边。

// ---------- 观众 → 主机 ----------

// clientMsg 是观众端发来的一条消息。
//
// 所有字段都带 omitempty 且用指针表示"可选的布尔/数字",因为 JSON 里
// 的 false 和 0 与"没填"无法区分 —— 而 down:false(抬起按键)是一个
// 必须能表达的值,不能和"这条消息没有 down 字段"混为一谈。
type clientMsg struct {
	Type string  `json:"t"`
	Act  string  `json:"act,omitempty"` // t="ctl" 时的动作:request / release
	Kind string  `json:"k,omitempty"`   // t="in" 时的事件类型:mv/btn/whl/key/txt
	X    float64 `json:"x,omitempty"`   // 归一化坐标,0~1
	Y    float64 `json:"y,omitempty"`
	B    *int    `json:"b,omitempty"`    // 鼠标按键:0左 1中 2右
	Down *bool   `json:"down,omitempty"` // 按下还是抬起
	DX   int     `json:"dx,omitempty"`   // 滚轮横向,单位 120
	DY   int     `json:"dy,omitempty"`   // 滚轮纵向
	Code string  `json:"code,omitempty"` // KeyboardEvent.code
	S    string  `json:"s,omitempty"`    // 文本(打字 / 剪贴板)

	// Pw 是观众在观看页上填的控制密码,随申请一起送上来。
	//
	// 它和下面 serverMsg.PasswordSet 不是一回事:这个字段装的是秘密本身,
	// 那个是个布尔("主机设没设密码")。故意不叫同一个名字,免得两边读串。
	Pw string `json:"pw,omitempty"`
}

// 客户端消息类型。
const (
	// msgCtl 是控制权协商:请求接管、主动归还。
	msgCtl = "ctl"
	// msgOpen 是"请求开启共享与远控":请求、撤回。
	//
	// 和 msgCtl 分开而不是给 ctl 加一个动作,是因为它回答的是另一个问题:
	// ctl 问"能不能让我操作",前提是远控已经开着;open 问"能不能把它开起来",
	// 而它正是远控没开的时候唯一的出路。两者的状态机也只有一处相同(都占
	// 主机那一份待批名额)。
	msgOpen = "open"
	// msgIn 是一次输入事件。
	msgIn = "in"
	// msgClip 把一段文本写进主机剪贴板。
	msgClip = "clip"
	// msgHold 是主机自己在用键鼠、注入暂时让路。只由主机→观众单向发。
	msgHold = "hold"
)

// 输入事件类型(t="in" 时的 k)。
const (
	kindMove   = "mv"
	kindButton = "btn"
	kindWheel  = "whl"
	kindKey    = "key"
	kindText   = "txt"
)

// ctl 动作。
const (
	actRequest = "request"
	actRelease = "release"
)

// ---------- 主机 → 观众 ----------

// serverMsg 是主机推给观众的消息。
//
// 用一个扁平结构而不是每种消息一个类型:消息种类就这么几种,而扁平结构
// 在 JS 那边读起来也更直接。
type serverMsg struct {
	Type string `json:"t"`

	// t="hello" 时告诉观众它的会话 ID,以及本机是否开启了剪贴板同步。
	You       string `json:"you,omitempty"`
	Clipboard bool   `json:"clipboard,omitempty"`

	// Remote 和 Sharing 是连上那一刻的远控/共享状态,t="hello" 时下发。
	//
	// 页面靠这两个值决定自己该是"请求开启"那一套还是"申请控制"那一套。
	// 不能用页面自己缓存的上一次状态:观众可能是在主机点完允许之后才
	// 刷新页面的,那样他会看到一个已经过时的"请求开启"按钮。
	Remote  bool `json:"remote,omitempty"`
	Sharing bool `json:"sharing,omitempty"`

	// PasswordSet 表示主机设了控制密码,t="hello" 时下发。
	//
	// 只用来决定观看页要不要显示那个密码框 —— 主机没设密码时摆一个填了
	// 也没用的输入框,比不显示更让人困惑。**不是凭据**,只是个开关状态。
	PasswordSet bool `json:"passwordSet,omitempty"`

	// t="ctl" 时的控制权状态。
	State  string `json:"state,omitempty"`
	Reason string `json:"reason,omitempty"`

	// t="clip" 时主机剪贴板的内容。
	S string `json:"s,omitempty"`

	// t="err" 时的错误码和说明。
	Code string `json:"code,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// 控制权状态(serverMsg.State)。
const (
	// stateGranted 表示这条连接现在可以操作主机。
	stateGranted = "granted"
	// statePending 表示请求已送到,等主机点"允许"。
	statePending = "pending"
	// stateDenied 表示主机拒绝了,或者等太久过期了。
	stateDenied = "denied"
	// stateRevoked 表示主机把控制权收回去了。
	stateRevoked = "revoked"
	// stateBusy 表示已经有人在控制,这条连接只能看。
	stateBusy = "busy"
	// stateIdle 表示当前没人控制,可以申请。
	stateIdle = "idle"
)

// "请求开启"的状态(t="open" 时的 State)。
//
// 刻意和上面那一组分开命名:两边都有 pending/denied,但含义不同(一边是
// 等着拿控制权,一边是等着把功能打开),共用常量会让 rc.js 里那两个分支
// 看起来可以互相替代,而它们不能。
const (
	// openPending 表示请求已送到,等主机点"允许"。
	openPending = "pending"
	// openOpened 表示共享和远控都已经打开,接下来就能看能控了。
	openOpened = "opened"
	// openDenied 表示主机拒绝了,或者等太久过期了。
	openDenied = "denied"
	// openFailed 表示主机点了允许,但落地的时候失败了(端口被占、采集源
	// 不支持之类)。和 denied 分开:这不是"不同意",是"同意但没做成",
	// 页面上的措辞和可重试性都不一样。
	openFailed = "failed"
)

// 让路状态(t="hold" 时的 State)。
//
// 用字符串而不是布尔,是因为 serverMsg 的字段全带 omitempty —— 一个
// false 会被整个省掉,"关掉了"和"没提过这件事"在 JSON 里就分不出来了。
const (
	// holdOn 表示主机正在操作,注入已让开。
	holdOn = "on"
	// holdOff 表示让路结束,注入恢复。
	holdOff = "off"
)

// 错误码(serverMsg.Code)。
const (
	// errInject 是注入失败 —— 最典型的原因是前台窗口以管理员身份运行。
	errInject = "inject_failed"
	// errRate 是观众发的输入太快,连接已被断开。
	errRate = "rate_limited"
	// errBadMsg 是收到了解析不了的消息。
	errBadMsg = "bad_message"
	// errAuth 是控制密码连续填错太多次,连接已被断开。
	errAuth = "auth_failed"
)
