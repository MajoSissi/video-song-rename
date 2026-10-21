//go:build windows

package winproc

import (
	"os/exec"
	"syscall"
)

// RevealCommand 返回「在资源管理器里打开 path 所在目录并选中它」的命令。
//
// 命令行是**手拼**的，不能让 os/exec 按参数去拼 —— 这是这个函数存在的全部理由。
//
// explorer 只认 `/select,<路径>` 这一种写法，引号必须**只包住路径**：
//
//	explorer /select,"C:\My Videos\a.mp4"     ← 对：打开 My Videos 并选中 a.mp4
//	explorer "/select,C:\My Videos\a.mp4"     ← 错：打开「文档」
//
// 第二种正是 os/exec 拼出来的东西：参数里带空格时，它会把**整个**参数（连同
// `/select,`）包进引号。explorer 解析不出被引号裹住的 `/select` 开关，于是
// 退回到默认目录 —— 用户看到的就是「点定位，打开了文档」。
//
// 本机实测（Win11，2026-10-08）：
//
//	/select,C:\...\Temp\vsr probe dir\song.mp4   → 新窗口「文档」
//	/select,C:\...\Temp\vsrprobedir\song.mp4     → 新窗口「vsrprobedir」   ← 无空格就正常
//	/select,"C:\...\Temp\vsr probe dir\song.mp4" → 新窗口「vsr probe dir」 ← 手拼就正常
//
// 所以「路径带空格才坏」：同一个文件夹里没空格的路径一直是好的，用户很难
// 从「有的能定位、有的不能」里看出规律，只会觉得这个按钮不可靠。
//
// 而 os/exec 拼不出第一种形式 —— 参数里的 `"` 会被它转义成 `\"`，写
// `"/select,\""+path+"\""` 只会得到第三种错误写法。只能绕开参数拼接，
// 直接把命令行交给 CreateProcess（SysProcAttr.CmdLine 非空时 os/exec 原样使用它）。
//
// path 必须是**磁盘上真实存在**的路径（调用方已经 os.Stat 过）：Windows 文件名
// 里不允许出现 `"`，所以手拼不会有注入面。
func RevealCommand(path string) *exec.Cmd {
	// 不带 args：命令行整条由 CmdLine 决定。exec.Command 仍会去解析 explorer.exe
	// 的完整路径，找不到时会返回一个 Start 必然报错的 Cmd，这是我们要的行为。
	command := exec.Command("explorer")
	command.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `explorer /select,"` + path + `"`,
	}
	return command
}
