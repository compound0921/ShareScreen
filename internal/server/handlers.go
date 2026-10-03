package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"

	"sharescreen/internal/config"
	"sharescreen/internal/ffmpeg"
	"sharescreen/internal/portmap"
	"sharescreen/internal/stream"
	"sharescreen/internal/window"
)

type watchURL struct {
	Label string `json:"label"`
	URL   string `json:"url"`
	Kind  string `json:"kind"` // lan | public
}

// pushURL 是给外部推流程序(OBS)用的地址。
type pushURL struct {
	Label string `json:"label"`
	URL   string `json:"url"`
	Note  string `json:"note"`
	Kind  string `json:"kind"` // whip | rtmp
}

type stateResponse struct {
	Config    config.Config       `json:"config"`
	Status    stream.Status       `json:"status"`
	Viewers   int                 `json:"viewers"` // -1 表示未知
	Capacity  int                 `json:"capacity"`
	WatchURLs []watchURL          `json:"watchUrls"`
	PushURLs  []pushURL           `json:"pushUrls"`
	Encoders  []ffmpeg.Encoder    `json:"encoders"`
	Presets   []config.Resolution `json:"presets"`
}

type statusResponse struct {
	Status   stream.Status `json:"status"`
	Viewers  int           `json:"viewers"`
	Capacity int           `json:"capacity"`

	// PublicHost 一并下发,是为了让前端认得出公网地址被自动映射改掉了 ——
	// 改了就得重新渲染观看链接,否则页面上挂的还是旧地址。
	PublicHost string `json:"publicHost"`

	// PortMap 是自动端口映射的状态快照;没启用这一版功能时为 nil。
	PortMap *portmap.Snapshot `json:"portMap,omitempty"`
}

// handleState 返回首屏需要的全部信息。
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.currentConfig()

	encoders := s.encoders
	if encoders == nil {
		encoders = []ffmpeg.Encoder{}
	}
	presets := s.presets
	if presets == nil {
		presets = []config.Resolution{}
	}

	writeJSON(w, http.StatusOK, stateResponse{
		Config:    cfg,
		Status:    s.effectiveStatus(cfg),
		Viewers:   s.viewerCount(cfg),
		Capacity:  cfg.Capacity(),
		WatchURLs: s.watchURLs(cfg),
		PushURLs:  s.pushURLs(cfg),
		Encoders:  encoders,
		Presets:   presets,
	})
}

// effectiveStatus 返回当前推流状态。
//
// 外部推流模式(OBS 直推)下本程序不跑 ffmpeg,手里没有它的状态,
// 得改从 MediaMTX 看 —— 只有它知道有没有推流源连着。
func (s *Server) effectiveStatus(cfg config.Config) stream.Status {
	if !cfg.Video.External() {
		return s.stream.Status()
	}

	st := stream.Status{External: true, TargetKbps: cfg.Video.BitrateKbps}

	path, err := s.mtx.PathState(cfg.StreamPath)
	if err != nil {
		st.State = stream.StateFailed
		st.LastError = "无法连接 MediaMTX: " + err.Error()
		return st
	}
	if path.HasSource && path.Ready {
		st.State = stream.StateRunning
		st.Running = true
	} else {
		st.State = stream.StateStopped
	}
	return st
}

// pushURLs 返回外部推流程序(OBS)要填的地址。
func (s *Server) pushURLs(cfg config.Config) []pushURL {
	return []pushURL{
		{
			Label: "WHIP",
			URL:   fmt.Sprintf("http://127.0.0.1:%d/%s/whip", cfg.WebRTCPort, cfg.StreamPath),
			Note:  "含音频,推荐。需要 OBS 29 或更高版本。",
			Kind:  "whip",
		},
		{
			Label: "RTMP",
			URL:   fmt.Sprintf("rtmp://127.0.0.1:%d/%s", cfg.RTMPPort, cfg.StreamPath),
			Note:  "**只有画面,没有声音。** RTMP 传的是 AAC,而浏览器的 WebRTC 不支持 AAC,MediaMTX 也不做转码(实测)。仅在 WHIP 不可用时使用。",
			Kind:  "rtmp",
		},
	}
}

// handleStatus 是轻量轮询端点,前端每 2 秒调一次。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := s.currentConfig()
	resp := statusResponse{
		Status:     s.effectiveStatus(cfg),
		Viewers:    s.viewerCount(cfg),
		Capacity:   cfg.Capacity(),
		PublicHost: cfg.PublicHost,
	}
	if s.portMap != nil {
		// Snapshot 内部加锁复制 —— 后台协程正在改那份状态,不能直接把
		// Mapper 塞进响应里。拿到的既然是副本,这里就可以按配置补话。
		snap := s.portMap.Snapshot()
		noteHostMismatch(cfg, &snap)
		resp.PortMap = &snap
	}
	writeJSON(w, http.StatusOK, resp)
}

