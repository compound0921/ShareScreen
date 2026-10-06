//go:build windows

package audio

import (
	"context"
	"fmt"
	"io"
	"log"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ── COM 基础设施 ──────────────────────────────────────────────
//
// 这里直接按 vtable 下标调 COM 接口,不引入 CGO —— 整个项目没有 CGO,
// 加进来会让构建复杂很多。代价是方法下标必须手写且编译器不会校验:
// 下标错了不会报错,只会变成奇怪的行为。所以下面每个接口都注释了
// 完整的方法顺序,改的时候对着数。

// guid 对应 Win32 的 GUID 布局。
type guid struct {
	data1 uint32
	data2 uint16
	data3 uint16
	data4 [8]byte
}

// propertyKey 对应 Win32 的 PROPERTYKEY:GUID + 属性号。
//
// 布局正好是 16 + 4 = 20 字节,和 C 里一致 —— 字段都是对齐的,
// 不会像 WAVEFORMATEX 那样被 Go 补出多余的字节(见下面的偏移注释)。
type propertyKey struct {
	fmtid guid
	pid   uint32
}

var (
	// CLSID_MMDeviceEnumerator —— 音频设备枚举器的入口。
	clsidMMDeviceEnumerator = guid{0xBCDE0395, 0xE52F, 0x467C, [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidMMDeviceEnumerator   = guid{0xA95664D2, 0x9614, 0x4F35, [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidMMDeviceCollection   = guid{0x0BD7A1BE, 0x7A1A, 0x44DB, [8]byte{0x83, 0x97, 0xCC, 0x53, 0x92, 0x38, 0x7B, 0x5E}}
	iidMMDevice             = guid{0xD666063F, 0x1587, 0x4E43, [8]byte{0x81, 0xF1, 0xB9, 0x48, 0xE8, 0x07, 0x36, 0x3F}}
	iidAudioClient          = guid{0x1CB9AD4C, 0xDBFA, 0x4C32, [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidAudioCaptureClient   = guid{0xC8ADBD64, 0xE71E, 0x48A0, [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
	iidPropertyStore        = guid{0x886D8EEB, 0x8CF2, 0x4446, [8]byte{0x8D, 0x02, 0xCD, 0xBA, 0x1D, 0xBD, 0xCF, 0x99}}
	// IID_IAudioMeterInformation —— 端点的音量表,用来问"这台此刻在放多响"。
	iidAudioMeterInformation = guid{0xC02216F6, 0x8C67, 0x4B5B, [8]byte{0x9D, 0x00, 0xD0, 0x08, 0xE7, 0x3E, 0x00, 0x64}}

	// PKEY_Device_FriendlyName —— 端点属性里那个给人看的名字,
	// 形如 "扬声器 (Realtek(R) Audio)"。
	pkeyDeviceFriendlyName = propertyKey{
		fmtid: guid{0xA45C254E, 0xDF1C, 0x4EFD, [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}},
		pid:   14,
	}

	// WAVEFORMATEXTENSIBLE 里用来区分整数 PCM 和浮点的子格式。
	subFormatPCM       = guid{0x00000001, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	subFormatIEEEFloat = guid{0x00000003, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
)

const (
	clsctxAll = 0x17

	// IMMDeviceEnumerator::GetDefaultAudioEndpoint 的两个枚举值:
	// eRender(播放设备)和 eConsole(控制台角色,即"默认设备")。
	endpointRender  = 0
	endpointConsole = 0

	// EnumAudioEndpoints 的状态掩码:只要当前插着、能用的设备。
	deviceStateActive = 0x00000001

	// IPropertyStore 的打开模式。只读就够。
	stgmRead = 0

	// PROPVARIANT 的类型码。这个属性只可能是字符串。
	vtLPWSTR = 31

	// IAudioClient::Initialize 的共享模式。回环采集只能用共享模式。
	shareModeShared = 0

	// 回环标志。加上它,采集端拿到的是渲染设备正在播放的内容。
	streamFlagsLoopback = 0x00020000

	// 静音数据包:缓冲区里没有有效数据,但时间轴要照常推进。
	bufferFlagSilent = 0x00000002

	coinitMultithreaded = 0x0
)

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
	// PropVariantClear 负责释放 PROPVARIANT 里的字符串 —— 那是设备属性
	// 系统按 COM 的规矩分配的内存,不释放就是每次枚举泄漏一点。
	procPropVariantClear = ole32.NewProc("PropVariantClear")
)

// hresultError 把 HRESULT 转成带解释的 error。
// 这几个码是实际会撞上的,给出中文解释比让用户去查十六进制强。
func hresultError(what string, hr int32) error {
	code := uint32(hr)
	msg := ""
	switch code {
	case 0x80070490:
		msg = "找不到设备(音频服务没在运行?)"
	case 0x80070005:
		msg = "访问被拒绝(系统隐私设置里禁用了音频?)"
	case 0x88890001:
		msg = "音频客户端尚未初始化"
	case 0x88890003:
		msg = "端点类型不匹配"
	case 0x88890004:
		msg = "播放设备已失效(默认设备被切走了?)"
	case 0x88890008:
		msg = "设备不支持该音频格式"
	case 0x88890010:
		msg = "音频服务未运行"
	}
	if msg == "" {
		return fmt.Errorf("%s 失败: HRESULT %#08x", what, code)
	}
	return fmt.Errorf("%s 失败: %s (HRESULT %#08x)", what, msg, code)
}

// comCallRaw 按 vtable 下标调用 COM 方法,原样返回 HRESULT。
//
// vtable 布局:对象头是指向函数指针数组的指针,第 idx 个方法就在
// vtable + idx*sizeof(uintptr) 处。IUnknown 的 QueryInterface / AddRef /
// Release 固定占 0 / 1 / 2,具体接口的方法从 3 开始。
//
// 接口指针全程以 unsafe.Pointer 传递,只在进 syscall 的那一刻转成 uintptr ——
// 反过来(uintptr 再转回 unsafe.Pointer)会让 GC 有机会在两者之间移动对象,
// go vet 也会直接报 possible misuse of unsafe.Pointer。
func comCallRaw(this unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(this)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(idx)*unsafe.Sizeof(uintptr(0))))

	callArgs := make([]uintptr, 0, len(args)+1)
	callArgs = append(callArgs, uintptr(this))
	callArgs = append(callArgs, args...)

	r, _, _ := syscall.SyscallN(fn, callArgs...)
	return r
}

// comCall 调用返回 HRESULT 的 COM 方法,失败时转成 error。
func comCall(this unsafe.Pointer, idx int, args ...uintptr) error {
	if hr := int32(comCallRaw(this, idx, args...)); hr < 0 {
		return hresultError("COM 调用", hr)
	}
	return nil
}

// release 调用 IUnknown::Release(下标 2)。
// 它返回的是引用计数而不是 HRESULT,所以不能走 comCall。
func release(p unsafe.Pointer) {
	if p != nil {
		comCallRaw(p, 2)
	}
}

// coInit 声明当前线程使用 MTA(多线程公寓)。
//
// COM 要求每个线程在调用任何 COM 接口前先声明公寓模型,而且不能假定
// 上一次调用过 —— Go 的 goroutine 会在 OS 线程之间迁移。所以凡是直接
// 碰 COM 的函数,入口都要先调它,并配合 runtime.LockOSThread。
func coInit() error {
	r, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	if hr := int32(r); hr < 0 {
		return hresultError("CoInitializeEx", hr)
	}
	return nil
}

// coCreateInstance 是 COM 的对象工厂。它是普通函数,不是接口方法。
func coCreateInstance(clsid, iid *guid, out *unsafe.Pointer) error {
	r, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(clsid)),
		0, // pUnkOuter,聚合用,这里不需要
		clsctxAll,
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(out)))
	if hr := int32(r); hr < 0 {
		return hresultError("CoCreateInstance", hr)
	}
	return nil
}

// ── 音频格式 ──────────────────────────────────────────────────

const (
	waveFormatPCM       = 1
	waveFormatIEEEFloat = 3
	// WAVE_FORMAT_EXTENSIBLE —— 格式描述后面还跟着子格式 GUID。
	waveFormatExtensibleTag = 0xFFFE
)

// WAVEFORMATEX / WAVEFORMATEXTENSIBLE 的字段偏移。
//
// 这里刻意不用 Go 结构体去映射,而是按字节偏移读 —— 因为 Win32 的
// WAVEFORMATEX 是紧凑的 18 字节,而 Go 会把结构体尺寸补齐到对齐边界
// (4 的倍数,即 20 字节)。把它嵌进扩展结构后,子格式 GUID 会落在偏移 28,
// 而 C 里是 24。差这 4 个字节的后果是:子格式永远匹配不上,格式识别直接失败。
const (
	offFormatTag     = 0  // uint16
	offChannels      = 2  // uint16
	offSamplesPerSec = 4  // uint32
	offBlockAlign    = 12 // uint16
	offBitsPerSample = 14 // uint16
	// 下面这个只在 wFormatTag == WAVE_FORMAT_EXTENSIBLE 时有效。
	// 布局是:18 字节的 WAVEFORMATEX + 2 字节 wValidBitsPerSample +
	// 4 字节 dwChannelMask,所以 GUID 从 24 开始。
	offSubFormat = 24
)

func readU16(p unsafe.Pointer, off uintptr) uint16 {
	return *(*uint16)(unsafe.Add(p, off))
}

func readU32(p unsafe.Pointer, off uintptr) uint32 {
	return *(*uint32)(unsafe.Add(p, off))
}

// parseFormat 从 GetMixFormat 返回的结构里提取我们要的信息。
//
// 只支持实际会遇到的几种组合。采样格式直接透传 —— 混音格式的原始字节就是
// ffmpeg 要的格式,在 Go 这边转换一遍纯属浪费,还会损失精度。
func parseFormat(p unsafe.Pointer) (Format, error) {
	tag := readU16(p, offFormatTag)
	bits := readU16(p, offBitsPerSample)

	// 扩展格式把真正的类型放在子格式 GUID 里,外面那层 tag 只是个壳。
	if tag == waveFormatExtensibleTag {
		switch *(*guid)(unsafe.Add(p, offSubFormat)) {
		case subFormatPCM:
			tag = waveFormatPCM
		case subFormatIEEEFloat:
			tag = waveFormatIEEEFloat
		}
	}

	f := Format{
		SampleRate: int(readU32(p, offSamplesPerSec)),
		Channels:   int(readU16(p, offChannels)),
		BlockAlign: int(readU16(p, offBlockAlign)),
	}
	if f.SampleRate <= 0 || f.Channels <= 0 || f.BlockAlign <= 0 {
		return f, fmt.Errorf("音频格式异常:%d Hz / %d 声道 / 每帧 %d 字节",
			f.SampleRate, f.Channels, f.BlockAlign)
	}

	switch {
	case tag == waveFormatIEEEFloat && bits == 32:
		f.SampleFmt = "f32le"
	case tag == waveFormatPCM && bits == 16:
		f.SampleFmt = "s16le"
	case tag == waveFormatPCM && bits == 32:
		f.SampleFmt = "s32le"
	default:
		return f, fmt.Errorf("不支持的音频格式:tag=%#x、%d 位", tag, bits)
	}
	return f, nil
}

// ── 设备枚举 ──────────────────────────────────────────────────

// ListDevices 返回当前所有可用的播放设备,系统默认的那一台带标记。
//
// 界面上那个下拉框用它。枚举是毫秒级的,按需调用即可。
func ListDevices() ([]Device, error) {
	out := []Device{}
	err := eachDevice(func(_ unsafe.Pointer, id, name string, isDefault bool) {
		out = append(out, Device{ID: id, Name: name, Default: isDefault})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Levels 返回每台可用播放设备此刻的峰值电平。
//
// "别处在放声音"就是靠它判断的。和 ListDevices 走同一趟枚举 —— 两次
// 分开枚举会拿到两份可能对不上的名单(中途插拔了设备),而这里的两份
// 信息必须来自同一时刻。
//
// 空列表而不是 nil:调用方(和界面)按"没有候选"处理,不该再判一次 nil。
func Levels() ([]Level, error) {
	out := []Level{}
	err := eachDevice(func(dev unsafe.Pointer, id, name string, isDefault bool) {
		out = append(out, Level{ID: id, Name: name, Default: isDefault, Peak: devicePeak(dev)})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// eachDevice 枚举所有当前可用的播放设备,对每一台调一次 fn。
//
// fn 拿到的是 IMMDevice 接口指针,**只在这次调用期间有效** —— 返回之后
// 就会被释放。想多要一个属性(名字、音量表)就在 fn 里自己取。
//
// 单台设备取不到(拔了、状态刚好变了)就跳过,不连累整个列表:能列出
// 剩下的,也好过一个空的下拉框。
func eachDevice(fn func(dev unsafe.Pointer, id, name string, isDefault bool)) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 每次调用都重新声明公寓并重新创建枚举器。调用方可能是一根没有
	// 锁定线程的轮询协程,它两次调用会落在不同的 OS 线程上 —— 缓存
	// COM 指针跨调用用,等于赌线程。

	if err := coInit(); err != nil {
		return err
	}

	var enum unsafe.Pointer
	if err := coCreateInstance(&clsidMMDeviceEnumerator, &iidMMDeviceEnumerator, &enum); err != nil {
		return fmt.Errorf("初始化音频设备枚举器失败: %w", err)
	}
	defer release(enum)

	// IMMDeviceCollection 方法顺序:
	// 0/1/2 IUnknown / 3 GetCount / 4 Item
	var coll unsafe.Pointer
	if err := comCall(enum, 3,
		endpointRender, deviceStateActive,
		uintptr(unsafe.Pointer(&coll))); err != nil {
		return fmt.Errorf("枚举播放设备失败: %w", err)
	}
	defer release(coll)

	var n uint32
	if err := comCall(coll, 3, uintptr(unsafe.Pointer(&n))); err != nil {
		return fmt.Errorf("读取播放设备数量失败: %w", err)
	}

	// 先记下系统默认设备的 ID,好在列表里标出来。
	//
	// 拿不到不算错 —— 那只是少一个标记,不该让整个下拉框空着。
	var def unsafe.Pointer
	defaultID := ""
	if err := comCall(enum, 4, endpointRender, endpointConsole,
		uintptr(unsafe.Pointer(&def))); err == nil && def != nil {
		defaultID = deviceID(def)
		release(def)
	}

	for i := uint32(0); i < n; i++ {
		var dev unsafe.Pointer
		if err := comCall(coll, 4, uintptr(i), uintptr(unsafe.Pointer(&dev))); err != nil {
			continue
		}
		id, name := deviceID(dev), deviceName(dev)
		if id == "" {
			release(dev)
			continue
		}
		if name == "" {
			name = "未命名设备"
		}
		fn(dev, id, name, id == defaultID)
		release(dev)
	}
	return nil
}

// meterWarnOnce 让"这台机器的音量表不可用"只记一条日志。
//
// 音量表是可选增强的探测手段,不支持它的驱动完全可能有 —— 每次轮询都
// 记一条会把日志刷满,而它每次说的都是同一件事。
var meterWarnOnce sync.Once

// devicePeak 读一台端点此刻的峰值电平。
//
// 拿不到就返回 0,**不报错**:0 的含义是"没在放东西",正好让这台设备
// 从候选里消失 —— 功能静默失效,而不是让采集出问题。
//
// IAudioMeterInformation 的方法顺序:
// 0/1/2 IUnknown / 3 GetPeakValue / 4 GetMeteringChannelCount /
// 5 GetChannelsPeakValues / 6 QueryHardwareSupport
//
// 它是引擎(audiodg)在软件里实现的,不要求我们在这台端点上持有流 ——
// 正是"有没有别的程序在往它上面放"这个问题要问的东西。
//
// 两个已知的偏差,都接受:
//   - 它量的是**端点音量之前**的信号,所以被静音的设备照样报活跃。
//     那种情况下建议切过去也听不到东西;
//   - 别的程序独占占用该设备时它是 0。但独占模式下回环采集本身也用不了,
//     这个功能碰不到那种情况。
func devicePeak(dev unsafe.Pointer) float32 {
	var meter unsafe.Pointer
	if err := comCall(dev, 3,
		uintptr(unsafe.Pointer(&iidAudioMeterInformation)),
		clsctxAll,
		0, // 激活参数,音量表不用
		uintptr(unsafe.Pointer(&meter))); err != nil {
		meterWarnOnce.Do(func() {
			log.Printf("音频:这台机器的音量表不可用,设备提示将静默失效: %v", err)
		})
		return 0
	}
	defer release(meter)

	var peak float32
	if err := comCall(meter, 3, uintptr(unsafe.Pointer(&peak))); err != nil {
		return 0
	}
	return peak
}

// deviceID 取端点的 ID 字符串(IMMDevice::GetId,下标 5)。
func deviceID(dev unsafe.Pointer) string {
	var p *uint16
	if err := comCall(dev, 5, uintptr(unsafe.Pointer(&p))); err != nil || p == nil {
		return ""
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	return windows.UTF16PtrToString(p)
}

// deviceName 取端点的人类可读名字。
//
// 走属性存储:IMMDevice::OpenPropertyStore(下标 4)→
// IPropertyStore::GetValue(下标 5)→ PKEY_Device_FriendlyName。
// 取不到就返回空串,调用方会退化成 ID —— 名字只是给人看的。
func deviceName(dev unsafe.Pointer) string {
	var store unsafe.Pointer
	if err := comCall(dev, 4, stgmRead, uintptr(unsafe.Pointer(&store))); err != nil {
		return ""
	}
	defer release(store)

	// PROPVARIANT 在 x64 上是 24 字节(vt + 3 个保留字 + 8 字节联合体 +
	// 8 字节指针),这里给到 32 字节留余量。
	//
	// 按偏移读而不是映射成 Go 结构体,理由和 WAVEFORMATEX 那边一样:
	// 联合体的对齐规则很容易差几个字节,而差几个字节不会报错,
	// 只会读出一个乱七八糟的指针。
	var pv [32]byte
	if err := comCall(store, 5,
		uintptr(unsafe.Pointer(&pkeyDeviceFriendlyName)),
		uintptr(unsafe.Pointer(&pv[0]))); err != nil {
		return ""
	}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv[0])))

	if *(*uint16)(unsafe.Pointer(&pv[0])) != vtLPWSTR {
		return ""
	}
	ptr := *(*unsafe.Pointer)(unsafe.Add(unsafe.Pointer(&pv[0]), 8))
	if ptr == nil {
		return ""
	}
	return windows.UTF16PtrToString((*uint16)(ptr))
}

// ── 采集会话 ──────────────────────────────────────────────────

// Capture 是一次桌面音频采集会话。
//
// 打开和取数据必须分两步:调用方要先拿到 Format 才能构造 ffmpeg 的参数,
// 而那要在启动 ffmpeg 之前。所以 Open 只把设备准备好并读出格式,
// Run 才真正开始出数据。
//
// 这两个方法可以跑在不同的线程上 —— COM 对象是在 MTA 里创建的,
// 任何 MTA 线程都能用。但各自都必须先 coInit。
type Capture struct {
	format  Format
	client  unsafe.Pointer // IAudioClient
	capture unsafe.Pointer // IAudioCaptureClient

	// 实际打开的是哪台设备。配置里指定的设备可能已经拔了 —— 那时这里
	// 和配置对不上,调用方据此给用户一条提示。
	deviceID   string
	deviceName string

	// audible 记着上一次真正收到非静音数据包的时刻。界面据此判断
	// "采的这台一直没声音"。
	audible audibility
}

// Open 打开默认播放设备的回环采集。
func Open() (*Capture, error) { return OpenDevice("") }

// OpenDevice 打开指定播放设备的回环采集;id 为空表示跟随系统默认设备。
//
// 指定的设备不存在时(拔了耳机、换了声卡)**回退到默认设备**而不是报错:
// 宁可出声而设备不是用户点的那台,也好过整个共享没有声音。调用方可以
// 用 DeviceID 和配置比对,把这次回退告诉用户。
//
// 返回时设备已初始化但还没有开始出数据,需要再调 Run。失败时返回的
// error 已经是给用户看的中文描述。
func OpenDevice(id string) (*Capture, error) {
	// 整个函数都在同一个 OS 线程上跑,因为线程一旦迁移,
	// 上面那次 coInit 就跟这个线程没关系了。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := coInit(); err != nil {
		return nil, err
	}

	var enum unsafe.Pointer
	if err := coCreateInstance(&clsidMMDeviceEnumerator, &iidMMDeviceEnumerator, &enum); err != nil {
		return nil, fmt.Errorf("初始化音频设备枚举器失败: %w", err)
	}
	defer release(enum)

	// IMMDeviceEnumerator 方法顺序:
	// 0 QueryInterface / 1 AddRef / 2 Release /
	// 3 EnumAudioEndpoints / 4 GetDefaultAudioEndpoint / 5 GetDevice /
	// 6 RegisterEndpointNotificationCallback / 7 Unregister...
	var dev unsafe.Pointer
	if id != "" {
		p, err := windows.UTF16PtrFromString(id)
		if err == nil {
			// GetDevice 失败(设备不在了)时 dev 保持为 nil,下面走默认设备。
			if err := comCall(enum, 5,
				uintptr(unsafe.Pointer(p)),
				uintptr(unsafe.Pointer(&dev))); err != nil {
				dev = nil
			}
			runtime.KeepAlive(p)
		}
	}
	if dev == nil {
		if err := comCall(enum, 4,
			endpointRender, endpointConsole,
			uintptr(unsafe.Pointer(&dev))); err != nil {
			return nil, fmt.Errorf("找不到默认播放设备: %w", err)
		}
	}
	defer release(dev)

	// IMMDevice 方法顺序:
	// 0/1/2 IUnknown / 3 Activate / 4 OpenPropertyStore / 5 GetId / 6 GetState
	var client unsafe.Pointer
	if err := comCall(dev, 3,
		uintptr(unsafe.Pointer(&iidAudioClient)),
		clsctxAll,
		0, // 激活参数,音频设备不用
		uintptr(unsafe.Pointer(&client))); err != nil {
		return nil, fmt.Errorf("打开音频客户端失败: %w", err)
	}

	// 记下实际打开的是哪台设备。取值失败不影响采集 —— 那只是让上面那条
	// "你选的设备不在了"的提示少一个名字。
	c := &Capture{
		client:     client,
		deviceID:   deviceID(dev),
		deviceName: deviceName(dev),
	}
	opened := false
	defer func() {
		if !opened {
			c.Close()
		}
	}()

	// IAudioClient 方法顺序:
	// 0/1/2 IUnknown / 3 Initialize / 4 GetBufferSize / 5 GetStreamLatency /
	// 6 GetCurrentPadding / 7 IsFormatSupported / 8 GetMixFormat /
	// 9 GetDevicePeriod / 10 Start / 11 Stop / 12 Reset / 13 SetEventHandle /
	// 14 GetService
	var mix unsafe.Pointer
	if err := comCall(client, 8, uintptr(unsafe.Pointer(&mix))); err != nil {
		return nil, fmt.Errorf("读取设备混音格式失败: %w", err)
	}
	// GetMixFormat 用 CoTaskMemAlloc 分配,必须配套 CoTaskMemFree。
	defer procCoTaskMemFree.Call(uintptr(mix))

	f, err := parseFormat(mix)
	if err != nil {
		return nil, err
	}
	c.format = f

	// 共享模式下格式由音频引擎决定,我们只能照抄 GetMixFormat 的结果 ——
	// 这也是为什么 Format 必须一路传到 ffmpeg 的参数里,不能写死。
	// 缓冲时长传 0 表示交给引擎决定;周期在共享模式下必须是 0。
	if err := comCall(client, 3,
		shareModeShared,
		streamFlagsLoopback,
		0,
		0,
		uintptr(mix), // pFormat
		0); err != nil {
		return nil, fmt.Errorf("初始化回环采集失败: %w", err)
	}

	var cap unsafe.Pointer
	if err := comCall(client, 14,
		uintptr(unsafe.Pointer(&iidAudioCaptureClient)),
		uintptr(unsafe.Pointer(&cap))); err != nil {
		return nil, fmt.Errorf("获取采集接口失败: %w", err)
	}
	c.capture = cap

	opened = true
	return c, nil
}

// Format 返回设备的原始 PCM 格式。必须在 Open 之后、构造 ffmpeg 参数之前读。
func (c *Capture) Format() Format { return c.format }

// DeviceID 返回实际打开的播放设备 ID。
func (c *Capture) DeviceID() string { return c.deviceID }

// DeviceName 返回实际打开的播放设备名;取不到名字时可能为空。
func (c *Capture) DeviceName() string { return c.deviceName }

// SilentFor 返回这台设备已经连续多久没出过声音了。
//
// 从 Run 开始计时:一次都还没响过的设备报的是"从开始采集到现在",
// 不是 0 —— 那正是最该被发现的情况。Run 还没跑起来时返回 0。
func (c *Capture) SilentFor(now time.Time) time.Duration {
	return c.audible.silentFor(now)
}

// Close 释放 COM 对象。可以在任意 MTA 线程上调用。
func (c *Capture) Close() {
	if c.capture != nil {
		release(c.capture)
		c.capture = nil
	}
	if c.client != nil {
		release(c.client)
		c.client = nil
	}
}

// Run 开始把桌面音频写进 w,直到 w 返回错误、ctx 被取消,或设备失效。
//
// 它会阻塞。调用方通常在单独的 goroutine 里跑它,停止时关闭 w 所在的连接
// 或取消 ctx 即可。
func (c *Capture) Run(ctx context.Context, w io.Writer) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := coInit(); err != nil {
		return err
	}

	// 静音计时从这一刻起算。一个声音都还没听到,所以不能叫 markAudible ——
	// 见 audibility.restart。
	c.audible.restart(time.Now())

	// IAudioClient::Start(10)
	if err := comCall(c.client, 10); err != nil {
		return fmt.Errorf("启动音频采集失败: %w", err)
	}
	defer comCall(c.client, 11) // Stop —— 退出时一定要停,否则设备一直占着

	const (
		// 静音补齐的粒度。20ms 正好是一个 Opus 帧,再小只是徒增系统调用。
		silenceStep = 20 * time.Millisecond
		// 单次补齐的上限。进程被挂起或长时间没被调度时,
		// 不要把这段时间一次性灌成静音。
		silenceMax = 200 * time.Millisecond
		// 没有真实数据时的轮询间隔。
		idleSleep = 5 * time.Millisecond
	)

	bytesPerSec := float64(c.format.SampleRate * c.format.BlockAlign)
	stepBytes := int64(float64(silenceStep) / float64(time.Second) * bytesPerSec)
	maxBytes := int64(float64(silenceMax) / float64(time.Second) * bytesPerSec)
	silence := make([]byte, maxBytes)

	start := time.Now()
	var produced int64

	// padSilence 把输出补齐到当前时刻。
	//
	// 这是回环采集绕不开的一环:桌面没有声音时,音频引擎根本不产生数据包,
	// GetNextPacketSize 会一直返回 0。而 ffmpeg 是按连续流处理的 —— 一个
	// 字节都收不到它就一直在那儿等,连输出都打不开(实测:静音时 ffmpeg
	// 卡在编码开始之前,画面也跟着出不来)。所以静音得自己补,模拟一个
	// 真实采集设备的连续输出。
	//
	// 补的是"已经过去、但还没输出"的那段时间。真实数据一到位,产出量立刻
	// 追上墙上时钟,缺口自然归零 —— 所以有声音时不会重复补。
	padSilence := func() error {
		want := int64(time.Since(start).Seconds() * bytesPerSec)
		gap := want - produced
		if gap < stepBytes {
			return nil
		}
		if gap > maxBytes {
			// 差得太远,说明进程被挂起过。重新对齐,别把这段时间补上,
			// 否则会在瞬间给出一大段静音,把音画同步彻底冲垮。
			start = time.Now()
			produced = 0
			return nil
		}
		if _, err := w.Write(silence[:gap]); err != nil {
			return err
		}
		produced += gap
		return nil
	}

	buf := make([]byte, 0, 1<<16)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := c.drain(&buf)
		if err != nil {
			return err
		}
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			produced += int64(n)
		}

		if err := padSilence(); err != nil {
			return err
		}

		// 没有真实数据时别空转。
		if n == 0 {
			time.Sleep(idleSleep)
		}
	}
}

// drain 取出当前所有已就绪的数据包,按顺序追加进 buf,返回写入的字节数。
func (c *Capture) drain(buf *[]byte) (int, error) {
	// IAudioCaptureClient 方法顺序:
	// 0/1/2 IUnknown / 3 GetBuffer / 4 ReleaseBuffer / 5 GetNextPacketSize
	written := 0
	for {
		var packetFrames uint32
		if err := comCall(c.capture, 5, uintptr(unsafe.Pointer(&packetFrames))); err != nil {
			return written, fmt.Errorf("读取音频包长度失败: %w", err)
		}
		if packetFrames == 0 {
			return written, nil
		}

		var (
			data   unsafe.Pointer
			frames uint32
			flags  uint32
		)
		if err := comCall(c.capture, 3,
			uintptr(unsafe.Pointer(&data)),
			uintptr(unsafe.Pointer(&frames)),
			uintptr(unsafe.Pointer(&flags)),
			0, // 设备位置,不需要
			0, // QPC 位置,不需要
		); err != nil {
			return written, fmt.Errorf("取音频数据失败: %w", err)
		}

		n := int(frames) * c.format.BlockAlign
		if n > 0 {
			if cap(*buf) < written+n {
				grown := make([]byte, written+n)
				copy(grown, (*buf)[:written])
				*buf = grown
			}
			*buf = (*buf)[:written+n]

			if flags&bufferFlagSilent != 0 {
				// 静音包没有有效数据,但必须照样输出等长的零 ——
				// 直接跳过会让观众那边的音频时间轴往前跳。
				for i := 0; i < n; i++ {
					(*buf)[written+i] = 0
				}
			} else {
				copy((*buf)[written:written+n],
					unsafe.Slice((*byte)(data), n))
				// 真的收到内容了。只有这一条路会推进静音计时 ——
				// "引擎没产生数据包"和"数据包带静音标志"都不算。
				c.audible.markAudible(time.Now())
			}
			written += n
		}

		// 无论取到多少帧都必须归还,否则引擎会一直重复给同一个包。
		if err := comCall(c.capture, 4, uintptr(frames)); err != nil {
			return written, fmt.Errorf("释放音频缓冲失败: %w", err)
		}
	}
}
