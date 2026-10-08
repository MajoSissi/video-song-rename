package naming

import (
	"encoding/json"
	"regexp"
	"strings"
)

// 这个文件是「怎么把识别结果拼成文件名」的**唯一实现**。
//
// 为什么不再用「一个模板字符串 + 事后清洗」那套：
//
// 老做法是先做字符串替换，再回头把空槽留下的分隔符擦掉。擦的时候必须猜——
// 哪些横杠是用户故意写的、哪些是空槽残留的。这个猜法必然有边界情况：
// 上个月就是因为「折叠连续分隔符」和「收尾裁剪」各用了一份分隔符清单，
// 对不上，于是 `{artist}/{title:en}/{title:zh}` 在中文缺失时留下尾部的 `/`。
// 补清单只是把洞堵上，换一种写法还会漏。
//
// 换成槽位之后，规则只有一句：**把非空的槽位拼起来，中间插分隔符**。
// 中文歌名没识别到 → 这一格根本不存在 → 结构上就不可能多出那个杠。
// 不是「这次擦干净了」，是「不会有东西需要擦」。

// 变量名。前端下拉框里选的、配置里存的，都是这几个字符串。
const (
	VarTitle    = "title"
	VarArtist   = "artist"
	VarAlbum    = "album"
	VarYear     = "year"
	VarOriginal = "original"
	// VarLiteral 是一段固定文字，不随识别结果变。
	// 用来写死名字，或者在名字里加个固定标签。
	VarLiteral = "literal"
)

// Slot 是模板里的一格。
//
// 渲染时这一格先取到值（变量取值，或者 literal 的固定文字），
// **值为空就整格消失** —— 连它的前缀、后缀、和它前面那个分隔符一起消失。
// 这条「一起消失」是关键：`{title} ({year})` 在年份未知时必须是 "Song"，
// 而不是 "Song ("。所以括号得挂在这一格上，不能当成独立的一段文字。
type Slot struct {
	// Var 是取值来源，见上面的 Var* 常量。
	Var string `json:"var"`
	// Script 只对 title / artist 有意义：en/zh/ja/ko/other。
	// 空串表示「主标题」——就是 PickTitle 挑出来的那一个。
	Script string `json:"script,omitempty"`
	// Text 只对 literal 有意义。
	Text string `json:"text,omitempty"`
	// Prefix / Suffix 紧贴在值两侧，值空了就一起消失。
	Prefix string `json:"prefix,omitempty"`
	Suffix string `json:"suffix,omitempty"`
	// Separator 是**这一格前面**的分隔符。
	//
	// 用指针是为了区分两种「空」：
	//   nil  → 跟随模板的全局分隔符（界面上留空就是这个）
	//   &""  → 明确不要分隔符（`{title} ({year})` 里年份那格就是这种）
	// 两者不能用同一个零值表示，否则「歌名 (年份)」会变成「歌名 - (年份)」。
	Separator *string `json:"separator,omitempty"`
}

// Template 是一份完整的命名规则。
type Template struct {
	// Separator 是全局分隔符，给没单独设分隔符的槽位用。
	Separator string `json:"separator"`
	// Slots 按顺序渲染。
	Slots []Slot `json:"slots"`
}

// DefaultSeparator 是新模板的默认分隔符。
const DefaultSeparator = " - "

// separatorFor 返回这一格该用的分隔符。
func (t Template) separatorFor(slot Slot) string {
	if slot.Separator != nil {
		return *slot.Separator
	}
	return t.Separator
}

// Value 取这一格的值。取不到返回空串。
func (s Slot) Value(f Fields) string {
	switch s.Var {
	case VarTitle:
		if s.Script == "" {
			return f.Title
		}
		return f.Titles[scriptFromName(s.Script)]
	case VarArtist:
		if s.Script == "" {
			return f.Artist
		}
		return f.Artists[scriptFromName(s.Script)]
	case VarAlbum:
		return f.Album
	case VarYear:
		return f.Year
	case VarOriginal:
		return f.Original
	case VarLiteral:
		return s.Text
	}
	return ""
}

