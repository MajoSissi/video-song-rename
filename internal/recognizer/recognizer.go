// Package recognizer 负责调用 Rust sidecar，并把它的 JSON 结果翻译成 Go 的类型。
//
// sidecar 是独立进程：指纹算法那份 Rust 代码不重写，也不通过 cgo 链接——
// 本仓库的构建机没有 C 编译器（CGO_ENABLED=0）。进程边界同时也是一道隔离：
// sidecar 崩溃只会让一个文件失败，不会带走整个界面。
package recognizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"videosongrename/internal/winproc"
)

// SidecarName 是识别程序的默认文件名。
const SidecarName = "shazam-sidecar.exe"

// Status 是 sidecar 报告的结果分类。
type Status string

const (
	StatusMatched     Status = "matched"
	StatusNoMatch     Status = "no_match"
	StatusUnsupported Status = "unsupported"
	StatusDecode      Status = "decode"
	StatusMissingFile Status = "missing_file"
	StatusIO          Status = "io"
	StatusRateLimited Status = "rate_limited"
	StatusNetwork     Status = "network"
	StatusUpstream    Status = "upstream"
	StatusRejected    Status = "rejected"
	StatusInternal    Status = "internal"
)

// Result 是一次识别的结果。
type Result struct {
	Status    Status `json:"status"`
	OK        bool   `json:"ok"`
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Album     string `json:"album"`
	Genre     string `json:"genre"`
	Released  string `json:"released"`
	Label     string `json:"label"`
	CoverArt  string `json:"cover_art"`
	ShazamURL string `json:"shazam_url"`
	TrackKey  string `json:"track_key"`
	// Titles / Artists 是同一首歌在不同 storefront 下的写法。
	// 只有配了额外语种才会有多于一项，否则就是主标题自己一个。
	Titles    []string `json:"titles"`
	Artists   []string `json:"artists"`
	Error     string   `json:"error"`
	Retryable bool     `json:"retryable"`
}

// Matched 报告这次识别是否拿到了曲目。
func (r Result) Matched() bool { return r.Status == StatusMatched }

// Options 是传给 sidecar 的参数。
type Options struct {
	Path           string
	SegmentSeconds int
	Language       string
	Country        string
	UserAgent      string
	TimeoutSeconds int
	// FFmpegPath 为空表示让 sidecar 自己探测。
	FFmpegPath string
	// Proxy 为空表示跟随环境变量；"-" 表示强制直连；其余当作代理地址。
	// 具体的取值规则由 sidecar 解释，这里只负责原样透传。
	Proxy string
	// Locales 是额外查询的「语言/国家」组合，每项一次完整的接口调用。
	Locales []string
}

// Environment 是 sidecar 报告的运行环境。
type Environment struct {
	Version    string `json:"version"`
	FFmpegPath string `json:"ffmpeg_path"`
	// Proxy 是 sidecar 实际会用的代理，人话描述。
	Proxy string `json:"proxy"`
	// ProxyEnv 是环境里存在的代理相关变量，用来回答「我没设代理啊」。
	ProxyEnv map[string]string `json:"proxy_env"`
}

// NetworkCheck 是一次连通性探测的结果。
type NetworkCheck struct {
	Status Status `json:"status"`
	OK     bool   `json:"ok"`
	// Proxy 是这次探测实际走的代理。
	Proxy string `json:"proxy"`
	// HTTPStatus 是拿到的应答码。走到这里就说明代理、DNS、TLS 都通了，
	// 所以它是 4xx 也无所谓——探测路径本来就不带签名。
	HTTPStatus int    `json:"http_status"`
	ElapsedMS  int64  `json:"elapsed_ms"`
	Error      string `json:"error"`
}

// Recognizer 持有一个 sidecar 可执行文件的路径。
type Recognizer struct {
	binary string
}

// ErrSidecarMissing 表示找不到识别程序，界面要把它当成阻断性错误。
var ErrSidecarMissing = errors.New("找不到 shazam-sidecar.exe")

