package portmap

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// State 是自动映射的运行状态。
type State string

const (
	// StateDisabled 表示用户没开这个功能,或者刚把它关掉。程序不碰网络。
	StateDisabled State = "disabled"
	// StateDiscovering 表示正在找路由器。
	StateDiscovering State = "discovering"
	// StateActive 表示映射已经建好,路由器接受了。
	StateActive State = "active"
	// StateFailed 表示这一步走不通,需要用户手动配。Message 里说明卡在哪。
	StateFailed State = "failed"
)

// RuleStatus 是单条映射的状态,给界面逐条显示用。
type RuleStatus struct {
	Proto  string `json:"proto"`
	Label  string `json:"label"`
	Port   int    `json:"port"`
	Mapped bool   `json:"mapped"`
	Error  string `json:"error,omitempty"`
}

// Snapshot 是状态的只读副本。
//
// 之所以要"快照"而不是直接暴露 Mapper:这些字段由后台协程随时改写,而读它的人
// 在 HTTP 处理器里。复制一份出去,双方都不必迁就对方的加锁时机。
type Snapshot struct {
	State   State  `json:"state"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`

	ExternalIP string `json:"externalIp,omitempty"`
	Gateway    string `json:"gateway,omitempty"`
	// ServiceKind 是命中的 UPnP 服务变体,排障时最有用的一条。
	ServiceKind string `json:"serviceKind,omitempty"`
	// GatewayCount 大于 1 意味着可能有多重 NAT。
	GatewayCount int `json:"gatewayCount,omitempty"`

	Rules []RuleStatus `json:"rules,omitempty"`

	UpdatedAt time.Time `json:"updatedAt"`
}

// Options 是构造 Mapper 需要的参数。
type Options struct {
	// InternalIP 是本机在局域网里的地址,映射要指向它。
	InternalIP string

	// Rules 是要建立的映射。
	Rules []Rule

	// LeaseSec 是请求的租约。0 表示用默认值。
	LeaseSec uint32

	// OnChange 在公网地址变化时被调用(包括第一次拿到)。
	//
	// 调用发生在 Mapper 自己的协程里,所以回调里可以放心做慢操作 ——
	// 它不会挡住 HTTP 请求。但也要注意:回调超时会让续期循环推迟。
	OnChange func(externalIP string)

	// DiscoverFn 用于测试注入。为 nil 时用 Discover。
	DiscoverFn func(ctx context.Context, timeout time.Duration) ([]*Gateway, error)
}

// 重试节奏。
const (
	// discoverTimeout 是每次找路由器的等待时长。
	discoverTimeout = 3 * time.Second
	// defaultLeaseSec 是默认请求的租约,1 小时。
	defaultLeaseSec = 3600
	// minCheckInterval 是两次检查之间的最短间隔。
	minCheckInterval = 60 * time.Second
	// maxCheckInterval 是两次检查之间的最长间隔。
	maxCheckInterval = 15 * time.Minute
	// permanentCheckInterval 用于路由器给了永久映射(租约 0)的情况。
	// 没有到期时间可依据,只能定期确认条目还在。
	permanentCheckInterval = 10 * time.Minute
)

// discoverBackoff 是找不到路由器时的重试节奏。一直重试而不是放弃 ——
// 路由器可能重启,用户也可能刚刚才把 UPnP 开关打开。
var discoverBackoff = []time.Duration{
	30 * time.Second,
	time.Minute,
	5 * time.Minute,
}

// Mapper 在后台维持一组端口映射。
//
// 零值不可用,用 New 构造。
type Mapper struct {
	opts Options

	mu       sync.RWMutex
	snap     Snapshot
	enabled  bool
	internal string
	rules    []Rule

	// removed 是曾经在 rules 里、后来被 Configure 移掉、但还没到路由器上
	// 撤销的规则。
	//
	// 必须有这本账:Configure 只表达"现在要开哪些",不会替我们删。
	// 少了它,关掉远控(端口从规则里消失)或改远控端口之后,那条映射会
	// 一直留在路由器上对着公网开着,直到租约到期 —— 而租约是路由器给的,
	// 可能很长。这直接违背"不开启时公网上不会多出任何指向本机的洞"。
	removed []Rule

	wake  chan struct{}
	done  chan struct{}
	ended chan struct{}

	closeOnce sync.Once
}

// New 构造一个 Mapper,并把后台协程挂起来。
//
// 协程在这里就启动,而不是另开一个 Start() 让调用方记得调 ——
// 忘了调 Start 的话,Close 会永远等一个不会退出的协程,而这是个只会
// 卡死、不会报错的失败方式。未启用时协程只是阻塞等信号,没有任何开销。
func New(o Options) *Mapper {
	m := newMapper(o)
	go m.run()
	return m
}

