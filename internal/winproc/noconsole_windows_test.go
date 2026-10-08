//go:build windows

package winproc

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

// 两个标志缺一不可，而且**只设 HideWindow 挡不住**。
//
// 这个程序自己是 GUI 程序（-H windowsgui），没有控制台。GUI 程序拉起控制台子进程
// 时，Windows 默认给它**新建**一个控制台窗口 —— 这就是用户报的
// 「打开软件会有个命令窗口闪过去」。
//
// HideWindow 的意思是「建出来，但别显示」：窗口确实建了，中间那一帧仍然会闪。
// CREATE_NO_WINDOW 才是「压根不要建」。所以这条断言盯的是 CreationFlags ——
// 只验 HideWindow 的话，把它删掉、退回原来那个会闪的写法，测试照样通过。
func TestNoConsole_SuppressesTheConsoleWindow(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "echo")
	NoConsole(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("NoConsole 没设 SysProcAttr —— 子进程会带出一个控制台窗口")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow 没设 —— 窗口会先建出来再藏，中间那一帧仍然会闪")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Errorf("CreationFlags 里没有 CREATE_NO_WINDOW（实际 %#x）—— "+
			"只靠 HideWindow 挡不住启动时那一帧闪现",
			cmd.SysProcAttr.CreationFlags)
	}
}
