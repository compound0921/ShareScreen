package server

import (
	"context"
	"errors"
	"testing"

	"sharescreen/internal/config"
	"sharescreen/internal/portmap"
	"sharescreen/internal/publicip"
)

// fakeProbe 顶替外部探测,不碰网络。
type fakeProbe struct {
	res publicip.Result
	err error
}

func (f fakeProbe) Lookup(context.Context) (publicip.Result, error) { return f.res, f.err }

// ── linkHost ──

// LanHost 为空就用自动探测到的那个;非空就用用户的。
func TestLinkHost(t *testing.T) {
	s := &Server{lanIP: "192.168.3.236"}

	if got := s.linkHost(config.Config{}); got != "192.168.3.236" {
		t.Errorf("LanHost 为空时 = %q,想要自动探测的 192.168.3.236", got)
	}
	if got := s.linkHost(config.Config{LanHost: "10.126.126.1"}); got != "10.126.126.1" {
		t.Errorf("LanHost 非空时 = %q,想要用户的 10.126.126.1", got)
	}
}

// 局域网链接里显示的地址要跟着 LanHost 走。
func TestWatchURLsHonorsLanHost(t *testing.T) {
	s := &Server{lanIP: "192.168.3.236"}
	cfg := config.Config{WebRTCPort: 8889, StreamPath: "live", LanHost: "10.126.126.1"}

	var lan string
	for _, u := range s.watchURLs(cfg) {
		if u.Kind == "lan" {
			lan = u.URL
		}
	}
	if lan == "" {
		t.Fatal("没有生成局域网链接")
	}
	want := "http://10.126.126.1:8889/live/?muted=0"
	if lan != want {
		t.Errorf("局域网链接 = %q,想要 %q", lan, want)
	}
}

// 控制链接同样要跟着走。
func TestRemoteLinksHonorLanHost(t *testing.T) {
	s := &Server{lanIP: "192.168.3.236"}
	cfg := config.Config{
		RemoteControl: config.RemoteControlConfig{Enabled: true, Port: 8090, Token: "tok"},
		LanHost:       "10.126.126.1",
	}

	var lan string
	for _, u := range s.remoteLinks(cfg) {
		if u.Kind == "lan" {
			lan = u.URL
		}
	}
	want := "http://10.126.126.1:8090/rc#t=tok"
	if lan != want {
		t.Errorf("控制链接 = %q,想要 %q", lan, want)
	}
}

// ⚠️ 这条是这次改动最要紧的回归:**端口映射的转发目标不受 LanHost 影响**。
//
// 映射指向哪台主机是路由表决定的事实。让它跟着用户的选择走,选错一块网卡
// 就会把路由器收到一个不属于它局域网的内网地址 —— 实测正是 402 Invalid Args
// 的成因。
func TestPortMapTargetIgnoresLanHost(t *testing.T) {
	pm := &recordingPortMap{}
	s := &Server{lanIP: "192.168.3.236", portMap: pm}

	cfg := config.Config{
		WebRTCPort: 8889,
		UDPPort:    8189,
		LanHost:    "10.126.126.1", // 用户选了一块别的网卡
	}
	s.syncPortMapRules(cfg)

	if pm.internal != "192.168.3.236" {
		t.Errorf("映射的转发目标 = %q,必须是路由表推导出来的 192.168.3.236,而不是 LanHost",
			pm.internal)
	}
}

type recordingPortMap struct{ internal string }

func (r *recordingPortMap) Configure(_ []portmap.Rule, internalIP string) { r.internal = internalIP }
func (r *recordingPortMap) Snapshot() portmap.Snapshot                    { return portmap.Snapshot{} }
func (r *recordingPortMap) SetEnabled(bool)                               {}
func (r *recordingPortMap) Retry()                                        {}

// ── 候选下发 ──

