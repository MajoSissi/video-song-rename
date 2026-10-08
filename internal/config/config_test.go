package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"videosongrename/internal/naming"
)

// 模板库这一段的核心风险是「静默丢配置」：升级后用户自己改过的模板被内置值顶掉，
// 而且不会有任何报错。所以这里把每条不变量都钉住。

// tpl 用模板字符串构造一项，读起来比手写 Slot 字面量清楚。
func tpl(name, body string) Template {
	return Template{Name: name, Template: naming.ParseBody(body)}
}

// spec 同样用字符串描述期望值，方便和迁移结果比对。
func spec(body string) naming.Template { return naming.ParseBody(body) }

func TestDefaultHasTemplates(t *testing.T) {
	cfg := Default()
	cfg.Normalize()

	if len(cfg.Templates) == 0 {
		t.Fatal("默认配置的模板库不该是空的")
	}
	if cfg.ActiveTemplate != cfg.Templates[0].Name {
		t.Errorf("默认应当选中第一项，实际选中 %q", cfg.ActiveTemplate)
	}
	if !cfg.NameTemplate.Equal(cfg.Templates[0].Template) {
		t.Errorf("NameTemplate 应当等于当前选中项：%+v vs %+v",
			cfg.NameTemplate, cfg.Templates[0].Template)
	}
}

// 核心不变量：无论输入多离谱，NameTemplate 必须等于 ActiveTemplate 那一项。
func TestNormalizeKeepsNameTemplateInSync(t *testing.T) {
	cfg := Config{
		Templates: []Template{
			tpl("甲", "{title}"),
			tpl("乙", "{artist} - {title}"),
		},
		ActiveTemplate: "乙",
		NameTemplate:   spec("这里是垃圾，应该被覆盖"),
	}
	cfg.Normalize()

	if !cfg.NameTemplate.Equal(spec("{artist} - {title}")) {
		t.Errorf("NameTemplate 应当被重新派生，实际 %+v", cfg.NameTemplate)
	}
}

func TestNormalizeFallsBackWhenActiveMissing(t *testing.T) {
	cfg := Config{
		Templates:      []Template{tpl("甲", "{title}")},
		ActiveTemplate: "这个名字不存在",
	}
	cfg.Normalize()

	if cfg.ActiveTemplate != "甲" {
		t.Errorf("选中的模板不存在时应当退回第一项，实际 %q", cfg.ActiveTemplate)
	}
}

func TestNormalizeReseedsEmptyLibrary(t *testing.T) {
	cfg := Config{Templates: nil, ActiveTemplate: "随便"}
	cfg.Normalize()

	if len(cfg.Templates) == 0 {
		t.Fatal("模板库被清空后应当铺回内置的，不该留一个空页面")
	}
	if !HasTemplate(cfg.Templates, cfg.ActiveTemplate) {
		t.Errorf("选中项 %q 不在铺回来的库里", cfg.ActiveTemplate)
	}
}

func TestNormalizeDropsBlankAndDuplicateTemplates(t *testing.T) {
	cfg := Config{
		Templates: []Template{
			tpl("甲", "{title}"),
			{Name: "  ", Template: spec("{artist}")},  // 空名字，丢
			{Name: "乙", Template: naming.Template{}}, // 一个槽位都没有，丢
			tpl("甲", "{album}"),                      // 重名，保留先出现的
			tpl(" 丙 ", "{year}"),                      // 两端空白，应当被 trim
		},
	}
	cfg.Normalize()

	if len(cfg.Templates) != 2 {
		t.Fatalf("期望 2 项，实际 %d 项：%+v", len(cfg.Templates), cfg.Templates)
	}
	if !cfg.Templates[0].Template.Equal(spec("{title}")) {
		t.Errorf("重名应当保留先出现的，实际 %+v", cfg.Templates[0].Template)
	}
	if cfg.Templates[1].Name != "丙" || !cfg.Templates[1].Template.Equal(spec("{year}")) {
		t.Errorf("两端空白没被 trim：%+v", cfg.Templates[1])
	}
}

