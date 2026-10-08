// Package naming 决定「用哪个标题」和「最终叫什么名字」。
package naming

import (
	"path/filepath"
	"strings"
	"unicode"

	"videosongrename/internal/media"
)

// Script 是标题用的书写系统。用它实现「英文优先于中文」这类偏好。
type Script string

const (
	ScriptEnglish  Script = "english"
	ScriptChinese  Script = "chinese"
	ScriptJapanese Script = "japanese"
	ScriptKorean   Script = "korean"
	ScriptOther    Script = "other"
)

// Classify 判断一段文本主要用哪种文字。
//
// 顺序有讲究：日文标题是汉字加假名，先看假名才能把它和纯中文标题分开；
// 反过来会被判成中文。拉丁字母按「字母中的占比」算，避免 "Track 01" 这种
// 被数字稀释；表意文字按「全部字符中的占比」算。
func Classify(text string) Script {
	if strings.TrimSpace(text) == "" {
		return ScriptOther
	}

	var han, kana, hangul, latin, letters, total int
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		switch {
		case unicode.In(r, unicode.Hiragana, unicode.Katakana):
			kana++
			letters++
		case unicode.In(r, unicode.Han):
			han++
			letters++
		case unicode.In(r, unicode.Hangul):
			hangul++
			letters++
		// 只认 ASCII 字母为「英文」。西里尔、希腊字母同样是 `IsLetter`，
		// 但按它们算会让 "Привет" 被判成英文标题。
		case r < 128 && unicode.IsLetter(r):
			latin++
			letters++
		case unicode.IsLetter(r):
			letters++
		}
	}

	if total == 0 || letters == 0 {
		return ScriptOther
	}

	ratio := func(n int, denominator int) float64 {
		if denominator == 0 {
			return 0
		}
		return float64(n) / float64(denominator)
	}

	switch {
	case ratio(kana, total) > 0.2:
		return ScriptJapanese
	case ratio(hangul, total) > 0.3:
		return ScriptKorean
	case ratio(latin, letters) > 0.7:
		return ScriptEnglish
	case ratio(han, total) > 0.3:
		return ScriptChinese
	default:
		return ScriptOther
	}
}

// PickTitle 按优先级从候选里挑一个。偏好列表里的脚本优先，都没命中就取第一个。
//
// 上游会同时给出顶层标题和 SONG 段落里的标题，两者可能是同一首歌的不同写法
// （原文名与译名），所以这里真的需要挑，而不是随便取一个。
func PickTitle(candidates []string, preference []string) string {
	unique := dedupe(candidates)
	if len(unique) == 0 {
		return ""
	}

	if len(preference) > 0 {
		for _, want := range preference {
			for _, candidate := range unique {
				if string(Classify(candidate)) == want {
					return candidate
				}
			}
		}
	}
	return unique[0]
}

// IndexByScript 把一串候选按书写系统归位，用于 {title:en} 这类占位符。
//
// primary 排在最前面，所以同一个脚本有多个候选时它会赢——它就是
// PickTitle 挑出来的那个「主标题」。
func IndexByScript(candidates []string, primary string) map[Script]string {
	out := map[Script]string{}
	for _, candidate := range dedupe(append([]string{primary}, candidates...)) {
		script := Classify(candidate)
		if _, taken := out[script]; taken {
			continue
		}
		out[script] = candidate
	}
	return out
}

func dedupe(values []string) []string {
	unique := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
}

// Fields 是渲染模板要用到的全部取值。
type Fields struct {
	Title    string
	Artist   string
	Album    string
	Year     string
	Original string

	// Titles / Artists 是同一个字段在不同书写系统下的写法，键是 Script。
	// 用它支撑 {title:en} / {artist:ko} 这类占位符。
	Titles  map[Script]string
	Artists map[Script]string
}

// scriptAliases 让语种能写短的。
//
// `{title:ko}` 比 `{title:korean}` 好打得多，而全称也留着——界面上写的是全称，
// 用户照着抄不会错。两种都认。
var scriptAliases = map[string]Script{
	"en": ScriptEnglish, "english": ScriptEnglish,
	"zh": ScriptChinese, "chinese": ScriptChinese,
	"ja": ScriptJapanese, "japanese": ScriptJapanese,
	"ko": ScriptKorean, "korean": ScriptKorean,
	"other": ScriptOther,
}

// scriptLocales 把「语种」映射到查询时要用的「语言/国家」组合。
//
// 右边这些不是随便挑的：Shazam 的 storefront 用 language/country 定位一套
// 本地化文案，这四个组合是实测能拿到对应文字写法的。
// 注意 ScriptOther 不在表里 —— 它是个「认不出来」的兜底分类，
// 没有对应的 storefront，拿它去查只会白跑一次请求。
var scriptLocales = map[Script]string{
	ScriptEnglish:  "en-US/US",
	ScriptChinese:  "zh-CN/CN",
	ScriptJapanese: "ja-JP/JP",
	ScriptKorean:   "ko-KR/KR",
}

