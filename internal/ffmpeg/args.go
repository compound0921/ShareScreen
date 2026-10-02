package ffmpeg

import (
	"fmt"
	"strconv"

	"sharescreen/internal/config"
)

// Encoder 描述一个 H.264 编码器。
type Encoder struct {
	Name     string `json:"name"`     // ffmpeg 的编码器名,如 h264_nvenc
	Label    string `json:"label"`    // 界面显示名
	Kind     string `json:"kind"`     // 配置里保存的短名,如 nvenc
	Hardware bool   `json:"hardware"` // 是否硬件加速
}

// knownEncoders 按优先级排列 —— 越靠前越优先被"自动"选中。
var knownEncoders = []Encoder{
	{Name: "h264_nvenc", Label: "NVIDIA NVENC(硬件)", Kind: "nvenc", Hardware: true},
	{Name: "h264_qsv", Label: "Intel Quick Sync(硬件)", Kind: "qsv", Hardware: true},
	{Name: "h264_amf", Label: "AMD AMF(硬件)", Kind: "amf", Hardware: true},
	{Name: "libx264", Label: "x264(软件,CPU 占用高)", Kind: "x264", Hardware: false},
}

// BuildArgs 组装完整的 ffmpeg 参数(不含可执行文件本身)。
//
// target 是推流地址,例如 rtmp://127.0.0.1:1935/live。
// srcW/srcH 是采集源的原始尺寸(桌面分辨率),用来判断"目标尺寸等于源尺寸"
// 这种不需要缩放的伪缩放情况;传 0 表示未知。
//
// 参数顺序遵循 ffmpeg 的约定:输入选项在 -i 之前,输出选项在输入之后。
func BuildArgs(v config.VideoConfig, enc Encoder, target string, srcW, srcH int) ([]string, error) {
	input, err := inputArgs(v)
	if err != nil {
		return nil, err
	}

	args := []string{"-hide_banner"}
	args = append(args, input...)

	// 恒定帧率。实测不加这个,输出帧率会稳定漂到 57–58。
	args = append(args, "-fps_mode", "cfr")

	args = append(args, filterArgs(v, srcW, srcH)...)
	args = append(args, encoderArgs(enc, v)...)

	// 关键帧间隔。WebRTC 依赖关键帧做快速起播和流切换,1 秒一个。
	args = append(args, "-g", strconv.Itoa(v.FPS*keyframeSec(v)))

	// B 帧需要重排序缓冲,是低延迟的头号杀手。
	// 注意:永远不要加 -multipass —— 实测它与 -tune ll 冲突,直接报错。
	args = append(args, "-bf", "0")

	args = append(args, pixFmtArgs(v, srcW, srcH)...)

	// 输出走 RTSP 而不是 RTMP:只有 RTSP 能原样携带 Opus,而 Opus 是
	// 浏览器 WebRTC 唯一支持的音频编码。RTMP 只能带 AAC,MediaMTX 又
	// 不做 AAC→Opus 转码,结果就是观众那边只有画面没有声音(实测)。
	// 显式指定 TCP —— RTSP 默认走 UDP,丢包在推流侧是无法恢复的。
	args = append(args, "-f", "rtsp", "-rtsp_transport", "tcp", target)

	return args, nil
}

func keyframeSec(v config.VideoConfig) int {
	if v.KeyframeSec <= 0 {
		return 1
	}
	return v.KeyframeSec
}

func encoderArgs(enc Encoder, v config.VideoConfig) []string {
	br := fmt.Sprintf("%dk", v.BitrateKbps)
	rate := []string{"-b:v", br, "-maxrate", br, "-bufsize", br}

	var head []string
	switch enc.Kind {
	case "nvenc":
		// p7 是最高质量的预设。硬件编码器的余量非常大(实测见架构设计 §5.4),
		// 选最慢的预设不吃亏。
		head = []string{"-c:v", "h264_nvenc", "-preset", "p7", "-tune", "ll", "-rc", "cbr"}
	case "qsv":
		head = []string{"-c:v", "h264_qsv", "-preset", "veryfast"}
	case "amf":
		head = []string{"-c:v", "h264_amf", "-usage", "lowlatency", "-quality", "speed", "-rc", "cbr"}
	default:
		head = []string{"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency"}
	}
	return append(head, rate...)
}

// Describe 返回一段可读的命令行,供界面展示和排查问题。
func Describe(exe string, args []string) string {
	out := exe
	for _, a := range args {
		out += " " + quoteIfNeeded(a)
	}
	return out
}

func quoteIfNeeded(s string) string {
	for _, r := range s {
		if r == ' ' || r == '"' {
			return strconv.Quote(s)
		}
	}
	return s
}
