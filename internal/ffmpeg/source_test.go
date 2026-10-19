package ffmpeg

import (
	"testing"

	"sharescreen/internal/config"
)

// TestDDAGrabFailureHint 钉住"哪些采集层错误值得翻译成人话"。
//
// 这两条特征串来自 T2 真机诊断(2026-10-19):混合显卡笔记本上
// DuplicateOutput 返回 DXGI_ERROR_UNSUPPORTED,ffmpeg 只报
// "Selected output not supported";强制换适配器后变成
// "Failed to enumerate DXGI output"。用户看到原文不知道该做什么。
func TestDDAGrabFailureHint(t *testing.T) {
	tests := []struct {
		name string
		tail string
		want bool // 是否应给出提示
	}{
		{
			name: "实测报错:输出不支持(适配器 0 无桌面输出)",
			tail: "[Parsed_ddagrab_0] Selected output not supported\n" +
				"Error opening input file ddagrab=framerate=30.",
			want: true,
		},
		{
			name: "强制适配器后枚举不到输出",
			tail: "[Parsed_ddagrab_0] Failed to enumerate DXGI output 0",
			want: true,
		},
		{
			name: "编码器错误不该给采集提示",
			tail: "Could not open encoder before EOF",
			want: false,
		},
		{
			name: "空输出",
			tail: "",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DDAGrabFailureHint(tc.tail)
			if (got != "") != tc.want {
				t.Errorf("DDAGrabFailureHint(%q) = %q,想要非空=%v", tc.tail, got, tc.want)
			}
		})
	}
}

// WGC 的采集范围和远控支持:第一阶段只采主显示器,和 ddagrab 一样。
func TestWGCSourceHelpers(t *testing.T) {
	v := config.VideoConfig{Source: config.SourceScreenWGC}

	if !SupportsRemote(v) {
		t.Error("WGC 整屏采集应当支持远程控制")
	}
	if got := ViewerRotation(v, 180); got != 0 {
		t.Errorf("WGC 走 DWM 合成,ViewerRotation = %d,想要 0", got)
	}

	r, ok := CaptureRect(v)
	if !ok {
		t.Skip("本机拿不到桌面尺寸")
	}
	if r.X != 0 || r.Y != 0 {
		t.Errorf("WGC 只采主屏,原点应当是 (0,0),得到 (%d,%d)", r.X, r.Y)
	}
	if r.W <= 0 || r.H <= 0 {
		t.Errorf("WGC 采集矩形尺寸无效: %dx%d", r.W, r.H)
	}
}
