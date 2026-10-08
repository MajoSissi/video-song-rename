// Package job 是任务编排：扫描、并发识别、改名、撤销、导出。
//
// 它不认识 Wails，也不认识界面——只通过 Callbacks 往外推事件。这样它可以被
// 单元测试直接驱动，界面层换个壳也不用动它。
package job

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"videosongrename/internal/config"
	"videosongrename/internal/media"
	"videosongrename/internal/naming"
	"videosongrename/internal/recognizer"
)

// Status 是一个文件在任务里的状态。
type Status string

const (
	StatusPending  Status = "pending"  // 已入列，还没处理
	StatusRunning  Status = "running"  // 正在识别
	StatusMatched  Status = "matched"  // 识别到曲目
	StatusNoMatch  Status = "no_match" // 没识别到
	StatusPreview  Status = "preview"  // 预览模式：算出了新名字但没落盘
	StatusRenamed  Status = "renamed"  // 已改名
	StatusSkipped  Status = "skipped"  // 跳过（已处理过）
	StatusFailed   Status = "failed"   // 失败（网络/解码/写盘）
	StatusCanceled Status = "canceled" // 被停止
)

// Item 是一个待处理文件，也是推给界面的一行。
type Item struct {
	Index  int    `json:"index"`
	Path   string `json:"path"`
	Dir    string `json:"dir"`
	Name   string `json:"name"`
	Ext    string `json:"ext"`
	Size   int64  `json:"size"`
	Status Status `json:"status"`

	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Album     string `json:"album"`
	Genre     string `json:"genre"`
	Released  string `json:"released"`
	Label     string `json:"label"`
	CoverArt  string `json:"coverArt"`
	ShazamURL string `json:"shazamUrl"`

	// Titles / Artists 是同一首歌在各书写系统下的写法，键是脚本名
	// （english / chinese / japanese / korean / other）。模板里的
	// {title:en} 这类占位符就是从这里取的。
	Titles  map[string]string `json:"titles"`
	Artists map[string]string `json:"artists"`

	NewName  string `json:"newName"`
	NewPath  string `json:"newPath"`
	Message  string `json:"message"`
	Attempts int    `json:"attempts"`
}

// Progress 是总体进度快照。
type Progress struct {
	Total     int  `json:"total"`
	Done      int  `json:"done"`
	Matched   int  `json:"matched"`
	Unmatched int  `json:"unmatched"`
	Failed    int  `json:"failed"`
	Skipped   int  `json:"skipped"`
	Renamed   int  `json:"renamed"`
	Running   bool `json:"running"`
}

// Summary 是任务结束时的汇总。
type Summary struct {
	Total     int    `json:"total"`
	Matched   int    `json:"matched"`
	Unmatched int    `json:"unmatched"`
	Failed    int    `json:"failed"`
	Skipped   int    `json:"skipped"`
	Renamed   int    `json:"renamed"`
	Canceled  bool   `json:"canceled"`
	DryRun    bool   `json:"dryRun"`
	ElapsedMS int64  `json:"elapsedMs"`
	Reason    string `json:"reason"`
}

// Callbacks 是往外推事件的钩子。任何一个都可以为 nil。
type Callbacks struct {
	OnItem     func(Item)
	OnProgress func(Progress)
	OnLog      func(level, message string)
	OnDone     func(Summary)
}

// UndoResult 是一次撤销的结果。
type UndoResult struct {
	Restored int      `json:"restored"`
	Missing  int      `json:"missing"`
	Skipped  int      `json:"skipped"`
	Details  []string `json:"details"`
}

// stateEntry 记录一个文件处理过的痕迹，用于「跳过已处理」。
type stateEntry struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
	Status  string `json:"status"`
}

// undoEntry 是 undo.jsonl 里的一行。
//
// 两种形态，靠字段区分：
//
//	改名：{"from":"…","to":"…","id":"…","ts":…}
//	还原：{"restore":"…","ts":…}   —— 这个 to 已经被还原回原名了
//
// 用「追加一行还原记录」而不是「回写/删掉原来那行」，是因为这个文件是纯追加的：
// 中途崩溃最多丢掉最后一行，不会把整份记录写坏（用户最不能接受的就是
// 「改了一堆文件，记录还没了」）。代价是读的时候要按顺序重放一遍。
type undoEntry struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// ID 是改名**之后**那个文件的身份（卷序列号 + FileId，见 fileIdentity）。
	//
	// 改名不改变身份，所以 from 和 to 是同一个值，存一份就够。
	// 老记录没有这个字段，读出来是空串 —— 那时退回按路径匹配，行为不变。
	ID string `json:"id,omitempty"`
	// Restore 非空表示这是一条还原记录，值是**被还原掉的那个 to**。
	Restore string `json:"restore,omitempty"`
	TS      int64  `json:"ts"`
}

// Recognizer 是编排层对识别器的全部要求。
//
// 收成接口是为了让测试能塞一个假的进来：真实识别要联网，跑不动也不该在
// 单元测试里跑。改名、冲突、跳过、撤销这些逻辑一条都不依赖网络。
type Recognizer interface {
	Recognize(ctx context.Context, opts recognizer.Options) (recognizer.Result, error)
}

// Manager 持有一次会话的全部状态。
type Manager struct {
	mu       sync.Mutex
	items    []Item
	running  bool
	cancel   context.CancelFunc
	progress Progress

	logMu   sync.Mutex
	logFile *os.File

	renameMu sync.Mutex
	reserved map[string]bool

	state   map[string]stateEntry
	stateMu sync.Mutex

	pauseMu sync.Mutex
	paused  bool

	cfg        config.Config
	recognizer Recognizer
	dataDir    string
	callbacks  Callbacks
}

