// video-song-rename —— 给视频/音频自动识别歌曲并改名。
//
// 界面是 Wails v2（Go + 内嵌 HTML）。前端是手写的单个 index.html，
// 不走 npm、不走打包器：这个工具要能只靠 `go build` 出一个 exe。
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"videosongrename/internal/single"
)

// 前端资源。整个 frontend 目录被编进 exe，运行时不需要外部文件。
//
//go:embed all:frontend
var frontendFS embed.FS

// 图标。托盘和窗口共用这一份——换图标只动 assets/icon.ico 再重新构建。
//
//go:embed assets/icon.ico
var iconICO []byte

// version 是程序版本号。
//
// tools/genrsrc.go 用正则从这里读出来填进 exe 的版本资源，所以改版本号只改这一处，
// 资源里的版本号和代码里的永远一致。
const version = "1.0.0"

func main() {
	// 单实例，而且必须在**建窗口、读配置之前**抢。
	//
	// 两个实例各自读一份配置、各自跑一批改名，而改名是写磁盘的操作 ——
	// 两边可能同时改同一个文件；配置是读—改—写，后写的会盖掉先写的。
	// 这些都不报错，只会安静地把状态弄乱。
	//
	// 抢不到时 Acquire 已经顺手通知那个实例把窗口叫到前台了，这里直接退出即可
	// （用户看到的是「点了一下图标，窗口跳出来」，而不是「什么都没发生」）。
	instance, onlyOne := single.Acquire()
	if !onlyOne {
		return
	}

	// DPI 感知必须在**建任何窗口之前**声明，所以放在最前面。
	//
	// 150% 缩放的屏幕上，不声明的进程调 GetSystemMetrics(SM_CXSMICON)
	// 只会拿到虚拟化后的 16，而任务栏实际需要 24×24 —— 交出 16 被放大，
	// 托盘图标就是糊的。窗口和 WebView 同理会被整体拉伸。
	dpiMode := enableDPIAwareness()

	// Wails 的 assetserver 把 FS 根当成站点根，所以先切到 frontend 这一层，
	// 否则 index.html 会挂在 /frontend/index.html 上。
	assets, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		log.Fatalf("内嵌前端资源失败: %v", err)
	}

	app := NewApp()
	app.setIcon(iconICO)
	if dpiMode == "" {
		app.emitLog("WARN", "没能声明 DPI 感知，高分屏下图标与文字可能发虚")
	} else {
		app.emitLog("INFO", "DPI 感知已启用："+dpiMode)
	}

	err = wails.Run(&options.App{
		Title:     "视频歌曲重命名",
		Width:     1240,
		Height:    820,
		MinWidth:  960,
		MinHeight: 620,
		// 不要系统窗口框架：标题栏由前端自绘，这样才能和深色主题融成一体。
		// 边缘缩放 Wails 会自己开（Frameless && !DisableResize）。
		Frameless: true,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		// 深色底，避免 WebView 起来的一瞬间闪白。
		BackgroundColour: &options.RGBA{R: 15, G: 17, B: 21, A: 1},
		OnStartup: func(ctx context.Context) {
			app.startup(ctx)
			// 之后有人再点一次图标时，第二个实例会被 single 拦下，
			// 并通过命名事件让这里把窗口叫出来。
			instance.Watch(app.ShowWindow)
		},
		OnShutdown: app.shutdown,
		// 点 ✕ 时按设置决定收托盘还是退出，返回 true 表示拦下这次关闭。
		OnBeforeClose: app.beforeClose,
		Bind:          []interface{}{app},
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop:     true,
			DisableWebViewDrop: true,
		},
		Windows: &windows.Options{
			Theme: windows.Dark,
			// 保留无边框窗口的圆角与投影（Win11）。关掉的话窗口会是硬邦邦的直角，
			// 和「融入主题的外框架」正好相反。
			DisableFramelessWindowDecorations: false,
			WebviewIsTransparent:              false,
			WindowIsTranslucent:               false,
			DisableWindowIcon:                 false,
			// 深色标题栏，配合自绘标题栏，系统绘制的那一小段不会跳色。
			CustomTheme: &windows.ThemeSettings{
				DarkModeTitleBar: 0x00110f0f, // 0x00BBGGRR
				DarkModeBorder:   0x00262b35,
			},
		},
	})
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}
}
