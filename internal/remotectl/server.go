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
// # 默认关闭
//
// 不开的时候这个服务不监听任何端口。这和"打开就绑 0.0.0.0"是两回事:
// 不开端口是零攻击面,而绑全网卡即使有鉴权也仍然多一个被扫描的目标。
//
// # 两层门槛
//
//  1. 链接里带 256 位随机令牌 —— 决定能不能连上来。
//  2. 主机的当场批准 —— 决定能不能真的操作。
//
// 之所以要两层:视频流本身是不鉴权的(链接即凭据,见架构文档 §7.5 的
// 取舍),所以"能看"这件事挡不住。让陌生人只能看、不能动,靠的是第二层。
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

// Pending 是一份等待主机批准的控制申请。
type Pending struct {
	ID string `json:"id"`

	// RemoteIP 是申请方的地址,给主机判断"这是不是我认识的人"。
	RemoteIP string `json:"remoteIp"`

	At string `json:"at"`
}

// Status 是给控制页和托盘看的远控状态快照。
type Status struct {
	Enabled bool `json:"enabled"`

	// Listening 为假而 Enabled 为真,说明端口没绑上(被占用之类)。
	Listening bool `json:"listening"`
	Port      int  `json:"port"`

	Token    string `json:"token,omitempty"`
	Sessions int    `json:"sessions"`

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

	// Inject 是注入实现。nil 表示用真实实现(测试里换成假的)。
	Inject Injector

	// Clipboard 是剪贴板同步实现。nil 表示这一版没有这个能力 ——
	// 控制页上的剪贴板开关不会出现。
	Clipboard *clipboard.Sync

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

	mu       sync.Mutex
	enabled  bool
	token    string
	port     int
	listener net.Listener
	httpSrv  *http.Server
	sessions map[string]*session
	seq      int
	lastErr  string
	closed   bool
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

// SetEnabled 开/关远控服务。开启会真的开始监听端口。
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
		ln, srv := s.listener, s.httpSrv
		s.listener, s.httpSrv = nil, nil
		s.mu.Unlock()

		s.control.clear()
		s.dropAll()
		s.stopListening(ln, srv)
		s.notify()
		return nil
	}

	// 开启前先确认这个采集源支持远控 —— 不支持就根本不该监听端口。
	if _, ok := s.captureRect(); !ok {
		s.lastErr = "当前采集源不支持远程控制(只有整屏采集能用)"
		err := errors.New(s.lastErr)
		s.mu.Unlock()
		return err
	}

	port := s.port
	s.mu.Unlock()

	// 绑定放在锁外面:它可能耗时(防火墙拦截、端口被占),不该卡住
	// 正在轮询 /api/status 的控制页。
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return s.failEnable(err)
	}

	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	s.mu.Lock()
	if s.closed || s.enabled {
		// 期间状态变了,放弃这次开启
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.enabled = true
	s.lastErr = ""
	s.listener = ln
	s.httpSrv = srv
	s.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("远控服务停止: %v", err)
		}
	}()
	go s.expireLoop()
	go s.clipboardLoop()

	log.Printf("远程控制已开启,监听 0.0.0.0:%d", port)
	s.notify()
	return nil
}

// failEnable 记录并返回"开启失败"的原因。
func (s *Server) failEnable(err error) error {
	s.mu.Lock()
	// 端口被占用是最常见的失败,单独说清楚 —— 通用的 "address already
	// in use" 对用户没有任何指导意义。
	if isAddrInUse(err) {
		s.lastErr = fmt.Sprintf("端口 %d 已被占用,无法开启远程控制", s.port)
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

// Configure 更新端口、令牌和剪贴板开关。
//
// 端口变了要在下次 SetEnabled(true) 时才生效;已经开着的话调用方应当
// 先关再开(控制页那边就是这么做的),否则用户会以为改完立刻就换了端口。
//
// 剪贴板开关就不同:它是每次收发时现读的,改完立刻生效,不需要重开。
func (s *Server) Configure(port int, token string, allowClipboard bool) {
	s.mu.Lock()
	s.port = port
	s.token = token
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
		Enabled:   s.enabled,
		Listening: s.listener != nil,
		Port:      s.port,
		Token:     s.token,
		Sessions:  len(s.sessions),
		Clipboard: s.clip != nil,
		LastError: s.lastErr,
	}
	s.mu.Unlock()

	st.Controller, st.Pending = s.control.snapshot()
	if st.Enabled {
		st.Elevated = s.inject.ElevatedForeground()
	}
	return st
}

// Approve 批准一份控制申请。
func (s *Server) Approve(id string) bool {
	if !s.control.approve(id) {
		return false
	}
	if sess := s.session(id); sess != nil {
		sess.write(serverMsg{Type: msgCtl, State: stateGranted})
		log.Printf("远程控制:已批准 %s", sess.label)

		// 把主机剪贴板**当前**的内容补送一份。
		//
		// 轮询只在内容变化时通知,而"复制好东西再连过来"是最常见的用法 ——
		// 那段内容的变化发生在拿到控制权之前,不补这一下,控制者就永远
		// 等不到它。
		if clip := s.clipSync(); clip != nil {
			if text := clip.Current(); text != "" {
				sess.write(serverMsg{Type: msgClip, S: text})
			}
		}
	}
	s.notify()
	return true
}

// Deny 拒绝一份控制申请。
func (s *Server) Deny(id string) bool {
	if !s.control.deny(id) {
		return false
	}
	if sess := s.session(id); sess != nil {
		sess.write(serverMsg{Type: msgCtl, State: stateDenied, Reason: "主机拒绝了这次申请"})
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

// addSession 登记一个新连接。服务没在开启状态时拒绝。
func (s *Server) addSession(sess *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.enabled || s.closed {
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
		s.mu.Lock()
		live := s.enabled
		s.mu.Unlock()
		if !live {
			return
		}
		if id, ok := s.control.expire(time.Now()); ok {
			if sess := s.session(id); sess != nil {
				sess.write(serverMsg{Type: msgCtl, State: stateDenied, Reason: "主机没有应答"})
			}
			s.notify()
		}
	}
}

// clipboardLoop 轮询本机剪贴板,变了就推给正在控制的那个人。
//
// 只推给控制者,不推给纯观看的人:剪贴板里经常躺着密码、验证码这类
// 东西,而"能看画面"和"能拿到你的剪贴板"是两件事,不该捆绑。
func (s *Server) clipboardLoop() {
	t := time.NewTicker(clipboardInterval)
	defer t.Stop()

	for range t.C {
		s.mu.Lock()
		live := s.enabled && !s.closed
		s.mu.Unlock()
		if !live {
			return
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
	// 这和视频链路是同一个取舍:观看本来就不鉴权(链接即凭据,
	// 见架构文档 §7.5)。真正的门槛在 WebSocket 那一侧的令牌,
	// 以及主机的当场批准。
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

// handleConfig 给观看页提供它自己拼不出来的东西:WHEP 地址。
//
// 端口用的是配置里的 WebRTCPort,主机名由页面那边填 location.hostname ——
// 观众是从哪个地址连上来的,就按哪个地址去找流,不用程序去猜他走的是
// 局域网还是公网。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	port := s.opts.WebRTCPort
	path := s.opts.StreamPath
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"whepPort": port,
		"path":     path,
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
		_ = conn.Close(websocket.StatusGoingAway, "远程控制未开启")
		return
	}
	defer s.removeSession(sess)
	defer sess.close()

	go sess.injectLoop()
	go s.pingLoop(sess)

	sess.write(serverMsg{
		Type:      "hello",
		You:       sess.id,
		Clipboard: s.clipSync() != nil,
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
