package gpu

import "testing"

// 本机的真实 DeviceID 拿来当样本。
func TestVendorOf(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want Vendor
	}{
		{"NVIDIA", `PCI\VEN_10DE&DEV_2D59&SUBSYS_802F17AA&REV_A1\98B8A494872DB04800`, VendorNVIDIA},
		{"Intel", `PCI\VEN_8086&DEV_A78B&SUBSYS_802F17AA&REV_04\3&11583659&0&10`, VendorIntel},
		{"AMD 独显", `PCI\VEN_1002&DEV_744C&SUBSYS_0E3A1002&REV_C8`, VendorAMD},
		{"AMD 核显", `PCI\VEN_1022&DEV_15E7&SUBSYS_15E71022&REV_C1`, VendorAMD},

		// 下面这些都不是"我们要排序的真显卡",必须归未知,否则会把虚拟
		// 显示器当成一块 GPU 参与排序。
		{"向日葵虚拟显示器", `Root\OrayIddDriver`, VendorUnknown},
		{"微软基本显示适配器", `PCI\VEN_1414&DEV_0086&SUBSYS_00000000&REV_00`, VendorUnknown},
		{"空串", ``, VendorUnknown},

		// 厂商 ID 大小写不固定,别假设都是大写
		{"小写也能认", `pci\ven_10de&dev_2d59`, VendorNVIDIA},
		{"混写也能认", `PCI\Ven_8086&Dev_A78B`, VendorIntel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vendorOf(tt.id); got != tt.want {
				t.Errorf("vendorOf(%q) = %v, 期望 %v", tt.id, got, tt.want)
			}
		})
	}
}

// 本机是一台混合显卡笔记本:显示器挂在 NVIDIA 上,Intel 核显闲置,
// 另外还装了个向日葵的虚拟显示器。这条测试用的就是实测拿到的那组数据。
func TestPickPrimary(t *testing.T) {
	// 实测输出(每块卡按输出重复了若干次,Adapters 那边会去重)
	realMachine := []Adapter{
		{Name: "NVIDIA GeForce RTX 5060 Laptop GPU", DeviceID: "PCI\\VEN_10DE...", Vendor: VendorNVIDIA, Attached: true, Primary: true},
		{Name: "Intel(R) UHD Graphics", DeviceID: "PCI\\VEN_8086...", Vendor: VendorIntel, Attached: false, Primary: false},
		{Name: "OrayIddDriver Device", DeviceID: "Root\\OrayIddDriver", Vendor: VendorUnknown, Attached: false, Primary: false},
	}

	t.Run("认 Primary 标志而不是顺序", func(t *testing.T) {
		if got := pickPrimary(realMachine); got != VendorNVIDIA {
			t.Errorf("得到 %v,期望 NVIDIA", got)
		}
	})

	t.Run("主显示那块排在后面也要认出来", func(t *testing.T) {
		// 混合显卡机器上枚举顺序不保证,Intel 排前面是常态
		list := []Adapter{
			{Vendor: VendorIntel, Attached: true, Primary: false},
			{Vendor: VendorNVIDIA, Attached: true, Primary: true},
		}
		if got := pickPrimary(list); got != VendorNVIDIA {
			t.Errorf("得到 %v,期望 NVIDIA", got)
		}
	})

	t.Run("没有 Primary 标志时退而用第一块接了显示器的", func(t *testing.T) {
		list := []Adapter{
			{Vendor: VendorIntel, Attached: false, Primary: false},
			{Vendor: VendorIntel, Attached: true, Primary: false},
		}
		if got := pickPrimary(list); got != VendorIntel {
			t.Errorf("得到 %v,期望 Intel", got)
		}
	})

	t.Run("一块接了显示器的都没有就返回未知", func(t *testing.T) {
		// 无头机、纯 RDP 会话。不能崩,也不能瞎猜一个厂商。
		list := []Adapter{
			{Vendor: VendorNVIDIA, Attached: false, Primary: false},
			{Vendor: VendorUnknown, Attached: false, Primary: false},
		}
		if got := pickPrimary(list); got != VendorUnknown {
			t.Errorf("得到 %v,期望 未知", got)
		}
	})

	t.Run("空列表", func(t *testing.T) {
		if got := pickPrimary(nil); got != VendorUnknown {
			t.Errorf("得到 %v,期望 未知", got)
		}
	})

	t.Run("只有虚拟显示器时是未知而不是崩掉", func(t *testing.T) {
		list := []Adapter{
			{Name: "OrayIddDriver Device", Vendor: VendorUnknown, Attached: true, Primary: true},
		}
		if got := pickPrimary(list); got != VendorUnknown {
			t.Errorf("得到 %v,期望 未知", got)
		}
	})
}

func TestVendorString(t *testing.T) {
	for v, want := range map[Vendor]string{
		VendorNVIDIA:  "NVIDIA",
		VendorIntel:   "Intel",
		VendorAMD:     "AMD",
		VendorUnknown: "未知",
	} {
		if got := v.String(); got != want {
			t.Errorf("Vendor(%d).String() = %q, 期望 %q", v, got, want)
		}
	}
}
