package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// 图标在代码里画出来,而不是放一个外部资源文件。
//
// 这样二进制保持自包含,也省掉了 Windows 上常见的 .syso 资源文件和
// 随之而来的额外构建步骤(要先跑一遍 rsrc 工具生成它)。

const iconSize = 32

// 与界面的强调色一致
var (
	iconAccent = color.RGBA{0x25, 0x63, 0xeb, 0xff} // --accent
	iconScreen = color.RGBA{0xff, 0xff, 0xff, 0xff}
)

// iconICO 生成托盘图标的 ICO 数据。
//
// Windows Vista 起支持在 ICO 里直接嵌入 PNG,所以只要把 PNG 包一层
// ICONDIR 头就行,不必手写 BMP 像素数据和 1 位掩码 —— 那部分很容易写错
// 且没有任何收益。
func iconICO() []byte {
	data := renderPNG()

	var b bytes.Buffer
	// ICONDIR
	_ = binary.Write(&b, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&b, binary.LittleEndian, uint16(1)) // type: 1 = icon
	_ = binary.Write(&b, binary.LittleEndian, uint16(1)) // count

	// ICONDIRENTRY
	b.WriteByte(iconSize) // 宽(32 直接写 32)
	b.WriteByte(iconSize) // 高
	b.WriteByte(0)        // 调色板颜色数(32 位图不用)
	b.WriteByte(0)        // reserved
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))        // color planes
	_ = binary.Write(&b, binary.LittleEndian, uint16(32))       // bits per pixel
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data))) // 图像数据长度

	const headerLen = 6 + 16
	_ = binary.Write(&b, binary.LittleEndian, uint32(headerLen)) // 图像数据偏移

	b.Write(data)
	return b.Bytes()
}

// renderPNG 画一个"显示器"图形:蓝色圆角底 + 白色屏幕 + 支架。
func renderPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))

	// 圆角底
	fillRounded(img, 1, 1, iconSize-1, iconSize-1, 6, iconAccent)

	// 屏幕
	sx0, sy0 := iconSize*20/100, iconSize*24/100
	sx1, sy1 := iconSize-iconSize*20/100, iconSize*62/100
	fillRect(img, sx0, sy0, sx1, sy1, iconScreen)

	// 支架
	fillRect(img, iconSize*45/100, sy1, iconSize*55/100, iconSize*76/100, iconScreen)
	// 底座
	fillRect(img, iconSize*30/100, iconSize*76/100, iconSize*70/100, iconSize*83/100, iconScreen)

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

// fillRounded 填充一个圆角矩形。四角用"到圆心的距离"判断,
// 不做抗锯齿 —— 托盘图标实际显示尺寸是 16px,采样后看不出差别。
func fillRounded(img *image.RGBA, x0, y0, x1, y1, r int, c color.RGBA) {
	cx0, cy0 := x0+r, y0+r // 左上角圆心
	cx1, cy1 := x1-r, y1-r // 右下角圆心
	r2 := r * r

	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			var dx, dy int
			switch {
			case x < cx0 && y < cy0:
				dx, dy = cx0-x, cy0-y
			case x >= cx1 && y < cy0:
				dx, dy = x-cx1, cy0-y
			case x < cx0 && y >= cy1:
				dx, dy = cx0-x, y-cy1
			case x >= cx1 && y >= cy1:
				dx, dy = x-cx1, y-cy1
			default:
				img.SetRGBA(x, y, c)
				continue
			}
			if dx*dx+dy*dy <= r2 {
				img.SetRGBA(x, y, c)
			}
		}
	}
}
