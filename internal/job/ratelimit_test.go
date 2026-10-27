package job

import (
	"context"
	"strings"
	"testing"
	"time"

	"videosongrename/internal/pace"
	"videosongrename/internal/recognizer"
)

// 这一组用例钉住限流的三条行为：
//
//  1. 被限流的条目要有**自己的状态**，不能混进「失败」——界面上那句
//     「失败」给不出任何可执行的下一步；
//  2. 一次 429 之后剩下的文件要留在「待处理」，否则用户点「开始」继续时
//     它们一个都不会被选中，界面看着像跑完了；
//  3. 请求之间真的被隔开了，并发不等于速率。

func rateLimited() recognizer.Result {
	return recognizer.Result{
		Status:    recognizer.StatusRateLimited,
		Retryable: true,
		Error:     "Shazam 限流（HTTP 429）",
	}
}

// TestRateLimitedFileGetsItsOwnStatus 钉住「限流 ≠ 失败」。
func TestRateLimitedFileGetsItsOwnStatus(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": rateLimited(),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	item := manager.Items()[0]
	if item.Status != StatusRateLimited {
		t.Fatalf("被限流的条目状态应当是 %q，得到 %q（说明 %q，原因 %q）",
			StatusRateLimited, item.Status, item.Message, summary.Reason)
	}
	if summary.RateLimited != 1 {
		t.Fatalf("汇总里应当记 1 条被限流，得到 %+v", summary)
	}
	if summary.Failed != 0 {
		t.Fatalf("被限流不该同时算进失败，得到 %+v", summary)
	}
	// 文件名必须原样留着：什么都没识别出来，就没资格改名。
	if names := listDir(t, dir); len(names) != 1 || names[0] != "clip.mp4" {
		t.Fatalf("被限流时不该动文件名，实际 %v", names)
	}
}

// TestRateLimitAbortsTheRunAndLeavesTheRestPending 钉住「不再拿剩下的文件去撞墙」。
//
// 连续两次 429 说明这条 IP 确实在被封：继续跑只是把剩下几百个文件挨个变成
// 一条红色的「失败」。正确做法是停手，让它们停在「待处理」，等用户过几分钟
// 再点开始。
func TestRateLimitAbortsTheRunAndLeavesTheRestPending(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"a.mp4": rateLimited(),
		"b.mp4": rateLimited(),
		"c.mp4": rateLimited(),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	for _, name := range []string{"a.mp4", "b.mp4", "c.mp4"} {
		write(name)
	}

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Reason == "" || !strings.Contains(summary.Reason, "限流") {
		t.Fatalf("汇总里要说清楚是限流中止的，得到 %q", summary.Reason)
	}
	// 撞两次就该停手：第二次 429 之后不再发请求。
	if calls := fake.callCount(); calls > maxRateLimitStrikes {
		t.Fatalf("连着被挡 %d 次之后应当停手，实际发了 %d 次请求", maxRateLimitStrikes, calls)
	}

	var pending, limited int
	for _, item := range manager.Items() {
		switch item.Status {
		case StatusPending:
			pending++
			if item.Message != "等待限流解除后继续" {
				t.Errorf("%s 停在待处理，但说明是 %q —— 用户看不出为什么没跑",
					item.Name, item.Message)
			}
		case StatusRateLimited:
			limited++
		case StatusCanceled:
			t.Errorf("%s 被标成「已停止」。"+
				"限流中止之后用户要做的正是再点一次「开始」，"+
				"而 ScopePending 只认待处理的条目 —— 标成已停止就再也选不中了", item.Name)
		}
	}
	if limited == 0 {
		t.Fatal("应当至少有一条被标成被限流")
	}
	if pending+limited != 3 {
		t.Fatalf("三条都该有归宿，实际 待处理 %d / 被限流 %d", pending, limited)
	}
}

// TestLimitedItemsCanBeRetried 钉住「重试失败项也带上被限流的」。
func TestLimitedItemsCanBeRetried(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": rateLimited(),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	// 冷却过后（这里直接改桩的结果）再点「重试失败项」。
	fake.setResult("clip.mp4", matched("Back In Black", "AC/DC"))
	summary := run(t, manager, ScopeFailed)

	if summary.Renamed != 1 {
		t.Fatalf("被限流的条目应当能被「重试失败项」选中，得到 %+v", summary)
	}
}

// TestMatchedButRateLimitedStillFreezes 钉住「文件成功、IP 被限」这条路。
//
// 额外语种那几次请求被 429 挡下时，文件本身已经有结果了，该改名；
// 但同时必须让所有 worker 一起慢下来，否则下一个文件立刻又撞上去。
func TestMatchedButRateLimitedStillFreezes(t *testing.T) {
	hit := matched("Back In Black", "AC/DC")
	hit.RateLimited = true
	hit.RetryAfter = 30

	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"a.mp4": hit,
		"b.mp4": matched("Thunderstruck", "AC/DC"),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("a.mp4")
	write("b.mp4")

	var seen int64
	manager.callbacks = Callbacks{
		OnProgress: func(p Progress) {
			if p.CooldownUntil > seen {
				seen = p.CooldownUntil
			}
		},
	}

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	summary := run(t, manager, ScopePending)

	if summary.Matched != 2 {
		t.Fatalf("两条都该识别成功，得到 %+v", summary)
	}
	if seen == 0 {
		t.Fatal("额外语种被限流时应当把冷却截止时刻推给界面，界面上才有倒计时可显示")
	}

	times := fake.callTimes()
	if len(times) < 2 {
		t.Fatalf("应当发了两次请求，实际 %d 次", len(times))
	}
	// Retry-After 是 30 秒，闸门上限被测试压到 20 毫秒，所以第二次请求
	// 至少要等一个冷却窗口 —— 这里只断言「确实等了」，不测具体值。
	if gap := times[1].Sub(times[0]); gap < 10*time.Millisecond {
		t.Fatalf("冷却期间不该立刻发下一次请求，实际只隔了 %v", gap)
	}
}

