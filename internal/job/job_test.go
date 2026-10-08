package job

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"videosongrename/internal/config"
	"videosongrename/internal/recognizer"
)

// fakeRecognizer 把「文件名 -> 结果」写成一张表，测试就不需要联网。
type fakeRecognizer struct {
	mu      sync.Mutex
	byName  map[string]recognizer.Result
	calls   int
	lastCfg recognizer.Options
}

func (f *fakeRecognizer) Recognize(_ context.Context, opts recognizer.Options) (recognizer.Result, error) {
	f.mu.Lock()
	f.calls++
	f.lastCfg = opts
	result := f.byName[filepath.Base(opts.Path)]
	f.mu.Unlock()

	if result.Status == "" {
		result.Status = recognizer.StatusNoMatch
	}
	return result, nil
}

func (f *fakeRecognizer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func matched(title, artist string) recognizer.Result {
	return recognizer.Result{OK: true, Status: recognizer.StatusMatched, Title: title, Artist: artist}
}

// newHarness 建一个临时目录 + 管理器，返回目录与「写文件」的助手。
func newHarness(t *testing.T, cfg config.Config, fake *fakeRecognizer) (string, *Manager, func(string)) {
	t.Helper()

	dir := t.TempDir()
	manager := New(cfg, fake, dir, Callbacks{})

	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, manager, write
}

func baseConfig() config.Config {
	cfg := config.Default()
	cfg.SkipProcessed = false
	cfg.Concurrency = 1
	cfg.Retries = 0
	return cfg
}

func run(t *testing.T, manager *Manager, scope Scope) Summary {
	t.Helper()

	var summary Summary
	manager.callbacks = Callbacks{OnDone: func(s Summary) { summary = s }}

	if err := manager.Start(context.Background(), scope); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	return summary
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		names = append(names, entry.Name())
	}
	return names
}

