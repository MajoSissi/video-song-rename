//go:build windows

package main

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 托盘：Shell_NotifyIconW + 一个从不显示的隐藏窗口 + 自己跑消息循环。
//
// 为什么不用第三方库：本机没有 C 编译器，而 Windows 上的托盘本来就只需要
// user32/shell32 几个函数。自己写还能显式 runtime.LockOSThread()，
// 把「消息泵必须固定在创建窗口的那个 OS 线程」这条约束焊死——
// 这正是没法靠肉眼验证、一旦错了就表现为「菜单点了没反应」的那类问题。
//
// Wails 的 Windows 前端是**逐调用** LockOSThread，不是整体占住主线程，
// 所以这里在独立 goroutine 里再跑一个消息泵，两者互不干扰。

const (
	wmApp          = 0x8000
	wmTrayCallback = wmApp + 1 // 托盘图标的事件回调
	wmTrayRefresh  = wmApp + 2 // 请托盘线程刷新图标与悬停提示
	wmTrayBalloon  = wmApp + 3 // 请托盘线程弹一个气泡
	wmClose        = 0x0010
	wmDestroy      = 0x0002
	wmNull         = 0x0000
)

// Shell_NotifyIconW 的 wParam。
const (
	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002
)

// NOTIFYICONDATAW 的 uFlags。
const (
	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010 // 气泡
)

const (
	// 窗口类带 CS_DBLCLKS 才会收到 WM_LBUTTONDBLCLK。
	// 漏了它系统只送来两次 WM_LBUTTONUP，双击会**静默失效**——不报错，就是没反应。
	csDblClks = 0x0008

	trayIconID        = 1
	trayWindowClassID = "VideoSongRenameTrayWindow"
	trayAppName       = "视频歌曲重命名"

	smCXSmIcon = 49
	smCYSmIcon = 50

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon              = user32.NewProc("DestroyIcon")
	procRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
)

// ------------------------------------------------------------------ 结构体

// WNDCLASSEXW。x64 下是 80 字节。
type wndClassExW struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

// NOTIFYICONDATAW 的完整版（含 guidItem 与 hBalloonIcon）。x64 下是 976 字节。
//
// cbSize 写错的话 Shell_NotifyIconW 会直接返回 0，而且不告诉你为什么——
// 这是这个 API 最容易踩的坑，所以下面用 unsafe.Sizeof 算，别手写数字。
type notifyIconDataW struct {
	Size             uint32
	Wnd              windows.Handle
	ID               uint32
	Flags            uint32
	CallbackMessage  uint32
	Icon             windows.Handle
	Tip              [128]uint16
	State            uint32
	StateMask        uint32
	Info             [256]uint16
	TimeoutOrVersion uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	GUID             windows.GUID
	BalloonIcon      windows.Handle
}

type point struct{ X, Y int32 }

