package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"videosongrename/internal/config"
	"videosongrename/internal/job"
	"videosongrename/internal/media"
	"videosongrename/internal/naming"
	"videosongrename/internal/recognizer"
	"videosongrename/internal/winproc"
)

// 事件名。前端只认这几个字符串，改这里要同步改 index.html。
const (
	eventItem     = "job:item"
	eventProgress = "job:progress"
	eventLog      = "job:log"
	eventDone     = "job:done"
)

// App 是绑定给前端的对象。方法名即前端的调用名。
type App struct {
	ctx context.Context

	mu      sync.Mutex
	dataDir string
	cfg     config.Config
	manager *job.Manager
	rec     *recognizer.Recognizer

	// iconICO 是托盘图标与窗口图标的原始 .ico 字节。
	iconICO []byte
	// tray 是系统托盘。为 nil 表示没起来（不影响程序其余部分）。
	tray *winTray
	// quitting 为真表示这次关闭是真的退出，不是收进托盘。
	quitting bool
	// hintedTray 保证「已收进托盘」的气泡一个进程只弹一次。
	hintedTray bool

	lastRoot  string
	configErr string
}

// NewApp 建一个应用。sidecar 找不到时不在这里失败——
// 界面要能打开，并把「缺什么」清楚地告诉用户。
func NewApp() *App {
	dataDir := config.DataDir()

	cfg, err := config.Load(dataDir)
	app := &App{
		dataDir: dataDir,
		cfg:     cfg,
	}
	if err != nil {
		app.configErr = err.Error()
	}

	binary, locateErr := recognizer.Locate()
	if locateErr != nil {
		app.rec = nil
	} else {
		app.rec = recognizer.New(binary)
	}

	// 注意不能直接把 nil 指针塞进接口：那会得到一个「非 nil 接口装着 nil 指针」，
	// 调用时才炸。缺 sidecar 是一种正常状态，界面要能正常打开并提示。
	var rec job.Recognizer
	if app.rec != nil {
		rec = app.rec
	}
	app.manager = job.New(cfg, rec, dataDir, job.Callbacks{})
	app.manager.OpenLog()

	return app
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.manager.Close()

	var rec job.Recognizer
	if a.rec != nil {
		rec = a.rec
	}
	a.manager = job.New(a.cfg, rec, a.dataDir, job.Callbacks{
		OnItem: func(item job.Item) {
			wailsRuntime.EventsEmit(a.ctx, eventItem, item)
		},
		OnProgress: func(progress job.Progress) {
			wailsRuntime.EventsEmit(a.ctx, eventProgress, progress)
		},
		OnLog: func(level, message string) {
			a.emitLog(level, message)
		},
		OnDone: func(summary job.Summary) {
			wailsRuntime.EventsEmit(a.ctx, eventDone, summary)
			// 跑完一轮，悬停提示里的状态要跟着变。
			a.refreshTray()
		},
	})
	a.manager.OpenLog()

	// 拖放交给前端注册（runtime.OnFileDrop）。Wails 是**由前端那个调用**
	// 去装 DOM 的 dragover/drop 处理器的，只在 Go 这边注册收不到任何东西——
	// 两边都注册反而会让同一批文件被处理两次。

	a.startTray()
}

func (a *App) shutdown(context.Context) {
	a.manager.Stop()
	a.manager.Close()

	a.mu.Lock()
	tray := a.tray
	a.tray = nil
	a.mu.Unlock()
	if tray != nil {
		tray.Dispose()
	}
}

// emitLog 往前端推一条日志。
func (a *App) emitLog(level, message string) {
	if a.ctx == nil {
		return
	}
	wailsRuntime.EventsEmit(a.ctx, eventLog, map[string]string{
		"level":   level,
		"message": message,
		"time":    time.Now().Format("15:04:05"),
	})
}

// ---------------------------------------------------------------- 设置

// GetConfig 返回当前设置。
func (a *App) GetConfig() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// SaveConfig 保存设置并让下一轮生效。
func (a *App) SaveConfig(cfg config.Config) (config.Config, error) {
	cfg.Normalize()

	if err := config.Save(a.dataDir, cfg); err != nil {
		return a.GetConfig(), err
	}

	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()

	a.manager.SetConfig(cfg)
	return cfg, nil
}

// mutateConfig 在锁外改一份配置副本、落盘、并让下一轮生效。
//
// 先取副本再改，是为了避免在持有 a.mu 的时候去碰 manager（它有自己的锁）
// 和磁盘 IO——那两条都可能在慢盘上卡住，而 a.mu 还挡着界面。
func (a *App) mutateConfig(mutate func(*config.Config) error) error {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()

	if err := mutate(&cfg); err != nil {
		return err
	}
	_, err := a.SaveConfig(cfg)
	return err
}

