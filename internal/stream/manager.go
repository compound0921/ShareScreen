// Package stream 编排 ffmpeg 子进程的启停。
//
// 这是整个程序里最容易出错的部分,原因是一个 Windows API 的限制:
// **DXGI Desktop Duplication 在同一时刻只允许一个采集会话**。
// 第二个 ddagrab 实例会直接失败。所以"改个参数"这件事实际上是
// "彻底停掉旧进程 → 确认它的资源真的释放了 → 再启动新进程"。
package stream

import (
	"fmt"
	"sync"
	"time"

	"sharescreen/internal/config"
	"sharescreen/internal/ffmpeg"
	"sharescreen/internal/paths"
	"sharescreen/internal/proc"
)

const (
	// 停止旧进程时,先发 'q' 等它自己退出的时长;超时才强杀。
	stopGrace = 2 * time.Second
	// 强杀之后额外等待的时间。进程 Wait() 返回说明进程没了,但系统资源
	// (尤其是 DDA 会话)的释放可能还要一点点时间。
	settleDelay = 250 * time.Millisecond
	// 启动后等多久做健康检查 —— 启动期的错误(填错窗口标题、编码器不可用)
	// 会在这段时间内让进程退出。
	healthDelay = 1500 * time.Millisecond
	// 启动失败后的重试次数与退避基数。
	startRetries = 2
	retryBackoff = 600 * time.Millisecond
)

// State 是推流的运行状态。
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateFailed   State = "failed"
)

// Status 是给界面看的状态快照。
type Status struct {
	State       State    `json:"state"`
	Running     bool     `json:"running"`
	UptimeMs    int64    `json:"uptimeMs"`
	LastError   string   `json:"lastError,omitempty"`
	Command     string   `json:"command,omitempty"`
	OutputTail  []string `json:"outputTail,omitempty"`
	TargetKbps  int      `json:"targetKbps"`
	EncoderName string   `json:"encoderName,omitempty"`
}

// Manager 管理 ffmpeg 子进程。
type Manager struct {
	// opMu 串行化启停操作。它会被持有跨越 sleep,所以不能和 stMu 合并。
	opMu sync.Mutex
	// stMu 保护下面的状态字段,只短暂持有,保证 Status() 随时可读。
	stMu sync.RWMutex

	tool *paths.Tool
	job  *proc.Job

	cfg      config.Config
	encoders []ffmpeg.Encoder

	state       State
	child       *proc.Child
	startedAt   time.Time
	lastErr     string
	lastCommand string
	encoderName string
}

func New(tool *paths.Tool) (*Manager, error) {
	job, err := proc.NewJob()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		tool:  tool,
		job:   job,
		state: StateStopped,
	}
	return m, nil
}

// SetConfig 更新配置。不触发重启 —— 由调用方决定何时调用 Restart。
func (m *Manager) SetConfig(cfg config.Config) {
	m.stMu.Lock()
	m.cfg = cfg
	m.stMu.Unlock()
}

// SetEncoders 设置本机可用的编码器列表。
func (m *Manager) SetEncoders(list []ffmpeg.Encoder) {
	m.stMu.Lock()
	m.encoders = list
	m.stMu.Unlock()
}

// Status 返回当前状态快照。随时可调用,不会被正在进行的启停操作阻塞。
func (m *Manager) Status() Status {
	m.stMu.RLock()
	defer m.stMu.RUnlock()

	st := Status{
		State:       m.state,
		LastError:   m.lastErr,
		Command:     m.lastCommand,
		TargetKbps:  m.cfg.Video.BitrateKbps,
		EncoderName: m.encoderName,
	}
	if m.child != nil {
		st.Running = m.child.Running()
		st.OutputTail = m.child.Tail(12)
	}
	if st.State == StateRunning && !m.startedAt.IsZero() {
		st.UptimeMs = time.Since(m.startedAt).Milliseconds()
	}
	return st
}

// Start 异步启动推流。立即返回;进度通过 Status() 观察。
func (m *Manager) Start() {
	go func() {
		m.opMu.Lock()
		defer m.opMu.Unlock()

		if m.currentState() == StateStarting {
			return // 已有一次启动在进行中
		}
		m.setState(StateStarting)
		m.ensureClean() // 先确保没有残留的采集会话

		var lastErr error
		for attempt := 0; attempt <= startRetries; attempt++ {
			if attempt > 0 {
				time.Sleep(retryBackoff * time.Duration(attempt))
				m.ensureClean()
			}
			if err := m.startOnce(); err != nil {
				lastErr = err
				continue
			}
			if err := m.healthCheck(); err != nil {
				lastErr = err
				continue
			}
			m.markRunning()
			return
		}
		m.markFailed(lastErr)
	}()
}

