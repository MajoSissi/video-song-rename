//go:build ignore

// 生成 rsrc_windows_amd64.syso —— exe 的图标与版本资源。
//
// 环境里没有 windres / objcopy，所以直接手写 COFF 对象。.syso 会被 go build
// 自动链接进 exe（只要它待在包目录里）。
//
// 用法：
//     go run tools/genrsrc.go
//
// 输出：
//     rsrc_windows_amd64.syso
//
// 图标来源是 assets/icon.ico —— 和托盘图标同一份。换图标只要换那个文件再重跑。

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtVersion   = 16

	// COFF 段特征位：CNT_INITIALIZED_DATA | MEM_READ。
	//
	// 必须**恰好**是这两个。多带一个 MEM_DISCARDABLE，链接器会静默跳过整个段——
	// 编译成功、exe 生成、就是没图标，没有任何报错。
	sectionCharacteristics = 0x40000040

	imageRelAmd64Addr32 = 0x0002
	machineAMD64        = 0x8664
)

// ------------------------------------------------------------------ 资源目录树

// resData 指向一段真正的资源数据。
type resData struct {
	blob     []byte
	offset   uint32 // 数据在段内的偏移，写进 IMAGE_RESOURCE_DATA_ENTRY 并等链接器改成 RVA
	entryAt  uint32 // 这条数据项在段内的偏移，重定位打在这里
	codePage uint32
}

// resDir 是资源目录的一层。
type resDir struct {
	entries []resEntry
	offset  uint32
}

type resEntry struct {
	id  uint32
	sub *resDir
	dat *resData
}

// buildResourceTree 组装 type → name → lang → data 四层。
//
// 本项目全部用数字 ID（不需要字符串名），所以每层的 NumberOfNamedEntries 都是 0。
// 形状是固定的四层，直接展开写比套一个通用递归更好核对。
func buildResourceTree(icons []icoFrame, group []byte, version []byte) *resDir {
	// 叶子：名称层下面挂语言层，语言层指向真正的数据。
	leaf := func(id uint32, blob []byte, codePage uint32) resEntry {
		return resEntry{id: id, sub: &resDir{entries: []resEntry{
			{id: 1033, dat: &resData{blob: blob, codePage: codePage}}, // 1033 = en-US
		}}}
	}

	iconDir := &resDir{}
	for index, frame := range icons {
		iconDir.entries = append(iconDir.entries, leaf(uint32(index+1), frame.blob, 0))
	}

	return &resDir{entries: []resEntry{
		{id: rtIcon, sub: iconDir},
		{id: rtGroupIcon, sub: &resDir{entries: []resEntry{leaf(1, group, 0)}}},
		{id: rtVersion, sub: &resDir{entries: []resEntry{leaf(1, version, 1200)}}},
	}}
}

// ------------------------------------------------------------------ 布局

func align4(value uint32) uint32 { return (value + 3) &^ 3 }

func (d *resDir) size() uint32 { return 16 + uint32(len(d.entries))*8 }

// assignOffsets 按「所有目录 → 所有数据项 → 所有数据块」的顺序排布。
//
// 关键约束：**每一层的偏移都是相对资源目录起点（也就是段起点）的**，
// 不是相对父目录。所以只要都从同一个 0 开始累加就对。
func (d *resDir) assignOffsets(next *uint32) {
	d.offset = *next
	*next += d.size()
	for index := range d.entries {
		if d.entries[index].sub != nil {
			d.entries[index].sub.assignOffsets(next)
		}
	}
}

func (d *resDir) assignDataOffsets(next *uint32) {
	for index := range d.entries {
		entry := &d.entries[index]
		if entry.dat != nil {
			entry.dat.entryAt = *next
			*next += 16 // IMAGE_RESOURCE_DATA_ENTRY
		}
		if entry.sub != nil {
			entry.sub.assignDataOffsets(next)
		}
	}
}