// newMapper 只构造,不起协程。生产路径走 New(),测试走 newIdle。
func newMapper(o Options) *Mapper {
	if o.LeaseSec == 0 {
		o.LeaseSec = defaultLeaseSec
	}
	if o.DiscoverFn == nil {
		o.DiscoverFn = Discover
	}
	m := &Mapper{
		opts:     o,
		internal: o.InternalIP,
		rules:    o.Rules,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		ended:    make(chan struct{}),
	}
	m.snap = Snapshot{State: StateDisabled, Message: "未开启", UpdatedAt: time.Now()}
	return m
}

// Close 停掉后台协程。
//
// **不删除路由器上的映射。** 这是有意的:进程被强杀或断电时根本没有机会删,
// 如果干净退出删、崩溃不删,路由器上的状态就取决于"这次是怎么退的" ——
// 用户没法预期。既然崩溃时保证不了,就统一不删,靠租约自然过期兜底。
// 残留的映射指向一个已经没人监听的端口,本身无害,下次启动还能直接复用。
func (m *Mapper) Close() {
	m.closeOnce.Do(func() {
		close(m.done)
		<-m.ended
	})
}

// Snapshot 返回当前状态的副本。随时可调用。
func (m *Mapper) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.snap
	// Rules 是切片,必须复制 —— 否则调用方拿到的还是我们内部那个数组
	s.Rules = append([]RuleStatus(nil), m.snap.Rules...)
	return s
}

// SetEnabled 打开或关闭自动映射。非阻塞。
//
// 关闭时会撤掉路由器上的映射 —— 那是一个明确的意图("我不要这个了"),
// 和进程退出的情况不同,值得多花一次网络往返。
func (m *Mapper) SetEnabled(on bool) {
	m.mu.Lock()
	changed := m.enabled != on
	m.enabled = on
	m.mu.Unlock()

	if changed {
		if on {
			m.update(func(s *Snapshot) {
				s.State = StateDiscovering
				s.Message = "正在查找支持 UPnP 的路由器…"
				s.Hint = ""
			})
		}
		m.signal()
	}
}

// Configure 更新要映射的端口和本机地址。非阻塞。
//
// 端口改了必须调它 —— 否则我们守着旧端口,而 MediaMTX 已经换到新端口上监听,
// 映射就指向了一个没人监听的端口。
func (m *Mapper) Configure(rules []Rule, internalIP string) {
	m.mu.Lock()
	stale := !sameRules(m.rules, rules) || m.internal != internalIP

	// 从规则里消失的端口记进待删队列,由后台协程到路由器上撤掉。
	for _, old := range m.rules {
		if !containsRule(rules, old) && !containsRule(m.removed, old) {
			m.removed = append(m.removed, old)
		}
	}
	// 又被加回来的,取消待删 —— 否则刚建好就被自己撤掉。关掉远控再打开
	// 是很平常的操作,不能让它变成"映射时有时无"。
	if len(m.removed) > 0 {
		kept := m.removed[:0]
		for _, r := range m.removed {
			if !containsRule(rules, r) {
				kept = append(kept, r)
			}
		}
		m.removed = kept
	}

	m.rules, m.internal = rules, internalIP
	m.mu.Unlock()

	if stale {
		m.signal()
	}
}

// containsRule 报告 rules 里有没有 r。Rule 全是可比较字段,直接比即可。
func containsRule(rules []Rule, r Rule) bool {
	for _, x := range rules {
		if x == r {
			return true
		}
	}
	return false
}

// takeRemoved 取走待删队列。删失败的用 requeueRemoved 放回去。
func (m *Mapper) takeRemoved() []Rule {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.removed
	m.removed = nil
	return out
}

// requeueRemoved 把删失败的规则放回待删队列,下次再试。
//
// 这一条不能省:一个删不掉的映射就是一个一直对着公网开着的洞,
// 丢掉队列等于把它忘了。
func (m *Mapper) requeueRemoved(rules []Rule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rules {
		// 这期间又被加回来的,别再排队删了
		if containsRule(m.rules, r) || containsRule(m.removed, r) {
			continue
		}
		m.removed = append(m.removed, r)
	}
}

