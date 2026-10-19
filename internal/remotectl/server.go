// Package remotectl 提供远程控制:观众端观看页、输入回传通道和控制权仲裁。
//
// # 和观看链路的关系
//
// 视频一个字都没改,仍然由 ffmpeg 采集编码、MediaMTX 转成 WHEP 给浏览器。
// WHEP 是只读的,没有反向通路 —— 所以这里单独开一条:
//
//	观众浏览器 ──WHEP 拉流──► MediaMTX(原有链路,不动)
//	            ──WebSocket──► 本包(输入 + 控制权信令)──► 注入本机
//
// # 两个开关:授权和监听
//
// 远控本身默认关闭,但另有一个默认打开的「请求入口」:它在远控没开的
// 时候也占着端口监听,专门受理"请求开启共享与远控"。两件事分开是因为
// 这个功能的立足点就是"远控还没开的时候,对方也得够得着主机"——一个
// 开关同时管两件事就做不到。
//
// 监听意味着绑 0.0.0.0,这本身就是有代价的:即使有鉴权,它也仍然是网络上
// 多一个被扫描的目标 —— 而"不监听"是没有这个代价的。这个默认值就是拿它
// 换"发一条链接对方就能请求",取舍写在 config.RemoteControlConfig 的注释里。
//
// # 两层门槛
//
//  1. 链接里带 256 位随机令牌 —— 决定能不能连上来。
//  2. 主机的当场批准 —— 决定能不能真的操作(或者开启)。
//
// 还有控制密码这条捷径,它两层都免掉第二层。见 config 里那段。
//
// 之所以要两层:视频流本身是不鉴权的(链接即凭据),所以"能看"这件事
// 挡不住。让陌生人只能看、不能动,靠的是第二层。
package remotectl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"sharescreen/internal/clipboard"
	"sharescreen/internal/screen"
	"sharescreen/internal/webui"
)

// 几个时间常量。
const (
	// pingInterval 是探测观众是否还活着的间隔。
	//
	// 必须有:观众那边的网络断掉时 TCP 不一定立刻报错(尤其是手机切网、
	// 休眠),没有心跳的话这条连接会一直挂着、占着控制权不放,而主机
	// 看着界面上"有人正在控制"却找不到人。
	pingInterval = 30 * time.Second

	// pingTimeout 是单次心跳的等待上限。
	pingTimeout = 10 * time.Second

	// expireInterval 是检查"申请超时"的间隔。
	expireInterval = 5 * time.Second

	// clipboardInterval 是轮询本机剪贴板变化的间隔。
	//
	// 用轮询而不是监听 WM_CLIPBOARDUPDATE:那个需要窗口消息循环,而本程序
	// 是 GUI 子系统的无控制台程序,没有自己的消息泵。轮询的开销很小 ——
	// 判断变没变只读一个系统序列号,不读内容。
	clipboardInterval = 800 * time.Millisecond

	// maxTextBytes 是单条文本消息(打字、剪贴板)的长度上限。
	//
	// 剪贴板里经常躺着整篇文档,几 MB 的文本经 WebSocket 送过去会把
	// 连接堵住。超出就不同步,并在观众端说明原因 —— 比默默卡住好。
	maxTextBytes = 256 * 1024

	// shutdownTimeout 是关闭监听时等待在途请求结束的时间。
	shutdownTimeout = 2 * time.Second
)

// Pending 是一份等待主机批准的申请。
type Pending struct {
	ID string `json:"id"`

	// Kind 区分两种申请:"control" 是申请操作控制权,"open" 是请求开启
	// 共享与远控。
	//
	// 两种申请共用同一个排队名额(见 control),所以主机的弹窗、控制页上
	// 那一块、托盘提示都只需要认这一个结构 —— 多出来的只有"点允许之后
	// 该做什么",那件事在 Server.Approve 里按它分派。
	Kind string `json:"kind"`

	// RemoteIP 是申请方的地址,给主机判断"这是不是我认识的人"。
	RemoteIP string `json:"remoteIp"`

	At string `json:"at"`
}

// 申请的种类。
const (
	// pendingControl 申请操作控制权。
	pendingControl = "control"
	// pendingOpen 请求开启共享与远控。
	pendingOpen = "open"
)