func (d *resDir) assignBlobOffsets(next *uint32) {
	for index := range d.entries {
		entry := &d.entries[index]
		if entry.dat != nil {
			entry.dat.offset = *next
			*next += align4(uint32(len(entry.dat.blob)))
		}
		if entry.sub != nil {
			entry.sub.assignBlobOffsets(next)
		}
	}
}

func (d *resDir) collectRelocations(out *[]reloc) {
	for index := range d.entries {
		entry := &d.entries[index]
		if entry.dat != nil {
			*out = append(*out, reloc{at: entry.dat.entryAt, addend: entry.dat.offset})
		}
		if entry.sub != nil {
			entry.sub.collectRelocations(out)
		}
	}
}

type reloc struct {
	at     uint32 // 段内偏移
	addend uint32 // 预先写进去的值，链接器会把它改写成 RVA
}

// ------------------------------------------------------------------ 序列化

func (d *resDir) write(buf []byte) {
	binary.LittleEndian.PutUint32(buf[d.offset:], 0)                    // Characteristics
	binary.LittleEndian.PutUint32(buf[d.offset+4:], 0)                  // TimeDateStamp
	binary.LittleEndian.PutUint16(buf[d.offset+8:], 0)                  // MajorVersion
	binary.LittleEndian.PutUint16(buf[d.offset+10:], 0)                 // MinorVersion
	binary.LittleEndian.PutUint16(buf[d.offset+12:], 0)                 // NumberOfNamedEntries
	binary.LittleEndian.PutUint16(buf[d.offset+14:], uint16(len(d.entries)))

	for index, entry := range d.entries {
		at := d.offset + 16 + uint32(index)*8
		binary.LittleEndian.PutUint32(buf[at:], entry.id)

		var target uint32
		var isDir bool
		switch {
		case entry.sub != nil:
			target, isDir = entry.sub.offset, true
		case entry.dat != nil:
			target, isDir = entry.dat.entryAt, false
		}
		if isDir {
			target |= 0x80000000 // 最高位 1 = 指向子目录。漏了这面旗，系统就不会往下找。
		}
		binary.LittleEndian.PutUint32(buf[at+4:], target)
	}

	for index := range d.entries {
		entry := &d.entries[index]
		if entry.dat != nil {
			at := entry.dat.entryAt
			// OffsetToData 是 RVA，构建期不知道段基址，所以先写段内偏移，
			// 靠重定位在链接期补成真正的 RVA。
			binary.LittleEndian.PutUint32(buf[at:], entry.dat.offset)
			binary.LittleEndian.PutUint32(buf[at+4:], uint32(len(entry.dat.blob)))
			binary.LittleEndian.PutUint32(buf[at+8:], entry.dat.codePage)
			binary.LittleEndian.PutUint32(buf[at+12:], 0)
			copy(buf[entry.dat.offset:], entry.dat.blob)
		}
		if entry.sub != nil {
			entry.sub.write(buf)
		}
	}
}

// ------------------------------------------------------------------ COFF