// New 建一个管理器。cfg 会被拷贝一份并归一化，后续改动不影响调用方。
func New(cfg config.Config, rec Recognizer, dataDir string, callbacks Callbacks) *Manager {
	cfg.Normalize()
	return &Manager{
		items:      []Item{},
		reserved:   map[string]bool{},
		state:      map[string]stateEntry{},
		cfg:        cfg,
		recognizer: rec,
		dataDir:    dataDir,
		callbacks:  callbacks,
	}
}

// SetConfig 在任务开始前替换设置。
func (m *Manager) SetConfig(cfg config.Config) {
	cfg.Normalize()
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// Items 返回当前列表的副本。
func (m *Manager) Items() []Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Item, len(m.items))
	copy(out, m.items)
	return out
}

// Progress 返回当前进度快照。
func (m *Manager) Progress() Progress {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.progress
}

// IsRunning 报告是否有任务在跑。
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *Manager) logf(level, format string, args ...any) {
	message := fmt.Sprintf(format, args...)

	m.logMu.Lock()
	if m.logFile != nil {
		fmt.Fprintf(m.logFile, "%s [%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), level, message)
	}
	m.logMu.Unlock()

	if m.callbacks.OnLog != nil {
		m.callbacks.OnLog(level, message)
	}
}

// OpenLog 打开日志文件，失败不阻断流程。
func (m *Manager) OpenLog() {
	path := filepath.Join(m.dataDir, config.LogFileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	m.logMu.Lock()
	m.logFile = file
	m.logMu.Unlock()
}

// Close 释放日志句柄。
func (m *Manager) Close() {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	if m.logFile != nil {
		_ = m.logFile.Close()
		m.logFile = nil
	}
}

// ---------------------------------------------------------------- 扫描

// Scan 建立文件列表，替换掉原有的。root 可以是文件夹，也可以是单个文件。
func (m *Manager) Scan(root string) ([]Item, error) {
	return m.collect(root, true)
}

// Append 扫描并把结果并入现有列表，重复路径会跳过。
//
// 「拖几个文件进来」和「换一个文件夹」是两种意图，不能都走替换：
// 前者要保留已经识别过的结果，否则用户一拖，前面跑完的东西就白跑了。
func (m *Manager) Append(root string) ([]Item, error) {
	return m.collect(root, false)
}

func (m *Manager) collect(root string, replace bool) ([]Item, error) {
	if m.IsRunning() {
		return nil, errors.New("任务正在运行，先停止再重新扫描")
	}

	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	// 「跳过已处理」包含两层：状态文件里记录过的，以及上一轮打成 0_ 前缀的。
	skipPrefix := ""
	if cfg.SkipProcessed {
		skipPrefix = cfg.UnmatchedPrefix
	}

	files, err := media.Scan(root, media.Extensions(cfg.Extensions), cfg.Recursive, skipPrefix)
	if err != nil {
		return nil, err
	}

	m.loadState()

	m.mu.Lock()
	if replace {
		m.items = make([]Item, 0, len(files))
		m.reserved = map[string]bool{}
	}

	seen := make(map[string]bool, len(m.items)+len(files))
	for _, item := range m.items {
		seen[strings.ToLower(filepath.Clean(item.Path))] = true
	}

	added := 0
	for _, file := range files {
		key := strings.ToLower(filepath.Clean(file.Path))
		if seen[key] {
			continue
		}
		seen[key] = true

		item := Item{
			Path:   file.Path,
			Dir:    filepath.Dir(file.Path),
			Name:   file.Name,
			Ext:    file.Ext,
			Size:   file.Size,
			Status: StatusPending,
		}
		if cfg.SkipProcessed && m.isProcessed(file.Path, file.Size, file.ModTime) {
			item.Status = StatusSkipped
			item.Message = "已处理过，跳过"
		}
		m.items = append(m.items, item)
		added++
	}

	for index := range m.items {
		m.items[index].Index = index
	}
	m.recountLocked()
	snapshot := make([]Item, len(m.items))
	copy(snapshot, m.items)
	progress := m.progress
	m.mu.Unlock()

	if m.callbacks.OnItem != nil {
		for _, item := range snapshot {
			m.callbacks.OnItem(item)
		}
	}
	if m.callbacks.OnProgress != nil {
		m.callbacks.OnProgress(progress)
	}

	if replace {
		m.logf("INFO", "扫描完成：%d 个文件，其中 %d 个已处理过", len(snapshot), progress.Skipped)
	} else {
		m.logf("INFO", "追加 %d 个文件，列表共 %d 个", added, len(snapshot))
	}
	return snapshot, nil
}

// Clear 清空列表，用于换文件夹。
func (m *Manager) Clear() {
	if m.IsRunning() {
		return
	}
	m.mu.Lock()
	m.items = []Item{}
	m.progress = Progress{}
	m.reserved = map[string]bool{}
	m.mu.Unlock()
	if m.callbacks.OnProgress != nil {
		m.callbacks.OnProgress(Progress{})
	}
}

// ---------------------------------------------------------------- 执行

// Scope 决定这一轮处理哪些条目。
//
// 默认只处理「还没跑过的」，这是有意为之：已识别的文件重跑一遍要重新解码、
// 重新请求，结果却不会变，只是白白多打一轮 Shazam。想重来必须显式要求。
type Scope string

const (
	ScopePending Scope = "pending" // 只处理新扫描出来、还没跑过的
	ScopeFailed  Scope = "failed"  // 只重试上一轮失败的
	ScopeAll     Scope = "all"     // 全部重来
)

// Start 开始识别。
func (m *Manager) Start(ctx context.Context, scope Scope) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return errors.New("任务已经在运行")
	}
	if len(m.items) == 0 {
		m.mu.Unlock()
		return errors.New("列表是空的，先扫描一个文件夹")
	}

	cfg := m.cfg

	// 选出这一轮要处理的下标。
	queue := make([]int, 0, len(m.items))
	for index := range m.items {
		status := m.items[index].Status

		switch scope {
		case ScopeFailed:
			if status != StatusFailed {
				continue
			}
		case ScopeAll:
			if status == StatusSkipped {
				continue
			}
		default:
			if status != StatusPending {
				continue
			}
		}

		// 回到待处理，让界面上的状态是干净的。
		m.items[index].Status = StatusPending
		m.items[index].Message = ""
		m.items[index].NewName = ""
		m.items[index].NewPath = ""
		m.items[index].Attempts = 0
		queue = append(queue, index)
	}
	if len(queue) == 0 {
		m.mu.Unlock()
		return errors.New("没有需要处理的文件")
	}

	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.running = true
	m.pauseMu.Lock()
	m.paused = false
	m.pauseMu.Unlock()
	m.recountLocked()
	m.progress.Running = true
	progress := m.progress
	m.mu.Unlock()

	if m.callbacks.OnProgress != nil {
		m.callbacks.OnProgress(progress)
	}

	started := time.Now()
	m.logf("INFO", "开始处理 %d 个文件（并发 %d，窗口 %d 秒，%s）",
		len(queue), cfg.Concurrency, cfg.SegmentSeconds, dryRunLabel(cfg.DryRun))

	m.runWorkers(runCtx, queue)

	m.mu.Lock()
	m.running = false
	m.cancel = nil
	m.progress.Running = false
	summary := Summary{
		Total:     m.progress.Total,
		Matched:   m.progress.Matched,
		Unmatched: m.progress.Unmatched,
		Failed:    m.progress.Failed,
		Skipped:   m.progress.Skipped,
		Renamed:   m.progress.Renamed,
		Canceled:  runCtx.Err() == context.Canceled,
		DryRun:    cfg.DryRun,
		ElapsedMS: time.Since(started).Milliseconds(),
	}
	m.mu.Unlock()

	m.saveState()

	if summary.Canceled {
		summary.Reason = "已手动停止"
	}
	m.logf("INFO", "结束：识别 %d，未识别 %d，失败 %d，改名 %d，跳过 %d，耗时 %.1f 秒",
		summary.Matched, summary.Unmatched, summary.Failed, summary.Renamed, summary.Skipped,
		float64(summary.ElapsedMS)/1000)

	if m.callbacks.OnDone != nil {
		m.callbacks.OnDone(summary)
	}
	return nil
}

