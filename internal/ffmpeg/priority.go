package ffmpeg

import (
	"strings"

	"sharescreen/internal/gpu"
)

// Plan 返回"先试哪个、再试哪个"的候选顺序。
//
// kind 是用户选的编码器短名:
//
//   - 空串或 "auto":按 pref(驱动显示器那块显卡的厂商)优先,软件编码器恒排最后
//   - 显式短名:它排第一 —— 用户的选择优先,起不来才顺延到后面
//
// 有一条行为和以前不同:**显式指定的编码器不在这份列表里时不报错**。
// 以前这里返回「编码器 %q 在本机不可用」,那个"可用"是在启动探测里算出来的
// (见 ProbeUsable)—— 用户选的卡在这台机器上没有,报一个死错只会把他卡住,
// 盯着一个不知道该选什么的下拉框。退回自动顺序、把"你的首选没跑起来"
// 报到界面上,更解决实际问题。
func Plan(kind string, available []Encoder, pref gpu.Vendor) []Encoder {
	if len(available) == 0 {
		return nil
	}

	ordered := orderByVendor(available, pref)
	if kind == "" || kind == "auto" {
		return ordered
	}

	// 把用户指定的那个提到最前,其余保持原顺序
	for i, e := range ordered {
		if e.Kind != kind {
			continue
		}
		out := make([]Encoder, 0, len(ordered))
		out = append(out, e)
		out = append(out, ordered[:i]...)
		out = append(out, ordered[i+1:]...)
		return out
	}
	return ordered // 指定的不在列表里,退回自动顺序
}

// orderByVendor 把候选排成:与显示器同厂商的硬件 → 其余硬件 → 软件。
//
// 软件恒排最后:libx264 一定跑得起来,但它是最后手段,不该在硬件还能用的
// 时候被选中 —— 它的 CPU 开销在 2K60 下是实打实扛不住的。
func orderByVendor(list []Encoder, pref gpu.Vendor) []Encoder {
	want := kindForVendor(pref)

	var matched, otherHW, software []Encoder
	for _, e := range list {
		switch {
		case !e.Hardware:
			software = append(software, e)
		case want != "" && e.Kind == want:
			matched = append(matched, e)
		default:
			otherHW = append(otherHW, e)
		}
	}

	out := make([]Encoder, 0, len(list))
	out = append(out, matched...)
	out = append(out, otherHW...)
	out = append(out, software...)
	return out
}

// kindForVendor 是显卡厂商到编码器短名的对应。
//
// 这个映射放在 ffmpeg 包里,而不是让 gpu 包直接吐 "nvenc" 这种字符串:
// kind 的词汇表(nvenc/qsv/amf/x264)是这个包定义的,让 gpu 去知道它们
// 是把分层弄反了。
func kindForVendor(v gpu.Vendor) string {
	switch v {
	case gpu.VendorNVIDIA:
		return "nvenc"
	case gpu.VendorIntel:
		return "qsv"
	case gpu.VendorAMD:
		return "amf"
	}
	return ""
}

// encoderUnavailableMarkers 是"这个编码器在本机根本打不开"的输出特征。
//
// 用来区分两种失败:
//
//	编码器打不开      → 换下一个候选(重试同一个没有意义,它不会自己好)
//	其余(采集会话还没释放、MediaMTX 没就绪)→ 重试同一个
//
// 特征串宁可宽一点:误判只是让我们提前换个候选(多花一次尝试),
// 漏判则要白等一整轮健康检查。
var encoderUnavailableMarkers = []string{
	"Error while opening encoder",
	"Could not open encoder",
	"Unknown encoder",
	"Error initializing output stream",
	"Device creation failed",
	"No capable devices found",
	"Cannot load nvcuda.dll",
	"amfrt64.dll failed to open",
	"libmfx",
}

// EncoderUnavailable 从 ffmpeg 的输出里判断是不是"编码器起不来"。
//
// 传进来的是进程退出时抓到的输出尾部(见 stream.Manager.healthCheck)。
func EncoderUnavailable(tail string) bool {
	for _, m := range encoderUnavailableMarkers {
		if strings.Contains(tail, m) {
			return true
		}
	}
	return false
}
