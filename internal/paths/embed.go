package paths

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

// 内嵌:把 main 交进来的工具文件系统释放到磁盘。
//
// 只在带 embed_tools 标签构建时才会被调用。

// extract 把内嵌的工具释放到 %LOCALAPPDATA%\ShareScreen\bin\ 并返回它的位置。
//
// 已经释放过且大小一致就直接复用 —— 正常启动不产生任何写盘。
// 用大小而不是哈希判断,是因为它不需要额外维护版本号,而 155 MB 做一次
// 哈希要几百毫秒,为这点准确性不值得。
func extract(embeddedPath, name string) (*Tool, error) {
	dir, err := binDir()
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(dir, name)

	src, err := embeddedTools.Open(embeddedPath)
	if err != nil {
		return nil, fmt.Errorf("打开内嵌资源 %s 失败: %w", embeddedPath, err)
	}
	defer src.Close()

	st, err := src.Stat()
	if err != nil {
		return nil, err
	}

	if fi, err := os.Stat(dest); err == nil && fi.Size() == st.Size() {
		return &Tool{Exe: dest, Dir: dir}, nil
	}

	log.Printf("正在释放 %s(%.1f MB)到 %s", name, float64(st.Size())/(1<<20), dir)
	if err := writeAtomic(dest, src, st.Size()); err != nil {
		// 释放失败但目标文件已存在 —— 多半是另一个实例正在运行,
		// 文件被占用无法替换。沿用现有文件即可,不必让启动失败。
		if _, statErr := os.Stat(dest); statErr == nil {
			log.Printf("警告: %s 释放失败(%v),沿用已存在的文件", name, err)
			return &Tool{Exe: dest, Dir: dir}, nil
		}
		return nil, err
	}
	return &Tool{Exe: dest, Dir: dir}, nil
}

// writeAtomic 先写临时文件再改名。
//
// 直接覆盖写有个真实风险:写到一半进程没了(用户关掉、系统休眠、
// 被杀),磁盘上就留下一个截断的 exe。下次启动时它的大小对不上会被
// 重新释放,但如果赶巧在替换过程中被打断,拿到一个半截的可执行文件
// 会报一个完全看不懂的错。临时文件加改名就没有这个窗口。
func writeAtomic(dest string, src io.Reader, size int64) error {
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}

	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("写入 %s 失败: %w", dest, err)
	}
	if n != size {
		_ = os.Remove(tmp)
		return fmt.Errorf("写入 %s 不完整: 期望 %d 字节,实际 %d", dest, size, n)
	}

	// Windows 上目标文件被占用时 Rename 会失败,这时留着 tmp 也没有意义
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换 %s 失败: %w", dest, err)
	}
	return nil
}
