//go:build windows

package audio

import (
	"context"
	"fmt"
	"io"
	"runtime"
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

var (
	// CLSID_MMDeviceEnumerator —— 音频设备枚举器的入口。
	clsidMMDeviceEnumerator = guid{0xBCDE0395, 0xE52F, 0x467C, [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidMMDeviceEnumerator   = guid{0xA95664D2, 0x9614, 0x4F35, [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidMMDevice             = guid{0xD666063F, 0x1587, 0x4E43, [8]byte{0x81, 0xF1, 0xB9, 0x48, 0xE8, 0x07, 0x36, 0x3F}}
	iidAudioClient          = guid{0x1CB9AD4C, 0xDBFA, 0x4C32, [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidAudioCaptureClient   = guid{0xC8ADBD64, 0xE71E, 0x48A0, [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}

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
}

// Open 打开默认播放设备的回环采集。
//
// 返回时设备已初始化但还没有开始出数据,需要再调 Run。失败时返回的
// error 已经是给用户看的中文描述。
func Open() (*Capture, error) {
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
	if err := comCall(enum, 4,
		endpointRender, endpointConsole,
		uintptr(unsafe.Pointer(&dev))); err != nil {
		return nil, fmt.Errorf("找不到默认播放设备: %w", err)
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

	c := &Capture{client: client}
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

	// IAudioClient::Start(10)
	if err := comCall(c.client, 10); err != nil {
		return fmt.Errorf("启动音频采集失败: %w", err)
	}
	defer comCall(c.client, 11) // Stop —— 退出时一定要停,否则设备一直占着

	// 回环采集是"拉"模型:没有声音时引擎根本不产生数据包,所以取不到数据
	// 时要主动睡一会儿,不能空转。
	const idleSleep = 5 * time.Millisecond

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
		if n == 0 {
			time.Sleep(idleSleep)
			continue
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return err
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
			}
			written += n
		}

		// 无论取到多少帧都必须归还,否则引擎会一直重复给同一个包。
		if err := comCall(c.capture, 4, uintptr(frames)); err != nil {
			return written, fmt.Errorf("释放音频缓冲失败: %w", err)
		}
	}
}
