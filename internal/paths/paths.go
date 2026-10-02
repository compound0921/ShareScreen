// Package paths 定位随程序分发的第三方工具(ffmpeg / MediaMTX)。
//
// 当前是"侧载"实现 —— 从可执行文件旁边的 tools/ 目录查找。
// 后续若要改成 go:embed 内嵌分发,只需替换本文件:把 Resolve* 的实现改成
// "释放到 %LOCALAPPDATA%\ShareScreen\bin\ 并返回路径",调用方一行都不用动。
// 对外只暴露 ResolveFFmpeg / ResolveMediaMTX / DataDir 三个函数。
package paths

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Tool 是一个已定位的第三方工具。
type Tool struct {
	Exe string // 可执行文件完整路径
	Dir string // 该工具的工作目录(子进程的 cwd)
}

// 侧载时的相对布局
var (
	ffmpegRel   = filepath.Join("tools", "ffmpeg", "bin", "ffmpeg.exe")
	mediamtxRel = filepath.Join("tools", "mediamtx", "mediamtx.exe")
)

// ResolveFFmpeg 定位 ffmpeg。
func ResolveFFmpeg() (*Tool, error) { return resolve(ffmpegRel, "ffmpeg") }

// ResolveMediaMTX 定位 MediaMTX。
func ResolveMediaMTX() (*Tool, error) { return resolve(mediamtxRel, "mediamtx") }

// resolve 按顺序在若干根目录下查找 rel,最后回退到 PATH。
func resolve(rel, name string) (*Tool, error) {
	var tried []string
	for _, root := range searchRoots() {
		p := filepath.Join(root, rel)
		if looksExecutable(p) {
			return &Tool{Exe: p, Dir: filepath.Dir(p)}, nil
		}
		tried = append(tried, p)
	}

	// 兜底:系统 PATH
	if p, err := exec.LookPath(name); err == nil {
		if abs, err2 := filepath.Abs(p); err2 == nil {
			p = abs
		}
		return &Tool{Exe: p, Dir: filepath.Dir(p)}, nil
	}

	return nil, fmt.Errorf(
		"找不到 %s。已尝试以下位置:\n  %s\n"+
			"请确认 tools/ 目录与程序放在一起,或把 %s 加入 PATH",
		name, strings.Join(tried, "\n  "), name)
}

// searchRoots 返回按优先级排列的查找根目录。
//
// 先看可执行文件所在目录 —— 这是分发后的正常情况;
// 再看当前工作目录 —— 这是 go run . / 在源码目录直接运行时的需要
// (此时可执行文件在临时目录里,旁边没有 tools/)。
func searchRoots() []string {
	var roots []string
	seen := map[string]bool{}

	add := func(p string) {
		if p == "" {
			return
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if !seen[p] {
			seen[p] = true
			roots = append(roots, p)
		}
	}

	if exe, err := os.Executable(); err == nil {
		// 解析符号链接,否则某些场景下拿到的是链接本身的位置
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		add(filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		add(wd)
	}
	return roots
}

func looksExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode().IsRegular()
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