// ---------------------------------------------------------------- 命名

// NamingRule 是「命名」页的完整内容：一套命名规则 + 处理完之后把文件放哪儿。
//
// 两者放在同一个载荷里，是因为它们回答的是同一个问题——
// 「处理完之后，磁盘上会多出什么」。分成两页两个保存按钮，用户会以为
// 只改了一半，而实际上一次任务两件都会生效。
type NamingRule struct {
	// Separator 是全局分隔符，没单独设分隔符的槽位用它。
	Separator string `json:"separator"`
	// Slots 是排好序的槽位。每格自带可选的语种、分隔符、前后缀。
	Slots []naming.Slot `json:"slots"`
	// UnmatchedPrefix / UnmatchedSuffix 是未识别文件的标记。
	//
	// 它和命名规则在同一页，也应当同一次保存：未识别怎么称呼、
	// 识别到怎么称呼，本来就是「命名」这一个问题的两面。
	UnmatchedPrefix string `json:"unmatchedPrefix"`
	UnmatchedSuffix string `json:"unmatchedSuffix"`
	// MaxNameRunes 是文件名（不含扩展名）的字符数上限。
	MaxNameRunes int `json:"maxNameRunes"`
	// KeepDuplicates 是「重复的照样写两遍」。
	//
	// 默认 false = 去重。界面上那颗勾选框是**反着**绑的（勾上 = 去重），
	// 因为用户想的是「重复的别重复写」，而不是「保留重复」。
	KeepDuplicates bool `json:"keepDuplicates"`
	// Move 是归档规则。
	Move config.MoveRule `json:"move"`
}

// spec 把界面上的槽位还原成渲染规则。
func (r NamingRule) spec() naming.Template {
	return naming.Template{
		Separator:      r.Separator,
		Slots:          r.Slots,
		KeepDuplicates: r.KeepDuplicates,
	}
}

// NamingOptions 是命名页要用的全部静态选项。
//
// 变量目录放在 Go 这边，前端不再各写一份：
// 前后端之间是字符串约定，`{title:zh}` 在前端写成 `{title:cn}` 不会有任何编译错误，
// 只会在用户点保存之后才悄悄出错。
type NamingOptions struct {
	// Choices 是槽位下拉框的选项，已经拍平成「中文歌名」这样的成品名。
	Choices []naming.Choice `json:"choices"`
	// DefaultSeparator 是换到一份空规则时预填的分隔符。
	DefaultSeparator string `json:"defaultSeparator"`
}

// NamingOptions 返回命名页的静态选项。
func (a *App) NamingOptions() NamingOptions {
	return NamingOptions{
		Choices:          naming.Choices(),
		DefaultSeparator: naming.DefaultSeparator,
	}
}

// PreviewCase 是一条命名预览。
type PreviewCase struct {
	Label string `json:"label"`
	Note  string `json:"note"`
	Text  string `json:"text"`
	// Empty 表示这份样本下模板渲染不出任何东西，会退回原文件名。
	Empty bool `json:"empty"`
}

// previewSample 是预览用的一组样本数据。
type previewSample struct {
	label  string
	note   string
	fields naming.Fields
	// matched 为假表示「压根没识别到」，会走未识别前缀那条路。
	matched bool
}

// previewSamples 返回预览样本。
//
// 第二组刻意就是「缺中文歌名」——那正是用户报的场景。
// 预览里直接把它摆出来，用户不用在脑子里推理「中文没有会怎样」。
func previewSamples() []previewSample {
	withBoth := naming.Fields{
		Artist: "BTS",
		Title:  "Spring Day",
		Album:  "You Never Walk Alone",
		Year:   "2017",
		Titles: map[naming.Script]string{
			naming.ScriptEnglish: "Spring Day",
			naming.ScriptChinese: "春日",
		},
		Artists: map[naming.Script]string{naming.ScriptEnglish: "BTS"},
	}

	englishOnly := naming.Fields{
		Artist:  "BTS",
		Title:   "Spring Day",
		Album:   "You Never Walk Alone",
		Year:    "2017",
		Titles:  map[naming.Script]string{naming.ScriptEnglish: "Spring Day"},
		Artists: map[naming.Script]string{naming.ScriptEnglish: "BTS"},
	}

	titleOnly := naming.Fields{
		Title:  "Spring Day",
		Titles: map[naming.Script]string{naming.ScriptEnglish: "Spring Day"},
	}

	const sampleFile = "VID_20240103_210455.mp4"

	return []previewSample{
		{label: "全都识别到", note: "艺人、英文名、中文名、年份都有", fields: withBoth, matched: true},
		{label: "缺中文歌名", note: "只有英文写法 —— 中文那一格整个消失，不留分隔符", fields: englishOnly, matched: true},
		{label: "只识别到歌名", note: "艺人和专辑都没识别到", fields: titleOnly, matched: true},
		{label: "完全没识别到", note: "走「未识别」前缀，规则不参与", fields: naming.Fields{Original: sampleFile}, matched: false},
	}
}

