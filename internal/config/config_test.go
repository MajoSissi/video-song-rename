package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"videosongrename/internal/naming"
)

// spec 用模板字符串描述期望值，方便和迁移结果比对。
func spec(body string) naming.Template { return naming.ParseBody(body) }

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
}

func TestDefaultHasUsableNaming(t *testing.T) {
	cfg := Default()
	cfg.Normalize()

	if cfg.NameTemplate.IsZero() {
		t.Fatal("默认命名规则不该是空的")
	}
	if !cfg.NameTemplate.Equal(spec("{title}")) {
		t.Errorf("默认规则变了：%+v（老用户升级后文件名会跟着变）", cfg.NameTemplate)
	}
	if cfg.Move.Enabled {
		t.Error("默认不该自动搬文件：用户没说要，就别动他的目录结构")
	}
	if !cfg.Move.MatchedOnly {
		t.Error("默认应当只搬识别成功的")
	}
}

// 一份用户自己配的规则，归一化之后必须原样保留 —— 不能被默认值顶掉。
func TestNormalizeKeepsUserNaming(t *testing.T) {
	slash := " / "
	none := ""
	cfg := Config{
		NameTemplate: naming.Template{
			Separator: " - ",
			Slots: []naming.Slot{
				{Var: naming.VarArtist},
				{Var: naming.VarTitle, Script: "en"},
				{Var: naming.VarTitle, Script: "zh", Separator: &slash},
				{Var: naming.VarYear, Prefix: " (", Suffix: ")", Separator: &none},
			},
		},
	}
	cfg.Normalize()

	fields := naming.Fields{
		Artist: "BTS",
		Year:   "2017",
		Titles: map[naming.Script]string{naming.ScriptEnglish: "Spring Day", naming.ScriptChinese: "春日"},
	}
	if got := cfg.NameTemplate.Render(fields); got != "BTS - Spring Day / 春日 (2017)" {
		t.Errorf("归一化之后渲染成 %q，期望 %q", got, "BTS - Spring Day / 春日 (2017)")
	}
}

// 空规则（一个可用槽位都没有）必须退回默认，而不是留一份渲染不出东西的规则。
func TestNormalizeFallsBackForEmptyNaming(t *testing.T) {
	cfg := Config{NameTemplate: naming.Template{Slots: []naming.Slot{{Var: "不认识的变量"}}}}
	cfg.Normalize()

	if !cfg.NameTemplate.Equal(spec("{title}")) {
		t.Errorf("空规则应当退回默认，得到 %+v", cfg.NameTemplate)
	}
}

// 旧配置是「模板库 + 当前选中」。迁移要挑出**当时在用的**那一份。
func TestLoadMigratesActiveTemplate(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{
		"templates": [
			{"name": "只歌名", "body": "{title}"},
			{"name": "我的", "body": "{artist}__{title}"},
			{"name": "带括号", "body": "{title} ({year})"}
		],
		"active_template": "我的",
		"name_template": "{title}"
	}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !cfg.NameTemplate.Equal(spec("{artist}__{title}")) {
		t.Fatalf("拿到的是 %+v，应当是被选中的那一份", cfg.NameTemplate)
	}
}

// 选中项指向一个不存在的名字时退回第一项，而不是丢掉整份配置。
func TestLoadMigratesFallsBackToFirstTemplate(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{
		"templates": [
			{"name": "甲", "separator": " - ", "slots": [{"var": "album"}]}
		],
		"active_template": "不存在"
	}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !cfg.NameTemplate.Equal(spec("{album}")) {
		t.Errorf("应当退回第一项，得到 %+v", cfg.NameTemplate)
	}
}

// 更老的配置里 name_template 就是一个字符串。
func TestLoadMigratesLegacyStringTemplate(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"name_template":"{artist}_{title}","unmatched_prefix":"0_"}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !cfg.NameTemplate.Equal(spec("{artist}_{title}")) {
		t.Fatalf("用户改过的规则被顶掉了：%+v", cfg.NameTemplate)
	}
}

// 新配置（没有 templates 键）里手写的 name_template 不能被迁移逻辑碰。
func TestLoadKeepsCurrentNaming(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"name_template":{"separator":" ","slots":[{"var":"title"},{"var":"year"}]}}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !cfg.NameTemplate.Equal(naming.Template{
		Separator: " ",
		Slots:     []naming.Slot{{Var: naming.VarTitle}, {Var: naming.VarYear}},
	}) {
		t.Errorf("当前格式的规则被改了：%+v", cfg.NameTemplate)
	}
}