// Render 把模板填成最终的主干名。
//
// 顺序即优先级：先写的排前面。空槽直接跳过，所以「跳过一个」不会
// 在结果里留下任何痕迹——没有多余的分隔符，也没有多余的空格。
func (t Template) Render(f Fields) string {
	var out strings.Builder
	written := false

	for _, slot := range t.Slots {
		value := strings.TrimSpace(slot.Value(f))
		if value == "" {
			continue
		}
		if written {
			out.WriteString(t.separatorFor(slot))
		}
		out.WriteString(slot.Prefix)
		out.WriteString(value)
		out.WriteString(slot.Suffix)
		written = true
	}

	if !written {
		return ""
	}
	// 两端不该有空白。中间的空白是用户自己写的分隔符，一律不动——
	// 老代码把连续空白压成一个空格，那是在替用户改他的分隔符。
	return strings.TrimSpace(out.String())
}

// IsZero 报告这份模板是不是空的（没有可用槽位）。
func (t Template) IsZero() bool {
	for _, slot := range t.Slots {
		if slot.Var != "" {
			return false
		}
	}
	return true
}

// Equal 逐字段比较两份模板。
//
// 不能直接用 reflect.DeepEqual：Slot.Separator 是指针，两份内容一样但
// 各自取址的模板会被判成不等。而「配置里的模板是不是内置的那一份」
// 恰恰要靠这个判断（见 config.Load 的旧配置迁移）。
func (t Template) Equal(other Template) bool {
	if t.Separator != other.Separator || len(t.Slots) != len(other.Slots) {
		return false
	}
	for i := range t.Slots {
		a, b := t.Slots[i], other.Slots[i]
		if a.Var != b.Var || a.Script != b.Script || a.Text != b.Text ||
			a.Prefix != b.Prefix || a.Suffix != b.Suffix {
			return false
		}
		if (a.Separator == nil) != (b.Separator == nil) {
			return false
		}
		if a.Separator != nil && *a.Separator != *b.Separator {
			return false
		}
	}
	return true
}

// UnmarshalJSON 同时接受两种形状：
//
//	新：{"separator":" - ","slots":[…]}
//	旧："{artist} - {title}"      —— 老配置里 name_template 就是个字符串
//
// 老的读进来直接迁移成槽位，用户升级后不需要做任何事，也不会丢模板。
// 认不出来的形状原样交给默认解析器，让它报出正常的错误。
func (t *Template) UnmarshalJSON(data []byte) error {
	var body string
	if err := json.Unmarshal(data, &body); err == nil {
		*t = ParseBody(body)
		return nil
	}
	type plain Template
	return json.Unmarshal(data, (*plain)(t))
}

// scriptFromName 把配置里的语种名转成 Script。
//
// 认不出来就当成「主标题」（返回空 Script），因为 Titles[""] 本来就取不到值，
// 与其让它悄悄返回空字符串，不如让它走主标题那条路——用户至少能看到东西。
func scriptFromName(name string) Script {
	if name == "" {
		return ""
	}
	if script, ok := scriptAliases[strings.ToLower(name)]; ok {
		return script
	}
	return ""
}

// Normalize 把一份模板收敛到合法状态：丢掉空槽、补上默认分隔符、去掉多余的空白。
func (t *Template) Normalize() {
	if t.Separator == "" {
		t.Separator = DefaultSeparator
	}
	// 这里**不做 TrimSpace**：分隔符是用户显式给的，" - " 和 "-" 是两个不同的选择。

	cleaned := make([]Slot, 0, len(t.Slots))
	for _, slot := range t.Slots {
		slot.Var = strings.TrimSpace(slot.Var)
		if !knownVar(slot.Var) {
			continue
		}
		if slot.Var == VarLiteral && strings.TrimSpace(slot.Text) == "" {
			continue
		}
		if slot.Var != VarTitle && slot.Var != VarArtist {
			slot.Script = ""
		} else if slot.Script != "" && scriptFromName(slot.Script) == "" {
			slot.Script = ""
		}
		cleaned = append(cleaned, slot)
	}
	t.Slots = cleaned
}