// Stop 请求停止。正在跑的那次 sidecar 调用会被一起掐掉。
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		m.logf("WARN", "收到停止请求")
		cancel()
	}
}

func (m *Manager) runWorkers(ctx context.Context, queue []int) {
	m.mu.Lock()
	concurrency := m.cfg.Concurrency
	m.mu.Unlock()
	if concurrency > len(queue) {
		concurrency = len(queue)
	}
	if concurrency < 1 {
		concurrency = 1
	}

	ch := make(chan int)
	var wg sync.WaitGroup

	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range ch {
				if ctx.Err() != nil {
					m.markCanceled(index)
					continue
				}
				// 暂停点：等在这里，而不是掐断正在跑的那一次。
				if !m.waitWhilePaused(ctx) {
					m.markCanceled(index)
					continue
				}
				m.processOne(ctx, index)
			}
		}()
	}

	for _, index := range queue {
		if ctx.Err() != nil {
			m.markCanceled(index)
			continue
		}
		ch <- index
	}
	close(ch)
	wg.Wait()
}

func (m *Manager) markCanceled(index int) {
	m.mu.Lock()
	if m.items[index].Status == StatusPending || m.items[index].Status == StatusRunning {
		m.items[index].Status = StatusCanceled
		m.items[index].Message = "已停止"
	}
	item := m.items[index]
	m.recountLocked()
	progress := m.progress
	m.mu.Unlock()

	if m.callbacks.OnItem != nil {
		m.callbacks.OnItem(item)
	}
	if m.callbacks.OnProgress != nil {
		m.callbacks.OnProgress(progress)
	}
}

func (m *Manager) processOne(ctx context.Context, index int) {
	m.update(index, func(item *Item) {
		item.Status = StatusRunning
		item.Message = "识别中…"
	})

	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	options := recognizer.Options{
		Path:           m.itemPath(index),
		SegmentSeconds: cfg.SegmentSeconds,
		Language:       cfg.Language,
		Country:        cfg.Country,
		UserAgent:      cfg.UserAgent,
		TimeoutSeconds: cfg.TimeoutSeconds,
		FFmpegPath:     cfg.FFmpegPath,
		Proxy:          cfg.Proxy,
		Locales:        cfg.ExtraLocales(),
	}

	result, attempts := m.recognizeWithRetry(ctx, options, cfg.Retries, index)

	if ctx.Err() != nil {
		m.markCanceled(index)
		return
	}

	switch {
	case result.Matched():
		m.finishMatched(index, result, attempts)
	case result.Status == recognizer.StatusNoMatch:
		m.finishUnmatched(index, attempts)
	case result.Status == recognizer.StatusUnsupported ||
		result.Status == recognizer.StatusDecode ||
		result.Status == recognizer.StatusMissingFile ||
		result.Status == recognizer.StatusIO:
		// 解码类失败不是网络问题，重试无用，也不该改名——文件本身读不了。
		m.update(index, func(item *Item) {
			item.Status = StatusFailed
			item.Attempts = attempts
			item.Message = "无法解码：" + firstLine(result.Error)
		})
		m.bump(func(p *Progress) { p.Failed++ })
	default:
		m.update(index, func(item *Item) {
			item.Status = StatusFailed
			item.Attempts = attempts
			item.Message = firstLine(result.Error)
		})
		m.bump(func(p *Progress) { p.Failed++ })
	}
}

