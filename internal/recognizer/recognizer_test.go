package recognizer

import (
	"strings"
	"testing"
)

// hasFlagValue 报告 args 里有没有 `--name value` 这一对。
func hasFlagValue(args []string, name, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

// TestBuildArgsOmitsProxyWhenEmpty 守住「留空」与「-」是两件事。
//
// 这是整条代理链上最容易写错的一处。sidecar 把「不传 --proxy」解释成
// 「跟随 HTTP_PROXY / HTTPS_PROXY」，把 `--proxy -` 解释成「强制直连」。
// 如果这里图省事写成「总是传 --proxy」，空字符串会在 sidecar 那边变成用法错误，
// 整个识别直接失败；反过来，如果把 `-` 也当成空值过滤掉，那么设了系统代理的
// 用户就永远绕不开那个变量——而这正是「强制直连」这个选项存在的唯一理由。
func TestBuildArgsOmitsProxyWhenEmpty(t *testing.T) {
	args := buildArgs(Options{Path: "a.mp4"})
	if hasFlag(args, "--proxy") {
		t.Fatalf("Proxy 为空时不该传 --proxy，实际: %v", args)
	}
}

func TestBuildArgsPassesDirectFlag(t *testing.T) {
	args := buildArgs(Options{Path: "a.mp4", Proxy: "-"})
	if !hasFlagValue(args, "--proxy", "-") {
		t.Fatalf("Proxy 为 \"-\" 时要原样传给 sidecar，实际: %v", args)
	}
}

func TestBuildArgsPassesProxyURL(t *testing.T) {
	for _, url := range []string{
		"http://127.0.0.1:7890",
		"https://proxy.example:8443",
		"socks5://127.0.0.1:1080",
	} {
		args := buildArgs(Options{Path: "a.mp4", Proxy: url})
		if !hasFlagValue(args, "--proxy", url) {
			t.Errorf("代理地址 %q 没有被原样传给 sidecar，实际: %v", url, args)
		}
	}
}

// TestBuildArgsTrimsProxy 守住「从界面粘过来的地址带空格」。
//
// 用户从浏览器或文档里复制地址时经常带上首尾空格。不 trim 的话
// reqwest 会把它当成一个非法 URL 拒掉，而错误消息里那个地址看起来完全正常——
// 这种「看着没问题却报错」最难查。
func TestBuildArgsTrimsProxy(t *testing.T) {
	args := buildArgs(Options{Path: "a.mp4", Proxy: "  http://127.0.0.1:7890\t"})
	if !hasFlagValue(args, "--proxy", "http://127.0.0.1:7890") {
		t.Fatalf("代理地址没有被 trim，实际: %v", args)
	}
}

// TestCommandLineShowsProxy 守住「命令预览里看得到代理」。
//
// 命令预览是排错时最有用的东西。代理没进去的话，用户会照着一条没有代理的
// 命令去排查为什么连不上。
func TestCommandLineShowsProxy(t *testing.T) {
	r := New("shazam-sidecar.exe")
	line := r.CommandLine(Options{Path: "a.mp4", Proxy: "socks5://127.0.0.1:1080"})
	if !strings.Contains(line, "--proxy socks5://127.0.0.1:1080") {
		t.Fatalf("命令预览里看不到代理: %s", line)
	}
}

// TestBuildArgsPassesMinInterval 守住「文件内部那几次请求的间隔」。
//
// 编排层只能节流文件之间；一个文件配了三个额外语种就是四次请求，
// 那四次由 sidecar 按 --min-interval 自己隔开。这个参数漏传的话，
// 界面上把间隔调到 5 秒也没用——文件内部依然是零间隔齐射。
func TestBuildArgsPassesMinInterval(t *testing.T) {
	args := buildArgs(Options{Path: "a.mp4", RequestIntervalMS: 1500})
	if !hasFlagValue(args, "--min-interval", "1500") {
		t.Fatalf("请求间隔没有传给 sidecar，实际: %v", args)
	}

	// 0 表示「不指定」，不该出现在命令行里：sidecar 的默认值是它自己的事。
	if hasFlag(buildArgs(Options{Path: "a.mp4"}), "--min-interval") {
		t.Error("间隔为 0 时不该传 --min-interval")
	}
}
