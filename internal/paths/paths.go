// Package paths 定位随程序分发的第三方工具(ffmpeg / MediaMTX)。
//
// 工具和 exe 一起分发:解压出来的文件夹里,`ShareScreen.exe` 旁边就是一个
// `tools/` 目录,两个组件按固定路径放在里面。查找顺序见 sidecar.go。
//
// 为什么不做成单个 exe:早先有过一版把两个工具嵌进二进制,拷到哪台机器
// 都能跑,代价是 8 MB 变成 165 MB、每次编译都要重新打包 155 MB,更要命的是
// 用户看不见里面装着什么 —— 组件版本对不上时,既没法换也没法说清楚。
// 现在统一按文件夹分发。
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Tool 是一个已定位的第三方工具。
type Tool struct {
	Exe string // 可执行文件完整路径
	Dir string // 该工具的工作目录(子进程的 cwd)
}

// ResolveFFmpeg 定位 ffmpeg。
func ResolveFFmpeg() (*Tool, error) {
	return resolveSidecar(sidecarFFmpeg, "ffmpeg")
}

// ResolveMediaMTX 定位 MediaMTX。
func ResolveMediaMTX() (*Tool, error) {
	return resolveSidecar(sidecarMediaMTX, "mediamtx")
}

// DataDir 返回配置等可写数据的存放目录(%LOCALAPPDATA%\ShareScreen),
// 必要时创建。
func DataDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		// 非 Windows 或环境变量缺失时退回到用户主目录
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法确定数据目录: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(base, "ShareScreen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建数据目录失败 %s: %w", dir, err)
	}
	return dir, nil
}
