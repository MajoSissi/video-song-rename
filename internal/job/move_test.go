package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"videosongrename/internal/config"
	"videosongrename/internal/recognizer"
)

// 这一组守住「处理完之后把文件搬走」。
//
// 它比改名多出三种失败面：目录要建、目标可能已有同名文件、可能跨卷。
// 每一条都对应一个真实后果 —— 覆盖掉一个同名文件、把文件搬丢、
// 或者搬完之后连还原记录都对不上。

// archiveDir 算出同级同名目录应当在哪。和实现里那条规则分开写一份，
// 是为了让用例真的能抓住「路径算错了」，而不是跟着实现一起错。
func archiveDir(t *testing.T, dir, folder string) string {
	t.Helper()
	return filepath.Join(filepath.Dir(dir), folder)
}

func TestMatchedFileIsMovedIntoNamingSiblingDir(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	cfg.Move = config.MoveRule{Enabled: true, Mode: config.MoveModeSibling, Suffix: "_已整理", MatchedOnly: true}

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Moved != 1 {
		t.Fatalf("应当搬走 1 个，得到 %+v", summary)
	}

	target := archiveDir(t, dir, filepath.Base(dir)+"_已整理")
	if _, err := os.Stat(filepath.Join(target, "Back In Black.mp4")); err != nil {
		t.Fatalf("归档目录里没有这个文件: %v（目录内容 %v）", err, listDir(t, target))
	}
	// 原目录里不该还留着一份 —— 那是「复制」而不是「移动」。
	if _, err := os.Stat(filepath.Join(dir, "clip.mp4")); err == nil {
		t.Error("原目录里还留着 clip.mp4，这不是移动")
	}

	// 条目要跟着落到新位置，不然界面上点的「打开所在目录」会去一个空目录。
	items := manager.Items()
	if items[0].Path != filepath.Join(target, "Back In Black.mp4") {
		t.Errorf("条目路径没跟着走：%q", items[0].Path)
	}
	if !items[0].Moved {
		t.Error("条目没标成已移动")
	}
}