// 旧配置只有 name_template（字符串），没有 templates / active_template。
func TestLoadMigratesLegacyCustomTemplate(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"name_template":"{artist}_{title}","unmatched_prefix":"0_","segment_seconds":12}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(legacy), 0o644); err != nil {
		t.Fatalf("写旧配置失败: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if !cfg.NameTemplate.Equal(spec("{artist}_{title}")) {
		t.Fatalf("用户改过的模板被顶掉了：%+v", cfg.NameTemplate)
	}
	if !HasTemplate(cfg.Templates, "自定义") {
		t.Fatalf("自定义模板应当被迁移进库，实际库是 %+v", cfg.Templates)
	}
	if cfg.ActiveTemplate != "自定义" {
		t.Errorf("迁移后应当选中迁移进来的那一项，实际 %q", cfg.ActiveTemplate)
	}
	// 内置模板也要在，不能只剩迁移进来的那一项。
	if len(cfg.Templates) < 2 {
		t.Errorf("迁移后应当保留内置模板，实际只有 %+v", cfg.Templates)
	}
}

// 旧配置里的模板如果正好等于某个内置模板，应当直接认领那一项，而不是新建「自定义」。
func TestLoadLegacyTemplateMatchingBuiltin(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"name_template":"{title} - {artist}"}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(legacy), 0o644); err != nil {
		t.Fatalf("写旧配置失败: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if cfg.ActiveTemplate != "歌名 - 艺人" {
		t.Errorf("应当认领内置的同名模板，实际选中 %q", cfg.ActiveTemplate)
	}
	if HasTemplate(cfg.Templates, "自定义") {
		t.Error("主体跟内置一致时不该多造一个「自定义」")
	}
}

// 上一版的配置：有 templates 键，但每一项还是 {"name":…,"body":"…"}。
//
// 这是**真实存在**的一版配置形状，也是这次改动最容易丢数据的地方：
// body 一旦被当成未知键忽略，模板就变成空的，再被 Normalize 清掉，
// 用户攒的模板会全部消失且没有任何报错。
func TestLoadMigratesLegacyTemplateBody(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
		"templates": [
			{"name": "只歌名", "body": "{title}"},
			{"name": "我的", "body": "{artist}__{title}"},
			{"name": "带括号", "body": "{title} ({year})"}
		],
		"active_template": "我的",
		"name_template": "{artist}__{title}"
	}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(legacy), 0o644); err != nil {
		t.Fatalf("写旧配置失败: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if len(cfg.Templates) != 3 {
		t.Fatalf("模板数量变了，实际 %d 项：%+v", len(cfg.Templates), cfg.Templates)
	}
	if cfg.ActiveTemplate != "我的" {
		t.Errorf("选中项丢了，实际 %q", cfg.ActiveTemplate)
	}
	if !cfg.NameTemplate.Equal(spec("{artist}__{title}")) {
		t.Errorf("迁移后的模板不对：%+v", cfg.NameTemplate)
	}

	// 带括号的那一项，括号必须挂在年份上。
	bracketed := TemplateSpec(cfg.Templates, "带括号")
	fields := naming.Fields{Title: "Song", Year: "2017"}
	if got := bracketed.Render(fields); got != "Song (2017)" {
		t.Errorf("带括号的模板迁移后渲染成 %q，期望 %q", got, "Song (2017)")
	}
	fields.Year = ""
	if got := bracketed.Render(fields); got != "Song" {
		t.Errorf("年份缺失时渲染成 %q，期望 %q", got, "Song")
	}
}

func TestSaveLoadRoundTripKeepsTemplates(t *testing.T) {
	dir := t.TempDir()

	cfg := Default()
	cfg.Templates = append(cfg.Templates, tpl("我的", "{title}__{artist}"))
	cfg.ActiveTemplate = "我的"
	cfg.Normalize()

	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if loaded.ActiveTemplate != "我的" || !loaded.NameTemplate.Equal(spec("{title}__{artist}")) {
		t.Errorf("往返后选中项丢了：active=%q name=%+v", loaded.ActiveTemplate, loaded.NameTemplate)
	}
	if !HasTemplate(loaded.Templates, "我的") {
		t.Errorf("往返后自定义模板丢了：%+v", loaded.Templates)
	}
}