func writeCOFF(path string, section []byte, relocs []reloc) error {
	const (
		headerSize    = 20
		sectionHdr    = 40
		relocSize     = 10
		symbolSize    = 18
		stringTable   = 4
	)

	relocTableAt := headerSize + sectionHdr + len(section)
	symbolTableAt := relocTableAt + len(relocs)*relocSize
	stringTableAt := symbolTableAt + symbolSize

	total := stringTableAt + stringTable
	out := make([]byte, total)

	// ---- COFF 头 ----
	binary.LittleEndian.PutUint16(out[0:], machineAMD64)
	binary.LittleEndian.PutUint16(out[2:], 1) // NumberOfSections
	binary.LittleEndian.PutUint32(out[4:], 0) // TimeDateStamp
	binary.LittleEndian.PutUint32(out[8:], uint32(symbolTableAt))
	binary.LittleEndian.PutUint32(out[12:], 1) // NumberOfSymbols
	binary.LittleEndian.PutUint16(out[16:], 0) // SizeOfOptionalHeader
	binary.LittleEndian.PutUint16(out[18:], 0) // Characteristics

	// ---- 段头 ----
	at := headerSize
	copy(out[at:], ".rsrc\x00\x00\x00")
	// VirtualSize 必须等于 SizeOfRawData
	binary.LittleEndian.PutUint32(out[at+8:], uint32(len(section)))
	binary.LittleEndian.PutUint32(out[at+12:], 0) // VirtualAddress
	binary.LittleEndian.PutUint32(out[at+16:], uint32(len(section)))
	// PointerToRawData **必须**是段数据在文件里的真实偏移，不能写 0。
	//
	// 一般 COFF 对象里这个字段确实是 0，但 Go 1.27 的 debug/pe 有一条
	// 「PointerToRawData == 0 就当成 BSS 段」，之后任何 sect.Data() 都会
	// 返回 "unexpected read from section with uninitialized data"，
	// 链接直接失败。踩过一次，记在这里。
	binary.LittleEndian.PutUint32(out[at+20:], headerSize+sectionHdr)
	binary.LittleEndian.PutUint32(out[at+24:], uint32(relocTableAt))
	binary.LittleEndian.PutUint32(out[at+28:], 0) // PointerToLinenumbers
	binary.LittleEndian.PutUint16(out[at+32:], uint16(len(relocs)))
	binary.LittleEndian.PutUint16(out[at+34:], 0) // NumberOfLinenumbers
	binary.LittleEndian.PutUint32(out[at+36:], sectionCharacteristics)

	// ---- 段数据 ----
	copy(out[headerSize+sectionHdr:], section)

	// ---- 重定位表 ----
	at = relocTableAt
	for _, item := range relocs {
		binary.LittleEndian.PutUint32(out[at:], item.at)
		binary.LittleEndian.PutUint32(out[at+4:], 0) // 指向符号表第 0 项（段符号）
		binary.LittleEndian.PutUint16(out[at+8:], imageRelAmd64Addr32)
		at += relocSize
	}

	// ---- 符号表：一条段符号 ----
	at = symbolTableAt
	copy(out[at:], ".rsrc\x00\x00\x00")
	binary.LittleEndian.PutUint32(out[at+8:], 0)  // Value
	binary.LittleEndian.PutUint16(out[at+12:], 1) // SectionNumber
	binary.LittleEndian.PutUint16(out[at+14:], 0) // Type
	out[at+16] = 3                                // StorageClass = STATIC
	out[at+17] = 0                                // NumberOfAuxSymbols

	// ---- 字符串表（符号名正好 8 字节，内联存放，所以只有长度字段）----
	binary.LittleEndian.PutUint32(out[stringTableAt:], stringTable)

	return os.WriteFile(path, out, 0o644)
}

// ------------------------------------------------------------------ 图标

type icoFrame struct {
	width, height int
	bits          uint16
	blob          []byte
}

func parseICO(raw []byte) ([]icoFrame, error) {
	if len(raw) < 6 || binary.LittleEndian.Uint16(raw[2:4]) != 1 {
		return nil, fmt.Errorf("不是有效的 .ico")
	}
	count := int(binary.LittleEndian.Uint16(raw[4:6]))
	frames := make([]icoFrame, 0, count)
	for index := 0; index < count; index++ {
		entry := raw[6+index*16 : 6+index*16+16]
		width, height := int(entry[0]), int(entry[1])
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}
		size := int(binary.LittleEndian.Uint32(entry[8:12]))
		offset := int(binary.LittleEndian.Uint32(entry[12:16]))
		if offset+size > len(raw) {
			return nil, fmt.Errorf("第 %d 档超出文件范围", index)
		}
		frames = append(frames, icoFrame{
			width: width, height: height,
			bits: binary.LittleEndian.Uint16(entry[6:8]),
			blob: raw[offset : offset+size],
		})
	}
	return frames, nil
}