// Locate 按优先级找 sidecar：
//  1. 环境变量 SHAZAM_SIDECAR
//  2. 主程序同目录（发布形态：两个 exe 放一起）
//  3. 源码树里的 cargo 产物（开发形态）
//  4. PATH
func Locate() (string, error) {
	var candidates []string

	if override := strings.TrimSpace(os.Getenv("SHAZAM_SIDECAR")); override != "" {
		candidates = append(candidates, override)
	}

	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, SidecarName))
		// 开发时 go build 的产物在临时目录，往上一层找源码树。
		candidates = append(candidates,
			filepath.Join(dir, "sidecar", "target", "release", SidecarName),
			filepath.Join(dir, "..", "sidecar", "target", "release", SidecarName),
		)
	}

	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "sidecar", "target", "release", SidecarName),
			filepath.Join(cwd, SidecarName),
		)
	}

	if found, err := exec.LookPath("shazam-sidecar"); err == nil {
		candidates = append(candidates, found)
	}

	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			if absolute, err := filepath.Abs(candidate); err == nil {
				return absolute, nil
			}
			return candidate, nil
		}
	}
	return "", ErrSidecarMissing
}

// New 用给定的 sidecar 路径建一个识别器。
func New(binary string) *Recognizer {
	return &Recognizer{binary: binary}
}

// Binary 返回当前使用的 sidecar 路径。
func (r *Recognizer) Binary() string { return r.binary }

// Version 问一次 sidecar 的版本，用来做启动自检。
func (r *Recognizer) Version(ctx context.Context) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	command := exec.CommandContext(runCtx, r.binary, "--version")
	// sidecar 是控制台程序，而界面没有控制台 —— 不设这一句，
	// 启动时会给它新建一个控制台窗口，用户看到的就是「黑框闪一下」。
	winproc.NoConsole(command)

	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("无法执行 %s: %w", r.binary, err)
	}
	return strings.TrimSpace(string(output)), nil
}

// Env 问一次 sidecar 的运行环境：版本，以及它**实际**会用的 ffmpeg。
//
// 用 sidecar 自己的判断，而不是 Go 这边照抄一份探测规则——ffmpeg 是它在用，
// 两边各写一份迟早会走偏，界面上就会显示一个其实没用上的路径。
func (r *Recognizer) Env(ctx context.Context) (Environment, error) {
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	command := exec.CommandContext(runCtx, r.binary, "--env")
	winproc.NoConsole(command)

	output, err := command.Output()
	if err != nil {
		return Environment{}, fmt.Errorf("无法执行 %s --env: %w", r.binary, err)
	}

	var env Environment
	if err := json.Unmarshal(output, &env); err != nil {
		return Environment{}, fmt.Errorf("解析 --env 的输出失败: %w", err)
	}
	return env, nil
}

// CheckNetwork 按给定的代理设置真的问一次 Shazam，验证连通性。
//
// 这一步会发出一次真实请求，所以只在用户按下「测试连接」时调用。
// 存在的理由：代理填错在批量跑之前完全不可见——用户要等到第一批文件全部
// 失败才发现，而那时错误信息里通常只有一句「网络错误」，分不清是代理没通、
// 代理地址写错，还是 Shazam 那边的问题。
//
// 返回的 NetworkCheck 里带着实际走的代理，界面可以直接把「用的是哪个代理」
// 显示出来，省掉一轮「你到底有没有用上我填的那个」的猜测。
func (r *Recognizer) CheckNetwork(
	ctx context.Context,
	proxy string,
	timeoutSeconds int,
	userAgent string,
) (NetworkCheck, error) {
	seconds := maxInt(timeoutSeconds, 5)
	args := []string{"--check-network", "--timeout", fmt.Sprintf("%d", seconds)}
	if trimmed := strings.TrimSpace(proxy); trimmed != "" {
		args = append(args, "--proxy", trimmed)
	}
	if userAgent != "" {
		args = append(args, "--user-agent", userAgent)
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds+15)*time.Second)
	defer cancel()

	command := exec.CommandContext(runCtx, r.binary, args...)
	winproc.NoConsole(command)
	var stderr strings.Builder
	command.Stderr = &stderr

	stdout, err := command.Output()
	if err != nil {
		// 探测失败时 sidecar 仍然会在 stdout 上给一份 JSON，只有「连 JSON 都没
		// 拿到」才是真正的执行失败。
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || len(stdout) == 0 {
			if runCtx.Err() == context.DeadlineExceeded {
				return NetworkCheck{Status: StatusNetwork, Error: "探测超时"},
					errors.New("探测超时")
			}
			return NetworkCheck{Status: StatusInternal, Error: err.Error()},
				fmt.Errorf("执行识别程序失败: %w", err)
		}
	}

	var check NetworkCheck
	if err := json.Unmarshal(stdout, &check); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = string(stdout)
		}
		return NetworkCheck{Status: StatusInternal, Error: message},
			fmt.Errorf("解析探测结果失败: %w", err)
	}
	if check.Status == "" {
		check.Status = StatusInternal
	}
	return check, nil
}

