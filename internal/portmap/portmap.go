// Package portmap 通过 UPnP IGD 让程序自己在路由器上开端口,省掉手动配映射那一步。
//
// 这一层只做"和一台已发现的路由器说话":发现、查外网地址、增删查映射。
// 什么时候做、失败了怎么办,是 mapper.go 的事。
//
// # 为什么四种客户端都要试
//
// UPnP 的 WAN 连接服务有四个变体:WANIPConnection v1/v2 和 WANPPPConnection v1,
// 外加它们分布在 internetgateway1 / internetgateway2 两个包里。路由器只提供
// 其中一种,提供哪种取决于 WAN 口怎么拨号 —— **不是版本越新越可能命中**。
//
// 实测(华为路由 AX3 Pro,WS7200-10)只提供 WANPPPConnection:1,而
// WANIPConnection v1/v2 全部不响应。只接 WANIPConnection 的话,在这台设备上
// 会直接失败,而且失败得很难查 —— 发现阶段能探到设备,一问服务就没有。
package portmap

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/soap"
)

// Proto 是映射的协议。取值就是 IGD 期望的字面量,大写。
type Proto string

const (
	ProtoTCP Proto = "TCP"
	ProtoUDP Proto = "UDP"
)

// soapTimeout 是单次 SOAP 请求的上限。
//
// 必须显式设:goupnp 的 soap.NewSOAPClient 不设超时,零值 http.Client 等于
// 永不超时 —— 一台半死的路由器足以把我们的协程永久挂住。
const soapTimeout = 5 * time.Second

// rpcAttempts 是单次 RPC 的尝试次数。
//
// 需要重试是因为实测遇到过:同一台华为路由器会在 SOAP 请求上偶发地直接断开
// HTTP 连接(报 EOF),7 次请求里出现 2 次,重发一次就成功。这不是我们的参数
// 有问题,所以不能当成失败报给用户。
const rpcAttempts = 3

// Rule 是一条要建立的映射。
type Rule struct {
	Proto Proto

	// InternalPort 是本机监听的端口。
	InternalPort int

	// ExternalPort 是路由器对公网开放的端口。
	//
	// 恒等于 InternalPort,这是硬约束而不是偏好:MediaMTX 把 UDP 端口写死在
	// ICE 候选里(webrtcAdditionalHosts 只接受主机名,端口由它自己补成
	// webrtcLocalUDPAddress 的端口)。外网端口一旦不同,浏览器拿到的候选地址
	// 就指向一个没人监听的端口。
	ExternalPort int

	Description string
}

// RulesFor 由 MediaMTX 的两个对外端口派生映射规则。
//
// 只映射这两个。推流入口(8554/1935)在任何情况下都不能映射出去 ——
// 那等于让任何人都能覆盖你的画面。
func RulesFor(webrtcTCP, mediaUDP int) []Rule {
	return []Rule{
		{Proto: ProtoTCP, InternalPort: webrtcTCP, ExternalPort: webrtcTCP, Description: "ShareScreen-WHEP"},
		{Proto: ProtoUDP, InternalPort: mediaUDP, ExternalPort: mediaUDP, Description: "ShareScreen-ICE"},
	}
}

func (r Rule) extPort() uint16 { return uint16(r.ExternalPort) }
func (r Rule) intPort() uint16 { return uint16(r.InternalPort) }

// Mapping 是路由器上的一条映射记录。
type Mapping struct {
	Proto          Proto
	ExternalPort   int
	InternalPort   int
	InternalClient string
	Description    string
	LeaseSec       uint32
	Enabled        bool
}