// Status 是给控制页和托盘看的远控状态快照。
type Status struct {
	// Enabled 是**授权**:能不能操作这台电脑。语义没有变过,托盘和控制页
	// 读的都是它。
	Enabled bool `json:"enabled"`

	// RequestEntry 是请求入口开着没有。它和 Enabled 是两个独立的开关,
	// 但共用同一个端口,见包注释。
	RequestEntry bool `json:"requestEntry"`

	// Sharing 报告主机此刻在不在推流。由 Options.Sharing 提供 —— 共享
	// 的开关在别处(控制页、托盘),这里只是把它反映给远控页,好让观众
	// 知道自己该看到"请求开启"还是"申请控制"。
	Sharing bool `json:"sharing"`

	// Listening 为假而 Enabled(或 RequestEntry)为真,说明端口没绑上
	// (被占用之类)。
	Listening bool `json:"listening"`
	Port      int  `json:"port"`

	Token    string `json:"token,omitempty"`
	Sessions int    `json:"sessions"`

	// Password 是当前设的控制密码,**明文**,空表示没设。
	//
	// 状态里带一个秘密看着刺眼,但这里必须给:控制页要把它显示出来、
	// 让主机复制给对方(存哈希就没法显示了,这是当初选明文的理由)。
	// 而这个接口只绑回环(见 internal/server/server.go 顶部),不出本机。
	//
	// 放在 Status 而不是让页面去读配置快照:控制页手里的 config 是载入时
	// 的那一份,而 Status 每个远控接口都会返回最新值,不存在"页面显示的是
	// 旧密码、复制出去却对不上"的窗口。
	Password string `json:"password,omitempty"`

	// Clipboard 报告剪贴板同步是不是开着。
	Clipboard bool `json:"clipboard"`

	// Controller 是当前控制者的标识,没人控制时为空。
	Controller string   `json:"controller,omitempty"`
	Pending    *Pending `json:"pending,omitempty"`

	// Elevated 为真表示前台窗口以管理员身份运行,此时注入会被系统
	// 静默丢弃 —— 界面必须把这件事说出来,否则观众只会觉得"卡了"。
	Elevated bool `json:"elevated,omitempty"`

	// LastError 是最近一次失败的原因(端口占用、采集源不支持等)。
	LastError string `json:"lastError,omitempty"`
}

// Options 是构造 Server 需要的东西。
type Options struct {
	// CaptureRect 返回当前采集源覆盖的屏幕区域。第二项为 false 表示
	// 这个源不能远程控制(窗口会移动、OBS 是合成画面),此时开启会被拒绝。
	// 传 nil 表示"总是不可用"。
	CaptureRect func() (screen.Rect, bool)

	// WebRTCPort 和 StreamPath 用来给观看页拼 WHEP 地址。
	WebRTCPort int
	StreamPath string

	// Rotate 返回观众需要把画面转回来多少度(0 或 180)。
	//
	// 采集端补不了:ddagrab 那条路是零拷贝,插一个 CPU 翻转实测比 gdigrab
	// 还慢(3.66s vs 3.06s CPU/5 秒),而 GPU 翻转这个 ffmpeg 版本不支持。
	// 所以放在观众那边转 —— 浏览器做一次 CSS 旋转不花任何代价,主机侧的
	// 零拷贝原样保留。代价是流本身是倒的(录下来、用别的播放器看都是倒的)。
	//
	// **是个函数而不是一个值**:采集源可以在控制页当场改,而"要不要转"
	// 正是由采集源决定的(ddagrab 要、gdigrab 不要)。写成固定值的话,用户
	// 从 GPU 采集切回兼容模式之后画面会一直倒着,而且看不出为什么 ——
	// 和 CaptureRect 是同一个理由。
	Rotate func() int

	// LANHosts 是本机在局域网里的地址(路由表推出来的那个排第一)。
	//
	// 观看页只用它做一件事:发现自己是从公网地址打开的、而其中某个地址
	// 又够得着,就把整页换过去重开。同网段走公网会绕一圈互联网,实测延迟
	// 差 2–3 倍 —— 而这件事让观众自己判断是不可能的,他不知道自己"其实在
	// 同一个网里"。
	LANHosts []string

	// Inject 是注入实现。nil 表示用真实实现(测试里换成假的)。
	Inject Injector

	// Clipboard 是剪贴板同步实现。nil 表示这一版没有这个能力 ——
	// 控制页上的剪贴板开关不会出现。
	Clipboard *clipboard.Sync

	// Sharing 报告主机此刻在不在推流。nil 视为不在。
	//
	// 做成函数而不是一个值:它每次问都要拿到当下的事实,而共享的开关
	// 随时会被控制页或托盘改掉。
	Sharing func() bool

	// OnApproveOpen 落地一次"开启"请求:开始共享 + 开启远控,返回实际的
	// 结果。
	//
	// 这两个动作都在 server 层(推流管理器、远控配置),本包够不着,所以
	// 反过来注入。nil 表示这一版不支持 —— 那样批准会失败,而不是静悄悄
	// 什么都不做。
	//
	// **会阻塞**(里面要绑端口、写配置),调用方必须放到独立协程里跑,
	// 不能压在观众连接那条读协程上。
	OnApproveOpen func() OpenResult

	// OnChange 在状态发生变化时调用(有人连上来、有人申请、控制权易主)。
	// 用来刷新托盘提示 —— 控制页那边靠 2 秒轮询,不需要这个。
	OnChange func(Status)
}

