package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"

	"sharescreen/internal/audio"
	"sharescreen/internal/config"
	"sharescreen/internal/ffmpeg"
	"sharescreen/internal/portmap"
	"sharescreen/internal/remotectl"
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

	// Addresses 是下拉里能选的地址。这里带一份是为了首屏就有东西可选 ——
	// 只放在 /api/status 里的话,下拉会空到第一次轮询(约 2 秒)才填上。
	Addresses *addressesResponse `json:"addresses,omitempty"`
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

	// RemoteControl 是远程控制的状态快照;没启用这一版功能时为 nil。
	//
	// 放在这个 2 秒轮询的端点里,而不是新开一条推送通道:控制页本来
	// 就在按这个频率刷新,"有人请求控制"晚两秒出现完全可以接受。
	RemoteControl *remotectl.Status `json:"remoteControl,omitempty"`

	// RemoteSupported 报告当前采集源能不能远程控制。
	//
	// 前端拿它来提前禁用开关并说明原因 —— 让用户勾上一个注定失败的
	// 开关,再告诉他"当前采集源不支持",是更差的体验。
	RemoteSupported bool `json:"remoteSupported"`

	// Addresses 是下拉里能选的地址。跟着 2 秒轮询下发,因为外部探测的结果
	// 会变 —— 放在 /api/state 里就只在首次渲染时对一次,之后再也不会更新。
	Addresses *addressesResponse `json:"addresses,omitempty"`
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
		Addresses: s.buildAddresses(),
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
//
// 只给 WHIP。以前还列过一个 RTMP 地址作为兜底,去掉了:它**只有画面没有
// 声音** —— RTMP 只能带 AAC,而浏览器的 WebRTC 只认 Opus,MediaMTX 又不
// 转码(实测,见架构设计 §5.5)。一条注定没声音的地址摆在"推荐"下面,
// 用户选了它然后来问为什么没声音,这个来回不值得。
//
// MediaMTX 那边的 RTMP 监听还开着(本机回环,不映射到公网),所以真要
// 用 RTMP 的路子仍然通 —— 只是界面上不再引着人去用。
func (s *Server) pushURLs(cfg config.Config) []pushURL {
	return []pushURL{
		{
			Label: "WHIP",
			URL:   fmt.Sprintf("http://127.0.0.1:%d/%s/whip", cfg.WebRTCPort, cfg.StreamPath),
			Note:  "含音频,推荐。需要 OBS 29 或更高版本。",
			Kind:  "whip",
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
		Status:          s.effectiveStatus(cfg),
		Viewers:         s.viewerCount(cfg),
		Capacity:        cfg.Capacity(),
		PublicHost:      cfg.PublicHost,
		RemoteSupported: ffmpeg.SupportsRemote(cfg.Video),
		Addresses:       s.buildAddresses(),
	}
	if s.remote != nil {
		st := s.remote.Status()
		resp.RemoteControl = &st
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

// noteHostMismatch 在"公网地址"和实际能通的那个地址对不上时出声。
//
// 自动映射成功、状态灯是绿的,但链接就是打不开 —— 这种"每一步看着都对"的
// 故障最难查,所以宁可在这里把话说明白。三种对不上:
//
//  1. **填的 IP 不是本机当前的公网 IP。** 最隐蔽的一种:家宽的 IP 会变,而
//     手动填进去的地址程序不会去改(见 config.CanAutoSetPublicHost),于是
//     它会一直错下去,而界面上没有任何地方说这件事。实测踩到过:配置里写着
//     112.226.166.178,路由器报告的却是 112.254.141.83,公网链接对所有人都
//     打不开,但状态灯一直是绿的。
//  2. **填的端口和自动映射开的不是一个**(比如照手动部署文档写了 8443)。
//  3. **压根没写端口** —— 链接会按 80 拼,而映射开在别的端口上。
//
// 只警告,不改用户的输入:DDNS 域名那种情况地址本来就不该跟着 IP 变,程序
// 无权替用户决定。所以这里只把"现在这样是打不开的"讲清楚,并给出改法。
//
// 改动的是传进来的副本,不是 Mapper 内部的状态。
func noteHostMismatch(cfg config.Config, snap *portmap.Snapshot) {
	if snap.State != portmap.StateActive || cfg.PublicHost == "" {
		return
	}

	host := hostOnly(cfg.PublicHost)

	// 填的是 IP 字面量、而且和路由器报的外网地址不同。
	//
	// 域名不参与这个判断 —— DDNS 域名本来就该和当前 IP 不一样,那正是它
	// 存在的意义。
	if ip := net.ParseIP(host); ip != nil && snap.ExternalIP != "" && ip.String() != snap.ExternalIP {
		snap.Hint = fmt.Sprintf(
			"公网地址填的是 %s,而路由器报告的外网地址是 %s —— 这个链接打不开。"+
				"家宽的 IP 会变,把手填的地址清空、让程序自己填(它拿到的就是当前地址)。",
			ip, snap.ExternalIP)
		return
	}

	// 没写端口是**正常情况**:链接的端口由 publicWatchHost 补上,用户不必管。
	// (以前这里会警告"链接会走 80" —— 那是因为当时地址是原样拼进 URL 的。
	// 改成程序负责补端口之后,那条警告就不再成立了。)
	_, port, err := net.SplitHostPort(cfg.PublicHost)
	if err != nil {
		return
	}
	if port == strconv.Itoa(cfg.WebRTCPort) {
		return
	}

	snap.Hint = fmt.Sprintf(
		"公网地址里写的是 %s 端口,而自动映射开的是 %d —— 两者对不上,链接打不开。"+
			"把公网地址改成 %s,或者清空它让程序自己填。",
		port, cfg.WebRTCPort,
		net.JoinHostPort(host, strconv.Itoa(cfg.WebRTCPort)))
}

// publicWatchHost 返回公网观看链接里该用的「主机:端口」。
//
// **端口不归「公网地址」这个字段负责。** 绝大多数部署(自动映射)下外网端口
// 就是 WebRTCPort,让用户再抄一遍既多余又容易抄错 —— 实测就有人填了 IP 忘了
// 端口,链接按 80 拼出来,而界面上看不出任何问题:状态是绿的、地址也在,
// 就是打不开。现在地址怎么拼由程序负责,用户只管填"这台机器在公网上叫什么"。
//
// 仍然允许写端口:手动部署时外网端口可能被翻译过(文档里的 8443 → 8889),
// 那种情况内外端口本来就不一样,程序猜不出来,只能由用户写。写了就用他写的。
func publicWatchHost(cfg config.Config) string {
	host := hostOnly(cfg.PublicHost)
	if _, port, err := net.SplitHostPort(cfg.PublicHost); err == nil && port != "" {
		return net.JoinHostPort(host, port)
	}
	return net.JoinHostPort(host, strconv.Itoa(cfg.WebRTCPort))
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

		// 远控配置**只经 /api/rc/enable 改**,这条路一律原样保留服务端现值。
		//
		// 和公网地址同一类问题,但后果更重。控制页手里的 remoteControl 是
		// 载入时的快照 —— renderRemoteControl 只回填复选框,从不写回 config
		// 对象(见 app.js)。于是"开了远控之后再改任意一个视频参数"这条再平常
		// 不过的操作,提交上来的 enabled 是 false,把服务端配置冲掉,顺带把
		// Token 和剪贴板开关一起清零 —— 而远控监听还开着。
		//
		// 后果不只是界面和实际不一致:VideoConfig 之外,采集端画不画主机光标
		// 读的也是这个 enabled(见 ffmpeg.CaptureOptsFor)。配置被冲掉之后,
		// 紧接着由这次改动触发的那次重启就会把光标画回画面里,表现为"远控用
		// 着好好的,改个帧率之后鼠标又开始拖了"。
		next.RemoteControl = old.RemoteControl

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
				s.syncPortMapRules(updated)
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

	// 只写主机,不带端口 —— 端口由 publicWatchHost 在拼链接时补。
	// 这个字段的职责就一条:"这台机器在公网上叫什么"。
	host := externalIP
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

// handleAudioDevices 返回可选的播放设备列表,供界面上的设备下拉框使用。
//
// 和窗口列表一样按需枚举:用户随时会插拔耳机、切 HDMI,缓存下来只会让他
// 选到一台已经不存在的设备。枚举是毫秒级的。
func (s *Server) handleAudioDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list, err := audio.ListDevices()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "枚举播放设备失败: " + err.Error(),
		})
		return
	}
	if list == nil {
		list = []audio.Device{}
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
	// 链接里的主机名可以用户指定(LanHost),但**端口映射不走这里** ——
	// 见 linkHost 的说明。
	if host := s.linkHost(cfg); host != "" {
		out = append(out, watchURL{
			Label: "局域网",
			URL:   fmt.Sprintf("http://%s:%d/%s/%s", host, cfg.WebRTCPort, cfg.StreamPath, playQuery),
			Kind:  "lan",
		})
	}
	// 端口由 publicWatchHost 补,不要求用户写进地址里
	if cfg.PublicHost != "" {
		out = append(out, watchURL{
			Label: "公网",
			URL:   fmt.Sprintf("http://%s/%s/%s", publicWatchHost(cfg), cfg.StreamPath, playQuery),
			Kind:  "public",
		})
	}
	return out
}

// audioChanged 报告音频参数是否有实质差异。
//
// 开关变化要重启 —— 开了才走 WASAPI 采集那条路。码率变化同样要重启,
// 因为它是 ffmpeg 的输出参数。换设备也必须重启:设备是在启动时才打开的,
// 不重启的话用户在下拉框里换了一台,采的还是原来那台 —— 静默不生效。
func audioChanged(a, b config.AudioConfig) bool {
	return a.Enabled != b.Enabled ||
		a.BitrateKbps != b.BitrateKbps ||
		a.DeviceID != b.DeviceID
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