func TestMatchedFileIsRenamedToTitle(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Renamed != 1 || summary.Matched != 1 {
		t.Fatalf("应改名 1 个，得到 %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(dir, "Back In Black.mp4")); err != nil {
		t.Fatalf("目标文件不存在: %v，实际目录 %v", err, listDir(t, dir))
	}
}

func TestUnmatchedFileGetsPrefix(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("mystery.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Unmatched != 1 {
		t.Fatalf("应有 1 个未识别，得到 %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(dir, "0_mystery.mp4")); err != nil {
		t.Fatalf("应改名为 0_mystery.mp4，实际目录 %v", listDir(t, dir))
	}
}

// TestUnmatchedFileGetsSuffix 覆盖「只留后缀、不要前缀」。
//
// 这正是后缀存在的理由：前缀能让未识别的一批在文件管理器里排在一起，
// 但会挡住文件名的开头。有人就是想让原名还在最前面，一眼认得出是哪个文件。
func TestUnmatchedFileGetsSuffix(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	cfg := baseConfig()
	cfg.UnmatchedPrefix = ""
	cfg.UnmatchedSuffix = "_未识别"

	dir, manager, write := newHarness(t, cfg, fake)
	write("mystery.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	// 未识别不等于「改名成功」：Renamed 只统计识别到的那些。
	if summary.Unmatched != 1 || summary.Renamed != 0 {
		t.Fatalf("应有 1 个未识别、0 个识别成功改名，得到 %+v", summary)
	}

	names := listDir(t, dir)
	if len(names) != 1 || names[0] != "mystery_未识别.mp4" {
		t.Fatalf("应改名为 mystery_未识别.mp4，实际 %v", names)
	}
	// 名字开头必须还在——选后缀而不是前缀的动机就在这一点上。
	if !strings.HasPrefix(names[0], "mystery") {
		t.Fatalf("原名字被挡到了后面: %v", names)
	}
}

// TestUnmatchedFileGetsPrefixAndSuffix 前后缀同时给。
//
// 两个标记都要落到文件名上，顺序是「前缀 + 原名 + 后缀」。
// 前后缀类型一样、位置相邻，写反了编译器不会吭声——
// 后果是 0_mystery_未识别.mp4 变成 _未识别0_mystery.mp4。
func TestUnmatchedFileGetsPrefixAndSuffix(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	cfg := baseConfig()
	cfg.UnmatchedPrefix = "0_"
	cfg.UnmatchedSuffix = "_未识别"

	dir, manager, write := newHarness(t, cfg, fake)
	write("mystery.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	if _, err := os.Stat(filepath.Join(dir, "0_mystery_未识别.mp4")); err != nil {
		t.Fatalf("应改名为 0_mystery_未识别.mp4，实际目录 %v", listDir(t, dir))
	}
}

// TestAlreadyMarkedUnmatchedIsNotRenamedAgain 守「套娃」：标记不该被叠第二层。
//
// 三条子用例，因为标记可以是前缀、后缀、或两个都有。
// 第二条是新增后缀时最容易漏的：只配后缀的用户跑第二遍时，
// mystery_未识别 会被再改成 mystery_未识别_未识别，越跑越长。
//
// 这里刻意不走「跳过已处理」那条路（baseConfig 把 SkipProcessed 关了）：
// 状态文件丢了、或者用户换了台机器时，唯一还拦得住套娃的就是这个判断。
func TestAlreadyMarkedUnmatchedIsNotRenamedAgain(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		suffix  string
		already string
	}{
		{"只配前缀", "0_", "", "0_mystery.mp4"},
		{"只配后缀", "", "_未识别", "mystery_未识别.mp4"},
		{"前后缀都配", "0_", "_未识别", "0_mystery_未识别.mp4"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
			cfg := baseConfig()
			cfg.UnmatchedPrefix = tc.prefix
			cfg.UnmatchedSuffix = tc.suffix

			dir, manager, write := newHarness(t, cfg, fake)
			write(tc.already)

			if _, err := manager.Scan(dir); err != nil {
				t.Fatal(err)
			}
			summary := run(t, manager, ScopePending)

			if summary.Unmatched != 1 {
				t.Fatalf("仍应记 1 个未识别，得到 %+v", summary)
			}
			if got := listDir(t, dir); len(got) != 1 || got[0] != tc.already {
				t.Fatalf("已经带标记的文件不该再改名：期望 %q，实际 %v", tc.already, got)
			}
		})
	}
}

// TestUnmatchedWithNoMarkerLeavesFileAlone 前后缀都留空 = 「未识别的别动」。
//
// 这条守的后果比看上去严重：新名字和原名相同时，天真的实现会把新名字交给
// media.UniquePath 求一个不冲突的路径，而它只看目标是否存在——此刻**源文件
// 自己正占着那个名字**，于是它判定冲突并返回 `mystery (1).mp4`。
// 用户看到的是一个什么都没认出来的文件被莫名加上 " (1)"，而且像是正常改名。
//
// 上面「已经带标记」那一关拦不住它：没有标记，Marked 对任何名字都返回 false。
func TestUnmatchedWithNoMarkerLeavesFileAlone(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	cfg := baseConfig()
	cfg.UnmatchedPrefix = ""
	cfg.UnmatchedSuffix = ""

	dir, manager, write := newHarness(t, cfg, fake)
	write("mystery.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	// 仍然算「未识别」——用户需要知道这首歌没认出来，只是名字不用动。
	if summary.Unmatched != 1 {
		t.Fatalf("仍应记 1 个未识别，得到 %+v", summary)
	}
	if got := listDir(t, dir); len(got) != 1 || got[0] != "mystery.mp4" {
		t.Fatalf("不该动文件，实际 %v", got)
	}

	// 一个文件都没改过，撤销记录就该是空的。
	// 这条比「检查 from != to」更严：真出了上面那个 bug，记录会是
	// from=mystery.mp4 / to=mystery (1).mp4，两者并不相等，那种写法拦不住。
	history, err := manager.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("没有文件被改名，撤销记录不该有内容: %+v", history)
	}
}

// TestDryRunPreviewShowsSuffixedName 预览模式也要把后缀算出来给用户看。
//
// 预览里少一段，用户就没法在真改名之前发现后缀配错了——而预览的全部意义
// 就是「先看一眼再决定」。
func TestDryRunPreviewShowsSuffixedName(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	cfg := baseConfig()
	cfg.UnmatchedPrefix = ""
	cfg.UnmatchedSuffix = "_未识别"
	cfg.DryRun = true

	dir, manager, write := newHarness(t, cfg, fake)
	write("mystery.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	items := manager.Items()
	if items[0].Status != StatusPreview || items[0].NewName != "mystery_未识别.mp4" {
		t.Fatalf("预览结果不对: %+v", items[0])
	}
	if got := listDir(t, dir); len(got) != 1 || got[0] != "mystery.mp4" {
		t.Fatalf("预览模式不该真的改名，实际 %v", got)
	}
}

// TestTemplateDecidesWhichLocalesGetQueried 钉住「模板 → 额外语种 → 识别器」这条链。
//
// 额外语种现在是**派生值**，不再是设置项：以前要用户自己在设置页手填一份
// ko-KR/KR，忘了填的后果是模板里那一格永远是空的，而界面上看不出任何异常。
// 两个包各自单测都过、拼起来却没接上，正是这种派生关系最典型的失败方式。
func TestTemplateDecidesWhichLocalesGetQueried(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	// 这份内置模板同时用到英文名和中文名，所以两套 storefront 都该被查。
	cfg.ActiveTemplate = "艺人 - 英文名 - 中文名"
	cfg.Normalize()

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	want := []string{"en-US/US", "zh-CN/CN"}
	got := fake.lastCfg.Locales
	if len(got) != len(want) {
		t.Fatalf("期望查 %v，实际 %v", want, got)
	}
	// 顺序也要对：map 的遍历顺序会让命令行每次不一样，看着像有 bug，
	// 所以 naming.ExtraLocales 把顺序写死成英→中→日→韩。
	for index, locale := range want {
		if got[index] != locale {
			t.Fatalf("第 %d 项期望 %q，实际 %q（完整 %v）", index, locale, got[index], got)
		}
	}
}

func TestAlreadyCorrectNameIsLeftAlone(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"Back In Black.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("Back In Black.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Renamed != 0 {
		t.Fatalf("文件名已经对了就不该再改，得到 %+v", summary)
	}
	if got := listDir(t, dir); len(got) != 1 || got[0] != "Back In Black.mp4" {
		t.Fatalf("目录不该有变化，实际 %v", got)
	}
}

func TestTwoFilesMatchingTheSameTitleDoNotOverwrite(t *testing.T) {
	// 并发下两条不同来源的视频可能是同一首歌，目标名会撞上。
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"a.mp4": matched("Same Song", "Band"),
		"b.mp4": matched("Same Song", "Band"),
	}}
	cfg := baseConfig()
	cfg.Concurrency = 4 // 故意让两个 worker 同时跑

	dir, manager, write := newHarness(t, cfg, fake)
	write("a.mp4")
	write("b.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Renamed != 2 {
		t.Fatalf("两个都该改名，得到 %+v", summary)
	}

	names := listDir(t, dir)
	if len(names) != 2 {
		t.Fatalf("应当有两个文件，实际 %v", names)
	}
	hasPlain, hasCounter := false, false
	for _, name := range names {
		switch name {
		case "Same Song.mp4":
			hasPlain = true
		case "Same Song (1).mp4":
			hasCounter = true
		}
	}
	if !hasPlain || !hasCounter {
		t.Fatalf("应当一个占原名、一个带 (1)，实际 %v", names)
	}
}