// Naming 返回当前的命名规则与归档位置。
func (a *App) Naming() NamingRule {
	a.mu.Lock()
	defer a.mu.Unlock()
	return NamingRule{
		Separator:       a.cfg.NameTemplate.Separator,
		Slots:           append([]naming.Slot(nil), a.cfg.NameTemplate.Slots...),
		UnmatchedPrefix: a.cfg.UnmatchedPrefix,
		UnmatchedSuffix: a.cfg.UnmatchedSuffix,
		MaxNameRunes:    a.cfg.MaxNameRunes,
		KeepDuplicates:  a.cfg.NameTemplate.KeepDuplicates,
		Move:            a.cfg.Move,
	}
}

// SaveNaming 保存命名规则与归档位置。
func (a *App) SaveNaming(input NamingRule) (NamingRule, error) {
	spec := input.spec()
	spec.Normalize()
	// 一格都不剩的规则等于没规则。存下去会被 Normalize 换回默认值，
	// 用户看到的是「保存成功了但名字还是老样子」——不如当场说清楚。
	if spec.IsZero() {
		return a.Naming(), errors.New("至少要留一个槽位")
	}

	move := input.Move
	move.Normalize()
	// 用一条样例路径试着算一次：算得出来就说明填的东西够用。
	// 不试的话，用户会带着一条永远不生效的归档规则去跑任务，而界面上很安静。
	if move.Enabled && move.TargetDir(filepath.Join("D:", "视频", "2024", "sample.mp4")) == "" {
		return a.Naming(), errors.New("归档位置还没填完整：选一个目录，或者给目录起个名字")
	}

	if err := a.mutateConfig(func(cfg *config.Config) error {
		cfg.NameTemplate = spec
		cfg.UnmatchedPrefix = input.UnmatchedPrefix
		cfg.UnmatchedSuffix = input.UnmatchedSuffix
		cfg.MaxNameRunes = input.MaxNameRunes
		cfg.Move = move
		return nil
	}); err != nil {
		return a.Naming(), err
	}
	return a.Naming(), nil
}

// PreviewNaming 用几组样本把这份规则渲染一遍，给界面做预览。
//
// 预览**在 Go 里算**，不在前端用 JS 重写一遍渲染逻辑。
// 两处各写一份迟早会对不上：用户看到的预览就成了假的，
// 而且越是「看起来没问题」越不容易被发现。
func (a *App) PreviewNaming(input NamingRule) []PreviewCase {
	spec := input.spec()
	spec.Normalize()

	// 用**表单里的**未识别前缀和长度上限，不是已保存的那份：
	// 用户改完想先看一眼，这个顺序是合理的。
	unmatched := naming.Unmatched{Prefix: input.UnmatchedPrefix, Suffix: input.UnmatchedSuffix}
	maxRunes := input.MaxNameRunes
	if maxRunes <= 0 {
		a.mu.Lock()
		maxRunes = a.cfg.MaxNameRunes
		a.mu.Unlock()
	}

	samples := previewSamples()
	out := make([]PreviewCase, 0, len(samples))
	for _, sample := range samples {
		fields := sample.fields
		fields.Original = "VID_20240103_210455.mp4"

		rendered := naming.Apply(spec, unmatched, fields, maxRunes, sample.matched)
		out = append(out, PreviewCase{
			Label: sample.label,
			Note:  sample.note,
			Text:  rendered,
			Empty: spec.Render(fields) == "",
		})
	}
	return out
}

// MovePreview 是归档位置的一次试算。
type MovePreview struct {
	Dir  string `json:"dir"`
	Note string `json:"note"`
	// OK 为假时 Dir 没有意义，界面只显示 Note。
	OK bool `json:"ok"`
}

