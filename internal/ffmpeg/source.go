package ffmpeg

import (
	"fmt"

	"sharescreen/internal/config"
	"sharescreen/internal/screen"
)

// obsDeviceName 是 OBS 虚拟摄像头在 DirectShow 里的设备名。
// 可用 `ffmpeg -list_devices true -f dshow -i dummy` 查看实际名称。
const obsDeviceName = "OBS Virtual Camera"

// CaptureOpts 是采集阶段的**行为**开关(相对于 VideoConfig 描述的"采成什么样")。
//
// 单独拆出来是为了让这个包不必知道远程控制这个功能的存在:BuildArgs 是
// 纯命令构造器,只该收到已经决定好的值。策略由 CaptureOptsFor 一处给出。
type CaptureOpts struct {
	// DrawMouse 是否把系统光标画进画面。
	//
	// 默认 true —— 观看时能看见对方的光标是有用的。远程控制开启时必须
	// 关掉,否则控制者看到的唯一指针是 0.3–1 秒前的那一个,操作起来像
	// 在拖东西。关掉之后指针改由控制端自己画(浏览器原生光标,零延迟)。
	// 见 docs/架构设计.md §12.11。
	DrawMouse bool
}

// CaptureOptsFor 由"采集源 + 远控是否开启"推出采集行为开关。
//
// 这是光标门控的**唯一**一处定义。多个调用点各写一遍的话,迟早会在某个
// 分支上分叉,而症状是"光标有时候画、有时候不画",极难归因。
//
// 注意门控依据的是远控**开关本身**,不是"此刻有没有人持有控制权":后者
// 要求在有人取到控制权的那一刻重启采集,会在会话中途黑屏。
// 代价是远控开着时观看端也看不到光标,这是已知且接受的取舍。
func CaptureOptsFor(v config.VideoConfig, remoteEnabled bool) CaptureOpts {
	return CaptureOpts{
		// 采集源不支持远控时,开关没有意义,光标要留着 ——
		// 否则就是白白弄丢一个正常观看需要的功能。
		DrawMouse: !(remoteEnabled && SupportsRemote(v)),
	}
}

// inputArgs 返回采集源的输入段参数。
func inputArgs(v config.VideoConfig, o CaptureOpts) ([]string, error) {
	fps := fmt.Sprintf("%d", v.FPS)

	switch v.Source {
	case config.SourceScreenDDAGrab:
		// ddagrab 是 lavfi 源滤镜,不是设备。
		// 它输出 d3d11 硬件帧,可直接零拷贝进 NVENC。
		//
		// 不画光标也仍然维持帧率:ddagrab 的 dup_frames 默认为 true
		// (画面没更新时重复上一帧),外面还有 -fps_mode cfr 兜底。
		// 附带好处是纯指针移动时画面不变,那些重复帧几乎不占码率。
		f := fmt.Sprintf("ddagrab=framerate=%d", v.FPS)
		if !o.DrawMouse {
			f += ":draw_mouse=0"
		}
		return []string{"-f", "lavfi", "-i", f}, nil

	case config.SourceScreenGDI:
		args := []string{"-f", "gdigrab", "-framerate", fps}
		if !o.DrawMouse {
			// 必须是 -i 之前的输入选项 —— 它不是滤镜,放进 -vf 会被拒绝。
			args = append(args, "-draw_mouse", "0")
		}
		return append(args, "-i", "desktop"), nil

	// 下面两种源**不理会** o.DrawMouse,光标一律画。
	//
	// 不是漏了:这两种源都不支持远程控制(见 SupportsRemote),而
	// CaptureOptsFor 正是按它推的 —— 走正常路径到不了这里时 DrawMouse
	// 必定为 true。真收到 false 只可能是调用方绕过了 CaptureOptsFor。
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
