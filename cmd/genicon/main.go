// genicon 生成 exe 图标资源。
//
//	go run ./cmd/genicon              # 写出 rsrc.syso
//	go run ./cmd/genicon -preview dir # 顺便把各尺寸导成 PNG,用于检查外观
//
// 产物 rsrc.syso 放在仓库根目录,`go build` 会自动把它链进 exe,
// 不需要任何额外的构建参数。改了图标就重新跑一次。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"sharescreen/internal/icon"
)

func main() {
	out := flag.String("o", "rsrc.syso", "输出的资源对象文件")
	preview := flag.String("preview", "", "把各尺寸图标导出为 PNG 到该目录")
	flag.Parse()

	if *preview != "" {
		if err := os.MkdirAll(*preview, 0o755); err != nil {
			fail(err)
		}
		for _, s := range icon.Sizes {
			data, err := icon.PNG(s)
			if err != nil {
				fail(err)
			}
			path := filepath.Join(*preview, fmt.Sprintf("icon-%d.png", s))
			if err := os.WriteFile(path, data, 0o644); err != nil {
				fail(err)
			}
			fmt.Printf("写出 %s(%d 字节)\n", path, len(data))
		}
	}

	data, err := icon.BuildSyso(icon.Sizes)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("已生成 %s(%d 字节,%d 个尺寸)\n", *out, len(data), len(icon.Sizes))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "失败:", err)
	os.Exit(1)
}