// deleteRemoved 把已经从规则里移掉的映射从路由器上撤销。
//
// 不阻塞当前这一轮,也不报给用户 —— 用户表达的意图是"不要这个端口了",
// 拿一条删除失败去打扰他没有意义。但会放回队列重试。
func (m *Mapper) deleteRemoved(ctx context.Context, gw *Gateway) {
	removed := m.takeRemoved()
	if len(removed) == 0 {
		return
	}

	var failed []Rule
	for _, r := range removed {
		if err := gw.DeleteMapping(ctx, r); err != nil {
			log.Printf("自动映射:撤掉不再需要的 %s %d 失败,下次再试: %v",
				r.Proto, r.ExternalPort, err)
			failed = append(failed, r)
			continue
		}
		log.Printf("自动映射:已撤掉不再需要的 %s %d", r.Proto, r.ExternalPort)
	}
	m.requeueRemoved(failed)
}

// Retry 立刻重来一次,不等退避。
func (m *Mapper) Retry() {
	m.update(func(s *Snapshot) {
		if s.State == StateFailed {
			s.State = StateDiscovering
			s.Message = "正在重新查找…"
		}
	})
	m.signal()
}

// signal 叫醒后台协程。缓冲为 1,重复调用不会堆积。
func (m *Mapper) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Mapper) isEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enabled
}

func (m *Mapper) config() (string, []Rule) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.internal, append([]Rule(nil), m.rules...)
}

// update 改快照。
func (m *Mapper) update(f func(*Snapshot)) {
	m.mu.Lock()
	f(&m.snap)
	m.snap.UpdatedAt = time.Now()
	m.mu.Unlock()
}

// runState 是后台协程私有的状态,不必加锁。
type runState struct {
	gateway    *Gateway
	externalIP string
	mapped     bool
	failures   int
}

