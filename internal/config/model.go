// Package config 定义运行配置及其持久化。
package config

import (
	"strings"

	"sharescreen/internal/mediamtx"
)

// SourceType 是采集源类型。
type SourceType string

const (
	// 屏幕,走 ddagrab(DXGI Desktop Duplication)。性能最好,但不支持单窗口。
	SourceScreenDDAGrab SourceType = "screen_ddagrab"
	// 屏幕,走 gdigrab。兼容性兜底。
	SourceScreenGDI SourceType = "screen_gdigrab"
	// 单个窗口 —— 只能用 gdigrab,ddagrab 不支持。
	SourceWindow SourceType = "window"
	// OBS 虚拟摄像头(通过 DirectShow 读取)。只有画面 —— 虚拟摄像头
	// 是纯视频设备,OBS 采到的音频不会跟着它走。
	SourceOBS SourceType = "obs"

	// OBS 直接推流到 MediaMTX,本程序不启动 ffmpeg。
	//
	// 在程序自己支持桌面音频采集之前,这是唯一能带声音的方式。现在
	// 音频已经内置(见 AudioConfig),这个源保留给需要 OBS 做更复杂
	// 画面合成的场景 —— 比如多个来源叠加、加滤镜、加转场。
	//
	// 必须走 WHIP 而不是 RTMP:RTMP 只能带 AAC,浏览器的 WebRTC 不认,
	// 而 MediaMTX 不做 AAC→Opus 转码(实测)。
	SourceOBSPush SourceType = "obs_push"
)

// ValidSource 报告 s 是否为已知的采集源。
func ValidSource(s SourceType) bool {
	switch s {
	case SourceScreenDDAGrab, SourceScreenGDI, SourceWindow, SourceOBS, SourceOBSPush:
		return true
	}
	return false
}