// Server 是远控服务。
type Server struct {
	opts   Options
	inject Injector

	control control

	// clipboard 是同步实现(来自 Options,一旦有就不会变),clip 是
	// "现在要不要同步"。两者分开是因为开关随时可改,而实现不是。
	clipboard *clipboard.Sync
	clip      *clipboard.Sync

	mu sync.Mutex
	// enabled 和 reqOn 是两个独立开关,但只共用一个 listener ——
	// 端口在监听的充要条件是两者之一为真,见包注释。
	enabled  bool
	reqOn    bool
	token    string
	password string
	port     int
	listener net.Listener
	httpSrv  *http.Server
	sessions map[string]*session
	seq      int
	lastErr  string
	closed   bool

	// loopsOnce 保证两条常驻协程只起一次。
	//
	// 不能挂在"开启"上:端口现在可以被请求入口单独打开、关掉、再打开,
	// 那样每来一轮就多两条协程,而旧的那些还在 tick。
	loopsOnce sync.Once

	// openMu 串行化"开启"的落地动作。
	//
	// 两条路会走到它:主机点允许(弹窗那条协程),以及填对密码的请求
	// (观众连接的读协程再起一条协程去执行)。两个人几乎同时发起就会
	// 并发地开同一个东西,而绑端口、启动推流都不是可重入的。
	openMu sync.Mutex
}

func New(o Options) *Server {
	inject := o.Inject
	if inject == nil {
		inject = osInjector{}
	}
	return &Server{
		opts:      o,
		inject:    inject,
		clipboard: o.Clipboard,
		sessions:  map[string]*session{},
	}
}

// SetEnabled 开/关远控授权。开启会确保端口在监听。
//
// 关掉它**不一定会关掉端口**:请求入口还开着的时候端口要继续留着,否则
// 对方就再也没法请求把它开回来了(见包注释)。要停掉对外监听请关请求入口。
func (s *Server) SetEnabled(on bool) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("远控服务已关闭")
	}
	if on == s.enabled {
		// 状态没变。重复开启时把上次的失败原因再报一次 —— 控制页的
		// 开关是幂等的,用户重复点"开启"时应该看到同一个原因,而不是
		// 一个说不清的 nil。
		last := s.lastErr
		s.mu.Unlock()
		if !on || last == "" {
			return nil
		}
		return errors.New(last)
	}

	if !on {
		s.enabled = false
		s.lastErr = ""
		ln, srv := s.takeListenerLocked()
		s.mu.Unlock()

		s.control.clear()
		s.dropAll()
		s.stopListening(ln, srv)
		s.notify()
		return nil
	}

	// 开启前先确认这个采集源支持远控 —— 不支持就根本不该授权。
	if _, ok := s.captureRect(); !ok {
		s.lastErr = "当前采集源不支持远程控制(只有整屏采集能用)"
		err := errors.New(s.lastErr)
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	// 端口通常已经为请求入口绑着了,这一句就是空操作 —— 那种情况下
	// **绝不能**再 Listen 一次,同一个端口绑两次必然失败。
	if err := s.ensureListening(); err != nil {
		return s.failEnable(err)
	}

	s.mu.Lock()
	if s.closed || s.enabled {
		// 期间状态变了,放弃这次开启
		s.mu.Unlock()
		return nil
	}
	s.enabled = true
	s.lastErr = ""
	port := s.port
	s.mu.Unlock()

	log.Printf("远程控制已开启,端口 0.0.0.0:%d", port)
	s.notify()
	return nil
}