// recognizeWithRetry 按 RetryPlan 的节奏重试，返回最后一次结果与尝试次数。
func (m *Manager) recognizeWithRetry(
	ctx context.Context,
	options recognizer.Options,
	retries int,
	index int,
) (recognizer.Result, int) {
	attempts := 0

	for {
		attempts++
		result, err := m.recognizer.Recognize(ctx, options)
		if err == nil {
			return result, attempts
		}

		plan := recognizer.RetryPlan(result.Status)
		if attempts > retries || len(plan) == 0 || ctx.Err() != nil {
			return result, attempts
		}

		wait := plan[minInt(attempts-1, len(plan)-1)]
		m.logf("WARN", "%s 第 %d 次失败（%s），%.0f 秒后重试",
			filepath.Base(options.Path), attempts, result.Status, wait.Seconds())
		m.update(index, func(item *Item) {
			item.Message = fmt.Sprintf("%s，%.0f 秒后重试（第 %d 次）", result.Status, wait.Seconds(), attempts)
		})

		select {
		case <-ctx.Done():
			return result, attempts
		case <-time.After(wait):
		}
	}
}

func (m *Manager) finishMatched(index int, result recognizer.Result, attempts int) {
	m.mu.Lock()
	cfg := m.cfg
	item := m.items[index]
	m.mu.Unlock()

	fields := naming.Fields{
		Title:    result.Title,
		Artist:   result.Artist,
		Album:    result.Album,
		Year:     yearOf(result.Released),
		Original: item.Name,
		Titles:   naming.IndexByScript(result.Titles, result.Title),
		Artists:  naming.IndexByScript(result.Artists, result.Artist),
	}

	base := naming.Apply(cfg.NameTemplate, cfg.UnmatchedNaming(), fields, cfg.MaxNameRunes, true)

	newName := base + item.Ext
	if media.SamePath(item.Dir+string(os.PathSeparator)+newName, item.Path) {
		m.update(index, func(target *Item) {
			target.Status = StatusMatched
			target.Attempts = attempts
			applyTrack(target, result)
			target.NewName = newName
			target.NewPath = target.Path
			target.Message = "文件名已经是对的"
		})
		m.bump(func(p *Progress) { p.Matched++ })
		return
	}

	if cfg.DryRun {
		m.update(index, func(target *Item) {
			target.Status = StatusPreview
			target.Attempts = attempts
			applyTrack(target, result)
			target.NewName = newName
			target.NewPath = filepath.Join(target.Dir, newName)
			target.Message = "预览：将重命名为 " + newName
		})
		m.bump(func(p *Progress) { p.Matched++ })
		return
	}

	newPath, err := m.rename(index, newName)
	if err != nil {
		m.update(index, func(target *Item) {
			target.Status = StatusFailed
			target.Attempts = attempts
			target.Title = result.Title
			target.Artist = result.Artist
			target.Message = "改名失败：" + firstLine(err.Error())
		})
		m.bump(func(p *Progress) { p.Failed++ })
		return
	}

	m.recordUndo(item.Path, newPath)
	m.rememberProcessed(newPath)

	m.update(index, func(target *Item) {
		target.Status = StatusRenamed
		target.Attempts = attempts
		applyTrack(target, result)
		target.NewName = filepath.Base(newPath)
		target.NewPath = newPath
		target.Path = newPath
		target.Name = filepath.Base(newPath)
		target.Message = "已重命名"
	})
	m.bump(func(p *Progress) { p.Matched++; p.Renamed++ })
}

// applyTrack 把识别到的曲目信息写到条目上。三处出口（已是对的 / 预览 / 已改名）
// 都要写同一组字段，抄三遍迟早会漏一个。
func applyTrack(target *Item, result recognizer.Result) {
	target.Title = result.Title
	target.Artist = result.Artist
	target.Album = result.Album
	target.Genre = result.Genre
	target.Released = result.Released
	target.Label = result.Label
	target.CoverArt = result.CoverArt
	target.ShazamURL = result.ShazamURL
	target.Titles = scriptMap(result.Titles, result.Title)
	target.Artists = scriptMap(result.Artists, result.Artist)
}

// scriptMap 把候选按书写系统归位，并转成界面要的字符串键。
func scriptMap(candidates []string, primary string) map[string]string {
	index := naming.IndexByScript(candidates, primary)
	if len(index) == 0 {
		return nil
	}
	out := make(map[string]string, len(index))
	for script, value := range index {
		out[string(script)] = value
	}
	return out
}

func (m *Manager) finishUnmatched(index int, attempts int) {
	m.mu.Lock()
	cfg := m.cfg
	item := m.items[index]
	m.mu.Unlock()

	unmatched := cfg.UnmatchedNaming()
	base := naming.Apply(cfg.NameTemplate, unmatched,
		naming.Fields{Original: item.Name}, cfg.MaxNameRunes, false)
	newName := base + item.Ext

	// 已经带上未识别标记的（比如上一轮留下的）就不动了，避免套娃。
	//
	// 判断交给 Unmatched.Marked：不能直接写 strings.HasPrefix(name, prefix)，
	// 因为前缀为空时它**恒为真** —— 把前缀清空、只留后缀的用户会发现
	// 未识别的一个都不改名了，而界面上看不出任何异常。
	stem := strings.TrimSuffix(item.Name, item.Ext)
	if unmatched.Marked(stem) {
		m.update(index, func(target *Item) {
			target.Status = StatusNoMatch
			target.Attempts = attempts
			target.Message = "未识别（已经是未识别命名）"
		})
		m.bump(func(p *Progress) { p.Unmatched++ })
		return
	}

	// 前后缀都留空 = 「未识别的就别动」。这时算出来的新名字和原名一模一样。
	//
	// 上面那一关拦不住它：没有标记，Marked 对任何名字都返回 false。
	//
	// 不拦的后果不是「白跑一次 os.Rename(p, p)」那么轻——rename 里会先问
	// media.UniquePath 要一个不冲突的路径，而它只看目标是否存在；此刻**源文件
	// 自己正占着那个名字**，于是它判定冲突，返回 `mystery (1).mp4`。
	// 结果是一个什么都没认出来的文件被莫名加上 " (1)"，而且看起来像是正常改名。
	// finishMatched 里也有同样的一道同名判断，理由一样。
	if media.SamePath(filepath.Join(item.Dir, newName), item.Path) {
		m.update(index, func(target *Item) {
			target.Status = StatusNoMatch
			target.Attempts = attempts
			target.Message = "未识别（按设置保持原名）"
		})
		m.bump(func(p *Progress) { p.Unmatched++ })
		return
	}

	if cfg.DryRun {
		m.update(index, func(target *Item) {
			target.Status = StatusPreview
			target.Attempts = attempts
			target.NewName = newName
			target.NewPath = filepath.Join(target.Dir, newName)
			target.Message = "预览：未识别，将改为 " + newName
		})
		m.bump(func(p *Progress) { p.Unmatched++ })
		return
	}

	newPath, err := m.rename(index, newName)
	if err != nil {
		m.update(index, func(target *Item) {
			target.Status = StatusFailed
			target.Attempts = attempts
			target.Message = "改名失败：" + firstLine(err.Error())
		})
		m.bump(func(p *Progress) { p.Failed++ })
		return
	}

	m.recordUndo(item.Path, newPath)
	m.rememberProcessed(newPath)

	m.update(index, func(target *Item) {
		target.Status = StatusNoMatch
		target.Attempts = attempts
		target.NewName = filepath.Base(newPath)
		target.NewPath = newPath
		target.Path = newPath
		target.Name = filepath.Base(newPath)
		target.Message = "未识别，已改为 " + filepath.Base(newPath)
	})
	m.bump(func(p *Progress) { p.Unmatched++ })
}

