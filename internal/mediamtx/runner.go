package mediamtx

import (
	"fmt"
	"time"

	"sharescreen/internal/paths"
	"sharescreen/internal/proc"
)

// MediaMTX 没有需要落盘的运行状态(不录制),所以不需要优雅退出,
// 这里只给一个很短的宽限期,超时就强杀。
const stopGrace = 500 * time.Millisecond

// Runner 管理 MediaMTX 子进程。
//
// 它持有自己的 Job Object,而不是和 ffmpeg 共用一个 —— 这样两边都能用
// "job 内活动进程数" 来判断自己的子进程是否真的退干净了。
type Runner struct {
	tool  *paths.Tool
	job   *proc.Job
	child *proc.Child
}

func NewRunner(tool *paths.Tool) (*Runner, error) {
	job, err := proc.NewJob()
	if err != nil {
		return nil, err
	}
	return &Runner{tool: tool, job: job}, nil
}

// Start 启动 MediaMTX,使用 configPath 指向的配置。
// 已在运行时是空操作。
func (r *Runner) Start(configPath string) error {
	if r.Running() {
		return nil
	}

	child, err := proc.Start(r.job, proc.Spec{
		Name: "MediaMTX",
		Exe:  r.tool.Exe,
		Dir:  r.tool.Dir,
		// MediaMTX 把第一个位置参数当作配置文件路径
		Args:  []string{configPath},
		Stdin: false,
	})
	if err != nil {
		return err
	}
	r.child = child
	return nil
}

// Stop 停止 MediaMTX。未在运行时是空操作。
func (r *Runner) Stop() error {
	if r.child == nil {
		return nil
	}
	err := r.child.Stop(stopGrace)
	r.child = nil
	return err
}

// Close 停止子进程并关闭 job。
//
// 关闭 job 会连带杀死其中所有进程(KILL_ON_JOB_CLOSE),这是主程序
// 异常退出后的最后一道保险。
func (r *Runner) Close() {
	_ = r.Stop()
	r.job.Close()
}

// Running 报告 MediaMTX 是否在运行。
func (r *Runner) Running() bool {
	return r.child != nil && r.child.Running()
}

// Tail 返回最近的输出行,用于诊断启动失败。
func (r *Runner) Tail(n int) []string {
	if r.child == nil {
		return nil
	}
	return r.child.Tail(n)
}

// WaitExit 等待进程退出,最多 timeout,返回是否已退出。
// 用于启动后确认它没有立刻挂掉(比如端口被占)。
func (r *Runner) WaitExit(timeout time.Duration) (bool, error) {
	if r.child == nil {
		return true, fmt.Errorf("MediaMTX 未启动")
	}
	done := make(chan error, 1)
	go func() { done <- r.child.Wait() }()

	select {
	case err := <-done:
		return true, err
	case <-time.After(timeout):
		return false, nil
	}
}
