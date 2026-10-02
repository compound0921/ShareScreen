// Package paths 定位随程序分发的第三方工具(ffmpeg / MediaMTX)。
//
// 有两种来源,启动时自动选择:
//
//	内嵌   main 通过 SetEmbeddedTools 交进来一个文件系统 —— 工具被打进了
//	       二进制,这里负责释放到 %LOCALAPPDATA%\ShareScreen\bin\ 并返回路径
//	侧载   从可执行文件旁边的 tools/ 目录查找
//
// 内嵌优先。main 只在带 embed_tools 标签构建时才会注入,所以默认构建
// 就是侧载。
//
// 为什么默认侧载:内嵌让二进制从 8 MB 涨到约 165 MB,而且每次编译都要
// 把那 155 MB 重新打包一遍,编译时间从秒级变成十几秒。日常开发用侧载,
// 要分发时才构建内嵌版。
package paths

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Tool 是一个已定位的第三方工具。
type Tool struct {
	Exe string // 可执行文件完整路径
	Dir string // 该工具的工作目录(子进程的 cwd)
}

// 内嵌资源在文件系统里的路径。注意这只是 FS 里的键名,
// 不是磁盘路径 —— 真正的 go:embed 声明在仓库根的 embed_tools.go,
// 因为 go:embed 的路径相对于声明它的包目录,不能往外跳。
const (
	embeddedFFmpeg   = "tools/ffmpeg/bin/ffmpeg.exe"
	embeddedMediaMTX = "tools/mediamtx/mediamtx.exe"
)

// embeddedTools 由 main 在带 embed_tools 标签构建时注入,否则为 nil。
var embeddedTools fs.FS

// SetEmbeddedTools 把内嵌的工具文件系统交给本包。
//
// 由仓库根的 embed_tools.go 调用。不在那里直接实现 Resolve,
// 是因为 go:embed 只能嵌入所在包目录及其子目录 —— 那个包必须在
// 仓库根,而根的包是 main,不适合承载业务逻辑。
func SetEmbeddedTools(fsys fs.FS) { embeddedTools = fsys }

// ResolveFFmpeg 定位 ffmpeg。
func ResolveFFmpeg() (*Tool, error) {
	if embeddedTools != nil {
		return extract(embeddedFFmpeg, "ffmpeg.exe")
	}
	return resolveSidecar(sidecarFFmpeg, "ffmpeg")
}

// ResolveMediaMTX 定位 MediaMTX。
func ResolveMediaMTX() (*Tool, error) {
	if embeddedTools != nil {
		return extract(embeddedMediaMTX, "mediamtx.exe")
	}
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

// binDir 是内嵌模式下释放出来的工具的存放目录。
func binDir() (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(d, "bin")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fmt.Errorf("创建目录失败 %s: %w", p, err)
	}
	return p, nil
}
