//go:build !windows

package job

// fileIdentity 在非 Windows 平台上没有实现，一律返回空串。
//
// 返回空串不是「出错」，而是「这个平台没有这项能力」—— 调用方会自动退回
// 按路径匹配，也就是这项功能之前一直用的那套。整个项目目前只发布 Windows
// 版本（托盘、DPI、无边框窗口都是 Windows 专有的），这个文件只是为了让
// `GOOS=darwin go build` 之类的交叉检查不至于编不过。
func fileIdentity(string) string { return "" }
