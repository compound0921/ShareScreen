//go:build embed_tools

package main

import (
	"embed"

	"sharescreen/internal/paths"
)

// 内嵌构建 —— 把两个第三方 exe 打进二进制:
//
//	go build -tags embed_tools -ldflags "-H=windowsgui -s -w" -o sharescreen.exe .
//
// 产物约 165 MB,不再依赖外部的 tools/ 目录,可以直接拷给别人用。
//
// 这个声明必须待在仓库根。go:embed 的路径是相对于声明它的包目录的,
// 而且不允许用 .. 往外跳 —— 所以只有根目录(或 tools/ 内部的某个包)
// 能嵌入 tools/ 下的文件。根的包是 main,不适合承载业务逻辑,于是这里
// 只做一件事:把文件系统交给 paths,其余逻辑都在那边。
//
// 只嵌这两个文件,不含 ffmpeg 压缩包里的 doc/ 和 presets/ ——
// 那些会把嵌入量从 155 MB 撑到 315 MB,而我们一个都用不到。
//
//go:embed tools/ffmpeg/bin/ffmpeg.exe tools/mediamtx/mediamtx.exe
var toolBinaries embed.FS

func init() {
	paths.SetEmbeddedTools(toolBinaries)
}
