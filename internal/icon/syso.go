package icon

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// 这个文件手写 Windows 的 PE 资源段。
//
// 为什么不用 rsrc / go-winres 这类工具:它们要装一遍、拼一条命令、还得让
// 生成步骤留在构建流程里。而资源段的格式是固定的,一次写对以后就不用再管,
// 生成出来的 rsrc.syso 直接进仓库,`go build` 自动带上 —— 构建命令一个字
// 都不用改,也不需要任何额外依赖。
//
// 生成的 COFF 对象里只有一段 .rsrc。Go 链接器的处理方式是:把段数据原样
// 搬进 PE,并在每个重定位处写入
//
//	.rsrc 段的虚拟地址 + 重定位位置上的 4 字节值
//
// (见 cmd/link/internal/loadpe 里读加数的代码,以及 ld/pe.go 的 addpersrc。)
//
// 所以 PE 里那些必须是"映像内绝对地址"的字段 —— 资源目录的每个子目录指针、
// 以及数据入口的 OffsetToData —— 只要在重定位处写**段内偏移**就够了,
// 链接器会把段地址加上去。这就是下面 relocs 这个列表的含义。

const (
	rtIcon      = 3  // RT_ICON:图标本体
	rtGroupIcon = 14 // RT_GROUP_ICON:图标组,把若干尺寸归成一个图标

	langNeutral = 1033 // en-US,资源语言随便取一个即可

	dirHeaderSize  = 16 // IMAGE_RESOURCE_DIRECTORY
	dirEntrySize   = 8  // IMAGE_RESOURCE_DIRECTORY_ENTRY
	dataEntrySize  = 16 // IMAGE_RESOURCE_DATA_ENTRY
	coffHeaderSize = 20
	coffSectSize   = 40
	symEntrySize   = 18
	relocSize      = 10
)

// BuildSyso 生成一个可直接被 `go build` 链接的 .syso 对象文件。
func BuildSyso(sizes []int) ([]byte, error) {
	data, relocs, err := buildRsrcData(sizes)
	if err != nil {
		return nil, err
	}
	return wrapCOFF(data, relocs), nil
}

