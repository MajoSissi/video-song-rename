//go:build !windows

package single

// Instance 在非 Windows 上是空壳。本程序只发 Windows 版，
// 这个文件存在的意义只是让 main.go 不必自己加构建约束。
type Instance struct{}

// Acquire 在非 Windows 上永远成功。
func Acquire() (*Instance, bool) { return &Instance{}, true }

// Watch 是空操作。
func (i *Instance) Watch(show func()) {}
