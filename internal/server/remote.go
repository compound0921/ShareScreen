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

// SetRequestEntry 开关"请求开启"入口。它也走 applyRemoteControl。
func (s *Server) SetRequestEntry(on bool) error {
	return s.applyRemoteControl(func(rc *config.RemoteControlConfig) {
		rc.RequestEntry = on
	})
}

// approveOpen 落地一次"请求开启共享与远控":开始共享 + 打开远控。
//
// 拆成接收三个依赖的纯函数,而不是直接写 s.stream / s.SetRemoteControlEnabled:
// 这条路上最容易出问题的是**部分成功**(远控开不起来但共享能开、或者反过来),
// 而那种矩阵要能一个个试过去 —— 真起一个推流管理器和远控服务是测不动的。
// 和 audioHint、popupController 是同一个路数。
//
// 顺序**先远控、后共享**:
//
//   - 远控那一步是会失败的那个(采集源不支持、端口被占),先做它,失败就能
//     立刻报给远端,而不是先拉起一条流再来说"其实没完全成功"。
//   - 共享用"本来就在跑"挡一下:已经在推流时再 Start 一次会重启采集,
//     观众那边就是一次没必要的黑屏。
func approveOpen(sharing bool, start func(), enable func(bool) error) remotectl.OpenResult {
	var out remotectl.OpenResult

	if err := enable(true); err != nil {
		out.Err = fmt.Errorf("远程控制没能打开:%w", err)
	} else {
		out.Remote = true
	}

	// 远控没开成也照样开始共享:画面还是能看的,只是不能操作。把两件事
	// 绑在一起做全有或全无,会让"采集源不支持远控"这种机器连看都看不成,
	// 而那是它们本来就能做的事。
	if !sharing {
		start()
	}
	out.Sharing = true

	return out
}

// ApproveOpen 是 approveOpen 在生产里的接线:主机同意一次"请求开启"之后,
// 由 remotectl 通过 Options.OnApproveOpen 调回来。
//
// 导出是因为它要跨包交给 remotectl(和这个包里别的 handler 不一样 ——
// 那些只挂在 mux 上,不用给谁看)。
func (s *Server) ApproveOpen() remotectl.OpenResult {
	return approveOpen(
		s.stream.Status().Running,
		s.stream.Start,
		s.SetRemoteControlEnabled,
	)
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

	// 令牌在第一次需要它的时候生成 —— 远控开了,或者请求入口开着。
	//
	// 不在默认配置里预生成,是为了让"从没用过这个功能"的配置文件里
	// 不躺着一串没人认识的秘密 —— 那种东西过一阵子就没人敢删了。
	// 但请求入口默认就是开的,所以实际效果是"装好第一次跑就有一串",
	// 这是它的代价的一部分:没有令牌,那条链接就发不出去。
	if (next.RemoteControl.Enabled || next.RemoteControl.RequestEntry) &&
		next.RemoteControl.Token == "" {
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
	//
	// 判据是"本来有没有在监听"(两个开关任一为真),不是只看 Enabled ——
	// 只按 Enabled 找,请求入口开着而远控关着的时候换端口,旧监听会一直
	// 占着旧端口不放,而新端口永远绑不上。
	if next.RemoteControl.Port != before.RemoteControl.Port && before.RemoteControl.MappedPort() != 0 {
		s.remote.DisconnectAll()
		_ = s.remote.SetEnabled(false)
		_ = s.remote.SetRequestEntry(false)
	}
	s.remote.Configure(next.RemoteControl.Port, next.RemoteControl.Token,
		next.RemoteControl.ControlPassword, next.RemoteControl.AllowClipboard)

	// 路由器上的映射也要跟上:关着的时候公网上不该留着指向这个端口的洞。
	s.syncPortMapRules(next)

	// 开启失败时**不**把配置里的开关退回去。失败通常是暂时的(端口被占、
	// 防火墙没放行),用户下次启动仍然想要这个功能;把开关退回去等于让他
	// 每次都得重新勾一遍,而真正的原因会随 Status 一起报出来。
	err := s.remote.SetEnabled(next.RemoteControl.Enabled)
	if err == nil {
		err = s.remote.SetRequestEntry(next.RemoteControl.RequestEntry)
	}

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

// handleRCEnable 开关远程控制、开关请求入口,顺带可以改端口。
//
// 开启是这个程序里会新增对外监听的入口,所以这里做的每一件事都要能解释
// 清楚:生成令牌(首次)、写配置、真正开监听、把端口加进路由器映射。
func (s *Server) handleRCEnable(w http.ResponseWriter, r *http.Request) {
	if s.remote == nil {
		s.rcUnavailable(w)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 字段全是可选的:端口走同一个端点改,但只改端口时不该顺手把开关也
	// 动一下。用指针才能区分"没填"和"填了 false"。
	var req struct {
		Enabled         *bool   `json:"enabled"`
		RequestEntry    *bool   `json:"requestEntry"`
		Port            *int    `json:"port"`
		AllowClipboard  *bool   `json:"allowClipboard"`
		ControlPassword *string `json:"controlPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式错误"})
		return
	}

	err := s.applyRemoteControl(func(rc *config.RemoteControlConfig) {
		if req.Enabled != nil {
			rc.Enabled = *req.Enabled
		}
		if req.RequestEntry != nil {
			rc.RequestEntry = *req.RequestEntry
		}
		if req.Port != nil {
			rc.Port = *req.Port
		}
		if req.AllowClipboard != nil {
			rc.AllowClipboard = *req.AllowClipboard
		}
		// 用指针才能区分"没填"和"填了空串(= 清除密码)"。空串是有意义的
		// 一次设置:它是"把这个捷径关掉,回到全靠当场批准"。
		if req.ControlPassword != nil {
			rc.ControlPassword = *req.ControlPassword
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

	s.remote.Configure(next.RemoteControl.Port, tok,
		next.RemoteControl.ControlPassword, next.RemoteControl.AllowClipboard)
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
	if host := s.linkHost(cfg); host != "" {
		out = append(out, watchURL{
			Label: "局域网",
			URL:   fmt.Sprintf("http://%s:%d%s", host, port, suffix),
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
// 远控端口在**端口会监听**的时候才映射(远控开着、或者请求入口开着,见
// MappedPort)。请求入口那条是有意的:它的全部意义就是"远端的人够得到"，
// 不映射就只剩局域网能用,名不副实。
//
// 真正把它暴露到公网还要 AutoPortMap 也打开(默认关),所以默认配置是
// "监听 8090,但路由器上没有洞"。
func (s *Server) syncPortMapRules(cfg config.Config) {
	if s.portMap == nil {
		return
	}
	// 转发目标**固定用 s.lanIP**(路由表推导出来的那个),不跟 cfg.LanHost 走。
	//
	// 这是"事实"和"偏好"的区别:映射指向哪台主机由路由表决定,选错一块网卡
	// 会让路由器收到一个不属于它局域网的内网地址,直接回 402 Invalid Args
	// (实测踩过)。LanHost 只影响链接里显示什么,见 linkHost。
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
