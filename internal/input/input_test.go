package input

import "testing"

func TestNormPixel(t *testing.T) {
	tests := []struct {
		name         string
		n            float64
		origin, size int
		want         int
	}{
		{"左上角就是原点", 0, 0, 1920, 0},
		{"右下角落在最后一个像素上", 1, 0, 1920, 1919},
		{"原点非零时跟着偏移", 0, -1920, 1920, -1920},
		{"原点非零时右端也正确", 1, -1920, 1920, -1},
		{"中间取整", 0.5, 0, 100, 50},
		{"负数夹到原点", -0.5, 0, 1920, 0},
		{"超过 1 夹到右下角", 1.5, 0, 1920, 1919},
		{"尺寸为 1 时只能是原点", 0.7, 300, 1, 300},
		{"尺寸非法时退回原点", 0.5, 300, 0, 300},
		{"NaN 当成 0", nan(), 0, 1920, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normPixel(tt.n, tt.origin, tt.size); got != tt.want {
				t.Errorf("normPixel(%v, %d, %d) = %d, 期望 %d",
					tt.n, tt.origin, tt.size, got, tt.want)
			}
		})
	}
}

func TestAbsCoord(t *testing.T) {
	tests := []struct {
		name                string
		pixel, origin, size int
		want                int32
	}{
		{"单屏左端是 0", 0, 0, 1920, 0},
		{"单屏右端是 65535", 1919, 0, 1920, 65535},
		{"虚拟桌面原点为负时左端仍是 0", -1920, -1920, 3840, 0},
		{"虚拟桌面右端仍是 65535", 1919, -1920, 3840, 65535},
		{"超出虚拟桌面会被夹住", 5000, 0, 1920, 65535},
		{"小于虚拟桌面原点会被夹住", -5000, 0, 1920, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := absCoord(tt.pixel, tt.origin, tt.size); got != tt.want {
				t.Errorf("absCoord(%d, %d, %d) = %d, 期望 %d",
					tt.pixel, tt.origin, tt.size, got, tt.want)
			}
		})
	}
}

// TestCoordRoundTrip 验证"归一化坐标 → 像素 → 绝对坐标"这条链路的两个
// 端点恰好铺满 0~65535。
//
// 端点是唯一有精确答案的位置,中间值靠整数除法,差一两个单位是正常的
// (换算本身就有取整)。端点错了就说明分母用错了 —— 用 size 当分母会让
// 画面最右一列永远点不到,这是远程控制里最典型的手感 bug。
func TestCoordRoundTrip(t *testing.T) {
	for _, size := range []int{1, 2, 100, 1280, 1920, 2560, 3840} {
		for _, origin := range []int{0, -1920, 100} {
			lo := absCoord(normPixel(0, origin, size), origin, size)
			hi := absCoord(normPixel(1, origin, size), origin, size)
			if lo != 0 {
				t.Errorf("size=%d origin=%d: 左端换算成 %d,期望 0", size, origin, lo)
			}
			if size > 1 && hi != 65535 {
				t.Errorf("size=%d origin=%d: 右端换算成 %d,期望 65535", size, origin, hi)
			}
		}
	}
}

// TestNormPixelCoversWholeRect 确认归一化坐标铺满整块矩形,一个像素不漏。
func TestNormPixelCoversWholeRect(t *testing.T) {
	const origin, size = 0, 100
	seen := map[int]bool{}
	// 采样要足够密,否则会漏掉边缘像素而误判
	for i := 0; i <= 100000; i++ {
		seen[normPixel(float64(i)/100000, origin, size)] = true
	}
	for p := origin; p < origin+size; p++ {
		if !seen[p] {
			t.Errorf("像素 %d 落不进任何归一化坐标", p)
		}
	}
}