// service 是四种 WAN 连接客户端共有的方法集。
//
// 四个变体由同一个代码生成器产出,方法签名逐字相同,所以一个接口就能盖住。
// 接口的另一个好处是测试里可以塞假实现。
type service interface {
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	AddPortMappingCtx(ctx context.Context, remoteHost string, extPort uint16, protocol string,
		intPort uint16, intClient string, enabled bool, description string, leaseSec uint32) error
	DeletePortMappingCtx(ctx context.Context, remoteHost string, extPort uint16, protocol string) error
	GetSpecificPortMappingEntryCtx(ctx context.Context, remoteHost string, extPort uint16, protocol string,
	) (intPort uint16, intClient string, enabled bool, description string, leaseSec uint32, err error)
	GetGenericPortMappingEntryCtx(ctx context.Context, index uint16,
	) (remoteHost string, extPort uint16, protocol string, intPort uint16, intClient string,
		enabled bool, description string, leaseSec uint32, err error)
}

// Gateway 是一台已发现的、且至少提供一种 WAN 连接服务的路由器。
type Gateway struct {
	FriendlyName string
	Manufacturer string
	ModelName    string

	// Location 是设备描述 XML 的地址,排查时用得上。
	Location string

	// LocalAddr 是本机通往这台路由器的网卡地址。映射必须指向它 ——
	// 多网卡机器上挑错地址,映射建到别的网段就是死条目。
	LocalAddr net.IP

	// ServiceKind 说明命中的是四个变体里的哪一个,例如 "WANPPPConnection:1"。
	// 实测不同路由器给的是不同变体,排障时这一条最有用。
	ServiceKind string

	svc service

	// retryMu 串行化发往同一台设备的 RPC。
	// 华为那台在并发请求下更容易断连,而我们本来也不需要并发。
	retryMu sync.Mutex
}

// 四个服务变体,按尝试顺序排列。
//
// IP 在前 PPP 在后只是习惯 —— 两者都得试,因为命中哪个取决于路由器而不是版本。
var serviceKinds = []struct {
	urn   string
	label string
	build func(*goupnp.RootDevice, *url.URL) (service, error)
}{
	{
		urn:   internetgateway2.URN_WANIPConnection_2,
		label: "WANIPConnection:2",
		build: func(r *goupnp.RootDevice, loc *url.URL) (service, error) {
			cs, err := internetgateway2.NewWANIPConnection2ClientsFromRootDevice(r, loc)
			return first(cs, err)
		},
	},
	{
		urn:   internetgateway2.URN_WANIPConnection_1,
		label: "WANIPConnection:1",
		build: func(r *goupnp.RootDevice, loc *url.URL) (service, error) {
			cs, err := internetgateway2.NewWANIPConnection1ClientsFromRootDevice(r, loc)
			return first(cs, err)
		},
	},
	{
		urn:   internetgateway1.URN_WANIPConnection_1,
		label: "WANIPConnection:1",
		build: func(r *goupnp.RootDevice, loc *url.URL) (service, error) {
			cs, err := internetgateway1.NewWANIPConnection1ClientsFromRootDevice(r, loc)
			return first(cs, err)
		},
	},
	{
		urn:   internetgateway1.URN_WANPPPConnection_1,
		label: "WANPPPConnection:1",
		build: func(r *goupnp.RootDevice, loc *url.URL) (service, error) {
			cs, err := internetgateway1.NewWANPPPConnection1ClientsFromRootDevice(r, loc)
			return first(cs, err)
		},
	},
}

func first[T any](cs []T, err error) (service, error) {
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, errors.New("没有该服务的客户端")
	}
	// 这四个具体类型都内嵌 goupnp.ServiceClient,类型断言成接口
	return any(cs[0]).(service), nil
}

// 发现时用的搜索目标。服务类型比设备类型更直接;设备类型作为兜底,
// 因为少数路由器的 SSDP 只应答设备类型,不应答服务类型。
var searchTargets = []string{
	internetgateway2.URN_WANIPConnection_2,
	internetgateway2.URN_WANIPConnection_1,
	internetgateway1.URN_WANIPConnection_1,
	internetgateway1.URN_WANPPPConnection_1,
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
}