// 每格单独设的分隔符、以及「明确不要分隔符」的槽位，往返之后必须还是原样。
//
// 这里最容易丢的是 nil 和空串的区别：一旦序列化时把两者写成同一个值，
// 「歌名 (年份)」在下次启动后就会变成「歌名 - (年份)」。
func TestSaveLoadRoundTripKeepsPerSlotSeparators(t *testing.T) {
	dir := t.TempDir()

	none := ""
	slash := " / "
	cfg := Default()
	cfg.Templates = []Template{{
		Name: "混用",
		Template: naming.Template{
			Separator: " - ",
			Slots: []naming.Slot{
				{Var: naming.VarArtist},
				{Var: naming.VarTitle, Script: "en"},
				{Var: naming.VarTitle, Script: "zh", Separator: &slash},
				{Var: naming.VarYear, Prefix: " (", Suffix: ")", Separator: &none},
			},
		},
	}}
	cfg.ActiveTemplate = "混用"
	cfg.Normalize()

	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	got := TemplateSpec(loaded.Templates, "混用")
	want := naming.Template{
		Separator: " - ",
		Slots: []naming.Slot{
			{Var: naming.VarArtist},
			{Var: naming.VarTitle, Script: "en"},
			{Var: naming.VarTitle, Script: "zh", Separator: &slash},
			{Var: naming.VarYear, Prefix: " (", Suffix: ")", Separator: &none},
		},
	}
	if !got.Equal(want) {
		t.Errorf("往返后槽位变了：\n得到 %+v\n期望 %+v", got, want)
	}

	// 渲染结果也要对，这才是最终判据。
	fields := naming.Fields{
		Artist: "BTS",
		Year:   "2017",
		Titles: map[naming.Script]string{naming.ScriptEnglish: "Spring Day", naming.ScriptChinese: "春日"},
	}
	if rendered := got.Render(fields); rendered != "BTS - Spring Day / 春日 (2017)" {
		t.Errorf("往返后渲染成 %q，期望 %q", rendered, "BTS - Spring Day / 春日 (2017)")
	}
}

// 代理三种取值必须原样往返，尤其是「留空」与「-」不能互相塌陷。
//
// 这两者塌陷的后果不对称：都变成空的话，设了 HTTP_PROXY 的用户没法强制直连；
// 都变成 "-" 的话，所有人都莫名其妙地不跟随系统代理了。而两种错法在界面上
// 都看不出来——输入框里显示的字符是对的，只有真正跑起来才不一样。
func TestProxyRoundTripsAndNormalizes(t *testing.T) {
	for _, want := range []string{"", "-", "http://127.0.0.1:7890", "socks5://127.0.0.1:1080"} {
		dir := t.TempDir()

		cfg := Default()
		cfg.Proxy = want
		cfg.Normalize()
		if err := Save(dir, cfg); err != nil {
			t.Fatalf("Save 失败: %v", err)
		}

		loaded, err := Load(dir)
		if err != nil {
			t.Fatalf("Load 失败: %v", err)
		}
		if loaded.Proxy != want {
			t.Errorf("代理往返后从 %q 变成了 %q", want, loaded.Proxy)
		}
	}
}

// 前后空格要去掉。从浏览器里复制代理地址经常带尾随空格，留着的话
// reqwest 会把一个看起来完全正常的地址当成非法 URL 拒掉。
func TestProxyIsTrimmed(t *testing.T) {
	cfg := Default()
	cfg.Proxy = "  http://127.0.0.1:7890 \t"
	cfg.Normalize()
	if cfg.Proxy != "http://127.0.0.1:7890" {
		t.Errorf("代理地址没有被 trim: %q", cfg.Proxy)
	}

	// 全是空白等价于留空，也就是「跟随环境变量」。
	blank := Default()
	blank.Proxy = "   "
	blank.Normalize()
	if blank.Proxy != "" {
		t.Errorf("全空白的代理应当收敛成空串，得到 %q", blank.Proxy)
	}
}

// 新配置（有 templates 键）不该被迁移逻辑碰。
func TestLoadDoesNotMigrateNewConfig(t *testing.T) {
	dir := t.TempDir()

	cfg := Default()
	cfg.Templates = []Template{tpl("唯一", "{original}")}
	cfg.ActiveTemplate = "唯一"
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if len(loaded.Templates) != 1 || loaded.Templates[0].Name != "唯一" {
		t.Errorf("新配置的模板库被改了：%+v", loaded.Templates)
	}
	if HasTemplate(loaded.Templates, "自定义") {
		t.Error("新配置不该被塞进「自定义」")
	}
}