// TestRequestsAreSpacedByInterval 钉住「并发数不等于请求速率」。
//
// 这条是整个改动的核心：三个 worker 并发跑，不代表允许同一秒发三次请求。
func TestRequestsAreSpacedByInterval(t *testing.T) {
	const interval = 60 * time.Millisecond

	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	cfg := baseConfig()
	cfg.Concurrency = 3

	dir, manager, write := newHarness(t, cfg, fake)
	manager.gate = pace.NewGate(interval, 10*time.Millisecond, 20*time.Millisecond)

	for _, name := range []string{"a.mp4", "b.mp4", "c.mp4", "d.mp4"} {
		write(name)
	}
	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	times := fake.callTimes()
	if len(times) != 4 {
		t.Fatalf("期望 4 次请求，实际 %d 次", len(times))
	}
	span := times[len(times)-1].Sub(times[0])
	if minimum := time.Duration(len(times)-1) * interval / 2; span < minimum {
		t.Fatalf("4 次请求总跨度只有 %v，节流没有生效（并发 3 时它们挤在同一秒里）", span)
	}
}

// TestGateTakesIntervalFromConfig 钉住配置里的间隔真的进了闸门。
//
// 这一条防的是「字段加了、界面能填、但没接到实际请求节奏上」——
// 那种 bug 在界面上完全看不出来：设置保存成功，速度一点没变。
func TestGateTakesIntervalFromConfig(t *testing.T) {
	cfg := baseConfig()
	cfg.RequestIntervalMS = 700

	manager := New(cfg, &fakeRecognizer{byName: map[string]recognizer.Result{}}, t.TempDir(), Callbacks{})
	if got := manager.gateRef().Interval(); got != 700*time.Millisecond {
		t.Fatalf("闸门的间隔应当是 700ms，实际 %v", got)
	}
}

// TestIntervalIsPassedToSidecar 钉住「文件内部那几次请求也守同一个节奏」。
//
// 编排层只能节流「文件之间」。一个文件配了三个额外语种就是四次请求，
// 这四次必须由 sidecar 自己按 --min-interval 隔开，所以间隔必须传下去。
func TestIntervalIsPassedToSidecar(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	cfg := baseConfig()
	cfg.RequestIntervalMS = 800

	dir, manager, write := newHarness(t, cfg, fake)
	write("clip.mp4")
	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	if fake.lastCfg.RequestIntervalMS != 800 {
		t.Fatalf("请求间隔没有传给 sidecar，实际 %d", fake.lastCfg.RequestIntervalMS)
	}
}

// TestResumePicksUpRateLimitedItems 钉住「开始」按钮的兜底语义。
//
// 只传 ScopePending 的话，一条被限流、其余都跑完的队列再点「开始」会得到
// 「没有需要处理的文件」—— 而用户刚刚被程序告知「等几分钟再点开始」。
// ScopeResume 就是为这一步存在的：先做没跑过的，一个都没有就退到失败/限流。
func TestResumePicksUpRateLimitedItems(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{
		"clip.mp4": rateLimited(),
	}}
	dir, manager, write := newHarness(t, baseConfig(), fake)
	write("clip.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}
	run(t, manager, ScopePending)

	// 先确认那个坑是真的：单靠 ScopePending 什么都选不中。
	if err := manager.Start(context.Background(), ScopePending); err == nil {
		t.Fatal("被限流的条目不该被 ScopePending 选中，这个分支变了就更新注释")
	}

	fake.setResult("clip.mp4", matched("Back In Black", "AC/DC"))
	summary := run(t, manager, ScopeResume)

	if summary.Renamed != 1 {
		t.Fatalf("「开始」应当接着跑被限流的条目，得到 %+v", summary)
	}
}

// TestStopWhileCoolingDownIsStillManualStop 钉住「冷却期间按停止仍然是手动停止」。
//
// 这条和限流中止是两件事：手动停止要把剩下的标成「已停止」（用户就是想收工），
// 限流中止要留着「待处理」（用户马上就要继续）。
func TestStopWhileCoolingDownIsStillManualStop(t *testing.T) {
	fake := &fakeRecognizer{byName: map[string]recognizer.Result{}}
	cfg := baseConfig()
	dir, manager, write := newHarness(t, cfg, fake)
	write("a.mp4")
	write("b.mp4")

	if _, err := manager.Scan(dir); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var summary Summary
	manager.callbacks = Callbacks{OnDone: func(s Summary) { summary = s }}
	if err := manager.Start(ctx, ScopePending); err != nil {
		t.Fatal(err)
	}

	if summary.Reason != "已手动停止" {
		t.Fatalf("手动停止的汇总是 %q", summary.Reason)
	}
	for _, item := range manager.Items() {
		if item.Status != StatusCanceled {
			t.Fatalf("%s 应当被标成已停止，实际 %q", item.Name, item.Status)
		}
	}
}
