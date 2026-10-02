// genrelease 打出发布件。
//
//	go run ./cmd/genrelease -version 0.1
//
// 产出在 dist/ 下:
//
//	ShareScreen-0.1-standalone.zip   内嵌版。解压出一个 exe,拷到哪台机器都能跑,
//	                                 不需要装任何东西。体积大,但下载的只有压缩包。
//	ShareScreen-0.1-slim.exe         侧载版。8 MB,需要自己按 README 的「准备」
//	                                 把 ffmpeg 和 MediaMTX 放到 tools/ 下。
//	SHA256SUMS                       上面两个文件的校验和。
//
// 内嵌版之所以要打成 zip:Go 的 go:embed 不做压缩,exe 里原样塞着 155 MB 的
// 工具,压成 zip 只剩四成 —— 下载量差一倍多。侧载版本来就只有 8 MB,没必要再包一层。
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	appName   = "ShareScreen"
	outDir    = "dist"
	buildTags = "embed_tools"
	// 去掉控制台窗口和符号表。和 README 里写的一致。
	ldflags = "-H=windowsgui -s -w"
)

// requiredTools 是内嵌版必须存在的构建输入。它们就是要被嵌进去的对象,
// 不存在的话 go:embed 会直接编译失败,但报错信息不如这里说人话。
var requiredTools = []string{
	filepath.Join("tools", "ffmpeg", "bin", "ffmpeg.exe"),
	filepath.Join("tools", "mediamtx", "mediamtx.exe"),
}

func main() {
	version := flag.String("version", "", "版本号,写进文件名;留空用当天日期")
	flag.Parse()
	if *version == "" {
		*version = time.Now().Format("20060102")
	}

	if err := run(*version); err != nil {
		fmt.Fprintln(os.Stderr, "失败:", err)
		os.Exit(1)
	}
}

func run(version string) error {
	for _, p := range requiredTools {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("缺少构建输入 %s\n"+
				"内嵌版要把这两个组件打进 exe,所以它们必须先存在 —— "+
				"见 README 的「准备」一节", p)
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	type artifact struct {
		desc string
		path string
	}

	var made []artifact

	// ── 内嵌版:编译 → 打包 ──
	standaloneExe := filepath.Join(outDir, "standalone.tmp.exe")
	defer os.Remove(standaloneExe)

	fmt.Println("编译内嵌版…")
	if err := goBuild(standaloneExe, buildTags); err != nil {
		return err
	}
	zipPath := filepath.Join(outDir, fmt.Sprintf("%s-%s-standalone.zip", appName, version))
	fmt.Println("打包内嵌版…")
	if err := zipOne(zipPath, fmt.Sprintf("%s.exe", appName), standaloneExe); err != nil {
		return err
	}
	made = append(made, artifact{"内嵌版,解压即用", zipPath})

	// ── 侧载版 ──
	slimPath := filepath.Join(outDir, fmt.Sprintf("%s-%s-slim.exe", appName, version))
	fmt.Println("编译侧载版…")
	if err := goBuild(slimPath, ""); err != nil {
		return err
	}
	made = append(made, artifact{"侧载版,需自备 tools/", slimPath})

	// ── 校验和 ──
	sumsPath := filepath.Join(outDir, "SHA256SUMS")
	var sums strings.Builder
	for _, a := range made {
		sum, err := fileSHA256(a.path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%s  %s\n", sum, filepath.Base(a.path))
	}
	if err := os.WriteFile(sumsPath, []byte(sums.String()), 0o644); err != nil {
		return err
	}

	// ── 汇总 ──
	fmt.Println()
	for _, a := range made {
		fi, err := os.Stat(a.path)
		if err != nil {
			return err
		}
		fmt.Printf("  %-30s %7.1f MB   %s\n",
			filepath.Base(a.path), float64(fi.Size())/(1<<20), a.desc)
	}
	fmt.Printf("  %s\n", filepath.Base(sumsPath))
	fmt.Printf("\n都放在 %s/ 下。\n", outDir)
	return nil
}

// goBuild 调 go 命令编译。tags 为空表示不加构建标签(侧载版)。
func goBuild(out, tags string) error {
	args := []string{"build"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "-ldflags", ldflags, "-o", out, ".")

	cmd := exec.Command("go", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build 失败: %w", err)
	}
	return nil
}

// zipOne 把单个文件压成一个 zip。用 Deflate —— 这正是打包的意义所在。
func zipOne(zipPath, entryName, srcPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	fi, err := src.Stat()
	if err != nil {
		return err
	}

	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	w, err := zw.CreateHeader(&zip.FileHeader{
		Name:     entryName,
		Method:   zip.Deflate,
		Modified: fi.ModTime(),
	})
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, src); err != nil {
		return err
	}
	return zw.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
