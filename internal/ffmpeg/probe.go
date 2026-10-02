package ffmpeg

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const createNoWindow = 0x08000000

// ProbeEncoders 询问本机的 ffmpeg 有哪些可用的 H.264 编码器。
//
// 枚举出来不等于能用(比如列着 h264_nvenc 但没装显卡驱动),真正的确认
// 只能靠实际推流一次。这里的结果用于生成界面的可选项。
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

// ResolveEncoder 根据配置里的短名选出实际要用的编码器。
//
// kind 为空或 "auto" 时选优先级最高的一个(knownEncoders 已按"硬件优先"排列)。
func ResolveEncoder(kind string, available []Encoder) (Encoder, error) {
	if len(available) == 0 {
		return Encoder{}, fmt.Errorf("没有可用的编码器")
	}
	if kind == "" || kind == "auto" {
		return available[0], nil
	}
	for _, e := range available {
		if e.Kind == kind {
			return e, nil
		}
	}
	return Encoder{}, fmt.Errorf("编码器 %q 在本机不可用,请改用其他编码器", kind)
}
