// Package mediamtx 负责 MediaMTX 子进程:生成配置、启停、查询状态。
package mediamtx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Options 是生成配置时需要外部决定的参数。
type Options struct {
	RTSPPort   int    // ffmpeg 推流入口(主),只绑回环
	RTMPPort   int    // 推流入口(OBS 兼容),只绑回环
	WebRTCPort int    // WHEP 拉流 + 内置播放器页面
	UDPPort    int    // WebRTC 媒体流(ICE)
	APIPort    int    // 管理 API,只绑回环
	StreamPath string // 流路径,例如 "live"

	// AdditionalHosts 是 MediaMTX 要额外声明为"可达"的地址,必须是公网 IP
	// 或域名(可带端口)。
	//
	// 为什么必须配:WHEP 应答里的 ICE 候选默认只包含本机网卡地址
	// (192.168.x.x、IPv6 地址)。公网浏览器拿到这些地址是连不上的 ——
	// 症状是页面能打开、播放器一直转圈、媒体流永远建立不起来。
	AdditionalHosts []string
}

// DefaultOptions 返回本项目的默认端口。
func DefaultOptions() Options {
	return Options{
		RTSPPort:   8554,
		RTMPPort:   1935,
		WebRTCPort: 8889,
		UDPPort:    8189,
		APIPort:    9997,
		StreamPath: "live",
	}
}

// WriteConfig 生成一份最小化的 MediaMTX 配置并写入 dir,返回文件路径。
//
// 为什么不直接用 tools/mediamtx/mediamtx.yml:那份配置 `api: false`(拿不到
// 观众数),没有显式声明播放路径,而且开着 moq —— moq 会在工作目录里生成
// auto.key / auto.crt 自签证书。程序自持配置可以避免依赖一份可能被改动的文件。
func WriteConfig(dir string, o Options) (string, error) {
	var b strings.Builder

	fmt.Fprintf(&b, "# 由 ShareScreen 自动生成 —— 每次启动都会重写,请勿手工修改。\n\n")
	fmt.Fprintf(&b, "logLevel: info\n")
	fmt.Fprintf(&b, "logDestinations: [stdout]\n\n")

	// 管理 API:控制页靠它拿观众数。只绑回环,不对外暴露。
	fmt.Fprintf(&b, "api: yes\n")
	fmt.Fprintf(&b, "apiAddress: 127.0.0.1:%d\n\n", o.APIPort)

	// 只保留实际用到的协议,其余关掉以缩小攻击面
	fmt.Fprintf(&b, "hls: no\n")
	fmt.Fprintf(&b, "srt: no\n")
	// moq 会在工作目录生成自签证书,而我们用不到它
	fmt.Fprintf(&b, "moq: no\n\n")

	// 推流入口只绑回环 —— ffmpeg 就在本机,没有任何理由对外监听。
	//
	// 主入口是 RTSP 而不是 RTMP:RTSP 能原样携带 Opus,而 Opus 是浏览器
	// WebRTC 唯一支持的音频编码。走 RTMP 就得用 AAC,MediaMTX 不做
	// AAC→Opus 转码,观众那边直接没声音(实测)。
	fmt.Fprintf(&b, "rtsp: yes\n")
	// 只留 TCP。默认值 [udp, multicast, tcp] 会额外绑死 UDP :8000、:8001 和一段
	// 组播地址 —— 而本程序推流时明确要求走 TCP(ffmpeg 那一侧有 -rtsp_transport
	// tcp),观众走的是 WebRTC,这几个 UDP 监听一个都用不上。
	//
	// 不只是省几个端口::8000 是写死的,只要机器上已经有一个 MediaMTX 在跑,
	// 第二个就永远起不来。现象是"MediaMTX 启动失败: exit status 1",真正的错因
	// (udp :8000 被占)埋在它自己的 stderr 里,很难往"端口冲突"上想 —— 实测就是
	// 这么踩到的。关掉 UDP 之后这类冲突不复存在。
	fmt.Fprintf(&b, "rtspTransports: [tcp]\n")
	fmt.Fprintf(&b, "rtspAddress: 127.0.0.1:%d\n\n", o.RTSPPort)

	// RTMP 只为兼容 OBS 保留 —— OBS 低版本只能用 RTMP 推过来(但那样没声音)。
	fmt.Fprintf(&b, "rtmp: yes\n")
	fmt.Fprintf(&b, "rtmpAddress: 127.0.0.1:%d\n\n", o.RTMPPort)

	// WebRTC:媒体流走 UDP,信令与播放器页面走 TCP
	fmt.Fprintf(&b, "webrtc: yes\n")
	fmt.Fprintf(&b, "webrtcAddress: :%d\n", o.WebRTCPort)
	fmt.Fprintf(&b, "webrtcLocalUDPAddress: :%d\n", o.UDPPort)
	fmt.Fprintf(&b, "webrtcAllowOrigins: [\"*\"]\n")
	fmt.Fprintf(&b, "webrtcIPsFromInterfaces: yes\n")

	// 公网部署的关键一行:显式声明对外可达的地址。
	// 配了它就等于告诉 MediaMTX "除了本机网卡,这些地址也是我能被找到的地方"。
	if len(o.AdditionalHosts) > 0 {
		items := make([]string, len(o.AdditionalHosts))
		for i, h := range o.AdditionalHosts {
			items[i] = fmt.Sprintf("%q", h)
		}
		fmt.Fprintf(&b, "webrtcAdditionalHosts: [%s]\n", strings.Join(items, ", "))
	}
	b.WriteString("\n")

	// 显式声明路径(不依赖 all_others 兜底)。
	// 不设 readUser/readPass —— 播放路径不做鉴权,观众点开链接就能看。
	// 换句话说,链接本身就是凭据,别把它发到公开场合。
	fmt.Fprintf(&b, "paths:\n")
	fmt.Fprintf(&b, "  %s:\n", o.StreamPath)

	path := filepath.Join(dir, "mediamtx.yml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("写入 MediaMTX 配置失败: %w", err)
	}
	return path, nil
}