func TestDryRunLeavesDiskUntouched(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	cfg.DryRun = true

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Renamed != 0 {
		t.Fatalf("预览模式不该真的改名，得到 %+v", summary)
	}
	if got := listDir(t, dir); len(got) != 1 || got[0] != "clip.mp4" {
		t.Fatalf("预览模式目录不该变化，实际 %v", got)
	}

	// 但新名字要算出来给用户看。
	items := manager.Items()
	if items[0].Status != StatusPreview || items[0].NewName != "Back In Black.mp4" {
		t.Fatalf("预览结果不对: %+v", items[0])
	}
}

func TestDecodeFailureIsNotRenamed(t *testing.T) {
	// 读不了的文件不该被当成「未识别」打上 0_ 前缀——它可能只是缺个解码器。
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"broken.mp4": {Status: recognizer.StatusUnsupported, Error: "Unrecognized format"},
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("broken.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Failed != 1 || summary.Unmatched != 0 {
		t.Fatalf("应记为失败而不是未识别，得到 %+v", summary)
	}
	if got := listDir(t, dir); len(got) != 1 || got[0] != "broken.mp4" {
		t.Fatalf("解码失败不该改名，实际 %v", got)
	}
}

func TestUndoRestoresOriginalNames(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	result, err := manager.Undo()
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if result.Restored != 1 {
		t.Fatalf("应还原 1 个，得到 %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "clip.mp4")); err != nil {
		t.Fatalf("原名应当回来了，实际目录 %v", listDir(t, dir))
	}
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// TestUndoOneRestoresOnlyThatFile 单条还原只动那一个文件。
//
// 这是「记录原文件名」这个需求的核：一批改错了，用户往往只想改回其中几个。
// 以前只有「撤销全部」，为了救一个文件得把整批都还原回去，再重跑一遍。
func TestUndoOneRestoresOnlyThatFile(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4":  matched("Back In Black", "AC/DC"),
		"other.mp4": matched("Thunderstruck", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")
	write("other.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	result, err := manager.UndoOne(filepath.Join(dir, "Back In Black.mp4"))
	if err != nil {
		t.Fatalf("单条还原失败: %v", err)
	}
	if result.Restored != 1 {
		t.Fatalf("应还原 1 个，得到 %+v", result)
	}

	names := listDir(t, dir)
	if !contains(names, "clip.mp4") {
		t.Errorf("clip.mp4 没回来，实际 %v", names)
	}
	if !contains(names, "Thunderstruck.mp4") {
		t.Errorf("没点它的那个不该被动，实际 %v", names)
	}
	if contains(names, "Back In Black.mp4") {
		t.Errorf("还原过的现名不该还在，实际 %v", names)
	}
}

// TestUndoOneKeepsTheRecord 还原过之后记录仍然在，只是标成「已还原」。
//
// 记录**不删**是刻意的：「记录原文件名」本身就是那一页的用处。
// 删掉的话，用户过两天想查「这文件原来叫什么」就查不到了。
// 防重复撤销改用标记，不再靠删文件。
func TestUndoOneKeepsTheRecord(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	target := filepath.Join(dir, "Back In Black.mp4")
	if _, err := manager.UndoOne(target); err != nil {
		t.Fatalf("单条还原失败: %v", err)
	}

	history, err := manager.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("记录不该被删掉，实际 %+v", history)
	}
	if !history[0].Restored {
		t.Errorf("这条应当标成「已还原」: %+v", history[0])
	}
	if history[0].From == "" || history[0].To == "" {
		t.Errorf("还原之后原名和现名都要留着，否则查不到: %+v", history[0])
	}

	if _, err := manager.UndoOne(target); err == nil {
		t.Error("同一个文件不该能还原两次 —— 第二次点应当明确报错")
	}
}

// TestUndoKeepsTheRecordSoItCanStillBeRead 撤销全部之后记录也还在。
//
// 老实现撤完就把 undo.jsonl 删了（注释写的是「免得重复撤销」）。
// 加了单条还原之后那个做法就不成立了：删掉等于把「原文件名」一起删掉，
// 而用户恰恰是回头来查这个的。
func TestUndoKeepsTheRecordSoItCanStillBeRead(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	if _, err := manager.Undo(); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}

	history, err := manager.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !history[0].Restored {
		t.Fatalf("撤销后记录应当还在、且标成已还原，实际 %+v", history)
	}

	// 再撤一次：确实没得撤了。要说出来，而不是静悄悄「还原 0 个」——
	// 那种反馈和「撤销成功但没生效」长得一模一样。
	if _, err := manager.Undo(); err == nil {
		t.Error("已经没有可撤销的记录，第二次撤销应当报错")
	}
}

// TestUndoReplaysChainedRenamesInReverse 链式改名必须倒着还原。
//
// A→B→C 得先做 C→B、再做 B→A。顺序反过来的话第一步就撞上
// 「原名 B 已被占用」，两条都还原不了 —— 而且报的是「跳过」，
// 用户看到的是「撤销完了，但文件没动」。
func TestUndoReplaysChainedRenamesInReverse(t *testing.T) {
	dir, manager, write := newHarness(t, baseConfig(), &fakeRecognizer{byName: map[string]recognizer.Result{}})
	write("a.mp4")

	// 手工造一条链，绕过识别流程 —— 这里要测的是还原顺序，不是识别。
	for _, step := range [][2]string{{"a.mp4", "b.mp4"}, {"b.mp4", "c.mp4"}} {
		from, to := filepath.Join(dir, step[0]), filepath.Join(dir, step[1])
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
		manager.recordUndo(from, to)
	}

	result, err := manager.Undo()
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if result.Restored != 2 || result.Skipped != 0 {
		t.Fatalf("两步都该还原成功，得到 %+v", result)
	}
	if names := listDir(t, dir); len(names) != 1 || names[0] != "a.mp4" {
		t.Fatalf("应当还原成 a.mp4，实际 %v", names)
	}
}

// TestUndoOneSkipsWhenTheOriginalNameIsTaken 原名被占了就跳过，
// 而且**不能**标成已还原。
//
// 「不标已还原」是关键：用户把占位的文件挪走之后应当还能再点一次。
// 标成已还原的话那颗按钮就没了，只能去手删记录文件 —— 一个界面上的死路。
func TestUndoOneSkipsWhenTheOriginalNameIsTaken(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	// 现名还在，但原名被另一个文件占了。
	write("clip.mp4")

	result, err := manager.UndoOne(filepath.Join(dir, "Back In Black.mp4"))
	if err != nil {
		t.Fatalf("「跳过」不该是个 error，它是正常结果: %v", err)
	}
	if result.Skipped != 1 || result.Restored != 0 {
		t.Fatalf("应记成跳过，得到 %+v", result)
	}
	if len(result.Details) == 0 {
		t.Error("跳过的原因要写进 details —— 界面上就靠它告诉用户为什么没还原")
	}

	history, err := manager.History()
	if err != nil {
		t.Fatal(err)
	}
	if history[0].Restored {
		t.Error("没还原成就别标成已还原，否则用户再也没法重试")
	}
}

// TestUndoOneReportsAMissingFile 现名那个文件被挪走/删了：要记成「找不到」。
//
// 和「跳过」分开报，因为用户能做的事不一样：找不到就是文件不在那儿了，
// 跳过是去把占位的文件挪开。
func TestUndoOneReportsAMissingFile(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	target := filepath.Join(dir, "Back In Black.mp4")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	result, err := manager.UndoOne(target)
	if err != nil {
		t.Fatalf("「找不到」不该是个 error: %v", err)
	}
	if result.Missing != 1 || result.Restored != 0 {
		t.Fatalf("应记成找不到，得到 %+v", result)
	}
}

// TestRecordUndoStoresTheFileIdentity 改名记录里要真的存下文件身份。
//
// 不存的话，整套「按唯一值认文件」会静悄悄地退化成按路径 ——
// 而且退化得最难发现：一切照常工作，只有用户手动改过名时才出问题。
// 所以这里比的是「存下来的那个值等于那个文件此刻的身份」，不是一个非空判断：
// 写个常量进去也能过非空判断。
func TestRecordUndoStoresTheFileIdentity(t *testing.T) {
	dir, manager, write := newHarness(t, baseConfig(), &fakeRecognizer{byName: map[string]recognizer.Result{}})
	write("a.mp4")

	from := filepath.Join(dir, "a.mp4")
	to := filepath.Join(dir, "b.mp4")
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	manager.recordUndo(from, to)

	records, err := manager.loadUndo()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("应当有 1 条记录，实际 %d 条", len(records))
	}
	if records[0].ID == "" {
		t.Fatal("记录里没有文件身份 —— 还原就只能按路径认文件了，这正是要修的那件事")
	}
	if want := fileIdentity(to); records[0].ID != want {
		t.Errorf("存下来的身份不是这个文件的：记录 %q，实际 %q", records[0].ID, want)
	}
}

// TestUndoFindsTheFileAfterAnOutsideRename 在程序外把文件改了名，还原照样找得到。
//
// 这就是第 6 条反馈问的那件事。原来只按文件名认文件：用户在资源管理器里
// 手动改一下名，还原就报「文件已不在」—— 而文件明明就在旁边。
//
// 现在记录里存了文件的身份（卷序列号 + FileId）。改名不改变身份，
// 所以「路径对不上、身份对得上」恰好说明它只是被改了名。
func TestUndoFindsTheFileAfterAnOutsideRename(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	renamed := filepath.Join(dir, "Back In Black.mp4")
	outside := filepath.Join(dir, "我手动改的名字.mp4")
	if err := os.Rename(renamed, outside); err != nil {
		t.Fatal(err)
	}

	// 记录页上那一行现在得是「可还原」而不是「已不在」—— 它决定用户
	// 能不能勾上这一条。看不到勾选框的话，文件还在也救不回来。
	history, err := manager.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !history[0].Exists {
		t.Fatalf("手动改过名的文件在记录页上应当仍是「可还原」，实际 %+v", history)
	}
	if history[0].NowPath != outside {
		t.Errorf("记录页应当指向文件真正的位置 %q，实际 %q", outside, history[0].NowPath)
	}

	result, err := manager.UndoOne(renamed)
	if err != nil {
		t.Fatalf("按身份找到的文件应当能还原: %v", err)
	}
	if result.Restored != 1 || result.Missing != 0 {
		t.Fatalf("应当还原成功，得到 %+v", result)
	}

	names := listDir(t, dir)
	if !contains(names, "clip.mp4") {
		t.Fatalf("应当改回 clip.mp4，实际 %v", names)
	}
	if contains(names, "我手动改的名字.mp4") {
		t.Errorf("手动改的那个名字应当已经不在了，实际 %v", names)
	}
	// 这一条要单独说：用户以为只是「还原」，程序其实先认出了一个被改过名的文件。
	// 不写进 details 的话，日志里看不出身份判断到底生效了没有 ——
	// 而「生效了没有」正是这个功能唯一值得看的地方。
	if len(result.Details) == 0 {
		t.Error("按身份认出文件这件事要写进 details")
	}
}

// TestUndoRefusesWhenTheNameIsNowADifferentFile 旧现名上换成了**另一个**文件时，
// 不能动它。
//
// 「文件被改过名」和「同名却是别人」是两件事。只看名字的话，一个恰好占用了
// 旧现名的无关文件会被改名 —— 它和这次改名一点关系都没有，而用户看到的
// 却是「还原成功」。
func TestUndoRefusesWhenTheNameIsNowADifferentFile(t *testing.T) {
	dir, manager, write := newHarness(t, baseConfig(), &fakeRecognizer{byName: map[string]recognizer.Result{}})
	write("a.mp4")

	from := filepath.Join(dir, "a.mp4")
	to := filepath.Join(dir, "b.mp4")
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	manager.recordUndo(from, to)

	// 造一个「b.mp4 已经是另一个文件」的局面。
	//
	// 占位文件**先建**，再删掉原来的 b.mp4 顶上去：先删再建的话，
	// NTFS 可能把刚空出来的那条 MFT 记录分给新文件，身份就撞上了 ——
	// 那样这条测试会变成偶发绿灯，比不写还糟。
	write("placeholder.mp4")
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "placeholder.mp4"), to); err != nil {
		t.Fatal(err)
	}

	result, err := manager.UndoOne(to)
	if err != nil {
		t.Fatalf("「跳过」不该是个 error，它是正常结果: %v", err)
	}
	if result.Skipped != 1 || result.Restored != 0 {
		t.Fatalf("现名换成了别的文件，应当跳过，得到 %+v", result)
	}
	if names := listDir(t, dir); !contains(names, "b.mp4") {
		t.Fatalf("那个无关文件不该被改名，实际 %v", names)
	}
}

