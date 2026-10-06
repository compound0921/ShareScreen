package main

import "testing"

// TestChooseLANIP 是这次 402 的回归测试。
//
// 真实故障:这台机器上 astral 虚拟网卡(10.126.126.1)在 net.Interfaces()
// 里排第一,老代码取"第一个私有地址"就把它当成了局域网地址,于是程序让
// 路由器把 8889 转发到 10.126.126.1 —— 一台不属于该局域网的主机,路由器
// 回 402 Invalid Args。
func TestChooseLANIP(t *testing.T) {
	tests := []struct {
		name     string
		outbound string
		scanned  []string
		want     string
	}{
		{
			name:     "默认路由出口压过枚举顺序 —— 就是这次的 bug",
			outbound: "192.168.3.236",
			scanned:  []string{"10.126.126.1", "192.168.3.236"},
			want:     "192.168.3.236",
		},
		{
			name:     "没有默认路由时退回枚举,私有地址仍然可用",
			outbound: "",
			scanned:  []string{"10.126.126.1"},
			want:     "10.126.126.1",
		},
		{
			name:     "出口地址是回环 —— 不可用,退回枚举",
			outbound: "127.0.0.1",
			scanned:  []string{"192.168.3.236"},
			want:     "192.168.3.236",
		},
		{
			name:     "出口地址是 169.254 自动配置地址 —— 连不上任何东西",
			outbound: "169.254.13.7",
			scanned:  []string{"192.168.3.236"},
			want:     "192.168.3.236",
		},
		{
			name:     "IPv6 不收 —— 这条链路只按 IPv4 验证过",
			outbound: "fe80::1",
			scanned:  []string{"192.168.3.236"},
			want:     "192.168.3.236",
		},
		{
			name:     "候选里的回环和非法值要跳过去",
			outbound: "",
			scanned:  []string{"127.0.0.1", "不是IP", "fe80::1", "192.168.1.9"},
			want:     "192.168.1.9",
		},
		{
			name:     "什么都没有就返回空串,而不是瞎猜一个",
			outbound: "",
			scanned:  nil,
			want:     "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseLANIP(tc.outbound, tc.scanned); got != tc.want {
				t.Errorf("chooseLANIP(%q, %v) = %q,想要 %q",
					tc.outbound, tc.scanned, got, tc.want)
			}
		})
	}
}

// TestScanLANIPsSkipsUnusable 确认枚举这一层不会把回环、链路本地和 IPv6 塞进来。
func TestScanLANIPsSkipsUnusable(t *testing.T) {
	for _, ip := range scanLANIPs() {
		if !isUsableLANIP(ip) {
			t.Errorf("枚举结果里有不能用的地址:%q", ip)
		}
	}
}
