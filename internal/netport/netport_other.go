//go:build !windows

package netport

import (
	"fmt"
	"net"
)

// Owner 描述占用某个端口的进程。
type Owner struct {
	PID  uint32
	Name string
	Path string
}

// Find 在非 Windows 平台上查不到占用者 —— 这个程序只面向 Windows。
func Find(port int, udp bool) (Owner, bool) { return Owner{}, false }

// Kill 在非 Windows 平台上暂不支持。
func Kill(pid uint32) error {
	return fmt.Errorf("结束进程尚未在这个平台上实现")
}

// Free 从 from 开始找一个空闲的 TCP 端口。
func Free(from int) int { return freePort(from, "tcp") }

// FreeUDP 从 from 开始找一个空闲的 UDP 端口。
func FreeUDP(from int) int { return freePort(from, "udp") }

func freePort(from int, network string) int {
	for p := from; p < from+200 && p <= 65535; p++ {
		ln, err := net.Listen(network, fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			_ = ln.Close()
			return p
		}
	}
	return 0
}
