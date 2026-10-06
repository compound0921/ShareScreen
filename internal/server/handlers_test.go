package server

import (
	"strings"
	"testing"

	"sharescreen/internal/config"
	"sharescreen/internal/portmap"
)

// 「映射开好了、链接却打不开」是最难查的一类故障:状态灯是绿的,每一步看着
// 都对。这里锁住那条检查。
func TestNoteHostMismatch(t *testing.T) {
	const realIP = "112.254.141.83"

	cases := []struct {
		name       string
		publicHost string
		externalIP string
		wantIn     string // 提示里必须出现的东西;空串表示不该报警
	}{
		{"都对得上", "example.com:8889", realIP, ""},
		{"IP 就是当前的外网地址", realIP + ":8889", realIP, ""},

		// 下面这两种是实测踩到的:手填的 IP 是上一次的公网地址,家宽 IP 变了,
		// 而手动填的地址程序不会去改 —— 于是公网链接对所有人都打不开,
		// 状态灯却一直是绿的。
		{"IP 是旧的", "112.226.166.178:8889", realIP, realIP},
		{"IP 是旧的(没写端口)", "112.226.166.178", realIP, realIP},

		{"端口对不上", "example.com:8443", realIP, "8889"},

		// 不带端口**不该**警告:端口不归这个字段负责,拼链接时由
		// publicWatchHost 补上。以前这里会警告"链接会走 80",那是因为
		// 当时地址是原样拼进 URL 的 —— 那个缺陷已经修掉了。
		{"域名不带端口", "example.com", realIP, ""},
		{"IP 不带端口", realIP, realIP, ""},

		{"没填公网地址", "", realIP, ""},
	}

	for _, c := range cases {
		cfg := config.Config{PublicHost: c.publicHost, WebRTCPort: 8889}
		snap := portmap.Snapshot{State: portmap.StateActive, ExternalIP: c.externalIP}
		snap.Hint = "原有的提示"
		noteHostMismatch(cfg, &snap)

		if c.wantIn == "" {
			if snap.Hint != "原有的提示" {
				t.Errorf("%s: 不该动原有的提示,实际 %q", c.name, snap.Hint)
			}
			continue
		}
		if !strings.Contains(snap.Hint, c.wantIn) {
			t.Errorf("%s: 提示里应当出现 %q,实际 %q", c.name, c.wantIn, snap.Hint)
		}
	}
}

// 只有真的映射成功时才谈得上"对不上" —— 失败了另有失败文案,别混进去。
func TestNoteHostMismatchOnlyWhenActive(t *testing.T) {
	cfg := config.Config{PublicHost: "example.com:8443", WebRTCPort: 8889}
	snap := portmap.Snapshot{State: portmap.StateFailed, Hint: "失败提示"}
	noteHostMismatch(cfg, &snap)

	if snap.Hint != "失败提示" {
		t.Errorf("非 active 状态不该被改,实际 %q", snap.Hint)
	}
}

func TestHostOnly(t *testing.T) {
	if got := hostOnly("example.com:8443"); got != "example.com" {
		t.Errorf("hostOnly = %q", got)
	}
	if got := hostOnly("example.com"); got != "example.com" {
		t.Errorf("hostOnly(无端口) = %q", got)
	}
	if got := hostOnly("112.226.166.178:8889"); got != "112.226.166.178" {
		t.Errorf("hostOnly(IP) = %q", got)
	}
}

// 公网地址不管端口 —— 拼链接时程序自己补。
//
// 这条是为了挡住一个具体的坑:地址原样拼进 URL,用户填了 IP 忘了端口,链接
// 就变成 http://<IP>/live/ 走 80,而状态灯是绿的、地址也在,看不出任何问题。
// 现在用户只管填"这台机器在公网上叫什么"。
func TestPublicWatchHost(t *testing.T) {
	tests := []struct {
		name       string
		publicHost string
		webrtcPort int
		want       string
	}{
		{"没写端口 —— 补上观看端口", "112.226.166.178", 8889, "112.226.166.178:8889"},
		{"域名没写端口 —— 同样补上", "share.example.com", 8889, "share.example.com:8889"},
		{"写了端口 —— 用他写的(手动部署可能翻译过端口)",
			"share.example.com:8443", 8889, "share.example.com:8443"},
		{"非默认观看端口 -- 补出来的也是它", "10.0.0.1", 9000, "10.0.0.1:9000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{PublicHost: tc.publicHost, WebRTCPort: tc.webrtcPort}
			if got := publicWatchHost(cfg); got != tc.want {
				t.Errorf("publicWatchHost = %q,想要 %q", got, tc.want)
			}
		})
	}
}
