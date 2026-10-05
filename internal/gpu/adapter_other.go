//go:build !windows

package gpu

// 本项目只面向 Windows。这个桩让 `go vet ./...` 能在其他平台跑通。

func Adapters() []Adapter { return nil }
