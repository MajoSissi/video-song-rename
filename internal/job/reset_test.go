package job

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"videosongrename/internal/config"
	"videosongrename/internal/recognizer"
)

// 「中途停止之后没法整批重来」是用户报的原话。它由两件事凑成：
// 停止会把没轮到的条目标成「已停止」，而「开始」只认「待处理」和「失败」。
// 这两条用例分别钉住两个出口 —— 点「开始」能接着跑，点「重置」能整批重来。

func TestResumePicksUpManuallyStoppedItems(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"a.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("a.mp4")
	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}

	// 模拟「跑过一次、中途按了停止」：文件还在原地，状态是已停止。
	manager.mu.Lock()
	manager.items[0].Status = StatusCanceled
	manager.items[0].Message = "已停止"
	manager.recountLocked()
	manager.mu.Unlock()

	summary := run(t, manager, ScopeResume)
	if fake.callCount() != 1 {
		t.Fatalf("「已停止」的条目该被「开始」重新捡起来，实际只请求了 %d 次", fake.callCount())
	}
	if summary.Matched != 1 {
		t.Errorf("重新跑完之后该有 1 条识别成功，实际 %d", summary.Matched)
	}
}

// 重置之后，一排状态各异（已改名 / 已归档 / 已停止 / 失败 / 跳过）的条目
// 必须全部回到「待处理」。少清一种，界面上就会留下一排洗不掉的颜色。
func TestResetPutsEveryStatusBackToPending(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	for _, name := range []string{"a.mp4", "b.mp4", "c.mp4", "d.mp4", "e.mp4", "f.mp4"} {
		write(name)
	}
	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}

	statuses := []Status{
		StatusRenamed, StatusMoved, StatusCanceled, StatusFailed, StatusSkipped, StatusRateLimited,
	}
	manager.mu.Lock()
	for index, status := range statuses {
		manager.items[index].Status = status
		manager.items[index].Moved = status == StatusMoved
	}
	manager.recountLocked()
	manager.mu.Unlock()

	list, err := manager.Reset()
	if err != nil {
		t.Fatalf("Reset 失败: %v", err)
	}
	if len(list) != len(statuses) {
		t.Fatalf("重置不该丢条目：重置前 %d 个，重置后 %d 个", len(statuses), len(list))
	}
	for _, item := range list {
		if item.Status != StatusPending {
			t.Errorf("%s 重置后还是 %q，应该是 pending", item.Name, item.Status)
		}
	}

	progress := manager.Progress()
	if progress.Done != 0 || progress.Matched != 0 || progress.Renamed != 0 ||
		progress.Moved != 0 || progress.Failed != 0 || progress.Skipped != 0 ||
		progress.Unmatched != 0 || progress.RateLimited != 0 {
		t.Errorf("重置后计数该归零，实际 %+v", progress)
	}
	if progress.Total != len(statuses) {
		t.Errorf("Total 该还是 %d（清单没变），实际 %d", len(statuses), progress.Total)
	}
}

