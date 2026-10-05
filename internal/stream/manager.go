// Package stream 编排 ffmpeg 子进程的启停。
//
// 这是整个程序里最容易出错的部分,原因是一个 Windows API 的限制:
// **DXGI Desktop Duplication 在同一时刻只允许一个采集会话**。
// 第二个 ddagrab 实例会直接失败。所以"改个参数"这件事实际上是
// "彻底停掉旧进程 → 确认它的资源真的释放了 → 再启动新进程"。
package stream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"sharescreen/internal/audio"
	"sharescreen/internal/config"
	"sharescreen/internal/ffmpeg"
	"sharescreen/internal/gpu"
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
	// healthCheck 的轮询粒度。
	//
	// 健康检查不再无条件睡满 healthDelay,而是按这个间隔看进程还在不在。
	// 编码器打不开时 ffmpeg 几十毫秒就退出了(实测 h264_amf 缺 DLL 时
	// 32 毫秒),干等那 1.5 秒是纯浪费 —— 而回退会连着试好几个候选,
	// 这个等待会一次次叠加。
	healthPoll = 50 * time.Millisecond
	// 启动失败后的重试次数与退避基数。这是**同一个编码器**的重试次数;
	// 换下一个编码器是另一层(见 runStart)。
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
	Warning     string   `json:"warning,omitempty"`
	Command     string   `json:"command,omitempty"`
	OutputTail  []string `json:"outputTail,omitempty"`
	TargetKbps  int      `json:"targetKbps"`
	EncoderName string   `json:"encoderName,omitempty"`

	// EncoderFallback 非空表示这次用上了回退:首选的那个编码器起不来,
	// 已经自动换成了 EncoderName 里的这个。值是**首选**的短名,用来告诉
	// 用户"你选的那个没跑起来"。
	EncoderFallback string `json:"encoderFallback,omitempty"`

	// External 为真表示流是外部程序推过来的(OBS 直推),本程序没有
	// 运行 ffmpeg。界面据此换一套文案,并且不显示 ffmpeg 的输出尾部。
	External bool `json:"external"`
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

	// 桌面原始尺寸。用来识别"目标尺寸等于源尺寸"的伪缩放 —— 那种情况
	// 不该走 hwdownload,否则白白丢掉零拷贝。0 表示未知。
	screenW, screenH int

	// prefVendor 是驱动显示器那块显卡的厂商,用来给编码器排序。识不出来
	// 就是 Unknown,排序退回原来的优先级。
	prefVendor gpu.Vendor

	state           State
	child           *proc.Child
	startedAt       time.Time
	lastErr         string
	lastCommand     string
	encoderName     string
	encoderFallback string

	// 音频采集的一整套资源。它们的生命周期必须一起管:Capture 是 COM
	// 设备对象,listener/conn 是把 PCM 送进 ffmpeg 的数据通路。
	//
	// audioDone 很关键 —— Release 掉 COM 对象之后 Run 还在跑的话就是
	// use-after-free。Close 之前必须等它关掉。
	audio       *audio.Capture
	audioLn     net.Listener
	audioConn   net.Conn
	audioCancel context.CancelFunc
	audioDone   chan struct{}

	// lastWarning 是"不影响推流但用户该知道"的问题,目前只有音频采集失败。
	// 和 lastErr 分开:后者意味着流没起来,前者意味着流起来了但少了点东西。
	lastWarning string
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

// Config 返回当前配置的快照。
//
// 远程控制要拿它判断采集源覆盖的是哪块屏幕 —— 而采集源随时可能在控制页
// 被改掉(改成窗口采集之后远控就不该再注入),所以必须每次现读,不能在
// 启动时抓一份留着。
func (m *Manager) Config() config.Config {
	m.stMu.RLock()
	defer m.stMu.RUnlock()
	return m.cfg
}

// SetEncoders 设置本机可用的编码器列表。
func (m *Manager) SetEncoders(list []ffmpeg.Encoder) {
	m.stMu.Lock()
	m.encoders = list
	m.stMu.Unlock()
}

// SetScreenSize 告诉管理器采集源的原始尺寸(桌面分辨率)。
//
// 参数构造器靠它判断"目标分辨率等于桌面分辨率"这种伪缩放 ——
// 不设置的话那种情况会走 hwdownload,无声地丢掉零拷贝路径。
func (m *Manager) SetScreenSize(w, h int) {
	m.stMu.Lock()
	m.screenW, m.screenH = w, h
	m.stMu.Unlock()
}