// rename 在锁内算唯一路径并改名，避免并发下两条同名曲目抢同一个目标名。
func (m *Manager) rename(index int, newName string) (string, error) {
	m.mu.Lock()
	item := m.items[index]
	m.mu.Unlock()

	m.renameMu.Lock()
	defer m.renameMu.Unlock()

	target := media.UniquePath(item.Dir, strings.TrimSuffix(newName, item.Ext), item.Ext, m.reserved)
	if err := os.Rename(item.Path, target); err != nil {
		return "", err
	}
	m.reserved[strings.ToLower(target)] = true
	return target, nil
}

func (m *Manager) itemPath(index int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.items[index].Path
}

func (m *Manager) update(index int, mutate func(*Item)) {
	m.mu.Lock()
	if index < 0 || index >= len(m.items) {
		m.mu.Unlock()
		return
	}
	mutate(&m.items[index])
	item := m.items[index]
	m.mu.Unlock()

	if m.callbacks.OnItem != nil {
		m.callbacks.OnItem(item)
	}
}

func (m *Manager) bump(mutate func(*Progress)) {
	m.mu.Lock()
	mutate(&m.progress)
	m.recountLocked()
	progress := m.progress
	m.mu.Unlock()

	if m.callbacks.OnProgress != nil {
		m.callbacks.OnProgress(progress)
	}
}

// recountLocked 从列表重算进度。自增容易在重试、停止这些路径上算错，
// 重算一遍比信任一个计数器便宜。
func (m *Manager) recountLocked() {
	progress := Progress{Total: len(m.items), Running: m.running}
	for _, item := range m.items {
		switch item.Status {
		case StatusSkipped:
			progress.Skipped++
			progress.Done++
		case StatusMatched, StatusPreview:
			progress.Matched++
			progress.Done++
		case StatusRenamed:
			progress.Matched++
			progress.Renamed++
			progress.Done++
		case StatusNoMatch:
			progress.Unmatched++
			progress.Done++
		case StatusFailed:
			progress.Failed++
			progress.Done++
		case StatusCanceled:
			progress.Done++
		}
	}
	m.progress = progress
}

// ---------------------------------------------------------------- 状态文件

func (m *Manager) statePath() string {
	return filepath.Join(m.dataDir, config.StateFileName)
}

func (m *Manager) loadState() {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	m.state = map[string]stateEntry{}
	raw, err := os.ReadFile(m.statePath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, &m.state)
}

func (m *Manager) isProcessed(path string, size, modTime int64) bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	entry, ok := m.state[strings.ToLower(filepath.Clean(path))]
	if !ok {
		return false
	}
	// 大小或修改时间变了就当没处理过：用户换过文件内容。
	return entry.Size == size && entry.ModTime == modTime
}

func (m *Manager) rememberProcessed(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	m.stateMu.Lock()
	m.state[strings.ToLower(filepath.Clean(path))] = stateEntry{
		Size:    info.Size(),
		ModTime: info.ModTime().UnixMilli(),
		Status:  "renamed",
	}
	m.stateMu.Unlock()
}

func (m *Manager) saveState() {
	m.stateMu.Lock()
	raw, err := json.MarshalIndent(m.state, "", "  ")
	m.stateMu.Unlock()
	if err != nil {
		return
	}
	_ = os.WriteFile(m.statePath(), append(raw, '\n'), 0o644)
}

// ---------------------------------------------------------------- 撤销

func (m *Manager) undoPath() string {
	return filepath.Join(m.dataDir, config.UndoFileName)
}

// undoRecord 是一条改名记录，带上「还原过没有」。
//
// 磁盘上没有这个结构：它是 loadUndo 把 undo.jsonl 重放一遍之后的结果。
type undoRecord struct {
	From     string
	To       string
	ID       string
	TS       int64
	Restored bool
}

// recordUndo 记一条改名。ID 取的是**改名之后**那个文件的身份。
//
// 在改名之后取而不是之前：身份本来就不随改名变化，而改名之后路径是确定存在的，
// 少一次「万一 rename 成功了但源路径已经没了」的时序顾虑。
func (m *Manager) recordUndo(from, to string) {
	m.appendUndo(undoEntry{
		From: from,
		To:   to,
		ID:   fileIdentity(to),
		TS:   time.Now().UnixMilli(),
	})
}

