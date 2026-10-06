// Package server 提供控制页的 HTTP 接口。
//
// 服务只绑定 127.0.0.1 —— 控制界面是本机自己用的,没有任何理由对外监听,
// 绑回环还能避免 Windows 首次运行弹防火墙授权框。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"sharescreen/internal/config"
	"sharescreen/internal/ffmpeg"
	"sharescreen/internal/mediamtx"
	"sharescreen/internal/portmap"
	"sharescreen/internal/remotectl"
	"sharescreen/internal/stream"
	"sharescreen/internal/webui"
)

// PortMapper 是控制页需要的自动端口映射能力。
//
// 定成接口而不是直接用 *portmap.Mapper,是为了让 server 的测试不必碰真实网络 ——
// 这个包里的东西都围绕 HTTP 处理器,不该被 UPnP 拖下水。
type PortMapper interface {
	Snapshot() portmap.Snapshot
	SetEnabled(on bool)
	Configure(rules []portmap.Rule, internalIP string)
	Retry()
}

// RemoteController 是控制页需要的远程控制能力。
//
// 和 PortMapper 一样定成接口:这个包围绕 HTTP 处理器,不该被输入注入、
// WebSocket 那些东西拖下水;定成接口之后,这里的测试也不必真起一个
// 监听端口的服务。
type RemoteController interface {
	SetEnabled(on bool) error
	Configure(port int, token string, allowClipboard bool)
	Status() remotectl.Status
	Approve(id string) bool
	Deny(id string) bool
	Revoke()
	DisconnectAll()
}

// Server 持有控制页需要的全部依赖和可变状态。
type Server struct {
	stream   *stream.Manager
	runner   *mediamtx.Runner
	mtx      *mediamtx.Client
	encoders []ffmpeg.Encoder
	presets  []config.Resolution
	lanIP    string
	cfgPath  string
	onTopo   func(config.Config) error
	portMap  PortMapper
	remote   RemoteController

	// lanIPs 是探测到的局域网候选,第一条是"自动"会选的那个。
	// 只用来填下拉,不参与任何判断。
	lanIPs []AddressCandidate

	// pubIP 为 nil 表示不做外部探测(测试里就是这个)。
	pubIP PublicIPProbe

	// 外部探测的结果,由轮询写、由请求读。
	echoMu     sync.RWMutex
	echoIP     string
	echoSource string
	// echoFailed 挡住"每 10 分钟刷一条一样的失败日志"。
	echoFailed atomic.Bool

	cfgMu sync.RWMutex
	cfg   config.Config

	// 控制页存活状态,见 alive.go。
	onControlGone func() bool
	aliveMu       sync.Mutex
	aliveConns    int  // 当前开着的控制页数量
	aliveSeen     bool // 是否曾经有控制页连上来过
}

type Options struct {
	Stream     *stream.Manager
	Runner     *mediamtx.Runner
	MTX        *mediamtx.Client
	Encoders   []ffmpeg.Encoder
	Presets    []config.Resolution
	LANIP      string
	ConfigPath string
	Config     config.Config

	// LANIPs 是探测到的局域网候选,第一条是"自动"会选的那个。
	// 来自 main 里的网卡扫描,只用来填控制页的下拉。
	LANIPs []AddressCandidate

	// PublicIP 为 nil 表示不做公网地址的外部探测 ——
	// 那是个可选增强,没有它一切照常,只是下拉里少一个候选。
	PublicIP PublicIPProbe

	// PortMap 为 nil 表示这一版没有自动映射能力,相关界面元素不显示。
	PortMap PortMapper

	// RemoteControl 为 nil 表示这一版没有远程控制能力,相关界面元素不显示。
	RemoteControl RemoteController

	// OnTopologyChange 在需要重建 MediaMTX 配置时调用。
	//
	// 只有公网地址变化会触发 —— 那个值写在 MediaMTX 的配置文件里
	// (webrtcAdditionalHosts),改了必须重写文件并重启它的进程,
	// 否则界面上那个输入框就是个摆设。
	OnTopologyChange func(config.Config) error

	// OnControlPageGone 在控制页关闭后调用(判断方式见 alive.go)。
	//
	// 返回 false 表示已经收尾了,看门狗随之结束(通常是整程序退出)。
	// 返回 true 表示这次不退、继续盯着 —— 远程控制开着时就是这样,见
	// alive.go 的说明。
	//
	// nil 表示不启用:关掉浏览器,程序照常在托盘里待着。
	OnControlPageGone func() bool
}

func New(o Options) *Server {
	s := &Server{
		stream:        o.Stream,
		runner:        o.Runner,
		mtx:           o.MTX,
		encoders:      o.Encoders,
		presets:       o.Presets,
		lanIP:         o.LANIP,
		lanIPs:        o.LANIPs,
		pubIP:         o.PublicIP,
		cfgPath:       o.ConfigPath,
		cfg:           o.Config,
		onTopo:        o.OnTopologyChange,
		portMap:       o.PortMap,
		remote:        o.RemoteControl,
		onControlGone: o.OnControlPageGone,
	}
	s.stream.SetConfig(o.Config)
	s.stream.SetEncoders(o.Encoders)
	return s
}

// Handler 返回注册好路由的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(webui.Files, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("/", s.handleIndex)

	mux.HandleFunc("/api/alive", s.handleAlive)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/start", s.handleStart)
	mux.HandleFunc("/api/stop", s.handleStop)
	mux.HandleFunc("/api/restart", s.handleRestart)
	mux.HandleFunc("/api/presets", s.handlePresets)
	mux.HandleFunc("/api/encoders", s.handleEncoders)
	mux.HandleFunc("/api/windows", s.handleWindows)
	mux.HandleFunc("/api/audio/devices", s.handleAudioDevices)
	mux.HandleFunc("/api/portmap/retry", s.handlePortMapRetry)

	mux.HandleFunc("/api/rc/enable", s.handleRCEnable)
	mux.HandleFunc("/api/rc/approve", s.handleRCApprove)
	mux.HandleFunc("/api/rc/deny", s.handleRCDeny)
	mux.HandleFunc("/api/rc/revoke", s.handleRCRevoke)
	mux.HandleFunc("/api/rc/rotate", s.handleRCRotate)
	mux.HandleFunc("/api/rc/links", s.handleRCLinks)

	return mux
}

// ListenAndServe 在 127.0.0.1:port 上启动服务,直到 ctx 取消。
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	if s.onControlGone != nil {
		go s.watchControlPage(ctx)
	}
	// 跟着 ctx 走,程序退出时一起停
	s.startAddressPoller(ctx)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(webui.Files, "static/index.html")
	if err != nil {
		http.Error(w, "控制页资源缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// ---------- 配置读写 ----------

func (s *Server) currentConfig() config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) updateConfig(c config.Config) error {
	c.Normalize()
	s.cfgMu.Lock()
	s.cfg = c
	s.cfgMu.Unlock()

	s.stream.SetConfig(c)

	if err := config.Save(s.cfgPath, c); err != nil {
		return err
	}
	return nil
}

// ---------- 响应辅助 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("写入响应失败: %v", err)
	}
}
