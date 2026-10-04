// genrelease 打出发布件。
//
//	go run ./cmd/genrelease -version 1.2
//
// 产出 dist/ShareScreen-1.2.zip,解压出来是一个文件夹:
//
//	ShareScreen-1.2/
//	  ShareScreen.exe                  程序本体,8 MB
//	  使用说明.txt                      给用户看的那一页
//	  tools/ffmpeg/bin/ffmpeg.exe      采集和编码
//	  tools/mediamtx/mediamtx.exe      WebRTC 分发
//
// 为什么发文件夹而不是单个 exe:早先有一版把两个组件嵌进二进制,编出来是
// 一个 165 MB 的单文件,拷到哪都能跑。但它把 ffmpeg 和 MediaMTX 藏进了 exe
// 里 —— 用户看不见里面装着什么,组件版本对不上时既换不掉也说不清,出问题时
// 连"到底跑的是哪份 ffmpeg"都无从确认。现在三样东西各归各位。
//
// 仍然要打成 zip:两个组件合计 155 MB,而它们本身压不动,能压下去的只有
// 程序本体那一部分,打完大约七折。
package main

import (
	"archive/zip"
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	appName = "ShareScreen"
	outDir  = "dist"
	// 去掉控制台窗口和符号表。和 README 里写的一致。
	ldflags = "-H=windowsgui -s -w"

	// userReadme 是随包发出去的那份说明在仓库里的位置。到了包里它躺在
	// 文件夹根部,文件名不再带这个前缀。
	userReadme = "docs/使用说明.txt"
)

// payload 是发布文件夹里除程序本体之外的内容:从仓库哪儿拿、放到包里哪儿。
//
// 只拿两个组件的可执行文件,不带 ffmpeg 压缩包里那堆 doc/ 和 presets/ ——
// 那些会把发布包撑大三倍,而我们一个都用不到。
var payload = []struct {
	src string // 仓库里的位置
	dst string // 发布文件夹里的相对路径
}{
	{filepath.Join("tools", "ffmpeg", "bin", "ffmpeg.exe"),
		filepath.Join("tools", "ffmpeg", "bin", "ffmpeg.exe")},
	{filepath.Join("tools", "mediamtx", "mediamtx.exe"),
		filepath.Join("tools", "mediamtx", "mediamtx.exe")},
}

func main() {
	version := flag.String("version", "", "版本号,写进文件夹名;留空用当天日期")
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
	for _, p := range payload {
		if _, err := os.Stat(p.src); err != nil {
			return fmt.Errorf("缺少发布内容 %s\n"+
				"发布包要把这些一起带上,所以它们必须先存在 —— "+
				"见 README 的「准备」一节", p.src)
		}
	}
	if _, err := os.Stat(userReadme); err != nil {
		return fmt.Errorf("缺少 %s", userReadme)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	folder := fmt.Sprintf("%s-%s", appName, version)
	stage := filepath.Join(outDir, folder)
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}

	// 打包成功后清掉中间目录:发布件是那个 zip,dist/ 里再留一份解压好的
	// 只会让人分不清哪个是产物。失败时留着,好让人看见打到哪一步了。
	done := false
	defer func() {
		if done {
			_ = os.RemoveAll(stage)
		}
	}()

	fmt.Println("编译…")
	exeName := appName + ".exe"
	if err := goBuild(filepath.Join(stage, exeName)); err != nil {
		return err
	}

	fmt.Println("拷贝组件…")
	for _, p := range payload {
		if err := copyFile(p.src, filepath.Join(stage, p.dst)); err != nil {
			return err
		}
	}
	// 说明是给人双击打开的,行尾和编码要按记事本能认的方式写,见这个函数。
	if err := writeUserReadme(filepath.Join(stage, "使用说明.txt")); err != nil {
		return err
	}

	zipPath := filepath.Join(outDir, folder+".zip")
	fmt.Println("打包…")
	if err := zipTree(zipPath, stage, folder); err != nil {
		return err
	}
	done = true

	fi, err := os.Stat(zipPath)
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("  %-28s %6.1f MB\n", filepath.Base(zipPath), float64(fi.Size())/(1<<20))
	fmt.Printf("\n解压出来是 %s/,里面是程序本体、使用说明和 tools/。\n"+
		"发给别人时整个 zip 发过去就行,别让人只把 exe 拷出来。\n", folder)
	return nil
}

// goBuild 调 go 命令编译程序本体。
func goBuild(out string) error {
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", out, ".")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build 失败: %w", err)
	}
	return nil
}

// copyFile 拷一个文件,必要时建目录。权限带上可执行位 —— 拷的是 exe。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("拷贝 %s 失败: %w", src, err)
	}
	return out.Close()
}

// writeUserReadme 把仓库里那份说明写成发布版:UTF-8 BOM + CRLF。
//
// 不是多此一举 —— 这份文件的读者只会双击它,也就是用记事本打开。而
// Windows 记事本一直到 1903 才默认按 UTF-8 解析无 BOM 的文件,在那之前
// 它按系统代码页读,整篇中文都是乱码;只用 LF 换行的话,老记事本还会把
// 所有内容显示成一行。仓库里那份保持 UTF-8 无 BOM、LF,和其他文档一致,
// 转换只发生在打包这一步。
func writeUserReadme(dst string) error {
	data, err := os.ReadFile(userReadme)
	if err != nil {
		return err
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))

	out := append([]byte{0xEF, 0xBB, 0xBF}, data...)
	return os.WriteFile(dst, out, 0o644)
}

// zipTree 把 dir 整个压进 zip,每个条目都带上 root 这层目录名。
//
// 带这一层是为了解压出来得到 ShareScreen-1.2/ 这样一个文件夹,而不是把
// exe 和 tools/ 直接散进用户当前的目录里。
func zipTree(zipPath, dir, root string) error {
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}

		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     filepath.ToSlash(filepath.Join(root, rel)),
			Method:   zip.Deflate,
			Modified: fi.ModTime(),
		})
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})

	// 两个都要走完:出错时不 Close 会留下一个结构不完整的 zip,
	// 而它的文件名看起来完全正常。
	closeErr := zw.Close()
	if walkErr != nil {
		return walkErr
	}
	return closeErr
}
