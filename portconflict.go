package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sharescreen/internal/config"
	"sharescreen/internal/netport"
	"sharescreen/internal/paths"
)

// 端口被占用时,原来只是报个错就退出 —— 那是个死胡同:用户只知道"起不来",
// 却不知道下一步该做什么。这里给两条出路:
//
//  1. 结束占用它的进程 —— 只在占用者确实是自家进程时才做,而且不问
//  2. 换一个空闲端口 —— 改了会写回配置,免得下次还问
//
// 两条都不选就退出。

// maxPortRetries 是最多让路几次。防止换了端口又被占、再换又被占这种死循环。
const maxPortRetries = 3

// portConflict 描述一个被占用的端口。
type portConflict struct {
	label  string
	addr   string // 探测用的绑定地址,必须是该组件实际会绑的那个
	port   int
	udp    bool
	router bool // 改这个端口要同步改路由器映射,否则公网观众连不上

	owner netport.Owner
	known bool

	set func(*config.Config, int)
}

func (c *portConflict) proto() string {
	if c.udp {
		return "UDP"
	}
	return "TCP"
}

// ownerText 描述是谁占着,查不到时给个含糊但诚实的说法。
func (c *portConflict) ownerText() string {
	if c.known && c.owner.Name != "" {
		return fmt.Sprintf("%s(PID %d)", c.owner.Name, c.owner.PID)
	}
	return "另一个程序"
}

// err 返回给用户看的失败说明。
func (c *portConflict) err() error {
	return fmt.Errorf(
		"%s 的 %s 端口 %d 被 %s 占用。\n"+
			"等你关掉它之后重新启动即可。",
		c.label, c.proto(), c.port, c.ownerText())
}

// findPortConflict 返回第一个被占用的端口;全部可用时返回 nil。
func findPortConflict(cfg config.Config) *portConflict {
	probes := []portConflict{
		{
			label: "控制页", addr: loopback(cfg.ControlPort), port: cfg.ControlPort,
			set: func(c *config.Config, p int) { c.ControlPort = p },
		},
		{
			label: "推流入口", addr: loopback(cfg.RTSPPort), port: cfg.RTSPPort,
			set: func(c *config.Config, p int) { c.RTSPPort = p },
		},
		{
			label: "MediaMTX 管理接口", addr: loopback(cfg.APIPort), port: cfg.APIPort,
			set: func(c *config.Config, p int) { c.APIPort = p },
		},
		{
			// MediaMTX 的 WebRTC 监听的是 :8889(所有网卡),不是回环 ——
			// 探测地址必须跟着它,否则占 0.0.0.0 的进程检测不出来。
			label: "WebRTC 播放端口", addr: any(cfg.WebRTCPort), port: cfg.WebRTCPort, router: true,
			set: func(c *config.Config, p int) { c.WebRTCPort = p },
		},
		{
			label: "WebRTC 媒体端口", addr: any(cfg.UDPPort), port: cfg.UDPPort,
			udp: true, router: true,
			set: func(c *config.Config, p int) { c.UDPPort = p },
		},
		{
			label: "RTMP 入口", addr: loopback(cfg.RTMPPort), port: cfg.RTMPPort,
			set: func(c *config.Config, p int) { c.RTMPPort = p },
		},
	}

	for i := range probes {
		p := &probes[i]

		// 先查系统的监听表 —— 它列的是真实的监听套接字,不受 SO_REUSEADDR
		// 影响,是权威的。
		//
		// 单靠"试着绑一下"不够:实测占位进程设了 SO_REUSEADDR 时,Windows
		// 允许另一个进程绑同一个端口,连 MediaMTX 自己都能绑上 —— 探不出冲突,
		// 然后两个进程抢同一个端口,行为完全看谁先收到数据。
		owner, found := netport.Find(p.port, p.udp)
		if !found && !portInUse(p.addr, p.udp) {
			continue
		}
		p.owner, p.known = owner, found
		return p
	}
	return nil
}

func loopback(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }
func any(port int) string      { return fmt.Sprintf("0.0.0.0:%d", port) }

// portInUse 试着绑一下端口,绑不上就认为被占用。
//
// 探测用的地址必须和该组件实际会绑的一致。Windows 上绑 127.0.0.1 和绑
// 0.0.0.0 是两回事:一个进程占着 0.0.0.0:8889 的时候,127.0.0.1:8889 照样
// 绑得上 —— 探测地址选错,冲突就检测不出来,一直要等到 MediaMTX 起不来
// 才暴露。
func portInUse(addr string, udp bool) bool {
	if udp {
		pc, err := net.ListenPacket("udp", addr)
		if err == nil {
			_ = pc.Close()
		}
		return err != nil
	}
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close()
	}
	return err != nil
}