// appendUndo 往 undo.jsonl 追加一行。
//
// 借用 logMu 当文件锁：日志和这个文件都不会被并发写，共用一把省一个字段。
func (m *Manager) appendUndo(entry undoEntry) {
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}

	m.logMu.Lock()
	defer m.logMu.Unlock()
	file, err := os.OpenFile(m.undoPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(raw, '\n'))
}

// loadUndo 重放 undo.jsonl，把还原记录盖到对应的改名记录上。
//
// 返回的顺序就是写入顺序（时间升序）。**顺序不能丢**：链式改名要靠它倒着还原。
func (m *Manager) loadUndo() ([]undoRecord, error) {
	file, err := os.Open(m.undoPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var records []undoRecord
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var entry undoEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			// 崩在写一半的那行：跳过，不能让半行废掉整份记录。
			continue
		}

		if entry.Restore != "" {
			// 盖到最近一条「还没还原过」的同名记录上。
			// 从后往前找，因为同一个 to 可能被改过不止一次。
			for i := len(records) - 1; i >= 0; i-- {
				if records[i].To == entry.Restore && !records[i].Restored {
					records[i].Restored = true
					break
				}
			}
			continue
		}
		if entry.To == "" {
			continue
		}
		records = append(records, undoRecord{From: entry.From, To: entry.To, ID: entry.ID, TS: entry.TS})
	}
	return records, scanner.Err()
}

// ---------------------------------------------------------------- 按身份找文件
//
// 一段前情：还原原来只看路径 —— 记录里存着「改成了什么名」，还原时去那个路径
// 上找文件。可用户在资源管理器里手动改一下名，路径就对不上了，还原报「文件已不在」，
// 而文件明明就在旁边。第 6 条反馈问的就是这件事。
//
// 办法是记下文件的**身份**（卷序列号 + FileId，见 fileIdentity）。改名不改变身份，
// 所以「路径对不上但身份对得上」正好就是「这个文件被改过名」。
//
// 判断顺序是刻意的：**先按身份找，再按路径找**。
//   · 先按路径找的话，一个恰好在同一路径上的**别的**文件会被当成目标 ——
//     还原就把一个无关的文件改名了，而且看起来一切正常。
//   · 身份找不到时才退回路径（老记录没有身份字段），那条路一直有效。

// identityIndex 缓存「目录 → (文件身份 → 路径)」。
//
// 存在的理由是 History() 要给每一条记录定位文件，而每条都要扫一遍它所在的目录。
// 同一批记录几乎全在同一个文件夹里，按目录缓存之后每个目录只扫一次，
// 而不是每条记录扫一次 —— 几百条记录乘以几百个文件，那个差别是能感觉到的。
type identityIndex map[string]map[string]string

func newIdentityIndex() identityIndex { return identityIndex{} }

// find 在 dir 里找身份为 id 的那个文件，返回它的完整路径；找不到返回空串。
//
// **不递归**。这个程序只在原目录里改名，用户在资源管理器里手动改名也不会
// 换目录。为了一个边缘情况去递归整个磁盘，代价和收益完全不成比例 ——
// 真的被挪到别处去了，那本来也就超出了「还原」能负责的范围。
func (idx identityIndex) find(dir, id string) string {
	if id == "" {
		return ""
	}
	table, ok := idx[dir]
	if !ok {
		table = map[string]string{}
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				path := filepath.Join(dir, entry.Name())
				if found := fileIdentity(path); found != "" {
					// 同一份数据被硬链接出两个名字时会撞同一个身份，
					// 这里保留先遇到的。媒体文件里几乎不会有硬链接，
					// 真有也只是「还原时挑中另一个名字」，不会改错文件。
					if _, seen := table[found]; !seen {
						table[found] = path
					}
				}
			}
		}
		idx[dir] = table
	}
	return table[id]
}

// forget 丢掉某个目录的缓存。
//
// 还原会真的改文件名，缓存里的「身份 → 路径」当场就过期了。不丢的话，
// 链式改名（A→B→C，两条记录）还原第一步 C→B 之后，第二步会照着过期的
// 路径 C 去找一个已经不存在的文件，报成「文件刚被移走或删掉」——
// 而文件就在隔壁的 B。这个坑是 job_test 里的链式还原用例当场抓出来的。
func (idx identityIndex) forget(dir string) { delete(idx, dir) }

// locate 算出这条记录对应的文件此刻在哪，以及是怎么找到的。
//
// 第二个返回值是 "id" 或 "path"，调用方靠它决定措辞：按身份找到、
// 而且名字和记录里的现名不一样，那就是「文件被改过名了」。
func locate(record undoRecord, index identityIndex) (string, string) {
	if found := index.find(filepath.Dir(record.From), record.ID); found != "" {
		return found, "id"
	}
	return record.To, "path"
}