func TestSaveWritesTemplatesKey(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Default()); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	if err != nil {
		t.Fatalf("读回配置失败: %v", err)
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, key := range []string{"templates", "active_template", "name_template", "close_to_tray"} {
		if _, ok := probe[key]; !ok {
			t.Errorf("保存出来的配置缺少 %q 键——下次 Load 会被当成旧配置", key)
		}
	}

	// name_template 现在必须是**对象**（分隔符 + 槽位），不再是字符串。
	// 写成字符串也能被读回来（向后兼容），但那说明序列化退回了旧形状。
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(probe["name_template"], &spec); err != nil {
		t.Fatalf("name_template 不是对象：%s", probe["name_template"])
	}
	for _, key := range []string{"separator", "slots"} {
		if _, ok := spec[key]; !ok {
			t.Errorf("name_template 里缺少 %q", key)
		}
	}

	// 模板项里也不该再有 body 键。
	var templates []map[string]json.RawMessage
	if err := json.Unmarshal(probe["templates"], &templates); err != nil {
		t.Fatalf("templates 解析失败: %v", err)
	}
	for _, item := range templates {
		if _, ok := item["body"]; ok {
			t.Errorf("模板项里还写着 body：%s", probe["templates"])
			break
		}
		if _, ok := item["slots"]; !ok {
			t.Errorf("模板项里没有 slots：%s", probe["templates"])
			break
		}
	}
}

// TestUnmatchedSuffixRoundTrips 钉住后缀能存能读。
//
// 只测前缀是不够的：两个字段类型一样、位置相邻，接错线的话
// 存进去的可能是前缀的值，而往返测试正好能抓住这种串位。
func TestUnmatchedSuffixRoundTrips(t *testing.T) {
	for _, want := range []string{"", "_未识别", " (待确认)"} {
		dir := t.TempDir()

		cfg := Default()
		cfg.UnmatchedSuffix = want
		cfg.Normalize()
		if err := Save(dir, cfg); err != nil {
			t.Fatalf("Save 失败: %v", err)
		}

		loaded, err := Load(dir)
		if err != nil {
			t.Fatalf("Load 失败: %v", err)
		}
		if loaded.UnmatchedSuffix != want {
			t.Errorf("后缀往返后从 %q 变成了 %q", want, loaded.UnmatchedSuffix)
		}
		// 前缀不能被顺手改掉。
		if loaded.UnmatchedPrefix != cfg.UnmatchedPrefix {
			t.Errorf("改后缀把前缀带歪了：%q", loaded.UnmatchedPrefix)
		}
	}
}

// TestExtraLocalesFollowsTheActiveTemplate 钉住「语种是派生值」这条。
//
// 这**不是**配置项：切一下当前模板，要查的语种就得跟着变。
// 如果哪天有人把它改回一个存起来的字段，这条会立刻红。
func TestExtraLocalesFollowsTheActiveTemplate(t *testing.T) {
	cfg := Default()

	// 内置模板里挑两个对照：一个不带语种，一个带韩文。
	plain := naming.ParseBody("{artist} - {title}")
	withKorean := naming.ParseBody("{artist} - {title:ko}")

	cfg.Templates = []Template{
		{Name: "朴素", Template: plain},
		{Name: "带韩文", Template: withKorean},
	}

	cfg.ActiveTemplate = "朴素"
	cfg.Normalize()
	if got := cfg.ExtraLocales(); len(got) != 0 {
		t.Errorf("朴素模板不该多查任何语种，得到 %v", got)
	}

	cfg.ActiveTemplate = "带韩文"
	cfg.Normalize()
	got := cfg.ExtraLocales()
	if len(got) != 1 || got[0] != "ko-KR/KR" {
		t.Errorf("切到带韩文的模板后应当只查 ko-KR/KR，得到 %v", got)
	}
}

// TestLegacyTitleLocalesKeyIsIgnored 守住旧配置的兼容。
//
// title_locales 这个键已经删了（改成从模板推导）。旧配置文件里还留着它，
// 读的时候必须**当作没看见**而不是报错 —— 否则升级一次用户就打不开程序了。
// 下一次保存它会自然消失，所以不需要写迁移代码，但这条得钉住。
func TestLegacyTitleLocalesKeyIsIgnored(t *testing.T) {
	dir := t.TempDir()

	legacy := `{"title_locales":["zh-CN/CN","ko-KR/KR"],"unmatched_prefix":"0_"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("写旧配置失败: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("读带 title_locales 的旧配置不该报错: %v", err)
	}
	// 默认值要被补上，不能因为旧文件里只有两个键就留下一份空配置。
	if cfg.UnmatchedPrefix != "0_" {
		t.Errorf("旧配置的 unmatched_prefix 没读出来: %q", cfg.UnmatchedPrefix)
	}
	if len(cfg.Templates) == 0 {
		t.Error("旧配置没模板，应当补上内置模板")
	}
}