// PreviewMove 拿一条样例路径算一下文件会落到哪个目录。
//
// 传进来的是**表单里当前的规则**，不是已保存的那份：用户改完想先看一眼再保存，
// 这个顺序是合理的，不该逼他先存一个可能是错的配置。
func (a *App) PreviewMove(rule config.MoveRule, sample string) MovePreview {
	rule.Normalize()
	if !rule.Enabled {
		return MovePreview{Note: "不移动，文件改名后留在原目录"}
	}
	if strings.TrimSpace(sample) == "" || !filepath.IsAbs(sample) {
		sample = filepath.Join("D:", "视频", "2024", "VID_20240103_210455.mp4")
	}

	dir := rule.TargetDir(sample)
	if dir == "" {
		return MovePreview{Note: "还定不出目录：切换到「指定目录」并选一个，或者给同级目录起个名字"}
	}
	return MovePreview{
		Dir:  dir,
		OK:   true,
		Note: "目录不存在会自动创建；遇到同名文件会加 (1)，不会覆盖",
	}
}

// PickMoveFolder 弹目录选择框，给「指定目录」用。
func (a *App) PickMoveFolder() (string, error) {
	return wailsRuntime.OpenDirectoryDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择处理完之后把文件放进哪个目录",
	})
}

// ---------------------------------------------------------------- 窗口与托盘

// setIcon 把内嵌的 .ico 交给 App（托盘和窗口图标共用这一份）。
func (a *App) setIcon(raw []byte) {
	a.mu.Lock()
	a.iconICO = raw
	a.mu.Unlock()
}

// ShowWindow 把主窗口叫回前台。
func (a *App) ShowWindow() {
	if a.ctx == nil {
		return
	}
	wailsRuntime.WindowShow(a.ctx)
	wailsRuntime.WindowUnminimise(a.ctx)
}

// HideWindow 把主窗口收起来，程序继续在托盘里跑。
func (a *App) HideWindow() {
	if a.ctx == nil {
		return
	}
	wailsRuntime.WindowHide(a.ctx)
}

// QuitApp 真正退出程序。
func (a *App) QuitApp() {
	a.mu.Lock()
	a.quitting = true
	a.mu.Unlock()

	if a.ctx != nil {
		wailsRuntime.Quit(a.ctx)
	}
}

// CloseWindow 是标题栏那个 ✕ 的行为：按设置决定收进托盘还是退出。
//
// 系统层面的关闭（Alt+F4）走 beforeClose，两条路径共用同一份判断。
func (a *App) CloseWindow() {
	a.mu.Lock()
	closeToTray := a.cfg.CloseToTray
	a.mu.Unlock()

	if !closeToTray {
		a.QuitApp()
		return
	}

	a.HideWindow()
	a.hintHiddenOnce()
}

// beforeClose 在窗口即将关闭时被调用，返回 true 表示拦下这次关闭。
func (a *App) beforeClose(ctx context.Context) bool {
	a.mu.Lock()
	closeToTray := a.cfg.CloseToTray
	quitting := a.quitting
	a.mu.Unlock()

	if quitting || !closeToTray {
		return false
	}

	wailsRuntime.WindowHide(ctx)
	a.hintHiddenOnce()
	return true
}

// hintHiddenOnce 第一次收进托盘时弹个气泡。
//
// 无控制台的构建里没有 stdout，用户点完 ✕ 如果窗口就这么消失了、
// 任务栏上又没有图标，他会以为程序崩了。气泡是唯一能说明白的通路。
func (a *App) hintHiddenOnce() {
	a.mu.Lock()
	already := a.hintedTray
	a.hintedTray = true
	tray := a.tray
	a.mu.Unlock()

	if already || tray == nil {
		return
	}
	tray.Balloon(trayAppName, "程序已收进托盘，继续在后台运行。双击托盘图标可以重新打开窗口。")
}

// trayState 是给托盘菜单用的状态快照。每次弹菜单现取。
func (a *App) trayState() trayStatus {
	items := a.manager.Items()
	progress := a.manager.Progress()

	pending := 0
	for _, item := range items {
		// 被限流的也算「还有事要做」：托盘那颗「开始」按的就是「接着跑」，
		// 把它漏掉的话，整批被限流的队列在托盘里会显示成「全部完成」。
		if item.Status == job.StatusPending ||
			item.Status == job.StatusFailed ||
			item.Status == job.StatusRateLimited {
			pending++
		}
	}

	running := a.manager.IsRunning()
	paused := a.manager.IsPaused()

	// 文案要短：菜单是一扫而过的地方，超过十几个字就读不完了。
	var status string
	switch {
	case running && paused:
		status = "任务已暂停"
	case running && progress.Total > 0:
		status = fmt.Sprintf("处理中 %d/%d", progress.Done, progress.Total)
	case running:
		status = "处理中"
	case len(items) == 0:
		status = "队列是空的"
	case pending > 0:
		status = fmt.Sprintf("%d 个待处理", pending)
	default:
		status = fmt.Sprintf("%d 个任务已完成", len(items))
	}

	return trayStatus{
		Status:   status,
		Running:  running,
		Paused:   paused,
		CanStart: pending > 0,
	}
}

