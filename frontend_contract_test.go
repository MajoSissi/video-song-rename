package main

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"videosongrename/internal/naming"
)

// 前端和后端之间的接口是**字符串约定**，编译器完全看不见：
//
//	el('foo')    ← 依赖 HTML 里存在 id="foo"
//	App.Bar()    ← 依赖 app.go 里存在导出的 (*App).Bar
//	RT.Baz()     ← 依赖 Wails 运行时确实有 Baz
//
// 三处任意一处写错，`go build` 和 `go vet` 都不会吭声，要等用户点下去才炸。
// 这个测试就是把这份约定从「运行时才发现」提前到「提交前就红」。

const frontendPath = "frontend/index.html"

var (
	reHTMLID    = regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)
	reElCall    = regexp.MustCompile(`\bel\('([A-Za-z0-9_-]+)'\)`)
	reAppCall   = regexp.MustCompile(`\bApp\.([A-Za-z0-9_]+)\(`)
	reRTCall    = regexp.MustCompile(`\bRT\.([A-Za-z0-9_]+)\(`)
	reScriptTag = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
)

// wailsRuntimeMethods 是 Wails v2 注入的 window.runtime 上的方法白名单。
// 只列我们真的用得到的那些——多列没意义，关键是**拼错要能被发现**。
var wailsRuntimeMethods = map[string]bool{
	"WindowMinimise":            true,
	"WindowUnminimise":          true,
	"WindowToggleMaximise":      true,
	"WindowIsMaximised":         true,
	"WindowMaximise":            true,
	"WindowUnmaximise":          true,
	"WindowHide":                true,
	"WindowShow":                true,
	"WindowSetTitle":            true,
	"WindowCenter":              true,
	"WindowSetBackgroundColour": true,
	"Quit":                      true,
	"EventsOn":                  true,
	"EventsOff":                 true,
	"EventsEmit":                true,
	"BrowserOpenURL":            true,
	"ClipboardSetText":          true,
	"LogPrint":                  true,
	// 拖放必须在**前端**注册，光在 Go 侧挂 OnFileDrop 收不到任何东西：
	// Wails 的 DOM dragover/drop 监听器就是从这次调用里装上去的。
	// 签名见 wails/v2@v2.16.0 internal/frontend/runtime/runtime_prod_desktop.js：
	// OnFileDrop(callback, useDropTarget) / OnFileDropOff()。
	"OnFileDrop":    true,
	"OnFileDropOff": true,
}

func readFrontend(t *testing.T) (html, js string) {
	t.Helper()

	raw, err := os.ReadFile(frontendPath)
	if err != nil {
		t.Fatalf("读不到 %s: %v", frontendPath, err)
	}
	html = string(raw)

	blocks := reScriptTag.FindAllStringSubmatch(html, -1)
	if len(blocks) == 0 {
		t.Fatal("frontend/index.html 里找不到 <script> 块，结构变了就更新这个测试")
	}
	// 取最后一块：前面可能有别的脚本，主逻辑在最后。
	js = blocks[len(blocks)-1][1]
	return html, js
}

// cssRule 取出某个选择器的样式块内容。
//
// 注意：它按字面匹配选择器，`.bottom` 不会命中 `.bottom.collapsed` 那种带后缀的规则
// （因为 `\s*\{` 要求选择器后面紧跟花括号）。这正是我们要的——只关心那一条。
func cssRule(t *testing.T, html, selector string) string {
	t.Helper()

	pattern := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(selector) + `\s*\{(.*?)\}`)
	match := pattern.FindStringSubmatch(html)
	if match == nil {
		t.Fatalf("找不到 %s 的样式块，结构变了就更新这个测试", selector)
	}
	return match[1]
}