// buildArgs 把参数拼成 sidecar 的命令行。识别与「命令预览」共用同一份，
// 界面上看到的就是真正会跑的那条命令。
func buildArgs(opts Options) []string {
	args := []string{
		"--file", opts.Path,
		"--seconds", fmt.Sprintf("%d", maxInt(opts.SegmentSeconds, 1)),
		"--language", defaultString(opts.Language, "en-US"),
		"--country", defaultString(opts.Country, "GB"),
		"--timeout", fmt.Sprintf("%d", maxInt(opts.TimeoutSeconds, 5)),
	}
	if opts.UserAgent != "" {
		args = append(args, "--user-agent", opts.UserAgent)
	}
	// 空值一律不传。`--proxy ""` 在 sidecar 那边是用法错误——「不传」才表示
	// 跟随环境变量，这两种意思不能混。
	if proxy := strings.TrimSpace(opts.Proxy); proxy != "" {
		args = append(args, "--proxy", proxy)
	}
	if opts.FFmpegPath != "" {
		args = append(args, "--ffmpeg", opts.FFmpegPath)
	}
	for _, locale := range opts.Locales {
		if strings.TrimSpace(locale) != "" {
			args = append(args, "--locale", locale)
		}
	}
	return args
}

// CommandLine 返回这次识别会执行的完整命令行。
func (r *Recognizer) CommandLine(opts Options) string {
	args := append([]string{r.binary}, buildArgs(opts)...)
	for index, arg := range args {
		if strings.ContainsAny(arg, " \t") {
			args[index] = `"` + arg + `"`
		}
	}
	return strings.Join(args, " ")
}

// Recognize 跑一次识别。返回的 Result 里带着错误分类，网络类错误由调用方决定是否重试。
func (r *Recognizer) Recognize(ctx context.Context, opts Options) (Result, error) {
	if opts.Path == "" {
		return Result{}, errors.New("缺少文件路径")
	}

	args := buildArgs(opts)

	// 给进程本身留出比 HTTP 超时更宽的时间：解码长文件也要花时间。
	timeout := time.Duration(maxInt(opts.TimeoutSeconds, 5)+45) * time.Second
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(runCtx, r.binary, args...)
	winproc.NoConsole(command)
	// sidecar 的日志走 stderr，成功路径上不需要；失败时拼进错误消息。
	var stderr strings.Builder
	command.Stderr = &stderr

	stdout, err := command.Output()
	if err != nil {
		// 退出码 2（未匹配）与 5/6 之类都是「有结果但非零」，stdout 里有 JSON。
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || len(stdout) == 0 {
			if runCtx.Err() == context.DeadlineExceeded {
				return Result{Status: StatusNetwork, Retryable: true, Error: "识别超时"},
					fmt.Errorf("识别超时: %s", opts.Path)
			}
			return Result{Status: StatusInternal, Error: err.Error()},
				fmt.Errorf("执行识别程序失败: %w", err)
		}
	}

	var result Result
	if err := json.Unmarshal(stdout, &result); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = string(stdout)
		}
		return Result{Status: StatusInternal, Error: message},
			fmt.Errorf("解析识别结果失败: %w", err)
	}

	// sidecar 没报 status 时兜底，避免前端拿到空字符串。
	if result.Status == "" {
		result.Status = StatusInternal
	}
	return result, nil
}

// RetryPlan 给出某个错误该等多久重试。空切片表示不该重试。
//
// 429 的退避必须明显大于网络抖动：重试越快，限流窗口越难过去。
func RetryPlan(status Status) []time.Duration {
	switch status {
	case StatusRateLimited:
		return []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second}
	case StatusNetwork, StatusUpstream:
		return []time.Duration{1 * time.Second, 3 * time.Second, 8 * time.Second}
	default:
		return nil
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
