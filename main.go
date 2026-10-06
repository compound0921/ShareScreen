// ShareScreen —— Windows 屏幕共享工具。
//
// 采集和编码交给 ffmpeg,WebRTC 服务端交给 MediaMTX,本程序负责把两者
// 串起来,并提供一个 localhost 网页作为控制界面。
//
// 观众用浏览器打开观看地址即可,无需安装任何客户端。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"sharescreen/internal/clipboard"
	"sharescreen/internal/config"
	"sharescreen/internal/ffmpeg"
	"sharescreen/internal/gpu"
	"sharescreen/internal/mediamtx"
	"sharescreen/internal/notify"
	"sharescreen/internal/paths"
	"sharescreen/internal/portmap"
	"sharescreen/internal/remotectl"
	"sharescreen/internal/screen"
	"sharescreen/internal/server"
	"sharescreen/internal/stream"
	"sharescreen/internal/tray"
)

func main() {
	cfgPath := flag.String("config", "", "配置文件路径(默认放在 %LOCALAPPDATA%\\ShareScreen\\config.json)")
	printArgs := flag.Bool("print-args", false, "打印将要执行的 ffmpeg 命令后退出(用于排查参数问题)")
	noBrowser := flag.Bool("no-browser", false, "启动后不自动打开浏览器")
	noTray := flag.Bool("no-tray", false, "不创建系统托盘图标")
	flag.Parse()

	// 不声明 DPI 感知的话,系统会返回被缩放过的桌面尺寸,
	// 分辨率预设的宽高比就会算错。
	screen.SetDPIAware()

	// 日志在 main 里建立,而不是在 run() 里 defer 关闭 —— 原因见 reportFatal。
	if dataDir, err := paths.DataDir(); err == nil {
		if closeLog, lerr := setupLogging(dataDir); lerr == nil {
			defer closeLog()
		} else {
			log.Printf("警告: 无法写入日志文件: %v", lerr)
		}
	}

	if err := run(*cfgPath, *printArgs, *noBrowser, *noTray); err != nil {
		reportFatal(err)
	}
}

// reportFatal 报告启动失败,然后退出。
//
// 这里刻意不用 log.Fatalf。日志的输出是 MultiWriter(文件, stderr),而
// MultiWriter 遇到第一个写入错误就返回 —— 只要日志文件已经关了,写 stderr
// 那一步就永远轮不到,错误一个字都出不来。这个组合曾经让"端口被占用"这类
// 启动失败变成完全静默:双击启动的用户只看到窗口闪一下,日志也毫无线索。
//
// 所以这里三件事都做:直接写 stderr(绕开 MultiWriter)、照常记日志、
// 弹一个对话框 —— 双击启动时进程没有控制台,不弹框就等于没提示。
func reportFatal(err error) {
	msg := err.Error()
	fmt.Fprintln(os.Stderr, "错误:", msg)
	log.Println("错误:", msg)
	showFatalDialog("ShareScreen 启动失败", msg)
	os.Exit(1)
}