// Discover 用 SSDP 找局域网里的路由器,返回其中提供了 WAN 连接服务的那些。
//
// timeout 是整个搜索的等待时长,不是每个目标的 —— 六个搜索目标是并发发出的。
// 串行发的话最坏要等 6 倍时间,而控制页每 2 秒轮询一次,"正在查找"会挂在那儿
// 十几秒,看起来像卡死了。
//
// 一台都找不到不是错误:绝大多数家用路由器默认关着 UPnP,这是常态而不是故障。
// 返回空列表,由调用方决定怎么告诉用户。
func Discover(ctx context.Context, timeout time.Duration) ([]*Gateway, error) {
	type found struct {
		root *goupnp.RootDevice
		loc  *url.URL
		addr net.IP
	}

	var (
		mu      sync.Mutex
		seen    = map[string]bool{}
		devices []found
		wg      sync.WaitGroup
	)

	// 多个搜索目标常常命中同一台设备(同一个 location),靠 seen 去重。
	// 加锁范围很小,只是为了读写这个 map。
	for _, target := range searchTargets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()

			sub, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			maybes, err := goupnp.DiscoverDevicesCtx(sub, target)
			if err != nil {
				return // 单个目标探不到很正常
			}

			mu.Lock()
			defer mu.Unlock()
			for _, m := range maybes {
				if m.Err != nil || m.Root == nil || m.Location == nil {
					continue
				}
				key := m.Location.String()
				if seen[key] {
					continue
				}
				seen[key] = true
				devices = append(devices, found{root: m.Root, loc: m.Location, addr: m.LocalAddr})
			}
		}(target)
	}
	wg.Wait()

	var out []*Gateway
	for _, d := range devices {
		if g := buildGateway(d.root, d.loc, d.addr); g != nil {
			out = append(out, g)
		}
	}
	return out, nil
}

// buildGateway 在一台设备上找出第一个可用的 WAN 连接服务;一个都没有就返回 nil。
func buildGateway(root *goupnp.RootDevice, loc *url.URL, localAddr net.IP) *Gateway {
	for _, k := range serviceKinds {
		svc, err := k.build(root, loc)
		if err != nil {
			continue
		}
		sc := any(svc).(interface{ GetServiceClient() *goupnp.ServiceClient }).GetServiceClient()
		// 不设超时的话,一台半死的路由器能把调用方永久挂住
		sc.SOAPClient.HTTPClient = http.Client{Timeout: soapTimeout}

		addr := localAddr
		if addr == nil {
			// SSDP 没告诉我们应答来自哪个网卡时,自己问一次路由表:
			// 朝网关地址拨一个 UDP 包(不发出去)就能拿到出口地址。
			addr = localAddrToward(loc.Hostname())
		}

		d := &root.Device
		return &Gateway{
			FriendlyName: d.FriendlyName,
			Manufacturer: d.Manufacturer,
			ModelName:    d.ModelName,
			Location:     loc.String(),
			LocalAddr:    addr,
			ServiceKind:  k.label,
			svc:          svc,
		}
	}
	return nil
}

// localAddrToward 返回本机通往 target 的网卡地址。失败返回 nil。
//
// UDP 的 Dial 不实际发包,只让内核按路由表选一个出口地址 —— 正是我们要的。
func localAddrToward(target string) net.IP {
	if target == "" {
		return nil
	}
	conn, err := net.Dial("udp", net.JoinHostPort(target, "9"))
	if err != nil {
		return nil
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP
	}
	return nil
}

// ExternalIP 返回路由器自己报告的公网地址。
func (g *Gateway) ExternalIP(ctx context.Context) (string, error) {
	return retry1(ctx, func() (string, error) {
		return g.svc.GetExternalIPAddressCtx(ctx)
	})
}