// TestUndoStillWorksForRecordsWithoutIdentity 老记录（没有身份字段）照样能还原。
//
// undo.jsonl 是纯追加的，用户升级上来时里面全是没有 id 的行。那些记录只能
// 按路径认文件 —— 那正是这次改动**之前**的行为。不能因为有了更好的办法，
// 就把老记录变成还原不了的。
func TestUndoStillWorksForRecordsWithoutIdentity(t *testing.T) {
	dir, manager, write := newHarness(t, baseConfig(), &fakeRecognizer{byName: map[string]recognizer.Result{}})
	write("a.mp4")

	from := filepath.Join(dir, "a.mp4")
	to := filepath.Join(dir, "b.mp4")
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	// 手工写一行没有 id 的，就是升级前留下的那种记录。
	manager.appendUndo(undoEntry{From: from, To: to, TS: 1})

	result, err := manager.Undo()
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if result.Restored != 1 {
		t.Fatalf("没有身份的老记录也该能按路径还原，得到 %+v", result)
	}
	if names := listDir(t, dir); !contains(names, "a.mp4") {
		t.Fatalf("应当改回 a.mp4，实际 %v", names)
	}
}

// TestUndoManyRestoresOnlyTheChosenOnes 勾了哪几条就只还原哪几条。
//
// 这是批量还原的核心：一批改错了十个，用户往往只想改回其中两三个。
// 只要有一个没勾的文件被动了，这个功能就比「撤销全部」更危险 ——
// 撤销全部至少是可预期的，而「顺手多还原几个」用户根本不知道。
func TestUndoManyRestoresOnlyTheChosenOnes(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"one.mp4":   matched("Back In Black", "AC/DC"),
		"two.mp4":   matched("Thunderstruck", "AC/DC"),
		"three.mp4": matched("Hells Bells", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("one.mp4")
	write("two.mp4")
	write("three.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	result, err := manager.UndoMany([]string{
		filepath.Join(dir, "Back In Black.mp4"),
		filepath.Join(dir, "Hells Bells.mp4"),
	})
	if err != nil {
		t.Fatalf("批量还原失败: %v", err)
	}
	if result.Restored != 2 || result.Skipped != 0 || result.Missing != 0 {
		t.Fatalf("应当正好还原 2 个，得到 %+v", result)
	}

	names := listDir(t, dir)
	for _, want := range []string{"one.mp4", "three.mp4", "Thunderstruck.mp4"} {
		if !contains(names, want) {
			t.Errorf("%s 应当在，实际 %v", want, names)
		}
	}
	if contains(names, "Back In Black.mp4") || contains(names, "Hells Bells.mp4") {
		t.Errorf("还原过的现名不该还在，实际 %v", names)
	}
}

