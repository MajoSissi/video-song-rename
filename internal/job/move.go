package job

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"videosongrename/internal/config"
	"videosongrename/internal/media"
)

// 这个文件负责「处理完之后把文件搬走」。
//
// 顺序是**先改名、再移动**，而不是一次算好目标路径直接搬：
//   - 改名在本目录里做，失败的话文件还在原地、只是名字没变，用户很容易理解；
//   - 移动是另一件事（可能跨卷、可能建目录），它失败时上面那次改名已经发生了，
//     报错得说清楚「名字改了但没搬走」，而不是笼统一句「处理失败」。
//
// 两件事都做完才写一条还原记录（from = 最初路径，to = 最终路径）：
// 还原就是把文件从最终位置搬回最初位置，跨目录也照做。

// moveTo 把 from 这个文件挪进 targetDir，返回最终路径。
//
// from 是**调用方手里的当前路径**，不是条目里存的那份：改名刚刚发生过的话，
// 条目字段要等这一轮结束才更新，用条目里的旧路径会去找一个已经不存在的文件。
//
// 目标目录不存在就建；同名已有文件时按 " (1)" 让路，绝不覆盖 ——
// 这个功能是用来**整理**的，覆盖掉一个同名文件正好和它相反。
func (m *Manager) moveTo(from, targetDir string) (string, error) {
	if media.SamePath(targetDir, filepath.Dir(from)) {
		// 已经在目标目录里了。不返回错误：用户把归档目录设成了文件本来所在的
		// 那一层，是配置问题，但文件本身没有任何毛病。
		return from, nil
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", err
	}

	// 和 rename 共用一把锁：两个 worker 同时往同一个归档目录里搬同名文件时，
	// UniquePath 必须看到对方已经预定出去的那个名字。
	m.renameMu.Lock()
	defer m.renameMu.Unlock()

	name := filepath.Base(from)
	ext := filepath.Ext(name)
	target := media.UniquePath(targetDir, strings.TrimSuffix(name, ext), ext, m.reserved)
	if media.SamePath(target, from) {
		return from, nil
	}

	if err := moveFile(from, target); err != nil {
		return "", err
	}
	m.reserved[strings.ToLower(target)] = true
	return target, nil
}

// moveFile 先试改名，跨卷时退回「复制 + 删源」。
//
// Windows 上 os.Rename 跨卷会失败（类 Unix 是 EXDEV）。跨卷复制失败时把
// 半截的目标文件删掉：留一个 0 字节或写了一半的文件在归档目录里，
// 比直接报错更糟 —— 用户会以为搬成功了。
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}

	if err := copyFile(from, to); err != nil {
		_ = os.Remove(to)
		return err
	}
	if err := os.Remove(from); err != nil {
		return fmt.Errorf("已复制到目标目录，但原文件删不掉（可能被占用）：%w", err)
	}
	return nil
}

// copyFile 复制一个文件，尽量保住修改时间。
//
// 用 io.Copy 而不是自己开缓冲区：它在大文件上会走 sendfile / 内核拷贝。
func copyFile(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()

	info, err := source.Stat()
	if err != nil {
		return err
	}

	target, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		return err
	}
	// 复制完成之后再落一次盘：不 Sync 的话，紧接着删源文件、然后断电，
	// 有可能两边都没有完整内容。
	if err := target.Sync(); err != nil {
		target.Close()
		return err
	}
	if err := target.Close(); err != nil {
		return err
	}

	_ = os.Chtimes(to, info.ModTime(), info.ModTime())
	return nil
}

// moveTarget 返回「这个文件该搬去哪」，空串表示不搬。
//
// matched 为假（没识别出来）时还要看 MatchedOnly：默认配置下未识别的
// 留在原目录，等人工处理；只有用户明确要求「全搬走」才会一起挪。
func moveTarget(rule config.MoveRule, path string, matched bool) string {
	if !rule.Enabled {
		return ""
	}
	if !matched && rule.MatchedOnly {
		return ""
	}
	return rule.TargetDir(path)
}

// archiveTarget 是 moveTarget 的调用入口：已经搬进过归档目录的，不再搬。
//
// 这条判断是给「重置之后重跑一遍」用的。文件搬走之后，条目里记的路径已经是
// 归档目录里的那一份；再按同一套规则算一次，sibling 模式会给它算出一个
// 「归档目录的归档目录」—— 用户看到的是 `演唱会_已整理_已整理`，
// 而这种套娃每重置一次就深一层。
//
// 代价是「改了归档目录想让已归档的文件换过去」做不到，得先清空队列重新扫描。
// 这个取舍是有意的：套娃会静默地毁掉目录结构，而换目录有别的路可走。
func archiveTarget(rule config.MoveRule, item Item, matched bool) string {
	if item.Moved {
		return ""
	}
	return moveTarget(rule, item.Path, matched)
}

// landingDir 给预览用：有归档目录就用它，没有就是原目录。
func landingDir(target, dir string) string {
	if target != "" {
		return target
	}
	return dir
}

// previewMessage 拼预览模式那句说明。
func previewMessage(target, landing, newName string) string {
	if target == "" {
		return "将重命名为 " + newName
	}
	return "将放进 " + landing + "，命名为 " + newName
}

// moveMessage 拼一句「文件去哪了」，给任务列表的「说明」列用。
func moveMessage(target string) string {
	return "已移动到 " + filepath.Dir(target)
}
