package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"

	"sharescreen/internal/config"
	"sharescreen/internal/ffmpeg"
	"sharescreen/internal/portmap"
	"sharescreen/internal/remotectl"
)

// SetRemoteControlEnabled 开关远程控制。托盘菜单走这条路。
//
// 和控制页的 /api/rc/enable 最终落到同一个 applyRemoteControl 上 ——
// 两条入口各写一遍的话,迟早会在某个细节上分叉(最典型的是"一边生成了
// 令牌、另一边没生成"),而那种分叉只在用到另一条路的时候才暴露。
func (s *Server) SetRemoteControlEnabled(on bool) error {
	return s.applyRemoteControl(func(rc *config.RemoteControlConfig) {
		rc.Enabled = on
	})
}

// applyRemoteControl 把远控配置真正落到位:写配置、开/关监听、同步路由器映射。
//
// 这三个动作必须一起做,少任何一个都会留下一个"界面显示已开启但其实
// 没通"的状态 —— 那是最难查的一类问题,因为每一步单独看都是对的。
func (s *Server) applyRemoteControl(mutate func(*config.RemoteControlConfig)) error {
	if s.remote == nil {
		return errors.New("这一版没有远程控制功能")
	}

	before := s.currentConfig()
	next := before
	mutate(&next.RemoteControl)
	next.Normalize() // 端口非法时退回默认值,别让一个坏数字传下去

	// 令牌在第一次开启时才生成。
	//
	// 不在默认配置里预生成,是为了让"从没用过这个功能"的配置文件里
	// 不躺着一串没人认识的秘密 —— 那种东西过一阵子就没人敢删了。
	if next.RemoteControl.Enabled && next.RemoteControl.Token == "" {
		tok, err := remotectl.NewToken()
		if err != nil {
			return fmt.Errorf("生成凭据失败: %w", err)
		}
		next.RemoteControl.Token = tok
	}

	if err := s.updateConfig(next); err != nil {
		return err
	}

	// 换端口必须先把监听拆掉,否则旧的还占着,新的绑不上。
	if next.RemoteControl.Port != before.RemoteControl.Port && before.RemoteControl.Enabled {
		s.remote.DisconnectAll()
		_ = s.remote.SetEnabled(false)
	}
	s.remote.Configure(next.RemoteControl.Port, next.RemoteControl.Token, next.RemoteControl.AllowClipboard)

	// 路由器上的映射也要跟上:关着的时候公网上不该留着指向这个端口的洞。
	s.syncPortMapRules(next)

	// 开启失败时**不**把配置里的开关退回去。失败通常是暂时的(端口被占、
	// 防火墙没放行),用户下次启动仍然想要这个功能;把开关退回去等于让他
	// 每次都得重新勾一遍,而真正的原因会随 Status 一起报出来。
	err := s.remote.SetEnabled(next.RemoteControl.Enabled)

	// 远控开关还决定视频里画不画主机光标(见 ffmpeg.CaptureOptsFor),而那是
	// ffmpeg 的启动参数 —— 开关一变就得重启采集。这是这个开关唯一的代价:
	// 点一下黑屏 2–5 秒(连同音频一起重启)。之所以接受,是因为按"此刻有没有
	// 人持有控制权"来切会在会话中途黑屏,比这更糟。
	//
	// 两个约束:
	//   - updateConfig 必须已经跑过(它在上面)。Restart 是异步的,startOnce
	//     从配置快照里读光标开关,顺序反了就会拿旧值重启。
	//   - 比的是"这份配置下画不画光标",不是 Enabled 本身。改剪贴板开关和改
	//     端口也走这个函数,那些不该重启;而在不支持远控的采集源上打开开关
	//     (界面会禁用,但配置能被别处写入)同样不该重启。
	//
	// 刻意**不**用 err 去挡:开关失败时配置仍然是 Enabled(见上),若这里因为
	// 失败而不重启,用户修好端口再打开一次时 Enabled 并没有变化,光标就永远
	// 画不回来了。宁可多一次黑屏,也要让"配置里写的是什么,采集就跑的是什么"
	// 这条不变量成立。
	if s.cursorHidden(next) != s.cursorHidden(before) && s.stream.Status().Running {
		s.stream.Restart()
	}

	return err
}

// cursorHidden 报告这份配置下采集会不会把主机光标画进画面。
//
// 判定整个交给 ffmpeg.CaptureOptsFor,这里不另写一份 —— 那边是唯一一处
// 定义,重复一份迟早会在某个分支上分叉。
func (s *Server) cursorHidden(c config.Config) bool {
	return !ffmpeg.CaptureOptsFor(c.Video, c.RemoteControl.Enabled).DrawMouse
}

