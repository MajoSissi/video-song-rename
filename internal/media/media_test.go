package media

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeNameReplacesIllegalCharacters(t *testing.T) {
	cases := map[string]string{
		`AC/DC - Back in Black`: "AC_DC - Back in Black",
		// 相邻的非法字符压成一个下划线，不堆成一串。
		`What?: "Live" <2020>|*`: "What_ _Live_ _2020_",
		// 制表与换行当空白处理，不当非法字符。
		"a\tb\nc":          "a b c",
		"trailing dots...": "trailing dots",
		"  spaced  out  ":  "spaced out",
		"":                 "Unknown",
		"...":              "Unknown",
	}

	for input, want := range cases {
		if got := SanitizeName(input, 120); got != want {
			t.Errorf("SanitizeName(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

func TestSanitizeNameRefusesWindowsDeviceNames(t *testing.T) {
	// 这些名字在 Windows 上建不出文件，直接改名会失败。
	for _, name := range []string{"CON", "con", "PRN", "NUL", "COM1", "LPT9"} {
		got := SanitizeName(name, 120)
		if strings.EqualFold(got, name) {
			t.Errorf("SanitizeName(%q) = %q，应当被改写", name, got)
		}
	}
}

func TestSanitizeNameTruncatesOnRunesNotBytes(t *testing.T) {
	// 用字节截断会把一个汉字切成半个，得到非法 UTF-8。
	long := strings.Repeat("歌", 200)
	got := SanitizeName(long, 40)

	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长名字应当以省略号结尾，得到 %q", got)
	}
	if count := len([]rune(strings.TrimSuffix(got, "…"))); count != 40 {
		t.Errorf("截断后应有 40 个字符，实际 %d", count)
	}
}

func TestUniquePathAddsCounter(t *testing.T) {
	dir := t.TempDir()

	// 先占住目标名。
	if err := os.WriteFile(filepath.Join(dir, "Song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := UniquePath(dir, "Song", ".mp3", nil)
	if filepath.Base(got) != "Song (1).mp3" {
		t.Errorf("期望 Song (1).mp3，得到 %s", filepath.Base(got))
	}
}

func TestUniquePathHonoursReservations(t *testing.T) {
	dir := t.TempDir()

	// 磁盘上还没有这个文件，但同一批里已经有人预定了它。
	reserved := map[string]bool{
		strings.ToLower(filepath.Join(dir, "Song.mp3")): true,
	}

	got := UniquePath(dir, "Song", ".mp3", reserved)
	if filepath.Base(got) != "Song (1).mp3" {
		t.Errorf("被预定的路径不该再被返回，得到 %s", filepath.Base(got))
	}
}

func TestScanSkipsPrefixAndRespectsRecursion(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(root, "keep.mp4"))
	write(filepath.Join(root, "0_skip.mp4")) // 上一轮没识别出来的
	write(filepath.Join(root, "notes.txt"))  // 不是媒体
	write(filepath.Join(root, ".hidden.mp4"))
	write(filepath.Join(nested, "deep.mp3"))

	exts := Extensions([]string{".mp4", ".mp3"})

	recursive, err := Scan(root, exts, true, "0_")
	if err != nil {
		t.Fatal(err)
	}
	if len(recursive) != 2 {
		t.Fatalf("递归扫描应得到 2 个文件，得到 %d: %v", len(recursive), names(recursive))
	}

	flat, err := Scan(root, exts, false, "0_")
	if err != nil {
		t.Fatal(err)
	}
	if len(flat) != 1 {
		t.Fatalf("不递归应只得到 1 个文件，得到 %d: %v", len(flat), names(flat))
	}
}

// 点名一个文件时，扩展名与前缀这两道过滤**照样要过**。
//
// 拖一堆东西进来的时候，夹在中间的 .txt / .jpg 不该混进队列；带 0_ 前缀的
// 那些重跑一遍也只是再失败一次。挡下时要给出能区分原因的错误，界面才能
// 告诉用户「为什么没进来」——拖了没反应是最难自查的失败。
func TestScanFiltersASingleFile(t *testing.T) {
	root := t.TempDir()

	media := filepath.Join(root, "clip.mp4")
	if err := os.WriteFile(media, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := Scan(media, Extensions([]string{".mp4"}), true, "0_")
	if err != nil {
		t.Fatalf("正常媒体文件不该被挡: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("应当返回 1 条，得到 %d", len(files))
	}

	// 不在扩展名列表里的：要挡，而且错误要能被 errors.Is 认出来。
	notes := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(notes, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(notes, Extensions([]string{".mp4"}), true, "0_"); !errors.Is(err, ErrNotMedia) {
		t.Errorf("非媒体文件应当返回 ErrNotMedia，得到 %v", err)
	}

	// 带未识别前缀的：要挡。
	prefixed := filepath.Join(root, "0_already.mp4")
	if err := os.WriteFile(prefixed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(prefixed, Extensions([]string{".mp4"}), true, "0_"); !errors.Is(err, ErrSkipped) {
		t.Errorf("带 0_ 前缀的文件应当返回 ErrSkipped，得到 %v", err)
	}

	// 前缀关掉时（skipPrefix 为空）就该放行。
	if files, err := Scan(prefixed, Extensions([]string{".mp4"}), true, ""); err != nil || len(files) != 1 {
		t.Errorf("不启用跳过规则时应当放行，得到 %d 条 / %v", len(files), err)
	}

	// 隐藏文件挡掉。
	hidden := filepath.Join(root, ".hidden.mp4")
	if err := os.WriteFile(hidden, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(hidden, Extensions([]string{".mp4"}), true, ""); !errors.Is(err, ErrNotMedia) {
		t.Errorf("点文件应当返回 ErrNotMedia，得到 %v", err)
	}
}

func names(files []File) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Name)
	}
	return out
}
