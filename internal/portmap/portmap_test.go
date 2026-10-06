package portmap

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/huin/goupnp/soap"
)

// ---------- 外网地址可用性 ----------

func TestExternalIPUsable(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
		why  string // 只用于失败时定位
	}{
		{"112.226.166.178", true, "公网 IPv4"},
		{"100.64.0.1", false, "CGNAT 段首"},
		{"100.127.255.254", false, "CGNAT 段尾"},
		{"10.0.0.5", false, "私网"},
		{"192.168.3.236", false, "私网"},
		{"172.16.0.1", false, "私网"},
		{"127.0.0.1", false, "回环"},
		{"2400:cb00::1", false, "IPv6"},
		{"", false, "空"},
		{"不是地址", false, "解析不了"},
	}
	for _, c := range cases {
		got, why := ExternalIPUsable(c.ip)
		if got != c.want {
			t.Errorf("ExternalIPUsable(%q) = %v(%s),想要 %v —— %s", c.ip, got, why, c.want, c.why)
		}
		if !c.want && why == "" {
			t.Errorf("ExternalIPUsable(%q) 判为不可用却没给原因", c.ip)
		}
	}
}

// 边界:CGNAT 段的相邻地址不属于它。
func TestExternalIPUsableCGNATBoundary(t *testing.T) {
	if ok, _ := ExternalIPUsable("100.63.255.255"); !ok {
		t.Error("100.63.255.255 在 CGNAT 段之外,应当可用")
	}
	if ok, _ := ExternalIPUsable("100.128.0.0"); !ok {
		t.Error("100.128.0.0 在 CGNAT 段之外,应当可用")
	}
}

// ---------- 错误码 ----------

func TestErrorCode(t *testing.T) {
	fault := &soap.SOAPFaultError{}
	fault.Detail.UPnPError.Errorcode = 718

	if got := ErrorCode(fault); got != 718 {
		t.Errorf("ErrorCode = %d,想要 718", got)
	}
	if got := ErrorCode(fmt.Errorf("普通错误")); got != 0 {
		t.Errorf("非 SOAP 错误应当返回 0,得到 %d", got)
	}
	// 包装过的也要能取出来
	if got := ErrorCode(fmt.Errorf("外层: %w", fault)); got != 718 {
		t.Errorf("包装后 ErrorCode = %d,想要 718", got)
	}
	if ErrorCodeName(718) == "" {
		t.Error("718 应当有对应的说明文字")
	}
	if ErrorCodeName(12345) != "" {
		t.Error("未知错误码不该编一个名字出来")
	}
}

// SOAP fault 是路由器的明确答复,不该重试;连接层的错误才该重试。
func TestIsTransient(t *testing.T) {
	fault := &soap.SOAPFaultError{}
	fault.Detail.UPnPError.Errorcode = 501

	if isTransient(fault) {
		t.Error("SOAP fault 不该重试")
	}
	if !isTransient(fmt.Errorf("EOF")) {
		t.Error("连接层错误应当重试")
	}
	if isTransient(nil) {
		t.Error("nil 不该算瞬时错误")
	}
}

// ---------- 续期节奏 ----------

func TestNextCheck(t *testing.T) {
	ok := []RuleStatus{{Mapped: true}, {Mapped: true}}

	// 一小时租约 → 提前 20% 检查,但不超过 15 分钟的上限
	if got := nextCheck(ok, 3600); got != maxCheckInterval {
		t.Errorf("租约 3600 的检查间隔 = %v,想要 %v", got, maxCheckInterval)
	}
	// 短租约 → 按 80% 提前
	if got := nextCheck(ok, 120); got != 96*time.Second {
		t.Errorf("租约 120 的检查间隔 = %v,想要 96s", got)
	}
	// 极短租约 → 不低于下限
	if got := nextCheck(ok, 10); got != minCheckInterval {
		t.Errorf("租约 10 的检查间隔 = %v,想要 %v", got, minCheckInterval)
	}
	// 永久映射 → 固定间隔
	if got := nextCheck(ok, 0); got != permanentCheckInterval {
		t.Errorf("永久映射的检查间隔 = %v,想要 %v", got, permanentCheckInterval)
	}
	// 有没建上的 → 尽快重试
	if got := nextCheck([]RuleStatus{{Mapped: false}}, 3600); got != minCheckInterval {
		t.Errorf("有未建成的条目时 = %v,想要 %v", got, minCheckInterval)
	}
}

