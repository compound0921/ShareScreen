package stream

import (
	"strings"
	"testing"
)

// TestParseWGCReady 钉住采集进程的 readiness 协议。
//
// 协议只有一行 JSON;解析错了的表现是"WGC 一直起不来",而且日志里
// 看不出是格式问题。
func TestParseWGCReady(t *testing.T) {
	const good = `{"event":"ready","version":"0.1.0","width":1920,"height":1200,"fps":60}`

	t.Run("正常 ready", func(t *testing.T) {
		vi, matched, err := parseWGCReady(good, 60)
		if err != nil {
			t.Fatalf("parseWGCReady 报错:%v", err)
		}
		if !matched {
			t.Fatal("应当识别为 ready")
		}
		if vi.Width != 1920 || vi.Height != 1200 || vi.FPS != 60 {
			t.Errorf("解析结果 = %+v", vi)
		}
		if vi.PixelFmt != "nv12" {
			t.Errorf("PixelFmt = %q,想要 nv12", vi.PixelFmt)
		}
	})

	t.Run("普通日志行不算 ready", func(t *testing.T) {
		_, matched, err := parseWGCReady("capture started", 60)
		if err != nil || matched {
			t.Errorf("matched=%v err=%v,想要 false/nil", matched, err)
		}
	})

	t.Run("非 ready 事件不算 ready", func(t *testing.T) {
		_, matched, err := parseWGCReady(`{"event":"tick"}`, 60)
		if err != nil || matched {
			t.Errorf("matched=%v err=%v,想要 false/nil", matched, err)
		}
	})

	t.Run("帧率不一致要报错", func(t *testing.T) {
		_, matched, err := parseWGCReady(good, 30)
		if !matched || err == nil {
			t.Errorf("matched=%v err=%v,想要 true/error", matched, err)
		}
	})

	t.Run("奇数尺寸要报错", func(t *testing.T) {
		const odd = `{"event":"ready","version":"0.1.0","width":1919,"height":1200,"fps":60}`
		_, matched, err := parseWGCReady(odd, 60)
		if !matched || err == nil {
			t.Errorf("matched=%v err=%v,想要 true/error", matched, err)
		}
	})

	t.Run("零尺寸要报错", func(t *testing.T) {
		const zero = `{"event":"ready","version":"0.1.0","width":0,"height":0,"fps":60}`
		_, matched, err := parseWGCReady(zero, 60)
		if !matched || err == nil {
			t.Errorf("matched=%v err=%v,想要 true/error", matched, err)
		}
	})
}

func TestWGCPipeNameUnique(t *testing.T) {
	a := wgcPipeName()
	b := wgcPipeName()
	if a == b {
		t.Errorf("两次生成的管道名相同: %q", a)
	}
	if !strings.HasPrefix(a, `\\.\pipe\`) {
		t.Errorf("管道名格式不对: %q", a)
	}
}
