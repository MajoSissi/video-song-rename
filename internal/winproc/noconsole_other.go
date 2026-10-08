//go:build !windows

package winproc

import "os/exec"

// NoConsole 在非 Windows 上是空操作：那边的进程模型里没有「父进程没控制台、
// 子进程就新建一个」这回事，终端窗口不会凭空冒出来。
//
// 这个文件存在的意义只是让调用方（recognizer / app）不必自己加构建约束。
func NoConsole(cmd *exec.Cmd) {}