// TestFrontendIDsExist 保证每个 el('x') 都有对应的 id="x"。
//
// 这条错了的表现是 `Cannot read properties of null` —— 整个初始化中断，
// 界面卡在空白或者只有一半。
func TestFrontendIDsExist(t *testing.T) {
	html, js := readFrontend(t)

	present := map[string]bool{}
	for _, match := range reHTMLID.FindAllStringSubmatch(html, -1) {
		present[match[1]] = true
	}

	var missing []string
	for _, match := range reElCall.FindAllStringSubmatch(js, -1) {
		if !present[match[1]] {
			missing = append(missing, match[1])
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("这些 id 被 el() 引用，但 HTML 里没有：%s", strings.Join(missing, ", "))
	}
}

// TestFrontendAppMethodsExist 保证每个 App.Xxx() 都能在 *App 上找到。
//
// 这条错了的表现是「按钮点了没反应」，而且控制台只有一句
// `App.Xxx is not a function`，很容易被当成后端没起来。
func TestFrontendAppMethodsExist(t *testing.T) {
	_, js := readFrontend(t)

	appType := reflect.TypeOf(&App{})

	var missing []string
	seen := map[string]bool{}
	for _, match := range reAppCall.FindAllStringSubmatch(js, -1) {
		name := match[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := appType.MethodByName(name); !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("这些方法被前端调用，但 *App 上没有导出：%s", strings.Join(missing, ", "))
	}
}

// TestFrontendRuntimeMethodsKnown 保证 RT.Xxx() 不是拼错的。
//
// 大小写在这里特别容易翻车：Wails 用的是英式拼写 `Minimise` / `Maximise`，
// 写成美式 `Minimize` 不会报错，只是窗口纹丝不动。
func TestFrontendRuntimeMethodsKnown(t *testing.T) {
	_, js := readFrontend(t)

	var unknown []string
	seen := map[string]bool{}
	for _, match := range reRTCall.FindAllStringSubmatch(js, -1) {
		name := match[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if !wailsRuntimeMethods[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Fatalf("前端调了这些运行时方法，但它们不在白名单里（拼错？）：%s\n"+
			"如果确实是 Wails 的合法方法，把它加进 wailsRuntimeMethods",
			strings.Join(unknown, ", "))
	}
}

// TestFrontendFramelessTitlebarKeepsButtonsClickable 守住无边框窗口最容易踩的坑。
//
// --wails-draggable 是 CSS 自定义属性，**会继承**。标题栏整体设成 drag 之后，
// 按钮所在的那一层如果没显式写 no-drag，点下去就变成拖窗口——按钮永远点不动，
// 而且没有任何报错。窗口是无边框的，这就等于关不掉了。
//
// 只断言**真正决定行为**的两条：标题栏是 drag、按钮容器是 no-drag。
// `.wbtn` 上那份声明是冗余的（它从 `.wbtns` 继承），留着是防重构，所以不在这里断言——
// 让测试因为一个无害的冗余声明变红，只会训练人忽略红灯。
func TestFrontendFramelessTitlebarKeepsButtonsClickable(t *testing.T) {
	html, _ := readFrontend(t)

	// 标题栏本身得是 drag，否则窗口拖不动。
	bar := cssRule(t, html, ".titlebar")
	if !strings.Contains(bar, "drag") || strings.Contains(bar, "no-drag") {
		t.Fatal(".titlebar 应当声明 --wails-draggable: drag，否则窗口没法拖动")
	}

	// 这一条才是关键：它是按钮往上最近的、声明了该属性的祖先。
	// 漏了它，最小化/最大化/关闭三个按钮全部点不动。
	buttons := cssRule(t, html, ".wbtns")
	if !strings.Contains(buttons, "no-drag") {
		t.Fatal(".wbtns 没有声明 --wails-draggable: no-drag，" +
			"最小化/最大化/关闭按钮会变成拖窗口——窗口是无边框的，等于关不掉")
	}
}

// TestFrontendHasNoNativeDialogs 守住「不用 WebView 原生弹窗」这条。
//
// confirm() / alert() / prompt() 是浅色的系统对话框，从这套深色界面里弹出来
// 像另一个程序。原生 confirm 还挡不住误操作：焦点默认在「确定」上，
// 一个回车就把东西删了。全部换成自绘的 askConfirm。
func TestFrontendHasNoNativeDialogs(t *testing.T) {
	_, js := readFrontend(t)

	// 去掉注释再找：注释里正当地提到了 confirm() / prompt()。
	//
	// 两种注释都要去。只去 `//` 的话，块注释里的「不用原生 prompt()」会被当成
	// 真的调用 —— 踩过一次，改了代码却在测试上卡住，而测试的理由是「注释」。
	body := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(js, "")
	body = regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(body, "")

	for _, name := range []string{"confirm", "alert", "prompt"} {
		if regexp.MustCompile(`\b` + name + `\s*\(`).MatchString(body) {
			t.Errorf("前端还在调 %s()，应该用 askConfirm() 自绘弹窗", name)
		}
	}
}

// TestTitlebarChipShowsNameSeparately 守住顶栏两个状态芯片的文案结构。
//
// shazam-sidecar 的 `--env` 只报版本号（"0.1.0"），产品名是界面的固定标签。
// 踩过：预览桩里把 sidecarVersion 编成 "shazam-sidecar 0.3.1 (rodio)"，
// 于是截图里看着挺好，真机上顶栏只有一个孤零零的 "0.1.0"。
func TestTitlebarChipShowsNameSeparately(t *testing.T) {
	html, js := readFrontend(t)

	for _, want := range []string{
		`<span class="chip-name">shazam-sidecar</span>`,
		`<span class="chip-name">ffmpeg</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("顶栏里找不到固定的名称标签 %s", want)
		}
	}

	// 值那一侧不能再自己拼产品名，否则会变成 "shazam-sidecar shazam-sidecar 0.1.0"。
	if strings.Contains(js, "'shazam-sidecar ' +") || strings.Contains(js, `"shazam-sidecar " +`) {
		t.Error("chipSidecarText 里在拼产品名，名称已经写在 HTML 标签里了")
	}
}

// TestBottomPanelHasNoTopBorder 守住任务页「中间那条分割线」不再回来。
//
// .bottom（输出日志面板）上面紧挨着 .tablewrap 的底边，两条线只差 13px。
// 同时出现看着就是「列表中间多了个分割」，而它其实什么信息都没表达。
func TestBottomPanelHasNoTopBorder(t *testing.T) {
	html, _ := readFrontend(t)

	block := cssRule(t, html, ".bottom")
	if strings.Contains(block, "border-top") {
		t.Error(".bottom 又加回了 border-top —— 表格底边就在它上面 13px，两条线会看着像分割")
	}
}

// TestFrontendDoesNotHardcodeNamingOptions 守住「变量目录只有 Go 那一份」。
//
// 老实现是前后端各写一份分隔符清单，靠一个比对测试钉住它们一致。
// 现在改成界面**从后端取**（NamingOptions），前端那份清单已经删掉了 ——
// 复制没了，就不需要那个测试了。这条守的是「别再写回去」。
//
// 为什么这件事重要：变量名是前后端之间的字符串约定，
// `{title:zh}` 在前端写成 `{title:cn}` 不会有任何编译错误，
// 只会在用户点保存之后才悄悄出错。
func TestFrontendDoesNotHardcodeNamingOptions(t *testing.T) {
	html, js := readFrontend(t)

	// 变量名只该出现在注释里（解释渲染规则时），不该出现在可执行代码里。
	for _, forbidden := range []string{
		`const SEPARATORS = [`,
		`const VARIABLES = [`,
		`const SCRIPTS = [`,
	} {
		if strings.Contains(js, forbidden) {
			t.Errorf("index.html 里又出现了 %s —— 变量目录应当从 NamingOptions 取", forbidden)
		}
	}

	// 分隔符快捷按钮撤掉之后，两边都不该再有那份符号清单。
	if strings.Contains(js, `'-', '_', '.'`) || strings.Contains(js, `"-", "_", "."`) {
		t.Error("index.html 里又硬编码了分隔符候选")
	}
	if strings.Contains(html, `id="sepPicks"`) {
		t.Error("命名页又长出了分隔符快捷按钮（sepPicks）—— 那一排已经撤掉了")
	}

	// 反向确认：页面确实在用后端给的那份。
	if !strings.Contains(js, "NamingOptions()") {
		t.Error("index.html 没有调用 NamingOptions()，变量目录从哪来？")
	}

	// 槽位编辑器要真的在页面上，不能被换回一个文本框。
	for _, want := range []string{`id="slotList"`, `id="btnAddSlot"`, `id="tplSep"`} {
		if !strings.Contains(html, want) {
			t.Errorf("命名页里找不到 %s —— 槽位编辑器结构变了", want)
		}
	}
	if strings.Contains(html, `id="tplBody"`) {
		t.Error("命名页里又出现了 tplBody 输入框 —— 已经改成槽位编辑了")
	}
}

// TestTemplateLibraryIsGone 守住「不提供多份模板」这条产品决定。
//
// 模板库删干净之后，界面上不该再出现：左栏那份列表、任务页的模板下拉、
// 右键菜单里的重命名 / 复制 / 删除。少删一处，用户就会看到两套互相打架的
// 「命名」入口 —— 而真正生效的只有全局那一份。
func TestTemplateLibraryIsGone(t *testing.T) {
	html, js := readFrontend(t)

	for _, gone := range []string{
		`id="tplList"`,        // 左栏模板列表
		`id="btnNewTemplate"`, // 新建模板
		`id="tplSearch"`,      // 搜模板
		`id="selTemplate"`,    // 任务页的模板下拉
		`id="view-templates"`, // 整块视图
	} {
		if strings.Contains(html, gone) {
			t.Errorf("模板库已经删了，页面里还留着 %s", gone)
		}
	}

	for _, gone := range []string{
		"App.Templates(", "App.SaveTemplate(", "App.DeleteTemplate(",
		"App.RenameTemplate(", "App.DuplicateTemplate(", "App.SetActiveTemplate(",
		"App.ResetTemplates(", "App.PreviewTemplate(", "App.TemplateEditorOptions(",
	} {
		if strings.Contains(js, gone) {
			t.Errorf("前端还在调 %s —— 这些方法已经不存在了", gone)
		}
	}

	// 导航上那一项还在，只是换了名字。
	if !strings.Contains(html, `data-view="naming"`) {
		t.Error("导航里找不到「命名」这一项")
	}
	if !strings.Contains(html, "<span>命名</span>") {
		t.Error("导航项的文字应当叫「命名」")
	}
}

// TestProxyDirectButtonIsNotBlank 守住「强制直连」填的是 `-`，不是留空。
//
// 这是代理设置里唯一一个反直觉的地方：输入框留空表示「跟随 HTTP_PROXY /
// HTTPS_PROXY 环境变量」，而想绕开系统代理的用户会本能地按「留空」。
// 如果这个按钮真的把输入框清空，它做的就和用户期望完全相反，而且不会有任何
// 报错——用户以为自己在直连，流量其实还是从代理走。
//
// 整条链路上这个语义只由一个字符承载（`-`），所以值得单独钉住：
// 前端写 `-` → recognizer 原样传 `--proxy -` → sidecar 转成 no_proxy()。
func TestProxyDirectButtonIsNotBlank(t *testing.T) {
	html, js := readFrontend(t)

	for _, want := range []string{
		`id="cfgProxy"`,
		`id="btnProxyDirect"`,
		`id="btnTestNetwork"`,
		`id="proxyState"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("设置页网络分组里找不到 %s，结构变了就更新这个测试", want)
		}
	}

	if !strings.Contains(js, `el('cfgProxy').value = '-'`) {
		t.Error("「强制直连」按钮没有把 cfgProxy 设成 '-'。" +
			"留空是「跟随环境变量」，是另一件事——设了 HTTP_PROXY 的用户按它会绕不开代理，且没有任何提示")
	}
	if strings.Contains(js, `el('cfgProxy').value = ''`) {
		t.Error("有代码在把 cfgProxy 清空。清空等于「跟随环境变量」，不是「直连」")
	}

	// 「测试连接」必须真的走后端。在前端编一个「连接正常」的话，
	// 这个按钮就成了纯粹的安慰剂——而它的全部价值就在于戳穿配错的代理。
	if !strings.Contains(js, "App.TestNetwork(") {
		t.Error("「测试连接」没有调用 App.TestNetwork()，它是自己编的结果吗？")
	}
}

// TestStubNamingOptionsMatchGo 守住预览桩的槽位目录和后端一致。
//
// 桩是**不得不**复制一份的：静态预览页面跑在浏览器里，没有 Go 运行时。
// 既然复制躲不掉，就让两份桩共用**同一个** JSON（tools/preview/naming-options.json），
// 并由这条测试逐项比对 —— 否则桩会慢慢漂移，
// 预览页面看着正常、真机上却是另一套变量名，而截图又正是靠桩生成的。
func TestStubNamingOptionsMatchGo(t *testing.T) {
	const path = "tools/preview/naming-options.json"

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s: %v", path, err)
	}

	var stub struct {
		Choices []struct {
			Var    string `json:"var"`
			Script string `json:"script"`
			Label  string `json:"label"`
			Group  string `json:"group"`
		} `json:"choices"`
		DefaultSeparator string `json:"defaultSeparator"`
	}
	if err := json.Unmarshal(raw, &stub); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}

	// 槽位下拉：变量、语种、标签、分组，**连顺序**一起比。
	// 顺序也是约定的一部分——界面上的下拉就是按这个顺序铺的，
	// 桩的顺序不对，截图里看到的选项次序就和真机不一样。
	want := naming.Choices()
	if len(stub.Choices) != len(want) {
		t.Fatalf("下拉选项数量不一致：桩 %d 个，后端 %d 个", len(stub.Choices), len(want))
	}
	for i, choice := range want {
		got := stub.Choices[i]
		if got.Var != choice.Var || got.Script != choice.Script {
			t.Errorf("第 %d 项指向的位置不一致：桩 %s/%s，后端 %s/%s",
				i, got.Var, got.Script, choice.Var, choice.Script)
		}
		if got.Label != choice.Label {
			t.Errorf("第 %d 项（%s/%s）的标签不一致：桩 %q，后端 %q",
				i, choice.Var, choice.Script, got.Label, choice.Label)
		}
		if got.Group != choice.Group {
			t.Errorf("第 %d 项（%s/%s）的分组不一致：桩 %q，后端 %q",
				i, choice.Var, choice.Script, got.Group, choice.Group)
		}
	}

	// 默认分隔符：新建模板时的起手值，前端直接拿它填输入框。
	if stub.DefaultSeparator != naming.DefaultSeparator {
		t.Errorf("默认分隔符不一致：桩 %q，后端 %q", stub.DefaultSeparator, naming.DefaultSeparator)
	}
}

// TestPreviewStubMatchesRealSidecarShape 守住「预览桩不许编和真机不一样的形状」。
//
// 踩过的坑：桩里把 sidecarVersion 写成 'shazam-sidecar 0.3.1 (rodio)'，
// 于是截图里顶栏显示成带产品名的样子，看着挺好；真机上只有 "0.1.0"。
// 预览的全部价值就在于「看到的和真机一样」，桩一旦编数据就是在骗人，
// 而且骗得越像越难发现。
//
// 真实形状由 internal/job 的 TestIntegrationEnvVersionIsBareNumber 钉住。
func TestPreviewStubMatchesRealSidecarShape(t *testing.T) {
	const (
		stubFile  = "tools/preview/stub.js"
		shotsFile = "tools/shoot_ui.py"
	)

	pattern := regexp.MustCompile(`sidecarVersion:\s*'([^']*)'`)

	for _, file := range []string{stubFile, shotsFile} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("读不到 %s: %v", file, err)
		}

		match := pattern.FindStringSubmatch(string(raw))
		if match == nil {
			t.Errorf("%s 里找不到 sidecarVersion，结构变了就更新这个测试", file)
			continue
		}

		// 真实值只有一个版本号。带空格说明又把产品名拼进去了。
		if strings.ContainsAny(match[1], " \t") {
			t.Errorf("%s 的 sidecarVersion = %q，带空格。"+
				"真机上是 `--env` 报的纯版本号（形如 \"0.1.0\"），产品名写在 HTML 的 chip-name 里",
				file, match[1])
		}
	}
}

// TestPreviewStubsProvideEveryRuntimeMethodTheFrontendCalls 守住两个静态桩的**运行时方法**齐不齐。
//
// 为什么值得单开一条：漏一个方法的后果不是「这一处不好使」，而是
// **整个页面的脚本全废**。前端在启动时同步调用 RT.OnFileDrop，
// 桩里没有它就抛 TypeError，而抛出点在注册导航点击、调用 boot() **之前** ——
// 于是导航点不动、data-boot 永远不出现、模板加载不执行，
// 页面看起来「能显示但全是死的」，没有任何一处报错指向真正的原因。
//
// 真实踩过：tools/preview/stub.js 漏了 OnFileDrop，`?view=templates` 切不过去，
// 排查时一直以为是时序问题（先怀疑 setTimeout 太短），绕了很久。
// tools/shoot_ui.py 的桩有这个方法，所以截图套件一直是好的 ——
// 两套桩各写一份，漂移就是这么来的。
//
// 断言的是「前端用到的每一个都必须在桩里存在」，不比对方法全集：
// 桩里多几个用不上的无害，少一个才是致命的。
func TestPreviewStubsProvideEveryRuntimeMethodTheFrontendCalls(t *testing.T) {
	_, js := readFrontend(t)

	used := map[string]bool{}
	for _, match := range reRTCall.FindAllStringSubmatch(js, -1) {
		used[match[1]] = true
	}
	if len(used) == 0 {
		t.Fatal("没从 frontend/index.html 里解析出任何 RT.* 调用，正则或结构变了")
	}

	// 桩有两种写法，必须都认：
	//   · 对象字面量：`OnFileDrop: (cb) => {...}` / `OnFileDrop: function () {}`
	//   · 方法简写：  `WindowMinimise() {}`
	// 只认冒号会漏掉简写那一半 —— 第一版就是这么写的，
	// 于是 WindowMinimise 被误报成「缺失」，差点去改一个本来没错的文件。
	blockPattern := regexp.MustCompile(`(?s)window\.runtime\s*=\s*\{(.*?)\n\s*\};`)
	memberPattern := regexp.MustCompile(`([A-Z][A-Za-z]*)\s*(?::|\()`)

	for _, file := range []string{"tools/preview/stub.js", "tools/shoot_ui.py"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("读不到 %s: %v", file, err)
		}

		block := blockPattern.FindStringSubmatch(string(raw))
		if block == nil {
			t.Errorf("%s 里找不到 window.runtime = { ... } 块，结构变了就更新这个测试", file)
			continue
		}

		provided := map[string]bool{}
		for _, match := range memberPattern.FindAllStringSubmatch(block[1], -1) {
			provided[match[1]] = true
		}

		var missing []string
		for name := range used {
			if !provided[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("%s 的 window.runtime 少了这些方法：%s\n"+
				"前端启动时会调它们，缺一个就会抛 TypeError，"+
				"而抛出点在最前面，后面的代码全部不执行 —— 页面会「能显示但全是死的」",
				file, strings.Join(missing, ", "))
		}
	}
}

// TestPreviewStubsProvideEveryAppMethodTheFrontendCalls 守住「桩别漏方法」的另一半。
//
// 上面那条只管 window.runtime（RT.*）；前端还会调 window.go.main.App.*，
// 那一边一直没人查。缺一个方法，调用点抛 TypeError，后面的代码全部不执行，
// 页面「能显示但全是死的」——报错只在控制台，截图看不出来。
//
// 这条是被一次真实的错逼出来的：stub.js 里 Undo 返回的是
// {restored, failed, message}，和 Go 的 UndoResult 对不上，
// 记录页打出来的是「还原 undefined」——方法在不在，得有条测试钉住。
func TestPreviewStubsProvideEveryAppMethodTheFrontendCalls(t *testing.T) {
	_, js := readFrontend(t)

	used := map[string]bool{}
	for _, match := range reAppCall.FindAllStringSubmatch(js, -1) {
		used[match[1]] = true
	}
	if len(used) == 0 {
		t.Fatal("没从 frontend/index.html 里解析出任何 App.* 调用，正则或结构变了")
	}

	for _, file := range []string{"tools/preview/stub.js", "tools/shoot_ui.py"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("读不到 %s: %v", file, err)
		}
		source := string(raw)

		var missing []string
		for name := range used {
			// 成员有两种写法，都要认：`Name: ...`（对象字面量）和 `Name(...)`（方法简写）。
			if !regexp.MustCompile(`\b` + name + `\s*(?::|\()`).MatchString(source) {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("%s 的 App 桩少了这些方法：%s\n"+
				"前端会调它们，缺一个就在调用点抛 TypeError，"+
				"后面的代码全部不执行 —— 预览页会「能显示但全是死的」",
				file, strings.Join(missing, ", "))
		}
	}
}
