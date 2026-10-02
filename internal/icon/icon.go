// Package icon 在代码里画出程序图标。
//
// 图形定义只有这一份,同时供给两个地方:托盘图标(exe 运行时)和 exe 自身的
// 资源(编译时由 cmd/genicon 生成 rsrc.syso)。放在一起是为了让两边永远一致
// —— 分成一个 .png 和一个 .ico 迟早会改了一个忘了另一个。
//
// 只依赖标准库,不引入图形库。
package icon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// 与界面强调色一致(控制页的 --accent)
var (
	colorBG = color.RGBA{0x25, 0x63, 0xEB, 0xFF}
	colorFG = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
)

// Sizes 是打包进 exe 的图标尺寸。
//
// 16/20/24 对应任务栏和小图标视图,32/48 对应资源管理器和 Alt+Tab,
// 128/256 对应"大图标""超大图标"视图,以及高分屏上的 Alt+Tab 缩略图。
var Sizes = []int{16, 20, 24, 32, 48, 64, 128, 256}

// super 是超采样倍数。先按这个倍数画再缩回去,用平均代替抗锯齿。
const super = 4

// Render 画出指定边长的图标。
func Render(size int) *image.RGBA {
	big := size * super
	canvas := image.NewRGBA(image.Rect(0, 0, big, big))
	draw(canvas, float64(big))
	return downscale(canvas, size)
}

// PNG 返回指定边长图标的 PNG 编码,供 ICO 内嵌使用。
func PNG(size int) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, Render(size)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// draw 在一块边长 s 的画布上作画。所有尺寸都按 s 的比例算,
// 这样同一个图形在任何尺寸下比例都一致。
func draw(dst *image.RGBA, s float64) {
	// 圆角底。半径取 22% 是 Windows 应用图标的常见比例,
	// 比正圆的方、比直角柔和。
	fillRoundRect(dst, 0, 0, s, s, 0.22*s, colorBG)

	// 16 / 20 px 用一套单独的几何。
	//
	// 照搬大尺寸的比例在这两个尺寸下会糊成一团:支架只占 9% 宽(16px 下
	// 1.4 像素)、底座只有 6.5% 高(1 像素),全都被抗锯齿抹平,最后剩下
	// 一个认不出是什么的白块。所以这里把屏幕撑得更满、支架和底座都加厚,
	// 让每个部件至少占到两个像素。
	if s < 24*super {
		fillRoundRect(dst, 0.155*s, 0.170*s, 0.845*s, 0.605*s, 0.075*s, colorFG)
		fillRect(dst, 0.425*s, 0.605*s, 0.575*s, 0.715*s, colorFG)
		fillRoundRect(dst, 0.255*s, 0.715*s, 0.745*s, 0.830*s, 0.040*s, colorFG)
		return
	}

	// 显示器:屏幕 + 支架 + 底座
	fillRoundRect(dst, 0.200*s, 0.220*s, 0.800*s, 0.600*s, 0.055*s, colorFG)
	fillRect(dst, 0.455*s, 0.600*s, 0.545*s, 0.710*s, colorFG)
	fillRoundRect(dst, 0.310*s, 0.710*s, 0.690*s, 0.775*s, 0.030*s, colorFG)

	// 投射弧线只在够大的尺寸上画。48px 时最外层弧的线宽还不到一个像素,
	// 画上去只会让屏幕区域变脏,反而更难认。
	if s >= 64*super {
		drawCastArcs(dst, s)
	}
}

// drawCastArcs 在屏幕左下角画三道向外扩散的弧线,表示画面正在投出去。
//
// 用背景色画在白屏之上,等于从屏幕里挖掉三圈 —— 弧线自然被限制在屏幕内,
// 不需要额外做裁剪:跑到屏幕外的部分画在同样是背景色的底上,看不见。
func drawCastArcs(dst *image.RGBA, s float64) {
	cx, cy := 0.262*s, 0.545*s
	for _, r := range []float64{0.070, 0.115, 0.160} {
		strokeArc(dst, cx, cy, r*s, 0.018*s, -math.Pi/2, 0)
	}
}

