// Package media 处理文件名层面的所有事：识别媒体文件、清洗标题、算不冲突的新路径。
package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// windowsReserved 是 Windows 上不能用作文件名的设备名。带上扩展名也一样被拒，
// 所以只比较主干部分。
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

var (
	// 控制字符（含制表、换行）换成空格：它们是「排版」而不是「非法字符」，
	// 换成下划线会把 "a\tb" 变成难看的 "a_b"。
	controlChars  = regexp.MustCompile(`[\x00-\x1f]`)
	illegalChars  = regexp.MustCompile(`[<>:"/\\|?*]`)
	whitespaceRun = regexp.MustCompile(`\s+`)
	// 连续下划线只在相邻非法字符被替换时产生，压成一个。
	underscoreRun = regexp.MustCompile(`_{2,}`)
)

// SanitizeName 把一段文本变成合法的 Windows 文件名主干。
//
// maxRunes 是主干长度上限，超出会被截断并加省略号。Windows 的 255 上限按
// UTF-16 码元算，用字符数近似——中文与 emoji 都是单码元或双码元，留了余量。
func SanitizeName(name string, maxRunes int) string {
	cleaned := controlChars.ReplaceAllString(name, " ")
	cleaned = illegalChars.ReplaceAllString(cleaned, "_")
	cleaned = whitespaceRun.ReplaceAllString(cleaned, " ")
	cleaned = underscoreRun.ReplaceAllString(cleaned, "_")
	cleaned = strings.TrimSpace(cleaned)
	// Windows 不接受结尾的点或空格。
	cleaned = strings.TrimRight(cleaned, ". ")

	if maxRunes > 0 && utf8.RuneCountInString(cleaned) > maxRunes {
		runes := []rune(cleaned)
		cleaned = strings.TrimRight(string(runes[:maxRunes]), ". ")
		cleaned = strings.TrimSpace(cleaned) + "…"
	}

	if cleaned == "" {
		return "Unknown"
	}
	if windowsReserved[strings.ToUpper(cleaned)] {
		return cleaned + "_"
	}
	return cleaned
}

// Extensions 把扩展名列表变成小写集合，方便查表。
func Extensions(list []string) map[string]bool {
	set := make(map[string]bool, len(list))
	for _, ext := range list {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		set[ext] = true
	}
	return set
}

// IsMedia 判断这个文件名是不是要处理的媒体文件。
func IsMedia(name string, exts map[string]bool) bool {
	return exts[strings.ToLower(filepath.Ext(name))]
}

// File 是一个待处理的媒体文件。
type File struct {
	Path    string
	Name    string
	Ext     string
	Size    int64
	ModTime int64
}

// ErrNotMedia 表示这个文件不在受支持的扩展名列表里。
var ErrNotMedia = errors.New("不是受支持的媒体文件")

// ErrSkipped 表示这个文件被「跳过已处理」的规则挡下了。
var ErrSkipped = errors.New("已带未识别前缀，跳过")

// Scan 递归（或只扫一层）列出目录下的媒体文件。
//
// skipPrefix 命中的文件会被跳过，默认是 "0_"，也就是上一轮没识别出来的那些——
// 重跑时它们只会再失败一次，白白多打一轮请求。
//
// root 是单个文件时同样要过扩展名与前缀这两道过滤：拖一堆东西进来的时候，
// 夹在中间的 .txt / .jpg 不该混进队列。挡下时返回 ErrNotMedia / ErrSkipped
// 包着的错误，调用方据此告诉用户「为什么没进来」。
func Scan(root string, exts map[string]bool, recursive bool, skipPrefix string) ([]File, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("无法访问 %s: %w", root, err)
	}

	if !info.IsDir() {
		name := info.Name()
		if strings.HasPrefix(name, ".") {
			return nil, fmt.Errorf("%w: %s", ErrNotMedia, name)
		}
		if !IsMedia(name, exts) {
			return nil, fmt.Errorf("%w: %s", ErrNotMedia, name)
		}
		if skipPrefix != "" && strings.HasPrefix(name, skipPrefix) {
			return nil, fmt.Errorf("%w: %s", ErrSkipped, name)
		}
		return []File{{
			Path:    root,
			Name:    name,
			Ext:     filepath.Ext(name),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixMilli(),
		}}, nil
	}

	var files []File
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// 单个子目录没权限不该让整次扫描失败。
			return nil
		}
		if entry.IsDir() {
			if path != root && !recursive {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if skipPrefix != "" && strings.HasPrefix(name, skipPrefix) {
			return nil
		}
		if !IsMedia(name, exts) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files = append(files, File{
			Path:    path,
			Name:    name,
			Ext:     filepath.Ext(name),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixMilli(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("扫描失败: %w", err)
	}
	return files, nil
}

// UniquePath 返回一个当前不存在的目标路径，冲突时依次加 " (1)"、" (2)"。
//
// reserved 里放本次任务已经预定出去的目标路径：同一批里有两条同名曲目时，
// 磁盘上还没生成文件，光看文件系统会算出同一个名字。
func UniquePath(dir, base, ext string, reserved map[string]bool) string {
	candidate := filepath.Join(dir, base+ext)
	if !exists(candidate) && !reserved[strings.ToLower(candidate)] {
		return candidate
	}
	for counter := 1; counter < 10000; counter++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, counter, ext))
		if !exists(candidate) && !reserved[strings.ToLower(candidate)] {
			return candidate
		}
	}
	// 理论上到不了这里；真到了也不能返回一个会覆盖别人的路径。
	return filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, os.Getpid(), ext))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// SamePath 判断两个路径是否指向同一个文件，忽略大小写。
func SamePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
