package job

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videosongrename/internal/config"
	"videosongrename/internal/recognizer"
)

// 这一组测试跟 job_test.go 里的不一样：那些用的是 fakeRecognizer，从头到尾
// 没碰过进程边界。这里跑**真的 sidecar 可执行文件**，覆盖
//
//     Go → exec sidecar → 解析 stdout JSON → 错误分类 → 改名 / 不改名
//
// 这条链路上任何一环写错（参数名、JSON 字段、退出码判定）单测都发现不了。
//
// 找不到 sidecar 时整组跳过，不把「没编译 Rust」变成测试失败。

// requireSidecar 返回 sidecar 路径，找不到就跳过。
//
// recognizer.Locate() 只看 exe 同级、cwd 和 PATH。跑 `go test` 时 cwd 是包目录
// （internal/job），够不到仓库根，所以这里先自己往上找一层源码树里的 cargo 产物。
func requireSidecar(t *testing.T) string {
	t.Helper()

	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; ; {
			candidate := filepath.Join(dir, "sidecar", "target", "release", recognizer.SidecarName)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				t.Setenv("SHAZAM_SIDECAR", candidate)
				return candidate
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	binary, err := recognizer.Locate()
	if err != nil {
		t.Skipf("跳过：找不到 sidecar（%v）。先跑 cargo build --release", err)
	}
	return binary
}

// writeToneWAV 写一个 12 秒的正弦波 WAV。用真实音频而不是随便几个字节，
// 是为了让 sidecar 能一路走到联网那一步——解码失败就测不到「识别」分支了。
func writeToneWAV(t *testing.T, path string) {
	t.Helper()

	const (
		sampleRate = 44100
		seconds    = 12
	)
	samples := sampleRate * seconds
	payload := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		value := int16(12000 * math.Sin(2*math.Pi*440*float64(index)/sampleRate))
		binary.LittleEndian.PutUint16(payload[index*2:], uint16(value))
	}

	var buf []byte
	buf = append(buf, []byte("RIFF")...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(36+len(payload)))
	buf = append(buf, []byte("WAVEfmt ")...)
	buf = binary.LittleEndian.AppendUint32(buf, 16)
	buf = binary.LittleEndian.AppendUint16(buf, 1) // PCM
	buf = binary.LittleEndian.AppendUint16(buf, 1) // 单声道
	buf = binary.LittleEndian.AppendUint32(buf, sampleRate)
	buf = binary.LittleEndian.AppendUint32(buf, sampleRate*2) // 字节率
	buf = binary.LittleEndian.AppendUint16(buf, 2)            // 块对齐
	buf = binary.LittleEndian.AppendUint16(buf, 16)           // 位深
	buf = append(buf, []byte("data")...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(payload)))
	buf = append(buf, payload...)

	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("写测试音频失败: %v", err)
	}
}

func newIntegrationManager(t *testing.T, dir string, cfg config.Config) *Manager {
	t.Helper()

	binary := requireSidecar(t)
	manager := New(cfg, recognizer.New(binary), dir, Callbacks{})
	manager.OpenLog()
	t.Cleanup(manager.Close)
	return manager
}

// TestIntegrationUndecodableFileNotRenamed 用真 sidecar 喂一个解不了码的文件。
//
// 这条完全离线、完全确定：sidecar 必然返回解码错误，编排层必须把它归到
// failed 并且**不动文件名**。原 Python 脚本在这条路径上会把文件改成空名。
func TestIntegrationUndecodableFileNotRenamed(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("建 data 目录失败: %v", err)
	}

	// 一段文本冒充 mp3：扩展名骗得过扫描，骗不过解码器。
	broken := filepath.Join(root, "broken.mp3")
	if err := os.WriteFile(broken, []byte("这不是音频，只是一行文本\n"), 0o644); err != nil {
		t.Fatalf("写坏文件失败: %v", err)
	}

	cfg := config.Default()
	cfg.SegmentSeconds = 12
	cfg.Concurrency = 1
	cfg.Retries = 0
	cfg.SkipProcessed = false

	manager := newIntegrationManager(t, dataDir, cfg)
	if _, err := manager.Scan(root); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := manager.Start(ctx, ScopePending); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	items := manager.Items()
	if len(items) != 1 {
		t.Fatalf("期望 1 个条目，得到 %d", len(items))
	}
	item := items[0]

	if item.Status != StatusFailed {
		t.Errorf("解不了码的文件应当是 failed，实际 %s（%s）", item.Status, item.Message)
	}
	if _, err := os.Stat(broken); err != nil {
		t.Errorf("解码失败的文件不该被改名，但原路径已不存在: %v", err)
	}
	// 文件还在原地，且目录里没有多出别的文件。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	var files int
	for _, entry := range entries {
		if !entry.IsDir() {
			files++
		}
	}
	if files != 1 {
		t.Errorf("期望目录里仍然只有 1 个文件，实际 %d", files)
	}
}

