//go:build windows

package screen

import (
	"syscall"
	"unsafe"
)

// DEVMODEW。只列到 dmDisplayFrequency —— 后面那些字段本项目用不到,但要
// **占住位置**:dmSize 必须是整个结构的大小(220),否则 EnumDisplaySettings
// 会按老版本的尺寸截断,读出来的东西是错的(实测填错时枚举直接失败)。
//
// 偏移对照(Windows SDK 的 winspool.h):dmDeviceName 之后是 spec/driver/
// size/extra 四个 WORD、dmFields,然后是那段**联合体** —— 按打印机解释是
// dmOrientation 那一串,按显示器解释是 dmPosition(两个 int32)+
// dmDisplayOrientation。这里按显示器解释,因为要的就是它。
type devmodeW struct {
	DeviceName         [32]uint16
	SpecVersion        uint16
	DriverVersion      uint16
	Size               uint16
	DriverExtra        uint16
	Fields             uint32
	PositionX          int32
	PositionY          int32
	DisplayOrientation uint32
	DisplayFixedOutput uint32
	Color              int16
	Duplex             int16
	YResolution        int16
	TTOption           int16
	Collate            int16
	FormName           [32]uint16
	LogPixels          uint16
	BitsPerPel         uint32
	PelsWidth          uint32
	PelsHeight         uint32
	DisplayFlags       uint32
	DisplayFrequency   uint32
	ICMMethod          uint32
	ICMIntent          uint32
	MediaType          uint32
	DitherType         uint32
	Reserved1          uint32
	Reserved2          uint32
	PanningWidth       uint32
	PanningHeight      uint32
}

// DisplayRotation 返回主显示器被系统旋转了多少度(0/90/180/270)。
//
// 为什么需要它:DXGI 桌面复制(ffmpeg 的 ddagrab 走的就是它)给的是
// **未旋转的原始画面**,旋转是系统在扫描输出那一步做的。所以显示器设成
// 180° 时,采集出来的画面是倒的 —— 而 gdigrab 走 GDI,拿到的是已经转好的
// 画面,不用补。实测两者互为 180° 旋转:SSIM 0.9999。
//
// 只认主显示器:采集端起手就只面向主屏,而且多显示器各自方向不同时,
// "该按哪个转"这个问题本身就没有干净答案(见 ffmpeg.ViewerRotation)。
func DisplayRotation() int {
	user32 := syscall.NewLazyDLL("user32.dll")
	enum := user32.NewProc("EnumDisplaySettingsW")

	var dm devmodeW
	dm.Size = uint16(unsafe.Sizeof(dm))

	const enumCurrentSettings = ^uintptr(0) // -1
	if r, _, _ := enum.Call(0, enumCurrentSettings, uintptr(unsafe.Pointer(&dm))); r == 0 {
		return 0 // 问不到就当成没转 —— 转错了比不转更糟
	}

	// DMDO_DEFAULT/90/180/270 依次是 0/1/2/3
	switch dm.DisplayOrientation {
	case 1:
		return 90
	case 2:
		return 180
	case 3:
		return 270
	}
	return 0
}
