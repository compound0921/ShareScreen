// portmapcheck 检查路由器支不支持 UPnP 自动端口映射,并可以试建一次映射。
//
//	go run ./cmd/portmapcheck              # 只读:发现路由器、查外网地址、列出已有映射
//	go run ./cmd/portmapcheck -map         # 顺便试建 8889/tcp 和 8189/udp,试完删掉
//	go run ./cmd/portmapcheck -map -keep   # 试建并保留(等于手工做好映射)
//	go run ./cmd/portmapcheck -ports 9000,9001 -map
//
// 程序自己启动时走的是同一份 internal/portmap 代码 —— 这个工具不是另一套实现,
// 只是把那一层的每一步单独摊开给你看。控制页说"自动映射不可用"时,先用它定位
// 卡在哪一步:是根本没发现路由器,还是发现了但不认端口,还是外网地址本身不可用。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"sharescreen/internal/portmap"
)

func main() {
	doMap := flag.Bool("map", false, "试建映射(默认试完就删)")
	keep := flag.Bool("keep", false, "试建的映射保留下来,不删")
	ports := flag.String("ports", "8889,8189", "要映射的端口,格式 TCP端口,UDP端口")
	lease := flag.Int("lease", 3600, "请求的租约秒数")
	timeout := flag.Duration("timeout", 3*time.Second, "SSDP 每个搜索目标的等待时长")
	flag.Parse()

	tcpPort, udpPort, err := parsePorts(*ports)
	if err != nil {
		fail("%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Printf("目标端口:TCP %d、UDP %d(内外一致)\n\n", tcpPort, udpPort)

	// ── 1. 发现 ──
	fmt.Println("=== 发现路由器(SSDP)===")
	gateways, err := portmap.Discover(ctx, *timeout)
	if err != nil {
		fail("发现失败: %v", err)
	}
	if len(gateways) == 0 {
		fmt.Println("  一台都没找到。")
		fmt.Println()
		fmt.Println("路由器要么关着 UPnP,要么这台机器不在它的局域网里。")
		fmt.Println("先去路由器后台找 UPnP / 通用即插即用 开关;开了再跑一次。")
		os.Exit(1)
	}
	for i, g := range gateways {
		fmt.Printf("  [%d] %s\n", i, g.FriendlyName)
		fmt.Printf("      厂商/型号:%s %s\n", g.Manufacturer, g.ModelName)
		fmt.Printf("      服务类型:%s\n", g.ServiceKind)
		fmt.Printf("      设备描述:%s\n", g.Location)
		fmt.Printf("      本机出口:%v\n", g.LocalAddr)
	}
	if len(gateways) > 1 {
		fmt.Printf("\n  ⚠ 发现了 %d 台网关设备。家庭网络里这通常意味着双重 NAT,\n", len(gateways))
		fmt.Println("    最外层那台才管公网,映射建在里层等于没建。")
	}

	// ── 2. 每台的外网地址 ──
	fmt.Println("\n=== 外网地址 ===")
	var usable []*portmap.Gateway
	for i, g := range gateways {
		ip, err := g.ExternalIP(ctx)
		if err != nil {
			fmt.Printf("  [%d] 查询失败: %v\n", i, err)
			continue
		}
		ok, why := portmap.ExternalIPUsable(ip)
		if ok {
			fmt.Printf("  [%d] %s  ✓ 可用\n", i, ip)
			usable = append(usable, g)
		} else {
			fmt.Printf("  [%d] %s  ✗ %s\n", i, ip, why)
		}
	}
	if len(usable) == 0 {
		fmt.Println("\n没有一台报告可用的公网地址 —— 自动映射即使建上也访问不到。")
		fmt.Println("这种情况只能走 VPS 中转或内网穿透,见 docs/公网架构设计.md。")
		os.Exit(1)
	}

	// ── 3. 现有映射 ──
	fmt.Println("\n=== 路由器上现有的映射 ===")
	listMappings(ctx, usable[0])

	if !*doMap {
		fmt.Println("\n(只读模式。加 -map 可以试建一次映射。)")
		return
	}

	// ── 4. 试建 ──
	fmt.Println("\n=== 试建映射 ===")
	internal := usable[0].LocalAddr.String()
	if internal == "" || internal == "<nil>" {
		fail("拿不到本机内网地址,无法建立映射")
	}
	fmt.Printf("映射目标:%s  租约:%d 秒\n\n", internal, *lease)

	if usable[0].ModelName != "" {
		// 华为等部分路由器在并发请求下更容易断连,一条一条来
		time.Sleep(200 * time.Millisecond)
	}

	rules := portmap.RulesFor(tcpPort, udpPort)
	allOK := true
	for _, r := range rules {
		if !tryOne(ctx, usable[0], r, internal, uint32(*lease), *keep) {
			allOK = false
		}
		time.Sleep(200 * time.Millisecond)
	}

	fmt.Println()
	if !allOK {
		fmt.Println("结论:有映射没能建立 —— 程序里的自动映射在这台路由器上不会生效,")
		fmt.Println("      请按 README 手动配置。")
		os.Exit(1)
	}
	if *keep {
		fmt.Printf("结论:两条映射都已建立并保留。公网观看地址是 http://%s/%s/\n",
			portmap.HostPort(externalIPOf(ctx, usable[0]), tcpPort), "live")
	} else {
		fmt.Println("结论:路由器接受写入 —— 自动映射可行。(试建的映射已删除)")
	}
}

func tryOne(ctx context.Context, g *portmap.Gateway, r portmap.Rule,
	internal string, lease uint32, keep bool) bool {

	fmt.Printf("--- %s %d ---\n", r.Proto, r.ExternalPort)

	actualLease, err := g.AddMapping(ctx, r, internal, lease)
	if err != nil {
		fmt.Printf("  建立失败: %v\n", err)
		if code := portmap.ErrorCode(err); code != 0 {
			fmt.Printf("  UPnP 错误码 %d:%s\n", code, portmap.ErrorCodeName(code))
		}
		return false
	}
	fmt.Printf("  已建立(路由器给的租约 %d 秒)\n", actualLease)

	m, err := g.GetMapping(ctx, r)
	if err != nil {
		fmt.Printf("  ⚠ 回读失败: %v\n", err)
		return true // 建立成功了,回读失败不足以判失败
	}
	fmt.Printf("  回读:%s:%d  启用=%v  描述=%q\n",
		m.InternalClient, m.InternalPort, m.Enabled, m.Description)

	if m.InternalClient != internal || m.InternalPort != r.InternalPort {
		fmt.Printf("  ⚠ 回读指向的不是我们 —— 想要的 %s:%d\n", internal, r.InternalPort)
		return false
	}

	if keep {
		fmt.Println("  保留")
		return true
	}

	if err := g.DeleteMapping(ctx, r); err != nil {
		fmt.Printf("  ⚠ 删除失败: %v(条目留在路由器上了)\n", err)
		return true
	}
	fmt.Println("  已删除")
	return true
}

func listMappings(ctx context.Context, g *portmap.Gateway) {
	ms, err := g.ListMappings(ctx, 64)
	if err != nil {
		fmt.Printf("  读取失败: %v\n", err)
		return
	}
	if len(ms) == 0 {
		fmt.Println("  (无)")
		return
	}
	for i, m := range ms {
		fmt.Printf("  [%d] %-3s 外%5d → 内 %s:%d  %q  启用=%v 租约=%ds\n",
			i, m.Proto, m.ExternalPort, m.InternalClient, m.InternalPort,
			m.Description, m.Enabled, m.LeaseSec)
	}
}

func externalIPOf(ctx context.Context, g *portmap.Gateway) string {
	ip, err := g.ExternalIP(ctx)
	if err != nil {
		return "<公网IP>"
	}
	return ip
}

// parsePorts 解析 "-ports 8889,8189" 这种写法。
func parsePorts(s string) (tcp, udp int, err error) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("端口格式应为「TCP端口,UDP端口」,收到 %q", s)
	}
	tcp, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || tcp <= 0 || tcp > 65535 {
		return 0, 0, fmt.Errorf("TCP 端口非法: %q", parts[0])
	}
	udp, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || udp <= 0 || udp > 65535 {
		return 0, 0, fmt.Errorf("UDP 端口非法: %q", parts[1])
	}
	return tcp, udp, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	os.Exit(1)
}