// TestUndoManyReplaysChainedRenamesInReverse 选中的几条里夹着链式改名时，
// 也必须倒着还原 —— 理由和 Undo() 完全一样。
//
// 拆成「前端循环调 UndoOne」就会丢掉这个顺序保证，所以后端才要收一个数组。
func TestUndoManyReplaysChainedRenamesInReverse(t *testing.T) {
	dir, manager, write := newHarness(t, baseConfig(), &fakeRecognizer{byName: map[string]recognizer.Result{}})
	write("a.mp4")

	for _, step := range [][2]string{{"a.mp4", "b.mp4"}, {"b.mp4", "c.mp4"}} {
		from, to := filepath.Join(dir, step[0]), filepath.Join(dir, step[1])
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
		manager.recordUndo(from, to)
	}

	result, err := manager.UndoMany([]string{
		filepath.Join(dir, "b.mp4"),
		filepath.Join(dir, "c.mp4"),
	})
	if err != nil {
		t.Fatalf("批量还原失败: %v", err)
	}
	if result.Restored != 2 || result.Skipped != 0 {
		t.Fatalf("链式两步都该还原成功，得到 %+v", result)
	}
	if names := listDir(t, dir); len(names) != 1 || names[0] != "a.mp4" {
		t.Fatalf("应当还原成 a.mp4，实际 %v", names)
	}
}

