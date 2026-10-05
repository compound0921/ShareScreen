//go:build windows

package gpu

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procEnumDisplayDevicesW = user32.NewProc("EnumDisplayDevicesW")
)

// DISPLAY_DEVICE.StateFlags 里的位。
const (
	stateAttachedToDesktop = 0x1
	statePrimaryDevice     = 0x4
)

// displayDeviceW 对应 Win32 的 DISPLAY_DEVICE(W 版)。
//
// 尺寸恰好 840 字节:
//
//	cb 4 + DeviceName 32*2 + DeviceString 128*2 + StateFlags 4
//	   + DeviceID 128*2 + DeviceKey 128*2
//
// 数组元素是 UTF-16,按 2 字节对齐,而 StateFlags 落在偏移 324(4 的倍数),
// 所以中间没有填充。但"应该没有"不作数 —— 用编译期断言钉死,对不上直接
// 编不过。同一个教训在 WAVEFORMATEX 和 INPUT 上各踩过一次
// (docs/架构设计.md §5.5、internal/input/input_windows.go)。
type displayDeviceW struct {
	cb           uint32
	DeviceName   [32]uint16
	DeviceString [128]uint16
	StateFlags   uint32
	DeviceID     [128]uint16
	DeviceKey    [128]uint16
}

var _ [unsafe.Sizeof(displayDeviceW{})]byte = [840]byte{}

var (
	adaptersOnce sync.Once
	adaptersList []Adapter
)

// Adapters 返回本机的显示适配器(已去重)。
//
// 结果缓存一次:适配器列表在一个进程生命周期内不会变。插拔显卡、eGPU
// 热插拔、休眠唤醒之后桌面可能换到另一块卡上,而缓存不会跟着刷新 ——
// 这是已知限制,首版不做 WM_DEVICECHANGE 监听(要重启程序才会重新识别)。
func Adapters() []Adapter {
	adaptersOnce.Do(func() { adaptersList = enumerate() })
	return adaptersList
}

func enumerate() []Adapter {
	var out []Adapter
	seen := map[string]bool{}

	for i := 0; ; i++ {
		var dd displayDeviceW

		// cb 是**入参**,必须每次重设。漏了它会直接返回失败,枚举出 0 个,
		// 而且不报任何错 —— 表现就是"识别不到显卡",极难归因。
		dd.cb = uint32(unsafe.Sizeof(dd))

		r, _, _ := procEnumDisplayDevicesW.Call(
			0, uintptr(i), uintptr(unsafe.Pointer(&dd)), 0)
		if r == 0 {
			break // 枚举结束
		}

		id := windows.UTF16ToString(dd.DeviceID[:])

		// 同一块卡会按它的每一个输出各出现一次(本机实测出现 4 次),
		// 按 DeviceID 去重。不去重的话同一块卡会在排序里占好几个位置。
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		out = append(out, Adapter{
			Name:     windows.UTF16ToString(dd.DeviceString[:]),
			DeviceID: id,
			Vendor:   vendorOf(id),
			Attached: dd.StateFlags&stateAttachedToDesktop != 0,
			Primary:  dd.StateFlags&statePrimaryDevice != 0,
		})
	}
	return out
}
