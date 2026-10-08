package main

import (
	"encoding/binary"
	"errors"
)

// 托盘里那些「纯逻辑」的部分放在这里，不带 build tag：
// 菜单长什么样、点击落到哪个动作、ICO 怎么挑档——这些都能单测，
// 而真正的 Win32 调用（tray_windows.go）没法在测试里跑。
//
// 分开的理由很实际：菜单结构本身就是需求（哪些项、什么顺序、哪几条是灰的），
// 混在 syscall 里就只能靠肉眼验证。

// ------------------------------------------------------------------ 菜单

// Win32 菜单标志。
const (
	mfString    = 0x0000
	mfGrayed    = 0x0001 // 灰色、不可点击
	mfSeparator = 0x0800
)

// 托盘菜单项 ID。
const (
	trayIDStatus = 1 // 顶部的状态行，灰色不可点
	trayIDShow   = 2
	trayIDToggle = 3 // 开始 / 暂停 / 继续
	trayIDData   = 4
	trayIDQuit   = 5
)

// menuItem 描述右键菜单里的一条项目。
type menuItem struct {
	Flags uintptr
	ID    int
	Text  string
}

// trayStatus 是弹菜单那一刻的状态快照。
//
// 每次弹菜单现取，不缓存——状态行是给人看「现在到底行不行」的，
// 过期了就没意义。
type trayStatus struct {
	// Status 是顶部那行灰字的文案。要短，菜单是一扫而过的地方。
	Status string
	// Running 表示任务正在跑。
	Running bool
	// Paused 表示任务被暂停了。
	Paused bool
	// CanStart 表示队列里有待处理的文件，点了「开始处理」有意义。
	CanStart bool
}

// trayMenuLayout 算出右键菜单的内容（从上到下）。
//
// 结构是静态的，只有那个切换项的文案和可用性随状态变。
// 抽成纯函数是为了让测试能整条断言顺序、分段和 ID——
// 只断言「各项都在」的话，把「退出」挪到最上面照样能过。
func trayMenuLayout(st trayStatus) []menuItem {
	toggleText := "开始处理"
	toggleFlags := uintptr(mfString)
	switch {
	case st.Paused:
		toggleText = "继续"
	case st.Running:
		toggleText = "暂停"
	case !st.CanStart:
		// 队列是空的，点了什么也不会发生——与其让用户点了没反应，不如直接置灰。
		toggleFlags = mfGrayed
	}

	status := st.Status
	if status == "" {
		status = "就绪"
	}

	return []menuItem{
		{Flags: mfGrayed, ID: trayIDStatus, Text: status},
		{Flags: mfSeparator},
		{Flags: mfString, ID: trayIDShow, Text: "显示主窗口"},
		{Flags: toggleFlags, ID: trayIDToggle, Text: toggleText},
		{Flags: mfSeparator},
		{Flags: mfString, ID: trayIDData, Text: "打开数据目录"},
		{Flags: mfString, ID: trayIDQuit, Text: "退出"},
	}
}

// ------------------------------------------------------------------ 鼠标

type trayClick int

const (
	trayClickNone trayClick = iota
	trayClickShow           // 打开主窗口
	trayClickMenu           // 弹右键菜单
)

// 托盘回调消息里带的鼠标事件。
const (
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
)

// trayClickAction 把托盘回调的 lParam 翻译成动作。
//
// 左键单击**故意什么都不做**：托盘图标就缩在任务栏角落，误触概率很高，
// 单击就弹窗口非常烦人。要打开窗口得双击。
func trayClickAction(lparam uintptr) trayClick {
	switch uint32(lparam) {
	case wmLButtonDblClk:
		return trayClickShow
	case wmRButtonUp:
		return trayClickMenu
	default:
		// 包含 WM_LBUTTONUP
		return trayClickNone
	}
}

// ------------------------------------------------------------------ ICO

// icoFrame 是 .ico 里的一档。
type icoFrame struct {
	Width  int // 0 在文件里表示 256，这里已经换算过
	Height int
	Data   []byte // 直接就是 RT_ICON 要的那种 DIB，不需要重新编码
}

var errNotICO = errors.New("不是有效的 .ico 文件")

// parseICO 解析 .ico 容器：6 字节头 + N×16 字节目录项。
//
// 每一档的 Data 原样取出，不做任何转码——CreateIconFromResourceEx 吃的
// 就是这种 DIB，重新编码一遍只会引入 bug。
func parseICO(raw []byte) ([]icoFrame, error) {
	if len(raw) < 6 {
		return nil, errNotICO
	}
	if binary.LittleEndian.Uint16(raw[0:2]) != 0 {
		return nil, errNotICO
	}
	if binary.LittleEndian.Uint16(raw[2:4]) != 1 { // 1 = 图标
		return nil, errNotICO
	}

	count := int(binary.LittleEndian.Uint16(raw[4:6]))
	if count <= 0 || len(raw) < 6+count*16 {
		return nil, errNotICO
	}

	frames := make([]icoFrame, 0, count)
	for index := 0; index < count; index++ {
		entry := raw[6+index*16 : 6+index*16+16]

		width := int(entry[0])
		height := int(entry[1])
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}

		size := int(binary.LittleEndian.Uint32(entry[8:12]))
		offset := int(binary.LittleEndian.Uint32(entry[12:16]))
		if size <= 0 || offset < 0 || offset+size > len(raw) {
			continue
		}

		frames = append(frames, icoFrame{
			Width:  width,
			Height: height,
			Data:   raw[offset : offset+size],
		})
	}

	if len(frames) == 0 {
		return nil, errNotICO
	}
	return frames, nil
}

// pickICO 挑一档最接近 want 的。
//
// 顺序是「正好相等 → 比目标大的里面最小的 → 最大的那一档」。
// 这条顺序是有意为之：放大是凭空插值，边缘会发虚；缩小只是丢像素，观感明显更好。
// 高分屏上系统要 20/24，而 .ico 里只有 16 和 32 时，宁可给它 32 让系统缩。
func pickICO(frames []icoFrame, want int) (icoFrame, bool) {
	if len(frames) == 0 {
		return icoFrame{}, false
	}

	best := frames[0]
	// 初值不能给 0：所有分数都可能是负数（没有更大的档时），
	// 那样第一档就永远赢不过初值，函数会退化成「总是返回第一档」。
	bestScore := -1 << 30
	for _, frame := range frames {
		var score int
		switch {
		case frame.Width == want:
			score = 1 << 20 // 正好相等，压倒一切
		case frame.Width > want:
			score = 1000 - (frame.Width - want) // 更大：越小越好
		default:
			score = -1000 - (want - frame.Width) // 更小：只在没有更大的时才考虑
		}
		if score > bestScore {
			bestScore = score
			best = frame
		}
	}
	return best, true
}