// ---------- 规则 ----------

func TestRulesFor(t *testing.T) {
	rs := RulesFor(8889, 8189)
	if len(rs) != 2 {
		t.Fatalf("想要 2 条规则,得到 %d", len(rs))
	}
	for _, r := range rs {
		// 内外端口必须一致 —— MediaMTX 把 UDP 端口写死在 ICE 候选里
		if r.ExternalPort != r.InternalPort {
			t.Errorf("%s 的内外端口不一致(%d / %d)", r.Proto, r.InternalPort, r.ExternalPort)
		}
	}
	if rs[0].Proto != ProtoTCP || rs[0].InternalPort != 8889 {
		t.Errorf("第一条应当是 TCP 8889,得到 %s %d", rs[0].Proto, rs[0].InternalPort)
	}
	if rs[1].Proto != ProtoUDP || rs[1].InternalPort != 8189 {
		t.Errorf("第二条应当是 UDP 8189,得到 %s %d", rs[1].Proto, rs[1].InternalPort)
	}
}

func TestSameIP(t *testing.T) {
	// 路由器回读时带尾随空格是有的,不能因此判成"指向别处"而白重建一次
	if !sameIP("192.168.3.236 ", "192.168.3.236") {
		t.Error("只差空白应当判为同一个地址")
	}
	if !sameIP("::ffff:192.168.3.236", "192.168.3.236") {
		t.Error("IPv4 映射写法应当判为同一个地址")
	}
	if sameIP("192.168.3.236", "192.168.3.237") {
		t.Error("不同地址不该判为相同")
	}
}

// ---------- 状态机 ----------

// fakeSvc 是 service 接口的假实现,用来在没有真路由器的情况下跑状态机。
type fakeSvc struct {
	externalIP string
	mappings   map[string]Mapping
	addErr     error
	delErr     error // 模拟删除失败(路由器断连、超时)
	added      []string
	deleted    []string
}

func newFake(externalIP string) *fakeSvc {
	return &fakeSvc{externalIP: externalIP, mappings: map[string]Mapping{}}
}

func key(proto string, port uint16) string { return fmt.Sprintf("%s/%d", proto, port) }

func (f *fakeSvc) GetExternalIPAddressCtx(context.Context) (string, error) {
	return f.externalIP, nil
}

func (f *fakeSvc) AddPortMappingCtx(_ context.Context, _ string, extPort uint16, proto string,
	intPort uint16, intClient string, enabled bool, desc string, lease uint32) error {
	if f.addErr != nil {
		return f.addErr
	}
	k := key(proto, extPort)
	f.mappings[k] = Mapping{
		Proto: Proto(proto), ExternalPort: int(extPort), InternalPort: int(intPort),
		InternalClient: intClient, Description: desc, LeaseSec: lease, Enabled: enabled,
	}
	f.added = append(f.added, k)
	return nil
}

func (f *fakeSvc) DeletePortMappingCtx(_ context.Context, _ string, extPort uint16, proto string) error {
	if f.delErr != nil {
		return f.delErr
	}
	k := key(proto, extPort)
	delete(f.mappings, k)
	f.deleted = append(f.deleted, k)
	return nil
}

func (f *fakeSvc) GetSpecificPortMappingEntryCtx(_ context.Context, _ string, extPort uint16, proto string,
) (uint16, string, bool, string, uint32, error) {
	m, ok := f.mappings[key(proto, extPort)]
	if !ok {
		fault := &soap.SOAPFaultError{}
		fault.Detail.UPnPError.Errorcode = 714
		return 0, "", false, "", 0, fault
	}
	return uint16(m.InternalPort), m.InternalClient, m.Enabled, m.Description, m.LeaseSec, nil
}

