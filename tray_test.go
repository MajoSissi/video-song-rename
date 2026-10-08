package main

import (
	"os"
	"testing"
)

// 托盘里能单测的部分都在 tray.go（平台无关）。真正的 syscall 测不了，
// 但「菜单长什么样」「双击落在哪个动作」「图标挑哪一档」这些恰恰是最容易
// 被后续改动悄悄破坏、又最难靠肉眼发现的地方。

func TestTrayMenuLayout(t *testing.T) {
	items := trayMenuLayout(trayStatus{Status: "3 个任务 · 已识别 2", CanStart: true})

	want := []menuItem{
		{Flags: mfGrayed, ID: trayIDStatus, Text: "3 个任务 · 已识别 2"},
		{Flags: mfSeparator, ID: 0, Text: ""},
		{Flags: mfString, ID: trayIDShow, Text: "显示主窗口"},
		{Flags: mfString, ID: trayIDToggle, Text: "开始处理"},
		{Flags: mfSeparator, ID: 0, Text: ""},
		{Flags: mfString, ID: trayIDData, Text: "打开数据目录"},
		{Flags: mfString, ID: trayIDQuit, Text: "退出"},
	}

	if len(items) != len(want) {
		t.Fatalf("菜单项数不对：期望 %d，实际 %d：%+v", len(want), len(items), items)
	}
	// 逐项比，顺序和分段本身就是需求。只断言「各项都在」的话，
	// 把「退出」挪到最上面照样能通过。
	for index := range want {
		if items[index] != want[index] {
			t.Errorf("第 %d 项不对：\n期望 %+v\n实际 %+v", index, want[index], items[index])
		}
	}
}

func TestTrayMenuToggleFollowsState(t *testing.T) {
	cases := []struct {
		name  string
		state trayStatus
		text  string
		gray  bool
	}{
		{"没在跑但队列非空", trayStatus{CanStart: true}, "开始处理", false},
		{"没在跑且队列是空的", trayStatus{CanStart: false}, "开始处理", true},
		{"正在跑", trayStatus{Running: true, CanStart: true}, "暂停", false},
		{"已暂停", trayStatus{Running: true, Paused: true, CanStart: true}, "继续", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var toggle menuItem
			for _, item := range trayMenuLayout(tc.state) {
				if item.ID == trayIDToggle {
					toggle = item
				}
			}
			if toggle.Text != tc.text {
				t.Errorf("切换项文案：期望 %q，实际 %q", tc.text, toggle.Text)
			}
			isGray := toggle.Flags&mfGrayed != 0
			if isGray != tc.gray {
				t.Errorf("切换项置灰：期望 %v，实际 %v", tc.gray, isGray)
			}
		})
	}
}

func TestTrayMenuStatusNeverEmpty(t *testing.T) {
	for _, state := range []trayStatus{{}, {Running: true}, {Status: ""}} {
		items := trayMenuLayout(state)
		if items[0].Text == "" {
			t.Error("状态行不能是空的——菜单顶部出现一行空白比不显示更让人困惑")
		}
	}
}

// 状态行必须短。菜单是一扫而过的地方，用户不会去读一行二十个字。
func TestTrayMenuStatusIsShort(t *testing.T) {
	samples := []string{
		"就绪", "3 个任务", "处理中 2/5", "已完成：识别 3，未识别 1",
		"任务已暂停", "识别程序未就绪",
	}
	for _, sample := range samples {
		items := trayMenuLayout(trayStatus{Status: sample})
		if got := len([]rune(items[0].Text)); got > 14 {
			t.Errorf("状态行太长（%d 字）：%q", got, items[0].Text)
		}
	}
}

// 反向哨兵：动作项不许被置灰。一旦某个动作项变灰，用户会以为程序卡住了。
// （顶部状态行和「队列为空时的开始处理」是有意置灰的，不算。）
func TestTrayMenuActionsAreClickable(t *testing.T) {
	for _, state := range []trayStatus{
		{Status: "就绪", CanStart: true},
		{Status: "处理中", Running: true, CanStart: true},
	} {
		for _, item := range trayMenuLayout(state) {
			if item.Flags&mfGrayed == 0 {
				continue
			}
			if item.ID != trayIDStatus && item.ID != trayIDToggle {
				t.Errorf("动作项 %q 不该被置灰", item.Text)
			}
		}
	}
}

func TestTrayClickAction(t *testing.T) {
	cases := []struct {
		message uintptr
		want    trayClick
	}{
		{wmLButtonUp, trayClickNone}, // 单击故意什么都不做
		{wmLButtonDblClk, trayClickShow},
		{wmRButtonUp, trayClickMenu},
		{0x9999, trayClickNone},
	}
	for _, tc := range cases {
		if got := trayClickAction(tc.message); got != tc.want {
			t.Errorf("消息 0x%04X：期望 %v，实际 %v", tc.message, tc.want, got)
		}
	}
}

func TestParseICOReadsGeneratedIcon(t *testing.T) {
	raw, err := os.ReadFile("assets/icon.ico")
	if err != nil {
		t.Fatalf("读不到 assets/icon.ico（先跑 python tools/make_icon.py）: %v", err)
	}

	frames, err := parseICO(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	sizes := make(map[int]bool)
	for _, frame := range frames {
		sizes[frame.Width] = true
		if len(frame.Data) == 0 {
			t.Errorf("%d×%d 这一档没有数据", frame.Width, frame.Height)
		}
	}

	// 托盘要 16/20/24，高分屏还要更大的。少一档就会被迫放大。
	for _, want := range []int{16, 20, 24, 32, 48, 256} {
		if !sizes[want] {
			t.Errorf("图标里缺少 %d×%d 这一档", want, want)
		}
	}
}

func TestParseICORejectsGarbage(t *testing.T) {
	cases := map[string][]byte{
		"太短":      {0, 1, 2},
		"类型不是图标":  {0, 0, 2, 0, 1, 0},
		"档位数为零":   {0, 0, 1, 0, 0, 0},
		"目录项超出文件": {0, 0, 1, 0, 5, 0, 16, 0, 0, 0},
	}
	for name, raw := range cases {
		if _, err := parseICO(raw); err == nil {
			t.Errorf("%s：应当报错，却解析成功了", name)
		}
	}
}

// 挑档的顺序是有意为之：正好相等 > 更大的里面最小的 > 更小的里面最大的。
// 放大是凭空插值、边缘发虚，缩小只是丢像素，观感明显更好。
func TestPickICOPrefersEqualThenLarger(t *testing.T) {
	frames := []icoFrame{{Width: 16}, {Width: 20}, {Width: 32}, {Width: 48}}

	cases := []struct{ want, expect int }{
		{16, 16},
		{20, 20},
		{32, 32},
		{24, 32}, // 没有 24：宁可拿 32 缩，也不拿 20 放
		{40, 48},
		{64, 48}, // 比所有档都大：退而取最大的那档
	}
	for _, tc := range cases {
		frame, ok := pickICO(frames, tc.want)
		if !ok {
			t.Fatalf("want=%d 时应当挑到一档", tc.want)
		}
		if frame.Width != tc.expect {
			t.Errorf("want=%d：期望挑 %d，实际挑 %d", tc.want, tc.expect, frame.Width)
		}
	}
}

func TestPickICOOnEmpty(t *testing.T) {
	if _, ok := pickICO(nil, 16); ok {
		t.Error("没有档位时不该挑出东西来")
	}
}
