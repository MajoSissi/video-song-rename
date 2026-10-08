//go:build windows

package main

import (
	"testing"
	"unsafe"
)

// 这两个数字写错的话，Shell_NotifyIconW 会**静默**返回 0（不弹错误、不写日志），
// 表现就是「托盘图标没出现」，而原因极难反推。钉死它们。
func TestWindowsStructSizes(t *testing.T) {
	if got := unsafe.Sizeof(wndClassExW{}); got != 80 {
		t.Errorf("WNDCLASSEXW 在 x64 上应当是 80 字节，实际 %d", got)
	}
	if got := unsafe.Sizeof(notifyIconDataW{}); got != 976 {
		t.Errorf("NOTIFYICONDATAW（完整版）在 x64 上应当是 976 字节，实际 %d", got)
	}
}

// 窗口类必须带 CS_DBLCLKS，否则系统只送来两次 WM_LBUTTONUP，
// 永远不会有 WM_LBUTTONDBLCLK —— 双击打开窗口会静默失效。
func TestTrayWindowClassHasDoubleClick(t *testing.T) {
	class := trayWindowClass(0x1234, nil, 0x5678)

	if class.Style&csDblClks == 0 {
		t.Error("窗口类缺少 CS_DBLCLKS：双击会没反应")
	}
	if class.Size != uint32(unsafe.Sizeof(wndClassExW{})) {
		t.Errorf("cbSize 必须是结构体自身大小，实际 %d", class.Size)
	}
	if class.Instance != 0x1234 || class.WndProc != 0x5678 {
		t.Errorf("实例句柄或窗口过程没传进去：%+v", class)
	}
}

// 图标数据里 UTF-16 的填充必须保证 NUL 结尾，否则 Windows 会一直往后读。
func TestFillUTF16Terminates(t *testing.T) {
	var buf [8]uint16
	fillUTF16(buf[:], "abc")
	if buf[3] != 0 {
		t.Errorf("结尾没有 NUL：%v", buf)
	}
	if string(rune(buf[0])) != "a" || string(rune(buf[2])) != "c" {
		t.Errorf("内容不对：%v", buf)
	}

	// 超长要截断，且最后一格留 NUL
	var small [4]uint16
	fillUTF16(small[:], "abcdefgh")
	if small[3] != 0 {
		t.Errorf("截断后结尾不是 NUL：%v", small)
	}

	// 代理对（emoji）不能只填一半
	var pair [4]uint16
	fillUTF16(pair[:], "🎵🎵🎵")
	if pair[3] != 0 {
		t.Errorf("emoji 截断后结尾不是 NUL：%v", pair)
	}
	for index := 0; index < 3; index++ {
		if pair[index] >= 0xD800 && pair[index] <= 0xDBFF {
			// 高位代理后面必须跟低位代理
			if index+1 >= 4 || pair[index+1] < 0xDC00 || pair[index+1] > 0xDFFF {
				t.Errorf("代理对被切断了：%v", pair)
			}
		}
	}
}