// 只搬识别成功的：默认配置就是这个行为，未识别的要留在原目录等人工处理。
func TestUnmatchedFilesStayWhenMatchedOnly(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	cfg.Move = config.MoveRule{Enabled: true, Mode: config.MoveModeSibling, Suffix: "_已整理", MatchedOnly: true}

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")
	write("mystery.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	target := archiveDir(t, dir, filepath.Base(dir)+"_已整理")
	if _, err := os.Stat(filepath.Join(target, "Back In Black.mp4")); err != nil {
		t.Fatalf("识别成功的没被搬走: %v", err)
	}
	// 未识别的那个改了名（前缀），但仍然留在原目录。
	if _, err := os.Stat(filepath.Join(dir, "0_mystery.mp4")); err != nil {
		t.Fatalf("未识别的应当留在原目录，实际目录 %v", listDir(t, dir))
	}
}

// MatchedOnly = false 时，未识别的也一起搬走。
func TestUnmatchedFilesMoveWhenNotMatchedOnly(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	cfg := baseConfig()
	cfg.Move = config.MoveRule{Enabled: true, Mode: config.MoveModeSibling, Suffix: "_已整理", MatchedOnly: false}

	dir, manager, write := newHarness(t, cfg, fake)
	write("mystery.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	target := archiveDir(t, dir, filepath.Base(dir)+"_已整理")
	if _, err := os.Stat(filepath.Join(target, "0_mystery.mp4")); err != nil {
		t.Fatalf("未识别的也该搬走: %v（目录内容 %v）", err, listDir(t, target))
	}
}

// 固定目录模式：不管文件原来在哪，都进同一个目录。
func TestMoveIntoFixedDir(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dest := t.TempDir()
	cfg := baseConfig()
	cfg.Move = config.MoveRule{Enabled: true, Mode: config.MoveModeFixed, Dir: dest, MatchedOnly: true}

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	if _, err := os.Stat(filepath.Join(dest, "Back In Black.mp4")); err != nil {
		t.Fatalf("没进固定目录: %v（目录内容 %v）", err, listDir(t, dest))
	}
}

// 目标目录里已经有同名文件时，加 (1) 让路，绝不覆盖。
//
// 覆盖掉一个同名文件是「整理」功能最不能犯的错：用户看到的是「搬过去就少了一个」，
// 而且新搬进去的那个名字看着完全正常。
func TestMoveDoesNotOverwriteExistingFile(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	cfg.Move = config.MoveRule{Enabled: true, Mode: config.MoveModeSibling, Suffix: "_已整理", MatchedOnly: true}

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")

	target := archiveDir(t, dir, filepath.Base(dir)+"_已整理")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// 归档目录里先放一个同名但内容不同的文件。
	if err := os.WriteFile(filepath.Join(target, "Back In Black.mp4"), []byte("原来的"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	// 原来那份内容必须还在。
	raw, err := os.ReadFile(filepath.Join(target, "Back In Black.mp4"))
	if err != nil || string(raw) != "原来的" {
		t.Fatalf("归档目录里的同名文件被覆盖了：内容 %q，错误 %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(target, "Back In Black (1).mp4")); err != nil {
		t.Fatalf("新文件应当让路成 (1)，目录内容 %v", listDir(t, target))
	}
}

// 名字本来就是对的、只是要搬走 —— 状态不能写成「已重命名」。
func TestMoveOnlyGetsMovedStatus(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"Back In Black.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	cfg.Move = config.MoveRule{Enabled: true, Mode: config.MoveModeSibling, Suffix: "_已整理", MatchedOnly: true}

	dir, manager, write := newHarness(t, cfg, fake)
	write("Back In Black.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	item := manager.Items()[0]
	if item.Status != StatusMoved {
		t.Errorf("只搬不改名的条目应当是 %q，得到 %q（%s）", StatusMoved, item.Status, item.Message)
	}
	if summary.Renamed != 0 {
		t.Errorf("没改名却统计了 %d 个改名", summary.Renamed)
	}
	if summary.Moved != 1 {
		t.Errorf("应当统计 1 个移动，得到 %d", summary.Moved)
	}
}

// 搬完还能还原回原目录原名 —— 这是整个功能里最容易漏的一环。
//
// 还原记录里存的是「最初路径 → 最终路径」，跨目录也要能照原样走回去。
func TestUndoRestoresMovedFile(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	cfg.Move = config.MoveRule{Enabled: true, Mode: config.MoveModeSibling, Suffix: "_已整理", MatchedOnly: true}

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	moved := filepath.Join(archiveDir(t, dir, filepath.Base(dir)+"_已整理"), "Back In Black.mp4")
	result, err := manager.UndoOne(moved)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if result.Restored != 1 {
		t.Fatalf("应当还原 1 个，得到 %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "clip.mp4")); err != nil {
		t.Fatalf("没有还原回原路径: %v（目录内容 %v）", err, listDir(t, dir))
	}
	if _, err := os.Stat(moved); err == nil {
		t.Error("还原之后归档目录里不该还留着那个文件")
	}
}

// 预览模式：算出目标位置但不能碰磁盘。
func TestDryRunShowsMoveTargetWithoutTouchingDisk(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	cfg.DryRun = true
	cfg.Move = config.MoveRule{Enabled: true, Mode: config.MoveModeSibling, Suffix: "_已整理", MatchedOnly: true}

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	item := manager.Items()[0]
	want := filepath.Join(archiveDir(t, dir, filepath.Base(dir)+"_已整理"), "Back In Black.mp4")
	if item.NewPath != want {
		t.Errorf("预览位置算错了：得到 %q，期望 %q", item.NewPath, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "clip.mp4")); err != nil {
		t.Error("预览模式动了磁盘上的文件")
	}
	if _, err := os.Stat(filepath.Dir(want)); err == nil {
		t.Error("预览模式建出了归档目录")
	}
}

// 归档目录就是文件本来所在的那一层时，什么都不该发生。
func TestMoveIntoSameDirIsNoop(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"Back In Black.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	// 名字给空、去掉前后缀 → sibling 名字等于上级目录名，也就是它自己所在的目录。
	cfg.Move = config.MoveRule{
		Enabled:     true,
		Mode:        config.MoveModeSibling,
		Name:        filepath.Base("目标目录"),
		MatchedOnly: true,
	}
	dir, manager, write := newHarness(t, cfg, fake)
	// 让归档目录正好等于当前目录：Name 用当前目录名，且它就在上一级里。
	cfg.Move.Name = filepath.Base(dir)
	manager.SetConfig(cfg)
	write("Back In Black.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	item := manager.Items()[0]
	if !strings.EqualFold(item.Path, filepath.Join(dir, "Back In Black.mp4")) {
		t.Errorf("文件被搬到了别处：%q", item.Path)
	}
	if summary.Moved != 0 {
		t.Errorf("搬到自己所在目录不该算移动，得到 %d", summary.Moved)
	}
}

// 归档目录建不出来时（这里用一个「路径里塞了文件」的形状）要如实报失败，
// 而不是把条目留在「处理中」或者悄悄当成成功。
func TestMoveFailureIsReported(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	// 在目标目录的位置放一个**文件**：MkdirAll 到这里必然失败。
	blocker := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"_已整理")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(blocker) })

	cfg := baseConfig()
	cfg.Move = config.MoveRule{Enabled: true, Mode: config.MoveModeSibling, Suffix: "_已整理", MatchedOnly: true}
	manager.SetConfig(cfg)

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Failed != 1 {
		t.Fatalf("应当报 1 个失败，得到 %+v", summary)
	}
	item := manager.Items()[0]
	if !strings.Contains(item.Message, "移动失败") {
		t.Errorf("没说明是移动失败：%q", item.Message)
	}
	// 失败之后文件必须还在——改名成功了，只是没搬走。
	if _, err := os.Stat(item.Path); err != nil {
		t.Fatalf("文件不见了: %v", err)
	}
}
