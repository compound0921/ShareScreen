//go:build windows

// Package netport 查询端口的占用情况,以及找空闲端口。
package netport

import (
	"fmt"
	"net"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Owner 描述占用某个端口的进程。
type Owner struct {
	PID  uint32
	Name string // 可执行文件名,如 mediamtx.exe
	Path string // 完整路径;权限不足时可能为空
}

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedUdpTable = iphlpapi.NewProc("GetExtendedUdpTable")
)

const (
	afINET = 2

	// TCP_TABLE_OWNER_PID_LISTENER —— 只要处于监听状态的连接。
	// 用 *_ALL 的话,某个恰好以该端口为源端口的对外连接也会被算进来。
	tcpTableOwnerPIDListener = 3
	// UDP_TABLE_OWNER_PID
	udpTableOwnerPID = 1

	// MIB_TCPROW_OWNER_PID 每条记录 24 字节:
	// 状态、本地地址、本地端口、远端地址、远端端口、PID,六个 DWORD。
	tcpRowSize = 24
	// MIB_UDPROW_OWNER_PID 只有本地地址、本地端口、PID,12 字节。
	udpRowSize = 12
)

// MIB_TCPTABLE_OWNER_PID / MIB_UDPTABLE_OWNER_PID 都以一个 DWORD 计数开头,
// 后面紧跟记录数组。所以第一条记录从偏移 4 开始。
const tableHeaderSize = 4

// Find 返回占用指定端口的进程。
//
// udp 为真时查 UDP 表。查不到(端口空闲,或没权限看)时 ok 返回 false。
func Find(port int, udp bool) (Owner, bool) {
	table, err := readTable(udp)
	if err != nil || len(table) < tableHeaderSize {
		return Owner{}, false
	}

	count := int(*(*uint32)(unsafe.Pointer(&table[0])))
	rowSize := tcpRowSize
	if udp {
		rowSize = udpRowSize
	}
	// 端口字段在记录里的偏移:TCP 是 8(状态 + 本地地址之后),UDP 是 4。
	portOffset := 8
	pidOffset := 20
	if udp {
		portOffset = 4
		pidOffset = 8
	}

	for i := 0; i < count; i++ {
		off := tableHeaderSize + i*rowSize
		if off+rowSize > len(table) {
			break
		}
		if localPort(table, off+portOffset) != port {
			continue
		}
		pid := *(*uint32)(unsafe.Pointer(&table[off+pidOffset]))
		o := Owner{PID: pid, Name: processName(pid)}
		o.Path = processPath(pid)
		return o, true
	}
	return Owner{}, false
}

// localPort 读出一个记录里的本地端口。
//
// 端口字段是**网络字节序**的:内存里第一个字节是高字节。所以 8080(0x1F90)
// 存成 1F 90 两个字节,拼回来是 高<<8|低。
//
// 不能把整个 DWORD 当整数读 —— 那样得到的是字节翻转的值(8080 变成 36928),
// 而且不会报错,只会静静地匹配不上任何端口。
func localPort(table []byte, off int) int {
	high := uint32(table[off])
	low := uint32(table[off+1])
	return int(high<<8 | low)
}

// readTable 取回整张连接表。表的大小事先不知道,得先问一次拿所需尺寸,
// 再分配缓冲区重问 —— 这也是这类 Win32 枚举接口的固定套路。
func readTable(udp bool) ([]byte, error) {
	proc := procGetExtendedTcpTable
	class := uintptr(tcpTableOwnerPIDListener)
	if udp {
		proc = procGetExtendedUdpTable
		class = udpTableOwnerPID
	}

	var size uint32
	// 第一次调用只为了拿 size,必定返回 ERROR_INSUFFICIENT_BUFFER
	ret, _, _ := proc.Call(0, uintptr(unsafe.Pointer(&size)), 1, afINET, class, 0)
	if ret != 0 && windows.Errno(ret) != windows.ERROR_INSUFFICIENT_BUFFER {
		return nil, fmt.Errorf("查询端口占用表失败: %w", windows.Errno(ret))
	}
	if size == 0 {
		return nil, nil
	}

	buf := make([]byte, size)
	ret, _, _ = proc.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		1, afINET, class, 0)
	if ret != 0 {
		return nil, fmt.Errorf("查询端口占用表失败: %w", windows.Errno(ret))
	}
	return buf, nil
}

func processName(pid uint32) string {
	path := processPath(pid)
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}

// processPath 取进程的可执行文件全路径。
//
// 用 QUERY_LIMITED_INFORMATION 而不是 QUERY_INFORMATION —— 前者对系统进程
// 也能拿到,而且不需要提权。
func processPath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

// Kill 结束指定进程。
//
// 直接 TerminateProcess,不发关闭消息:调用方已经确认过这是自家进程,
// 目的就是让它立刻把端口让出来,优雅退出的那套流程在这里只会拖时间。
func Kill(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

// Free 从 from 开始往上找一个空闲的 TCP 端口。
//
// 直接问系统能不能监听,比翻占用表可靠 —— 中间可能有竞态,但这里只是
// 选个没人用的端口,选错了下一次绑定会失败并重试。
func Free(from int) int {
	for p := from; p < from+200 && p <= 65535; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			_ = ln.Close()
			return p
		}
	}
	return 0
}

// FreeUDP 找一个空闲的 UDP 端口。
func FreeUDP(from int) int {
	for p := from; p < from+200 && p <= 65535; p++ {
		pc, err := net.ListenPacket("udp", fmt.Sprintf("0.0.0.0:%d", p))
		if err == nil {
			_ = pc.Close()
			return p
		}
	}
	return 0
}