// buildGroupIcon 造 GRPICONDIR。注意 nID 指向 RT_ICON 的 ID（从 1 开始），
// 不像 .ico 文件里存的是数据偏移。
func buildGroupIcon(frames []icoFrame) []byte {
	out := make([]byte, 6+len(frames)*14)
	binary.LittleEndian.PutUint16(out[0:], 0) // 保留
	binary.LittleEndian.PutUint16(out[2:], 1) // 1 = 图标
	binary.LittleEndian.PutUint16(out[4:], uint16(len(frames)))

	for index, frame := range frames {
		at := 6 + index*14
		dimension := func(value int) byte {
			if value >= 256 {
				return 0
			}
			return byte(value)
		}
		out[at] = dimension(frame.width)
		out[at+1] = dimension(frame.height)
		out[at+2] = 0 // 调色板颜色数
		out[at+3] = 0 // 保留
		binary.LittleEndian.PutUint16(out[at+4:], 1) // 色彩平面
		binary.LittleEndian.PutUint16(out[at+6:], frame.bits)
		binary.LittleEndian.PutUint32(out[at+8:], uint32(len(frame.blob)))
		binary.LittleEndian.PutUint16(out[at+12:], uint16(index+1)) // nID
	}
	return out
}

// ------------------------------------------------------------------ 版本资源

func utf16z(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, (len(units)+1)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(out[index*2:], unit)
	}
	return out
}

func pad4(buf []byte) []byte {
	for len(buf)%4 != 0 {
		buf = append(buf, 0)
	}
	return buf
}

// versionNode 拼一个 VS_VERSIONINFO 里的节点：wLength/wValueLength/wType + szKey + 对齐 + 值。
//
// 关键：**整个节点要补到 4 字节边界，而且补齐的长度要算进 wLength**。
// 下一个兄弟节点的位置是「上一个的起点 + wLength」，不补的话后面全部错位——
// 症状是资源确实在 exe 里、Windows 却一个字段都读不出来，不报任何错。
func versionNode(key string, valueLength uint16, valueType uint16, value []byte, children []byte) []byte {
	buf := make([]byte, 6)
	binary.LittleEndian.PutUint16(buf[2:], valueLength)
	binary.LittleEndian.PutUint16(buf[4:], valueType)
	buf = append(buf, utf16z(key)...)
	buf = pad4(buf)
	buf = append(buf, value...)
	if len(children) > 0 {
		buf = pad4(buf)
		buf = append(buf, children...)
	}
	buf = pad4(buf)
	binary.LittleEndian.PutUint16(buf[0:], uint16(len(buf)))
	return buf
}

func versionString(key, value string) []byte {
	encoded := utf16z(value)
	// wValueLength 对字符串来说按「字符数」算，含结尾的 NUL。
	return versionNode(key, uint16(len(encoded)/2), 1, encoded, nil)
}