// trayToggle 按当前状态决定是「开始」「暂停」还是「继续」。
func (a *App) trayToggle() {
	if a.manager.IsRunning() {
		if a.manager.IsPaused() {
			a.manager.Resume()
		} else {
			a.manager.Pause()
		}
		a.refreshTray()
		return
	}

	if err := a.Start(string(job.ScopeResume)); err != nil {
		a.mu.Lock()
		tray := a.tray
		a.mu.Unlock()
		if tray != nil {
			tray.Balloon(trayAppName, err.Error())
		}
		a.emitLog("ERROR", err.Error())
	}
	a.refreshTray()
}

// refreshTray 让托盘重新读一次悬停提示。没有托盘时什么都不做。
func (a *App) refreshTray() {
	a.mu.Lock()
	tray := a.tray
	a.mu.Unlock()
	if tray != nil {
		tray.Refresh()
	}
}

// startTray 起系统托盘。失败只记日志，不阻断程序——
// 托盘没了还能用窗口，反过来就不成立了。
func (a *App) startTray() {
	a.mu.Lock()
	icon := a.iconICO
	a.mu.Unlock()

	if len(icon) == 0 {
		a.emitLog("WARN", "没有内嵌图标，跳过托盘")
		return
	}

	tray, err := startTray(trayCallbacks{
		Icon:    func() []byte { return icon },
		Tooltip: func() string { return trayAppName + " · " + a.trayState().Status },
		Menu:    a.trayState,
		Show:    a.ShowWindow,
		Toggle:  a.trayToggle,
		OpenData: func() {
			if err := a.OpenConfigFolder(); err != nil {
				a.emitLog("ERROR", "打开数据目录失败："+err.Error())
			}
		},
		Quit: a.QuitApp,
		Log:  func(message string) { a.emitLog("INFO", message) },
	})
	if err != nil {
		a.emitLog("WARN", "托盘图标没起来："+err.Error()+"（程序照常运行）")
		return
	}

	a.mu.Lock()
	a.tray = tray
	a.mu.Unlock()
}

// ---------------------------------------------------------------- 选路径

// PickFolder 弹目录选择框。
func (a *App) PickFolder() (string, error) {
	return wailsRuntime.OpenDirectoryDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择包含视频/音频的文件夹",
	})
}

// PickFiles 弹文件选择框，支持多选。
func (a *App) PickFiles() ([]string, error) {
	return wailsRuntime.OpenMultipleFilesDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择视频/音频文件",
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "媒体文件", Pattern: "*.mp4;*.mkv;*.avi;*.mov;*.flv;*.wmv;*.webm;*.m4v;*.ts;*.mp3;*.wav;*.flac;*.m4a;*.aac;*.ogg;*.wma"},
			{DisplayName: "全部文件", Pattern: "*.*"},
		},
	})
}

// ---------------------------------------------------------------- 扫描

// ScanPath 扫描一个文件夹或文件，返回列表。
func (a *App) ScanPath(root string) ([]job.Item, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("路径是空的")
	}
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("路径不可用: %w", err)
	}

	items, err := a.manager.Scan(root)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.lastRoot = root
	a.mu.Unlock()

	return items, nil
}

// AddPathsResult 是拖入一批路径之后的结果。
type AddPathsResult struct {
	Items []job.Item `json:"items"`
	// Added 是这次真正新增的条目数。
	Added int `json:"added"`
	// Rejected 是被挡下的路径，形如「文件名：原因」。
	//
	// 必须回报：拖了一堆东西进来却什么都没发生，用户没法知道是程序坏了
	// 还是那些文件本来就不该处理。
	Rejected []string `json:"rejected"`
	// Expanded 是这次被**展开**的文件夹，形如「路径（展开出 N 个文件）」。
	//
	// 拖一个文件进来只该多一条；一次多出一大片时，用户得能看出来是哪一步
	// 带进来的 —— 否则「列表自己长出来了」就是唯一的解释。
	Expanded []string `json:"expanded"`
}

