//go:build windows

package main

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// 托盘的真机冒烟测试。
//
// 单元测试只能证明结构体大小、菜单布局这些**静态**事实。而托盘最容易踩的三个坑
// 全是运行期才暴露、而且**不报错**的：
//
//	cbSize 写错        → Shell_NotifyIconW 返回 0，图标不出现
//	DIB 格式不对       → CreateIconFromResourceEx 返回 0，图标不出现
//	DPI 感知没声明     → 图标出现了，但是在 150% 屏上是糊的
//
// 只有真的把图标挂到任务栏上，才能证明这三条都没踩。
// 它会短暂占用一个托盘位置，而且需要交互式桌面会话，所以默认跳过。
//
//	VSR_TRAY_SMOKE=1 go test -tags production -run TestTrayLive -v .
func TestTrayLiveSmoke(t *testing.T) {
	if os.Getenv("VSR_TRAY_SMOKE") == "" {
		t.Skip("需要交互式桌面会话；设 VSR_TRAY_SMOKE=1 才跑")
	}

	// 顺序很重要：必须在任何窗口被创建之前声明 DPI 感知，否则拿到的
	// SM_CXSMICON 是被虚拟化过的值，挑出来的图标档位就是错的。
	dpiMode := enableDPIAwareness()
	if dpiMode == "" {
		t.Fatal("没能声明 DPI 感知——150% 缩放下托盘图标会发虚")
	}
	t.Logf("DPI 感知：%s", dpiMode)

	icon, err := os.ReadFile("assets/icon.ico")
	if err != nil {
		t.Fatalf("读图标失败: %v", err)
	}

	var mu sync.Mutex
	var logs []string
	record := func(line string) {
		mu.Lock()
		logs = append(logs, line)
		mu.Unlock()
	}

	tray, err := startTray(trayCallbacks{
		Icon:     func() []byte { return icon },
		Tooltip:  func() string { return trayAppName },
		Menu:     func() trayStatus { return trayStatus{Status: "空闲", CanStart: true} },
		Show:     func() {},
		Toggle:   func() {},
		OpenData: func() {},
		Quit:     func() {},
		Log:      record,
	})
	if err != nil {
		// 这里失败基本就是 cbSize 或窗口类的问题，错误信息里带着 cbSize。
		t.Fatalf("startTray 失败: %v", err)
	}

	// startTray 返回 nil error 就意味着 Shell_NotifyIconW(NIM_ADD) 成功了。
	if activeTray == nil {
		t.Fatal("activeTray 没被赋值——图标加成功了但回调还认不得实例")
	}
	if tray.hwnd == 0 {
		t.Fatal("托盘窗口句柄是 0")
	}
	if tray.icon == 0 {
		t.Fatal("图标句柄是 0——CreateIconFromResourceEx 没接受这份 DIB")
	}

	// applyIcon 只在**失败路径**上写日志，所以「一条日志都没有」就等于图标这条路
	// 整条通了：解析 ICO → 按 DPI 挑档 → CreateIconFromResourceEx。
	//
	// 这里以前是 grep 一句「托盘图标已就绪：24×24（系统要求 24×24）」。那行已经
	// 从生产路径上删掉了：它从不指示任何需要用户动手的事，用户也没法从那串数字里
	// 得出什么结论，纯粹是每次启动往日志面板里塞的一行噪声。断言顺势收成「零日志」，
	// 反而比找某句话更严——失败路径有三条，找一句话只能盖住其中一条。
	mu.Lock()
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	if joined != "" {
		t.Fatalf("applyIcon 中途失败了（它只在失败时写日志）:\n%s", joined)
	}

	// 刷新与气泡都走 PostMessage，不能 panic、不能卡住。
	tray.Refresh()
	tray.Balloon(trayAppName, "冒烟测试")
	time.Sleep(300 * time.Millisecond)

	// Refresh 会在托盘线程上**再跑一次** applyIcon（换 DPI、换缩放时会走这条路）。
	// 这一轮同样不该产生日志。
	mu.Lock()
	afterRefresh := strings.Join(logs, "\n")
	mu.Unlock()
	if afterRefresh != "" {
		t.Fatalf("刷新之后出现了日志，说明 applyIcon 这一次失败了:\n%s", afterRefresh)
	}

	// 拆掉。Dispose 是异步的（投一条 WM_CLOSE），给消息循环一点时间跑完收尾。
	tray.Dispose()
	time.Sleep(500 * time.Millisecond)

	// 再拆一次不能出问题——OnShutdown 与异常路径可能都会调。
	tray.Dispose()

	t.Log("托盘冒烟通过：图标挂上了、句柄有效、刷新与气泡没炸、能干净拆掉")
}

// TestTrayLiveSystemIconSize 打印这台机器上系统要的小图标尺寸。
//
// 它是上面那条 DPI 结论的证据：150% 缩放下应当是 24，而不是 16。
// 注意本进程的 DPI 感知可能已被上一条测试声明过，重复调用会失败，
// 所以这里只报数、不断言。
func TestTrayLiveSystemIconSize(t *testing.T) {
	if os.Getenv("VSR_TRAY_SMOKE") == "" {
		t.Skip("需要交互式桌面会话；设 VSR_TRAY_SMOKE=1 才跑")
	}
	width := getSystemMetrics(smCXSmIcon)
	height := getSystemMetrics(smCYSmIcon)
	t.Logf("系统要求的小图标 %d×%d（16 = DPI 被虚拟化，24 = 150%% 缩放下的正确值）", width, height)
	if width < 16 || width > 64 {
		t.Fatalf("小图标尺寸 %d 不像话，SM_CXSMICON 常量可能写错了", width)
	}
}
