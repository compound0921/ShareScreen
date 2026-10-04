package paths

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 从可执行文件旁边的 tools/ 目录查找。
//
// 发布件就是一个文件夹:ShareScreen.exe 和 tools/ 并排放着,解压出来
// 保持原样即可。二进制本身只有 8 MB,两个组件在目录里看得见也换得掉。

var (
	sidecarFFmpeg   = filepath.Join("tools", "ffmpeg", "bin", "ffmpeg.exe")
	sidecarMediaMTX = filepath.Join("tools", "mediamtx", "mediamtx.exe")
)

// resolveSidecar 按顺序在若干根目录下查找 rel,最后回退到 PATH。
func resolveSidecar(rel, name string) (*Tool, error) {
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
			"请确认 tools/ 目录和 ShareScreen.exe 放在一起 —— "+
			"发布包解压出来的文件夹要保持原样,别只把 exe 单独拷出来",
		name, strings.Join(tried, "\n  "))
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