// GetMapping 查一条映射。条目不存在时返回的 error 可以用 ErrorCode 判出 714。
func (g *Gateway) GetMapping(ctx context.Context, r Rule) (Mapping, error) {
	return retry1(ctx, func() (Mapping, error) {
		intPort, intClient, enabled, desc, lease, err :=
			g.svc.GetSpecificPortMappingEntryCtx(ctx, "", r.extPort(), string(r.Proto))
		if err != nil {
			return Mapping{}, err
		}
		return Mapping{
			Proto:          r.Proto,
			ExternalPort:   r.ExternalPort,
			InternalPort:   int(intPort),
			InternalClient: intClient,
			Description:    desc,
			LeaseSec:       lease,
			Enabled:        enabled,
		}, nil
	})
}

// ListMappings 把路由器上的映射表整个拉下来,最多 max 条。
//
// 路由器用"索引越界即报错"来表示列表结束,所以这里靠错误来终止循环。
// 拿不到完整列表不算致命 —— 排查时能看多少看多少。
func (g *Gateway) ListMappings(ctx context.Context, max int) ([]Mapping, error) {
	var out []Mapping
	for i := 0; i < max; i++ {
		m, err := retry1(ctx, func() (Mapping, error) {
			_, extPort, proto, intPort, intClient, enabled, desc, lease, err :=
				g.svc.GetGenericPortMappingEntryCtx(ctx, uint16(i))
			if err != nil {
				return Mapping{}, err
			}
			return Mapping{
				Proto:          Proto(proto),
				ExternalPort:   int(extPort),
				InternalPort:   int(intPort),
				InternalClient: intClient,
				Description:    desc,
				LeaseSec:       lease,
				Enabled:        enabled,
			}, nil
		})
		if err != nil {
			if i == 0 {
				return nil, err
			}
			return out, nil // 越界 = 列表结束,这是正常终止
		}
		out = append(out, m)
	}
	return out, nil
}

// AddMapping 建立一条映射,返回路由器实际给的租约秒数。
//
// internalClient 是映射要指向的本机内网地址。
//
// 关于租约:请求的 leasSec 会被路由器按自己的上限截断,所以返回值才是真的。
// 返回 0 表示路由器给的是永久映射。实测华为那台会原样接受请求值。
func (g *Gateway) AddMapping(ctx context.Context, r Rule, internalClient string, leaseSec uint32) (uint32, error) {
	err := g.addOnce(ctx, r, internalClient, leaseSec)
	if err != nil {
		// 725:路由器只收永久映射(租约 0)。这不是错误,换一种说法再问一次。
		if ErrorCode(err) == codeOnlyPermanentLeases {
			if e2 := g.addOnce(ctx, r, internalClient, 0); e2 == nil {
				return 0, nil
			}
			return 0, err
		}
		return 0, err
	}

	// 回读一次,拿路由器真正给的租约。拿不到就按请求值算,不值得因此失败。
	m, gerr := g.GetMapping(ctx, r)
	if gerr != nil {
		return leaseSec, nil
	}
	return m.LeaseSec, nil
}

func (g *Gateway) addOnce(ctx context.Context, r Rule, internalClient string, leaseSec uint32) error {
	_, err := retry1(ctx, func() (struct{}, error) {
		return struct{}{}, g.svc.AddPortMappingCtx(ctx, "", r.extPort(), string(r.Proto),
			r.intPort(), internalClient, true, r.Description, leaseSec)
	})
	return err
}

// DeleteMapping 删除一条映射。条目本就不存在时不算失败。
func (g *Gateway) DeleteMapping(ctx context.Context, r Rule) error {
	_, err := retry1(ctx, func() (struct{}, error) {
		return struct{}{}, g.svc.DeletePortMappingCtx(ctx, "", r.extPort(), string(r.Proto))
	})
	if ErrorCode(err) == codeNoSuchEntry {
		return nil
	}
	return err
}

// ---------- UPnP 错误码 ----------

// IGD 规范定义的错误码。只列我们用得上的。
const (
	codeActionFailed        = 501
	codeActionNotAuthorized = 606
	codeNoSuchEntry         = 714 // 条目不存在
	codeConflict            = 718 // 与其他条目冲突
	codeSamePortValues      = 724 // 内外端口必须相同
	codeOnlyPermanentLeases = 725 // 只支持永久租约
	codeExtPortWildcardOnly = 727 // 外部端口只支持通配
)