// noteHostMismatch 在"自动映射开的端口"和"公网地址里写的端口"对不上时出声。
//
// 自动映射按内外端口一致的原则开 WebRTCPort,而用户手填的公网地址可能写了
// 别的端口(比如照着手动部署文档写的 8443)。两边对不上时,映射本身是成功的、
// 状态灯也是绿的,但链接就是打不开 —— 这种"每一步看着都对"的故障最难查,
// 所以宁可在这里把话说明白。
//
// 改动的是传进来的副本,不是 Mapper 内部的状态。
func noteHostMismatch(cfg config.Config, snap *portmap.Snapshot) {
	if snap.State != portmap.StateActive || cfg.PublicHost == "" {
		return
	}

	_, port, err := net.SplitHostPort(cfg.PublicHost)
	if err != nil {
		return // 没写端口,那走的是 80,不参与这个判断
	}
	if port == strconv.Itoa(cfg.WebRTCPort) {
		return
	}

	snap.Hint = fmt.Sprintf(
		"公网地址里写的是 %s 端口,而自动映射开的是 %d —— 两者对不上,链接打不开。"+
			"把公网地址改成 %s,或者清空它让程序自己填。",
		port, cfg.WebRTCPort,
		net.JoinHostPort(hostOnly(cfg.PublicHost), strconv.Itoa(cfg.WebRTCPort)))
}

// hostOnly 去掉主机串里的端口部分。
func hostOnly(hostPort string) string {
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		return h
	}
	return hostPort
}

// handleConfig 读取或更新配置。
//
// 更新时,如果推流参数有变化且当前正在推流,会自动触发重启 ——
// 参数面板的每次改动都要求用户再点一次"重启"太啰嗦。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.currentConfig())
		return

	case http.MethodPut:
		old := s.currentConfig()

		var next config.Config
		if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "配置格式错误: " + err.Error(),
			})
			return
		}

		// 注意:这里**不**用"公网地址变了就是用户改的"来判断。
		//
		// 控制页每次保存都会把整个配置对象发回来,而自动映射可能刚在两次
		// 轮询之间换了公网 IP —— 这时候页面手里还是旧值,值一变就被当成
		// 用户改的,自动跟随会悄无声息地失效。
		//
		// 所以标记由前端在"用户真的动了那个输入框"时清掉(见 app.js),
		// 后端原样采信。见 config.CanAutoSetPublicHost 与 ApplyAutoPublicHost。

		if err := s.updateConfig(next); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": err.Error(),
			})
			return
		}

		updated := s.currentConfig()
		restarted := false
		needsRestart := false

		// 公网地址写在 MediaMTX 的配置文件里(webrtcAdditionalHosts),
		// 改了就得重写文件并重启它的进程。之后推流也必须跟着重启 ——
		// MediaMTX 一重起,ffmpeg 的推流连接就断了。
		if old.PublicHost != updated.PublicHost && s.onTopo != nil {
			if err := s.onTopo(updated); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "重建 MediaMTX 配置失败: " + err.Error(),
				})
				return
			}
			needsRestart = true
		}

		if s.portMap != nil {
			if old.AutoPortMap != updated.AutoPortMap {
				// 后台协程去干活,不在这里等 —— 发现路由器要几秒,
				// 用户不该为了勾一个复选框转圈
				s.portMap.SetEnabled(updated.AutoPortMap)
			}
			// 端口改了必须跟着告知,否则映射还守着旧端口,而 MediaMTX
			// 已经换到新端口上监听了
			if old.WebRTCPort != updated.WebRTCPort || old.UDPPort != updated.UDPPort {
				s.portMap.Configure(portmap.RulesFor(updated.WebRTCPort, updated.UDPPort), s.lanIP)
			}
		}

		streamChanged := videoChanged(old.Video, updated.Video) ||
			audioChanged(old.Audio, updated.Audio)
		if streamChanged && s.stream.Status().Running {
			needsRestart = true
		}
		if needsRestart {
			s.stream.Restart()
			restarted = true
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"config":    updated,
			"capacity":  updated.Capacity(),
			"restarted": restarted,
		})
		return
	}

	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// ApplyAutoPublicHost 把自动映射拿到的公网地址写进配置并让它生效。
//
// 它和用户手动填地址走的是**同一条路**:PublicHost 变了就要重写 MediaMTX
// 配置(webrtcAdditionalHosts)并重启它,否则浏览器拿到的 ICE 候选还是内网
// 地址 —— 症状是页面能打开、播放器一直转圈,而看不出是哪儿不对。
//
// 由 portmap 的后台协程调用,所以这里可以放心做慢操作(重启 MediaMTX 要几秒)。
func (s *Server) ApplyAutoPublicHost(externalIP string) {
	if externalIP == "" {
		return
	}

	old := s.currentConfig()
	if !old.CanAutoSetPublicHost() {
		// 用户自己填了地址(多半是 DDNS 域名)。他填那个是有意的 ——
		// 域名不会变,而这台机器的公网 IP 会。
		return
	}

	host := net.JoinHostPort(externalIP, strconv.Itoa(old.WebRTCPort))
	if old.PublicHost == host && old.PublicHostAuto {
		return // 没变,不必白重启一次 MediaMTX
	}

	next := old
	next.PublicHost, next.PublicHostAuto = host, true

	if err := s.updateConfig(next); err != nil {
		log.Printf("自动映射:公网地址写入配置失败: %v", err)
		return
	}
	log.Printf("自动映射:公网观看地址已更新为 %s", host)

	if s.onTopo != nil {
		if err := s.onTopo(next); err != nil {
			log.Printf("自动映射:重建 MediaMTX 配置失败: %v", err)
			return
		}
	}
	// MediaMTX 一重起,ffmpeg 的推流连接就断了,得跟着重推
	if s.stream.Status().Running {
		s.stream.Restart()
	}
}