// 重置只清状态，不碰磁盘 —— 已改过名的文件还在它现在的名字上。
func TestResetLeavesFilesOnDiskAlone(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")
	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	before := listDir(t, dir)
	manager.mu.Lock()
	paths := make([]string, len(manager.items))
	for index := range manager.items {
		paths[index] = manager.items[index].Path
	}
	manager.mu.Unlock()

	if _, err := manager.Reset(); err != nil {
		t.Fatalf("Reset 失败: %v", err)
	}

	if after := listDir(t, dir); !sameNames(before, after) {
		t.Errorf("重置不该改磁盘：重置前 %v，重置后 %v", before, after)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for index := range manager.items {
		if manager.items[index].Path != paths[index] {
			t.Errorf("重置后路径变了：%q -> %q", paths[index], manager.items[index].Path)
		}
	}
}

// 识别结果也要清掉。留着的话详情面板还写着上一轮的歌名，而状态是「待处理」。
func TestResetClearsPerItemResult(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")
	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	// 这一轮可能改过名，文件名变了；换一个一定存在的条目来断言。
	manager.mu.Lock()
	manager.items[0].Message = "识别中…"
	manager.items[0].Attempts = 2
	manager.items[0].Moved = true
	manager.mu.Unlock()

	list, err := manager.Reset()
	if err != nil {
		t.Fatalf("Reset 失败: %v", err)
	}
	item := list[0]
	if item.Title != "" || item.Artist != "" || item.Album != "" || item.ShazamURL != "" {
		t.Errorf("重置后不该还留着识别结果，实际 title=%q artist=%q album=%q url=%q",
			item.Title, item.Artist, item.Album, item.ShazamURL)
	}
	if item.Titles != nil || item.Artists != nil {
		t.Errorf("重置后多语种结果该清空，实际 %v / %v", item.Titles, item.Artists)
	}
	if item.Message != "" || item.Attempts != 0 || item.NewName != "" {
		t.Errorf("重置后残留了上一轮的运行痕迹：message=%q attempts=%d newName=%q",
			item.Message, item.Attempts, item.NewName)
	}
	// Moved 例外：文件确实还在归档目录里，清掉的话重跑会把它再搬一次。
	if !item.Moved {
		t.Error("Moved 不该被重置清掉 —— 清了之后重跑会算出「归档目录的归档目录」")
	}
}

// 重置之后重跑，已经归档的文件不能再搬一次。
//
// 这条挡的是静默的目录套娃：sibling 规则算的是「文件所在目录的父目录下，
// 建一个 <目录名>+后缀 的目录」。文件搬进 `X_已整理` 之后，规则会给它算出
// `X_已整理_已整理` —— 每重置一次深一层，而界面上只看到文件又动了。
func TestResetDoesNotFileArchivedFileTwice(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4":          matched("Back In Black", "AC/DC"),
		"Back In Black.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	cfg.Move = config.MoveRule{
		Enabled: true, Mode: config.MoveModeSibling, Suffix: "_已整理", MatchedOnly: true,
	}

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")
	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	first := run(t, manager, ScopePending)
	if first.Moved != 1 {
		t.Fatalf("第一轮该搬走 1 个，实际 %+v", first)
	}

	if _, err := manager.Reset(); err != nil {
		t.Fatalf("Reset 失败: %v", err)
	}
	second := run(t, manager, ScopeResume)

	if second.Moved != 0 {
		t.Errorf("已经归档过的文件不该再搬一次，实际又搬了 %d 个", second.Moved)
	}
	archived := archiveDir(t, dir, filepath.Base(dir)+"_已整理")
	if _, err := os.Stat(filepath.Join(archived, "Back In Black.mp4")); err != nil {
		t.Fatalf("文件应该还在第一次的归档目录里: %v（%v）", err, listDir(t, archived))
	}
	nested := archiveDir(t, dir, filepath.Base(dir)+"_已整理_已整理")
	if _, err := os.Stat(nested); err == nil {
		t.Errorf("套娃了：多出一个 %s", nested)
	}
}

// 重置之后要能整批再跑一遍 —— 连上一轮已经改名成功的那个也要重新识别。
// 这是「重置」和「开始」的分界线：开始只捡没跑完的，重置让所有条目都算没跑过。
func TestResetLetsFinishedBatchRunAgain(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"a.mp4": matched("Back In Black", "AC/DC"),
		"b.mp4": matched("Thunderstruck", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("a.mp4")
	write("b.mp4")
	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}

	run(t, manager, ScopePending)
	if got := fake.callCount(); got != 2 {
		t.Fatalf("第一轮该识别 2 个文件，实际 %d", got)
	}

	// 都跑完了，这时候点「开始」什么也不会发生（这是有意的：别白打请求）。
	// 它会返回一句「没有需要处理的文件」—— run() 遇到错就 Fatal，所以这里
	// 直接调，只关心它**没有**发出请求。
	if err := manager.Start(context.Background(), ScopeResume); err == nil {
		t.Log("全部成功之后没有可跑的条目，Start 报错是预期行为")
	}
	if got := fake.callCount(); got != 2 {
		t.Fatalf("全部成功之后点「开始」不该重新请求，实际累计 %d 次", got)
	}

	if _, err := manager.Reset(); err != nil {
		t.Fatalf("Reset 失败: %v", err)
	}
	// 重置之后状态全是 pending，连「已经改名成功」的那两个也算没跑过。
	if err := manager.Start(context.Background(), ScopeResume); err != nil {
		t.Fatalf("重置之后「开始」该能跑起来，实际 %v", err)
	}
	if got := fake.callCount(); got != 4 {
		t.Errorf("重置之后该把两个文件都重跑一遍（共 4 次请求），实际 %d 次", got)
	}
}

// 跑着的时候不允许重置：条目的下标正在被 worker 用着，中途清状态会让
// 同一行同时是「待处理」和「识别中」。
func TestResetRefusesWhileRunning(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	_, manager, _ := newHarness(t, baseConfig(), fake)

	manager.mu.Lock()
	manager.running = true
	manager.mu.Unlock()

	if _, err := manager.Reset(); err == nil {
		t.Fatal("运行中调用 Reset 该报错")
	}

	manager.mu.Lock()
	manager.running = false
	manager.mu.Unlock()
	if _, err := manager.Reset(); err != nil {
		t.Errorf("停下来之后 Reset 该成功，实际 %v", err)
	}
}

// sameNames 比两份文件名清单是否一致（listDir 已排过序，顺序也用不上）。
func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
