//go:build windows

package winproc

import (
	"strings"
	"testing"
)

// 引号必须**只包住路径**，不能把 /select 一起包进去。
//
// 这条差别就是「定位」按钮的整个 bug：explorer 解析不出被引号裹住的 /select
// 开关，会退回到默认目录「文档」。本机 Win11 实测：
//
//	explorer /select,C:\...\Temp\vsr probe dir\song.mp4    → 新窗口「文档」
//	explorer /select,C:\...\Temp\vsrprobedir\song.mp4      → 新窗口「vsrprobedir」
//	explorer /select,"C:\...\Temp\vsr probe dir\song.mp4"  → 新窗口「vsr probe dir」
//
// 第二行说明**路径不带空格时一直是好的** —— 所以这个 bug 看起来像「有的能定位、
// 有的不能」，而不是「按钮坏了」。
//
// 而 os/exec 默认拼出来的正是第一行那种：参数含空格时，它把整个参数包进引号。
// 所以这里要求 SysProcAttr.CmdLine 非空：一旦有人退回
// `exec.Command("explorer", "/select,"+path)`，CmdLine 就是空的，这条变红。
func TestRevealCommandKeepsSelectOutsideTheQuotes(t *testing.T) {
	paths := []string{
		`C:\Users\a b\My Videos\song.mp4`, // 带空格：os/exec 会整段加引号的那种
		`D:\videos\a.mp4`,                 // 不带空格：os/exec 拼出来恰好是对的，容易让人以为没问题
		`D:\我的视频\歌 曲.mp4`,                 // 非 ASCII + 空格
	}

	for _, path := range paths {
		command := RevealCommand(path)

		if command.SysProcAttr == nil || command.SysProcAttr.CmdLine == "" {
			t.Fatalf("%s：没有手拼命令行 —— os/exec 会把整个 /select,路径 包进引号，"+
				"路径带空格时 explorer 会打开「文档」而不是文件所在目录", path)
		}

		line := command.SysProcAttr.CmdLine
		if want := `explorer /select,"` + path + `"`; line != want {
			t.Errorf("命令行不对\n  实际: %s\n  期望: %s", line, want)
			continue
		}

		// 下面两条盯的是「引号的位置」这一件事本身。上面那条全等断言换个写法
		// （比如加个 /e）就会红，而这一条不会 —— 它只在引号跑错位置时才红。
		argument := strings.TrimPrefix(line, "explorer ")
		if strings.HasPrefix(argument, `"`) {
			t.Errorf("%s：引号裹住了 /select，explorer 会退回到「文档」：%s", path, line)
		}
		if !strings.HasPrefix(argument, `/select,"`) {
			t.Errorf("%s：应该是 /select,\"路径\" 的形式，实际是 %s", path, line)
		}
	}
}