// ── 图元 ──────────────────────────────────────────────────────
//
// 这些函数都直接按像素判断"在不在形状里",不做几何裁剪。
// 因为整块画布只画这么几个图形,而且是超采样后缩小的,
// 靠这种方式换来的代码简单远比省那点循环值得。

func fillRect(dst *image.RGBA, x0, y0, x1, y1 float64, c color.RGBA) {
	for y := int(y0); y < int(y1+0.5); y++ {
		for x := int(x0); x < int(x1+0.5); x++ {
			dst.SetRGBA(x, y, c)
		}
	}
}

// fillRoundRect 填充圆角矩形。圆角用"到圆心的距离"判断,
// 不做抗锯齿 —— 边缘的平滑交给 Render 里的超采样。
func fillRoundRect(dst *image.RGBA, x0, y0, x1, y1, r float64, c color.RGBA) {
	if r <= 0 {
		fillRect(dst, x0, y0, x1, y1, c)
		return
	}
	// 左上、右上、右下、左下四个圆心
	cx := [4]float64{x0 + r, x1 - r, x1 - r, x0 + r}
	cy := [4]float64{y0 + r, y0 + r, y1 - r, y1 - r}

	for y := int(y0); y < int(y1+0.5); y++ {
		for x := int(x0); x < int(x1+0.5); x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			if px >= x0+r && px <= x1-r || py >= y0+r && py <= y1-r {
				dst.SetRGBA(x, y, c)
				continue
			}
			// 落在四角区域内:看它离哪个圆心最近
			for i := 0; i < 4; i++ {
				dx, dy := px-cx[i], py-cy[i]
				if dx*dx+dy*dy <= r*r {
					dst.SetRGBA(x, y, c)
					break
				}
			}
		}
	}
}

// strokeArc 画一段环带(圆弧)。角度按图像坐标:0 指向右,x 轴顺时针为正
// (也就是 y 向下为正),和 math.Atan2(dy, dx) 一致。
func strokeArc(dst *image.RGBA, cx, cy, r, halfWidth, a0, a1 float64) {
	inner := r - halfWidth
	outer := r + halfWidth

	x0, x1 := int(cx-outer-1), int(cx+outer+1)
	y0, y1 := int(cy-outer-1), int(cy+outer+1)
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > dst.Rect.Dx() {
		x1 = dst.Rect.Dx()
	}
	if y1 > dst.Rect.Dy() {
		y1 = dst.Rect.Dy()
	}

	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			d := math.Hypot(dx, dy)
			if d < inner || d > outer {
				continue
			}
			if a := math.Atan2(dy, dx); a >= a0 && a <= a1 {
				dst.SetRGBA(x, y, colorBG)
			}
		}
	}
}

// downscale 用盒式平均把画布缩到 target×target 边长。
//
// 这一步就是抗锯齿的全部来源:超采样画出来的硬边缘,平均之后
// 在半透明和不透明之间得到过渡。
func downscale(src *image.RGBA, target int) *image.RGBA {
	const f = super
	dst := image.NewRGBA(image.Rect(0, 0, target, target))
	n := uint32(f * f)

	for y := 0; y < target; y++ {
		for x := 0; x < target; x++ {
			var sr, sg, sb, sa uint32
			for dy := 0; dy < f; dy++ {
				for dx := 0; dx < f; dx++ {
					c := src.RGBAAt(x*f+dx, y*f+dy)
					sr += uint32(c.R) * uint32(c.A) / 255
					sg += uint32(c.G) * uint32(c.A) / 255
					sb += uint32(c.B) * uint32(c.A) / 255
					sa += uint32(c.A)
				}
			}
			// 颜色按 alpha 加权平均,避免透明边缘出现暗边
			a := sa / n
			var r, g, b uint8
			if sa > 0 {
				r = uint8(sr * 255 / sa)
				g = uint8(sg * 255 / sa)
				b = uint8(sb * 255 / sa)
			}
			dst.SetRGBA(x, y, color.RGBA{r, g, b, uint8(a)})
		}
	}
	return dst
}