func (f *fakeSvc) GetGenericPortMappingEntryCtx(_ context.Context, index uint16,
) (string, uint16, string, uint16, string, bool, string, uint32, error) {
	if int(index) >= len(f.mappings) {
		fault := &soap.SOAPFaultError{}
		fault.Detail.UPnPError.Errorcode = 713
		return "", 0, "", 0, "", false, "", 0, fault
	}
	i := 0
	for _, m := range f.mappings {
		if i == int(index) {
			return "", uint16(m.ExternalPort), string(m.Proto), uint16(m.InternalPort),
				m.InternalClient, m.Enabled, m.Description, m.LeaseSec, nil
		}
		i++
	}
	return "", 0, "", 0, "", false, "", 0, fmt.Errorf("越界")
}

func fakeGateway(svc service) *Gateway {
	return &Gateway{
		FriendlyName: "测试路由器",
		ServiceKind:  "WANIPConnection:1",
		LocalAddr:    net.ParseIP("192.168.1.5"),
		svc:          svc,
	}
}

// newIdle 构造一个不跑后台协程的 Mapper。
//
// 测试要直接驱动 sync() 看它做了什么,而后台协程会并发改同一份快照 ——
// 两边抢起来,断言就成了碰运气。生产路径走 New(),那边一定会起协程。
func newIdle(o Options) *Mapper {
	m := newMapper(o)
	close(m.ended) // 没有协程要等,Close 立即返回
	return m
}

func TestNewStartsAndCloseStops(t *testing.T) {
	// 生产路径的关键性质:New 起协程,Close 能等到它退出(不然就是永久卡死)。
	m := New(Options{InternalIP: "192.168.1.5", Rules: RulesFor(8889, 8189)})
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close 没有返回 —— 后台协程没退出来")
	}
}

// 顺利路径:发现路由器 → 两条映射都建上 → 状态是 active。
func TestSyncBuildsMappings(t *testing.T) {
	svc := newFake("203.0.113.7")
	gw := fakeGateway(svc)

	var changed []string
	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesFor(8889, 8189),
		OnChange:   func(ip string) { changed = append(changed, ip) },
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	wait := m.sync(&runState{})
	snap := m.Snapshot()

	if snap.State != StateActive {
		t.Fatalf("状态 = %s,想要 active(消息:%s)", snap.State, snap.Message)
	}
	if snap.ExternalIP != "203.0.113.7" {
		t.Errorf("外网地址 = %q", snap.ExternalIP)
	}
	if len(svc.added) != 2 {
		t.Fatalf("应当建了两条映射,实际 %v", svc.added)
	}
	if len(snap.Rules) != 2 || !snap.Rules[0].Mapped || !snap.Rules[1].Mapped {
		t.Errorf("两条规则都该标为已建立:%+v", snap.Rules)
	}
	// 第一次拿到地址也要通知 —— 观看链接要靠它
	if len(changed) != 1 || changed[0] != "203.0.113.7" {
		t.Errorf("OnChange 调用 = %v,想要一次 203.0.113.7", changed)
	}
	if wait <= 0 {
		t.Errorf("下次检查间隔 = %v,应当为正", wait)
	}

	// 再跑一遍:映射已经对了,不该重复建
	m.sync(&runState{gateway: gw, externalIP: "203.0.113.7", mapped: true})
	if len(svc.added) != 2 {
		t.Errorf("第二次 sync 不该重复建立映射,累计 %v", svc.added)
	}
	if len(changed) != 1 {
		t.Errorf("地址没变不该再次通知,实际 %v", changed)
	}
}

