//go:build windows

package main

import (
	"syscall"
)

// DPI 感知必须在**创建任何窗口之前**声明，否则系统会按缩放比例把整个进程的
// 位图拉伸一遍。这不只是「有点糊」的问题，它会直接改掉托盘图标的档位：
//
//	150% 缩放的屏幕上，任务栏实际需要 24×24；
//	而 DPI 不感知的进程调 GetSystemMetrics(SM_CXSMICON) 只会拿到虚拟化后的 16。
//	交出去 16×16 让外壳放大到 24×24 → 糊。
//
// 同理，窗口和 WebView 也会被拉伸，文字发虚。所以这一条要在 main() 最前面跑。
//
// 三级回退对应三代 Windows，拿不到就返回空串（进程保持不感知，功能不受影响）。

var (
	user32dpi = syscall.NewLazyDLL("user32.dll")
	shcoredpi = syscall.NewLazyDLL("shcore.dll")

	procSetProcessDpiAwarenessContext = user32dpi.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32dpi.NewProc("SetProcessDPIAware")
	procSetProcessDpiAwareness        = shcoredpi.NewProc("SetProcessDpiAwareness")
)

const (
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 是个负的伪句柄 (-4)。
	// 以 uintptr 传要写补码，不能写 -4。
	dpiAwarenessPerMonitorV2 = ^uintptr(3)

	// PROCESS_PER_MONITOR_DPI_AWARE
	processPerMonitorDPIAware = 2
)

// enableDPIAwareness 声明进程的 DPI 感知级别，返回实际生效的那一级（失败返回空串）。
func enableDPIAwareness() string {
	// Win10 1703+
	if result, _, _ := procSetProcessDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2); result != 0 {
		return "Per-Monitor V2"
	}
	// Win8.1+，返回的是 HRESULT，0 = S_OK
	if result, _, _ := procSetProcessDpiAwareness.Call(processPerMonitorDPIAware); result == 0 {
		return "Per-Monitor"
	}
	// Vista+
	if result, _, _ := procSetProcessDPIAware.Call(); result != 0 {
		return "System"
	}
	return ""
}