func run(cfgPath string, printArgs, noBrowser, noTray bool) error {
	// 日志已经在 main 里建好了 —— 放那儿是为了让启动失败的错误报得出来。
	dataDir, err := paths.DataDir()
	if err != nil {
		return err
	}

	// ── 配置 ──
	if cfgPath == "" {
		cfgPath = filepath.Join(dataDir, "config.json")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Printf("警告: %v(使用默认配置)", err)
	}

	// ── 定位第三方组件 ──
	ffmpegTool, err := paths.ResolveFFmpeg()
	if err != nil {
		return err
	}
	mtxTool, err := paths.ResolveMediaMTX()
	if err != nil {
		return err
	}

	// ── 探测本机能力 ──
	//
	// 分两步:先问 ffmpeg 构建里有哪些编码器(快,但那只说明编译进去了),
	// 再让每个候选真编一帧,把跑不起来的剔掉(慢约一秒,但那是"能用"
	// 的唯一依据 —— 实测没有 A 卡的机器上 h264_amf 照样在构建列表里)。
	encoders, err := ffmpeg.ProbeEncoders(ffmpegTool.Exe)
	if err != nil {
		log.Printf("警告: 编码器探测失败: %v", err)
	} else {
		usable := ffmpeg.ProbeUsable(ffmpegTool.Exe, encoders)
		log.Printf("编码器: %s", encoderNames(encoders))
		if dropped := droppedNames(encoders, usable); dropped != "" {
			log.Printf("其中跑不起来的已剔除: %s", dropped)
		}
		encoders = usable
	}

	// 显示器挂在哪块显卡上 —— 决定编码器优先选谁。
	//
	// ddagrab 抓的是驱动显示器那块 GPU 的画面,编码若落在另一块卡上,
	// 帧要跨 PCIe 走一趟(架构设计 §5.4)。识不出来就是"未知",排序退回
	// 原本的优先级,不影响启动。
	adapterVendor := gpu.PrimaryVendor()
	if list := gpu.Adapters(); len(list) > 0 {
		log.Printf("显示器所属显卡: %s", adapterVendor)
		for _, a := range list {
			mark := ""
			if a.Primary {
				mark = "(主显示)"
			} else if a.Attached {
				mark = "(接了显示器)"
			}
			log.Printf("  %s [%s]%s", a.Name, a.Vendor, mark)
		}
	}

	desktopW, desktopH := screen.Size()
	presets := config.ResolutionPresets(desktopW, desktopH)
	log.Printf("桌面分辨率: %d×%d", desktopW, desktopH)

	// ── -print-args:只打印命令,不启动任何东西 ──
	if printArgs {
		plan := ffmpeg.Plan(cfg.Video.Encoder, encoders, adapterVendor)
		if len(plan) == 0 {
			return fmt.Errorf("没有可用的编码器")
		}
		enc := plan[0]
		target := fmt.Sprintf("rtsp://127.0.0.1:%d/%s", cfg.RTSPPort, cfg.StreamPath)

		// 干跑:不去真打开音频设备,用占位参数把音频那一段也打印出来,
		// 否则打印出来的命令行和实际跑的不是一回事。
		var audioIn *ffmpeg.AudioInput
		if cfg.Audio.Enabled {
			audioIn = &ffmpeg.AudioInput{
				URL:         "tcp://127.0.0.1:<系统分配的端口>",
				SampleFmt:   "f32le",
				SampleRate:  48000,
				Channels:    2,
				BitrateKbps: cfg.Audio.BitrateKbps,
			}
		}

		// 和 manager.startOnce 用同一个式子推,否则打印出来的命令
		// 会在"远控开关"这一项上和实际跑的不一致 —— 而排查时正是
		// 要靠这条命令去对。
		opts := ffmpeg.CaptureOptsFor(cfg.Video, cfg.RemoteControl.Enabled)
		args, err := ffmpeg.BuildArgs(cfg.Video, opts, audioIn, enc, target, desktopW, desktopH)
		if err != nil {
			return err
		}
		fmt.Println(ffmpeg.Describe(ffmpegTool.Exe, args))
		return nil
	}

	// ── 端口预检 ──
	// 最常见的失败是上一次没退干净,或者自己已经开着一个实例。
	//
	// 被占用时不能直接退出 —— 那是个死胡同,用户只知道"起不来",却不知道
	// 下一步做什么。交给 resolveConflict 处理:占着的是自家残留进程就直接
	// 清掉,是别的程序才问要不要换端口。每让一次路都要重新检查,最多三次。
	for attempt := 0; ; attempt++ {
		conflict := findPortConflict(cfg)
		if conflict == nil {
			break
		}
		if attempt >= maxPortRetries {
			return conflict.err()
		}
		retry, err := resolveConflict(&cfg, conflict, cfgPath)
		if err != nil {
			return err
		}
		if !retry {
			// 已经妥善收场(比如把已在运行的那个实例的控制页打开了),
			// 正常退出即可,不用再报一次错。
			return nil
		}
	}

	// ── 写 MediaMTX 配置并启动 ──
	mtxConfig, err := mediamtx.WriteConfig(dataDir, mtxOptions(cfg))
	if err != nil {
		return err
	}

	runner, err := mediamtx.NewRunner(mtxTool)
	if err != nil {
		return err
	}
	defer runner.Close()

	if err := runner.Start(mtxConfig); err != nil {
		return err
	}
	// MediaMTX 若因端口冲突等原因立刻退出,这里能第一时间发现
	if exited, werr := runner.WaitExit(1200 * time.Millisecond); exited {
		return fmt.Errorf("MediaMTX 启动失败: %v\n%s", werr, joinTail(runner.Tail(15)))
	}
	log.Printf("MediaMTX 已启动")

	// ── 推流管理器 ──
	manager, err := stream.New(ffmpegTool)
	if err != nil {
		return err
	}
	manager.SetScreenSize(desktopW, desktopH)
	manager.SetPreferredVendor(adapterVendor)
	defer manager.Close()

	// ── 控制页服务 ──
	ip := lanIP()

	// ── 远程控制 ──
	//
	// 默认不开启,也不监听任何端口。开启时才会绑 0.0.0.0 —— 这一步会
	// 触发 Windows 防火墙的授权弹窗,所以它必须由用户主动点。
	//
	// CaptureRect 是现读的:采集源可能在控制页被改成窗口采集,那时候
	// 屏幕坐标就对不上了,远控得跟着失效。
	//
	// trayReady 挡的是"托盘还没起来就调 SetStatus" —— systray 会把那次
	// 调用丢掉并往日志里写一行错误,而那是我们自己制造的噪音。
	var trayReady atomic.Bool

	// 主机端的批准弹窗。
	//
	// approve/deny/status 要等 remote 造出来才能填,所以先建一个只有 ask 的
	// 壳 —— OnChange 在 remote.SetEnabled 之前不会被调到(main.go 下面那段),
	// 而赋值就在它之前,顺序是安全的。和上面 applyHost 那层间接同一个道理。
	popups := &popupController{ask: notify.Ask}

	remote := remotectl.New(remotectl.Options{
		WebRTCPort: cfg.WebRTCPort,
		StreamPath: cfg.StreamPath,
		Clipboard:  clipboard.New(),
		CaptureRect: func() (screen.Rect, bool) {
			return ffmpeg.CaptureRect(manager.Config().Video)
		},
		OnChange: func(st remotectl.Status) {
			// 有人申请控制时在右下角弹窗,让主人不用切到控制页也能批准。
			// 非阻塞:里面只起一个 goroutine。
			popups.onChange(st)

			// 托盘提示里体现"有人正在控制",这是主人不在屏幕前时
			// 唯一的可见线索。
			if trayReady.Load() {
				tray.SetStatus(remoteStatusText(st))
			}
		},
	})
	defer remote.Close()
	remote.Configure(cfg.RemoteControl.Port, cfg.RemoteControl.Token, cfg.RemoteControl.AllowClipboard)

	popups.approve = remote.Approve
	popups.deny = remote.Deny
	popups.status = remote.Status

	// topoMu 串行化重建 MediaMTX。
	//
	// 有两条路会在任意时刻触发它:用户在控制页改公网地址(HTTP 处理器),
	// 以及自动映射拿到新的公网 IP(后台协程)。撞在一起就是两个 goroutine
	// 同时停、同时起同一个子进程 —— 后一个会把前一个刚拉起来的杀掉。
	var topoMu sync.Mutex

	// 公网地址变化时重建 MediaMTX:那个值写在它的配置文件里,不重建就不生效。
	//
	// 这里必须带重试。MediaMTX 不响应 stdin,Stop 实际上是强杀,而 Windows
	// 对监听端口的回收比 Linux 严格 —— 如果端口上还挂着未完全关闭的连接
	// (比如观众页面刚断开),紧接着的 bind 会失败。实测这个失败是间歇性的,
	// 加一次退避重试就能稳定通过。
	restartMTX := func(c config.Config) error {
		topoMu.Lock()
		defer topoMu.Unlock()

		p, err := mediamtx.WriteConfig(dataDir, mtxOptions(c))
		if err != nil {
			return err
		}

		const attempts = 3
		var lastErr error
		for attempt := 1; attempt <= attempts; attempt++ {
			if err := runner.Stop(); err != nil {
				lastErr = err
			}
			// 退避时间随重试次数递增
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)

			if err := runner.Start(p); err != nil {
				lastErr = err
				continue
			}
			if exited, werr := runner.WaitExit(1500 * time.Millisecond); exited {
				// 进程已经退出,再等一下让它的输出被读走 ——
				// 否则 Tail 是空的,看不到真正的原因
				time.Sleep(250 * time.Millisecond)
				lastErr = fmt.Errorf("%v\n%s", werr, joinTail(runner.Tail(20)))
				continue
			}
			return nil
		}
		return lastErr
	}

	// ── 自动端口映射 ──
	//
	// 让程序自己通过 UPnP 去路由器上开端口,省掉"进路由器后台手动加两条映射"
	// 那一步。默认关闭 —— 开端口是对外动作,而且大量路由器关着 UPnP 或在
	// 运营商级 NAT 后面,失败时必须安静退回手动模式,不能影响启动。
	//
	// applyHost 要在 server 造出来之后才能赋值,所以这里用一层间接:
	// 回调只在 mapper 启用之后才可能被调到,而启用发生在赋值之后,
	// 中间隔着一次 channel 发送,顺序是有保证的。
	var applyHost func(string)
	mapper := portmap.New(portmap.Options{
		InternalIP: ip,
		Rules:      portmap.RulesForWithControl(cfg.WebRTCPort, cfg.UDPPort, cfg.RemoteControl.MappedPort()),
		OnChange:   func(externalIP string) { applyHost(externalIP) },
	})
	defer mapper.Close()

	// ctx 要在 server 之前建好 —— "控制页关了"这个信号也是靠取消它来收尾的,
	// 而那个回调是在 server.New 时就装上去的。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(server.Options{
		Stream:           manager,
		Runner:           runner,
		MTX:              mediamtx.NewClient(cfg.APIPort),
		Encoders:         encoders,
		Presets:          presets,
		LANIP:            ip,
		ConfigPath:       cfgPath,
		Config:           cfg,
		PortMap:          mapper,
		RemoteControl:    remote,
		OnTopologyChange: restartMTX,

		// 关掉控制页 = 关掉程序。
		//
		// 控制页是本机唯一的操作入口,页面关了还留着进程,用户会以为程序
		// 已经退了,而 ffmpeg 还在采集、MediaMTX 还在对外服务。
		//
		// 远程控制是唯一的例外:它的典型用法恰恰是人不在电脑前,浏览器
		// 标签崩掉或者被误关会把整个程序连带杀掉 —— 而那个时候你没法
		// 重启它。所以远控开着的时候,关页只关页;要退出走托盘菜单。
		OnControlPageGone: func() bool {
			if remote.Status().Enabled {
				log.Printf("控制页已关闭;远程控制开着,程序继续在托盘运行")
				// true = 继续盯着:用户之后可能关掉远控再关页,
				// 那时候就该正常退出了。
				return true
			}
			log.Printf("控制页已关闭,退出程序")
			stop()
			return false
		},
	})

	applyHost = srv.ApplyAutoPublicHost
	if cfg.AutoPortMap {
		log.Printf("自动端口映射:已启用,正在查找路由器…")
		mapper.SetEnabled(true)
	}

	// 上次退出时远控是开着的,这次接着开。
	//
	// 失败不阻断启动:端口被占、采集源换成了窗口采集,都只意味着这一次
	// 没开成,控制页上会显示原因。为了一个可选的对外功能起不来整个程序
	// 是本末倒置。
	if cfg.RemoteControl.Enabled {
		if err := remote.SetEnabled(true); err != nil {
			log.Printf("远程控制未能开启: %v", err)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe(ctx, cfg.ControlPort)
	}()

	controlURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.ControlPort)
	log.Printf("控制页: %s", controlURL)
	if ip != "" {
		log.Printf("局域网观看地址: http://%s:%d/%s", ip, cfg.WebRTCPort, cfg.StreamPath)
	}

	// ── 系统托盘 ──
	//
	// 程序以 GUI 子系统构建(没有控制台窗口),托盘图标就是它"正在运行"的
	// 可见标志,也是唯一的操作入口。
	trayDone := make(chan struct{})
	if !noTray {
		go func() {
			defer close(trayDone)
			tray.Run(tray.Actions{
				OpenControl: func() { openBrowser(controlURL) },
				StartShare:  manager.Start,
				StopShare:   manager.Stop,
				Quit: func() {
					log.Printf("收到托盘退出请求")
					stop() // 取消 ctx,走正常收尾流程
				},

				// 远控的开关状态由 remotectl 持有,托盘只读它、不自己记 ——
				// 控制页那边也能开关,两处各记一份必然会走散。
				RemoteControlOn: func() bool { return remote.Status().Enabled },
				ToggleRemoteControl: func(on bool) bool {
					// 走控制页的同一段逻辑:写配置、开监听、同步路由器映射
					// 是一件事的三个面,分开做迟早会落下其中一个。
					if err := srv.SetRemoteControlEnabled(on); err != nil {
						log.Printf("远程控制:开关失败: %v", err)
						return false
					}
					return true
				},
				// 不依赖浏览器页面的那个刹车:页面卡住、断网、打不开的时候,
				// 主人仍然要能把对方踢开。
				RevokeControl: remote.Revoke,

				// 托盘就绪之后才允许更新提示文字,顺便把当前状态补上 ——
				// 之前那几次状态变化都发生在就绪之前,被挡掉了。
				OnReady: func() {
					trayReady.Store(true)
					tray.SetStatus(remoteStatusText(remote.Status()))
				},
			})
		}()
	}

	if !noBrowser {
		openBrowser(controlURL)
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Printf("正在退出…")
		// manager 和 runner 的 defer Close 会停掉子进程;
		// 即便这条路径没走到,Job Object 的 KILL_ON_JOB_CLOSE 也会兜底。
		return nil
	case <-trayDone:
		log.Printf("托盘已退出,程序结束")
		return nil
	}
}