// ExtraLocales 从一份模板推导出「还需要额外查哪些语种」。
//
// 以前这是设置页里一个手填的文本框，要用户自己记住「模板里写了 {title:ko}，
// 就得把 ko-KR/KR 填进去」。这两件事本来就该绑在一起：忘了填的后果是
// 那一格**永远是空的**，而界面上看不出任何异常——预览里那一段会安静地消失，
// 用户只会以为「这首歌就是没有韩文名」。
//
// 现在由模板本身决定：模板里用到哪个语种，就自动多查一次。
// 顺序写死（英→中→日→韩），保证同一份模板每次生成的命令行完全一样——
// 不然 map 的遍历顺序会让命令行预览每次都不一样，看着像有 bug。
func ExtraLocales(t Template) []string {
	needed := map[Script]bool{}
	for _, slot := range t.Slots {
		if slot.Var != VarTitle && slot.Var != VarArtist {
			continue
		}
		if script := scriptFromName(slot.Script); script != "" {
			needed[script] = true
		}
	}

	locales := make([]string, 0, len(needed))
	for _, script := range []Script{ScriptEnglish, ScriptChinese, ScriptJapanese, ScriptKorean} {
		if locale, ok := scriptLocales[script]; ok && needed[script] {
			locales = append(locales, locale)
		}
	}
	return locales
}

// separators 是「哪些字符算分隔符」，**只给解析器用**。
//
// 它不参与渲染。老实现拿它做「折叠连续分隔符」和「收尾裁剪」，
// 两处各用一份清单，对不上就漏出尾部的杠——换成槽位渲染之后，
// 空槽根本不产生分隔符，没有任何东西需要裁剪，那套逻辑整个删掉了。
//
// 现在只剩一个用处：把用户手写的 `{artist} - {title}` 这种老式模板
// 拆成槽位时，判断两段之间的那串文字是「分隔符」还是「前后缀」
// （见 template.go 的 isSeparatorRun）。
//
// 以前还有一份 `Separators()` 把它导给界面的快捷按钮，
// 按钮撤掉之后没人要了，那个导出函数一并删了。
var separators = []string{
	"-", "_", ".", "/", "|", ",", "+", "、", "・", "~",
	"－", "＿", "．", "／", "｜", "，", "＋", "〜", "～",
}

// Unmatched 描述「没识别出来时」怎么命名：前缀 + 原文件名 + 后缀。
//
// 用结构体而不是两个相邻的 string 参数，是因为 prefix 和 suffix 类型一样、
// 位置相邻，写成位置参数的话传反了编译器一声不吭 ——
// 而后果是 `0_song.mp4` 悄悄变成 `song0_.mp4`，得跑起来才看得见。
//
// 前后缀都留着是因为两种习惯都合理：前缀能让未识别的一批在文件管理器里
// 排在一起，后缀则不会挡住文件名的开头。默认只给前缀（"0_"）。
type Unmatched struct {
	Prefix string
	Suffix string
}

// Marked 判断一个文件名主干是不是**已经**带上了未识别的标记。
//
// 用来避免套娃：上一轮留下的 `0_song` 再跑一次不该变成 `0_0_song`。
//
// 空串不能当标记参与判断 —— strings.HasPrefix(s, "") 恒为真，
// 于是「把前缀清空」的用户会发现未识别的一个都不改名了。
// 这个坑很隐蔽：界面上看起来一切正常，只是那些文件纹丝不动。
func (u Unmatched) Marked(stem string) bool {
	if u.Prefix != "" && strings.HasPrefix(stem, u.Prefix) {
		return true
	}
	return u.Suffix != "" && strings.HasSuffix(stem, u.Suffix)
}

// Fallback 生成未识别文件的文件名主干。
func Fallback(unmatched Unmatched, original string) string {
	stem := strings.TrimSuffix(filepath.Base(original), filepath.Ext(original))
	return unmatched.Prefix + stem + unmatched.Suffix
}

// Apply 是「算出一个合法主干名」的收口：先渲染模板，再清洗。
func Apply(template Template, unmatched Unmatched, fields Fields, maxRunes int, matched bool) string {
	if !matched {
		return media.SanitizeName(Fallback(unmatched, fields.Original), maxRunes)
	}

	out := template.Render(fields)
	// 模板里没有可用槽位、或者所有槽位都取不到值时，至少别给出一个空名字——
	// 退回原文件名（去掉扩展名），总比把文件改成 ".mp4" 强。
	if out == "" {
		out = strings.TrimSuffix(filepath.Base(fields.Original), filepath.Ext(fields.Original))
	}
	return media.SanitizeName(out, maxRunes)
}