// 往返：每格的分隔符、「明确不要分隔符」的 nil/空串区别都必须保住。
//
// 一旦序列化时把两者写成同一个值，「歌名 (年份)」在下次启动后就会变成
// 「歌名 - (年份)」——而界面上看不出任何异常。
func TestSaveLoadRoundTripKeepsNaming(t *testing.T) {
	dir := t.TempDir()

	slash := " / "
	none := ""
	cfg := Default()
	cfg.NameTemplate = naming.Template{
		Separator: " - ",
		Slots: []naming.Slot{
			{Var: naming.VarArtist},
			{Var: naming.VarTitle, Script: "en"},
			{Var: naming.VarTitle, Script: "zh", Separator: &slash},
			{Var: naming.VarYear, Prefix: " (", Suffix: ")", Separator: &none},
		},
		KeepDuplicates: true,
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !loaded.NameTemplate.Equal(cfg.NameTemplate) {
		t.Errorf("往返后规则变了：\n得到 %+v\n期望 %+v", loaded.NameTemplate, cfg.NameTemplate)
	}
}

// 去重的零值就是「开着」。
//
// 老配置里没有 keep_duplicates 这个键，读进来必须是 false —— 那正好是想要的
// 行为，所以这件事不需要任何迁移代码。要是哪天有人把字段改成
// `KeepDuplicates bool` 之外的东西（比如 *bool 或 Dedupe），这条会立刻变红。
func TestLoadDefaultsToDedupe(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"name_template":{"separator":" - ","slots":[
		{"var":"title"},{"var":"title","script":"en"}]}}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.NameTemplate.KeepDuplicates {
		t.Error("老配置没有 keep_duplicates 键，应当落在「去重」上")
	}
	if Default().NameTemplate.KeepDuplicates {
		t.Error("默认规则应当去重")
	}
}

// 保存出来的配置里不该再有 templates / active_template，
// 但必须有 name_template 和 move，且 name_template 是对象而不是字符串。
func TestSaveWritesOnlyCurrentShape(t *testing.T) {
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

	for _, gone := range []string{"templates", "active_template"} {
		if _, ok := probe[gone]; ok {
			t.Errorf("还在写 %q 键 —— 模板库已经删了，写回去会让下次 Load 误判成旧配置", gone)
		}
	}
	for _, key := range []string{"name_template", "move"} {
		if _, ok := probe[key]; !ok {
			t.Errorf("保存出来的配置缺少 %q", key)
		}
	}

	var shaped map[string]json.RawMessage
	if err := json.Unmarshal(probe["name_template"], &shaped); err != nil {
		t.Fatalf("name_template 不是对象：%s", probe["name_template"])
	}
	for _, key := range []string{"separator", "slots"} {
		if _, ok := shaped[key]; !ok {
			t.Errorf("name_template 里缺少 %q", key)
		}
	}
}

// ---------------------------------------------------------------- 归档位置

func TestMoveRuleSiblingUsesParentName(t *testing.T) {
	rule := MoveRule{Enabled: true, Mode: MoveModeSibling}
	// 文件所在目录是 ...\2024，同级目录就该是 ...\2024_已整理 这种形状。
	got := rule.TargetDir(filepath.Join("D:", "视频", "2024", "a.mp4"))
	want := filepath.Join("D:", "视频", "2024")
	if got != want {
		t.Errorf("没加前后缀时目录名应当与上级同名，得到 %q，期望 %q", got, want)
	}

	rule.Suffix = "_已整理"
	got = rule.TargetDir(filepath.Join("D:", "视频", "2024", "a.mp4"))
	want = filepath.Join("D:", "视频", "2024_已整理")
	if got != want {
		t.Errorf("后缀没生效：得到 %q，期望 %q", got, want)
	}

	rule.Prefix = "整理_"
	rule.Suffix = ""
	got = rule.TargetDir(filepath.Join("D:", "视频", "2024", "a.mp4"))
	want = filepath.Join("D:", "视频", "整理_2024")
	if got != want {
		t.Errorf("前缀没生效：得到 %q，期望 %q", got, want)
	}
}

func TestMoveRuleSiblingCustomName(t *testing.T) {
	rule := MoveRule{Enabled: true, Mode: MoveModeSibling, Name: "已整理"}
	got := rule.TargetDir(filepath.Join("D:", "视频", "2024", "a.mp4"))
	if want := filepath.Join("D:", "视频", "已整理"); got != want {
		t.Errorf("自定义目录名没生效：得到 %q，期望 %q", got, want)
	}
}

