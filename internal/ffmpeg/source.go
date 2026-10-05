package ffmpeg

import (
	"fmt"

	"sharescreen/internal/config"
	"sharescreen/internal/screen"
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

// SupportsRemote 报告这个采集源能不能做远程控制。
//
// 只有整屏采集可以。窗口会移动 —— 观众看到的是"某个窗口的画面",而
// 注入需要的是屏幕坐标,两者对不上;OBS 送进来的是它自己合成的画面,
// 和屏幕压根没有对应关系。
//
// 单独拆成一个纯函数(而不是去调 CaptureRect)是因为控制页要拿它提前
// 禁用开关:让用户勾上一个注定失败的开关再告诉他不行,是更差的体验。
func SupportsRemote(v config.VideoConfig) bool {
	switch v.Source {
	case config.SourceScreenDDAGrab, config.SourceScreenGDI:
		return true
	}
	return false
}

// CaptureRect 返回该采集源实际覆盖的屏幕区域。
//
// 远程控制靠它把观众鼠标的归一化坐标换算回屏幕像素,所以这个函数必须
// 和上面 inputArgs 里的参数**保持一致** —— 改采集参数时这里要一起看。
//
// 第二项为 false 表示这块区域的位置无法确定,远程控制不能在它上面工作:
// 窗口会移动,而 OBS 送进来的是它自己的合成画面,和屏幕坐标没有关系。
// 这两种源观众照常能看,只是点了没有意义 —— 与其让点击落在错误的地方,
// 不如明确禁用。
//
// 两个屏幕源的采集范围**并不一样**,这是最容易搞错的地方:
//
//   - ddagrab 没带 output_idx,默认抓 output 0,也就是**主显示器**一块。
//   - gdigrab 的 desktop 抓的是**整个虚拟桌面**,多屏时会横跨所有显示器。
//
// 所以 gdigrab 那一支必须用虚拟桌面的矩形(原点可能是负数),而不是主屏。
func CaptureRect(v config.VideoConfig) (screen.Rect, bool) {
	switch v.Source {
	case config.SourceScreenDDAGrab:
		w, h := screen.Size()
		if w <= 0 || h <= 0 {
			return screen.Rect{}, false
		}
		return screen.Rect{X: 0, Y: 0, W: w, H: h}, true

	case config.SourceScreenGDI:
		r := screen.VirtualRect()
		if r.W <= 0 || r.H <= 0 {
			return screen.Rect{}, false
		}
		return r, true

	default:
		return screen.Rect{}, false
	}
}

// needsScaling 报告是否真的需要缩放。
//
// 两种情况都算"不需要":
//   - 宽高为 0 —— 明确表示不缩放
//   - 目标尺寸恰好等于源尺寸 —— 缩放比例 1:1,缩了等于没缩
//
// 第二种容易被忽略,但代价很实在:一旦按"要缩放"处理就会走 hwdownload,
// 把帧从显存拷回内存,即使 1:1 也照样拷。2K 下每帧十六七 MB,
// 60fps 就是接近 1GB/s 的无谓带宽消耗。而且它不报错,只表现为
// CPU 占用莫名其妙地高。
func needsScaling(v config.VideoConfig, srcW, srcH int) bool {
	if v.Width <= 0 || v.Height <= 0 {
		return false
	}
	if srcW > 0 && srcH > 0 && v.Width == srcW && v.Height == srcH {
		return false
	}
	return true
}

// isZeroCopy 报告该采集源能否让硬件帧直达编码器。
//
// 只有 ddagrab 且不需要缩放时才可以。ddagrab 输出的 d3d11 帧一旦经过
// hwdownload 就变成普通内存帧,零拷贝路径即告结束。
func isZeroCopy(v config.VideoConfig, srcW, srcH int) bool {
	return v.Source == config.SourceScreenDDAGrab && !needsScaling(v, srcW, srcH)
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
func filterArgs(v config.VideoConfig, srcW, srcH int) []string {
	if !needsScaling(v, srcW, srcH) {
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
func pixFmtArgs(v config.VideoConfig, srcW, srcH int) []string {
	if isZeroCopy(v, srcW, srcH) {
		return nil
	}
	return []string{"-pix_fmt", "yuv420p"}
}