// External 报告该采集源是否由外部程序推流。
//
// 为真时本程序不启动 ffmpeg —— MediaMTX 直接接收 OBS 推过来的流。
// 分辨率、帧率、编码器这些参数都由 OBS 那边决定,我们只保留码率
// 用来估算能支撑几个观众。
func (v VideoConfig) External() bool {
	return v.Source == SourceOBSPush
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

// AudioConfig 是桌面音频采集参数。
//
// 音频由本程序自己用 WASAPI 回环采集(见 internal/audio),因为在 Windows 上
// ffmpeg 拿不到桌面音频。采集到的是设备原始 PCM,由 ffmpeg 编码成 Opus ——
// 只有 Opus 能被浏览器 WebRTC 直接播放。
type AudioConfig struct {
	// Enabled 打开后用默认播放设备的声音。采的是"你听到什么",
	// 换耳机、切 HDMI、插蓝牙都会自动跟随,不需要改系统设置。
	Enabled bool `json:"enabled"`

	// BitrateKbps 是 Opus 的输出码率。音频码率本身很低,
	// 96k 立体声已经接近透明,再往上加听不出区别。
	BitrateKbps int `json:"bitrateKbps"`
}

// RemoteControlConfig 是远程控制的配置。
//
// 默认**关闭**,这一点是有意的:开启远控意味着程序要监听一个对外端口,
// 而不开启时它一个端口都不多占。和 AutoPortMap 一样,"对外动作不该在
// 用户没要求的时候做"。
type RemoteControlConfig struct {
	Enabled bool `json:"enabled"`

	// Port 是观看页和输入通道共用的端口(HTTP + WebSocket)。
	//
	// 和 WHEP 分端口是必须的:那个端口由 MediaMTX 占着,而我们需要在
	// 自己的页面里同时提供 WHEP 播放、输入回传和控制权信令。
	Port int `json:"port"`

	// Token 是链接凭据,首次开启时生成并留在配置文件里。
	//
	// 它只决定"能不能连上来看"。真正能不能**操作**由主机当场批准决定,
	// 见 internal/remotectl。两层是分开的:链接泄露出去,陌生人也只能看。
	//
	// 不设 omitempty —— 空值也是有意义的状态(尚未生成),留着更容易排查。
	Token string `json:"token"`

	// AllowClipboard 打开后控制者与主机之间双向同步剪贴板文本。
	//
	// 单独一个开关而不是跟着 Enabled 走:剪贴板会把手边的密码、验证码
	// 之类的东西送出去,这个决定应该由用户单独做一次。
	AllowClipboard bool `json:"allowClipboard,omitempty"`
}

// MappedPort 返回这个远控配置需要在路由器上开的端口;关着的时候返回 0,
// 表示一条映射都不该有。
//
// 单独一个方法是因为这个判断有两个调用点(程序启动时装配映射规则、
// 远控开关变化时刷新规则),而它们必须永远一致:一处按"关掉就不映射"、
// 另一处漏了,结果就是关掉远控之后路由器上还留着一个对着公网的洞。
func (r RemoteControlConfig) MappedPort() int {
	if !r.Enabled {
		return 0
	}
	return r.Port
}

// Config 是完整的运行配置。
type Config struct {
	Video VideoConfig `json:"video"`
	Audio AudioConfig `json:"audio"`

	// 实测上行带宽(Mbps)。用于计算"可支撑观众数",不参与推流本身。
	UplinkMbps float64 `json:"uplinkMbps"`

	// 公网观看地址的主机部分(域名或 IP,可带端口),留空则只显示局域网地址。
	// 程序不负责打通公网,只负责把它拼成可点的链接。
	PublicHost string `json:"publicHost,omitempty"`

	// 当前的 PublicHost 是程序自动写入的(来自 UPnP 拿到的公网地址),
	// 而不是用户手填或 DDNS。
	//
	// 它唯一的作用是决定 AutoPortMap 能不能覆盖 PublicHost:用户手填的值
	// (比如一个 DDNS 域名)永远不该被程序改掉,而自动写入的值必须跟着
	// 公网 IP 走 —— 家宽 IP 是会变的。
	PublicHostAuto bool `json:"publicHostAuto,omitempty"`

	// LanHost 是**局域网链接**里该显示的主机名,留空表示用自动探测到的那个。
	//
	// 只管链接。端口映射指向哪台主机不走这里 —— 那是路由表决定的事实,
	// 不是偏好:选错一块网卡会让路由器收到一个不属于它局域网的内网地址,
	// 直接回 402 Invalid Args(实测踩过)。多网卡(接了 VPN、Hyper-V、
	// 蒲公英之类)时自动挑的那块未必是同伴能连上的,所以链接这一侧留个口子。
	//
	// 没有配套的 auto 标记:没有任何机制会自动写它(公网那边有 UPnP,
	// 所以才有 PublicHostAuto),所以标记会没有作者。空 = 自动,够了。
	LanHost string `json:"lanHost,omitempty"`

	// AutoPortMap 打开后由程序自己通过 UPnP 在路由器上建立端口映射,
	// 不需要用户进路由器后台手动配。
	//
	// 默认关闭:开端口是对外动作,不该在用户没要求的时候做。而且大量路由器
	// 关着 UPnP 或处于运营商级 NAT 之后,开了也可能用不了 —— 那种情况下
	// 程序必须安静地退回手动模式,不能影响启动。
	AutoPortMap bool `json:"autoPortMap,omitempty"`

	// 远程控制(观众经批准后操作本机)。默认关闭,见 RemoteControlConfig。
	RemoteControl RemoteControlConfig `json:"remoteControl"`

	ControlPort int    `json:"controlPort"`
	RTSPPort    int    `json:"rtspPort"`
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
		// 默认打开音频。采不到时程序会退化成无声画面并给出警告,
		// 不会因此起不来 —— 所以默认开着是安全的。
		Audio: AudioConfig{
			Enabled:     true,
			BitrateKbps: 96,
		},
		// 上行带宽只是个起点,用户应当在界面里填实测值。
		// 它只影响"可支撑观众数"的估算,不参与推流。
		UplinkMbps: 10,
		// 远程控制默认关闭,也不生成令牌 —— 令牌在用户第一次开启时
		// 才创建,免得每个从没用过这个功能的配置文件里都躺着一串秘密。
		RemoteControl: RemoteControlConfig{
			Port: 8090,
		},
		ControlPort: 8080,
		RTSPPort:    o.RTSPPort,
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

	if c.Audio.BitrateKbps <= 0 || c.Audio.BitrateKbps > 512 {
		c.Audio.BitrateKbps = d.Audio.BitrateKbps
	}

	if c.UplinkMbps <= 0 {
		c.UplinkMbps = d.UplinkMbps
	}
	if c.ControlPort <= 0 || c.ControlPort > 65535 {
		c.ControlPort = d.ControlPort
	}
	if c.RTSPPort <= 0 || c.RTSPPort > 65535 {
		c.RTSPPort = d.RTSPPort
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

	// 远控端口不能和别人撞车。控制页那个服务只绑回环、远控要绑 0.0.0.0,
	// 撞上之后谁先起来另一个就起不来,而且报错是完全没有线索的
	// "address already in use"。这里直接退回默认端口,并由启用时的
	// 绑定失败把真正的冲突报给用户。
	if c.RemoteControl.Port <= 0 || c.RemoteControl.Port > 65535 ||
		c.RemoteControl.Port == c.ControlPort ||
		c.RemoteControl.Port == c.WebRTCPort ||
		c.RemoteControl.Port == c.APIPort ||
		c.RemoteControl.Port == c.RTSPPort ||
		c.RemoteControl.Port == c.RTMPPort ||
		c.RemoteControl.Port == c.UDPPort {
		c.RemoteControl.Port = d.RemoteControl.Port
	}

	// 令牌只可能是 crypto/rand 32 字节的 base64url 编码(43 个字符)。
	// 明显短于这个长度说明文件被人改过或者写坏了 —— 清掉,下次开启时
	// 重新生成。宁可让旧链接失效,也不要拿一个弱令牌当凭据。
	if len(c.RemoteControl.Token) < 32 {
		c.RemoteControl.Token = ""
	}
	// 没有公网地址就无所谓"是不是自动写的"。不归零的话,用户清空地址之后
	// 这个标记会一直挂着,下次自动写入时看不出区别,但配置读起来是矛盾的。
	if c.PublicHost == "" {
		c.PublicHostAuto = false
	}

	// LanHost 会被拼进 "http://<这里>:8889/...",所以带着协议头或斜杠拼出来
	// 就是一条死链。用户从别处粘一个完整 URL 进来是很自然的动作,这里替他
	// 剥掉协议头;剩下的只要有斜杠、冒号或空白就整条丢弃,退回自动。
	//
	// **不要求它必须是个能解析的 IP**:`myhost.local` 这类合法主机名要留得住,
	// 而一个格式对但网段不对的 IP(192.168.99.99)也不该被"智能修正" ——
	// 那正是下拉里那堆候选存在的意义。
	c.LanHost = strings.TrimSpace(c.LanHost)
	c.LanHost = strings.TrimPrefix(strings.TrimPrefix(c.LanHost, "https://"), "http://")
	if strings.ContainsAny(c.LanHost, "/: \t") {
		c.LanHost = ""
	}

	switch c.Video.Encoder {
	case "", "auto", "nvenc", "qsv", "amf", "x264":
		// 合法
	default:
		c.Video.Encoder = ""
	}
}

// CanAutoSetPublicHost 报告自动端口映射能不能改写公网地址。
//
// 用户手填的地址(DDNS 域名之类)永远不动 —— 他填那个是有意的,程序拿到的
// 公网 IP 反而会变。只有地址是空的、或者上一次就是程序自己写进去的,才可以改。
func (c *Config) CanAutoSetPublicHost() bool {
	return c.PublicHost == "" || c.PublicHostAuto
}

// Capacity 返回当前码率下能同时支撑的观众数。
//
// 每个观众占用一路独立码率(WebRTC 没有多播),所以这是道除法。
// 系数 0.7 是给协议开销和其他网络活动留的余量。
//
// 音频码率也要算进去 —— 它虽然小(96k 对比视频的 3000k),但乘以观众数
// 之后不是可以忽略的量,漏掉会让估算偏乐观。
func (c *Config) Capacity() int {
	total := c.Video.BitrateKbps
	if c.Audio.Enabled {
		total += c.Audio.BitrateKbps
	}
	if total <= 0 {
		return 0
	}
	usableKbps := c.UplinkMbps * 1000 * 0.7
	return int(usableKbps / float64(total))
}