// SetRequestEntry 开/关常驻的"请求入口"。
//
// 开着的时候远控端口一直监听,哪怕远控本身没开 —— 远端的链接永远敲得到
// 门。这是整个"请求开启"链路的立足点,默认值是开(代价写在 config 那边)。
//
// 和 SetEnabled 一样,**失败不回退这个开关**,只记 lastErr。理由是同一个:
// 配置已经写下去了,而"配置说开着、实际没监听"是 Status.Listening 能如实
// 表达出来的状态,下次开启还会重试。
func (s *Server) SetRequestEntry(on bool) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("远控服务已关闭")
	}
	if on == s.reqOn {
		last := s.lastErr
		s.mu.Unlock()
		if !on || last == "" {
			return nil
		}
		return errors.New(last)
	}

	if !on {
		s.reqOn = false
		ln, srv := s.takeListenerLocked()
		s.mu.Unlock()

		s.dropAll()
		s.stopListening(ln, srv)
		s.notify()
		return nil
	}

	s.reqOn = true
	port := s.port
	s.mu.Unlock()

	if err := s.ensureListening(); err != nil {
		return s.failEnable(err)
	}

	log.Printf("请求入口已开启,远控端口 0.0.0.0:%d 保持监听", port)
	s.notify()
	return nil
}

// wantListenLocked 报告端口此刻**应该**在监听。调用方须持有 s.mu。
func (s *Server) wantListenLocked() bool {
	return s.enabled || s.reqOn
}

// takeListenerLocked 在确实该停止监听时把监听器摘下来交给调用方去关;
// 还需要监听时返回零值,调用方拿到 nil 就什么都不用做。
//
// 调用方须持有 s.mu,而且**必须在解锁之后再调 stopListening** —— 那里
// 会等最多 2 秒的在途请求,攥着锁等等于把控制页的轮询一起堵住。
func (s *Server) takeListenerLocked() (net.Listener, *http.Server) {
	if s.wantListenLocked() {
		return nil, nil
	}
	ln, srv := s.listener, s.httpSrv
	s.listener, s.httpSrv = nil, nil
	return ln, srv
}

// ensureListening 确保端口在监听;已经绑着就是空操作。
//
// 幂等是必需的,不是优化:请求入口把端口绑上之后,再点"开启远控"走到的
// 就是这里,而同一个端口绑第二次必然报"地址已被占用"。
//
// 绑定放在锁外面:它可能耗时(防火墙拦截、端口被占),不该卡住正在轮询
// /api/status 的控制页。
func (s *Server) ensureListening() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("远控服务已关闭")
	}
	if s.listener != nil || !s.wantListenLocked() {
		s.mu.Unlock()
		return nil
	}
	port := s.port
	s.mu.Unlock()

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	s.mu.Lock()
	if s.closed || s.listener != nil || !s.wantListenLocked() {
		// 期间状态变了(关掉了,或者别人已经绑上了),放弃这一次
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.listener = ln
	s.httpSrv = srv
	s.lastErr = ""
	s.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("远控服务停止: %v", err)
		}
	}()
	s.startLoops()

	return nil
}

// startLoops 保证过期检查和剪贴板轮询这两条协程活着。
//
// 在第一次真正绑上监听时起,之后一直活着:端口可以被请求入口单独开、关、
// 再开,而循环体里靠 continue 跳过"这一轮不该干活"的 tick(见各自注释),
// 所以不需要跟着端口反复起停。
func (s *Server) startLoops() {
	s.loopsOnce.Do(func() {
		go s.expireLoop()
		go s.clipboardLoop()
	})
}

// loopState 一次拿够两条循环要判断的状态。
//
// 合成一个方法是为了每 tick 只拿一次锁 —— 分开判断的话,几次判断之间
// 状态还会变,算出来的是一个自相矛盾的时刻。
func (s *Server) loopState() (listening, enabled, closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listener != nil, s.enabled, s.closed
}

// failEnable 记录并返回"开启失败"的原因。
func (s *Server) failEnable(err error) error {
	s.mu.Lock()
	// 端口被占用是最常见的失败,单独说清楚 —— 通用的 "address already
	// in use" 对用户没有任何指导意义。
	if isAddrInUse(err) {
		// 措辞不写"开启远程控制":同一个端口现在也可能是被请求入口
		// 占着绑不上,那句话会让用户去找一个不存在的"远控开关问题"。
		s.lastErr = fmt.Sprintf("端口 %d 已被占用,远控端口无法监听", s.port)
	} else {
		s.lastErr = err.Error()
	}
	msg := s.lastErr
	s.mu.Unlock()

	s.notify()
	return errors.New(msg)
}

