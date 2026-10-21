//go:build !windows

package winproc

import "os/exec"

// RevealCommand 在非 Windows 上没有对应物（explorer 是 Windows 的东西）。
//
// 这个文件存在的意义和 noconsole_other.go 一样：让调用方（app）不必自己加
// 构建约束。真要在这边跑起来，得换成 xdg-open / open 之类，但这个程序本来就
// 是 Windows 专用的（托盘、DPI、单实例都是 Win32 实现）。
func RevealCommand(path string) *exec.Cmd {
	return exec.Command("explorer", "/select,"+path)
}