// imageData 返回某个尺寸在 ICO / 资源里的图像数据。
//
// 64 以下用 DIB(BMP),以上用 PNG。这不是为了省体积,是为了兼容:
// PNG 压缩的图标项 Windows 外壳能认,但很多老的 GDI/GDI+ 调用路径读不了
// (实测 .NET 的 System.Drawing.Icon 直接报"不是可用的图标")。
// 小尺寸的 DIB 本来也没多大 —— 16px 才 1KB 出头 —— 不值得为省这点去踩坑。
func imageData(size int) ([]byte, error) {
	if size >= 64 {
		return PNG(size)
	}
	return BMP(size)
}

// BMP 把图标编码成 ICO 内部的 DIB 格式。
//
// 布局是 BITMAPINFOHEADER + XOR 位图 + AND 掩码。有两个容易写错的点:
// 高度要写实际高度的两倍(XOR 和 AND 上下叠放),以及像素是自下而上、
// BGRA 顺序存的。
func BMP(size int) ([]byte, error) {
	img := Render(size)

	var b bytes.Buffer
	// BITMAPINFOHEADER
	_ = binary.Write(&b, binary.LittleEndian, uint32(40))            // biSize
	_ = binary.Write(&b, binary.LittleEndian, int32(size))           // biWidth
	_ = binary.Write(&b, binary.LittleEndian, int32(size*2))         // biHeight = 2 倍
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))             // biPlanes
	_ = binary.Write(&b, binary.LittleEndian, uint16(32))            // biBitCount
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))             // biCompression = BI_RGB
	_ = binary.Write(&b, binary.LittleEndian, uint32(size*size*4))   // biSizeImage
	_ = binary.Write(&b, binary.LittleEndian, int32(0))              // 水平分辨率
	_ = binary.Write(&b, binary.LittleEndian, int32(0))              // 垂直分辨率
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))             // 调色板颜色数
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))             // 重要颜色数

	// XOR 位图:自下而上,BGRA
	for y := size - 1; y >= 0; y-- {
		for x := 0; x < size; x++ {
			c := img.RGBAAt(x, y)
			b.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}

	// AND 掩码:32 位带 alpha 时它的内容会被忽略,但结构必须存在。
	// 每行按 4 字节对齐。
	rowBytes := ((size + 31) / 32) * 4
	for y := 0; y < size; y++ {
		b.Write(make([]byte, rowBytes))
	}
	return b.Bytes(), nil
}

// ICO 把若干尺寸打包成一个 ICO 文件。
func ICO(sizes []int) ([]byte, error) {
	type entry struct {
		size int
		data []byte
	}
	entries := make([]entry, 0, len(sizes))
	for _, s := range sizes {
		data, err := imageData(s)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{s, data})
	}

	var b bytes.Buffer
	// ICONDIR:reserved / type=1(图标)/ 图像数量
	_ = binary.Write(&b, binary.LittleEndian, uint16(0))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(len(entries)))

	// 图像数据紧跟在目录之后,所以偏移要先算出来
	offset := 6 + 16*len(entries)
	for _, e := range entries {
		dim := byte(e.size)
		if e.size >= 256 {
			dim = 0 // 256 在 ICONDIRENTRY 里用 0 表示
		}
		b.WriteByte(dim) // 宽
		b.WriteByte(dim) // 高
		b.WriteByte(0)   // 调色板颜色数(真彩色不用)
		b.WriteByte(0)   // reserved
		_ = binary.Write(&b, binary.LittleEndian, uint16(1))           // 颜色平面
		_ = binary.Write(&b, binary.LittleEndian, uint16(32))          // 位深
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(e.data))) // 数据长度
		_ = binary.Write(&b, binary.LittleEndian, uint32(offset))      // 数据偏移
		offset += len(e.data)
	}
	for _, e := range entries {
		b.Write(e.data)
	}
	return b.Bytes(), nil
}