// wsaEADDRINUSE 是 Windows 的"地址已被占用"(WSAEADDRINUSE)。
//
// 为什么要自己写这个数字,而不是用现成的常量:
//
//   - `syscall.EADDRINUSE` 在 Windows 上**对不上**。它是 Go 自己编的一个
//     合成值(实测 536870914),而系统返回的是 10048。实测
//     `errors.Is(err, syscall.EADDRINUSE)` 恒为 false,照着它写就永远
//     识别不出端口被占。
//   - `windows.WSAEADDRINUSE` 只在 Windows 上编译,而本文件要跨平台编译。
//
// 写死数字在所有平台上都编得过,在非 Windows 上只是永远不命中。
const wsaEADDRINUSE = syscall.Errno(10048)

func isAddrInUse(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		err = opErr.Err
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == wsaEADDRINUSE || errno == syscall.EADDRINUSE
}

// Configure 更新端口、令牌、控制密码和剪贴板开关。
//
// 端口变了要在下次 SetEnabled(true) 时才生效;已经开着的话调用方应当
// 先关再开(控制页那边就是这么做的),否则用户会以为改完立刻就换了端口。
//
// 剪贴板开关就不同:它是每次收发时现读的,改完立刻生效,不需要重开。
//
// 密码也是现读的,并且**只影响下一次申请**:改完之后,已经在控制的那个人
// 照旧控制着,不会被踢掉。改密码不该中断正在进行的一次协助 —— 想立刻
// 赶人就按"收回控制权",那是另一件事、另一个按钮。
//
// 这四个参数挤在一个调用里而不是拆成几个 setter:可变设置全都从这一条路
// 推下去,拆开之后迟早会有某条路(托盘、控制页、重置凭据)漏掉其中一个。
// 四种设置里漏一种是查不出来的 —— 界面会一直显示旧值。
func (s *Server) Configure(port int, token, password string, allowClipboard bool) {
	s.mu.Lock()
	s.port = port
	s.token = token
	s.password = password
	s.clip = nil
	if allowClipboard {
		s.clip = s.clipboard
	}
	s.mu.Unlock()
}

// clipSync 返回当前启用的剪贴板同步;没开则返回 nil。
func (s *Server) clipSync() *clipboard.Sync {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clip
}

func (s *Server) currentToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// isEnabled 报告远控授权此刻开着没有。
//
// 注意它和"端口在不在监听"是两件事(见包注释):请求入口开着而远控没开的
// 时候,监听为真、这个为假。要判断"能不能注入输入",认的是这个。
func (s *Server) isEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

// sharing 报告主机此刻在不在推流。
//
// 必须在**不持 s.mu** 的时候调:回调是外部传进来的,它可能去碰别的锁
// (推流管理器那边),两边谁的顺序不一致就是死锁。
func (s *Server) sharing() bool {
	if s.opts.Sharing == nil {
		return false
	}
	return s.opts.Sharing()
}

// currentPassword 返回当前的控制密码;没设则返回空串。
//
// 空串在调用方是一个有意义的信号("没设密码,走原来的批准流程"),
// 见 handleCtl。
func (s *Server) currentPassword() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.password
}

func (s *Server) captureRect() (screen.Rect, bool) {
	if s.opts.CaptureRect == nil {
		return screen.Rect{}, false
	}
	return s.opts.CaptureRect()
}

// Status 返回当前状态快照。
func (s *Server) Status() Status {
	s.mu.Lock()
	st := Status{
		Enabled:      s.enabled,
		RequestEntry: s.reqOn,
		Listening:    s.listener != nil,
		Port:         s.port,
		Token:        s.token,
		Sessions:     len(s.sessions),
		Clipboard:    s.clip != nil,
		Password:     s.password,
		LastError:    s.lastErr,
	}
	s.mu.Unlock()

	st.Sharing = s.sharing()

	st.Controller, st.Pending = s.control.snapshot()
	if st.Enabled {
		st.Elevated = s.inject.ElevatedForeground()
	}
	return st
}