func knownVar(name string) bool {
	switch name {
	case VarTitle, VarArtist, VarAlbum, VarYear, VarOriginal, VarLiteral:
		return true
	}
	return false
}

// ------------------------------------------------------------------ 旧模板迁移

// legacyPlaceholder 匹配旧模板字符串里的占位符。
var legacyPlaceholder = regexp.MustCompile(`\{(title|artist|album|year|original)(?::([a-z]+))?\}`)

// ParseBody 把旧的模板字符串（如 "{artist}-{title:en}-{title:zh}"）转成槽位。
//
// 规则只有一条：**两个占位符中间那段文字，要么是分隔符，要么是后一个占位符的前缀。**
//
//	"{artist} - {title}"    → " - " 全是分隔符字符 → 当作 title 的分隔符
//	"{title} ({year})"      → " (" 里有括号       → 当作 year 的前缀
//	                          末尾的 ")" 当作 year 的后缀
//
// 前缀挂在后一个槽位上（而不是前一个的后缀），是因为它得跟着**后一个**一起消失：
// 年份没识别到时，我们想要 "Song"，不是 "Song ("。
func ParseBody(body string) Template {
	body = strings.TrimSpace(body)
	if body == "" {
		return Template{}
	}

	matches := legacyPlaceholder.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		// 没有任何占位符：用户写死了名字。整段当成一个固定文字槽位。
		return Template{Separator: DefaultSeparator, Slots: []Slot{{Var: VarLiteral, Text: body}}}
	}

	slots := make([]Slot, 0, len(matches))
	// separators 记下每个「格间文字」用的分隔符，最后看它们是不是同一个。
	var gaps []string

	for i, match := range matches {
		name := body[match[2]:match[3]]
		script := ""
		if match[4] >= 0 {
			script = body[match[4]:match[5]]
		}
		slot := Slot{Var: name, Script: script}

		// 这一段占位符**前面**的文字：第一个是前导，其余是格间。
		start := 0
		if i > 0 {
			start = matches[i-1][1]
		}
		between := body[start:match[0]]

		switch {
		case i == 0:
			// 第一个占位符**前面**的文字，当成它的前缀。
			slot.Prefix = between
		case isSeparatorRun(between):
			// 整段都是分隔符字符：原样收下，**不能 TrimSpace**——
			// " - " 两边的空格是分隔符的一部分，去掉就变成 "-" 了。
			gap := between
			gaps = append(gaps, gap)
			slot.Separator = &gap
		default:
			// 不是分隔符（比如 "{title} ({year})" 里的 " ("）：
			// 挂成这一格的前缀，并且**明确不要分隔符**。
			// 只设前缀不设分隔符的话，它会继承全局值，渲染成 "Song -  (2017)"。
			slot.Prefix = between
			none := ""
			slot.Separator = &none
		}
		slots = append(slots, slot)
	}

	// 末尾剩下的文字当作最后一个槽位的后缀。
	if tail := body[matches[len(matches)-1][1]:]; tail != "" {
		slots[len(slots)-1].Suffix = tail
	}

	template := Template{Separator: DefaultSeparator, Slots: slots}

	// 所有格间分隔符都一样 → 提升成全局分隔符，槽位上不再各存一份。
	// 这样界面上的「全局分隔符」输入框对迁移过来的模板也是有意义的，
	// 用户改一下就能整体换掉，而不是发现每格都钉死了改不动。
	if len(gaps) > 0 && allSame(gaps) {
		template.Separator = gaps[0]
		for i := range template.Slots {
			template.Slots[i].Separator = nil
		}
	}

	return template
}