// handleRCEnable 开关远程控制,顺带可以改端口。
//
// 开启是这个程序里唯一会新增对外监听的入口,所以这里做的每一件事都要
// 能解释清楚:生成令牌(首次)、写配置、真正开监听、把端口加进路由器映射。
func (s *Server) handleRCEnable(w http.ResponseWriter, r *http.Request) {
	if s.remote == nil {
		s.rcUnavailable(w)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 两个字段都是可选的:端口走同一个端点改,但只改端口时不该顺手把
	// 开关也动一下。用指针才能区分"没填"和"填了 false"。
	var req struct {
		Enabled        *bool `json:"enabled"`
		Port           *int  `json:"port"`
		AllowClipboard *bool `json:"allowClipboard"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式错误"})
		return
	}

	err := s.applyRemoteControl(func(rc *config.RemoteControlConfig) {
		if req.Enabled != nil {
			rc.Enabled = *req.Enabled
		}
		if req.Port != nil {
			rc.Port = *req.Port
		}
		if req.AllowClipboard != nil {
			rc.AllowClipboard = *req.AllowClipboard
		}
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": err.Error(), "remote": s.remote.Status(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remote": s.remote.Status()})
}

// handleRCApprove 批准一份控制申请。
func (s *Server) handleRCApprove(w http.ResponseWriter, r *http.Request) {
	s.rcAction(w, r, func(id string) (bool, string) {
		if !s.remote.Approve(id) {
			return false, "这份申请已经失效了"
		}
		return true, ""
	})
}

// handleRCDeny 拒绝一份控制申请。
func (s *Server) handleRCDeny(w http.ResponseWriter, r *http.Request) {
	s.rcAction(w, r, func(id string) (bool, string) {
		if !s.remote.Deny(id) {
			return false, "这份申请已经失效了"
		}
		return true, ""
	})
}

// handleRCRevoke 收回控制权。不需要 id —— 不管现在是谁在控制,一律收回。
func (s *Server) handleRCRevoke(w http.ResponseWriter, r *http.Request) {
	if s.remote == nil || r.Method != http.MethodPost {
		s.rcUnavailable(w)
		return
	}
	s.remote.Revoke()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remote": s.remote.Status()})
}

// handleRCRotate 换一个令牌,让已经发出去的链接全部失效。
//
// 这是"链接可能泄露了"的补救手段。换完必须断开现有连接 —— 否则拿着
// 旧链接的人还能继续操作,换令牌就白换了。
func (s *Server) handleRCRotate(w http.ResponseWriter, r *http.Request) {
	if s.remote == nil || r.Method != http.MethodPost {
		s.rcUnavailable(w)
		return
	}

	tok, err := remotectl.NewToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "error": "生成凭据失败: " + err.Error(),
		})
		return
	}

	next := s.currentConfig()
	next.RemoteControl.Token = tok
	if err := s.updateConfig(next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "error": err.Error(),
		})
		return
	}

	s.remote.Configure(next.RemoteControl.Port, tok, next.RemoteControl.AllowClipboard)
	s.remote.DisconnectAll()

	if next.RemoteControl.Enabled {
		if err := s.remote.SetEnabled(true); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "error": err.Error(), "remote": s.remote.Status(),
			})
			return
		}
	}

	log.Printf("远程控制:凭据已重置,旧链接全部失效")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remote": s.remote.Status()})
}

// handleRCLinks 返回带凭据的可控制链接。
func (s *Server) handleRCLinks(w http.ResponseWriter, r *http.Request) {
	if s.remote == nil {
		writeJSON(w, http.StatusOK, map[string]any{"links": []watchURL{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": s.remoteLinks(s.currentConfig())})
}

// remoteLinks 拼出可控制链接。
//
// 令牌放在 URL 的 # 片段里而不是查询串:片段不会被发到服务器,所以
// 这几条链接在主机自己的访问日志里是不带凭据的。
func (s *Server) remoteLinks(cfg config.Config) []watchURL {
	token := cfg.RemoteControl.Token
	if token == "" {
		return []watchURL{}
	}
	port := cfg.RemoteControl.Port
	suffix := "/rc#t=" + token

	out := []watchURL{}
	if s.lanIP != "" {
		out = append(out, watchURL{
			Label: "局域网",
			URL:   fmt.Sprintf("http://%s:%d%s", s.lanIP, port, suffix),
			Kind:  "lan",
		})
	}
	if cfg.PublicHost != "" {
		// PublicHost 里带的端口是**观看**端口,不是远控端口,所以这里只取
		// 主机名。自动映射下两者是同一个数;手动配过端口翻译的话需要用户
		// 自己把端口改成映射出去的那个。
		if host := hostOnly(cfg.PublicHost); host != "" {
			out = append(out, watchURL{
				Label: "公网",
				URL:   "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + suffix,
				Kind:  "public",
			})
		}
	}
	return out
}

// syncPortMapRules 按当前配置刷新路由器上要开的端口。
//
// 远控端口只在远控开着的时候才映射 —— 这是"默认关闭 = 零攻击面"那句话
// 在路由器上的落实:不开启时,公网上不会多出任何一个指向本机的洞。
func (s *Server) syncPortMapRules(cfg config.Config) {
	if s.portMap == nil {
		return
	}
	s.portMap.Configure(
		portmap.RulesForWithControl(cfg.WebRTCPort, cfg.UDPPort, cfg.RemoteControl.MappedPort()),
		s.lanIP)
}

// rcAction 是 approve / deny 共用的请求解析。
func (s *Server) rcAction(w http.ResponseWriter, r *http.Request, fn func(string) (bool, string)) {
	if s.remote == nil || r.Method != http.MethodPost {
		s.rcUnavailable(w)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少申请编号"})
		return
	}

	if ok, msg := fn(req.ID); !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": msg, "remote": s.remote.Status()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remote": s.remote.Status()})
}

func (s *Server) rcUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "这一版没有远程控制功能"})
}