// 冲突:路由器说 718,状态必须是 failed 并给出可操作的建议。
func TestSyncConflictFails(t *testing.T) {
	svc := newFake("203.0.113.7")
	fault := &soap.SOAPFaultError{}
	fault.Detail.UPnPError.Errorcode = 718
	svc.addErr = fault
	gw := fakeGateway(svc)

	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesFor(8889, 8189),
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	m.sync(&runState{})
	snap := m.Snapshot()

	if snap.State != StateFailed {
		t.Fatalf("状态 = %s,想要 failed", snap.State)
	}
	if !containsAny(snap.Message, "冲突") {
		t.Errorf("消息应当说明冲突,实际 %q", snap.Message)
	}
	if snap.Hint == "" {
		t.Error("失败时必须给用户一条下一步怎么办的提示")
	}
}

// 找不到路由器(UPnP 关着)不能算崩溃,要给出手动配置的指引。
func TestSyncNoGateway(t *testing.T) {
	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesFor(8889, 8189),
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return nil, nil
		},
	})

	wait := m.sync(&runState{})
	snap := m.Snapshot()

	if snap.State != StateFailed {
		t.Fatalf("状态 = %s,想要 failed", snap.State)
	}
	if !containsAny(snap.Message, "没有发现") {
		t.Errorf("消息 = %q", snap.Message)
	}
	// 必须有退避,否则会疯狂重试把路由器打爆
	if wait != discoverBackoff[0] {
		t.Errorf("首次失败的退避 = %v,想要 %v", wait, discoverBackoff[0])
	}
}

// CGNAT 下映射建了也没用,应当直接判失败而不是假装成功。
func TestSyncCGNATFails(t *testing.T) {
	svc := newFake("100.64.3.9")
	gw := fakeGateway(svc)

	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesFor(8889, 8189),
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	m.sync(&runState{})
	snap := m.Snapshot()

	if snap.State != StateFailed {
		t.Fatalf("CGNAT 下状态 = %s,想要 failed", snap.State)
	}
	if len(svc.added) != 0 {
		t.Errorf("判定外网地址不可用后不该再去建映射,实际建了 %v", svc.added)
	}
	if !containsAny(snap.Hint, "VPS") {
		t.Errorf("CGNAT 的提示应当指向 VPS/穿透,实际 %q", snap.Hint)
	}
}