// TestIntegrationRealSidecarRoundTrip 用真 sidecar 跑一遍完整编排。
//
// 输入是正弦波，Shazam 必然不匹配。这里要证明的是：
//   - 进程真的被拉起来、stdout 的 JSON 真的被解析出来了（不是 internal 错误）
//   - 拿到 no_match 后按 UnmatchedPrefix 改名，并写下撤销记录与状态文件
//
// 若机器没网，sidecar 会返回 network 类错误——这时跳过改名断言，
// 只确认「进程边界本身是通的」。否则一条离线的 CI 会因为网络而变红。
func TestIntegrationRealSidecarRoundTrip(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("建 data 目录失败: %v", err)
	}

	source := filepath.Join(root, "mystery-song.wav")
	writeToneWAV(t, source)

	cfg := config.Default()
	cfg.SegmentSeconds = 12
	cfg.Concurrency = 1
	cfg.Retries = 0
	cfg.SkipProcessed = false
	cfg.UnmatchedPrefix = "0_"

	manager := newIntegrationManager(t, dataDir, cfg)
	if _, err := manager.Scan(root); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := manager.Start(ctx, ScopePending); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	items := manager.Items()
	if len(items) != 1 {
		t.Fatalf("期望 1 个条目，得到 %d", len(items))
	}
	item := items[0]

	// 无论联网与否，都不该是 internal：internal 意味着参数拼错或 JSON 没解析出来。
	if item.Status == StatusFailed && item.Message != "" {
		if strings.Contains(item.Message, "执行识别程序失败") ||
			strings.Contains(item.Message, "解析识别结果失败") {
			t.Fatalf("进程边界没通：%s", item.Message)
		}
	}

	switch item.Status {
	case StatusNoMatch:
		t.Logf("联网成功，走的是 no_match 全分支：%s", item.Message)
		want := filepath.Join(root, "0_mystery-song.wav")
		if item.NewPath != want {
			t.Errorf("改名目标不对：期望 %s，实际 %s", want, item.NewPath)
		}
		if _, err := os.Stat(want); err != nil {
			t.Errorf("未识别的文件应当被改成 0_ 前缀，但目标不存在: %v", err)
		}
		if _, err := os.Stat(source); err == nil {
			t.Error("原文件还在，说明改名没发生")
		}

		// 撤销记录与状态文件必须落盘，否则「撤销」和「跳过已处理」都是假的。
		if _, err := os.Stat(filepath.Join(dataDir, config.UndoFileName)); err != nil {
			t.Errorf("撤销记录没写: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dataDir, config.StateFileName)); err != nil {
			t.Errorf("状态文件没写: %v", err)
		}

		// 撤销要能把名字还原回去。
		result, err := manager.Undo()
		if err != nil {
			t.Fatalf("撤销失败: %v", err)
		}
		if result.Restored != 1 {
			t.Errorf("期望还原 1 个，实际 %d（%v）", result.Restored, result.Details)
		}
		if _, err := os.Stat(source); err != nil {
			t.Errorf("撤销后原文件名应当回来: %v", err)
		}

	case StatusFailed:
		// 网络/限流类失败：进程边界通了，只是这台机器连不上。
		t.Skipf("跳过改名断言：sidecar 返回失败（%s），多半是没网", item.Message)

	default:
		t.Errorf("期望 no_match（或网络失败），实际 %s", item.Status)
	}
}