// Status 返回当前状态快照。随时可调用,不会被正在进行的启停操作阻塞。
func (m *Manager) Status() Status {
	m.stMu.RLock()
	defer m.stMu.RUnlock()

	st := Status{
		State:           m.state,
		LastError:       m.lastErr,
		Warning:         m.lastWarning,
		Command:         m.lastCommand,
		TargetKbps:      m.cfg.Video.BitrateKbps,
		EncoderName:     m.encoderName,
		EncoderFallback: m.encoderFallback,
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
		m.runStart()
	}()
}

// SetPreferredVendor 告诉管理器显示器挂在哪个厂商的显卡上。
//
// 只影响候选排序(同厂商优先),拿不到就传 gpu.VendorUnknown。
func (m *Manager) SetPreferredVendor(v gpu.Vendor) {
	m.stMu.Lock()
	m.prefVendor = v
	m.stMu.Unlock()
}

// errEncoderUnavailable 表示"这个编码器在本机打不开",应当换下一个候选。
//
// 用它把两类失败分开:
//
//	编码器打不开      → 换下一个候选(重试同一个没意义,它不会自己好)
//	其余(窗口标题写错、采集源不可用、推流地址连不上)→ 换成别的编码器
//	                   也一样,原地重试才是对的
//
// 这个区分不是锦上添花:少了它,一个"窗口标题写错"的失败会被三个候选
// 各重试三次,失败时间凭空乘三(实测从 6 秒变成 15 秒)。
var errEncoderUnavailable = errors.New("编码器在本机不可用")

// runStart 是 Start / Restart 共用的启动主体。
//
// 调用者必须已经持有 opMu 并置好 StateStarting。
//
// 这里是回退发生的地方:按候选列表逐个试,某个编码器起不来就换下一个。
// 以前是"同一个编码器重试三次"—— 那对确定性的失败(卡不对、驱动没装)
// 毫无用处,只是把同样的错误重放三遍。
func (m *Manager) runStart() {
	cands := m.candidates()
	if len(cands) == 0 {
		// 外部推流模式(OBS 直推):本程序不跑 ffmpeg,没有什么要启动的。
		//
		// 这个早退必须放在这里,而不是留在 startOnce 里 —— 那样
		// startOnce 会返回 nil 却没设 m.child,紧接着的 healthCheck
		// 会报"没有子进程"并白跑满三次重试。
		m.markRunning()
		return
	}

	m.ensureClean()   // 先确保没有残留的采集会话
	m.setFallback("") // 清掉上一次的回退记录,这次从头算

	m.stMu.RLock()
	want := m.cfg.Video.Encoder
	m.stMu.RUnlock()

	var lastErr error
	for i, enc := range cands {
		if i > 0 {
			m.ensureClean()
		}
		err := m.tryEncoder(enc)
		if err == nil {
			// 最终用的不是用户指定的那个,就算发生过回退。
			//
			// 两条路都会走到这里:用户选的那个在启动探测里就被剔掉了
			// (比如在 N 卡机器上选了 amf),或者试了但起不来。
			// 两种都要说 —— 否则用户的选择被静默忽略,界面上的
			// 「编码器」显示着另一个名字,他不知道为什么。
			if want != "" && want != "auto" && enc.Kind != want {
				m.setFallback(want)
			}
			m.markRunning()
			return
		}
		lastErr = err
		// 和编码器无关的失败,换成别的编码器也一样 —— 立刻收手,
		// 别把同一个错误按候选数重放几遍。
		if !errors.Is(err, errEncoderUnavailable) {
			break
		}
	}
	m.markFailed(lastErr)
}