// Approve 批准一份待批的申请。
//
// 两种申请走同一个入口(它们共用一份待批名额),由 kind 决定"批准"到底
// 意味着什么:控制申请就是把控制权交给他;开启请求还要先把共享和远控真正
// 打开 —— 那件事在 server 层,通过 Options.OnApproveOpen 回调进来。
//
// 主机点的那一下对两者都是**知情同意**:弹窗上的文案必须把"同意之后会
// 发生什么"说清楚(见 notify.Describe),因为这里不再有第二次确认。
func (s *Server) Approve(id string) bool {
	kind, ok := s.control.approve(id)
	if !ok {
		return false
	}
	sess := s.session(id)

	if kind == pendingOpen {
		if sess != nil {
			log.Printf("远程控制:已批准 %s 的开启请求", sess.label)
			s.openFor(sess)
		}
		s.notify()
		return true
	}

	if sess != nil {
		s.grant(sess)
		log.Printf("远程控制:已批准 %s", sess.label)
	}
	s.notify()
	return true
}

// grant 告诉观众"控制权是你的了",并把主机剪贴板**当前**的内容补送一份。
//
// 批准(主机点"允许")和密码通道(填对了密码)都走这里。两处各写一遍会
// 分叉出一个很难发现的 bug:走密码那条路的控制者拿不到下面这段剪贴板,
// 表现为"同样的事情,有时候剪贴板能同步有时候不能"。
//
// 日志留给调用方 —— 两条路该记的话不一样("已批准"是人点的,"密码通过"
// 不是),而这里只负责把状态发出去。
func (s *Server) grant(sess *session) {
	sess.write(serverMsg{Type: msgCtl, State: stateGranted})

	// 补送剪贴板的原因:轮询只在内容变化时通知,而"复制好东西再连过来"
	// 是最常见的用法 —— 那段内容的变化发生在拿到控制权之前,不补这一下,
	// 控制者就永远等不到它。
	if clip := s.clipSync(); clip != nil {
		if text := clip.Current(); text != "" {
			sess.write(serverMsg{Type: msgClip, S: text})
		}
	}
}

// Deny 拒绝一份待批的申请。措辞按申请种类分 —— "拒绝了这次申请"和
// "拒绝了这次请求"在页面上落在两个不同的状态机上。
func (s *Server) Deny(id string) bool {
	kind, ok := s.control.deny(id)
	if !ok {
		return false
	}
	if sess := s.session(id); sess != nil {
		if kind == pendingOpen {
			sess.write(serverMsg{Type: msgOpen, State: openDenied, Reason: "主机拒绝了这次请求"})
		} else {
			sess.write(serverMsg{Type: msgCtl, State: stateDenied, Reason: "主机拒绝了这次申请"})
		}
	}
	s.notify()
	return true
}

// Revoke 收回控制权。没人在控制时是空操作 —— 它必须永远能按。
func (s *Server) Revoke() {
	id, ok := s.control.revoke()
	if !ok {
		return
	}
	if sess := s.session(id); sess != nil {
		sess.write(serverMsg{Type: msgCtl, State: stateRevoked, Reason: "主机收回了控制权"})
		sess.drain()
	}
	log.Printf("远程控制:控制权已被收回")
	s.notify()
}

// DisconnectAll 踢掉所有连接,但保持监听。用于切换开关时的清理。
func (s *Server) DisconnectAll() {
	s.control.clear()
	s.dropAll()
	s.notify()
}

// Close 停止服务。之后不能再开启。
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.enabled = false
	s.listener, s.httpSrv = nil, nil
	s.mu.Unlock()

	s.control.clear()
	s.dropAll()
	s.notify()
}

// notify 在状态变化时回调。
func (s *Server) notify() {
	if s.opts.OnChange != nil {
		s.opts.OnChange(s.Status())
	}
}

// ---------- 会话管理 ----------

