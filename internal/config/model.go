// Package config 定义运行配置及其持久化。
package config

import "sharescreen/internal/mediamtx"

// SourceType 是采集源类型。
type SourceType string

const (
	// 屏幕,走 ddagrab(DXGI Desktop Duplication)。性能最好,但不支持单窗口。
	SourceScreenDDAGrab SourceType = "screen_ddagrab"
	// 屏幕,走 gdigrab。兼容性兜底。
	SourceScreenGDI SourceType = "screen_gdigrab"
	// 单个窗口 —— 只能用 gdigrab,ddagrab 不支持。
	SourceWindow SourceType = "window"
	// OBS 虚拟摄像头(通过 DirectShow 读取)。
	SourceOBS SourceType = "obs"
)

// ValidSource 报告 s 是否为已知的采集源。
func ValidSource(s SourceType) bool {
	switch s {
	case SourceScreenDDAGrab, SourceScreenGDI, SourceWindow, SourceOBS:
		return true
	}
	return false
}

// VideoConfig 是推流参数。
type VideoConfig struct {
	Source      SourceType `json:"source"`
	WindowTitle string     `json:"windowTitle,omitempty"` // 仅 SourceWindow 使用
	Width       int        `json:"width"`                 // 0 = 原始分辨率,不缩放
	Height      int        `json:"height"`                // 0 = 原始分辨率,不缩放
	FPS         int        `json:"fps"`
	BitrateKbps int        `json:"bitrateKbps"`
	Encoder     string     `json:"encoder"`     // "" = 自动
	KeyframeSec int        `json:"keyframeSec"` // 关键帧间隔(秒);0 视为 1
}

// Config 是完整的运行配置。
type Config struct {
	Video VideoConfig `json:"video"`

	// 实测上行带宽(Mbps)。用于计算"可支撑观众数",不参与推流本身。
	UplinkMbps float64 `json:"uplinkMbps"`

	// 公网观看地址的主机部分(域名或 IP,可带端口),留空则只显示局域网地址。
	// 程序不负责打通公网,只负责把它拼成可点的链接。
	PublicHost string `json:"publicHost,omitempty"`

	ControlPort int    `json:"controlPort"`
	RTMPPort    int    `json:"rtmpPort"`
	WebRTCPort  int    `json:"webrtcPort"`
	UDPPort     int    `json:"udpPort"`
	APIPort     int    `json:"apiPort"`
	StreamPath  string `json:"streamPath"`
}

// Default 返回默认配置。
//
// 默认不缩放(Width/Height 为 0)。这是实测结论:原生分辨率零拷贝既避开
// 了 2 倍放大的模糊,又省掉一次 983 MB/s 的 CPU 拷贝,而且 H.264 对静止
// 区域的编码几乎不耗码率,分辨率提高并不显著增加带宽占用。
func Default() Config {
	o := mediamtx.DefaultOptions()
	return Config{
		Video: VideoConfig{
			Source:      SourceScreenDDAGrab,
			Width:       0,
			Height:      0,
			FPS:         60,
			BitrateKbps: 3000,
			Encoder:     "",
			KeyframeSec: 1,
		},
		// 上行带宽只是个起点,用户应当在界面里填实测值。
		// 它只影响"可支撑观众数"的估算,不参与推流。
		UplinkMbps:  10,
		ControlPort: 8080,
		RTMPPort:    o.RTMPPort,
		WebRTCPort:  o.WebRTCPort,
		UDPPort:     o.UDPPort,
		APIPort:     o.APIPort,
		StreamPath:  o.StreamPath,
	}
}

// Normalize 修正非法或缺失的字段,让后续代码不必到处做边界判断。
func (c *Config) Normalize() {
	d := Default()

	if !ValidSource(c.Video.Source) {
		c.Video.Source = d.Video.Source
	}
	if c.Video.FPS <= 0 || c.Video.FPS > 240 {
		c.Video.FPS = d.Video.FPS
	}
	if c.Video.BitrateKbps <= 0 {
		c.Video.BitrateKbps = d.Video.BitrateKbps
	}
	if c.Video.KeyframeSec <= 0 {
		c.Video.KeyframeSec = d.Video.KeyframeSec
	}
	// 宽高要么都给要么都不给;只给一个视为不缩放
	if c.Video.Width <= 0 || c.Video.Height <= 0 {
		c.Video.Width, c.Video.Height = 0, 0
	}
	// 缩放目标必须是偶数 —— yuv420p 的要求
	c.Video.Width -= c.Video.Width % 2
	c.Video.Height -= c.Video.Height % 2

	if c.UplinkMbps <= 0 {
		c.UplinkMbps = d.UplinkMbps
	}
	if c.ControlPort <= 0 || c.ControlPort > 65535 {
		c.ControlPort = d.ControlPort
	}
	if c.RTMPPort <= 0 || c.RTMPPort > 65535 {
		c.RTMPPort = d.RTMPPort
	}
	if c.WebRTCPort <= 0 || c.WebRTCPort > 65535 {
		c.WebRTCPort = d.WebRTCPort
	}
	if c.UDPPort <= 0 || c.UDPPort > 65535 {
		c.UDPPort = d.UDPPort
	}
	if c.APIPort <= 0 || c.APIPort > 65535 {
		c.APIPort = d.APIPort
	}
	if c.StreamPath == "" {
		c.StreamPath = d.StreamPath
	}

	switch c.Video.Encoder {
	case "", "auto", "nvenc", "qsv", "amf", "x264":
		// 合法
	default:
		c.Video.Encoder = ""
	}
}

// Capacity 返回当前码率下能同时支撑的观众数。
//
// 每个观众占用一路独立码率(WebRTC 没有多播),所以这是道除法。
// 系数 0.7 是给协议开销和其他网络活动留的余量。
func (c *Config) Capacity() int {
	if c.Video.BitrateKbps <= 0 {
		return 0
	}
	usableKbps := c.UplinkMbps * 1000 * 0.7
	return int(usableKbps / float64(c.Video.BitrateKbps))
}
