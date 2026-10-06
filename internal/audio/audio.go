// Package audio 采集 Windows 桌面音频。
//
// 用 WASAPI 的回环模式抓默认播放设备正在输出的声音 —— 也就是"你听到什么,
// 观众就听到什么"。不需要用户改任何系统设置,换耳机、切 HDMI、插蓝牙都自动跟随。
//
// 为什么不用 ffmpeg 采:实测这版 ffmpeg 没有任何能拿到桌面音频的输入设备。
// 它没有 WASAPI 输入设备(只有 dshow 和 openal),而 dshow 里唯一可见的录音
// 设备是麦克风 —— 现代声卡普遍不再提供"立体声混音",这台机器上它存在但被
// Windows 禁用了,程序无法自行启用。openal 更是直接返回 Invalid Value。
// 所以只能自己实现。
//
// 采集输出的是设备混音格式的原始 PCM 字节,不做任何转换 —— 采样格式和声道数
// 由 Format 报告,调用方据此构造 ffmpeg 的输入参数。
package audio

import "strconv"

// Device 是一个可以采集的播放设备(音频端点)。
//
// 之所以要能选:id 为空表示"跟随系统默认设备",但**系统默认设备不一定是有声音
// 的那一台**。最常见的是笔记本接了 HDMI 显示器 —— 声音还从笔记本自己的扬声器
// 出来,系统默认却指到了显示器那一路没人播放的输出上,于是回环采到的是静音,
// 观众那边就是"有画面、一点声音没有"。这种情况程序猜不出来(它没法知道声音
// 到底从哪台设备出来),只能让用户指定。
type Device struct {
	// ID 是 Windows 的端点 ID 字符串,可长期保存 —— 插拔设备之后同一个
	// 设备的 ID 不变,变了说明设备真的换了。
	ID   string `json:"id"`
	Name string `json:"name"`

	// Default 表示这是当前的系统默认播放设备。
	Default bool `json:"default"`
}

// Format 描述采集到的 PCM 格式。
//
// 这些值来自音频引擎的混音格式,不能由我们指定:共享模式下格式由引擎决定,
// 应用只能接受。所以 ffmpeg 的参数必须照着这里填,不能想当然写成 48kHz 立体声。
type Format struct {
	SampleRate int `json:"sampleRate"`
	Channels   int `json:"channels"`

	// SampleFmt 是 ffmpeg 的采样格式名,如 f32le、s16le。
	// 采集直接输出对应格式的原始字节,不做转换。
	SampleFmt string `json:"sampleFmt"`

	// BlockAlign 是每帧(所有声道加起来)的字节数,用于把帧数换算成字节数。
	BlockAlign int `json:"blockAlign"`
}

// String 返回便于日志阅读的描述。
func (f Format) String() string {
	return f.SampleFmt + " " + strconv.Itoa(f.SampleRate) + "Hz " +
		strconv.Itoa(f.Channels) + "声道"
}
