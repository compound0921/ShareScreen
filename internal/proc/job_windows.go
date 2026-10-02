//go:build windows

package proc

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job 是一个 Windows Job Object,把子进程绑定到本进程的生命周期上。
//
// 创建时设置 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE:当本进程以任何方式结束
// (正常退出、panic、被任务管理器强杀),OS 关闭 job 句柄时会连带杀死
// job 内的所有进程。
//
// 这是"父死子必死"唯一可靠的手段。不要改用退出时执行
// `taskkill /im ffmpeg.exe` —— 那会误杀用户自己打开的 ffmpeg。
type Job struct {
	h    windows.Handle
	once sync.Once
}

// NewJob 创建 Job Object。
func NewJob() (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &Job{h: h}, nil
}

// Assign 把进程加入 job。
//
// 注意存在一个理论上的竞态:os/exec 走 CreateProcess,无法先以挂起态创建再
// 加入 job,所以从进程启动到 Assign 之间有极短窗口。ffmpeg 和 MediaMTX
// 在这几毫秒内不会派生孙进程,实际无风险。
func (j *Job) Assign(pid int) error {
	// 只需要这两个权限;PROCESS_SET_QUOTA 是 AssignProcessToJobObject 的要求
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(pid=%d): %w", pid, err)
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(j.h, h); err != nil {
		return fmt.Errorf("AssignProcessToJobObject(pid=%d): %w", pid, err)
	}
	return nil
}

// jobObjectBasicAccountingInformation 对应 Win32 的
// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION。x/sys 提供了它的信息类常量
// (JobObjectBasicAccountingInformation)却没提供结构体,所以在这里补上。
// 字段布局必须与 Win32 完全一致,不能调整顺序或类型。
type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// ActiveProcesses 返回 job 内当前仍在运行的进程数。
//
// 用途:进程 Wait() 返回只说明那个进程消失了,不代表它持有的系统资源
// (例如 DXGI Desktop Duplication 会话)已经释放。启动新的采集进程前
// 轮询这个值降到 0,可以避免 ddagrab 的单会话冲突。
func (j *Job) ActiveProcesses() (int, error) {
	var info jobObjectBasicAccountingInformation
	var retLen uint32
	if err := windows.QueryInformationJobObject(
		j.h,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		&retLen,
	); err != nil {
		return 0, fmt.Errorf("QueryInformationJobObject: %w", err)
	}
	return int(info.ActiveProcesses), nil
}

// Close 关闭 job 句柄。因为设了 KILL_ON_JOB_CLOSE,这会杀死 job 内所有进程。
// 可重复调用。
func (j *Job) Close() {
	j.once.Do(func() {
		if j.h != 0 {
			windows.CloseHandle(j.h)
			j.h = 0
		}
	})
}