// restoreOne 把一个文件从它现在的位置改回 record.From，并补一条还原记录。
//
// 几种「做不了」分开报，因为用户能采取的行动完全不同：
// 文件真的没了 / 现名被别的文件占了 / 现名换成了另一个文件 / 权限之类改不动。
func (m *Manager) restoreOne(record undoRecord, index identityIndex) UndoResult {
	var result UndoResult

	current, how := locate(record, index)

	// 「文件被改过名」和「同名却是别人」是两件事，这一条挡的是后者。
	//
	// 只有按路径找到时才需要查：按身份找到意味着身份已经对上了。
	// 这里不查的话，一个恰好占用了旧现名的**无关文件**会被改名 ——
	// 它和这次改名一点关系都没有。
	if how == "path" && record.ID != "" {
		if _, err := os.Stat(record.To); err == nil {
			if now := fileIdentity(record.To); now != "" && now != record.ID {
				result.Skipped++
				result.Details = append(result.Details,
					"现名换成了别的文件，跳过: "+filepath.Base(record.To))
				return result
			}
		}
	}

	if _, err := os.Stat(current); err != nil {
		result.Missing++
		if how == "id" {
			// 身份找到了却 stat 不到，只可能是刚被删掉。说得具体一点，
			// 免得用户以为「按身份找」这条路根本没生效。
			result.Details = append(result.Details,
				"文件刚被移走或删掉: "+filepath.Base(current))
			return result
		}
		result.Details = append(result.Details, "文件已不在: "+filepath.Base(record.To))
		return result
	}

	if _, err := os.Stat(record.From); err == nil {
		result.Skipped++
		result.Details = append(result.Details, "原名已被占用，跳过: "+filepath.Base(record.From))
		return result
	}

	if err := os.Rename(current, record.From); err != nil {
		result.Skipped++
		result.Details = append(result.Details, "还原失败: "+err.Error())
		return result
	}

	m.appendUndo(undoEntry{Restore: record.To, TS: time.Now().UnixMilli()})
	// 刚刚动过文件，缓存里的「身份 → 路径」已经不成立了。
	index.forget(filepath.Dir(current))
	index.forget(filepath.Dir(record.From))
	result.Restored++
	if how == "id" && !strings.EqualFold(current, record.To) {
		// 这一条值得单独说：用户以为自己只是「还原」，实际上程序先认出了
		// 一个被手动改过名的文件。不说的话，日志里看不出「唯一值判断」生效了。
		result.Details = append(result.Details,
			"按文件身份认出了改名后的 "+filepath.Base(current)+" → "+filepath.Base(record.From))
	}
	return result
}

// UndoOne 按「现名」把单个文件改回原名。
//
// 用现名而不是列表下标当钥匙：列表会随时间重排，下标是会漂的，
// 而路径是稳定的。同一个 to 被改过多次时，还原最近的那一次。
func (m *Manager) UndoOne(to string) (UndoResult, error) {
	if m.IsRunning() {
		return UndoResult{}, errors.New("任务运行中，先停止")
	}

	records, err := m.loadUndo()
	if err != nil {
		return UndoResult{}, err
	}

	for index := len(records) - 1; index >= 0; index-- {
		if records[index].To != to || records[index].Restored {
			continue
		}
		result := m.restoreOne(records[index], newIdentityIndex())
		m.logf("INFO", "还原单条：%s → %s",
			filepath.Base(records[index].To), filepath.Base(records[index].From))
		return result, nil
	}
	return UndoResult{}, errors.New("这条记录已经还原过，或者记录里没有它")
}

// UndoMany 把选中的那几条一起还原，参数是它们各自的**现名**（和 UndoOne 一致）。
//
// 三条规矩，每条都有理由：
//
//  1. 照样倒着重放。选中的几条里可能夹着链式改名（A→B→C 两条都勾上了），
//     正着来第一步就撞上「原名被占用」，用户看到「还原 0」却不知道错在哪。
//  2. 同一个 to 在一批里只还原最新的一条。改过不止一次时列表里会有好几行
//     指向同一个现名，全勾上就全试一遍，第二条必然撞「原名被占用」——
//     凭空多出一串「跳过」，看着像出了问题。
//  3. 一条都没动就报错，不返回「还原 0」。勾了几行却什么都没发生，
//     最可能的原因就是这几行早就还原过了，得说出来。
func (m *Manager) UndoMany(targets []string) (UndoResult, error) {
	if m.IsRunning() {
		return UndoResult{}, errors.New("任务运行中，先停止")
	}

	want := make(map[string]bool, len(targets))
	for _, target := range targets {
		if target != "" {
			want[target] = true
		}
	}
	if len(want) == 0 {
		return UndoResult{}, errors.New("没有勾选要还原的记录")
	}

	records, err := m.loadUndo()
	if err != nil {
		return UndoResult{}, err
	}

	var result UndoResult
	done := make(map[string]bool, len(want))
	// 一份索引给整批复用。这一批里的记录几乎全在同一个目录，
	// 每条都新建一份索引就等于把那个目录重扫一遍。
	idx := newIdentityIndex()
	for index := len(records) - 1; index >= 0; index-- {
		record := records[index]
		if record.Restored || !want[record.To] || done[record.To] {
			continue
		}
		done[record.To] = true

		one := m.restoreOne(record, idx)
		result.Restored += one.Restored
		result.Missing += one.Missing
		result.Skipped += one.Skipped
		result.Details = append(result.Details, one.Details...)
	}

	if result.Restored == 0 && result.Missing == 0 && result.Skipped == 0 {
		return result, errors.New("勾选的记录都已经还原过了")
	}

	m.logf("INFO", "批量还原 %d 条：还原 %d，找不到 %d，跳过 %d",
		len(done), result.Restored, result.Missing, result.Skipped)
	return result, nil
}

// Undo 按记录倒序把文件名改回去。倒序是必须的：同一批里可能有链式改名
// （A→B→C 得先还原 C→B、再还原 B→A；反过来的话第一步就撞上「原名被占用」）。
//
// 记录**不删**，改成补一条还原记录。删掉的话「记录原文件名」就没了——
// 而用户想单条还原时靠的正是这份记录。已还原的条目在界面上标成「已还原」，
// 重复点不会真的再改一次。
func (m *Manager) Undo() (UndoResult, error) {
	if m.IsRunning() {
		return UndoResult{}, errors.New("任务运行中，先停止")
	}

	records, err := m.loadUndo()
	if err != nil {
		return UndoResult{}, err
	}

	var result UndoResult
	idx := newIdentityIndex()
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].Restored {
			continue
		}
		one := m.restoreOne(records[index], idx)
		result.Restored += one.Restored
		result.Missing += one.Missing
		result.Skipped += one.Skipped
		result.Details = append(result.Details, one.Details...)
	}

	// 一条都没碰过（全都已经还原了）：说清楚，别让用户对着「还原 0」发懵。
	if result.Restored == 0 && result.Missing == 0 && result.Skipped == 0 {
		return result, errors.New("没有可撤销的记录")
	}

	m.logf("INFO", "撤销完成：还原 %d，找不到 %d，跳过 %d",
		result.Restored, result.Missing, result.Skipped)
	return result, nil
}

