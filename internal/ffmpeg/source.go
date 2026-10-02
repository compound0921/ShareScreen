package ffmpeg

import (
	"fmt"

	"sharescreen/internal/config"
)

// obsDeviceName 是 OBS 虚拟摄像头在 DirectShow 里的设备名。
// 可用 `ffmpeg -list_devices true -f dshow -i dummy` 查看实际名称。
const obsDeviceName = "OBS Virtual Camera"

// inputArgs 返回采集源的输入段参数。
func inputArgs(v config.VideoConfig) ([]string, error) {
	fps := fmt.Sprintf("%d", v.FPS)

	switch v.Source {
	case config.SourceScreenDDAGrab:
		// ddagrab 是 lavfi 源滤镜,不是设备。
		// 它输出 d3d11 硬件帧,可直接零拷贝进 NVENC。
		return []string{
			"-f", "lavfi",
			"-i", fmt.Sprintf("ddagrab=framerate=%d", v.FPS),
		}, nil

	case config.SourceScreenGDI:
		return []string{
			"-f", "gdigrab",
			"-framerate", fps,
			"-i", "desktop",
		}, nil

	case config.SourceWindow:
		if v.WindowTitle == "" {
			return nil, fmt.Errorf("窗口采集需要填写窗口标题")
		}
		// title= 匹配标题中包含该字符串的窗口。
		// 中文标题能正确传递 —— 我们走 exec.Command 的参数切片,不经过 shell。
		return []string{
			"-f", "gdigrab",
			"-framerate", fps,
			"-i", "title=" + v.WindowTitle,
		}, nil

	case config.SourceOBS:
		return []string{
			"-f", "dshow",
			"-framerate", fps,
			"-i", "video=" + obsDeviceName,
		}, nil
	}

	return nil, fmt.Errorf("未知采集源: %s", v.Source)
}

// isZeroCopy 报告该采集源能否让硬件帧直达编码器。
//
// 只有 ddagrab 在"不缩放"时才可以。ddagrab 输出的 d3d11 帧一旦经过
// hwdownload 就变成普通内存帧,零拷贝路径即告结束。
func isZeroCopy(v config.VideoConfig) bool {
	return v.Source == config.SourceScreenDDAGrab && v.Width <= 0
}

// filterArgs 返回缩放相关的滤镜参数。
//
// 这里有三个实测踩出来的坑:
//
//  1. ddagrab 不缩放时必须**完全不产生滤镜链**,才能保持零拷贝。
//  2. ddagrab 要缩放时,唯一可行的写法是 hwdownload 回到内存再缩放。
//     GPU 侧缩放的两条路都走不通:scale_d3d11 报 "Unsupported pixel format",
//     hwmap=derive_device=cuda + scale_cuda 报 "Function not implemented"。
//     代价是每个全屏帧多一次 CPU 拷贝(2K 分辨率下约 16 MB/帧)。
//  3. 非 ddagrab 的源本来就是内存帧,普通 scale 即可。
func filterArgs(v config.VideoConfig) []string {
	if v.Width <= 0 || v.Height <= 0 {
		return nil
	}
	scale := fmt.Sprintf("scale=%d:%d", v.Width, v.Height)

	if v.Source == config.SourceScreenDDAGrab {
		return []string{"-vf", "hwdownload,format=bgra," + scale}
	}
	return []string{"-vf", scale}
}

// pixFmtArgs 返回像素格式参数。
//
// ddagrab 零拷贝路径下必须**不传** -pix_fmt:强行指定 yuv420p 会让 ffmpeg
// 在 d3d11 硬件帧和软件格式之间插入一次不可能的转换,直接报
// "Impossible to convert between the formats supported by the filter"。
// 这种情况下 NVENC 内部会自行处理格式。
func pixFmtArgs(v config.VideoConfig) []string {
	if isZeroCopy(v) {
		return nil
	}
	return []string{"-pix_fmt", "yuv420p"}
}
