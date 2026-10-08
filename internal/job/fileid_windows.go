//go:build windows

package job

import (
	"encoding/hex"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileIDInfo 是 FILE_ID_INFO 的载荷。
//
// x/sys 里有 `windows.FileIdInfo`（那个常量 = 18），却没有对应的结构体，
// 所以自己声明一份。布局必须和 Win32 一字不差：
// 8 字节卷序列号，紧跟 16 字节 FileId，共 24 字节。
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

// fileIdentity 取一个文件的「身份」，改名字不会变。
//
// 这是「按唯一值认文件」的底座：只按文件名认文件的话，用户在资源管理器里
// 手动把文件改个名，还原就对不上了 —— 记录里的现名指向一个不存在的路径，
// 而文件明明就在旁边。卷序列号 + FileId 是文件在卷内的唯一身份，
// 改名不变，同卷内移动也不变。
//
// **失败一律返回空串，不返回错误。** 调用方拿不到身份时的退路是按路径匹配，
// 那条路一直有效；为一个「更好的匹配方式」取不到值而中断整个还原，
// 是把可选优化做成了硬依赖。
//
// 注意硬链接：同一份数据的两个名字共享同一个 FileId。媒体文件里几乎不会
// 出现，出现了也只是「还原时可能挑中另一个名字」，不会改错文件。
func fileIdentity(path string) string {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}

	// 只要 FILE_READ_ATTRIBUTES。用 GENERIC_READ 去开一个正被别的程序
	// 独占写着的视频文件会失败，而我们只是想问它的身份证号。
	handle, err := windows.CreateFile(
		name,
		windows.FILE_READ_ATTRIBUTES,
		// 三个 SHARE 都给，跟 os.Open 一样宽 —— 拦着别的程序读写这个文件，
		// 只为查个 ID，不值得。
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		// 不传 FILE_FLAG_BACKUP_SEMANTICS：那是为了打开目录，这里不需要。
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(handle)

	var info fileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return ""
	}

	// 卷序列号一起带上。不同卷上的两个文件理论上可能撞同一个 FileId，
	// 带上序列号之后就不可能了 —— 而代价只是字符串长一点。
	return strconv.FormatUint(info.VolumeSerialNumber, 16) + "-" + hex.EncodeToString(info.FileID[:])
}