// isSeparatorRun 报告这段文字是不是「纯分隔符」（外加空白）。
//
// 空串也算："{a}{b}" 这种写法中间没有分隔符，属于合法的「不要分隔符」。
func isSeparatorRun(text string) bool {
	for _, r := range text {
		if r == ' ' || r == '\t' {
			continue
		}
		if !separatorSet[r] {
			return false
		}
	}
	return true
}

func allSame(values []string) bool {
	for _, value := range values {
		if value != values[0] {
			return false
		}
	}
	return true
}

// separatorSet 是 separators 的查表版本，给 isSeparatorRun 用。
var separatorSet = func() map[rune]bool {
	set := make(map[rune]bool, len(separators))
	for _, separator := range separators {
		for _, r := range separator {
			set[r] = true
		}
	}
	return set
}()

// ------------------------------------------------------------------ 界面用的目录

// Choice 是槽位下拉框里的一项：把「变量 + 语种」拍平成一个成品名。
//
// 为什么拍平：界面上原来是两个下拉框（先选「歌名」再选「中文」），
// 选一次位置要动两次手，而且「歌名 + 主标题」这种组合读起来才知道是什么。
// 现在一个下拉里直接写「中文歌名」，选一次就到位。
//
// 放在 Go 这边而不是写死在 HTML 里：变量名是前后端之间的约定字符串，
// 两处各写一份迟早会对不上（{title:zh} 写成 {title:cn} 这种）。
type Choice struct {
	Var    string `json:"var"`
	Script string `json:"script,omitempty"`
	Label  string `json:"label"`
	// Group 是下拉框里的分组名（<optgroup>）。带语种的变量有六个变体，
	// 不分组的会铺成一条很长的平表。
	Group string `json:"group"`
}

// choices 是唯一的权威目录。顺序即下拉框里的显示顺序。
//
// 没有「其他语种」这一项：它是 Classify 认不出文字种类时的**兜底分类**，
// 不是一种可以主动去查的语言 —— 它没有对应的 storefront，
// 选它只会让那一格永远取不到值，还白多一次请求。
var choices = []Choice{
	{Var: VarTitle, Label: "原版歌名", Group: "歌名"},
	{Var: VarTitle, Script: "en", Label: "英文歌名", Group: "歌名"},
	{Var: VarTitle, Script: "zh", Label: "中文歌名", Group: "歌名"},
	{Var: VarTitle, Script: "ja", Label: "日文歌名", Group: "歌名"},
	{Var: VarTitle, Script: "ko", Label: "韩文歌名", Group: "歌名"},

	{Var: VarArtist, Label: "艺人", Group: "艺人"},
	{Var: VarArtist, Script: "en", Label: "英文艺人", Group: "艺人"},
	{Var: VarArtist, Script: "zh", Label: "中文艺人", Group: "艺人"},
	{Var: VarArtist, Script: "ja", Label: "日文艺人", Group: "艺人"},
	{Var: VarArtist, Script: "ko", Label: "韩文艺人", Group: "艺人"},

	{Var: VarAlbum, Label: "专辑", Group: "其他"},
	{Var: VarYear, Label: "年份", Group: "其他"},
	{Var: VarOriginal, Label: "原文件名", Group: "其他"},
	{Var: VarLiteral, Label: "固定文字", Group: "其他"},
}

// Choices 返回槽位下拉框的选项。
func Choices() []Choice {
	return append([]Choice(nil), choices...)
}

// ChoiceLabel 返回某个「变量 + 语种」组合在界面上的名字。
//
// 找不到就退回变量名本身。界面用它显示历史模板里那种不在目录中的组合
// （比如手改过配置文件），总比显示一个空白下拉强。
func ChoiceLabel(variable, script string) string {
	for _, choice := range choices {
		if choice.Var == variable && choice.Script == script {
			return choice.Label
		}
	}
	if script == "" {
		return variable
	}
	return variable + ":" + script
}