// TestUndoManyTouchesEachPathOnce 同一个现名对应好几条记录时，一批里只还原最新的那条。
//
// 列表是按「一条改名记录一行」铺的，所以同一个现名可能出现好几行；
// 用户全勾上是很自然的事。不去重的话，第二条必然撞上「原名已被占用」，
// 白白多出一串「跳过」—— 看着像批量还原出错了，其实什么都没错。
//
// 这里第二条记录的 from 是个根本不存在的文件，所以它的还原一定成功；
// 第一条如果也去试，b.mp4 已经被挪走了，必然记成「找不到」。
func TestUndoManyTouchesEachPathOnce(t *testing.T) {
	dir, manager, write := newHarness(t, baseConfig(), &fakeRecognizer{byName: map[string]recognizer.Result{}})
	write("a.mp4")

	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	manager.recordUndo(a, b)
	manager.recordUndo(filepath.Join(dir, "ghost.mp4"), b)

	result, err := manager.UndoMany([]string{b})
	if err != nil {
		t.Fatalf("批量还原失败: %v", err)
	}
	if result.Restored != 1 || result.Missing != 0 || result.Skipped != 0 {
		t.Fatalf("同一个现名只该还原一次，得到 %+v", result)
	}
}

// TestUndoManySaysSoWhenThereIsNothingToDo 勾的全是已还原过的：要报错。
//
// 勾了三行、按下去什么都没发生、日志里也没有，用户只会以为按钮坏了。
// 这里和 Undo() 的处理保持一致：说不出来就不装作做成了。
func TestUndoManySaysSoWhenThereIsNothingToDo(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	target := filepath.Join(dir, "Back In Black.mp4")
	if _, err := manager.UndoMany([]string{target}); err != nil {
		t.Fatalf("第一次批量还原应当成功: %v", err)
	}
	if _, err := manager.UndoMany([]string{target}); err == nil {
		t.Error("勾的都是已还原过的记录，第二次应当明确报错，而不是静悄悄返回「还原 0」")
	}
	if _, err := manager.UndoMany(nil); err == nil {
		t.Error("一个都没勾就按下去，应当报错")
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s: %v", path, err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestUndoFileIsAppendOnlyEventStream 钉住磁盘格式：一行一件事，**只追加，不回写**。
//
// 「崩溃最多丢掉最后一行」这个保证就落在这里。哪天有人把 recordUndo / markRestored
// 改成读—改—写（比如为了「把已还原的条目删掉」），这条会红 ——
// 那正是需要停下来想一想的时刻：改坏整份记录，比多留几行代价大得多。
func TestUndoFileIsAppendOnlyEventStream(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	path := filepath.Join(dir, config.UndoFileName)
	afterRename := readLines(t, path)
	if len(afterRename) != 1 {
		t.Fatalf("改一次名应当只写一行，实际 %d 行: %v", len(afterRename), afterRename)
	}

	var renamed undoEntry
	if err := json.Unmarshal([]byte(afterRename[0]), &renamed); err != nil {
		t.Fatalf("改名那行解析不了: %v", err)
	}
	if renamed.From == "" || renamed.To == "" || renamed.Restore != "" {
		t.Fatalf("改名那行该有 from / to、没有 restore，实际 %+v", renamed)
	}

	if _, err := manager.UndoOne(filepath.Join(dir, "Back In Black.mp4")); err != nil {
		t.Fatal(err)
	}

	afterUndo := readLines(t, path)
	if len(afterUndo) != 2 {
		t.Fatalf("还原应当是**追加**一行，实际 %d 行: %v", len(afterUndo), afterUndo)
	}
	if afterUndo[0] != afterRename[0] {
		t.Error("第一行被改写了 —— 这个文件必须是纯追加的")
	}

	var restored undoEntry
	if err := json.Unmarshal([]byte(afterUndo[1]), &restored); err != nil {
		t.Fatalf("还原那行解析不了: %v", err)
	}
	if restored.Restore == "" || restored.From != "" || restored.To != "" {
		t.Fatalf("还原那行该只有 restore，实际 %+v", restored)
	}
	if restored.Restore != renamed.To {
		t.Errorf("还原行指的应当是被还原的那个 to：%q vs %q", restored.Restore, renamed.To)
	}
}

func TestRescanSkipsAlreadyProcessedFiles(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	cfg := baseConfig()
	cfg.SkipProcessed = true

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)
	first := fake.callCount()

	// 重新扫描同一个目录：改名后的文件应当被状态文件认出来并跳过。
	items, err := manager.Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != StatusSkipped {
		t.Fatalf("重扫后应标记为跳过，实际 %+v", items)
	}

	// 再点一次开始，不该产生任何新的网络请求——无论是被直接拒绝，
	// 还是跑了一轮但全被跳过。
	_ = manager.Start(context.Background(), ScopePending)
	if fake.callCount() != first {
		t.Fatalf("跳过的文件不该再被识别：调用数 %d -> %d", first, fake.callCount())
	}
}

func TestStartPendingDoesNotReprocessFinishedItems(t *testing.T) {
	// 这是原 Python 脚本的一个毛病：跑第二遍会把所有文件重新识别一次。
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched("Back In Black", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)
	after := fake.callCount()

	if err := manager.Start(context.Background(), ScopePending); err == nil {
		t.Fatal("没有待处理条目时应当返回错误，而不是重跑一遍")
	}
	if fake.callCount() != after {
		t.Fatalf("不该有新的识别调用：%d -> %d", after, fake.callCount())
	}
}

func TestAppendKeepsExistingItems(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("one.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}

	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "two.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := manager.Append(other)
	if err != nil {
		t.Fatal(err)
	}

	if len(items) != 2 {
		t.Fatalf("追加后应有 2 条，得到 %d", len(items))
	}
	// 索引必须连续，前端靠它定位表格行。
	if items[0].Index != 0 || items[1].Index != 1 {
		t.Fatalf("索引应当重排成 0,1，得到 %d,%d", items[0].Index, items[1].Index)
	}
}

func TestLongTitleIsTruncatedToFit(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": matched(strings.Repeat("Very Long ", 40), ""),
	}}
	cfg := baseConfig()
	cfg.MaxNameRunes = 30

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	if summary := run(t, manager, ScopePending); summary.Renamed != 1 {
		t.Fatalf("应当改名成功，得到 %+v", summary)
	}

	names := listDir(t, dir)
	if len(names) != 1 {
		t.Fatalf("应当只有一个文件，实际 %v", names)
	}
	if runes := len([]rune(strings.TrimSuffix(names[0], ".mp4"))); runes > 31 {
		t.Fatalf("文件名主干应当被截到 30 个字符左右，实际 %d: %q", runes, names[0])
	}
}