func buildVersionInfo(major, minor, patch, build int) []byte {
	fixed := make([]byte, 52)
	binary.LittleEndian.PutUint32(fixed[0:], 0xFEEF04BD)
	binary.LittleEndian.PutUint32(fixed[4:], 0x00010000)
	binary.LittleEndian.PutUint32(fixed[8:], uint32(major)<<16|uint32(minor))
	binary.LittleEndian.PutUint32(fixed[12:], uint32(patch)<<16|uint32(build))
	binary.LittleEndian.PutUint32(fixed[16:], uint32(major)<<16|uint32(minor))
	binary.LittleEndian.PutUint32(fixed[20:], uint32(patch)<<16|uint32(build))
	binary.LittleEndian.PutUint32(fixed[24:], 0x3F)          // FileFlagsMask
	binary.LittleEndian.PutUint32(fixed[28:], 0)             // FileFlags
	binary.LittleEndian.PutUint32(fixed[32:], 0x00040004)    // FileOS = VOS_NT_WINDOWS32
	binary.LittleEndian.PutUint32(fixed[36:], 1)             // FileType = VFT_APP
	binary.LittleEndian.PutUint32(fixed[40:], 0)             // FileSubtype
	binary.LittleEndian.PutUint32(fixed[44:], 0)             // FileDateMS
	binary.LittleEndian.PutUint32(fixed[48:], 0)             // FileDateLS

	text := fmt.Sprintf("%d.%d.%d.%d", major, minor, patch, build)

	table := []byte{}
	for _, pair := range [][2]string{
		{"CompanyName", ""},
		{"FileDescription", "视频歌曲重命名"},
		{"FileVersion", text},
		{"InternalName", "video-song-rename"},
		{"OriginalFilename", "video-song-rename.exe"},
		{"ProductName", "视频歌曲重命名"},
		{"ProductVersion", text},
	} {
		table = append(table, versionString(pair[0], pair[1])...)
	}
	stringTable := versionNode("040904B0", 0, 0, nil, table)
	stringFileInfo := versionNode("StringFileInfo", 0, 0, nil, stringTable)

	// VarFileInfo 里那个 Translation 是一个 DWORD，不是字符串。
	translation := make([]byte, 4)
	binary.LittleEndian.PutUint32(translation, 0x040904B0)
	variable := versionNode("Translation", 4, 0, translation, nil)
	varFileInfo := versionNode("VarFileInfo", 0, 0, nil, variable)

	return versionNode("VS_VERSION_INFO", 52, 0, fixed, append(stringFileInfo, varFileInfo...))
}

// ------------------------------------------------------------------ 入口

var versionPattern = regexp.MustCompile(`\bversion\s*=\s*"([0-9][0-9.]*)"`)

func readVersion(root string) (int, int, int, int) {
	raw, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "读不到 main.go，版本号用 0.0.0.0：%v\n", err)
		return 0, 0, 0, 0
	}
	match := versionPattern.FindSubmatch(raw)
	if match == nil {
		fmt.Fprintln(os.Stderr, "main.go 里没找到 version 常量，版本号用 0.0.0.0")
		return 0, 0, 0, 0
	}

	parts := strings.Split(string(match[1]), ".")
	numbers := []int{0, 0, 0, 0}
	for index := 0; index < len(parts) && index < 4; index++ {
		value, err := strconv.Atoi(parts[index])
		if err != nil {
			continue
		}
		numbers[index] = value
	}
	return numbers[0], numbers[1], numbers[2], numbers[3]
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	iconPath := filepath.Join(root, "assets", "icon.ico")
	raw, err := os.ReadFile(iconPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读不到 %s：%v\n（先跑 python tools/make_icon.py）\n", iconPath, err)
		os.Exit(1)
	}

	icons, err := parseICO(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	major, minor, patch, build := readVersion(root)
	tree := buildResourceTree(icons, buildGroupIcon(icons), buildVersionInfo(major, minor, patch, build))

	next := uint32(0)
	tree.assignOffsets(&next)
	tree.assignDataOffsets(&next)
	tree.assignBlobOffsets(&next)

	var relocations []reloc
	tree.collectRelocations(&relocations)

	section := make([]byte, next)
	tree.write(section)

	out := filepath.Join(root, "rsrc_windows_amd64.syso")
	if err := writeCOFF(out, section, relocations); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("写入 %s\n", out)
	fmt.Printf("  图标 %d 档：", len(icons))
	for index, frame := range icons {
		if index > 0 {
			fmt.Print(", ")
		}
		fmt.Printf("%d×%d", frame.width, frame.height)
	}
	fmt.Printf("\n  版本 %d.%d.%d.%d\n", major, minor, patch, build)
	fmt.Printf("  段大小 %d 字节，重定位 %d 条\n", len(section), len(relocations))
}
