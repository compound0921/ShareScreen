//go:build !windows

package screen

// 本项目的采集路径只面向 Windows。这个桩让 `go vet ./...` 能在其他平台跑通。

func SetDPIAware() {}

func Size() (w, h int) { return 0, 0 }
