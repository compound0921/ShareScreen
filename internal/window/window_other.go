//go:build !windows

package window

import "errors"

// 本项目只面向 Windows。这个桩让 `go vet ./...` 能在其他平台跑通。

type Window struct {
	Title     string `json:"title"`
	Process   string `json:"process"`
	PID       uint32 `json:"pid"`
	Minimized bool   `json:"minimized"`
}

func List() ([]Window, error) {
	return nil, errors.New("window: 仅支持 Windows")
}