// tryEncoder 用一个编码器反复尝试,直到成功或者判定它没救。
//
// 返回值里有 errEncoderUnavailable 就表示"换下一个候选";否则调用方
// 应当直接收手 —— 那种失败和编码器无关。
func (m *Manager) tryEncoder(enc ffmpeg.Encoder) error {
	var lastErr error
	for attempt := 0; attempt <= startRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(retryBackoff * time.Duration(attempt))
			m.ensureClean()
		}

		// startOnce 失败是进程都没起来(可执行文件找不到之类),
		// 换编码器也一样,所以直接交给外层。
		if err := m.startOnce(enc); err != nil {
			return err
		}

		err := m.healthCheck()
		if err == nil {
			return nil
		}
		lastErr = err

		// 编码器打不开:不重试,立刻让外层换下一个。
		// 实测失败只要几十毫秒(缺 DLL 时 32ms),重试纯属浪费。
		if ffmpeg.EncoderUnavailable(err.Error()) {
			return fmt.Errorf("%w(%s): %w", errEncoderUnavailable, enc.Name, err)
		}
	}
	return lastErr
}

// candidates 返回按"先试哪个"排好的编码器列表。
func (m *Manager) candidates() []ffmpeg.Encoder {
	m.stMu.RLock()
	cfg, encoders, vendor := m.cfg, m.encoders, m.prefVendor
	m.stMu.RUnlock()

	if cfg.Video.External() {
		return nil
	}
	return ffmpeg.Plan(cfg.Video.Encoder, encoders, vendor)
}

