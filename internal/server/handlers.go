package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"sharescreen/internal/config"
	"sharescreen/internal/ffmpeg"
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
	writeJSON(w, http.StatusOK, statusResponse{
		Status:   s.effectiveStatus(cfg),
		Viewers:  s.viewerCount(cfg),
		Capacity: cfg.Capacity(),
	})
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
		// MediaMTX 一重起,ffmpeg 的 RTMP 连接就断了。
		if old.PublicHost != updated.PublicHost && s.onTopo != nil {
			if err := s.onTopo(updated); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "重建 MediaMTX 配置失败: " + err.Error(),
				})
				return
			}
			needsRestart = true
		}

		if videoChanged(old.Video, updated.Video) && s.stream.Status().Running {
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
			URL:   fmt.Sprintf("http://%s:%d/%s/", s.lanIP, cfg.WebRTCPort, cfg.StreamPath),
			Kind:  "lan",
		})
	}
	// PublicHost 允许带端口,例如 example.com:8443
	if cfg.PublicHost != "" {
		out = append(out, watchURL{
			Label: "公网",
			URL:   fmt.Sprintf("http://%s/%s/", cfg.PublicHost, cfg.StreamPath),
			Kind:  "public",
		})
	}
	return out
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
