//go:build !windows

package audio

import (
	"context"
	"errors"
	"io"
)

// ErrUnsupported 表示当前平台没有桌面音频采集能力。
// 这个程序只面向 Windows,桩的作用是让别的平台还能编译通过。
var ErrUnsupported = errors.New("桌面音频采集仅支持 Windows")

// Capture 是非 Windows 平台上的空实现。
type Capture struct{ format Format }

// Open 在所有非 Windows 平台上都失败。
func Open() (*Capture, error) { return nil, ErrUnsupported }

// OpenDevice 在所有非 Windows 平台上都失败。
func OpenDevice(string) (*Capture, error) { return nil, ErrUnsupported }

// ListDevices 在所有非 Windows 平台上都返回空列表。
func ListDevices() ([]Device, error) { return nil, ErrUnsupported }

// Format 返回零值 —— 这些平台上根本走不到这里。
func (c *Capture) Format() Format { return c.format }

// DeviceID 返回空串 —— 非 Windows 平台没有设备可选。
func (c *Capture) DeviceID() string { return "" }

// DeviceName 返回空串。
func (c *Capture) DeviceName() string { return "" }

// Close 什么也不做。
func (c *Capture) Close() {}

// Run 在所有非 Windows 平台上都失败。
func (c *Capture) Run(ctx context.Context, w io.Writer) error { return ErrUnsupported }