// buildRsrcData 生成 .rsrc 段的内容,以及其中所有"存的是段内偏移"的位置。
//
// 资源目录是三层树:
//
//	根 → 类型(RT_ICON / RT_GROUP_ICON)→ 语言 → 数据入口 → 数据本体
//
// 布局顺序按这个树逐层往下排,每个节点记下自己的偏移,最后统一回填。
func buildRsrcData(sizes []int) ([]byte, []int, error) {
	n := len(sizes)
	if n == 0 {
		return nil, nil, fmt.Errorf("至少要有一个尺寸")
	}

	images := make([][]byte, n)
	for i, s := range sizes {
		data, err := imageData(s)
		if err != nil {
			return nil, nil, err
		}
		images[i] = data
	}
	group := buildGroupIcon(sizes, images)

	// 语言目录是"头 + 一个条目"的连续 24 字节,不能拆开排,
	// 否则头后面的那个条目就跑到别处去了。
	const langDirSize = dirHeaderSize + dirEntrySize

	cur := 0
	alloc := func(size int) int {
		p := cur
		cur += size
		return p
	}

	rootOff := alloc(dirHeaderSize)
	rootEntryOff := alloc(2 * dirEntrySize)

	iconTypeOff := alloc(dirHeaderSize)
	iconTypeEntryOff := alloc(n * dirEntrySize)

	groupTypeOff := alloc(dirHeaderSize)
	groupTypeEntryOff := alloc(1 * dirEntrySize)

	iconLangOff := make([]int, n)
	for i := range iconLangOff {
		iconLangOff[i] = alloc(langDirSize)
	}
	groupLangOff := alloc(langDirSize)

	iconDataOff := make([]int, n)
	for i := range iconDataOff {
		iconDataOff[i] = alloc(dataEntrySize)
	}
	groupDataOff := alloc(dataEntrySize)

	iconBlobOff := make([]int, n)
	for i := range iconBlobOff {
		iconBlobOff[i] = alloc(len(images[i]))
	}
	groupBlobOff := alloc(len(group))

	buf := make([]byte, cur)
	var relocs []int

	// 目录头:前 12 字节是特征/时间戳/版本,全为 0,只有命名数和 ID 数要填。
	putDirHeader := func(off, idEntries int) {
		binary.LittleEndian.PutUint16(buf[off+14:], uint16(idEntries))
	}
	// 目录条目 = ID + 指向下一级的偏移。偏移是 RVA,要重定位。
	//
	// 偏移字段的最高位是个标志位:置 1 表示"指向下一级目录",清 0 表示
	// "指向数据入口"。少了它,系统会把这个子目录当成数据入口去解释,
	// 解出来的东西完全是错的 —— 而外壳图标 API 往往不报错,只是换一个
	// 默认图标给你,所以不看字节很难发现。
	putDirEntry := func(off int, id uint32, target int, isDir bool) {
		binary.LittleEndian.PutUint32(buf[off:], id)
		v := uint32(target)
		if isDir {
			v |= 0x80000000
		}
		binary.LittleEndian.PutUint32(buf[off+4:], v)
		relocs = append(relocs, off+4)
	}
	// 数据入口:偏移 + 长度 + 代码页 + 保留。只有偏移是 RVA。
	putDataEntry := func(off, blobOff, size int) {
		binary.LittleEndian.PutUint32(buf[off:], uint32(blobOff))
		binary.LittleEndian.PutUint32(buf[off+4:], uint32(size))
		relocs = append(relocs, off)
	}

	putDirHeader(rootOff, 2)
	putDirEntry(rootEntryOff, rtIcon, iconTypeOff, true)
	putDirEntry(rootEntryOff+dirEntrySize, rtGroupIcon, groupTypeOff, true)

	putDirHeader(iconTypeOff, n)
	for i := range sizes {
		// 图标 ID 从 1 开始 —— 0 是保留值
		putDirEntry(iconTypeEntryOff+i*dirEntrySize, uint32(i+1), iconLangOff[i], true)
		putDirHeader(iconLangOff[i], 1)
		putDirEntry(iconLangOff[i]+dirHeaderSize, langNeutral, iconDataOff[i], false)
		putDataEntry(iconDataOff[i], iconBlobOff[i], len(images[i]))
		copy(buf[iconBlobOff[i]:], images[i])
	}

	putDirHeader(groupTypeOff, 1)
	putDirEntry(groupTypeEntryOff, 1, groupLangOff, true)
	putDirHeader(groupLangOff, 1)
	putDirEntry(groupLangOff+dirHeaderSize, langNeutral, groupDataOff, false)
	putDataEntry(groupDataOff, groupBlobOff, len(group))
	copy(buf[groupBlobOff:], group)

	return buf, relocs, nil
}

// buildGroupIcon 组装 GRPICONDIR —— 图标组的内容。
//
// 每个条目用 ID(而不是偏移)指向对应的 RT_ICON,这是和 ICO 文件最大的
// 区别:ICO 里是文件偏移,资源里是资源 ID。
func buildGroupIcon(sizes []int, images [][]byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&b, binary.LittleEndian, uint16(1)) // type:1 = 图标
	_ = binary.Write(&b, binary.LittleEndian, uint16(len(sizes)))

	for i, s := range sizes {
		dim := byte(s)
		if s >= 256 {
			dim = 0 // 256 在这两个字段里用 0 表示
		}
		b.WriteByte(dim) // 宽
		b.WriteByte(dim) // 高
		b.WriteByte(0)   // 调色板颜色数
		b.WriteByte(0)   // reserved
		_ = binary.Write(&b, binary.LittleEndian, uint16(1))           // 颜色平面
		_ = binary.Write(&b, binary.LittleEndian, uint16(32))          // 位深
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(images[i]))) // 数据长度
		_ = binary.Write(&b, binary.LittleEndian, uint16(i+1))            // 对应的 RT_ICON ID
	}
	return b.Bytes()
}