type msgStruct struct {
	Hwnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

// ------------------------------------------------------------------ 回调

// trayCallbacks 是托盘需要外面提供的动作。任何一个都可以为 nil。
//
// 这些都跑在**托盘线程**上，实现里不要直接碰 Wails 的窗口 API——
// 那些调用内部自己会 LockOSThread，跨线程混着来容易出怪问题。
// 需要操作窗口时用 runtime 的异步方法（它们本身就是投递出去的）。
type trayCallbacks struct {
	// Icon 返回托盘图标的 ICO 原始字节。
	Icon func() []byte
	// Tooltip 返回悬停提示文字。
	Tooltip func() string
	// Menu 在弹右键菜单前调用，返回此刻的状态。
	Menu func() trayStatus
	// Show 打开主窗口。
	Show func()
	// Toggle 开始 / 暂停 / 继续。
	Toggle func()
	// OpenData 打开数据目录。
	OpenData func()
	// Quit 退出程序。
	Quit func()
	// Log 写一行日志。
	Log func(string)
}

// activeTray 是给 WndProc 用的。C 回调带不了上下文，只能用包级变量。
//
// 赋值时机很关键：必须在 Shell_NotifyIconW(NIM_ADD) **成功之后**，
// 否则回调可能读到一个还没准备好的实例。
var activeTray *winTray

type winTray struct {
	cb trayCallbacks

	instance  uintptr
	className *uint16
	hwnd      windows.Handle
	icon      windows.Handle

	// 资源管理器重启后会广播 TaskbarCreated，图标被清掉，要重新加回去。
	taskbarCreated uint32

	// 气泡内容要跨线程传，PostMessage 只有两个整数参数，装不下字符串。
	mu           sync.Mutex
	balloonTitle string
	balloonText  string

	ready chan error
}

// ------------------------------------------------------------------ 启动

// startTray 建托盘并开始跑消息循环。返回后图标已经在任务栏上了。
func startTray(cb trayCallbacks) (*winTray, error) {
	tray := &winTray{cb: cb, ready: make(chan error, 1)}

	go func() {
		// 消息队列是按 OS 线程的：创建窗口和跑消息循环必须是同一个线程。
		// 不锁的话 Go 调度器可能把 goroutine 挪到别的 M 上，窗口就再也收不到消息了。
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := tray.init(); err != nil {
			tray.ready <- err
			return
		}
		tray.ready <- nil
		tray.pump()
	}()

	if err := <-tray.ready; err != nil {
		return nil, err
	}
	return tray, nil
}

func (t *winTray) init() error {
	// GetModuleHandleW(NULL) 拿的是当前 exe 的模块句柄，窗口类要挂在它下面。
	instance, _, callErr := procGetModuleHandleW.Call(0)
	if instance == 0 {
		return fmt.Errorf("取模块句柄失败: %v", callErr)
	}
	t.instance = instance

	name, err := windows.UTF16PtrFromString(trayWindowClassID)
	if err != nil {
		return err
	}
	t.className = name

	class := trayWindowClass(t.instance, name, windows.NewCallback(trayWndProc))
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return fmt.Errorf("注册托盘窗口类失败: %v", callErr)
	}

	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(name)),
		0, // 普通顶层窗口，但从不 ShowWindow，所以永远不可见
		0, 0, 0, 0,
		0, 0, t.instance, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("创建托盘窗口失败: %v", callErr)
	}
	t.hwnd = windows.Handle(hwnd)

	// 「资源管理器重启了」这条广播消息是动态注册出来的，没有固定编号。
	if message, _, _ := procRegisterWindowMessageW.Call(
		uintptr(unsafe.Pointer(mustUTF16Ptr("TaskbarCreated"))),
	); message != 0 {
		t.taskbarCreated = uint32(message)
	}

	t.applyIcon()

	data := t.baseData()
	data.Flags = nifMessage | nifIcon | nifTip
	fillUTF16(data.Tip[:], t.tooltip())

	if ok, _, callErr := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(data))); ok == 0 {
		return fmt.Errorf("Shell_NotifyIconW(NIM_ADD) 失败（cbSize=%d）: %v",
			unsafe.Sizeof(notifyIconDataW{}), callErr)
	}

	// 到这里图标才真的在任务栏上了，此时才让 WndProc 认得这个实例。
	activeTray = t
	return nil
}

func (t *winTray) pump() {
	var message msgStruct
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 = WM_QUIT，-1 = 出错
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&message)))
	}

	// 收尾：先摘图标，再释放句柄。
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(t.baseData())))
	if t.icon != 0 {
		procDestroyIcon.Call(uintptr(t.icon))
		t.icon = 0
	}
}

// Dispose 请托盘线程退出。异步：投一条 WM_CLOSE，窗口销毁后消息循环自然结束。
func (t *winTray) Dispose() {
	if t == nil || t.hwnd == 0 {
		return
	}
	procPostMessageW.Call(uintptr(t.hwnd), wmClose, 0, 0)
}

// Refresh 让托盘线程重新读一次图标与悬停提示。可以从任何 goroutine 调。
func (t *winTray) Refresh() {
	if t == nil || t.hwnd == 0 {
		return
	}
	procPostMessageW.Call(uintptr(t.hwnd), wmTrayRefresh, 0, 0)
}

// Balloon 弹一个气泡提示。无控制台的构建里，这是唯一能主动告诉用户事情的通路。
func (t *winTray) Balloon(title, text string) {
	if t == nil || t.hwnd == 0 {
		return
	}
	t.mu.Lock()
	t.balloonTitle, t.balloonText = title, text
	t.mu.Unlock()
	procPostMessageW.Call(uintptr(t.hwnd), wmTrayBalloon, 0, 0)
}

// ------------------------------------------------------------------ 窗口过程