// handlePortMapRetry 让用户手动触发一次重试,不必等退避时间走完。
func (s *Server) handlePortMapRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.portMap == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "这一版没有自动端口映射"})
		return
	}
	s.portMap.Retry()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// externalNote 在外部推流模式下统一解释为什么这些按钮不起作用。
const externalNote = "当前是 OBS 直推模式 —— 流的开关由 OBS 控制,本程序不推流。"

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.currentConfig().Video.External() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": externalNote})
		return
	}
	s.stream.Start()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.currentConfig().Video.External() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": externalNote})
		return
	}
	s.stream.Stop()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.currentConfig().Video.External() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": externalNote})
		return
	}
	s.stream.Restart()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	presets := s.presets
	if presets == nil {
		presets = []config.Resolution{}
	}
	writeJSON(w, http.StatusOK, presets)
}

// handleWindows 返回当前可见的顶层窗口列表,供界面上的窗口选择器使用。
//
// 按需调用而不是缓存 —— 窗口标题随时在变(浏览器标签、编辑器里的文件名),
// 缓存反而会让用户选到已经失效的标题。枚举本身是毫秒级的。
func (s *Server) handleWindows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list, err := window.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "枚举窗口失败: " + err.Error(),
		})
		return
	}
	if list == nil {
		list = []window.Window{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleEncoders(w http.ResponseWriter, r *http.Request) {
	encoders := s.encoders
	if encoders == nil {
		encoders = []ffmpeg.Encoder{}
	}
	writeJSON(w, http.StatusOK, encoders)
}

// viewerCount 返回当前观众数;查询失败返回 -1(未知),
// 而不是 0 —— "不知道"和"确实没人看"在界面上应当区分。
func (s *Server) viewerCount(cfg config.Config) int {
	st, err := s.mtx.PathState(cfg.StreamPath)
	if err != nil {
		return -1
	}
	return st.Viewers
}

// playQuery 是拼在观看链接后面的查询串。
//
// MediaMTX 自带的播放页默认把 <video> 设成静音:
//
//	video.muted = parseBoolString(params.get("muted"), true);
//	                                                    ↑ 默认值
//
// 结果就是画面一切正常、一点声音都没有 —— 太容易被误判成音频链路坏了。
// 我们自己生成的链接一律带上 muted=0。
const playQuery = "?muted=0"

func (s *Server) watchURLs(cfg config.Config) []watchURL {
	// 链接里不带凭据。
	//
	// 试过把 user/pass 编进查询串,但 MediaMTX 的 WHEP 端点只认 HTTP Basic
	// 认证,不认查询参数(实测:带正确密码的查询串仍然返回 401)。所以凭据
	// 只能单独告诉观众 —— 浏览器会在第一次访问时弹原生登录框,输入一次后
	// 自动记住。
	//
	// 末尾的斜杠是必须的:MediaMTX 对 /live 发 302 跳到 /live/。
	out := []watchURL{}
	if s.lanIP != "" {
		out = append(out, watchURL{
			Label: "局域网",
			URL:   fmt.Sprintf("http://%s:%d/%s/%s", s.lanIP, cfg.WebRTCPort, cfg.StreamPath, playQuery),
			Kind:  "lan",
		})
	}
	// PublicHost 允许带端口,例如 example.com:8443
	if cfg.PublicHost != "" {
		out = append(out, watchURL{
			Label: "公网",
			URL:   fmt.Sprintf("http://%s/%s/%s", cfg.PublicHost, cfg.StreamPath, playQuery),
			Kind:  "public",
		})
	}
	return out
}

// audioChanged 报告音频参数是否有实质差异。
//
// 开关变化要重启 —— 开了才走 WASAPI 采集那条路。码率变化同样要重启,
// 因为它是 ffmpeg 的输出参数。
func audioChanged(a, b config.AudioConfig) bool {
	return a.Enabled != b.Enabled || a.BitrateKbps != b.BitrateKbps
}

// videoChanged 报告两套推流参数是否有实质差异 —— 有差异就需要重启 ffmpeg。
func videoChanged(a, b config.VideoConfig) bool {
	return a.Source != b.Source ||
		a.WindowTitle != b.WindowTitle ||
		a.Width != b.Width ||
		a.Height != b.Height ||
		a.FPS != b.FPS ||
		a.BitrateKbps != b.BitrateKbps ||
		a.Encoder != b.Encoder ||
		a.KeyframeSec != b.KeyframeSec
}
