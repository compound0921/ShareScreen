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

// TestViewerRotation 钉住"要不要让观众把画面转回来"这条规则。
//
// 它是按采集源分的:ddagrab 走 DXGI,拿到的是未旋转的原始画面;gdigrab 走
// GDI,拿到的已经转好了。**分错了的表现是画面倒着、而且没有任何报错** ——
// 所以这条规则值得一条测试。
func TestViewerRotation(t *testing.T) {
	cases := []struct {
		name string
		src  config.SourceType
		rot  int
		want int
	}{
		{"GPU 采集 + 倒装的屏", config.SourceScreenDDAGrab, 180, 180},
		{"GPU 采集 + 正常的屏", config.SourceScreenDDAGrab, 0, 0},
		{"兼容模式不用补 —— 它拿到的是转好的画面", config.SourceScreenGDI, 180, 0},
		{"窗口采集同样不用补", config.SourceWindow, 180, 0},
		{"90° 不处理(长短边会互换,牵一发动全身)", config.SourceScreenDDAGrab, 90, 0},
		{"270° 同上", config.SourceScreenDDAGrab, 270, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ViewerRotation(config.VideoConfig{Source: c.src}, c.rot); got != c.want {
				t.Errorf("ViewerRotation(%s, %d) = %d,想要 %d", c.src, c.rot, got, c.want)
			}
		})
	}
}

// TestFilterArgsZeroCopy 钉住"d3d11 帧什么时候可以直接交给编码器"。
//
// 这条判据曾经漏了编码器:T2 里 GPU 采集在三台别的设备上都起不来,
// 根因就是 ddagrab + 非 NVENC 时仍然走了零拷贝,ffmpeg 报
// "Impossible to convert between the formats supported by the filter"。
func TestFilterArgsZeroCopy(t *testing.T) {
	nvenc := Encoder{Name: "h264_nvenc", Kind: "nvenc", Hardware: true}
	qsv := Encoder{Name: "h264_qsv", Kind: "qsv", Hardware: true}
	x264 := Encoder{Name: "libx264", Kind: "x264"}

	// 桌面是 2560×1600。sameSize 的目标尺寸与它相同(1:1,不缩放),
	// scaled 的目标是 1280×800。
	sameSize := config.VideoConfig{Source: config.SourceScreenDDAGrab, FPS: 60, Width: 2560, Height: 1600}
	scaled := config.VideoConfig{Source: config.SourceScreenDDAGrab, FPS: 60, Width: 1280, Height: 800}
	gdiSame := config.VideoConfig{Source: config.SourceScreenGDI, FPS: 60, Width: 2560, Height: 1600}
	gdiScaled := config.VideoConfig{Source: config.SourceScreenGDI, FPS: 60, Width: 1280, Height: 800}

	tests := []struct {
		name       string
		v          config.VideoConfig
		enc        Encoder
		srcW, srcH int
		wantVF     string // 空表示不应出现 -vf
		wantPixFmt bool   // 是否应出现 -pix_fmt yuv420p
	}{
		{
			name: "ddagrab + NVENC + 不缩放 —— 零拷贝,滤镜和 -pix_fmt 都不能有",
			v:    sameSize, enc: nvenc, srcW: 2560, srcH: 1600,
		},
		{
			name: "ddagrab + NVENC + 缩放 —— 下载后缩放",
			v:    scaled, enc: nvenc, srcW: 2560, srcH: 1600,
			wantVF: "hwdownload,format=bgra,scale=1280:800", wantPixFmt: true,
		},
		{
			// T2 回归用例。不加 hwdownload 的话 ffmpeg 直接报
			// "Impossible to convert between the formats supported by the filter"。
			name: "ddagrab + QSV + 不缩放 —— 必须下载回内存",
			v:    sameSize, enc: qsv, srcW: 2560, srcH: 1600,
			wantVF: "hwdownload,format=bgra", wantPixFmt: true,
		},
		{
			name: "ddagrab + x264 + 不缩放 —— 同上",
			v:    sameSize, enc: x264, srcW: 2560, srcH: 1600,
			wantVF: "hwdownload,format=bgra", wantPixFmt: true,
		},
		{
			name: "gdigrab + 不缩放 —— 内存帧,不加滤镜但照常指定 -pix_fmt",
			v:    gdiSame, enc: x264, srcW: 2560, srcH: 1600,
			wantPixFmt: true,
		},
		{
			name: "gdigrab + 缩放 —— 普通 scale",
			v:    gdiScaled, enc: x264, srcW: 2560, srcH: 1600,
			wantVF: "scale=1280:800", wantPixFmt: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := buildArgsForTest(t, tc.v, tc.enc, tc.srcW, tc.srcH)

			vf := ""
			if i := indexOf(args, "-vf"); i >= 0 {
				if i+1 >= len(args) {
					t.Fatalf("-vf 后面没有值:%v", args)
				}
				vf = args[i+1]
			}
			if vf != tc.wantVF {
				t.Errorf("-vf = %q,想要 %q(%v)", vf, tc.wantVF, args)
			}

			hasPixFmt := indexOf(args, "-pix_fmt") >= 0
			if hasPixFmt != tc.wantPixFmt {
				t.Errorf("-pix_fmt 出现 = %v,想要 %v(%v)", hasPixFmt, tc.wantPixFmt, args)
			}
			if hasPixFmt {
				if got := valueOf(t, args, "-pix_fmt"); got != "yuv420p" {
					t.Errorf("-pix_fmt = %q,想要 yuv420p", got)
				}
			}
		})
	}
}

// buildArgsForTest 用给定的源、编码器和采集尺寸拼一遍完整命令。
func buildArgsForTest(t *testing.T, v config.VideoConfig, enc Encoder, srcW, srcH int) []string {
	t.Helper()
	args, err := BuildArgs(v, CaptureOpts{DrawMouse: true}, nil, enc,
		"rtsp://127.0.0.1:8554/live", srcW, srcH)
	if err != nil {
		t.Fatalf("BuildArgs 报错:%v", err)
	}
	return args
}