// setupLogging 让日志同时写入文件和控制台(如果存在)。
//
// 程序以 GUI 子系统构建,没有控制台,文件是唯一的日志去处。
// 从终端运行时 stderr 仍然有效,两边都写,调试时不用去翻文件。
//
// 每次启动截断:日志是用来诊断"这一次"的,无限追加没有意义,
// 而且崩过一次之后你会先看日志再重启,不会被截断影响。
func setupLogging(dataDir string) (func(), error) {
	path := filepath.Join(dataDir, "sharescreen.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return func() {}, err
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.Printf("日志文件: %s", path)
	return func() { _ = f.Close() }, nil
}

// checkPorts 确认关键端口没被占用。
// mtxOptions 把本程序的配置映射成 MediaMTX 的配置参数。
// 启动时和公网地址变化时都会用到,所以提取出来。
func mtxOptions(c config.Config) mediamtx.Options {
	return mediamtx.Options{
		RTSPPort:        c.RTSPPort,
		RTMPPort:        c.RTMPPort,
		WebRTCPort:      c.WebRTCPort,
		UDPPort:         c.UDPPort,
		APIPort:         c.APIPort,
		StreamPath:      c.StreamPath,
		AdditionalHosts: publicHosts(c),
	}
}

// publicHosts 从 PublicHost 里取出要告诉 MediaMTX 的公网主机名。
//
// 只取主机部分,不带端口 —— webrtcAdditionalHosts 里的端口由 MediaMTX
// 自己补成它的 UDP 监听端口(8189)。所以路由器上 UDP 8189 的内外端口
// 必须一致,否则候选地址的端口就对不上了。
func publicHosts(cfg config.Config) []string {
	host := strings.TrimSpace(cfg.PublicHost)
	if host == "" {
		return nil
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		return nil
	}
	return []string{host}
}

// droppedNames 返回 after 里没有而 before 里有的编码器名,逗号分隔。
//
// 只为了日志:用户看到"可用编码器少了 amf"时,能立刻知道是启动探测
// 把它剔掉了,而不是别的地方出了问题。
func droppedNames(before, after []ffmpeg.Encoder) string {
	kept := map[string]bool{}
	for _, e := range after {
		kept[e.Name] = true
	}
	var out []string
	for _, e := range before {
		if !kept[e.Name] {
			out = append(out, e.Name)
		}
	}
	return strings.Join(out, ", ")
}

func encoderNames(list []ffmpeg.Encoder) string {
	if len(list) == 0 {
		return "(无)"
	}
	out := ""
	for i, e := range list {
		if i > 0 {
			out += ", "
		}
		out += e.Name
	}
	return out
}

func joinTail(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

// remoteStatusText 把远控状态翻译成托盘提示里的一句话。
//
// 悬停托盘图标是主人不在屏幕前时唯一能看到的状态显示,所以"有人正在控制"
// 这件事必须体现在这里,而不是只在浏览器页面上。
func remoteStatusText(st remotectl.Status) string {
	switch {
	case !st.Enabled:
		return "远程控制已关闭"
	case st.Pending != nil:
		return "有人申请控制,等你允许"
	case st.Controller != "":
		return "正在被远程控制"
	default:
		return "运行中"
	}
}

// lanIP 返回本机在局域网里的地址:别人该用它来访问这台机器,端口映射也该指向它。
//
// **不能"遍历网卡取第一个私有地址"。** Windows 上 net.Interfaces() 是按网卡
// 索引返回的,不是按路由优先级 —— 虚拟网卡(VPN、Hyper-V、WSL、蒲公英这类)
// 经常排在物理网卡前面。实测这台机器上排第一的是 astral(10.126.126.1),
// 而真正连到路由器的是 192.168.3.236。
//
// 拿错地址的后果不是"少个功能":
//   - 端口映射会让路由器把一个**不属于它局域网**的主机当成转发目标,
//     实测直接回 402 Invalid Args;
//   - 局域网观看链接会指向一个谁也连不上的地址。
//
// 而且它是偶发的:网卡顺序会随重启、VPN 连接、插拔网线变化。同一个网段里
// 今天能建上映射、明天报 402,极易被归因成"路由器抽风"。
//
// 正确做法是问路由表:朝一个公网地址拨一个 UDP 包(不实际发包),内核会按
// 默认路由选出出口地址 —— 家用网络里那就是连到路由器的那块网卡。
func lanIP() string {
	// 用 IP 字面量,不走 DNS —— 这里只是问内核要一个出口地址,
	// 解析域名会平白多一次网络往返,断网时还会卡住。
	outbound := ""
	for _, target := range []string{"8.8.8.8:80", "1.1.1.1:80"} {
		if outbound = outboundIP(target); outbound != "" {
			break
		}
	}
	return chooseLANIP(outbound, scanLANIPs())
}

// chooseLANIP 在"默认路由出口地址"和"遍历网卡扫出来的候选"之间做选择。
//
// 拆成纯函数是为了能测:真实环境下这台机器恰好有虚拟网卡,而 CI 或别人
// 的机器上没有 —— 把选择逻辑和"怎么拿到候选"分开,这条回归才有地方落。
func chooseLANIP(outbound string, scanned []string) string {
	// 默认路由的出口地址优先。它由内核按路由表给出,天然排除了那些
	// 只是"开着"但并不通往局域网的网卡。
	if isUsableLANIP(outbound) {
		return outbound
	}
	// 没有默认路由(纯内网、离线)时退回枚举结果 —— 此时它反而是对的,
	// 因为环境里通常只有一块网卡。
	for _, ip := range scanned {
		if isUsableLANIP(ip) {
			return ip
		}
	}
	return ""
}

// isUsableLANIP 判断一个地址能不能拿来当"局域网里怎么找到我"。
//
// 只收 IPv4:观看链接拼接、MediaMTX 的 ICE 候选都是按 IPv4 验证过的。
func isUsableLANIP(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	// 169.254.x.x 是 DHCP 拿不到地址时的自动配置地址,连不上任何东西
	return !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast()
}

// outboundIP 返回本机朝 target 发包时会走的那个源地址。
//
// UDP 的 Dial 不实际发包,只让内核按路由表选一个出口地址 —— 正是我们要的。
func outboundIP(target string) string {
	conn, err := net.Dial("udp", target)
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// scanLANIPs 遍历网卡,把候选地址按"私有地址在前"的顺序列出来。
//
// 顺序只是偏好,不是判据 —— 起决定作用的是 chooseLANIP 里的默认路由出口。
// 这里不再返回"第一个私有地址",因为那正是上面注释里那个 bug。
func scanLANIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var private, other []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || !isUsableLANIP(ip4.String()) {
				continue
			}
			if ip4.IsPrivate() {
				private = append(private, ip4.String())
			} else {
				other = append(other, ip4.String())
			}
		}
	}
	return append(private, other...)
}

// openBrowser 用系统默认浏览器打开 url。
func openBrowser(url string) {
	// rundll32 不需要经过 shell,避免引号和编码问题
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := cmd.Start(); err != nil {
		log.Printf("无法自动打开浏览器,请手动访问 %s", url)
	}
}