// ---------------------------------------------------------------- 导出

// ExportCSV 把当前列表写成 CSV，方便丢进表格里筛。
func (m *Manager) ExportCSV(path string) error {
	items := m.Items()

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// BOM：Excel 打开 UTF-8 CSV 不加这个会把中文显示成乱码。
	if _, err := file.WriteString("\ufeff"); err != nil {
		return err
	}

	header := []string{"状态", "原标题", "新标题", "歌名", "艺人", "专辑", "流派", "发行", "唱片", "路径", "说明"}
	if err := writer.Write(header); err != nil {
		return err
	}

	for _, item := range items {
		row := []string{
			string(item.Status), item.Name, item.NewName,
			item.Title, item.Artist, item.Album, item.Genre, item.Released, item.Label,
			item.Path, item.Message,
		}
		if err := writer.Write(row); err != nil {
			return err
		}
	}
	return writer.Error()
}

// ---------------------------------------------------------------- 暂停

// Pause 让 worker 在下一个文件开始前停住。
//
// 不做「掐断正在跑的那一次」：一次识别只有几十毫秒的 CPU 加一次网络往返，
// 中途放弃反而要重新解码。所以暂停是软的，界面上的语义是「跑完当前就停」。
func (m *Manager) Pause() {
	m.pauseMu.Lock()
	already := m.paused
	m.paused = true
	m.pauseMu.Unlock()

	if !already && m.IsRunning() {
		m.logf("WARN", "已暂停：当前文件跑完后不再取新文件")
	}
}

// Resume 解除暂停。
func (m *Manager) Resume() {
	m.pauseMu.Lock()
	was := m.paused
	m.paused = false
	m.pauseMu.Unlock()

	if was {
		m.logf("INFO", "已继续")
	}
}

// IsPaused 报告是否处于暂停状态。
func (m *Manager) IsPaused() bool {
	m.pauseMu.Lock()
	defer m.pauseMu.Unlock()
	return m.paused
}

// waitWhilePaused 在暂停期间阻塞，返回 false 表示期间被取消。
//
// 用轮询而不是 sync.Cond：Cond 没法同时等 ctx，而取消路径必须能立刻生效。
// 120 毫秒一次的开销可以忽略，换来的是「停止」永远按得动。
func (m *Manager) waitWhilePaused(ctx context.Context) bool {
	for {
		m.pauseMu.Lock()
		paused := m.paused
		m.pauseMu.Unlock()

		if !paused {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(120 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------- 记录

// HistoryEntry 是一条改名记录。
type HistoryEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
	TS   int64  `json:"ts"`
	// Restored 表示这条已经还原过了。还原过的条目仍然留着——
	// 「记录原文件名」本身就是这个页面的用处，改完就抹掉等于没记。
	Restored bool `json:"restored"`
	// Exists 表示这条记录对应的文件此刻还在磁盘上、还能还原。
	Exists bool `json:"exists"`
	// NowPath 是文件**实际**在哪。绝大多数时候等于 To；
	// 只有用户在程序外手动改过名时才会不一样，那时界面把现名那一格
	// 标出来，用户才知道「唯一值判断」真的认出了那个文件。
	NowPath string `json:"nowPath"`
}

// History 返回改名记录，最新的在前。
func (m *Manager) History() ([]HistoryEntry, error) {
	records, err := m.loadUndo()
	if err != nil {
		return nil, err
	}

	// 一份索引给所有记录复用：它们几乎全在同一个目录里，
	// 每条都新建一份就等于把那个目录重扫几百遍。
	idx := newIdentityIndex()

	entries := make([]HistoryEntry, 0, len(records))
	for _, record := range records {
		current, _ := locate(record, idx)
		_, statErr := os.Stat(current)
		entries = append(entries, HistoryEntry{
			From:     record.From,
			To:       record.To,
			TS:       record.TS,
			Restored: record.Restored,
			Exists:   statErr == nil,
			NowPath:  current,
		})
	}

	// 倒序：用户想看的总是刚才那几条。
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	return entries, nil
}

// Remove 把指定下标的条目从列表里拿掉，返回新列表。
//
// 只动队列，不碰磁盘上的文件——界面上写的是「移除」，不是「删除」。
func (m *Manager) Remove(indices []int) []Item {
	if m.IsRunning() {
		return m.Items()
	}

	drop := make(map[int]bool, len(indices))
	for _, index := range indices {
		drop[index] = true
	}

	m.mu.Lock()
	kept := make([]Item, 0, len(m.items))
	for index, item := range m.items {
		if drop[index] {
			continue
		}
		item.Index = len(kept)
		kept = append(kept, item)
	}
	m.items = kept
	m.recountLocked()
	snapshot := make([]Item, len(m.items))
	copy(snapshot, m.items)
	progress := m.progress
	m.mu.Unlock()

	if m.callbacks.OnProgress != nil {
		m.callbacks.OnProgress(progress)
	}
	return snapshot
}

// ---------------------------------------------------------------- 工具

var yearPattern = regexp.MustCompile(`\b(19|20)\d{2}\b`)

// yearOf 从 "2021-05-01" 这类发行日期里抠出年份，模板里的 {year} 用它。
func yearOf(released string) string {
	return yearPattern.FindString(released)
}

func dryRunLabel(dryRun bool) string {
	if dryRun {
		return "预览模式"
	}
	return "实际改名"
}

// firstLine 只留第一行：sidecar 的错误有时带一大段解码器输出，
// 界面上那一列放不下。
func firstLine(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return "未知错误"
	}
	if index := strings.IndexAny(message, "\r\n"); index >= 0 {
		message = message[:index]
	}
	const limit = 300
	if len(message) > limit {
		message = message[:limit] + "…"
	}
	return message
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
