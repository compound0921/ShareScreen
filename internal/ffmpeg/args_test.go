package ffmpeg

import (
	"strings"
	"testing"

	"sharescreen/internal/config"
)

// indexOf 返回 tok 在 args 里的下标,没有则 -1。
func indexOf(args []string, tok string) int {
	for i, a := range args {
		if a == tok {
			return i
		}
	}
	return -1
}

// valueOf 返回 args 里 opt 后面跟着的那个值。
//
// 光标相关的两个参数都是"选项 + 值"成对的,只检查选项出现过是不够的 ——
// 值写错(比如 -draw_mouse 1)照样是坏的。
func valueOf(t *testing.T, args []string, opt string) string {
	t.Helper()
	i := indexOf(args, opt)
	if i < 0 {
		t.Fatalf("参数里没有 %s:%v", opt, args)
	}
	if i+1 >= len(args) {
		t.Fatalf("%s 后面没有值:%v", opt, args)
	}
	return args[i+1]
}

func screenCfg(src config.SourceType) config.VideoConfig {
	return config.VideoConfig{Source: src, FPS: 60}
}

// TestInputArgsDrawMouse 覆盖四种采集源 × 画不画光标。
func TestInputArgsDrawMouse(t *testing.T) {
	tests := []struct {
		name    string
		src     config.SourceType
		draw    bool
		wantIn  string // -i 后面期望的输入串(非空时检查)
		wantOpt bool   // 是否期望出现 -draw_mouse 0
	}{
		{
			name:   "ddagrab 画光标 —— 默认路径必须和改动前逐字节一致",
			src:    config.SourceScreenDDAGrab,
			draw:   true,
			wantIn: "ddagrab=framerate=60",
		},
		{
			name:   "ddagrab 不画光标",
			src:    config.SourceScreenDDAGrab,
			draw:   false,
			wantIn: "ddagrab=framerate=60:draw_mouse=0",
		},
		{
			name:    "gdigrab 不画光标",
			src:     config.SourceScreenGDI,
			draw:    false,
			wantOpt: true,
		},
		{
			name: "gdigrab 画光标 —— 不该多出这个选项",
			src:  config.SourceScreenGDI,
			draw: true,
		},
		{
			// 窗口和 OBS 都不支持远程控制,CaptureOptsFor 到不了 false;
			// 真收到了也一律忽略(见 source.go 里的注释)。
			name: "窗口采集 —— 光标一律画",
			src:  config.SourceWindow, draw: false,
		},
		{
			name: "OBS 虚拟摄像头 —— dshow 压根没有这个选项",
			src:  config.SourceOBS, draw: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := screenCfg(tc.src)
			if tc.src == config.SourceWindow {
				v.WindowTitle = "记事本"
			}
			args, err := inputArgs(v, CaptureOpts{DrawMouse: tc.draw})
			if err != nil {
				t.Fatalf("inputArgs 报错:%v", err)
			}

			if tc.wantIn != "" {
				if got := valueOf(t, args, "-i"); got != tc.wantIn {
					t.Errorf("-i 的值 = %q,想要 %q", got, tc.wantIn)
				}
			}

			has := indexOf(args, "-draw_mouse") >= 0
			if has != tc.wantOpt {
				t.Errorf("-draw_mouse 出现 = %v,想要 %v(%v)", has, tc.wantOpt, args)
			}
			if tc.wantOpt {
				if got := valueOf(t, args, "-draw_mouse"); got != "0" {
					t.Errorf("-draw_mouse 的值 = %q,想要 \"0\"", got)
				}
				// 顺序是承重的:它是输入选项,必须排在 -i 之前,
				// 排到后面 ffmpeg 直接拒绝启动。
				if indexOf(args, "-draw_mouse") > indexOf(args, "-i") {
					t.Errorf("-draw_mouse 必须排在 -i 之前:%v", args)
				}
			}

			// ddagrab 用的是滤镜内联选项,不该再冒出 -draw_mouse。
			if tc.src == config.SourceScreenDDAGrab && has {
				t.Errorf("ddagrab 不该用 -draw_mouse,应该写进滤镜串:%v", args)
			}
		})
	}
}

// TestBuildArgsThreadsCaptureOpts 走一遍完整命令行,确认 CaptureOpts 没有
// 在中间被丢掉 —— 将来有人改 BuildArgs 签名时这条会先炸。
func TestBuildArgsThreadsCaptureOpts(t *testing.T) {
	enc := Encoder{Name: "h264_nvenc", Kind: "nvenc", Hardware: true}
	v := screenCfg(config.SourceScreenDDAGrab)

	args, err := BuildArgs(v, CaptureOpts{DrawMouse: false}, nil, enc, "rtsp://127.0.0.1:8554/live", 0, 0)
	if err != nil {
		t.Fatalf("BuildArgs 报错:%v", err)
	}
	if got := valueOf(t, args, "-i"); got != "ddagrab=framerate=60:draw_mouse=0" {
		t.Errorf("-i 的值 = %q,想要带上 draw_mouse=0", got)
	}

	on, err := BuildArgs(v, CaptureOpts{DrawMouse: true}, nil, enc, "rtsp://127.0.0.1:8554/live", 0, 0)
	if err != nil {
		t.Fatalf("BuildArgs 报错:%v", err)
	}
	if got := valueOf(t, on, "-i"); strings.Contains(got, "draw_mouse") {
		t.Errorf("画光标时不该出现 draw_mouse:-i = %q", got)
	}
}

// TestCaptureOptsFor 是光标门控的真值表 —— 这是整个功能的开关所在。
func TestCaptureOptsFor(t *testing.T) {
	tests := []struct {
		name          string
		src           config.SourceType
		remoteEnabled bool
		wantDraw      bool
	}{
		{"整屏 + 远控关 → 画", config.SourceScreenDDAGrab, false, true},
		{"整屏 + 远控开 → 不画", config.SourceScreenDDAGrab, true, false},
		{"gdigrab + 远控开 → 不画", config.SourceScreenGDI, true, false},
		// 这两种源不支持远控,开关对它们没有意义,不能白白弄丢光标。
		{"窗口 + 远控开 → 仍然画", config.SourceWindow, true, true},
		{"OBS + 远控开 → 仍然画", config.SourceOBS, true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CaptureOptsFor(screenCfg(tc.src), tc.remoteEnabled)
			if got.DrawMouse != tc.wantDraw {
				t.Errorf("DrawMouse = %v,想要 %v", got.DrawMouse, tc.wantDraw)
			}
		})
	}
}
