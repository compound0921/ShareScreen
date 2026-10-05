package ffmpeg

import (
	"testing"

	"sharescreen/internal/gpu"
)

// allAvailable 是四个编码器都能用时 knownEncoders 的原始顺序。
func allAvailable() []Encoder {
	return []Encoder{
		{Name: "h264_nvenc", Kind: "nvenc", Hardware: true},
		{Name: "h264_qsv", Kind: "qsv", Hardware: true},
		{Name: "h264_amf", Kind: "amf", Hardware: true},
		{Name: "libx264", Kind: "x264", Hardware: false},
	}
}

func kinds(list []Encoder) string {
	out := ""
	for i, e := range list {
		if i > 0 {
			out += ","
		}
		out += e.Kind
	}
	return out
}

func TestPlanAuto(t *testing.T) {
	tests := []struct {
		name string
		pref gpu.Vendor
		want string
	}{
		// 这是这次改动的核心:显示器挂在谁身上,谁的编码器就排第一。
		{"显示器在 NVIDIA 上", gpu.VendorNVIDIA, "nvenc,qsv,amf,x264"},
		{"显示器在 Intel 核显上", gpu.VendorIntel, "qsv,nvenc,amf,x264"},
		{"显示器在 AMD 卡上", gpu.VendorAMD, "amf,nvenc,qsv,x264"},
		// 认不出来就保持原顺序,至少还能选到能用的那个
		{"厂商未知", gpu.VendorUnknown, "nvenc,qsv,amf,x264"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kinds(Plan("", allAvailable(), tt.pref))
			if got != tt.want {
				t.Errorf("得到 %s,期望 %s", got, tt.want)
			}
		})
	}
}

// 只有 A 卡的机器:「自动」绝不能选中 nvenc。这是这次要修的那个 bug。
func TestPlanAutoOnAMDOnlyMachine(t *testing.T) {
	// ProbeUsable 会把跑不起来的 nvenc/qsv 剔掉,但就算它们漏网了,
	// 排序也不该把它们放在 amf 前面(显示器在 A 卡上)。
	amdOnly := []Encoder{
		{Name: "h264_amf", Kind: "amf", Hardware: true},
		{Name: "libx264", Kind: "x264", Hardware: false},
	}
	got := Plan("", amdOnly, gpu.VendorAMD)
	if len(got) == 0 || got[0].Kind != "amf" {
		t.Fatalf("A 卡机器上应当选中 amf,实际得到 %s", kinds(got))
	}
	if got[len(got)-1].Kind != "x264" {
		t.Errorf("软件编码器应当排最后,实际得到 %s", kinds(got))
	}
}

// 只剩软件编码器时不能出错。
func TestPlanOnlySoftware(t *testing.T) {
	only := []Encoder{{Name: "libx264", Kind: "x264", Hardware: false}}
	for _, pref := range []gpu.Vendor{gpu.VendorNVIDIA, gpu.VendorIntel, gpu.VendorAMD, gpu.VendorUnknown} {
		got := Plan("", only, pref)
		if len(got) != 1 || got[0].Kind != "x264" {
			t.Errorf("厂商 %v:得到 %s,期望 x264", pref, kinds(got))
		}
	}
}

// 用户显式指定了一个能用的编码器 → 它排第一,其余顺延。
func TestPlanExplicit(t *testing.T) {
	// 显示器在 NVIDIA 上,但用户偏要用 qsv
	got := Plan("qsv", allAvailable(), gpu.VendorNVIDIA)
	if len(got) == 0 || got[0].Kind != "qsv" {
		t.Fatalf("用户指定的 qsv 应当排第一,实际得到 %s", kinds(got))
	}
	// 其余保持厂商排序
	if want := "qsv,nvenc,amf,x264"; kinds(got) != want {
		t.Errorf("得到 %s,期望 %s", kinds(got), want)
	}
}

// 用户指定了一个"本机没有"的编码器 → 不报错,退回自动顺序。
//
// 这是相对以前的行为变化:以前会返回「编码器 %q 在本机不可用」,把用户
// 卡在一个他不知道该选什么的状态里。
func TestPlanExplicitMissing(t *testing.T) {
	// 这台机器探测下来只有 amf 和 x264
	available := []Encoder{
		{Name: "h264_amf", Kind: "amf", Hardware: true},
		{Name: "libx264", Kind: "x264", Hardware: false},
	}
	got := Plan("nvenc", available, gpu.VendorAMD)
	if len(got) != 2 {
		t.Fatalf("不该报错也不该丢候选,实际得到 %s", kinds(got))
	}
	if got[0].Kind != "amf" {
		t.Errorf("应当退回自动顺序(amf 在前),实际得到 %s", kinds(got))
	}
}

func TestPlanEmpty(t *testing.T) {
	if got := Plan("", nil, gpu.VendorNVIDIA); got != nil {
		t.Errorf("没有可用编码器时应当返回 nil,得到 %v", got)
	}
	if got := Plan("nvenc", []Encoder{}, gpu.VendorNVIDIA); got != nil {
		t.Errorf("没有可用编码器时应当返回 nil,得到 %v", got)
	}
}

// 用的是真实跑出来的 AMF 报错文本。
func TestEncoderUnavailable(t *testing.T) {
	positive := []string{
		"[AMF @ 000001e232e88e40] DLL amfrt64.dll failed to open",
		"Cannot load nvcuda.dll",
		"[h264_nvenc @ ...] Error while opening encoder - maybe incorrect parameters such as bit_rate, rate, width or height",
		"Unknown encoder 'h264_amf'",
		"[h264_qsv @ ...] Error initializing output stream 0:0",
		"Device creation failed: -1313558101.",
		"No capable devices found",
	}
	for _, s := range positive {
		if !EncoderUnavailable(s) {
			t.Errorf("应当判成编码器不可用: %q", s)
		}
	}

	// 反例:这些是别的问题,不该因此换编码器(换了也一样)
	negative := []string{
		"",
		"rtsp://127.0.0.1:8554/live: Connection refused",
		"Only one usage of each socket address is normally permitted.",
		"Impossible to convert between the formats supported by the filter",
	}
	for _, s := range negative {
		if EncoderUnavailable(s) {
			t.Errorf("不该判成编码器不可用: %q", s)
		}
	}
}