// resolveConflict 处理一个端口冲突。
//
// 返回值有两种"结束":retry 为真表示已经处理好了、调用方应当重新检查端口;
// retry 为假且 err 为 nil 表示已经妥善收场(比如打开了已在运行的实例),
// 正常退出即可;err 非 nil 才是真的要报错退出。
func resolveConflict(cfg *config.Config, c *portConflict, cfgPath string) (bool, error) {
	// 占用者是另一个 ShareScreen 自己 —— 这多半不是"冲突",而是用户忘了
	// 它已经开着。那就把他的控制页打开,而不是问他"要不要杀掉那个"。
	// 顺带把最危险的一条路(杀另一个自己)从根上去掉了。
	if isSameApp(c.owner) {
		url := fmt.Sprintf("http://127.0.0.1:%d/", cfg.ControlPort)
		log.Printf("已经有一个 ShareScreen 在运行,打开它的控制页: %s", url)
		openBrowser(url)
		return false, nil
	}

	// 占用者是自家的工具进程(残留的 mediamtx / ffmpeg)—— 直接结束,不问。
	//
	// 判据是**可执行文件的完整路径**,不是进程名 —— 用户完全可能自己开着
	// ffmpeg 做别的事情,按名字杀会误伤。这条线这个项目一直守着。
	//
	// 为什么这里不再问:能走到这一步,说明端口上坐的是我们自己拉起来、
	// 却没跟着上一个实例退干净的进程,它此刻唯一的作用就是挡路。真正的
	// 选择只有一个,弹个框只是把它变成"读一段话再点一下"。占端口的是
	// 别的程序时仍然会问 —— 那才是有代价的决定。
	//
	// 这一条也顺带覆盖了 WebRTC 那两个端口:残留的 mediamtx 让出 8889 之后
	// 什么都不用改,路由器映射原样有效,自然也就不必走下面那条"不能自动换"
	// 的路。
	if isOurProcess(c.owner) {
		if err := netport.Kill(c.owner.PID); err != nil {
			// 杀不掉(多半是权限)就继续往下走,让用户还有换端口这条路,
			// 而不是卡在这里报一个他无能为力的错。
			log.Printf("结束残留的 %s(PID %d)失败: %v", c.owner.Name, c.owner.PID, err)
		} else {
			log.Printf("已结束残留的 %s(PID %d), %s 端口 %d 让出",
				c.owner.Name, c.owner.PID, c.proto(), c.port)
			waitPortFree(c.addr, c.udp, 3*time.Second)
			return true, nil
		}
	}

	// WebRTC 播放端口和媒体端口不自动换。
	//
	// 这两个端口在路由器上做了映射,换了就等于把公网入口挪走 —— 观众那边
	// 直接连不上,而用户要等到别人反馈才会发现。这种事不能悄悄做,哪怕
	// 对话框里写了警告:警告只在有人真的读了它的时候才算数。
	//
	// 换端口要连带改路由器映射,那是只有用户能完成的一步,所以这里不替他决定,
	// 只把该做什么说清楚。
	if c.router {
		return false, fmt.Errorf(
			"%s 的 %s 端口 %d 被 %s 占用。\n"+
				"这个端口在路由器上做了映射,不能自动更换 —— 换了公网就断了。\n"+
				"请先关掉占用它的程序;确实要换的话,改配置文件里的端口,"+
				"并同步修改路由器的端口映射。",
			c.label, c.proto(), c.port, c.ownerText())
	}

	newPort := pickFreePort(c.port, c.udp)
	if newPort == 0 {
		return false, fmt.Errorf(
			"%s 的 %s 端口 %d 被 %s 占用,而且找不到可用的替代端口。",
			c.label, c.proto(), c.port, c.ownerText())
	}

	q := fmt.Sprintf("%s 的 %s 端口 %d 被 %s 占用。\n\n要改用 %d 吗?",
		c.label, c.proto(), c.port, c.ownerText(), newPort)
	if !ask("ShareScreen 启动受阻", q) {
		return false, c.err()
	}

	c.set(cfg, newPort)
	if err := config.Save(cfgPath, *cfg); err != nil {
		log.Printf("警告: 新端口没能写入配置,下次启动还会问一遍: %v", err)
	} else {
		log.Printf("%s 端口由 %d 改为 %d,已写入配置", c.label, c.port, newPort)
	}
	return true, nil
}

// isSameApp 判断占用端口的是不是另一个 ShareScreen 实例。
func isSameApp(o netport.Owner) bool {
	if o.Path == "" || !strings.EqualFold(o.Name, "sharescreen.exe") {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.EqualFold(filepath.Dir(self), filepath.Dir(o.Path))
}

// pickFreePort 找一个空闲端口。从当前端口往上找,尽量不打乱用户已有的
// 端口映射关系(比如 8080 换成 8081 而不是随机一个)。
func pickFreePort(from int, udp bool) int {
	if udp {
		return netport.FreeUDP(from + 1)
	}
	return netport.Free(from + 1)
}

// waitPortFree 等端口真正空出来。
//
// 进程终止和端口释放之间有个间隔,Windows 上尤其明显 —— 这也是为什么
// 重启 MediaMTX 时本来就要重试。
func waitPortFree(addr string, udp bool, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !portInUse(addr, udp) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// isOurProcess 判断占用端口的进程是不是自家程序。
//
// 认两条:进程名必须是 sharescreen / mediamtx / ffmpeg 三者之一,
// 而且可执行文件得待在我们自己的目录树里 —— 自家 exe 所在目录(侧载版
// 的 tools/ 就在里面),或者数据目录下(内嵌版把工具释放到那儿)。
func isOurProcess(o netport.Owner) bool {
	if o.Path == "" {
		return false
	}
	switch strings.ToLower(o.Name) {
	case "sharescreen.exe", "mediamtx.exe", "ffmpeg.exe":
	default:
		return false
	}

	dir := strings.ToLower(filepath.Dir(o.Path))
	if self, err := os.Executable(); err == nil {
		if strings.HasPrefix(dir, strings.ToLower(filepath.Dir(self))) {
			return true
		}
	}
	if dataDir, err := paths.DataDir(); err == nil {
		if strings.HasPrefix(dir, strings.ToLower(dataDir)) {
			return true
		}
	}
	return false
}
