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
	RTMPPort   int    // ffmpeg 推流入口,只绑回环
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

	// 播放路径的凭据。留空表示不鉴权 —— 只在纯局域网使用时可接受,
	// 一旦端口映射到公网,不鉴权等于把屏幕公开。
	ReadUser string
	ReadPass string
}

// DefaultOptions 返回本项目的默认端口。
func DefaultOptions() Options {
	return Options{
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

	// 只保留实际用到的两个协议,其余关掉以缩小攻击面
	fmt.Fprintf(&b, "rtsp: no\n")
	fmt.Fprintf(&b, "hls: no\n")
	fmt.Fprintf(&b, "srt: no\n")
	// moq 会在工作目录生成自签证书,而我们用不到它
	fmt.Fprintf(&b, "moq: no\n\n")

	// 推流入口只绑回环 —— ffmpeg 就在本机,没有任何理由对外监听
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

	// 显式声明路径(不依赖 all_others 兜底)
	fmt.Fprintf(&b, "paths:\n")
	fmt.Fprintf(&b, "  %s:\n", o.StreamPath)
	if o.ReadUser != "" && o.ReadPass != "" {
		fmt.Fprintf(&b, "    readUser: %s\n", yamlString(o.ReadUser))
		fmt.Fprintf(&b, "    readPass: %s\n", yamlString(o.ReadPass))
	}

	path := filepath.Join(dir, "mediamtx.yml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("写入 MediaMTX 配置失败: %w", err)
	}
	return path, nil
}

// yamlString 把任意字符串安全地写成 YAML 双引号标量。
//
// 一律加引号而不是"只在需要时加":凭据是随机生成的,与其去想哪些字符会
// 和 YAML 语法冲突,不如统一处理。非 ASCII 字符直接原样保留 —— 文件是
// UTF-8 写的,YAML 接受。
func yamlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