func trayWndProc(hwnd windows.Handle, message uint32, wparam, lparam uintptr) uintptr {
	tray := activeTray

	switch message {
	case wmTrayCallback:
		if tray != nil {
			tray.handleClick(uint32(lparam))
		}
		return 0

	case wmTrayRefresh:
		if tray != nil {
			tray.applyIcon()
			data := tray.baseData()
			data.Flags = nifTip
			fillUTF16(data.Tip[:], tray.tooltip())
			procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(data)))
		}
		return 0

	case wmTrayBalloon:
		if tray != nil {
			tray.showBalloon()
		}
		return 0

	case wmClose:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}

	if tray != nil && tray.taskbarCreated != 0 && message == tray.taskbarCreated {
		// 资源管理器重启了，之前的图标已经不在，重新加回去。
		data := tray.baseData()
		data.Flags = nifMessage | nifIcon | nifTip
		fillUTF16(data.Tip[:], tray.tooltip())
		procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(data)))
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wparam, lparam)
	return ret
}

func (t *winTray) handleClick(lparam uint32) {
	switch trayClickAction(uintptr(lparam)) {
	case trayClickShow:
		if t.cb.Show != nil {
			t.cb.Show()
		}
	case trayClickMenu:
		t.showMenu()
	}
}

func (t *winTray) showMenu() {
	if t.cb.Menu == nil {
		return
	}
	// 现取状态，不缓存——状态行是给人看「现在到底行不行」的。
	items := trayMenuLayout(t.cb.Menu())

	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	// AppendMenuW 会拷贝字符串，但传给 Call 的是 uintptr，Go 的 GC 看不到它。
	// 把指针攒在 keep 里，让它们在整轮 append 期间都活着。
	var keep []*uint16
	for _, item := range items {
		var text uintptr
		if !item.isSeparator() {
			pointer, err := windows.UTF16PtrFromString(item.Text)
			if err != nil {
				continue
			}
			keep = append(keep, pointer)
			text = uintptr(unsafe.Pointer(pointer))
		}
		procAppendMenuW.Call(menu, item.Flags, uintptr(item.ID), text)
	}
	runtime.KeepAlive(keep)

	var cursor point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))

	// 不先抢前台，点菜单外面菜单不会消失——这是 TrackPopupMenu 的老毛病。
	procSetForegroundWindow.Call(uintptr(t.hwnd))

	command, _, _ := procTrackPopupMenu.Call(
		menu,
		tpmReturnCmd|tpmRightButton|tpmLeftAlign,
		uintptr(cursor.X), uintptr(cursor.Y),
		0, uintptr(t.hwnd), 0,
	)
	if command != 0 {
		t.dispatch(int(command))
	}
	// 同上，配合 SetForegroundWindow 用。
	procPostMessageW.Call(uintptr(t.hwnd), wmNull, 0, 0)
}

func (t *winTray) dispatch(id int) {
	switch id {
	case trayIDStatus:
		// 顶部的灰色状态行。Win32 的禁用项在键盘导航等路径下**仍会**发命令，
		// 必须显式吃掉。顺带一个好处：ID 一旦和动作项撞号，
		// 这里会出现重复 case，编译器直接报错。
		return
	case trayIDShow:
		if t.cb.Show != nil {
			t.cb.Show()
		}
	case trayIDToggle:
		if t.cb.Toggle != nil {
			t.cb.Toggle()
		}
	case trayIDData:
		if t.cb.OpenData != nil {
			t.cb.OpenData()
		}
	case trayIDQuit:
		if t.cb.Quit != nil {
			t.cb.Quit()
		}
	}
}

// ------------------------------------------------------------------ 图标

func (t *winTray) applyIcon() {
	if t.cb.Icon == nil {
		return
	}
	raw := t.cb.Icon()
	frames, err := parseICO(raw)
	if err != nil {
		t.logf("托盘图标解析失败: %v", err)
		return
	}

	want := int(getSystemMetrics(smCXSmIcon))
	frame, ok := pickICO(frames, want)
	if !ok {
		t.logf("托盘图标里没有可用的尺寸")
		return
	}

	icon, err := createIconFromResource(frame.Data, frame.Width, frame.Height)
	if err != nil {
		t.logf("创建托盘图标失败: %v", err)
		return
	}

	// 先装新的再销毁旧的，中间不会出现空图标。
	previous := t.icon
	t.icon = icon
	if t.hwnd != 0 && previous != 0 {
		data := t.baseData()
		data.Flags = nifIcon
		procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(data)))
		procDestroyIcon.Call(uintptr(previous))
	}

	// 把这行写进日志：DPI 感知没生效时图标会糊，而「实际用了哪一档」
	// 是唯一能证明它生效了的客观信号。
	t.logf("托盘图标已就绪：%d×%d（系统要求 %d×%d）", frame.Width, frame.Height, want, want)
}