func (m *Manager) setFallback(fromKind string) {
	m.stMu.Lock()
	m.encoderFallback = fromKind
	m.stMu.Unlock()
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
		m.runStart()
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

// startOnce 用指定的编码器启动一次 ffmpeg。调用前必须已经确保环境干净。
//
// 编码器由调用方给(而不是在这里从配置解析),回退才有地方换 ——
// 以前这里自己解析,拿到什么就是什么,失败也没得改。
func (m *Manager) startOnce(enc ffmpeg.Encoder) error {
	m.stMu.RLock()
	cfg := m.cfg
	screenW, screenH := m.screenW, m.screenH
	m.stMu.RUnlock()

	// 音频必须在 ffmpeg 之前准备好:它决定了 ffmpeg 的音频输入参数
	// (采样格式、采样率、声道数都来自设备的混音格式)。
	//
	// 失败不算错 —— 退化成纯画面,并在状态里留一条警告。宁可没有声音,
	// 也不能因为音频出问题就让整个共享起不来。
	var audioIn *ffmpeg.AudioInput
	if cfg.Audio.Enabled {
		audioIn = m.startAudio(cfg)
	} else {
		// 用户把音频关了,上一次那条"音频不可用"的警告就过期了。
		m.clearWarning()
	}

	target := fmt.Sprintf("rtsp://127.0.0.1:%d/%s", cfg.RTSPPort, cfg.StreamPath)
	args, err := ffmpeg.BuildArgs(cfg.Video, audioIn, enc, target, screenW, screenH)
	if err != nil {
		m.stopAudio()
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
		// 启动失败会走重试,别把音频设备句柄漏在那儿。
		m.stopAudio()
		return err
	}

	m.stMu.Lock()
	m.child = child
	m.encoderName = enc.Name
	m.lastCommand = ffmpeg.Describe(m.tool.Exe, args)
	m.stMu.Unlock()
	return nil
}

// healthCheck 等一小段时间,确认 ffmpeg 没有立刻退出。
//
// 判定标准还是"活满 healthDelay",但过程改成轮询:**进程一退出就返回,
// 不等满**。编码器打不开时 ffmpeg 几十毫秒就走了(实测 h264_amf 缺 DLL
// 时 32 毫秒),干等 1.5 秒纯属浪费 —— 而回退会连着试好几个候选,
// 这个等待会一次次叠加。
//
// 成功路径仍然等满 healthDelay:那是用来兜"起来之后又慢死"的,
// 缩短它会把偶发失败误判成成功。
func (m *Manager) healthCheck() error {
	deadline := time.Now().Add(healthDelay)

	for {
		m.stMu.RLock()
		child := m.child
		m.stMu.RUnlock()

		if child == nil {
			return fmt.Errorf("内部错误:健康检查时没有子进程")
		}
		if !child.Running() {
			tail := child.TailText(15)
			err := child.ExitErr()
			if tail == "" {
				return fmt.Errorf("ffmpeg 启动后立即退出: %v", err)
			}
			return fmt.Errorf("ffmpeg 启动后立即退出: %v\n%s", err, tail)
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		time.Sleep(healthPoll)
	}
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

	// 先停 ffmpeg 再停音频:反过来的话 ffmpeg 的输入会先断,它可能
	// 以非零码退出,污染 stderr 里那点有用的信息。
	m.stopAudio()

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

// ---------- 桌面音频 ----------

// startAudio 打开桌面音频采集,并起一个本地端口等 ffmpeg 来连。
//
// 返回 nil 表示这次没有音频 —— 调用方应当继续走纯画面,而不是失败。
// 原因记在状态的 warning 字段里。
//
// 为什么走 TCP:音频是我们自己采的,要喂给一个并非由它采集的 ffmpeg 输入。
// ffmpeg 的 stdin 已经被 'q' 优雅退出占着,而 Windows 的 exec 又传不了额外的
// 文件描述符,所以回环 TCP 是最省事的一条路。
func (m *Manager) startAudio(cfg config.Config) *ffmpeg.AudioInput {
	cap, err := audio.Open()
	if err != nil {
		m.setWarning("桌面音频不可用,已按无声共享:" + err.Error())
		return nil
	}

	// 端口交给系统挑,避免和别的程序撞车。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cap.Close()
		m.setWarning("音频端口创建失败,已按无声共享:" + err.Error())
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	m.stMu.Lock()
	m.audio = cap
	m.audioLn = ln
	m.audioCancel = cancel
	m.audioDone = done
	m.stMu.Unlock()

	go m.runAudio(ctx, cap, ln, done)

	m.clearWarning()

	f := cap.Format()
	return &ffmpeg.AudioInput{
		URL:         fmt.Sprintf("tcp://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port),
		SampleFmt:   f.SampleFmt,
		SampleRate:  f.SampleRate,
		Channels:    f.Channels,
		BitrateKbps: cfg.Audio.BitrateKbps,
	}
}

// runAudio 等 ffmpeg 连上来,然后把 PCM 灌进去。
//
// 采集是阻塞的,必须另起 goroutine —— 启动流程后面还要做健康检查,
// 不能被它挡住。
func (m *Manager) runAudio(ctx context.Context, cap *audio.Capture, ln net.Listener, done chan struct{}) {
	defer close(done)

	conn, err := ln.Accept()
	if err != nil {
		return // 停止时 listener 被关掉,这是正常路径
	}
	defer conn.Close()

	m.stMu.Lock()
	m.audioConn = conn
	m.stMu.Unlock()

	// 停止时 Run 必然返回 context.Canceled,那不是故障,不值得报警。
	// 其余的错误(设备被切走、引擎重启)要让用户知道 —— 画面还在,
	// 但声音没了,这种事不说出来用户只会以为播放器坏了。
	if err := cap.Run(ctx, conn); err != nil && ctx.Err() == nil {
		m.setWarning("桌面音频中断,已按无声继续:" + err.Error())
	}
}

// stopAudio 停掉音频采集并释放设备。
//
// 顺序有讲究:先断数据通路让 Run 退出,等它真退出了才能 Release COM 对象。
// 反过来就是 use-after-free。
func (m *Manager) stopAudio() {
	m.stMu.Lock()
	cap, ln, conn := m.audio, m.audioLn, m.audioConn
	cancel, done := m.audioCancel, m.audioDone
	m.audio, m.audioLn, m.audioConn = nil, nil, nil
	m.audioCancel, m.audioDone = nil, nil
	m.stMu.Unlock()

	if cap == nil {
		return
	}

	if conn != nil {
		conn.Close() // 让卡在 Write 上的采集循环立刻返回
	}
	if ln != nil {
		ln.Close() // 让卡在 Accept 上的采集循环立刻返回
	}
	if cancel != nil {
		cancel()
	}

	if done != nil {
		select {
		case <-done:
		case <-time.After(stopGrace):
			// 采集线程没在宽限期内退出。正常不会发生;真发生了也只能
			// 继续往下走,否则整个停止流程会卡死在这里。
		}
	}

	cap.Close()
}

// setWarning 记录一条"不影响推流但用户该知道"的问题。
func (m *Manager) setWarning(msg string) {
	m.stMu.Lock()
	m.lastWarning = msg
	m.stMu.Unlock()
}

func (m *Manager) clearWarning() {
	m.stMu.Lock()
	m.lastWarning = ""
	m.stMu.Unlock()
}
