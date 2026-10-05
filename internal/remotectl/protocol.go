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
}

// 客户端消息类型。
const (
	// msgCtl 是控制权协商:请求接管、主动归还。
	msgCtl = "ctl"
	// msgIn 是一次输入事件。
	msgIn = "in"
	// msgClip 把一段文本写进主机剪贴板。
	msgClip = "clip"
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

// 错误码(serverMsg.Code)。
const (
	// errInject 是注入失败 —— 最典型的原因是前台窗口以管理员身份运行。
	errInject = "inject_failed"
	// errRate 是观众发的输入太快,连接已被断开。
	errRate = "rate_limited"
	// errBadMsg 是收到了解析不了的消息。
	errBadMsg = "bad_message"
)
