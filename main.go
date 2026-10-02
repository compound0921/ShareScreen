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
	"syscall"
	"time"

	"sharescreen/internal/config"
	"sharescreen/internal/ffmpeg"
	"sharescreen/internal/mediamtx"
	"sharescreen/internal/paths"
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

	if err := run(*cfgPath, *printArgs, *noBrowser, *noTray); err != nil {
		log.Fatalf("错误: %v", err)
	}
}

func run(cfgPath string, printArgs, noBrowser, noTray bool) error {
	// ── 数据目录与日志 ──
	dataDir, err := paths.DataDir()
	if err != nil {
		return err
	}
	if closeLog, lerr := setupLogging(dataDir); lerr != nil {
		log.Printf("警告: 无法写入日志文件: %v", lerr)
	} else {
		defer closeLog()
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
	encoders, err := ffmpeg.ProbeEncoders(ffmpegTool.Exe)
	if err != nil {
		log.Printf("警告: 编码器探测失败: %v", err)
	} else {
		log.Printf("可用编码器: %s", encoderNames(encoders))
	}

	desktopW, desktopH := screen.Size()
	presets := config.ResolutionPresets(desktopW, desktopH)
	log.Printf("桌面分辨率: %d×%d", desktopW, desktopH)

	// ── -print-args:只打印命令,不启动任何东西 ──
	if printArgs {
		enc, err := ffmpeg.ResolveEncoder(cfg.Video.Encoder, encoders)
		if err != nil {
			return err
		}
		target := fmt.Sprintf("rtmp://127.0.0.1:%d/%s", cfg.RTMPPort, cfg.StreamPath)
		args, err := ffmpeg.BuildArgs(cfg.Video, enc, target, desktopW, desktopH)
		if err != nil {
			return err
		}
		fmt.Println(ffmpeg.Describe(ffmpegTool.Exe, args))
		return nil
	}

	// ── 端口预检 ──
	// 最常见的失败是上一次没退干净,或者用户自己开了 MediaMTX。
	// 提前报清楚,好过让 MediaMTX 启动失败后界面一片沉默。
	if err := checkPorts(cfg); err != nil {
		return err
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
	defer manager.Close()

	// ── 控制页服务 ──
	ip := lanIP()

	// 公网地址变化时重建 MediaMTX:那个值写在它的配置文件里,不重建就不生效。
	//
	// 这里必须带重试。MediaMTX 不响应 stdin,Stop 实际上是强杀,而 Windows
	// 对监听端口的回收比 Linux 严格 —— 如果端口上还挂着未完全关闭的连接
	// (比如观众页面刚断开),紧接着的 bind 会失败。实测这个失败是间歇性的,
	// 加一次退避重试就能稳定通过。
	restartMTX := func(c config.Config) error {
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

	srv := server.New(server.Options{
		Stream:           manager,
		Runner:           runner,
		MTX:              mediamtx.NewClient(cfg.APIPort),
		Encoders:         encoders,
		Presets:          presets,
		LANIP:            ip,
		ConfigPath:       cfgPath,
		Config:           cfg,
		OnTopologyChange: restartMTX,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
func checkPorts(cfg config.Config) error {
	type probe struct {
		name string
		addr string
	}
	probes := []probe{
		{"控制页", fmt.Sprintf("127.0.0.1:%d", cfg.ControlPort)},
		{"ffmpeg 推流入口", fmt.Sprintf("127.0.0.1:%d", cfg.RTMPPort)},
		{"MediaMTX 管理接口", fmt.Sprintf("127.0.0.1:%d", cfg.APIPort)},
		{"WebRTC 播放端口", fmt.Sprintf("0.0.0.0:%d", cfg.WebRTCPort)},
	}

	for _, p := range probes {
		ln, err := net.Listen("tcp", p.addr)
		if err != nil {
			return fmt.Errorf(
				"%s 的端口 %s 已被占用。\n"+
					"多半是上一次没有正常退出,或者你自己开着 MediaMTX。\n"+
					"请先关掉占用该端口的程序再试。",
				p.name, p.addr)
		}
		_ = ln.Close()
	}
	return nil
}

// mtxOptions 把本程序的配置映射成 MediaMTX 的配置参数。
// 启动时和公网地址变化时都会用到,所以提取出来。
func mtxOptions(c config.Config) mediamtx.Options {
	return mediamtx.Options{
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

// lanIP 返回本机的局域网 IPv4 地址。优先私有地址段。
func lanIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var fallback string
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
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			if ip4.IsPrivate() {
				return ip4.String()
			}
			if fallback == "" {
				fallback = ip4.String()
			}
		}
	}
	return fallback
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
