//go:build windows

package single

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// testNames 给每个用例造一套**自己的**内核对象名。
//
// 不直接用包里的 mutexName / eventName：用户正开着这个程序跑测试时，
// 常量那个互斥体已经被占了，Acquire 会返回「已经有一个实例」——
// 测试就成了随机失败。那种测试没人敢信，也没人敢跑，等于没写。
//
// 带上 PID：同一台机器上并行跑两轮测试（或者 CI 和本地同时跑）也不会撞。
func testNames(t *testing.T) (string, string) {
	t.Helper()
	prefix := fmt.Sprintf(`Local\vsr-test-%d-%s`, os.Getpid(), t.Name())
	return prefix + "-mutex", prefix + "-event"
}

// 单实例的核心：第二个实例必须被拦下。
//
// 拦不下的代价不是「多一个图标」，而是两个进程各自读一份配置、各自跑一批改名，
// 而改名要写磁盘、配置是读—改—写 —— 后写的盖掉先写的，且全程不报错。
func TestAcquire_SecondInstanceIsRefused(t *testing.T) {
	mutex, event := testNames(t)

	first, ok := acquireNamed(mutex, event)
	if !ok || first == nil {
		t.Fatalf("第一个实例应当抢到名额，实际拿到 (%v, %v)", first, ok)
	}

	second, ok := acquireNamed(mutex, event)
	if ok {
		t.Fatal("第二个实例应当被拦下，却拿到了名额")
	}
	if second != nil {
		t.Fatal("被拦下时应当返回 nil —— 返回一个「半成品 Instance」会让人以为还能用")
	}
}

// 第二个实例不能只是安静地退出。
//
// 用户双击图标多半是因为「刚才那个窗口找不到了」（收在托盘里、或者被别的窗口盖住），
// 所以第二次启动要顺手把已有实例的窗口叫出来 —— 否则就是点了没反应。
//
// 走事件而不是 FindWindow + ShowWindow：窗口可能收在托盘里，而 Wails 自己记着
// 「窗口现在可见吗」。绕过它直接 ShowWindow 会让它的记账和实际状态对不上。
func TestAcquire_WakesTheRunningInstance(t *testing.T) {
	mutex, event := testNames(t)

	first, ok := acquireNamed(mutex, event)
	if !ok {
		t.Fatal("第一个实例应当抢到名额")
	}

	fired := make(chan struct{}, 8)
	first.Watch(func() { fired <- struct{}{} })

	if _, ok := acquireNamed(mutex, event); ok {
		t.Fatal("第二个实例应当被拦下")
	}

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("第二个实例启动之后，已有实例没收到「把窗口叫出来」的通知")
	}

	// 一次唤醒只该叫一次。等一会儿看有没有第二个回调追上来 ——
	// 追上来就说明 Watch 起了不止一个 goroutine（sync.Once 没起作用），
	// 那样连点两次图标会连着叫好几次。
	select {
	case <-fired:
		t.Fatal("一次唤醒触发了多个回调 —— Watch 起了不止一个 goroutine")
	case <-time.After(200 * time.Millisecond):
	}
}

// 一次唤醒只该叫一次窗口。
//
// 这条守的是**事件必须是自动复位的**（CreateEvent 的 manualReset 参数传 0）。
// 变异测试确认过：把那个 0 改成 1，等待中的 goroutine 会被全部唤醒，
// 而且信号不会被清掉 —— 于是循环里 show() 会被反复调用，
// 连点两次图标就变成叫个不停。所以这里的断言不是空转。
//
// 注意它**不**证明 sync.Once 有用：自动复位事件本来就只会唤醒一个等待者，
// 所以就算起了三个 goroutine，一次 SetEvent 也只调一次回调。
// Once 防的是「多起几个永远等不到东西的 goroutine」，那个从外部观察不到，
// 这里不为它编一条假的断言 —— 那种断言在最该报警的时候会「通过」。
func TestWatch_OneWakeCallsShowOnce(t *testing.T) {
	mutex, event := testNames(t)

	first, ok := acquireNamed(mutex, event)
	if !ok {
		t.Fatal("第一个实例应当抢到名额")
	}

	fired := make(chan struct{}, 8)
	first.Watch(func() { fired <- struct{}{} })

	acquireNamed(mutex, event)

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("一次唤醒之后没触发回调")
	}

	select {
	case <-fired:
		t.Fatal("一次唤醒触发了多个回调 —— 事件被建成了手动复位，信号不会自动清掉")
	case <-time.After(200 * time.Millisecond):
	}
}

// 事件没建出来时不能把程序拦下。
//
// Acquire 里那条 eventErr != nil 的分支是 fail-safe 的方向：
// 「叫不出窗口」比「用户根本打不开程序」轻得多。这里没法方便地制造
// CreateEvent 失败，所以只验它返回的 Instance 在 event 为 0 时 Watch 是安全的空转 ——
// 也就是那条分支实际会走到的形状。
func TestWatch_WithoutEventIsNoop(t *testing.T) {
	done := make(chan struct{})
	(&Instance{}).Watch(func() { close(done) })

	select {
	case <-done:
		t.Fatal("没有事件句柄时不该调用 show")
	case <-time.After(100 * time.Millisecond):
	}
}