// AddPaths 接收拖进来的路径：文件夹展开扫描，文件直接入列，结果并入现有列表。
func (a *App) AddPaths(paths []string) (AddPathsResult, error) {
	if len(paths) == 0 {
		return AddPathsResult{}, errors.New("没有收到路径")
	}

	before := len(a.manager.Items())

	var list []job.Item
	var rejected []string
	var expanded []string
	previous := before
	for _, path := range paths {
		merged, err := a.manager.Append(path)
		if err != nil {
			rejected = append(rejected, filepath.Base(path)+"："+scanReason(err))
			continue
		}
		delta := len(merged) - previous
		previous = len(merged)
		list = merged

		// 拖进来的如果是个**文件夹**，这一步会一次带进来一批。报出来：
		// 不报的话用户只看到列表凭空多出一堆文件，不知道是哪一下带进来的。
		if delta > 1 {
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				expanded = append(expanded, fmt.Sprintf("%s（展开出 %d 个文件）", path, delta))
			}
		}
	}

	if list == nil {
		return AddPathsResult{Rejected: rejected},
			errors.New("拖进来的东西里没有可处理的媒体文件")
	}

	a.mu.Lock()
	if len(list) > 0 {
		a.lastRoot = filepath.Dir(list[0].Path)
	}
	a.mu.Unlock()

	return AddPathsResult{
		Items:    list,
		Added:    len(list) - before,
		Rejected: rejected,
		Expanded: expanded,
	}, nil
}

// scanReason 把扫描错误翻成一句给人看的话。
func scanReason(err error) string {
	switch {
	case errors.Is(err, media.ErrNotMedia):
		return "不是支持的媒体类型"
	case errors.Is(err, media.ErrSkipped):
		return "已被「跳过已处理」挡下"
	}
	message := err.Error()
	if index := strings.IndexAny(message, "\r\n"); index >= 0 {
		message = message[:index]
	}
	return message
}

// Items 返回当前列表，用于界面刷新后重建。
func (a *App) Items() []job.Item { return a.manager.Items() }

// ResyncQueue 让队列跟上磁盘：把「文件已经换了名字」的条目对回实际路径。
//
// 它**不会往队列里加文件**，也不会删 —— 只在已经入列的条目上刷新路径与名字。
// 还原之后前端调它，而不是去重扫目录（见 job.Manager.ResyncQueue 的长注释：
// 重扫目录会把同目录其他所有视频一起拉进队列，那正是用户报的问题）。
func (a *App) ResyncQueue() []job.Item { return a.manager.ResyncQueue() }

// Progress 返回当前进度。
func (a *App) Progress() job.Progress { return a.manager.Progress() }

// Clear 清空列表。
func (a *App) Clear() { a.manager.Clear() }

// Reset 把队列里所有条目清回「待处理」，用于整批重新开始。
//
// 只清状态，不动磁盘上的文件，也不碰 state.json（下次重新扫描时
// 「跳过已处理」的判断照旧按那份文件来）。
func (a *App) Reset() ([]job.Item, error) {
	return a.manager.Reset()
}

// ---------------------------------------------------------------- 执行

// Start 开始识别。scope 取 pending / failed / all。
func (a *App) Start(scope string) error {
	if a.rec == nil {
		return errors.New("找不到 shazam-sidecar.exe。把它放在本程序同一个目录里再试")
	}

	chosen := job.Scope(strings.TrimSpace(scope))
	switch chosen {
	case job.ScopePending, job.ScopeFailed, job.ScopeAll, job.ScopeResume:
	default:
		chosen = job.ScopeResume
	}

	// 在后台跑，方法立刻返回，进度靠事件推。
	go func() {
		if err := a.manager.Start(context.Background(), chosen); err != nil {
			wailsRuntime.EventsEmit(a.ctx, eventLog, map[string]string{
				"level":   "ERROR",
				"message": err.Error(),
				"time":    time.Now().Format("15:04:05"),
			})
			wailsRuntime.EventsEmit(a.ctx, eventDone, job.Summary{Reason: err.Error()})
		}
	}()
	a.refreshTray()
	return nil
}

// Stop 请求停止。
func (a *App) Stop() {
	a.manager.Stop()
	a.refreshTray()
}

// Pause 软暂停：当前文件跑完后不再取新文件。
func (a *App) Pause() {
	a.manager.Pause()
	a.refreshTray()
}

// Resume 解除暂停。
func (a *App) Resume() {
	a.manager.Resume()
	a.refreshTray()
}

// IsPaused 报告是否暂停中。
func (a *App) IsPaused() bool { return a.manager.IsPaused() }

// History 返回改名记录，最新的在前。
func (a *App) History() ([]job.HistoryEntry, error) { return a.manager.History() }

// RemoveHistory 把指定的记录从记录页拿掉，参数是每条记录**现名**的完整路径。
//
// 只动记录，不碰磁盘：文件本身和它现在的名字都不变，代价是拿掉之后就
// 再也还原不回原名了。所以前端那颗按钮带确认，文案里也写明了这一点。
func (a *App) RemoveHistory(targets []string) ([]job.HistoryEntry, error) {
	return a.manager.RemoveHistory(targets)
}