// wrapCOFF 把段数据包成一个 COFF 对象文件。
//
// 对象里只有一个 .rsrc 段和一个符号表 —— 符号表里唯一的那个是段符号,
// 所有重定位都指向它。符号的值是 0,所以链接器算出来的加数就等于
// 我们写在重定位位置上的段内偏移。
func wrapCOFF(data []byte, relocs []int) []byte {
	sectDataOff := coffHeaderSize + coffSectSize
	relocOff := sectDataOff + len(data)
	symOff := relocOff + relocSize*len(relocs)
	stringTableOff := symOff + symEntrySize*2 // 段符号 + 它的辅助记录
	total := stringTableOff + 4               // 字符串表至少要有 4 字节的长度字段

	out := make([]byte, total)

	// ── COFF 文件头 ──
	binary.LittleEndian.PutUint16(out[0:], 0x8664) // Machine = AMD64
	binary.LittleEndian.PutUint16(out[2:], 1)      // NumberOfSections
	// TimeDateStamp 保持 0:定值能让构建可复现,同一次输入得到同一个文件
	binary.LittleEndian.PutUint32(out[8:], uint32(symOff)) // PointerToSymbolTable
	binary.LittleEndian.PutUint32(out[12:], 2)             // NumberOfSymbols(含辅助记录)

	// ── 段头 ──
	sh := out[coffHeaderSize:]
	copy(sh[0:8], ".rsrc")
	binary.LittleEndian.PutUint32(sh[16:], uint32(len(data)))       // SizeOfRawData
	binary.LittleEndian.PutUint32(sh[20:], uint32(sectDataOff))     // PointerToRawData
	binary.LittleEndian.PutUint32(sh[24:], uint32(relocOff))        // PointerToRelocations
	binary.LittleEndian.PutUint16(sh[32:], uint16(len(relocs)))     // NumberOfRelocations
	binary.LittleEndian.PutUint32(sh[36:], 0x40000040)              // 已初始化数据 | 可读

	// ── 段内容 ──
	copy(out[sectDataOff:], data)

	// ── 重定位 ──
	// 类型 0x0002 = IMAGE_REL_AMD64_ADDR32,32 位绝对地址。
	for i, off := range relocs {
		r := out[relocOff+i*relocSize:]
		binary.LittleEndian.PutUint32(r[0:], uint32(off)) // 段内偏移
		binary.LittleEndian.PutUint32(r[4:], 0)           // 符号表下标:段符号
		binary.LittleEndian.PutUint16(r[8:], 0x0002)      // IMAGE_REL_AMD64_ADDR32
	}

	// ── 符号表 ──
	// 段符号。issect 在 Go 链接器里判的是:
	// StorageClass == STATIC && Type == 0 && 名字以 '.' 开头。
	sym := out[symOff:]
	copy(sym[0:8], ".rsrc")
	// Value = 0 —— 加数完全由重定位位置上的内容决定
	binary.LittleEndian.PutUint16(sym[12:], 1) // SectionNumber(从 1 开始)
	binary.LittleEndian.PutUint16(sym[14:], 0) // Type
	sym[16] = 3                                // StorageClass = IMAGE_SYM_CLASS_STATIC
	sym[17] = 1                                // NumberOfAuxSymbols

	// 段符号的辅助记录:描述这个段本身
	aux := out[symOff+symEntrySize:]
	binary.LittleEndian.PutUint32(aux[0:], uint32(len(data)))     // Length
	binary.LittleEndian.PutUint16(aux[4:], uint16(len(relocs)))   // NumberOfRelocations

	// ── 字符串表 ──
	// 只有长度字段。我们的符号名不超过 8 字节,不需要额外字符串表条目,
	// 但表本身必须存在。
	binary.LittleEndian.PutUint32(out[stringTableOff:], 4)

	return out
}
