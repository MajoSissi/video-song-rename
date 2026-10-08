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

// Template 是命名模板库里的一项。
//
// 内嵌 naming.Template（分隔符 + 槽位），所以 JSON 里是
// {"name":…, "separator":…, "slots":[…]},没有额外一层嵌套。
type Template struct {
	// Name 是给人看的名字，也是这一项的标识——重名即视为「改这一项」。
	Name string `json:"name"`
	naming.Template
}

// UnmarshalJSON 兼容两种历史形状：
//
//	新：{"name":"…","separator":" - ","slots":[…]}
//	旧：{"name":"…","body":"{artist} - {title}"}
//
// 旧的 body 字符串直接迁移成槽位。不这么做的话，老配置里的 body 会被当成
// 未知键悄悄丢掉，模板变成空的，再被 Normalize 清理掉——用户升级后发现
// 自己攒的模板全没了，而且全程没有任何报错。
func (t *Template) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name      string        `json:"name"`
		Body      string        `json:"body"`
		Separator string        `json:"separator"`
		Slots     []naming.Slot `json:"slots"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	t.Name = raw.Name
	if len(raw.Slots) > 0 {
		t.Template = naming.Template{Separator: raw.Separator, Slots: raw.Slots}
		return nil
	}
	t.Template = naming.ParseBody(raw.Body)
	return nil
}

// BuiltinTemplates 返回内置模板。它们和用户自己加的一样可以被改、被删。
//
// 这里刻意用 ParseBody 从字符串构造，而不是手写 Slot 字面量：
// 内置模板本来就是「示例」，写成用户熟悉的模板串更看得出它长什么样，
// 顺带也把迁移路径本身跑了一遍。
func BuiltinTemplates() []Template {
	builtin := func(name, body string) Template {
		return Template{Name: name, Template: naming.ParseBody(body)}
	}
	return []Template{
		builtin("只歌名", "{title}"),
		builtin("歌名 - 艺人", "{title} - {artist}"),
		builtin("艺人 - 歌名", "{artist} - {title}"),
		builtin("歌名 (年份)", "{title} ({year})"),
		builtin("艺人 - 歌名 (年份)", "{artist} - {title} ({year})"),
		builtin("保持原文件名", "{original}"),
		// 中文用户最常见的一种：艺人 + 英文名 + 中文名。
		// 中文没识别到时，那一格连同分隔符一起消失，不会留下尾部的横杠。
		builtin("艺人 - 英文名 - 中文名", "{artist} - {title:en} - {title:zh}"),
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
	// Language 是请求的 Accept-Language。
	Language string `json:"language"`
	// Country 是接口国家代码。
	Country string `json:"country"`
	// TimeoutSeconds 是单次识别请求的超时。
	TimeoutSeconds int `json:"timeout_seconds"`
	// NameTemplate 是当前生效的命名规则。
	//
	// 它由 Templates 里 ActiveTemplate 那一项派生出来，界面不该直接改它——
	// 保留这个字段是为了让 job 层不必认识「模板库」这个概念。
	//
	// 类型是结构化的（分隔符 + 槽位），不再是模板字符串。JSON 的键没变，
	// 因为 naming.Template 自己认得出旧格式的字符串。
	NameTemplate naming.Template `json:"name_template"`
	// Templates 是可增删的模板库。
	Templates []Template `json:"templates"`
	// ActiveTemplate 是当前选中的模板名。
	ActiveTemplate string `json:"active_template"`
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

// ExtraLocales 返回当前命名模板还需要额外查询的「语言/国家」组合。
//
// 这**不是**一个配置项，而是从模板推导出来的，所以这里没有对应的字段。
// 理由见 naming.ExtraLocales 的注释：模板里用了哪个语种和要查哪些 storefront
// 本来就是同一件事，拆成两处设只会让人漏填，而漏填是静默失效。
//
// 旧配置里的 title_locales 键会被忽略（json.Unmarshal 不认未知字段不会报错），
// 下次保存就自然消失了，不需要写迁移代码。
func (c Config) ExtraLocales() []string {
	return naming.ExtraLocales(TemplateSpec(c.Templates, c.ActiveTemplate))
}

// UnmatchedNaming 返回未识别文件的前后缀。
func (c Config) UnmatchedNaming() naming.Unmatched {
	return naming.Unmatched{Prefix: c.UnmatchedPrefix, Suffix: c.UnmatchedSuffix}
}

// Default 返回一份默认设置。
func Default() Config {
	templates := BuiltinTemplates()
	return Config{
		SegmentSeconds:  12,
		Concurrency:     3,
		Retries:         2,
		Language:        "en-US",
		Country:         "GB",
		TimeoutSeconds:  20,
		NameTemplate:    templates[0].Template,
		Templates:       templates,
		ActiveTemplate:  templates[0].Name,
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

	normalizeTemplates(c, def)

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

// normalizeTemplates 把模板库收敛成一份自洽的状态。
//
// 三条不变量：库不为空、ActiveTemplate 指向库里真实存在的一项、NameTemplate 永远
// 等于当前选中项。最后一条是刻意的冗余——job 层只认 NameTemplate，
// 不该让它也去理解「模板库」这个概念。
func normalizeTemplates(c *Config, def Config) {
	seen := make(map[string]bool, len(c.Templates))
	cleaned := make([]Template, 0, len(c.Templates))
	for _, tpl := range c.Templates {
		name := strings.TrimSpace(tpl.Name)
		if name == "" || seen[name] {
			continue
		}
		tpl.Name = name
		tpl.Template.Normalize()
		// 一个槽位都不剩的模板直接丢掉。界面上也不允许存出这种模板
		// （SaveTemplate 会报错），但配置文件是可以手改的。
		if tpl.Template.IsZero() {
			continue
		}
		seen[name] = true
		cleaned = append(cleaned, tpl)
	}
	c.Templates = cleaned

	// 全删光时铺回内置的，免得界面上出现一个空模板库。
	if len(c.Templates) == 0 {
		c.Templates = append([]Template(nil), def.Templates...)
	}

	if !HasTemplate(c.Templates, c.ActiveTemplate) {
		c.ActiveTemplate = c.Templates[0].Name
	}
	c.NameTemplate = TemplateSpec(c.Templates, c.ActiveTemplate)
}

// HasTemplate 报告模板库里有没有叫这个名字的一项。
func HasTemplate(list []Template, name string) bool {
	for _, tpl := range list {
		if tpl.Name == name {
			return true
		}
	}
	return false
}

// TemplateSpec 取某个模板的渲染规则，没有就返回空模板。
func TemplateSpec(list []Template, name string) naming.Template {
	for _, tpl := range list {
		if tpl.Name == name {
			return tpl.Template
		}
	}
	return naming.Template{}
}

// templateByName 按渲染规则反查名字，用来判断一份旧配置的模板是不是内置的那几种。
func templateByName(list []Template, spec naming.Template) string {
	for _, tpl := range list {
		if tpl.Template.Equal(spec) {
			return tpl.Name
		}
	}
	return ""
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

	// 旧版本只有一个 name_template，没有模板库。
	//
	// Default() 已经把模板库填成了内置的那几份，所以「JSON 里到底有没有 templates
	// 这个键」是唯一能区分新旧配置的信号。只看结构体是不是空的会被 Default 骗过去，
	// 结果是用户自己改过的模板被内置值静默顶掉——这种丢配置的 bug 不会有任何报错。
	//
	// name_template 的解析交给 naming.Template.UnmarshalJSON：它认得出旧格式
	// 的字符串，读进来就已经是槽位了。
	if !hasJSONKey(raw, "templates") {
		cfg.Templates = BuiltinTemplates()
		cfg.ActiveTemplate = ""
		legacy := cfg.NameTemplate
		if !legacy.IsZero() {
			if name := templateByName(cfg.Templates, legacy); name != "" {
				cfg.ActiveTemplate = name
			} else {
				cfg.Templates = append(cfg.Templates, Template{Name: "自定义", Template: legacy})
				cfg.ActiveTemplate = "自定义"
			}
		}
	}

	cfg.Normalize()
	return cfg, nil
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
