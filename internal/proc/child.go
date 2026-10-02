package proc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// 不弹控制台窗口。否则每次启动 ffmpeg 都会闪一个黑框。
	createNoWindow = 0x08000000

	tailCapacity = 200 // 保留的最大输出行数
)

// Spec 描述一个待启动的子进程。
type Spec struct {
	Name  string // 用于日志与错误信息
	Exe   string
	Dir   string // 工作目录
	Args  []string
	Stdin bool // 是否开 stdin 管道(ffmpeg 需要,用于发 'q' 优雅退出)
}

// Child 是一个受管子进程。
type Child struct {
	spec Spec

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	tail   *RingBuffer
	assignErr error // job 绑定失败时记录,不致命但会失去孤儿清理兜底

	waitOnce sync.Once
	waitCh   chan struct{}
	waitErr  error

	stopOnce sync.Once
}

// Start 启动子进程并绑定到 job。
//
// 立即返回,不等待进程结束。输出由后台 goroutine 持续读走。
func Start(job *Job, spec Spec) (*Child, error) {
	cmd := exec.Command(spec.Exe, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}

	c := &Child{
		spec:   spec,
		cmd:    cmd,
		tail:   NewRingBuffer(tailCapacity),
		waitCh: make(chan struct{}),
	}

	if spec.Stdin {
		w, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("启动 %s: 创建 stdin 管道失败: %w", spec.Name, err)
		}
		c.stdin = w
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("启动 %s: 创建 stdout 管道失败: %w", spec.Name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("启动 %s: 创建 stderr 管道失败: %w", spec.Name, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s 失败: %w", spec.Name, err)
	}

	// 绑定到 job —— 必须在 Start 之后尽快做
	if job != nil {
		if err := job.Assign(cmd.Process.Pid); err != nil {
			c.assignErr = err
		}
	}

	// 必须有人消费输出,否则管道填满会把子进程卡死
	go c.drain(stdout)
	go c.drain(stderr)

	go func() {
		c.waitErr = cmd.Wait()
		close(c.waitCh)
	}()

	return c, nil
}

func (c *Child) drain(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	// ffmpeg 的进度统计用 \r 刷新同一行,所以 \r 和 \n 都要当分隔符
	sc.Split(splitOnNewline)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t")
		if line != "" {
			c.tail.Add(line)
		}
	}
}

func splitOnNewline(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' || data[i] == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// Name 返回 Spec 里的名字。
func (c *Child) Name() string { return c.spec.Name }

// PID 返回进程号。进程已结束时可能为 0。
func (c *Child) PID() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// AssignErr 返回绑定 job 时的错误(如果有)。非 nil 意味着失去孤儿清理兜底。
func (c *Child) AssignErr() error { return c.assignErr }

// Running 报告进程是否仍在运行。
func (c *Child) Running() bool {
	select {
	case <-c.waitCh:
		return false
	default:
		return true
	}
}

// Wait 阻塞直到进程退出,返回其退出错误(nil 表示退出码为 0)。
func (c *Child) Wait() error {
	<-c.waitCh
	return c.waitErr
}

// ExitErr 在进程已退出时返回退出错误;仍在运行时返回 nil。
func (c *Child) ExitErr() error {
	select {
	case <-c.waitCh:
		return c.waitErr
	default:
		return nil
	}
}

// Tail 返回最近 n 行输出。
func (c *Child) Tail(n int) []string { return c.tail.Last(n) }

// TailText 返回最近 n 行输出,拼成一段文本。
func (c *Child) TailText(n int) string {
	return strings.Join(c.tail.Last(n), "\n")
}

// Stop 停止进程。可重复调用,只有第一次生效。
//
// 顺序很重要:
//  1. 向 stdin 发 'q' —— ffmpeg 会据此优雅关闭输出,对端看到的是正常断开
//  2. 等 Wait 返回,最多 grace
//  3. 超时才强杀
//  4. 强杀之后**仍然要等 Wait 返回** —— 这是确认进程真正消失的唯一可靠信号,
//     而 ddagrab 能否重新启动恰恰取决于上一个进程是否真的没了
func (c *Child) Stop(grace time.Duration) error {
	c.stopOnce.Do(func() {
		if c.stdin != nil {
			// ffmpeg 从 stdin 读取单字符命令,非终端下同样生效
			_, _ = io.WriteString(c.stdin, "q")
			_ = c.stdin.Close()
		}

		select {
		case <-c.waitCh:
			return
		case <-time.After(grace):
		}

		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		<-c.waitCh
	})
	return c.waitErr
}

// ErrStillRunning 用于表示超时后进程仍未退出(理论上不该发生)。
var ErrStillRunning = errors.New("子进程仍在运行")
