package ffmpeg

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const createNoWindow = 0x08000000

// ProbeEncoders 询问本机的 ffmpeg **构建里**有哪些 H.264 编码器。
//
// 注意它回答的不是"这台机器能用哪些":那个列表是编译期的属性,装的是
// gyan.dev 的完整构建时,一台只有 A 卡的机器照样会列出 h264_nvenc。
// 实测:本机没有 AMD 显卡,h264_amf 依然在列表里,真跑报
// `[AMF] DLL amfrt64.dll failed to open`。
//
// 所以它只是第一步 —— 拿到候选之后还要过一遍 ProbeUsable,那才是
// "能不能用"的依据。
func ProbeEncoders(exe string) ([]Encoder, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "-hide_banner", "-encoders")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("执行 ffmpeg -encoders 失败: %w", err)
	}

	// 输出形如:" V....D h264_nvenc    NVIDIA NVENC H.264 encoder (codec h264)"
	// 第二个字段是编码器名。
	available := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			available[fields[1]] = true
		}
	}

	var found []Encoder
	for _, e := range knownEncoders { // 按 knownEncoders 的优先级顺序
		if available[e.Name] {
			found = append(found, e)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("本机 ffmpeg 未提供任何可用的 H.264 编码器")
	}
	return found, nil
}

// probeTimeout 是单个编码器试跑的上限。
//
// 实测:能用的编码器编一帧要 46 毫秒(libx264)到 1.2 秒(h264_qsv,
// 初始化慢),打不开的 30 毫秒就退出了。4 秒是给慢机器留的余量。
const probeTimeout = 4 * time.Second

// ProbeUsable 用合成画面让每个编码器真编一帧,剔掉跑不起来的。
//
// 这是"可用"这个说法的唯一依据(ProbeEncoders 给的是编译期列表)。
// 四个编码器并发跑,总共约 1.2 秒 —— 瓶颈是 h264_qsv 的初始化。
//
// 两处刻意的保守处理:
//
//   - **超时视为不确定,保留**。别把一台忙碌机器上的编码器误杀:留下来
//     最多是发起推流时多试一次,剔掉则可能让用户没得选。
//   - **全军覆没时返回原列表**。一个都跑不通说明探测本身出了问题(ffmpeg
//     被杀、磁盘满之类),不该把选择权清零。
func ProbeUsable(exe string, list []Encoder) []Encoder {
	if len(list) == 0 {
		return list
	}

	usable := make([]bool, len(list))
	var wg sync.WaitGroup

	for i, e := range list {
		// 软件编码器一定跑得起来,省一次进程启动
		if !e.Hardware {
			usable[i] = true
			continue
		}
		wg.Add(1)
		go func(i int, e Encoder) {
			defer wg.Done()
			usable[i] = encoderRuns(exe, e)
		}(i, e)
	}
	wg.Wait()

	out := make([]Encoder, 0, len(list))
	for i, e := range list {
		if usable[i] {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return list
	}
	return out
}

// encoderRuns 让一个编码器真编一帧,只回答"它能不能跑起来"。
//
// 输入是合成的 testsrc,**不是 ddagrab**,两个原因:
//
//  1. DXGI Desktop Duplication 同一时刻只允许一个采集会话(见
//     stream/manager.go 开头),拿它探测会和真正的采集打架;
//  2. 探测要发现的是"初始化就失败"(驱动没装、DLL 缺失),合成源足够。
//
// 代价是它**证明不了 d3d11 硬件帧那条零拷贝路径可用** —— 所以运行时
// 回退(Manager.runStart)必须保留,两者不能互相替代。
func encoderRuns(exe string, e Encoder) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=30",
		"-frames:v", "1",
		"-c:v", e.Name,
		"-f", "null", "-")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}

	if err := cmd.Run(); err == nil {
		return true
	}
	// 超时说明"不知道",按可用处理 —— 真相交给运行时回退去发现。
	return ctx.Err() == context.DeadlineExceeded
}
