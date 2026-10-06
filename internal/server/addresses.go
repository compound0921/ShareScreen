package server

import (
	"context"
	"log"
	"time"

	"sharescreen/internal/config"
	"sharescreen/internal/portmap"
	"sharescreen/internal/publicip"
)

// PublicIPProbe 探测本机的公网出口地址。nil 表示不做这项探测。
//
// 定成接口是为了让 server 的测试不碰网络 —— 和 PortMapper /
// RemoteController 一个路子。
type PublicIPProbe interface {
	Lookup(ctx context.Context) (publicip.Result, error)
}

const (
	// echoInterval 是外部探测的间隔。公网 IP 变得很慢,没必要问得勤 ——
	// 而且这是本程序唯一一个对外请求,能少发就少发。
	echoInterval = 10 * time.Minute

	// echoTimeout 盖住整条链(可能试好几个服务)。
	echoTimeout = 8 * time.Second
)

// AddressCandidate 是下拉里的一个候选地址。
//
// 导出是因为它要经 Options.LANIPs 从 main 传进来 —— 探测网卡那件事在
// package main(见 scanLANAddrs)。
type AddressCandidate struct {
	Host string `json:"host"`

	// Label 是给用户看的注解:网卡名、或者"路由器报告"这类来源。
	Label string `json:"label,omitempty"`
}

// addressesResponse 是下发给控制页的候选地址。
//
// 局域网和公网**分开下发**,因为它们是两个控件、两种语义:局域网那个只影响
// 链接里显示什么,公网那个决定公网链接指向哪。
type addressesResponse struct {
	// LAN 是探测到的局域网候选,按网卡扫描顺序。
	LAN []AddressCandidate `json:"lan,omitempty"`

	// LanAuto 是**自动实际会选的那个**地址(路由表推导,即 s.lanIP)。
	//
	// 必须单独下发,不能拿候选第一项顶替:候选是按网卡顺序排的,第一项通常
	// 不是自动选的那个 —— 这台机器上候选第一项是 astral 的 10.126.126.1,
	// 而自动选的其实是 192.168.3.236。前端要是拿第一项当"自动"来显示,就会
	// 出现"下拉说自动是 A、链接里却是 B"的怪现象,而且 A 因为被当成"自动那个"
	// 给跳过了,用户根本选不到它。
	LanAuto string `json:"lanAuto,omitempty"`

	// RouterIP 是路由器(UPnP)报告的外网地址。空表示没拿到。
	// 这是公网地址的**权威来源**:它知道端口映射有没有真的建起来。
	RouterIP string `json:"routerIp,omitempty"`

	// EchoIP 是外部回显服务探测到的地址,**仅供参考**。
	//
	// ⚠️ 运营商级 NAT 下它照样会返回一个漂亮的公网地址,但那是运营商那台
	// NAT 的出口,映射不进来。所以它绝不能拿去自动填写公网地址 ——
	// 那会悄悄产出一条打不开的链接,比留空更糟。
	EchoIP string `json:"echoIp,omitempty"`

	// EchoSource 是给出上面那个地址的服务,排障时有用。
	EchoSource string `json:"echoSource,omitempty"`
}

// linkHost 返回局域网链接里该显示的主机名。
//
// **端口映射不走这里。** 映射指向哪台主机是路由表决定的事实,不是偏好:
// 选错一块网卡会让路由器收到一个不属于它局域网的内网地址,直接回
// 402 Invalid Args(实测踩过)。所以那边继续用 s.lanIP,见 syncPortMapRules。
func (s *Server) linkHost(cfg config.Config) string {
	if cfg.LanHost != "" {
		return cfg.LanHost
	}
	return s.lanIP
}

// routerExternalIP 返回路由器报告的外网地址;没有或不可用就返回空串。
//
// 复用 portmap.ExternalIPUsable —— 它已经会挡掉私网、CGNAT、回环和 IPv6。
func (s *Server) routerExternalIP() string {
	if s.portMap == nil {
		return ""
	}
	ip := s.portMap.Snapshot().ExternalIP
	if ok, _ := portmap.ExternalIPUsable(ip); !ok {
		return ""
	}
	return ip
}

func (s *Server) buildAddresses() *addressesResponse {
	// LAN 直接透传探测结果,顺序就是 main 里定好的(网卡扫描序)。
	// 「自动会选哪个」另走 LanAuto —— 两者通常不是同一个。
	resp := &addressesResponse{LAN: s.lanIPs, LanAuto: s.lanIP}

	resp.RouterIP = s.routerExternalIP()

	s.echoMu.RLock()
	resp.EchoIP, resp.EchoSource = s.echoIP, s.echoSource
	s.echoMu.RUnlock()

	return resp
}

func (s *Server) setEcho(ip, source string) {
	s.echoMu.Lock()
	s.echoIP, s.echoSource = ip, source
	s.echoMu.Unlock()
}

// startAddressPoller 定时刷新外部探测到的公网地址。
//
// 跟着 ctx 走:控制页关掉、程序退出时一起停,不用另外接生命周期。
func (s *Server) startAddressPoller(ctx context.Context) {
	if s.pubIP == nil {
		return
	}
	go func() {
		s.refreshEcho(ctx)

		t := time.NewTicker(echoInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshEcho(ctx)
			}
		}
	}()
}

// refreshEcho 问一次回显服务。
//
// **路由器已经给出外网地址时一个请求都不发。** 那才是权威来源(只有它知道
// 端口映射有没有真的建起来),回显服务只在它拿不到的时候兜底。所以 UPnP 一
// 恢复,这个对外请求就自己停了。
func (s *Server) refreshEcho(ctx context.Context) {
	if s.routerExternalIP() != "" {
		s.setEcho("", "")
		return
	}

	lookupCtx, cancel := context.WithTimeout(ctx, echoTimeout)
	defer cancel()

	res, err := s.pubIP.Lookup(lookupCtx)
	if err != nil {
		// 可选增强,失败就是"没有这个候选",不打扰用户。
		// 日志也只记一次性的提示,不每 10 分钟刷一条。
		s.setEcho("", "")
		if !s.echoFailed.Swap(true) {
			log.Printf("公网地址探测:回显服务都用不了(%v),下拉里不会出现这一项", err)
		}
		return
	}

	s.echoFailed.Store(false)
	s.setEcho(res.IP, res.Service)
}
