package naming

import (
	"strings"
	"testing"
)

func TestClassifySeparatesJapaneseFromChinese(t *testing.T) {
	cases := map[string]Script{
		"Back In Black": ScriptEnglish,
		"Track 01":      ScriptEnglish,
		"你好世界":          ScriptChinese,
		"안녕하세요":         ScriptKorean,
		"こんにちは":         ScriptJapanese,
		"東京は夜の七時":       ScriptJapanese, // 汉字夹假名：先看假名才不会误判成中文
		"Hello 世界":      ScriptEnglish,
		"":              ScriptOther,
		"1234":          ScriptOther,
		"Привет":        ScriptOther,
	}

	for input, want := range cases {
		if got := Classify(input); got != want {
			t.Errorf("Classify(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestPickTitleFollowsPreferenceOrder(t *testing.T) {
	// 上游会同时给出原文名与译名，这里要真的按偏好挑。
	got := PickTitle([]string{"你好", "Hello"}, []string{"english", "chinese"})
	if got != "Hello" {
		t.Errorf("英文优先时应取 Hello，得到 %q", got)
	}

	got = PickTitle([]string{"你好", "Hello"}, []string{"chinese", "english"})
	if got != "你好" {
		t.Errorf("中文优先时应取 你好，得到 %q", got)
	}

	// 偏好里一个都没命中时退回第一个，不能返回空。
	got = PickTitle([]string{"你好", "Hello"}, []string{"korean"})
	if got != "你好" {
		t.Errorf("都没命中时应取第一个，得到 %q", got)
	}

	if got := PickTitle(nil, []string{"english"}); got != "" {
		t.Errorf("没有候选时应返回空串，得到 %q", got)
	}

	// 去重不能把顺序打乱。
	got = PickTitle([]string{"Hello", "Hello", "你好"}, []string{"english"})
	if got != "Hello" {
		t.Errorf("去重后应取 Hello，得到 %q", got)
	}
}

// slots 是构造测试模板的简写：默认全局分隔符 " - "。
func slots(items ...Slot) Template {
	return Template{Separator: " - ", Slots: items}
}

func TestRenderDropsEmptySlots(t *testing.T) {
	cases := []struct {
		name     string
		template Template
		title    string
		artist   string
		want     string
	}{
		{"单槽", slots(Slot{Var: VarTitle}), "Song", "Artist", "Song"},
		{"歌名在前", slots(Slot{Var: VarTitle}, Slot{Var: VarArtist}), "Song", "Artist", "Song - Artist"},
		// 艺人未知时不该留下一个孤零零的分隔符。
		{"歌名在前，艺人空", slots(Slot{Var: VarTitle}, Slot{Var: VarArtist}), "Song", "", "Song"},
		{"艺人在前，艺人空", slots(Slot{Var: VarArtist}, Slot{Var: VarTitle}), "Song", "", "Song"},
		// 前缀/后缀要跟着值一起消失：年份空时不能留下 "Song ("。
		{
			"带括号的年份，年份空",
			slots(Slot{Var: VarTitle}, Slot{Var: VarYear, Prefix: " (", Suffix: ")", Separator: new(string)}),
			"Song", "Artist", "Song",
		},
	}

	for _, c := range cases {
		got := c.template.Render(Fields{Title: c.title, Artist: c.artist, Original: "orig.mp4"})
		if got != c.want {
			t.Errorf("%s：渲染成 %q，期望 %q", c.name, got, c.want)
		}
	}
}

// 前缀和后缀必须**跟着值一起消失**。
//
// 这是「歌名 (年份)」能不能保住的关键：括号挂在年份那一格上，
// 年份取不到时整格（含括号）一起走。若把括号当成独立的固定文字槽位，
// 年份空的时候就会剩下一个孤零零的 "("。
func TestPrefixSuffixDieWithValue(t *testing.T) {
	template := slots(
		Slot{Var: VarTitle},
		Slot{Var: VarYear, Prefix: " (", Suffix: ")", Separator: new(string)},
	)

	withYear := Fields{Title: "Song", Year: "2017"}
	if got := template.Render(withYear); got != "Song (2017)" {
		t.Errorf("有年份时应是 %q，得到 %q", "Song (2017)", got)
	}

	withoutYear := Fields{Title: "Song"}
	if got := template.Render(withoutYear); got != "Song" {
		t.Errorf("没有年份时应是 %q，得到 %q", "Song", got)
	}
}

// 每格可以有自己的分隔符；留空（nil）就跟随全局。
func TestSlotSeparatorOverridesGlobal(t *testing.T) {
	// 用户想拼的：「艺人 - 英文歌名 / 中文歌名」。
	slash := " / "
	template := Template{
		Separator: " - ",
		Slots: []Slot{
			{Var: VarArtist},
			{Var: VarTitle, Script: "en"},
			{Var: VarTitle, Script: "zh", Separator: &slash},
		},
	}

	fields := Fields{
		Artist: "BTS",
		Titles: map[Script]string{ScriptEnglish: "Spring Day", ScriptChinese: "春日"},
	}
	if got := template.Render(fields); got != "BTS - Spring Day / 春日" {
		t.Errorf("混用分隔符时得到 %q，期望 %q", got, "BTS - Spring Day / 春日")
	}

	// 中文缺失：那一格连同它前面的 " / " 一起消失，不能留下尾巴。
	fields.Titles = map[Script]string{ScriptEnglish: "Spring Day"}
	if got := template.Render(fields); got != "BTS - Spring Day" {
		t.Errorf("中文缺失时得到 %q，期望 %q", got, "BTS - Spring Day")
	}
}

// 明确「不要分隔符」（Separator 指向空串）和「跟随全局」（nil）必须是两回事。
func TestEmptySeparatorIsNotInherit(t *testing.T) {
	none := ""
	template := slots(
		Slot{Var: VarTitle},
		Slot{Var: VarYear, Prefix: " (", Suffix: ")", Separator: &none},
	)

	got := template.Render(Fields{Title: "Song", Year: "2017"})
	if got != "Song (2017)" {
		t.Errorf("空分隔符被当成继承了，得到 %q，期望 %q", got, "Song (2017)")
	}
}

// 固定文字槽位不随识别结果变。
func TestLiteralSlotAlwaysRenders(t *testing.T) {
	template := slots(Slot{Var: VarLiteral, Text: "[MV]"}, Slot{Var: VarTitle})
	got := template.Render(Fields{Title: "Song"})
	if got != "[MV] - Song" {
		t.Errorf("得到 %q，期望 %q", got, "[MV] - Song")
	}
}

func TestApplyFallsBackToPrefixedOriginalName(t *testing.T) {
	got := Apply(ParseBody("{title}"), Unmatched{Prefix: "0_"}, Fields{Original: "my video.mp4"}, 120, false)
	if got != "0_my video" {
		t.Errorf("未识别时应当是 前缀+原文件名，得到 %q", got)
	}

	got = Apply(ParseBody("{title}"), Unmatched{Prefix: "0_"}, Fields{Title: "Real Song", Artist: "Band", Original: "my video.mp4"}, 120, true)
	if got != "Real Song" {
		t.Errorf("识别成功时应使用模板，得到 %q", got)
	}
}

// 所有槽位都取不到值时，不能给出空名字——退回原文件名。
func TestApplyFallsBackWhenEverySlotIsEmpty(t *testing.T) {
	template := slots(Slot{Var: VarTitle, Script: "zh"})
	got := Apply(template, Unmatched{Prefix: "0_"}, Fields{Original: "clip.mp4"}, 120, true)
	if got != "clip" {
		t.Errorf("全空时应退回原文件名，得到 %q", got)
	}
}

// 带语种的槽位：取不到就是空，空槽整个消失。
func TestRenderScriptSlots(t *testing.T) {
	fields := Fields{
		Title:  "Spring Day",
		Artist: "BTS",
		Titles: map[Script]string{
			ScriptEnglish: "Spring Day",
			ScriptKorean:  "봄날",
		},
		Artists: map[Script]string{
			ScriptEnglish: "BTS",
			ScriptKorean:  "방탄소년단",
		},
		Original: "clip.mp4",
	}

	template := slots(
		Slot{Var: VarArtist},
		Slot{Var: VarTitle, Script: "ko"},
		Slot{Var: VarTitle, Script: "en"},
	)
	if got := template.Render(fields); got != "BTS - 봄날 - Spring Day" {
		t.Errorf("得到 %q，期望 %q", got, "BTS - 봄날 - Spring Day")
	}

	// 换成韩文艺人 + 英文歌名。
	template = slots(Slot{Var: VarArtist, Script: "ko"}, Slot{Var: VarTitle, Script: "en"})
	if got := template.Render(fields); got != "방탄소년단 - Spring Day" {
		t.Errorf("得到 %q，期望 %q", got, "방탄소년단 - Spring Day")
	}
}

// TestRenderDropsEmptySlotsCleanly 守住「空槽位不能留下多余分隔符」。
//
// 这是 Majo 报的：模板写「艺人-英文歌名-中文歌名」，中文没识别到时
// 文件名末尾多出一根横杠。
//
// 老实现（字符串模板 + 事后擦洗）的根因是折叠和裁剪用了**两份对不上的清单**，
// 换一种分隔符写法还会漏。现在改成槽位渲染后，空槽压根不参与拼接，
// 这条测试的意义也变了：它不再是在验证「擦干净了」，而是在验证
// **每一种分隔符、每一个位置**都走同一条代码路径。
func TestRenderDropsEmptySlotsCleanly(t *testing.T) {
	// 只有英文写法：中/韩槽全空。
	onlyEnglish := Fields{
		Title: "Spring Day", Artist: "BTS", Original: "a.mp4",
		Titles:  map[Script]string{ScriptEnglish: "Spring Day"},
		Artists: map[Script]string{ScriptEnglish: "BTS"},
	}

	// 每种分隔符都要试：首槽空、中槽空、尾槽空、连着两个空。
	for _, separator := range separators {
		sep := separator
		build := func(items ...Slot) Template {
			return Template{Separator: sep, Slots: items}
		}
		cases := map[string]Template{
			"尾槽空": build(
				Slot{Var: VarArtist},
				Slot{Var: VarTitle, Script: "en"},
				Slot{Var: VarTitle, Script: "zh"}),
			"中槽空": build(
				Slot{Var: VarArtist},
				Slot{Var: VarTitle, Script: "zh"},
				Slot{Var: VarTitle, Script: "en"}),
			"首槽空": build(
				Slot{Var: VarTitle, Script: "zh"},
				Slot{Var: VarArtist},
				Slot{Var: VarTitle, Script: "en"}),
			"两个空": build(
				Slot{Var: VarArtist},
				Slot{Var: VarTitle, Script: "en"},
				Slot{Var: VarTitle, Script: "zh"},
				Slot{Var: VarTitle, Script: "ko"}),
		}
		for label, template := range cases {
			got := template.Render(onlyEnglish)
			if strings.HasPrefix(got, separator) || strings.HasSuffix(got, separator) {
				t.Errorf("%s 分隔符 %q：渲染成 %q，两端不该残留分隔符", label, separator, got)
			}
			// 连着两个分隔符也说明空槽没被跳过。
			if strings.Contains(got, separator+separator) {
				t.Errorf("%s 分隔符 %q：渲染成 %q，中间残留了连续分隔符", label, separator, got)
			}
		}
	}
}

// Majo 报的那个场景，把期望值写死。
//
// 上面那条是「换个遍、只看有没有残留」，这条是「就得是这个字符串」。
// 两条一起才既盖得全、又说得准。
func TestArtistEnglishChineseWithMissingChinese(t *testing.T) {
	build := func() Template {
		return Template{
			Separator: "-",
			Slots: []Slot{
				{Var: VarArtist},
				{Var: VarTitle, Script: "en"},
				{Var: VarTitle, Script: "zh"},
			},
		}
	}

	// 中文没识别到 —— 修复前这里是 "BTS-Spring Day-"。
	got := build().Render(Fields{
		Artist: "BTS",
		Titles: map[Script]string{ScriptEnglish: "Spring Day"},
	})
	if got != "BTS-Spring Day" {
		t.Errorf("中文缺失时得到 %q，期望 %q（末尾不该有横杠）", got, "BTS-Spring Day")
	}

	// 三个都在 —— 分隔符要原样留着。
	got = build().Render(Fields{
		Artist: "BTS",
		Titles: map[Script]string{ScriptEnglish: "Spring Day", ScriptChinese: "春日"},
	})
	if got != "BTS-Spring Day-春日" {
		t.Errorf("三格都有时得到 %q，期望 %q", got, "BTS-Spring Day-春日")
	}
}

// TestRenderKeepsSeparatorStyle 确认该在的分隔符都在。
//
// 上一条只保证「没有多余的」，容易写成「把分隔符全删掉」也能过。
// 这条钉住另一半：分隔符的写法（含两侧空格）要原样保留。
//
// 老实现会 `strings.Join(strings.Fields(out), " ")` 把连续空白压成一个空格，
// 那是在替用户改他的分隔符。现在分隔符是用户显式给的，一律不动。
func TestRenderKeepsSeparatorStyle(t *testing.T) {
	fields := Fields{
		Title: "Spring Day", Artist: "BTS", Original: "a.mp4",
		Titles:  map[Script]string{ScriptEnglish: "Spring Day", ScriptChinese: "春日"},
		Artists: map[Script]string{ScriptEnglish: "BTS"},
	}

	cases := []struct {
		separator string
		template  Template
		want      string
	}{
		{"-", Template{Separator: "-", Slots: []Slot{
			{Var: VarArtist}, {Var: VarTitle, Script: "en"}, {Var: VarTitle, Script: "zh"}}},
			"BTS-Spring Day-春日"},
		{" - ", Template{Separator: " - ", Slots: []Slot{
			{Var: VarArtist}, {Var: VarTitle, Script: "en"}, {Var: VarTitle, Script: "zh"}}},
			"BTS - Spring Day - 春日"},
		{" / ", Template{Separator: " / ", Slots: []Slot{
			{Var: VarArtist}, {Var: VarTitle, Script: "en"}}},
			"BTS / Spring Day"},
		{"/", Template{Separator: "/", Slots: []Slot{
			{Var: VarArtist}, {Var: VarTitle, Script: "en"}}},
			"BTS/Spring Day"},
		{"、", Template{Separator: "、", Slots: []Slot{
			{Var: VarArtist}, {Var: VarTitle, Script: "zh"}}},
			"BTS、春日"},
		// 双空格的分隔符是用户自己写的，不能被压成一个。
		{"  -  ", Template{Separator: "  -  ", Slots: []Slot{
			{Var: VarArtist}, {Var: VarTitle, Script: "en"}}},
			"BTS  -  Spring Day"},
	}

	for _, c := range cases {
		if got := c.template.Render(fields); got != c.want {
			t.Errorf("分隔符 %q 得到 %q，期望 %q", c.separator, got, c.want)
		}
	}
}

// ParseBody 负责把老配置里的模板字符串迁移成槽位。
//
// 迁移写错的后果是「升级之后文件名变了」，而且不会有任何报错。
func TestParseBodyMigratesLegacyTemplates(t *testing.T) {
	cases := []struct {
		body      string
		separator string
		want      []Slot
	}{
		// 最常见的一种：占位符之间是同一个分隔符 → 提升成全局，槽位上不留 override。
		{
			"{artist}-{title:en}-{title:zh}", "-",
			[]Slot{
				{Var: VarArtist},
				{Var: VarTitle, Script: "en"},
				{Var: VarTitle, Script: "zh"},
			},
		},
		{
			"{artist} - {title}", " - ",
			[]Slot{{Var: VarArtist}, {Var: VarTitle}},
		},
		{
			"{title}", " - ",
			[]Slot{{Var: VarTitle}},
		},
		// 没有占位符：整段当成固定文字。
		{
			"我的歌", " - ",
			[]Slot{{Var: VarLiteral, Text: "我的歌"}},
		},
	}

	for _, c := range cases {
		got := ParseBody(c.body)
		if got.Separator != c.separator {
			t.Errorf("ParseBody(%q) 的分隔符是 %q，期望 %q", c.body, got.Separator, c.separator)
		}
		if len(got.Slots) != len(c.want) {
			t.Errorf("ParseBody(%q) 得到 %d 个槽位，期望 %d 个：%+v", c.body, len(got.Slots), len(c.want), got.Slots)
			continue
		}
		for i, want := range c.want {
			if got.Slots[i].Var != want.Var || got.Slots[i].Script != want.Script {
				t.Errorf("ParseBody(%q) 第 %d 格是 %+v，期望 %+v", c.body, i, got.Slots[i], want)
			}
		}
	}
}

// 带括号的老模板：括号要挂到后一个槽位上，跟着它一起消失。
func TestParseBodyKeepsBracketsWithTheirSlot(t *testing.T) {
	got := ParseBody("{title} ({year})")

	if len(got.Slots) != 2 {
		t.Fatalf("期望两个槽位，得到 %+v", got.Slots)
	}
	year := got.Slots[1]
	if year.Var != VarYear {
		t.Fatalf("第二格应是年份，得到 %+v", year)
	}
	if year.Prefix != " (" || year.Suffix != ")" {
		t.Errorf("括号没挂到年份上：prefix=%q suffix=%q", year.Prefix, year.Suffix)
	}
	// 这一格和上一格之间不能插分隔符，否则会变成 "Song - (2017)"。
	if year.Separator == nil || *year.Separator != "" {
		t.Errorf("年份前面应当是「无分隔符」，得到 %v", year.Separator)
	}

	// 迁移之后渲染，两种情形都要对。
	fields := Fields{Title: "Song", Year: "2017"}
	if rendered := got.Render(fields); rendered != "Song (2017)" {
		t.Errorf("有年份时得到 %q，期望 %q", rendered, "Song (2017)")
	}
	fields.Year = ""
	if rendered := got.Render(fields); rendered != "Song" {
		t.Errorf("没有年份时得到 %q，期望 %q", rendered, "Song")
	}
}

// 迁移过来的模板要能**渲染出一模一样的结果**，这是迁移正确性的最终判据。
func TestParseBodyRoundTripsLegacyOutput(t *testing.T) {
	fields := Fields{
		Title: "Spring Day", Artist: "BTS", Original: "a.mp4",
		Titles:  map[Script]string{ScriptEnglish: "Spring Day"},
		Artists: map[Script]string{ScriptEnglish: "BTS"},
	}

	cases := []struct{ body, want string }{
		{"{artist}-{title:en}-{title:zh}", "BTS-Spring Day"},
		{"{artist} - {title:en} - {title:zh}", "BTS - Spring Day"},
		{"{artist}/{title:en}/{title:zh}", "BTS/Spring Day"},
		{"{artist}、{title:en}、{title:zh}", "BTS、Spring Day"},
		{"{artist}｜{title:en}｜{title:zh}", "BTS｜Spring Day"},
		{"{artist}－{title:en}－{title:zh}", "BTS－Spring Day"},
		{"{title:en} ({artist})", "Spring Day (BTS)"},
		{"{artist} - {title}", "BTS - Spring Day"},
	}

	for _, c := range cases {
		if got := ParseBody(c.body).Render(fields); got != c.want {
			t.Errorf("迁移 %q 后渲染成 %q，期望 %q", c.body, got, c.want)
		}
	}
}

func TestTemplateNormalizeDropsInvalidSlots(t *testing.T) {
	template := Template{
		Slots: []Slot{
			{Var: VarTitle},
			{Var: "不存在的变量"},
			{Var: VarLiteral, Text: "   "}, // 空的固定文字没有意义
			{Var: VarYear, Script: "zh"},   // 年份没有语种，应当被清掉
		},
	}
	template.Normalize()

	if template.Separator != DefaultSeparator {
		t.Errorf("空分隔符应当补成默认值，得到 %q", template.Separator)
	}
	if len(template.Slots) != 2 {
		t.Fatalf("应当只剩两格，得到 %+v", template.Slots)
	}
	if template.Slots[1].Script != "" {
		t.Errorf("年份不该带语种，得到 %q", template.Slots[1].Script)
	}
}

func TestIndexByScript(t *testing.T) {
	// 主标题排在最前，同一脚本下它优先。
	index := IndexByScript([]string{"봄날", "Spring Day"}, "Spring Day")
	if index[ScriptEnglish] != "Spring Day" {
		t.Errorf("英文槽应是 Spring Day，得到 %q", index[ScriptEnglish])
	}
	if index[ScriptKorean] != "봄날" {
		t.Errorf("韩文槽应是 봄날，得到 %q", index[ScriptKorean])
	}

	// 空候选、重复候选都不能产生空槽或覆盖。
	index = IndexByScript([]string{"", "Hello", "Hello"}, "Hello")
	if len(index) != 1 || index[ScriptEnglish] != "Hello" {
		t.Errorf("期望只有英文一个槽，得到 %+v", index)
	}
}

// TestExtraLocalesFollowsTheTemplate 钉住「语种由模板推导，不再手填」这条。
//
// 以前这是设置页里一个手填的文本框。漏填的后果是那一格**永远是空的**：
// 预览里那一段安静地消失，用户只会以为「这首歌本来就没有韩文名」。
// 所以这里逐条钉住：模板里有什么语种，就查什么；顺序还必须固定。
func TestExtraLocalesFollowsTheTemplate(t *testing.T) {
	cases := []struct {
		name     string
		template string
		want     []string
	}{
		{"没有语种就不多查", "{artist} - {title}", nil},
		{"韩文", "{title:ko}", []string{"ko-KR/KR"}},
		{"英文", "{title:en}", []string{"en-US/US"}},
		{"中文", "{title:zh}", []string{"zh-CN/CN"}},
		{"日文", "{title:ja}", []string{"ja-JP/JP"}},
		// 顺序写死成 英→中→日→韩。不固定的话 map 的遍历顺序会让
		// 「命令预览」每次都不一样，看着像有 bug。
		{"多个语种按固定顺序", "{title:ko} - {title:en} - {title:zh}",
			[]string{"en-US/US", "zh-CN/CN", "ko-KR/KR"}},
		{"艺人语种同样算数", "{artist:ko}", []string{"ko-KR/KR"}},
		// 同一语种出现两次只查一次，否则每个文件白多一次完整请求。
		{"重复的语种只查一次", "{title:ko} - {artist:ko}", []string{"ko-KR/KR"}},
		// 年份、专辑这些没有语种概念，不该产生请求。
		{"其他变量不产生查询", "{album} - {year} - {original}", nil},
		// other 是 Classify 的兜底分类，没有对应 storefront，拿它去查只会白跑。
		{"other 不产生查询", "{title:other}", nil},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := ExtraLocales(ParseBody(testCase.template))
			if len(got) != len(testCase.want) {
				t.Fatalf("ExtraLocales(%q) = %v，期望 %v", testCase.template, got, testCase.want)
			}
			for index := range got {
				if got[index] != testCase.want[index] {
					t.Fatalf("ExtraLocales(%q) = %v，期望 %v", testCase.template, got, testCase.want)
				}
			}
		})
	}
}

// TestUnmatchedMarkedIgnoresEmptyMarker 守住「前缀清空」那个静默失效。
//
// strings.HasPrefix(s, "") 恒为真。老代码直接拿它当「已经改过名」的判断，
// 于是把前缀清空的用户会发现未识别的一个都不改名 ——
// 界面上看起来一切正常，只是那些文件纹丝不动。
func TestUnmatchedMarkedIgnoresEmptyMarker(t *testing.T) {
	cases := []struct {
		name      string
		unmatched Unmatched
		stem      string
		want      bool
	}{
		{"默认前缀命中", Unmatched{Prefix: "0_"}, "0_clip", true},
		{"默认前缀不命中", Unmatched{Prefix: "0_"}, "clip", false},
		{"后缀命中", Unmatched{Suffix: "_未识别"}, "clip_未识别", true},
		{"后缀不命中", Unmatched{Suffix: "_未识别"}, "clip", false},
		{"前后缀都设，命中其一即可", Unmatched{Prefix: "0_", Suffix: "_x"}, "a_x", true},
		// 这两条是重点：标记为空时**不能**恒真，否则文件永远不改名。
		{"前缀为空不算命中", Unmatched{}, "clip", false},
		{"只有后缀时前缀为空也不算命中", Unmatched{Suffix: "_x"}, "clip", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.unmatched.Marked(testCase.stem); got != testCase.want {
				t.Errorf("Marked(%q) = %v，期望 %v", testCase.stem, got, testCase.want)
			}
		})
	}
}

// TestFallbackPutsSuffixAfterTheName 钉住后缀是接在名字**后面**，不是前面。
func TestFallbackPutsSuffixAfterTheName(t *testing.T) {
	cases := []struct {
		unmatched Unmatched
		original  string
		want      string
	}{
		{Unmatched{Prefix: "0_"}, "clip.mp4", "0_clip"},
		{Unmatched{Suffix: "_未识别"}, "clip.mp4", "clip_未识别"},
		{Unmatched{Prefix: "0_", Suffix: "_未识别"}, "clip.mp4", "0_clip_未识别"},
		// 扩展名不参与：它是后面单独拼回去的，留在主干里会被清洗掉或造成双扩展名。
		{Unmatched{Prefix: "0_"}, "D:/Music/a.b.mp4", "0_a.b"},
		{Unmatched{}, "clip.mp4", "clip"},
	}

	for _, testCase := range cases {
		if got := Fallback(testCase.unmatched, testCase.original); got != testCase.want {
			t.Errorf("Fallback(%+v, %q) = %q，期望 %q",
				testCase.unmatched, testCase.original, got, testCase.want)
		}
	}
}
