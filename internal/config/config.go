// Package config 负责设置的默认值、读写与校验。
//
// 配置文件与日志都放在 exe 旁边，不写 %AppData%：这个工具是绿色便携的，
// 「配置在哪」不应该是个要猜的问题。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"videosongrename/internal/media"
	"videosongrename/internal/naming"
)

const (
	// ConfigFileName 是配置文件名，放在 data/ 里。
	ConfigFileName = "video-song-rename.json"
	// LogFileName 是日志文件名，放在 data/ 里。
	LogFileName = "video-song-rename.log"
	// UndoFileName 记录每一次改名，用于撤销。
	UndoFileName = "video-song-rename.undo.jsonl"
	// StateFileName 记录已处理过的文件，避免重复烧网络。
	StateFileName = "video-song-rename.state.json"
)

// TitlePreference 是标题脚本的优先级。命中靠前的优先用。
var TitlePreference = []string{"english", "chinese", "korean", "japanese", "other"}

// DefaultNameTemplate 是没有任何配置时的命名规则。
//
// 只写歌名是刻意的：第一次用的人多半只想把文件名从「VID_20240103」换成
// 真正的歌名；艺人那一格等他确认识别确实准了，自己在「命名」页加上就行。
func DefaultNameTemplate() naming.Template {
	return naming.ParseBody("{title}")
}

// 移动模式。
const (
	// MoveModeSibling 在文件所在目录的**同级**建一个目录，名字默认与上级目录同名。
	//
	//	D:\视频\2024\a.mp4 → D:\视频\2024_已整理\a.mp4
	MoveModeSibling = "sibling"
	// MoveModeFixed 统一挪进一个固定目录。
	MoveModeFixed = "fixed"
)

// MoveRule 决定「处理成功的文件最后放在哪儿」。
//
// 为什么不是简单的「一个目标目录」：按日期或按批次建的目录结构里，
// 用户要的往往是「在这一层旁边再开一个收纳目录」，也就是模式 sibling。
// 每次都要手填一个绝对路径的话，一次处理十个不同文件夹就得填十遍。
type MoveRule struct {
	Enabled bool `json:"enabled"`
	// Mode 取 MoveMode*。留空按 sibling 处理。
	Mode string `json:"mode"`
	// Dir 是 fixed 模式的目标目录。相对路径按「文件所在目录」解析，
	// 这样「整理到同目录下的 sorted」也能写。
	Dir string `json:"dir"`
	// Name 非空时，sibling 模式用它代替上级目录名。
	Name string `json:"name"`
	// Prefix / Suffix 拼在目录名两侧，只对 sibling 模式生效。
	Prefix string `json:"prefix"`
	Suffix string `json:"suffix"`
	// MatchedOnly 为真时只移动识别成功的文件，未识别的留在原目录。
	//
	// 默认就是真：没识别出来的那些多半还要人工处理，把它们一起扫进归档目录，
	// 等于让用户去归档目录里找「哪些是失败的」——那正是这个功能想省掉的事。
	MatchedOnly bool `json:"matched_only"`
}

