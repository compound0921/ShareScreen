//go:build !windows

package screen

// 本项目的采集路径只面向 Windows。这个桩让 `go vet ./...` 能在其他平台跑通。
func DisplayRotation() int { return 0 }
