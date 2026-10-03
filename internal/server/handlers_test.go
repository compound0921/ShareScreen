package server

import (
	"strings"
	"testing"

	"sharescreen/internal/config"
	"sharescreen/internal/portmap"
)

// 「映射开好了、链接却打不开」是最难查的一类故障:状态灯是绿的,每一步看着
// 都对,只有端口的对应关系是错的。这里锁住那条检查。
func TestNoteHostMismatch(t *testing.T) {
	active := portmap.Snapshot{State: portmap.StateActive}

	cases := []struct {
		name       string
		publicHost string
		webrtcPort int
		wantWarn   bool
	}{
		{"端口一致", "example.com:8889", 8889, false},
		{"端口对不上", "112.226.166.178:8443", 8889, true},
		{"没写端口", "example.com", 8889, false},
		{"没填公网地址", "", 8889, false},
	}

	for _, c := range cases {
		cfg := config.Config{PublicHost: c.publicHost, WebRTCPort: c.webrtcPort}
		snap := active
		snap.Hint = "原有的提示"
		noteHostMismatch(cfg, &snap)

		gotWarn := strings.Contains(snap.Hint, "对不上")
		if gotWarn != c.wantWarn {
			t.Errorf("%s: 提示 = %q,想要警告=%v", c.name, snap.Hint, c.wantWarn)
		}
		if c.wantWarn && !strings.Contains(snap.Hint, "8889") {
			t.Errorf("%s: 提示里应当给出正确的端口,实际 %q", c.name, snap.Hint)
		}
		if !c.wantWarn && snap.Hint != "原有的提示" {
			t.Errorf("%s: 不该动原有的提示,实际 %q", c.name, snap.Hint)
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