// FolderName 返回 sibling 模式下那个目录的名字（还没拼上级路径）。
// 返回空串表示「定不出一个合法的名字」，调用方应当认为不移动。
func (r MoveRule) FolderName(filePath string) string {
	base := strings.TrimSpace(r.Name)
	if base == "" {
		parent := filepath.Dir(filePath)
		base = filepath.Base(parent)
		// 文件直接放在盘符根目录（D:\x.mp4）时 Base 给回来的是 "\"，
		// 拿它当目录名会拼出一个叫「_」的目录。这种情况只能靠自定义名。
		if base == "" || base == "." || base == `\` || base == "/" ||
			base == string(filepath.Separator) {
			return ""
		}
	}
	name := strings.TrimSpace(r.Prefix + base + r.Suffix)
	if name == "" {
		return ""
	}
	return media.SanitizeName(name, 80)
}

// TargetDir 算出这个文件处理完之后该放进哪个目录。空串表示「不移动」。
func (r MoveRule) TargetDir(filePath string) string {
	if !r.Enabled {
		return ""
	}

	if r.Mode == MoveModeFixed {
		dir := strings.TrimSpace(r.Dir)
		if dir == "" {
			return ""
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(filePath), dir)
		}
		return filepath.Clean(dir)
	}

	folder := r.FolderName(filePath)
	if folder == "" {
		return ""
	}
	parent := filepath.Dir(filePath)
	// 「同级同名称」：目录的爸爸和文件的爸爸是同一个。
	return filepath.Join(filepath.Dir(parent), folder)
}

// Normalize 把移动规则收敛到合法取值。
//
// 不因为「参数不全」就把 Enabled 关掉：那样用户勾了开关、填了一半、
// 点保存，回来发现开关自己弹回去了，而且不知道是哪一项不合格。
// 定不出目标目录时由任务层如实报「跳过移动」并写进日志。
func (r *MoveRule) Normalize() {
	r.Dir = strings.TrimSpace(r.Dir)
	r.Name = strings.TrimSpace(r.Name)
	r.Prefix = strings.TrimSpace(r.Prefix)
	r.Suffix = strings.TrimSpace(r.Suffix)
	switch r.Mode {
	case MoveModeFixed, MoveModeSibling:
	default:
		r.Mode = MoveModeSibling
	}
}

// Config 是全部可调项。
type Config struct {
	// SegmentSeconds 是取样窗口。Shazam 对 ≥15 秒的片段一律不返回结果，默认 12。
	SegmentSeconds int `json:"segment_seconds"`
	// Concurrency 是同时识别的文件数。
	Concurrency int `json:"concurrency"`
	// Retries 是单个文件在「可重试错误」下的重试次数。
	Retries int `json:"retries"`
	// RequestIntervalMS 是两次 Shazam 请求之间的最小间隔（毫秒）。
	//
	// 决定会不会被限流的是**间隔**，不是并发数：并发 3 意味着三个 worker
	// 可能在同一秒里各发一次请求，服务端看到的就是一次三连突发。
	// 并发只影响「解码和等待能重叠几份」，发请求的节奏统一由这个间隔管。
	RequestIntervalMS int `json:"request_interval_ms"`
	// Language 是请求的 Accept-Language。
	Language string `json:"language"`
	// Country 是接口国家代码。
	Country string `json:"country"`
	// TimeoutSeconds 是单次识别请求的超时。
	TimeoutSeconds int `json:"timeout_seconds"`
	// NameTemplate 是**唯一**的命名规则，全局生效。
	//
	// 类型是结构化的（分隔符 + 槽位），不再是模板字符串。JSON 的键没变，
	// 因为 naming.Template 自己认得出旧格式的字符串。
	NameTemplate naming.Template `json:"name_template"`
	// Move 决定处理成功的文件最后放在哪儿。见 MoveRule。
	Move MoveRule `json:"move"`
	// UnmatchedPrefix 是未识别文件的前缀，默认 "0_"。
	UnmatchedPrefix string `json:"unmatched_prefix"`
	// UnmatchedSuffix 是未识别文件的后缀，默认空。
	//
	// 前缀让未识别的一批在文件管理器里排在一起；后缀不会挡住文件名开头，
	// 两种习惯都有人用，所以两个都留着。
	UnmatchedSuffix string `json:"unmatched_suffix"`
	// Extensions 是要处理的扩展名（小写，含点）。
	Extensions []string `json:"extensions"`
	// Recursive 决定是否处理子目录。
	Recursive bool `json:"recursive"`
	// SkipProcessed 决定是否跳过状态文件里记录过的文件。
	SkipProcessed bool `json:"skip_processed"`
	// DryRun 为真时只预览，不真的改名。
	DryRun bool `json:"dry_run"`
	// MaxNameRunes 是文件名（不含扩展名）的字符数上限。
	MaxNameRunes int `json:"max_name_runes"`
	// TitlePreference 是标题脚本优先级，顺序即优先级。
	TitlePreference []string `json:"title_preference"`
	// UserAgent 为空时用 sidecar 的内置值。
	UserAgent string `json:"user_agent"`
	// Proxy 是识别时走的代理。
	//
	// 三种取值，由 sidecar 负责解析：
	//
	//	留空   跟随系统环境变量（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）
	//	-      强制直连，连环境变量也不看（off / direct 同义）
	//	其它   代理地址，必须带协议头，例如 socks5://127.0.0.1:1080
	//
	// 「留空」与「-」必须分开：reqwest 默认就会读环境变量，如果只有留空一种
	// 表达方式，那么设了 HTTP_PROXY 的用户就永远绕不开那个变量——他既没有
	// 办法指定别的代理，也没有办法说「这次别用」。
	//
	// 这里只做去空格，不做校验：填错了要让 sidecar 明确报出来，
	// 在这一层悄悄丢掉一个拼错的地址，等于给用户一个静默失效的设置。
	Proxy string `json:"proxy"`
	// FFmpegPath 是 ffmpeg 可执行文件的路径。
	//
	// 留空表示自动探测：先找 PATH 上的 ffmpeg，再找程序同目录的 ffmpeg.exe。
	// 填了就用填的那个，探测失败也不再回退——「我明明填了它却用了别的」
	// 是个查不出来的问题。
	FFmpegPath string `json:"ffmpeg_path"`
	// CloseToTray 为真时点关闭按钮是收进托盘，而不是退出程序。
	CloseToTray bool `json:"close_to_tray"`
}

// ExtraLocales 返回命名规则还需要额外查询的「语言/国家」组合。
//
// 这**不是**一个配置项，而是从规则推导出来的，所以这里没有对应的字段。
// 理由见 naming.ExtraLocales 的注释：规则里用了哪个语种和要查哪些 storefront
// 本来就是同一件事，拆成两处设只会让人漏填，而漏填是静默失效。
//
// 旧配置里的 title_locales 键会被忽略（json.Unmarshal 不认未知字段不会报错），
// 下次保存就自然消失了，不需要写迁移代码。
func (c Config) ExtraLocales() []string {
	return naming.ExtraLocales(c.NameTemplate)
}

// UnmatchedNaming 返回未识别文件的前后缀。
func (c Config) UnmatchedNaming() naming.Unmatched {
	return naming.Unmatched{Prefix: c.UnmatchedPrefix, Suffix: c.UnmatchedSuffix}
}

// Default 返回一份默认设置。
func Default() Config {
	return Config{
		SegmentSeconds: 12,
		Concurrency:    3,
		Retries:        2,
		// 1.5 秒一次 = 每分钟 40 次。这是一条经验上的安全线：再快一点，
		// 队列一长就很容易在中途吃到 429，而吃到之后的冷却远比这点节流贵。
		RequestIntervalMS: 1500,
		Language:          "en-US",
		Country:           "GB",
		TimeoutSeconds:    20,
		NameTemplate:      DefaultNameTemplate(),
		Move: MoveRule{
			Mode:        MoveModeSibling,
			MatchedOnly: true,
		},
		UnmatchedPrefix: "0_",
		Extensions: []string{
			".mp4", ".mkv", ".avi", ".mov", ".flv", ".wmv", ".webm", ".m4v", ".ts",
			".mp3", ".wav", ".flac", ".m4a", ".aac", ".ogg", ".wma",
		},
		Recursive:       true,
		SkipProcessed:   true,
		DryRun:          false,
		MaxNameRunes:    120,
		CloseToTray:     true,
		TitlePreference: append([]string(nil), TitlePreference...),
	}
}

// Normalize 把外部传入的值收敛到合法范围。前端可以传任何东西过来，
// 这一层是唯一的下限保护。
func (c *Config) Normalize() {
	def := Default()

	if c.SegmentSeconds < 1 {
		c.SegmentSeconds = def.SegmentSeconds
	}
	// 15 秒以上 Shazam 不会返回结果，静默接受等于给用户一个坏默认值。
	if c.SegmentSeconds > 14 {
		c.SegmentSeconds = 14
	}
	if c.Concurrency < 1 {
		c.Concurrency = 1
	}
	if c.Concurrency > 16 {
		c.Concurrency = 16
	}
	if c.Retries < 0 {
		c.Retries = 0
	}
	if c.Retries > 10 {
		c.Retries = 10
	}
	// 请求间隔不给「关掉」这个选项：0 和负数一律当没填，回到默认值。
	// 「把节流关了」在界面上看着像性能优化，实际是把 429 的开关打开。
	if c.RequestIntervalMS <= 0 {
		c.RequestIntervalMS = def.RequestIntervalMS
	}
	if c.RequestIntervalMS < 300 {
		c.RequestIntervalMS = 300
	}
	// 上限 10 秒：再慢的话一百首的队列要跑二十分钟，用户会以为程序卡死了。
	if c.RequestIntervalMS > 10000 {
		c.RequestIntervalMS = 10000
	}
	if c.TimeoutSeconds < 5 {
		c.TimeoutSeconds = 5
	}
	if c.TimeoutSeconds > 300 {
		c.TimeoutSeconds = 300
	}
	if strings.TrimSpace(c.Language) == "" {
		c.Language = def.Language
	}
	if strings.TrimSpace(c.Country) == "" {
		c.Country = def.Country
	}
	if c.MaxNameRunes < 16 {
		c.MaxNameRunes = 16
	}
	if c.MaxNameRunes > 200 {
		c.MaxNameRunes = 200
	}
	if len(c.TitlePreference) == 0 {
		c.TitlePreference = def.TitlePreference
	}

	normalizeNaming(c, def)

	c.FFmpegPath = strings.TrimSpace(c.FFmpegPath)
	c.Proxy = strings.TrimSpace(c.Proxy)

	// 扩展名统一成小写带点的形式，顺手去重。
	seen := make(map[string]bool, len(c.Extensions))
	cleaned := make([]string, 0, len(c.Extensions))
	for _, ext := range c.Extensions {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		if seen[ext] {
			continue
		}
		seen[ext] = true
		cleaned = append(cleaned, ext)
	}
	if len(cleaned) == 0 {
		cleaned = def.Extensions
	}
	c.Extensions = cleaned
}

// normalizeNaming 把命名规则收敛到一份可用的状态。
//
// 规则只有两份，边界也只有两条：空规则退回默认，移动规则本身自洁。
// 「一个槽位都不剩」必须当成空规则处理 —— 否则渲染会得到空名字，
// naming.Apply 再退回原文件名，界面上看不出任何异常，只是所有名字都没变。
func normalizeNaming(c *Config, def Config) {
	c.NameTemplate.Normalize()
	if c.NameTemplate.IsZero() {
		c.NameTemplate = def.NameTemplate
	}
	c.Move.Normalize()
}

// DataDirName 是程序旁边放数据的目录名。
const DataDirName = "data"

// DataDir 返回数据目录：exe 同级的 data/。
//
// 配置、日志、状态、改名记录全部落在这里，不写 %AppData%——这个工具是绿色便携的，
// 「数据在哪」不该是个要猜的问题，整个目录拷走就是完整的迁移。
// exe 所在位置不可写时（比如装在 Program Files）退回当前工作目录下的 data/。
func DataDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), DataDirName)
		if ensureWritable(dir) {
			return dir
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		dir := filepath.Join(cwd, DataDirName)
		ensureWritable(dir)
		return dir
	}
	return DataDirName
}

func ensureWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe := filepath.Join(dir, ".write-probe")
	file, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false
	}
	_ = file.Close()
	_ = os.Remove(probe)
	return true
}

// Load 读取配置。文件不存在时返回默认值，不报错。
func Load(dir string) (Config, error) {
	cfg := Default()
	path := filepath.Join(dir, ConfigFileName)

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.Normalize()
			return cfg, nil
		}
		return cfg, fmt.Errorf("读取配置失败: %w", err)
	}

	// 用默认值做底再覆盖：旧版本的配置缺少新字段时不会变成零值。
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Default(), fmt.Errorf("解析配置失败（已回退默认值）: %w", err)
	}

	// 旧配置是「模板库 + 当前选中」，现在只留一份全局规则。
	//
	// 迁移要挑出用户**实际在用**的那一份：先看 active_template 指向哪一项，
	// 没有就取第一项。挑错的后果是升级之后命名规则悄悄变了 ——
	// 下一次跑任务，文件名和上一批不是同一种形状，而没有任何报错。
	//
	// name_template 本身的解析交给 naming.Template.UnmarshalJSON：
	// 它认得出旧格式的字符串，读进来就已经是槽位了。
	if spec, ok := migratedTemplate(raw); ok {
		cfg.NameTemplate = spec
	}

	cfg.Normalize()
	return cfg, nil
}

// migratedTemplate 从旧配置的模板库里挑出「当时在用的那一份」。
//
// 只在文件里真的有 templates 键时才动：新配置没有这个键，返回 ok=false，
// 调用方就保留 name_template 原值，不会被默认值顶掉。
func migratedTemplate(raw []byte) (naming.Template, bool) {
	if !hasJSONKey(raw, "templates") {
		return naming.Template{}, false
	}

	var legacy struct {
		Templates []struct {
			Name      string        `json:"name"`
			Body      string        `json:"body"`
			Separator string        `json:"separator"`
			Slots     []naming.Slot `json:"slots"`
		} `json:"templates"`
		Active string `json:"active_template"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return naming.Template{}, false
	}

	spec := func(item struct {
		Name      string        `json:"name"`
		Body      string        `json:"body"`
		Separator string        `json:"separator"`
		Slots     []naming.Slot `json:"slots"`
	}) naming.Template {
		if len(item.Slots) > 0 {
			return naming.Template{Separator: item.Separator, Slots: item.Slots}
		}
		return naming.ParseBody(item.Body)
	}

	// 先把选中项捞出来，再退回第一项。
	for _, item := range legacy.Templates {
		if item.Name == legacy.Active {
			return spec(item), true
		}
	}
	for _, item := range legacy.Templates {
		if candidate := spec(item); !candidate.IsZero() {
			return candidate, true
		}
	}
	return naming.Template{}, false
}

// hasJSONKey 判断原始 JSON 里有没有某个顶层键。用来区分「字段不存在」和「字段是零值」。
func hasJSONKey(raw []byte, key string) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	_, ok := probe[key]
	return ok
}

// Save 原子写回配置。
func Save(dir string, cfg Config) error {
	cfg.Normalize()

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	path := filepath.Join(dir, ConfigFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换配置失败: %w", err)
	}
	return nil
}
