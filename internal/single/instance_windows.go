//go:build windows

// Package single 保证同一时间只有一个程序实例在跑。
//
// 为什么必须有：两个实例不只是「两个托盘图标」。它们各自读一份配置、各自跑一批
// 改名，而改名是**写磁盘**的操作 —— 两边可能同时改同一个文件；配置是读—改—写，
// 后写的那份会把先写的覆盖掉。这些都不会报错，只会安静地把状态弄乱。
//
// 实现用命名内核对象，不用锁文件：进程无论怎么退出（正常退出、崩溃、被任务管理器
// 结束），内核都会替它释放句柄，不会留下一个「上次没清干净」的锁把用户挡在门外。
package single

import (
	"errors"
	"sync"

	"golang.org/x/sys/windows"
)

const (
	// Local\ 前缀 = 每个登录会话一个命名空间。同一个用户开第二个会被拦下；
	// 换一个 Windows 用户登录还能再开一个 —— 那本来就是两份独立的配置和队列。
	// （用 Global\ 需要 SeCreateGlobalPrivilege，普通权限的进程建不出来。）
	mutexName = `Local\video-song-rename.single-instance`
	eventName = `Local\video-song-rename.show-window`
)

// Instance 是「我是那个唯一的实例」的凭证。
//
// 拿到之后要一直持有，**不要**去关它的句柄 —— 关掉等于把名额让出去，
// 之后启动的程序就会以为没人跑。句柄是有意不 Close 的：进程结束时内核会收走。
type Instance struct {
	mutex windows.Handle
	event windows.Handle
	once  sync.Once
}

// Acquire 抢名额。
//
// 返回 (nil, false) 表示已经有一个实例在跑 —— 这时它已经顺手通知对面把窗口
// 叫出来了，调用方**直接退出**即可，不用再做什么。
//
// 出错时返回 (nil, true)，也就是放行。宁可偶尔漏掉一次拦截，也不要把用户锁在
// 门外：单实例防的是「手滑双击两次」，不是安全边界。
func Acquire() (*Instance, bool) { return acquireNamed(mutexName, eventName) }

// acquireNamed 是 Acquire 的本体，名字由调用方给。
//
// 拆出这一层是为了**可测**：测试里直接用上面那两个常量的话，
// 用户恰好正开着这个程序时 Acquire 就会返回「已经有一个实例」，
// 测试变成随机失败 —— 那种测试没人敢信，也没人敢跑。
// 测试改成传一套自己的名字（见 instance_windows_test.go）。
func acquireNamed(mutex, event string) (*Instance, bool) {
	// 名字都是 ASCII，UTF16PtrFromString 不会失败（它只在字符串含 NUL 时报错）。
	name, _ := windows.UTF16PtrFromString(mutex)

	handle, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		// 名字被占了，但返回的句柄仍然有效，得自己关掉。
		windows.CloseHandle(handle)
		wakeExisting(event)
		return nil, false
	}
	if err != nil {
		return nil, true
	}

	// 自动复位事件（manualReset=0）：每次 SetEvent 只唤醒一次等待，
	// 连点两次图标就唤醒两次，不会攒成一次「点了没反应」。
	eventNamePtr, _ := windows.UTF16PtrFromString(event)
	eventHandle, eventErr := windows.CreateEvent(nil, 0, 0, eventNamePtr)
	if eventErr != nil {
		// 事件建不出来不影响「只有一个实例」这件事本身，只是第二个实例启动时
		// 叫不出窗口而已 —— 不值得因此把程序拦下。
		return &Instance{mutex: handle}, true
	}

	return &Instance{mutex: handle, event: eventHandle}, true
}

// Watch 在另一个实例要求「把窗口叫出来」时调用 show。
// 它自己开一个 goroutine 一直等，进程退出时随进程一起结束。
//
// 为什么走事件、而不是 FindWindow + ShowWindow：窗口可能是**收在托盘里**的，
// 而 Wails 自己记着「窗口现在可见吗」。绕过 Wails 直接 ShowWindow 会让它的记账
// 和实际状态对不上（托盘菜单里那两项就错了）。发个通知让它自己调 WindowShow，
// 走的才是同一条路径。
func (i *Instance) Watch(show func()) {
	if i == nil || i.event == 0 {
		return
	}
	i.once.Do(func() {
		go func() {
			for {
				status, err := windows.WaitForSingleObject(i.event, windows.INFINITE)
				if err != nil || status != windows.WAIT_OBJECT_0 {
					return
				}
				show()
			}
		}()
	})
}

// wakeExisting 通知已经跑着的那个实例：又有人点了一次图标，把窗口叫出来。
//
// 用 OpenEvent 而不是 CreateEvent：这里只想唤醒对方，不想在对方刚好已经退出时
// 顺手留下一个事件对象（那会让下一个实例的 Acquire 拿到一个陈旧的事件）。
func wakeExisting(event string) {
	name, _ := windows.UTF16PtrFromString(event)

	handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle)

	windows.SetEvent(handle)
}