// 候选列表按网卡扫描序透传,而"自动会选哪个"**单独下发**。
//
// 这两者不是一回事,而且通常不是同一个:这台机器上候选第一项是 astral 的
// 10.126.126.1,而自动选的其实是 192.168.3.236。前端要是拿第一项当"自动"
// 显示,就会同时错两处 —— 下拉说的和链接里的不一致,而真正自动选的那个因为
// 被当成"自动"跳过,反而选不到。
func TestBuildAddresses(t *testing.T) {
	s := &Server{
		lanIP: "192.168.3.236",
		lanIPs: []AddressCandidate{
			{Host: "10.126.126.1", Label: "astral"},
			{Host: "192.168.3.236", Label: "WLAN 2"},
		},
	}

	resp := s.buildAddresses()
	if len(resp.LAN) != 2 {
		t.Fatalf("局域网候选 = %v,想要两条", resp.LAN)
	}
	// 候选顺序原样透传(扫描序),不要在这里重排
	if resp.LAN[0].Host != "10.126.126.1" || resp.LAN[0].Label != "astral" {
		t.Errorf("候选[0] = %+v,应当原样透传扫描结果", resp.LAN[0])
	}
	// 「自动」是另一个值,单独下发
	if resp.LanAuto != "192.168.3.236" {
		t.Errorf("LanAuto = %q,想要自动实际会选的 192.168.3.236", resp.LanAuto)
	}
}

// 外部探测的结果要带上来源,而且 UPnP 没结果时才有值。
func TestBuildAddressesIncludesEcho(t *testing.T) {
	s := &Server{}
	s.setEcho("118.1.2.3", "https://ip.3322.net")

	resp := s.buildAddresses()
	if resp.EchoIP != "118.1.2.3" || resp.EchoSource != "https://ip.3322.net" {
		t.Errorf("回显候选 = %q/%q", resp.EchoIP, resp.EchoSource)
	}
	if resp.RouterIP != "" {
		t.Errorf("没有 portMap 时 RouterIP 应当为空,实际 %q", resp.RouterIP)
	}
}

// refreshEcho:**路由器已经给出地址时一个请求都不发**。
//
// 用一个会 panic 的探测器钉住这条 —— 它一旦被调到,测试立刻炸。
// 这是本程序唯一一个对外请求,少发一次就少暴露一次。
func TestRefreshEchoSkipsWhenRouterKnows(t *testing.T) {
	s := &Server{portMap: &staticPortMap{ip: "112.254.141.83"}, pubIP: panicProbe{}}

	s.refreshEcho(context.Background()) // 不该 panic

	if s.echoIP != "" {
		t.Errorf("路由器有地址时不该去问回显服务,却拿到了 %q", s.echoIP)
	}
}

// 路由器没有地址时才去问,拿到就记下来。
func TestRefreshEchoWhenRouterSilent(t *testing.T) {
	s := &Server{portMap: &staticPortMap{}, pubIP: fakeProbe{
		res: publicip.Result{IP: "118.1.2.3", Service: "https://ip.3322.net"},
	}}

	s.refreshEcho(context.Background())

	if s.echoIP != "118.1.2.3" || s.echoSource != "https://ip.3322.net" {
		t.Errorf("echo = %q/%q", s.echoIP, s.echoSource)
	}
}

// 探测失败不报错、不留残值 —— 它只是个可选增强。
func TestRefreshEchoFailureIsSilent(t *testing.T) {
	s := &Server{portMap: &staticPortMap{}, pubIP: fakeProbe{err: errors.New("都不通")}}
	s.setEcho("旧值", "旧来源") // 先放一个,确认会被清掉

	s.refreshEcho(context.Background())

	if s.echoIP != "" || s.echoSource != "" {
		t.Errorf("探测失败后应当清空,实际 %q/%q", s.echoIP, s.echoSource)
	}
}

// pubIP 为 nil(测试、或没接探测器)时轮询不该起,也不该 panic。
func TestAddressPollerNilProbe(t *testing.T) {
	s := &Server{}
	s.startAddressPoller(context.Background()) // 直接返回,不起协程
}

type staticPortMap struct{ ip string }

func (p *staticPortMap) Configure([]portmap.Rule, string) {}
func (p *staticPortMap) Snapshot() portmap.Snapshot       { return portmap.Snapshot{ExternalIP: p.ip} }
func (p *staticPortMap) SetEnabled(bool)                  {}
func (p *staticPortMap) Retry()                           {}

type panicProbe struct{}

func (panicProbe) Lookup(context.Context) (publicip.Result, error) {
	panic("路由器已经有地址了,不该调用外部探测")
}