// RemoveItems 把指定下标的条目移出队列。只动队列，不碰磁盘。
func (a *App) RemoveItems(indices []int) []job.Item { return a.manager.Remove(indices) }

// PreviewCommand 返回某个文件会执行的那条 sidecar 命令，供「命令预览」显示。
//
// 排错时这是最有用的东西：能不能识别是算法的事，跑没跑对是这条命令的事。
func (a *App) PreviewCommand(path string) (string, error) {
	if a.rec == nil {
		return "", errors.New("找不到 shazam-sidecar.exe")
	}
	if strings.TrimSpace(path) == "" {
		return "", errors.New("先选一个文件")
	}

	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()

	return a.rec.CommandLine(recognizer.Options{
		Path:           path,
		SegmentSeconds: cfg.SegmentSeconds,
		Language:       cfg.Language,
		Country:        cfg.Country,
		UserAgent:      cfg.UserAgent,
		TimeoutSeconds: cfg.TimeoutSeconds,
		FFmpegPath:     cfg.FFmpegPath,
		Proxy:          cfg.Proxy,
		Locales:        cfg.ExtraLocales(),
	}), nil
}

// TestNetwork 按给定的代理设置真的问一次 Shazam，把结果回报给界面。
//
// 传进来的 proxy 是**表单里当前的值**，不是已保存的值——用户改完代理想先
// 试一下再保存，这个顺序是合理的，不该强迫他先存一个可能是错的配置。
func (a *App) TestNetwork(proxy string) (recognizer.NetworkCheck, error) {
	if a.rec == nil {
		return recognizer.NetworkCheck{}, errors.New("找不到 shazam-sidecar.exe")
	}

	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()

	// 用表单里的代理，其余参数沿用已保存的设置。
	check, err := a.rec.CheckNetwork(a.ctx, proxy, cfg.TimeoutSeconds, cfg.UserAgent)
	if err != nil {
		return check, err
	}
	if check.OK {
		a.emitLog("INFO", fmt.Sprintf("代理连通性正常（%s，HTTP %d，%d ms）",
			check.Proxy, check.HTTPStatus, check.ElapsedMS))
	} else {
		a.emitLog("WARN", fmt.Sprintf("代理连通性失败（%s）：%s", check.Proxy, check.Error))
	}
	return check, nil
}

// Undo 撤销上一次改名。
func (a *App) Undo() (job.UndoResult, error) { return a.manager.Undo() }

// UndoOne 把单个文件改回原名，参数是它**现在的完整路径**。
//
// 用路径而不是列表下标：记录页的列表会随时间重排，下标是会漂的。
func (a *App) UndoOne(to string) (job.UndoResult, error) { return a.manager.UndoOne(to) }

// UndoMany 把勾选的多条记录一起改回原名，参数同样是每条**现在的完整路径**。
//
// 收成一个数组参数、一次调用，而不是让前端循环调 UndoOne：
// 每调一次都要把整份记录读出来重放一遍，勾几十行就是几十遍；
// 而且链式改名要求「倒着还原」，分散成多次调用就没法保证这个顺序。
func (a *App) UndoMany(to []string) (job.UndoResult, error) { return a.manager.UndoMany(to) }

// ExportCSV 弹保存框并把列表写成 CSV。
func (a *App) ExportCSV() (string, error) {
	path, err := wailsRuntime.SaveFileDialog(a.ctx, wailsRuntime.SaveDialogOptions{
		Title:           "导出识别结果",
		DefaultFilename: "识别结果.csv",
		Filters:         []wailsRuntime.FileFilter{{DisplayName: "CSV", Pattern: "*.csv"}},
	})
	if err != nil || path == "" {
		return "", err
	}
	if err := a.manager.ExportCSV(path); err != nil {
		return "", err
	}
	return path, nil
}

// ---------------------------------------------------------------- 自检

// SelfCheckResult 是启动自检的结果，界面用它决定要不要红字提示。
type SelfCheckResult struct {
	OK             bool   `json:"ok"`
	SidecarPath    string `json:"sidecarPath"`
	SidecarVersion string `json:"sidecarVersion"`
	FFmpegPath     string `json:"ffmpegPath"`
	// FFmpegConfigured 是设置里填的那份。跟 FFmpegPath 不一样就说明填的那份没跑起来。
	FFmpegConfigured string `json:"ffmpegConfigured"`
	DataDir          string `json:"dataDir"`
	ConfigPath       string `json:"configPath"`
	LogPath          string `json:"logPath"`
	Problem          string `json:"problem"`
	Hint             string `json:"hint"`
	ConfigError      string `json:"configError"`
}

