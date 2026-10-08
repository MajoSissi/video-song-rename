//go:build windows

// Package winproc 管的是「启动外部程序时的窗口行为」。
//
// 界面是 `-H windowsgui` 构建的，自身**没有控制台**。Windows 在这种父进程下
// 拉起控制台程序（shazam-sidecar.exe、cmd.exe 都是）时，默认会给子进程
// **新建一个控制台窗口** —— 用户看到的就是「启动软件时有个黑框闪一下」。
//
// 放在单独的包里，是因为 recognizer（拉 sidecar）和 app（打开日志 / 链接）
// 两边都要用，而它们之间没有依赖关系。
package winproc

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// NoConsole 让 cmd 启动的子进程不弹控制台窗口。
//
// **必须在 Start / Output / Run 之前调用。** 那几个方法一执行进程就已经建出来了，
// 之后再改 SysProcAttr 没有任何效果 —— os/exec 也不会报错，只是白设，
// 表现就是「改了没用、还是闪」。
//
// 两个字段一起设，它们管的是两件事：
//
//   - CreationFlags = CREATE_NO_WINDOW：明确要求**不创建**控制台。这是关键的那一个。
//   - HideWindow：把 STARTUPINFO 的 wShowWindow 设成 SW_HIDE，兜住不看
//     CreationFlags 的路径。
//
// 只设 HideWindow 是不够的：它管的是「窗口显示不显示」，而控制台窗口在
// CREATE_NO_WINDOW 缺位时是**先建出来、再隐藏**，中间那一帧仍然会闪。
func NoConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}
