//go:build !windows

package input

import "sharescreen/internal/screen"

// 只有 Windows 有 SendInput。这个桩让 `go vet ./...` 能在其他平台跑通,
// 顺便让 keys.go 里的映射表在任意平台上都能被测试。

func MoveNorm(area screen.Rect, nx, ny float64) error { return ErrUnsupported }

func ButtonEvent(b Button, down bool) error { return ErrUnsupported }

func Wheel(dx, dy int) error { return ErrUnsupported }

func KeyEvent(vk uint16, extended, down bool) error { return ErrUnsupported }

func TypeText(s string) error { return ErrUnsupported }

func ElevatedForeground() bool { return false }