func (s *Server) session(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// addSession 登记一个新连接。端口没在监听时拒绝。
//
// 判据是"有没有监听",不是"远控开没开":请求入口开着而远控没开的时候,
// 对方要能连上来说一句"请把共享和远控开一下"—— 那是这个功能的入口。
func (s *Server) addSession(sess *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener == nil || s.closed {
		return false
	}
	s.sessions[sess.id] = sess
	return true
}

func (s *Server) removeSession(sess *session) {
	s.mu.Lock()
	delete(s.sessions, sess.id)
	s.mu.Unlock()

	wasOwner := s.control.drop(sess.id)
	if wasOwner {
		log.Printf("远程控制:%s 断开,控制权释放", sess.label)
	}
	s.notify()
}

// dropAll 断开所有连接。调用方负责先清控制权状态。
func (s *Server) dropAll() {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessions = map[string]*session{}
	s.mu.Unlock()

	for _, sess := range sessions {
		sess.close()
		_ = sess.conn.Close(websocket.StatusGoingAway, "远程控制已关闭")
	}
}

// dropSlow 断开发送速度跟不上的连接。
func (s *Server) dropSlow(sess *session) {
	if sess == nil {
		return
	}
	sess.write(serverMsg{Type: "err", Code: errRate, Msg: "主机来不及处理这些输入"})
	sess.close()
	_ = sess.conn.Close(websocket.StatusPolicyViolation, "输入过快")
}

// expireLoop 定期清理等太久的控制申请。
func (s *Server) expireLoop() {
	t := time.NewTicker(expireInterval)
	defer t.Stop()

	for range t.C {
		listening, _, closed := s.loopState()
		if closed {
			return
		}
		// 这里必须是 continue 不能 return:请求入口让端口可以早于远控开启,
		// 而"远控关着"现在是常态而非终点。一旦 return,这条协程就没了 ——
		// 表现为远控关过一次之后,新来的开启请求永远不过期,一直挂在
		// 主机的弹窗和控制页上。
		if !listening {
			continue
		}
		if id, kind, ok := s.control.expire(time.Now()); ok {
			s.expireSession(id, kind)
			s.notify()
		}
	}
}

// expireSession 告诉等太久的那个观众:没人应答。
//
// 文案按申请的种类分:控制申请作废是"主机没有应答",开启请求超时则要
// 说明"可以再试一次"—— 后者没被拒绝,只是主机不在。
func (s *Server) expireSession(id, kind string) {
	sess := s.session(id)
	if sess == nil {
		return
	}
	if kind == pendingOpen {
		sess.write(serverMsg{Type: msgOpen, State: openDenied, Reason: "主机没有应答,可以再试一次"})
		return
	}
	sess.write(serverMsg{Type: msgCtl, State: stateDenied, Reason: "主机没有应答"})
}

// clipboardLoop 轮询本机剪贴板,变了就推给正在控制的那个人。
//
// 只推给控制者,不推给纯观看的人:剪贴板里经常躺着密码、验证码这类
// 东西,而"能看画面"和"能拿到你的剪贴板"是两件事,不该捆绑。
func (s *Server) clipboardLoop() {
	t := time.NewTicker(clipboardInterval)
	defer t.Stop()

	for range t.C {
		// 同上:远控关着时这一轮跳过,但协程要活着 —— 关掉再开回来是很
		// 正常的用法,协程跟着端口起停只会多出一堆生命周期要管。
		_, enabled, closed := s.loopState()
		if closed {
			return
		}
		if !enabled {
			continue
		}

		clip := s.clipSync()
		if clip == nil {
			continue
		}
		text, changed := clip.Read()
		if !changed {
			continue
		}

		if sess := s.session(s.control.holder()); sess != nil {
			sess.write(serverMsg{Type: msgClip, S: text})
		}
	}
}

// stopListening 关掉监听和所有连接。
func (s *Server) stopListening(ln net.Listener, srv *http.Server) {
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	if ln != nil {
		_ = ln.Close()
	}
}

// ---------- HTTP ----------

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	// 静态资源无条件提供。页面本身没有任何秘密 —— 令牌在 URL 的 #
	// 片段里,由页面脚本自己取,不会发到服务器。
	//
	// 这和视频链路是同一个取舍:观看本来就不鉴权(链接即凭据)。真正的
	// 门槛在 WebSocket 那一侧的令牌,以及主机的当场批准。
	static, _ := fs.Sub(webui.Files, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/rc/config", s.handleConfig)
	mux.HandleFunc("/ws", s.handleWS)

	return mux
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/rc" {
		http.NotFound(w, r)
		return
	}
	// rc.html 只在远控页用,和控制页的 index.html 是两个页面。
	data, err := fs.ReadFile(webui.Files, "static/rc.html")
	if err != nil {
		http.Error(w, "远控页资源缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleConfig 给观看页提供它自己拼不出来的东西:WHEP 地址和本机的局域网地址。
//
// 端口用的是配置里的 WebRTCPort,主机名由页面那边填 location.hostname ——
// 观众是从哪个地址连上来的,就按哪个地址去找流。
//
// lan 那一条是例外:**那是页面猜不出来的**。页面只知道自己是被人用哪个
// 地址打开的,不知道自己是不是就坐在同一个局域网里 —— 而这正是"同网段
// 却绕公网"那个坑的成因,见 rc.js 的 preferLAN。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	port := s.opts.WebRTCPort
	path := s.opts.StreamPath
	lan := s.opts.LANHosts
	rotate := s.opts.Rotate
	s.mu.Unlock()

	// 在锁外面调:它要去读当前的采集配置(见 Rotate 的说明)
	deg := 0
	if rotate != nil {
		deg = rotate()
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"whepPort": port,
		"path":     path,
		"lan":      lan,
		"rotate":   deg,
		// 这两个和 hello 里的是同一份信息,给的是**首屏**:页面在
		// WebSocket 连上之前就要决定画"请求开启"还是画播放器,而那时候
		// 还没有 hello。两处必须一致,都是现读的。
		"remote":  s.isEnabled(),
		"sharing": s.sharing(),
	})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !tokenMatch(s.currentToken(), tokenFromRequest(r)) {
		http.Error(w, "凭据无效", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 不校验 Origin。
		//
		// 跨源校验防的是"别的网页冒用你浏览器里的身份" —— 那要求存在
		// 一个浏览器会自动带上的凭据(cookie)。这里没有:令牌是页面
		// 自己放在 URL 里显式传来的,别的网页拿不到,也就冒用不了。
		// 而开这个校验反而会误伤:观众从域名和从局域网 IP 连上来时
		// Origin 与 Host 不一致,合法的连接会被拒掉。
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}

	label := remoteLabel(r)
	sess := newSession(s.newSessionID(), label, conn, s)
	if !s.addSession(sess) {
		_ = conn.Close(websocket.StatusGoingAway, "远控端口未监听")
		return
	}
	defer s.removeSession(sess)
	defer sess.close()

	go sess.injectLoop()
	go s.pingLoop(sess)

	// hello 里带上远控和共享的**当下**状态:页面靠它决定自己该显示
	// "请求开启"还是"申请控制"。不能让它用自己缓存的上一次状态 ——
	// 观众可能在主机点完允许之后才刷新页面。
	sess.write(serverMsg{
		Type:        "hello",
		You:         sess.id,
		Clipboard:   s.clipSync() != nil,
		PasswordSet: s.currentPassword() != "",
		Remote:      s.isEnabled(),
		Sharing:     s.sharing(),
	})

	log.Printf("远程控制:观众 %s 已连接(%s)", sess.id, label)
	s.notify()

	s.readPump(sess)
}

// newSessionID 生成一个短会话号。
//
// 不用全局计数器:那个数会暴露"这台机器上曾经连过多少人",而且多开
// 几个实例时会重复。用序号加时间戳已经足够区分,它只是个界面上的
// 标识,不是凭据。
func (s *Server) newSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return "s" + strconv.Itoa(s.seq)
}

// readPump 读取并处理观众端发来的消息,直到连接断开。
func (s *Server) readPump(sess *session) {
	sess.conn.SetReadLimit(maxTextBytes + 4096)

	windowAt := time.Now()
	count := 0

	for {
		_, data, err := sess.conn.Read(context.Background())
		if err != nil {
			return
		}

		count++
		if time.Since(windowAt) >= rateWindow {
			windowAt, count = time.Now(), 0
		} else if count > rateLimit {
			log.Printf("远程控制:%s 输入过于频繁,断开", sess.label)
			s.dropSlow(sess)
			return
		}

		var m clientMsg
		if err := json.Unmarshal(data, &m); err != nil {
			sess.write(serverMsg{Type: "err", Code: errBadMsg, Msg: "看不懂的消息"})
			continue
		}
		s.dispatch(sess, m)
	}
}

// pingLoop 定期探活,并在对方失联时关掉连接。
func (s *Server) pingLoop(sess *session) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()

	for {
		select {
		case <-sess.done:
			return
		case <-t.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
		err := sess.conn.Ping(ctx)
		cancel()
		if err != nil {
			// 对方已经不在了。主动关掉,让 removeSession 释放控制权 ——
			// 否则界面会一直显示"有人正在控制"。
			_ = sess.conn.Close(websocket.StatusPolicyViolation, "心跳超时")
			return
		}
	}
}

// remoteLabel 取对方地址,只留 IP(带端口对主机判断没有帮助)。
func remoteLabel(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// drain 丢掉队列里积压的事件。控制权被收回时调用 —— 那些事件是在
// 夺权之前排进来的,再放出去就等于没夺成。
func (s *session) drain() {
	s.inMu.Lock()
	s.queue = s.queue[:0]
	s.inMu.Unlock()
}