func TestRetryableNetworkErrorIsReportedAsFailure(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": {Status: recognizer.StatusNetwork, Retryable: true, Error: "连接超时"},
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Failed != 1 {
		t.Fatalf("网络失败应记为失败，得到 %+v", summary)
	}

	// 失败项要能靠 ScopeFailed 单独重试。
	after := fake.callCount()
	fake.byName["clip.mp4"] = matched("Back In Black", "AC/DC")
	summary = run(t, manager, ScopeFailed)

	if summary.Renamed != 1 || fake.callCount() != after+1 {
		t.Fatalf("重试失败项应当只跑那一条，得到 %+v（调用 %d -> %d）",
			summary, after, fake.callCount())
	}
}

func TestConfigIsPassedToRecognizer(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	cfg := baseConfig()
	cfg.SegmentSeconds = 9
	cfg.Language = "zh-CN"
	cfg.Country = "CN"

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	if fake.lastCfg.SegmentSeconds != 9 || fake.lastCfg.Language != "zh-CN" || fake.lastCfg.Country != "CN" {
		t.Fatalf("设置没有传到识别器: %+v", fake.lastCfg)
	}
}

func TestSegmentWindowIsClampedBelowShazamLimit(t *testing.T) {
	// Shazam 对 ≥15 秒的窗口一律不返回结果，配置层必须挡住。
	cfg := config.Default()
	cfg.SegmentSeconds = 22
	cfg.Normalize()

	if cfg.SegmentSeconds > 14 {
		t.Fatalf("取样窗口应被收敛到 14 秒以内，实际 %d", cfg.SegmentSeconds)
	}
}