func (t *winTray) showBalloon() {
	t.mu.Lock()
	title, text := t.balloonTitle, t.balloonText
	t.mu.Unlock()
	if text == "" {
		return
	}

	data := t.baseData()
	data.Flags = nifInfo
	fillUTF16(data.Info[:], text)
	fillUTF16(data.InfoTitle[:], title)
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(data)))
}

func (t *winTray) baseData() *notifyIconDataW {
	return &notifyIconDataW{
		Size:            uint32(unsafe.Sizeof(notifyIconDataW{})),
		Wnd:             t.hwnd,
		ID:              trayIconID,
		CallbackMessage: wmTrayCallback,
		Icon:            t.icon,
	}
}

func (t *winTray) tooltip() string {
	if t.cb.Tooltip == nil {
		return trayAppName
	}
	if text := t.cb.Tooltip(); text != "" {
		return text
	}
	return trayAppName
}

func (t *winTray) logf(format string, args ...any) {
	if t.cb.Log != nil {
		t.cb.Log(fmt.Sprintf(format, args...))
	}
}

// trayWindowClass 单独抽出来是为了能被测试断言——
// 「窗口类必须带 CS_DBLCLKS」这条错了的话，症状是双击没反应，没有任何报错。
func trayWindowClass(instance uintptr, className *uint16, proc uintptr) wndClassExW {
	return wndClassExW{
		Size:      uint32(unsafe.Sizeof(wndClassExW{})),
		Style:     csDblClks,
		WndProc:   proc,
		Instance:  windows.Handle(instance),
		ClassName: className,
	}
}

func createIconFromResource(data []byte, cx, cy int) (windows.Handle, error) {
	if len(data) == 0 {
		return 0, errors.New("图标数据是空的")
	}
	handle, _, callErr := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		1,          // fIcon = TRUE
		0x00030000, // 版本号，必须正好是这个
		uintptr(cx), uintptr(cy),
		0, // LR_DEFAULTCOLOR
	)
	if handle == 0 {
		return 0, fmt.Errorf("CreateIconFromResourceEx 失败: %v", callErr)
	}
	return windows.Handle(handle), nil
}

func getSystemMetrics(index int) int32 {
	value, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int32(value)
}

// ------------------------------------------------------------------ 工具

func mustUTF16Ptr(s string) *uint16 {
	pointer, err := windows.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return pointer
}

// fillUTF16 把 s 编成 UTF-16 填进 dst，保证以 NUL 结尾；超长就截断。
//
// 注意 dst 的长度是 **UTF-16 码元**数，不是 rune 数——一个 emoji 占两个码元。
// 按 rune 去截会以为放得下，实际编码后塞满整个缓冲、连结尾的 NUL 都没有，
// 于是 Windows 会一直往后读那块内存。
func fillUTF16(dst []uint16, s string) {
	for index := range dst {
		dst[index] = 0
	}
	if len(dst) == 0 {
		return
	}

	// UTF16FromString 遇到 NUL 会报错，先剔掉（配置里的字符串不该有，但别让它把整句吞掉）。
	encoded, err := windows.UTF16FromString(strings.ReplaceAll(s, "\x00", ""))
	if err != nil {
		return
	}

	// encoded 末尾自带一个 NUL，所以 len(encoded) 已经含了它。
	if len(encoded) > len(dst) {
		encoded = encoded[:len(dst)]
		encoded[len(dst)-1] = 0
		// 别把一个代理对切成两半：末位成了孤零零的高位代理就一起丢掉，
		// 否则 Windows 会画出一个替换字符。
		if len(encoded) >= 2 {
			last := encoded[len(encoded)-2]
			if last >= 0xD800 && last <= 0xDBFF {
				encoded[len(encoded)-2] = 0
			}
		}
	}
	copy(dst, encoded)
}

func (m menuItem) isSeparator() bool { return m.Flags&mfSeparator != 0 }