func TestCodeToVK(t *testing.T) {
	tests := []struct {
		code    string
		wantVK  uint16
		wantExt bool
		wantOK  bool
	}{
		{"KeyA", 0x41, false, true},
		{"KeyZ", 0x5A, false, true},
		{"Digit0", 0x30, false, true},
		{"Digit9", 0x39, false, true},
		{"F1", 0x70, false, true},
		{"F12", 0x7B, false, true},
		{"F24", 0x87, false, true},
		{"Numpad0", 0x60, false, true},
		{"Numpad9", 0x69, false, true},

		{"Enter", 0x0D, false, true},
		{"NumpadEnter", 0x0D, true, true}, // 和小键盘回车必须区分开
		{"Escape", 0x1B, false, true},
		{"Backspace", 0x08, false, true},
		{"Delete", 0x2E, true, true},
		{"ArrowLeft", 0x25, true, true},
		{"Home", 0x24, true, true},
		{"ControlLeft", 0xA2, false, true},
		{"ControlRight", 0xA3, true, true},
		{"AltLeft", 0xA4, false, true},
		{"AltRight", 0xA5, true, true},
		{"ShiftLeft", 0xA0, false, true},
		{"MetaLeft", 0x5B, true, true},
		{"NumpadDivide", 0x6F, true, true},
		{"NumpadMultiply", 0x6A, false, true},
		{"Semicolon", 0xBA, false, true},
		{"Backslash", 0xDC, false, true},

		// 认不出来的一律返回 false,绝不猜一个键 —— 猜错会在主机上
		// 打出一个用户没按过的字符。
		{"Unidentified", 0, false, false},
		{"", 0, false, false},
		{"Keya", 0, false, false},
		{"Key1", 0, false, false},
		{"Digit", 0, false, false},
		{"F25", 0, false, false},
		{"F0", 0, false, false},
		{"Numpad10", 0, false, false},
		{"NumpadFoo", 0, false, false},
		{"VolumeUp", 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			vk, ext, ok := CodeToVK(tt.code)
			if ok != tt.wantOK {
				t.Fatalf("CodeToVK(%q) ok = %v, 期望 %v", tt.code, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if vk != tt.wantVK {
				t.Errorf("CodeToVK(%q) vk = 0x%02X, 期望 0x%02X", tt.code, vk, tt.wantVK)
			}
			if ext != tt.wantExt {
				t.Errorf("CodeToVK(%q) extended = %v, 期望 %v", tt.code, ext, tt.wantExt)
			}
		})
	}
}

// sameVK 是允许共用同一个虚拟键码的物理键对。
//
// 日文键盘上 ¥ 键就长在美式键盘反斜杠的位置上,两者本来就产生同一个
// 虚拟键码(VK_OEM_5),所以这一对撞车是正确的,不是笔误。
var sameVK = map[string]bool{
	"Backslash|IntlYen": true,
}

// TestCodeToVKDistinct 确认映射表里没有两个不同的物理键意外撞到同一个
// 虚拟键码上 —— 那种错通常来自抄写,而后果是某个键永远按不出来。
func TestCodeToVKDistinct(t *testing.T) {
	seen := map[string]string{}
	for code := range codeTable {
		vk, ext, ok := CodeToVK(code)
		if !ok {
			t.Fatalf("表里的 %q 查不出来", code)
		}
		key := string(rune(vk)) + "/" + boolStr(ext)
		if prev, dup := seen[key]; dup {
			if sameVK[prev+"|"+code] || sameVK[code+"|"+prev] {
				continue
			}
			t.Errorf("%q 和 %q 撞到了同一个键码 0x%02X(扩展标记 %v)", code, prev, vk, ext)
		}
		seen[key] = code
	}
}

func TestUnicodeUnits(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []uint16
	}{
		{"ASCII 一个码元", "a", []uint16{0x61}},
		{"汉字一个码元", "中", []uint16{0x4E2D}},
		{"空串", "", nil},
		// U+1F600 超出 BMP,必须拆成代理对,否则主机上会变成两个问号
		{"emoji 拆成代理对", "😀", []uint16{0xD83D, 0xDE00}},
		{"混合", "a中", []uint16{0x61, 0x4E2D}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unicodeUnits(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("unicodeUnits(%q) = %v, 期望 %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("unicodeUnits(%q)[%d] = 0x%04X, 期望 0x%04X",
						tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "ext"
	}
	return "plain"
}

func nan() float64 {
	var zero float64
	return zero / zero
}