// SelfCheck 检查 sidecar、ffmpeg、配置目录是否就位。
//
// ffmpeg 只是「加分项」：rodio 能读的格式不需要它，读不了的容器
// （.wma / .m4a / .aac）才会回退到它。
func (a *App) SelfCheck() SelfCheckResult {
	result := SelfCheckResult{
		DataDir:     a.dataDir,
		ConfigPath:  filepath.Join(a.dataDir, config.ConfigFileName),
		LogPath:     filepath.Join(a.dataDir, config.LogFileName),
		ConfigError: a.configErr,
	}

	if a.rec != nil {
		result.SidecarPath = a.rec.Binary()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// --env 一次把版本和「它实际会用的 ffmpeg」都问出来。
		// ffmpeg 是 sidecar 在用，路径就该由它自己报——Go 这边再实现一遍
		// 探测规则的话，两边迟早会走偏，界面上就会显示一个其实没用上的路径。
		if env, err := a.rec.Env(ctx); err == nil {
			result.SidecarVersion = env.Version
			result.FFmpegPath = env.FFmpegPath
		} else if version, err := a.rec.Version(ctx); err == nil {
			result.SidecarVersion = version
		}
	}

	a.mu.Lock()
	configuredFFmpeg := a.cfg.FFmpegPath
	a.mu.Unlock()
	result.FFmpegConfigured = configuredFFmpeg

	switch {
	case a.rec == nil:
		result.Problem = "缺少识别程序 shazam-sidecar.exe"
		result.Hint = "把 shazam-sidecar.exe 放到本程序同一个目录，然后重启"
	case result.SidecarVersion == "":
		result.Problem = "识别程序存在但跑不起来"
		result.Hint = "检查 " + result.SidecarPath + " 是否被杀毒软件拦截"
	default:
		result.OK = true
		switch {
		case configuredFFmpeg != "" && result.FFmpegPath == "":
			// 填了路径却跑不起来——这个必须说出来。悄悄回退到 PATH 上的另一个，
			// 用户会以为自己填的那份在生效。
			result.Hint = "设置的 ffmpeg 路径跑不起来：" + configuredFFmpeg + "。.wma 与部分 .m4a 会读不了"
		case result.FFmpegPath == "":
			result.Hint = "没有找到 ffmpeg：mp4/mkv/mp3/flac/wav/ogg 不受影响，.wma 与部分 .m4a 会读不了"
		}
	}
	return result
}

// PickFFmpeg 弹文件选择框挑一个 ffmpeg.exe。
func (a *App) PickFFmpeg() (string, error) {
	return wailsRuntime.OpenFileDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "选择 ffmpeg 可执行文件",
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "可执行文件", Pattern: "*.exe"},
			{DisplayName: "全部文件", Pattern: "*.*"},
		},
	})
}

// OpenLog 用系统默认程序打开日志文件。
func (a *App) OpenLog() error {
	path := filepath.Join(a.dataDir, config.LogFileName)
	if _, err := os.Stat(path); err != nil {
		return errors.New("还没有日志文件")
	}
	// cmd.exe 是控制台程序，界面没有控制台 —— 不设这一句会先弹一个黑框再消失。
	// （下面的 explorer 不用管：它是 GUI 程序，本来就不会建控制台。）
	command := exec.Command("cmd", "/c", "start", "", path)
	winproc.NoConsole(command)
	return command.Start()
}

// OpenConfigFolder 打开配置所在目录。
func (a *App) OpenConfigFolder() error {
	return exec.Command("explorer", a.dataDir).Start()
}

// RevealPath 在资源管理器里选中一个文件。
func (a *App) RevealPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("路径是空的")
	}
	// 这一句不只是给用户看「文件已经不在了」：它同时保证了下面手拼的命令行里
	// 不会出现 `"`（Windows 文件名里不允许有），所以不存在注入面。
	if _, err := os.Stat(path); err != nil {
		return errors.New("文件已经不在了")
	}
	// 命令行**必须**由 winproc 手拼，不能让 os/exec 按参数去拼：
	// 路径一带空格，os/exec 会把整个 `/select,路径` 包进引号，explorer 认不出
	// 那个开关，会打开「文档」而不是文件所在目录。详见 winproc.RevealCommand。
	return winproc.RevealCommand(path).Start()
}

// OpenURL 用默认浏览器打开链接（Shazam 页面）。
func (a *App) OpenURL(url string) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return errors.New("只允许 http/https")
	}
	command := exec.Command("cmd", "/c", "start", "", url)
	winproc.NoConsole(command)
	return command.Start()
}
