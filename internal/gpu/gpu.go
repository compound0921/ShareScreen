// Package gpu 识别本机的显示适配器及其厂商。
//
// 用途是给编码器排序:ddagrab 抓的是**驱动显示器那块 GPU** 的画面,如果
// 编码落在另一块 GPU 上,帧要跨 PCIe 走一趟(见 docs/架构设计.md §5.4)。
// 让编码器和显示器同厂商,就能避开这次拷贝。
//
// 这里只做一件事:枚举显示适配器、读出它的 PCI 厂商 ID。**不碰 DXGI、
// 不用 COM** —— 一个 EnumDisplayDevices 调用就够了,而且不必关心 DXGI
// 那个"同一时刻只允许一个采集会话"的约束。
//
// 这个文件里的东西都是纯字符串/切片处理,不依赖任何系统调用,所以测试
// 能在任意平台上跑(平台相关的只有 Adapters 本身)。
package gpu

import "strings"

// Vendor 是显卡厂商。
type Vendor int

const (
	VendorUnknown Vendor = iota
	VendorNVIDIA
	VendorIntel
	VendorAMD
)

func (v Vendor) String() string {
	switch v {
	case VendorNVIDIA:
		return "NVIDIA"
	case VendorIntel:
		return "Intel"
	case VendorAMD:
		return "AMD"
	}
	return "未知"
}

// Adapter 是一块显示适配器。
type Adapter struct {
	Name     string // 例如 "NVIDIA GeForce RTX 5060 Laptop GPU"
	DeviceID string // 形如 PCI\VEN_10DE&DEV_2D59&SUBSYS_...
	Vendor   Vendor

	// Attached 表示这块适配器接着显示器。
	Attached bool

	// Primary 表示它驱动着主显示器。
	Primary bool
}

// 各家显卡的 PCI 厂商 ID。
const (
	venNVIDIA = "VEN_10DE"
	venIntel  = "VEN_8086"
	venAMD1   = "VEN_1002"
	venAMD2   = "VEN_1022"
)

// vendorOf 从 DeviceID 里解析 PCI 厂商 ID。
//
// 没有 VEN_ 前缀的一律算未知 —— 虚拟显示器和远程会话就是这一类:
// 向日葵的 OrayIddDriver、Parsec 的虚拟显示器、RDP 的 remote display
// adapter,它们的 DeviceID 都是 `Root\xxx`。微软基本显示适配器则是
// VEN_1414,同样不是我们要排序的对象。
func vendorOf(deviceID string) Vendor {
	up := strings.ToUpper(deviceID)
	switch {
	case strings.Contains(up, venNVIDIA):
		return VendorNVIDIA
	case strings.Contains(up, venIntel):
		return VendorIntel
	case strings.Contains(up, venAMD1), strings.Contains(up, venAMD2):
		return VendorAMD
	}
	return VendorUnknown
}

// pickPrimary 从适配器列表里挑出驱动主显示器那块,返回其厂商。
//
// 判据是 Primary 标志(StateFlags 里的 PRIMARY_DEVICE),**不是枚举顺序** ——
// 混合显卡笔记本上第 0 项未必是主输出,而且不同版本的 Windows 给出的顺序
// 也不保证一致。
func pickPrimary(list []Adapter) Vendor {
	var attached []Adapter
	for _, a := range list {
		if !a.Attached {
			continue
		}
		if a.Primary {
			return a.Vendor
		}
		attached = append(attached, a)
	}
	// 没有显式标 PRIMARY 的(某些虚拟显示器方案会这样)。退一步用第一块
	// 接着显示器的 —— 多显示器时这是猜的,但比直接说不知道强。
	if len(attached) > 0 {
		return attached[0].Vendor
	}
	return VendorUnknown
}

// PrimaryVendor 返回驱动主显示器那块适配器的厂商。
//
// 拿不到就返回 VendorUnknown,调用方据此退回到原本的编码器优先级 ——
// **识别失败绝不能影响启动**。
func PrimaryVendor() Vendor {
	return pickPrimary(Adapters())
}