// ErrorCode 从错误里取出 UPnP 错误码;不是 SOAP fault 时返回 0。
//
// 取结构体字段而不是匹配错误文本 —— 措辞会随固件和语言变,错误码不会。
func ErrorCode(err error) int {
	var fault *soap.SOAPFaultError
	if errors.As(err, &fault) {
		return fault.Detail.UPnPError.Errorcode
	}
	return 0
}

// ErrorCodeName 给错误码一个人话名字,界面和日志都用它。
func ErrorCodeName(code int) string {
	switch code {
	case codeActionFailed:
		return "路由器拒绝了这个请求(ActionFailed)"
	case codeActionNotAuthorized:
		return "路由器不允许这个操作(ActionNotAuthorized)"
	case codeNoSuchEntry:
		return "映射条目不存在"
	case codeConflict:
		return "与其他端口映射冲突"
	case codeSamePortValues:
		return "路由器要求内外端口相同"
	case codeOnlyPermanentLeases:
		return "路由器只支持永久映射"
	case codeExtPortWildcardOnly:
		return "路由器只接受通配的外部端口"
	}
	return ""
}

// ---------- 重试 ----------

// isTransient 判断这个错误值不值得重发。
//
// SOAP fault 是路由器给出的明确答复 —— 参数不对就是不对,重试一百次也一样。
// 其余错误(连接被断、超时、读不到响应)大多是瞬时的:实测华为路由 AX3 Pro
// 会偶发地在 SOAP 请求上直接断开 HTTP 连接,重发一次即成功。
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var fault *soap.SOAPFaultError
	return !errors.As(err, &fault)
}

// retry1 执行一次 RPC,瞬时错误重试。
func retry1[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var (
		zero T
		err  error
	)
	for i := 0; i < rpcAttempts; i++ {
		if i > 0 {
			if !sleepCtx(ctx, time.Duration(i)*300*time.Millisecond) {
				return zero, err
			}
		}
		var v T
		if v, err = fn(); err == nil {
			return v, nil
		}
		if !isTransient(err) {
			return zero, err
		}
	}
	return zero, err
}

// sleepCtx 睡一小会儿;ctx 提前结束则返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ---------- 外网地址可用性 ----------

// cgnatNet 是运营商级 NAT 的地址段(RFC 6598)。
//
// 必须单独判断:net.IP.IsPrivate() **不包含**这一段,而它恰恰是家用宽带最
// 常见的"看起来像公网、其实映射不出去"的地址。
var cgnatNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// ExternalIPUsable 判断路由器报告的外网地址能不能真的被公网访问到。
//
// 返回的第二项是不可用的原因,可以直接给用户看。
//
// 只接受 IPv4:这条链路上 MediaMTX 的 ICE 候选、观看链接拼接都是按 IPv4 验证
// 过的,IPv6 没有实测过,不猜。
func ExternalIPUsable(s string) (bool, string) {
	ip := net.ParseIP(s)
	if ip == nil || s == "" {
		return false, "路由器没有报告外网地址"
	}
	if ip.To4() == nil {
		return false, "路由器报告的是 IPv6 地址,当前版本不支持"
	}
	if ip.IsLoopback() {
		return false, "路由器报告的是回环地址"
	}
	if cgnatNet.Contains(ip) {
		return false, "外网地址 " + s + " 属于运营商级 NAT(CGNAT),端口映射对公网无效"
	}
	if ip.IsPrivate() {
		return false, "外网地址 " + s + " 是内网地址,说明外面还有一层 NAT"
	}
	return true, ""
}

// HostPort 把外网地址和端口拼成给用户看的主机串。
func HostPort(ip string, port int) string {
	return net.JoinHostPort(ip, strconv.Itoa(port))
}