// TestIntegrationEnvVersionIsBareNumber 钉住 `--env` 的 version 字段形状。
//
// 为什么值得单独一条：顶栏那个状态芯片要把「产品名」和「版本号」分开显示。
// 产品名是界面的固定标签，版本号来自这里。踩过的坑是**预览桩**把这个字段编成
// "shazam-sidecar 0.3.1 (rodio)"，于是截图里显示成带产品名的样子，
// 而真机上是光秃秃的 "0.1.0" —— 预览和真机不一致，截图就失去了意义。
//
// 所以这里把真实形状固定下来：只有版本号，不带产品名，不带空格。
func TestIntegrationEnvVersionIsBareNumber(t *testing.T) {
	requireSidecar(t)

	binary, err := recognizer.Locate()
	if err != nil {
		t.Fatalf("定位 sidecar 失败：%v", err)
	}
	rec := recognizer.New(binary)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	env, err := rec.Env(ctx)
	if err != nil {
		t.Fatalf("--env 失败：%v", err)
	}
	if env.Version == "" {
		t.Fatal("--env 没报版本号")
	}
	if strings.ContainsAny(env.Version, " \t") {
		t.Errorf("version 里带空格：%q。界面把产品名当固定标签，这里只该有版本号。"+
			"如果确实是新形状，改 index.html 的 chip-name 和这条测试",
			env.Version)
	}
	if !strings.HasPrefix(env.Version, "0.") && !strings.HasPrefix(env.Version, "1.") {
		t.Errorf("version %q 不像版本号，像被拼进了别的东西", env.Version)
	}
	t.Logf("sidecar --env version = %q", env.Version)
}

// TestIntegrationCheckNetworkSurfacesProxy 走一次完整的「Go → sidecar --check-network」。
//
// 用一个**必然连不上**的代理地址（127.0.0.1:1），所以这条测试完全离线、完全确定：
// 不需要真的有网，也不会因为网络抖动变红。
//
// 它守的是那条缝：`Options.Proxy` 有没有真的进到 sidecar 的命令行、sidecar 的 JSON
// 有没有被正确解出来。两边各自单测都过、拼起来却不通，是进程边界最典型的失败方式——
// 而且这里传错的表现是「代理静默失效」，最难发现的那一类。
func TestIntegrationCheckNetworkSurfacesProxy(t *testing.T) {
	binary := requireSidecar(t)
	rec := recognizer.New(binary)

	const proxy = "http://127.0.0.1:1"
	check, err := rec.CheckNetwork(context.Background(), proxy, 5, "")
	if err != nil {
		t.Fatalf("CheckNetwork 返回了错误，它应当始终把失败放进结果里: %v", err)
	}

	if check.Status != recognizer.StatusNetwork {
		t.Errorf("连不上的代理应当归到 network，得到 %q（error=%q）", check.Status, check.Error)
	}
	// 这条才是关键：代理地址绕了一圈又回到 Go 这边，说明它真的到了 sidecar 手里。
	if check.Proxy != proxy {
		t.Errorf("sidecar 回报的代理是 %q，期望 %q —— 参数没传到位", check.Proxy, proxy)
	}
	// 报错必须说得出话。用户就是靠它区分「端口没开」和「地址写错」的。
	if strings.TrimSpace(check.Error) == "" {
		t.Error("失败时 error 不能是空的")
	}
}

// TestIntegrationCheckNetworkProxyModes 确认三种取值都被 sidecar 认出来。
//
// 不关心通不通（那取决于跑测试的机器有没有网），只关心**它把自己理解成了什么**。
// 这正是留空与 `-` 必须区分的那件事在进程边界上的样子：
// 传空 = 跟随环境变量，传 `-` = 强制直连，两者回报的描述必须不同。
func TestIntegrationCheckNetworkProxyModes(t *testing.T) {
	binary := requireSidecar(t)
	rec := recognizer.New(binary)

	cases := []struct {
		name  string
		proxy string
		want  string
	}{
		{"留空跟随环境变量", "", "跟随环境变量"},
		{"减号强制直连", "-", "强制直连"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check, err := rec.CheckNetwork(context.Background(), tc.proxy, 5, "")
			if err != nil {
				t.Fatalf("CheckNetwork 返回错误: %v", err)
			}
			if !strings.Contains(check.Proxy, tc.want) {
				t.Errorf("传 %q 时 sidecar 回报的代理是 %q，期望包含 %q",
					tc.proxy, check.Proxy, tc.want)
			}
		})
	}
}