// 文件直接放在盘符根目录时，Base 给回来的是 "\"，不能拿它当目录名。
func TestMoveRuleSiblingAtDriveRootNeedsName(t *testing.T) {
	rule := MoveRule{Enabled: true, Mode: MoveModeSibling}
	if got := rule.TargetDir(`D:\a.mp4`); got != "" {
		t.Errorf("根目录下定不出名字时应当返回空串，得到 %q", got)
	}

	rule.Name = "已整理"
	if got := rule.TargetDir(`D:\a.mp4`); got != `D:\已整理` {
		t.Errorf("给了名字就该能用，得到 %q", got)
	}
}

func TestMoveRuleFixedDir(t *testing.T) {
	rule := MoveRule{Enabled: true, Mode: MoveModeFixed, Dir: `E:\音乐归档`}
	got := rule.TargetDir(filepath.Join("D:", "下载", "a.mp4"))
	if got != `E:\音乐归档` {
		t.Errorf("固定目录没生效：%q", got)
	}

	// 相对路径按「文件所在目录」解析 —— 「整理到同目录下的 sorted」得能用。
	rule.Dir = "sorted"
	got = rule.TargetDir(filepath.Join("D:", "下载", "2024", "a.mp4"))
	if want := filepath.Join("D:", "下载", "2024", "sorted"); got != want {
		t.Errorf("相对目录应当相对文件所在目录，得到 %q，期望 %q", got, want)
	}

	rule.Dir = "  "
	if got := rule.TargetDir(filepath.Join("D:", "下载", "a.mp4")); got != "" {
		t.Errorf("目录留空时应当不移动，得到 %q", got)
	}
}

func TestMoveRuleDisabledNeverMoves(t *testing.T) {
	rule := MoveRule{Mode: MoveModeSibling, Suffix: "_x"}
	if got := rule.TargetDir(filepath.Join("D:", "a", "b.mp4")); got != "" {
		t.Errorf("没启用时不该算出目录，得到 %q", got)
	}
}

func TestMoveRuleNormalizesMode(t *testing.T) {
	rule := MoveRule{Mode: "胡说", Dir: "  E:\\x  "}
	rule.Normalize()
	if rule.Mode != MoveModeSibling {
		t.Errorf("认不出的模式应当退回 sibling，得到 %q", rule.Mode)
	}
	if rule.Dir != filepath.Join("E:", "x") && rule.Dir != `E:\x` {
		t.Errorf("目录没被 trim：%q", rule.Dir)
	}
}

func TestMoveRuleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Move = MoveRule{
		Enabled:     true,
		Mode:        MoveModeFixed,
		Dir:         filepath.Join("E:", "音乐归档"),
		Prefix:      "a_",
		Suffix:      "_b",
		MatchedOnly: false,
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if loaded.Move != cfg.Move {
		t.Errorf("归档规则往返后变了：\n得到 %+v\n期望 %+v", loaded.Move, cfg.Move)
	}
}

// ---------------------------------------------------------------- 其他

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

// TestExtraLocalesFollowsTheNaming 钉住「语种是派生值」这条。
//
// 这**不是**配置项：命名规则里用到哪个语种，要查的语种就得跟着变。
// 如果哪天有人把它改回一个存起来的字段，这条会立刻红。
func TestExtraLocalesFollowsTheNaming(t *testing.T) {
	cfg := Default()

	cfg.NameTemplate = naming.ParseBody("{artist} - {title}")
	cfg.Normalize()
	if got := cfg.ExtraLocales(); len(got) != 0 {
		t.Errorf("不带语种的规则不该多查任何语种，得到 %v", got)
	}

	cfg.NameTemplate = naming.ParseBody("{artist} - {title:ko}")
	cfg.Normalize()
	got := cfg.ExtraLocales()
	if len(got) != 1 || got[0] != "ko-KR/KR" {
		t.Errorf("带上韩文之后应当只查 ko-KR/KR，得到 %v", got)
	}
}

// TestLegacyTitleLocalesKeyIsIgnored 守住旧配置的兼容。
//
// title_locales 这个键已经删了（改成从命名规则推导）。旧配置文件里还留着它，
// 读的时候必须**当作没看见**而不是报错 —— 否则升级一次用户就打不开程序了。
func TestLegacyTitleLocalesKeyIsIgnored(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"title_locales":["zh-CN/CN","ko-KR/KR"],"unmatched_prefix":"0_"}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("读带 title_locales 的旧配置不该报错: %v", err)
	}
	if cfg.UnmatchedPrefix != "0_" {
		t.Errorf("旧配置的 unmatched_prefix 没读出来: %q", cfg.UnmatchedPrefix)
	}
	if cfg.NameTemplate.IsZero() {
		t.Error("旧配置没命名规则，应当补上默认值")
	}
}