// 本机内网地址变了(换 WiFi),旧映射指向别处,必须重建。
func TestSyncRebuildsWhenInternalAddressMoves(t *testing.T) {
	svc := newFake("203.0.113.7")
	gw := fakeGateway(svc)

	m := newIdle(Options{
		InternalIP: "192.168.9.9", // 新的地址
		Rules:      RulesFor(8889, 8189),
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	// 路由器上留着指向旧地址的条目
	svc.mappings["TCP/8889"] = Mapping{
		Proto: ProtoTCP, ExternalPort: 8889, InternalPort: 8889,
		InternalClient: "192.168.1.5", Enabled: true,
	}

	m.sync(&runState{})
	snap := m.Snapshot()

	if snap.State != StateActive {
		t.Fatalf("状态 = %s,想要 active", snap.State)
	}
	if len(svc.added) == 0 {
		t.Fatal("指向旧地址的条目应当被重建")
	}
	if svc.mappings["TCP/8889"].InternalClient != "192.168.9.9" {
		t.Errorf("重建后仍指向 %q", svc.mappings["TCP/8889"].InternalClient)
	}
}

// 公网地址变了要通知一次,而且只通知一次。
func TestOnChangeFiresOncePerAddress(t *testing.T) {
	svc := newFake("203.0.113.7")
	gw := fakeGateway(svc)

	var changed []string
	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesFor(8889, 8189),
		OnChange:   func(ip string) { changed = append(changed, ip) },
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	st := &runState{}
	m.sync(st)
	m.sync(st)
	if len(changed) != 1 {
		t.Fatalf("地址没变却通知了 %d 次:%v", len(changed), changed)
	}

	svc.externalIP = "198.51.100.4"
	st.gateway = nil // 强制重新走一遍发现
	m.sync(st)

	if len(changed) != 2 || changed[1] != "198.51.100.4" {
		t.Errorf("地址变化应当再通知一次,实际 %v", changed)
	}
}

// 关闭功能要撤掉映射;但 Close() 不撤 —— 崩溃时撤不掉,行为必须一致。
func TestDisableDeletesButCloseDoesNot(t *testing.T) {
	svc := newFake("203.0.113.7")
	gw := fakeGateway(svc)

	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesFor(8889, 8189),
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	m.sync(&runState{})
	if len(svc.mappings) != 2 {
		t.Fatalf("应当先建好两条,实际 %d", len(svc.mappings))
	}

	m.Close()
	if len(svc.deleted) != 0 {
		t.Errorf("Close 不该删除映射,实际删了 %v", svc.deleted)
	}

	m.deleteAll(gw)
	if len(svc.deleted) != 2 {
		t.Errorf("撤销应当删掉两条,实际 %v", svc.deleted)
	}
}

// 快照里的切片必须是副本 —— 否则调用方拿到的数组会被后台协程改掉。
func TestSnapshotCopiesRules(t *testing.T) {
	m := newIdle(Options{Rules: RulesFor(8889, 8189)})
	m.update(func(s *Snapshot) {
		s.Rules = []RuleStatus{{Proto: "TCP", Port: 8889}}
	})

	got := m.Snapshot()
	got.Rules[0].Port = 1234

	if m.Snapshot().Rules[0].Port != 8889 {
		t.Error("改快照返回值影响到了内部状态,说明没有复制")
	}
}

// ---------- 规则被移除时的撤销 ----------
//
// 这一组盯的是"远控端口和 UPnP 同步"的另一半:端口进了规则要建,
// 从规则里消失也要撤。少了撤销那一半,关掉远控之后 TCP 8090 会一直
// 留在路由器上对着公网开着,直到租约到期 —— 而租约是路由器给的。

// 远控关掉之后,它在路由器上的映射必须被撤掉。
func TestSyncDeletesRemovedRule(t *testing.T) {
	svc := newFake("203.0.113.7")
	gw := fakeGateway(svc)

	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesForWithControl(8889, 8189, 8090),
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	st := &runState{}
	m.sync(st)
	if _, ok := svc.mappings[key("TCP", 8090)]; !ok {
		t.Fatalf("前提不成立:远控端口没建上,实际 %v", svc.mappings)
	}

	// 用户关掉远控:规则里只剩观看那两条
	m.Configure(RulesForWithControl(8889, 8189, 0), "192.168.1.5")
	m.sync(st)

	if _, ok := svc.mappings[key("TCP", 8090)]; ok {
		t.Errorf("远控关掉之后 TCP 8090 还留在路由器上 —— 这是一个一直对着公网开着的洞。已删:%v",
			svc.deleted)
	}
	// 观看那两条不能被牵连
	if _, ok := svc.mappings[key("TCP", 8889)]; !ok {
		t.Error("观看端口的映射不该跟着被删掉")
	}
	if _, ok := svc.mappings[key("UDP", 8189)]; !ok {
		t.Error("媒体端口的映射不该跟着被删掉")
	}
}

// 关掉又马上打开,映射不能被自己撤掉。
//
// Configure 在远控开关上会被连着调两次(关一次、开一次),中间后台协程
// 可能一轮都没跑。待删队列不认"又加回来了",结果就是刚建好就删掉。
func TestConfigureCancelsDeleteWhenRuleComesBack(t *testing.T) {
	svc := newFake("203.0.113.7")
	gw := fakeGateway(svc)

	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesForWithControl(8889, 8189, 8090),
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	st := &runState{}
	m.sync(st)

	m.Configure(RulesForWithControl(8889, 8189, 0), "192.168.1.5")    // 关掉远控
	m.Configure(RulesForWithControl(8889, 8189, 8090), "192.168.1.5") // 又打开

	m.sync(st)

	if _, ok := svc.mappings[key("TCP", 8090)]; !ok {
		t.Errorf("关掉又打开之后映射被自己撤掉了。已删:%v", svc.deleted)
	}
}

// 撤销失败要重试,不能把那个洞忘了。
func TestDeleteRemovedRetriesOnFailure(t *testing.T) {
	svc := newFake("203.0.113.7")
	gw := fakeGateway(svc)

	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesForWithControl(8889, 8189, 8090),
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	st := &runState{}
	m.sync(st)

	// 关掉远控,但这一轮路由器断连,删不掉
	svc.delErr = fmt.Errorf("连接被重置")
	m.Configure(RulesForWithControl(8889, 8189, 0), "192.168.1.5")
	m.sync(st)

	if _, ok := svc.mappings[key("TCP", 8090)]; !ok {
		t.Fatal("前提不成立:删除应该失败,条目还该在")
	}

	// 路由器恢复。下一轮必须把它补删掉,而不是当没这回事
	svc.delErr = nil
	m.sync(st)

	if _, ok := svc.mappings[key("TCP", 8090)]; ok {
		t.Errorf("上一轮删失败之后没有重试,TCP 8090 永远留在路由器上了。已删:%v", svc.deleted)
	}
}

// 改远控端口:旧端口的映射要撤掉,新端口要建上。
func TestSyncFollowsRemoteControlPortChange(t *testing.T) {
	svc := newFake("203.0.113.7")
	gw := fakeGateway(svc)

	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesForWithControl(8889, 8189, 8090),
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	st := &runState{}
	m.sync(st)

	m.Configure(RulesForWithControl(8889, 8189, 8091), "192.168.1.5")
	m.sync(st)

	if _, ok := svc.mappings[key("TCP", 8090)]; ok {
		t.Error("改了远控端口之后,旧端口的映射还留在路由器上")
	}
	if _, ok := svc.mappings[key("TCP", 8091)]; !ok {
		t.Error("新的远控端口没建上")
	}
}

// 提示语里的端口必须按实际规则生成,不能写死前两条。
//
// 远控端口只在远控开着时才是第三条规则。提示漏掉它的后果是:
// 用户照着一份看起来完整的说明放行完防火墙,然后面对"公网上能看画面、
// 但控制不了" —— 而没有任何地方告诉他少了什么。
func TestPortList(t *testing.T) {
	tests := []struct {
		name  string
		rules []Rule
		want  string
	}{
		{"远控关着 —— 两条", RulesFor(8889, 8189), "8889/tcp 和 8189/udp"},
		{"远控开着 —— 三条", RulesForWithControl(8889, 8189, 8090), "8889/tcp、8189/udp 和 8090/tcp"},
		{"一条", RulesFor(8889, 0)[:1], "8889/tcp"},
		{"没有规则", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := portList(tc.rules); got != tc.want {
				t.Errorf("portList = %q,想要 %q", got, tc.want)
			}
		})
	}
}

// 失败路径的手动配置提示要提到远控端口,条数也要对。
func TestManualHintMentionsRemoteControlPort(t *testing.T) {
	rules := RulesForWithControl(8889, 8189, 8090)
	got := manualHint("路由器返回错误码 402", rules)

	if !strings.Contains(got, "8090") {
		t.Errorf("提示里没有远控端口:%q", got)
	}
	if !strings.Contains(got, "3 条") {
		t.Errorf("提示里的条数不是规则条数:%q", got)
	}
}

// 成功路径的防火墙提示同样要提到远控端口。
func TestActiveHintMentionsRemoteControlPort(t *testing.T) {
	svc := newFake("203.0.113.7")
	gw := fakeGateway(svc)

	m := newIdle(Options{
		InternalIP: "192.168.1.5",
		Rules:      RulesForWithControl(8889, 8189, 8090),
		DiscoverFn: func(context.Context, time.Duration) ([]*Gateway, error) {
			return []*Gateway{gw}, nil
		},
	})

	m.sync(&runState{})
	snap := m.Snapshot()
	if snap.State != StateActive {
		t.Fatalf("状态 = %s,想要 active(消息:%s)", snap.State, snap.Message)
	}
	if !strings.Contains(snap.Hint, "8090") {
		t.Errorf("防火墙提示里没有远控端口:%q", snap.Hint)
	}
}