// run 是唯一的后台协程。所有网络动作都发生在它里面 ——
// 这样既不用给 Gateway 加锁,也不用担心两个 goroutine 同时去改路由器。
func (m *Mapper) run() {
	defer close(m.ended)

	st := &runState{}
	for {
		if !m.isEnabled() {
			// 判据是"有没有路由器可以说话",不是 st.mapped —— 建过一条、
			// 之后某一轮失败又把它置回 false 的情况,条目其实还在路由器上。
			if st.gateway != nil {
				m.deleteAll(st.gateway)
			}
			*st = runState{}
			m.update(func(s *Snapshot) {
				s.State = StateDisabled
				s.Message = "未开启"
				s.Hint = ""
				s.ExternalIP = ""
				s.Gateway = ""
				s.ServiceKind = ""
				s.GatewayCount = 0
				s.Rules = nil
			})

			select {
			case <-m.done:
				return
			case <-m.wake: // 被打开,或配置变了
			}
			continue
		}

		wait := m.sync(st)

		t := time.NewTimer(wait)
		select {
		case <-m.done:
			t.Stop()
			return
		case <-m.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// sync 做一遍完整的工作,返回下次该等多久。
func (m *Mapper) sync(st *runState) time.Duration {
	ctx, cancel := context.WithTimeout(context.Background(), 2*discoverTimeout+10*time.Second)
	defer cancel()

	// 还没找到路由器,或者上一轮断定它没了
	if st.gateway == nil {
		m.update(func(s *Snapshot) {
			s.State = StateDiscovering
			s.Message = "正在查找支持 UPnP 的路由器…"
			s.Hint = ""
		})

		gateways, err := m.opts.DiscoverFn(ctx, discoverTimeout)
		if err != nil {
			return m.fail(st, "查找路由器失败:"+err.Error())
		}
		if len(gateways) == 0 {
			return m.fail(st, "没有发现支持 UPnP 的路由器")
		}

		gw := pickGateway(gateways)
		st.gateway = gw
		st.failures = 0

		m.update(func(s *Snapshot) {
			s.Gateway = gw.FriendlyName
			s.ServiceKind = gw.ServiceKind
			s.GatewayCount = len(gateways)
		})
	}

	gw := st.gateway
	internal, rules := m.config()

	// 先把不再需要的撤掉。放在建设之前:关掉的端口(比如远控关掉之后的
	// TCP 8090)不该多留一轮,哪怕这一轮后面会失败。
	m.deleteRemoved(ctx, gw)

	// 外网地址:拿不到就别往下走了 —— 映射建了也没用。
	// 它同时也是要写进观看链接的那个地址,所以变化时得通知外面。
	externalIP, err := gw.ExternalIP(ctx)
	if err != nil && st.externalIP == "" {
		return m.fail(st, "读不到路由器报告的外网地址:"+err.Error())
	}
	if err == nil {
		if ok, why := ExternalIPUsable(externalIP); !ok {
			return m.fail(st, why)
		}
	} else {
		externalIP = st.externalIP // 这次没读到,沿用上次的
	}

	if st.mapped && externalIP != st.externalIP && st.externalIP != "" {
		// 公网 IP 变了 —— 用户遇到的"昨天还好好的,今天打不开了"多半就是它
		log.Printf("自动映射:公网地址由 %s 变为 %s", st.externalIP, externalIP)
	}
	notify := externalIP != "" && externalIP != st.externalIP
	st.externalIP = externalIP

	statuses, err := m.ensureMappings(ctx, gw, rules, internal)
	if err != nil {
		return m.fail(st, err.Error())
	}

	st.mapped = true
	st.failures = 0

	m.update(func(s *Snapshot) {
		s.State = StateActive
		s.ExternalIP = externalIP
		s.Gateway = gw.FriendlyName
		s.ServiceKind = gw.ServiceKind
		s.Rules = statuses
		// 端口清单要列全,不能只写观看端口。远控开着时一共开了三条,
		// 而这一行是"自动映射生效了"唯一的地方 —— 只说一条,用户就没法
		// 从界面上确认远控端口到底开没开(实测就是被问到过这件事)。
		s.Message = fmt.Sprintf("自动映射已生效 · 外网地址 %s · 已开端口 %s",
			HostPort(externalIP, rules[0].ExternalPort), portList(rules))
		s.Hint = fmt.Sprintf(
			"自动映射成功不等于一定可达。请放行 Windows 防火墙的 %s,"+
				"然后关掉手机 WiFi、用 4G 打开公网链接验证一次。",
			portList(rules))
	})

	if notify && m.opts.OnChange != nil {
		m.opts.OnChange(externalIP)
	}

	return nextCheck(statuses, m.opts.LeaseSec)
}

// ensureMappings 确认每条映射都在、都指向我们;不对的重建。
func (m *Mapper) ensureMappings(ctx context.Context, gw *Gateway, rules []Rule,
	internal string) ([]RuleStatus, error) {

	var (
		out    []RuleStatus
		firstE error
	)

	for _, r := range rules {
		st := RuleStatus{
			Proto: string(r.Proto),
			Label: ruleLabel(r),
			Port:  r.ExternalPort,
		}

		existing, err := gw.GetMapping(ctx, r)
		switch {
		case err == nil && mappingMatches(existing, r, internal):
			// 已经是对的,什么都不用做
			st.Mapped = true

		case err == nil:
			// 条目在,但指向别处 —— 可能是别的程序占着,也可能是本机网卡
			// 地址变了(换 WiFi、插拔网线都会)。重建。
			if _, aerr := gw.AddMapping(ctx, r, internal, m.opts.LeaseSec); aerr != nil {
				st.Error = describeAddError(aerr)
				if firstE == nil {
					firstE = fmt.Errorf("%s %d:%s", r.Proto, r.ExternalPort, st.Error)
				}
			} else {
				st.Mapped = true
			}

		default:
			// 条目不存在(或查不动)—— 直接建
			if _, aerr := gw.AddMapping(ctx, r, internal, m.opts.LeaseSec); aerr != nil {
				st.Error = describeAddError(aerr)
				if firstE == nil {
					firstE = fmt.Errorf("%s %d:%s", r.Proto, r.ExternalPort, st.Error)
				}
			} else {
				st.Mapped = true
			}
		}

		out = append(out, st)
	}

	return out, firstE
}

// deleteAll 撤掉我们建的全部映射。只用在用户主动关闭这个功能时。
func (m *Mapper) deleteAll(gw *Gateway) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, rules := m.config()
	// 连已经不在当前规则里的也一起撤 —— 关掉这个功能之后,路由器上不该
	// 还剩任何一个由我们开的洞。
	rules = append(rules, m.takeRemoved()...)
	for _, r := range rules {
		if err := gw.DeleteMapping(ctx, r); err != nil {
			// 删不掉不是致命问题:租约到期它会自己消失。
			// 这里打扰用户没有意义 —— 他刚表达了"不要了"。
			log.Printf("自动映射:撤掉 %s %d 失败(租约到期会自行消失): %v",
				r.Proto, r.ExternalPort, err)
		} else {
			log.Printf("自动映射:已撤掉 %s %d", r.Proto, r.ExternalPort)
		}
	}
}

// fail 记录一次失败,返回退避时长。
func (m *Mapper) fail(st *runState, message string) time.Duration {
	st.failures++

	wait := discoverBackoff[len(discoverBackoff)-1]
	if st.failures <= len(discoverBackoff) {
		wait = discoverBackoff[st.failures-1]
	}

	// 路由器整个不见了(重启、断电)时要重新发现,不能抱着旧句柄不放
	st.gateway = nil
	st.mapped = false

	// 提示里要列出实际要开的那几个端口 —— 当前规则可能含远控端口,而它
	// 只在远控开着时才存在,写死"两条"就会漏掉。
	_, rules := m.config()
	m.update(func(s *Snapshot) {
		s.State = StateFailed
		s.Message = message
		s.Hint = manualHint(message, rules)
	})

	log.Printf("自动映射:%s(将在 %s 后重试)", message, wait)
	return wait
}

// manualHint 在自动这条路走不通时,告诉用户下一步做什么。
//
// 只说该做什么,不解释原理 —— 用户此刻要的是"那我怎么办"。
func manualHint(message string, rules []Rule) string {
	if containsAny(message, "运营商级 NAT", "内网地址", "IPv6") {
		return "这台机器的上网出口拿不到公网地址,自动映射帮不上忙。" +
			"需要改用 VPS 中转或内网穿透,见 docs/公网部署手册.md。"
	}
	return fmt.Sprintf(
		"请在路由器后台手动添加 %d 条端口映射:%s,内外端口一致。"+
			"详见 README 的「让公网也能看」一节。",
		len(rules), portList(rules))
}

// portList 把要开的那几个端口写成"8889/tcp、8189/udp 和 8090/tcp"。
//
// 必须按实际规则生成,不能写死前两条。远控开着时映射有三条,而远控端口
// 落在 8xxx 之外、又只在开启后才有 —— 提示里漏掉它,用户会照着一份
// 看起来完整的说明放行完防火墙,然后面对"公网上能看画面、但控制不了",
// 而且没有任何地方告诉他少了什么。
func portList(rules []Rule) string {
	if len(rules) == 0 {
		return ""
	}
	parts := make([]string, len(rules))
	for i, r := range rules {
		parts[i] = fmt.Sprintf("%d/%s", r.ExternalPort, strings.ToLower(string(r.Proto)))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], "、") + " 和 " + parts[len(parts)-1]
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// pickGateway 在多台候选里挑一台。
//
// 双重 NAT 的家庭网络里会有不止一台 IGD,只有最外层那台管公网。
// 判据是"谁报告的外网地址像公网地址" —— 里层那台报告的是它自己的内网地址。
// 都像内网(或者都问不出来)就只能取第一台,并且靠 GatewayCount 提示用户。
func pickGateway(gs []*Gateway) *Gateway {
	if len(gs) == 1 {
		return gs[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for _, g := range gs {
		ip, err := g.ExternalIP(ctx)
		if err != nil {
			continue
		}
		if ok, _ := ExternalIPUsable(ip); ok {
			return g
		}
	}
	return gs[0]
}

// mappingMatches 判断路由器上那条记录是不是我们要的那条。
func mappingMatches(cur Mapping, r Rule, internal string) bool {
	if !cur.Enabled {
		return false
	}
	if cur.InternalPort != r.InternalPort {
		return false
	}
	// 路由器可能把地址写成别的等价形式,按 IP 比较而不是字符串
	return sameIP(cur.InternalClient, internal)
}

func sameIP(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	// 按 IP 比较而不是字符串:路由器回读时可能给出等价但写法不同的地址
	if ia, ib := net.ParseIP(a), net.ParseIP(b); ia != nil && ib != nil {
		return ia.Equal(ib)
	}
	// 解不出 IP 的只能按字面比 —— 至少把两边的空白去掉,
	// 有些路由器的回读值带着尾随空格。
	return a == b
}

// nextCheck 决定下次多久之后再检查。
//
// 依据是租约还剩多久 —— 检查得比租约到期更频繁,才不会出现"检查的时候已经过期"。
// 路由器给永久映射(租约 0)时没有到期时间可依据,只能定期确认条目还在。
func nextCheck(statuses []RuleStatus, lease uint32) time.Duration {
	for _, s := range statuses {
		if !s.Mapped {
			return minCheckInterval // 有没建上的,早点重试
		}
	}
	if lease == 0 {
		return permanentCheckInterval
	}

	d := time.Duration(lease) * time.Second * 8 / 10
	if d < minCheckInterval {
		d = minCheckInterval
	}
	if d > maxCheckInterval {
		d = maxCheckInterval
	}
	return d
}

func ruleLabel(r Rule) string {
	if r.Proto == ProtoUDP {
		return "媒体端口"
	}
	return "播放端口"
}

func describeAddError(err error) string {
	if code := ErrorCode(err); code != 0 {
		if name := ErrorCodeName(code); name != "" {
			return name
		}
		return fmt.Sprintf("路由器返回错误码 %d", code)
	}
	return err.Error()
}

func sameRules(a, b []Rule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