// Stop 异步停止推流。
func (m *Manager) Stop() {
	go func() {
		m.opMu.Lock()
		defer m.opMu.Unlock()
		m.setState(StateStopping)
		m.ensureClean()
		m.setState(StateStopped)
	}()
}

// Restart 异步重启推流(参数变更后使用)。
func (m *Manager) Restart() {
	go func() {
		m.opMu.Lock()
		defer m.opMu.Unlock()

		m.setState(StateStarting)
		m.ensureClean()

		var lastErr error
		for attempt := 0; attempt <= startRetries; attempt++ {
			if attempt > 0 {
				time.Sleep(retryBackoff * time.Duration(attempt))
				m.ensureClean()
			}
			if err := m.startOnce(); err != nil {
				lastErr = err
				continue
			}
			if err := m.healthCheck(); err != nil {
				lastErr = err
				continue
			}
			m.markRunning()
			return
		}
		m.markFailed(lastErr)
	}()
}

// Close 停止推流并关闭 job(连带杀死任何残留子进程)。
func (m *Manager) Close() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.ensureClean()
	m.setState(StateStopped)
	m.job.Close()
}

// ---------- 内部实现 ----------

// startOnce 启动一次 ffmpeg。调用前必须已经确保环境干净。
func (m *Manager) startOnce() error {
	m.stMu.RLock()
	cfg := m.cfg
	encoders := m.encoders
	m.stMu.RUnlock()

	enc, err := ffmpeg.ResolveEncoder(cfg.Video.Encoder, encoders)
	if err != nil {
		return err
	}

	target := fmt.Sprintf("rtmp://127.0.0.1:%d/%s", cfg.RTMPPort, cfg.StreamPath)
	args, err := ffmpeg.BuildArgs(cfg.Video, enc, target)
	if err != nil {
		return err
	}

	child, err := proc.Start(m.job, proc.Spec{
		Name:  "ffmpeg",
		Exe:   m.tool.Exe,
		Dir:   m.tool.Dir,
		Args:  args,
		Stdin: true, // 需要 stdin 才能发 'q' 优雅退出
	})
	if err != nil {
		return err
	}

	m.stMu.Lock()
	m.child = child
	m.encoderName = enc.Name
	m.lastCommand = ffmpeg.Describe(m.tool.Exe, args)
	m.stMu.Unlock()
	return nil
}

// healthCheck 等待一小段时间,确认 ffmpeg 没有立刻退出。
func (m *Manager) healthCheck() error {
	time.Sleep(healthDelay)

	m.stMu.RLock()
	child := m.child
	m.stMu.RUnlock()

	if child == nil {
		return fmt.Errorf("内部错误:健康检查时没有子进程")
	}
	if child.Running() {
		return nil
	}

	tail := child.TailText(15)
	err := child.ExitErr()
	if tail == "" {
		return fmt.Errorf("ffmpeg 启动后立即退出: %v", err)
	}
	return fmt.Errorf("ffmpeg 启动后立即退出: %v\n%s", err, tail)
}

// ensureClean 彻底停止当前子进程,并确认系统资源已释放。
//
// 这是规避 ddagrab 单会话冲突的关键。三个动作缺一不可:
//  1. 优雅停止(发 'q'),超时才强杀,并且一定等到 Wait() 返回
//  2. 轮询 job 内的活动进程数降到 0 —— Wait() 返回不等于资源已释放
//  3. 再等一个短暂的 settle 时间
func (m *Manager) ensureClean() {
	m.stMu.RLock()
	child := m.child
	m.stMu.RUnlock()

	if child != nil {
		_ = child.Stop(stopGrace)
	}

	m.waitJobEmpty(settleDelay + time.Second)
	time.Sleep(settleDelay)

	m.stMu.Lock()
	m.child = nil
	m.stMu.Unlock()
}

// waitJobEmpty 轮询直到 job 内没有活动进程,或超时。
func (m *Manager) waitJobEmpty(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := m.job.ActiveProcesses()
		if err == nil && n == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (m *Manager) currentState() State {
	m.stMu.RLock()
	defer m.stMu.RUnlock()
	return m.state
}

func (m *Manager) setState(s State) {
	m.stMu.Lock()
	m.state = s
	m.stMu.Unlock()
}

func (m *Manager) markRunning() {
	m.stMu.Lock()
	m.state = StateRunning
	m.startedAt = time.Now()
	m.lastErr = ""
	m.stMu.Unlock()
}

func (m *Manager) markFailed(err error) {
	m.stMu.Lock()
	m.state = StateFailed
	if err != nil {
		m.lastErr = err.Error()
	}
	m.stMu.Unlock()
}
