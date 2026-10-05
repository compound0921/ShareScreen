//go:build !windows

package clipboard

import "errors"

// 本项目只面向 Windows。这个桩让 `go vet ./...` 能在其他平台跑通。

var errUnsupported = errors.New("clipboard: 仅支持 Windows")

func sequence() uint32 { return 0 }

func readText() (string, error) { return "", errUnsupported }

func writeText(string) error { return errUnsupported }
