package pace

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

// TestWaitSpacesRequestsByInterval 钉住「并发不等于速率」。
//
// 闸门的全部意义就在这条：三个 worker 同时来要时刻，拿到的是三个被间隔隔开的
// 时刻，而不是同一个 now。
func TestWaitSpacesRequestsByInterval(t *testing.T) {
	const interval = 40 * time.Millisecond
	gate := NewGate(interval, time.Second, time.Minute)

	const workers = 3
	stamps := make([]time.Time, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			if !gate.Wait(context.Background()) {
				t.Error("Wait 不应返回 false")
				return
			}
			stamps[slot] = time.Now()
		}(i)
	}
	wg.Wait()

	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
	for i := 1; i < workers; i++ {
		// 允许一点调度抖动，但方向必须是对的。
		if gap := stamps[i].Sub(stamps[i-1]); gap < interval/2 {
			t.Fatalf("第 %d 次与第 %d 次请求只隔了 %v，期望约 %v", i, i-1, gap, interval)
		}
	}
}

// TestFirstWaitIsImmediate 钉住「点开始不用先白等一个间隔」。
func TestFirstWaitIsImmediate(t *testing.T) {
	gate := NewGate(5*time.Second, time.Minute, time.Hour)

	started := time.Now()
	if !gate.Wait(context.Background()) {
		t.Fatal("Wait 不应返回 false")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("第一次 Wait 等了 %v，应当是立即返回", elapsed)
	}
}

// TestLimitedBlocksEveryone 钉住熔断：一次 429 之后，其它 worker 也必须停。
func TestLimitedBlocksEveryone(t *testing.T) {
	gate := NewGate(0, 120*time.Millisecond, time.Second)

	strikes, wait, until := gate.Limited(0)
	if strikes != 1 || wait <= 0 || until.IsZero() {
		t.Fatalf("第一次 429 应当返回 1 次、正数等待、非零截止时刻，得到 %d %v %v", strikes, wait, until)
	}

	started := time.Now()
	done := make(chan struct{})
	go func() {
		gate.Wait(context.Background())
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("冷却期间 Wait 直接放行了，熔断没有生效")
	case <-time.After(wait / 2):
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("冷却结束后 Wait 仍然没放行")
	}
	if elapsed := time.Since(started); elapsed < wait/2 {
		t.Fatalf("只等了 %v，冷冻时长 %v", elapsed, wait)
	}
}

// TestCooldownDoublesUntilSuccess 钉住逐次翻倍，以及「成功才清零」。
func TestCooldownDoublesUntilSuccess(t *testing.T) {
	gate := NewGate(0, 20*time.Millisecond, time.Second)

	_, first, _ := gate.Limited(0)
	_, second, _ := gate.Limited(0)
	if second < 2*first {
		t.Fatalf("第二次冷却应当翻倍：第一次 %v，第二次 %v", first, second)
	}

	// 冷却期间再来一次只会往后推截止时刻，不会提前。
	gate.Limited(0)
	if !gate.CoolingUntil().After(time.Now()) {
		t.Fatal("冷却截止时刻被提前到了过去")
	}

	// 成功一次之后连续计数清零：下一次 429 又从基数起算。
	// 不清零的话，冷却一过就又被挡的循环会永远停在最长的那一档上。
	gate.Succeeded()
	if strikes, _, _ := gate.Limited(0); strikes != 1 {
		t.Fatalf("成功之后连续计数应当从 1 重新开始，得到 %d", strikes)
	}
}

// TestRetryAfterIsHonouredButCapped 钉住「听服务端的，但不盲从」。
func TestRetryAfterIsHonouredButCapped(t *testing.T) {
	gate := NewGate(0, 10*time.Millisecond, 100*time.Millisecond)

	_, wait, _ := gate.Limited(time.Second)
	if wait > 100*time.Millisecond {
		t.Fatalf("Retry-After 超过上限时应当被压到 max，得到 %v", wait)
	}
	if wait < 50*time.Millisecond {
		t.Fatalf("Retry-After 比自带退避长时应当采用它，得到 %v", wait)
	}
}

// TestWaitHonoursCancel 钉住「冷却期间按停止能立刻生效」。
func TestWaitHonoursCancel(t *testing.T) {
	gate := NewGate(0, time.Hour, time.Hour)
	gate.Limited(0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if gate.Wait(ctx) {
		t.Fatal("ctx 已取消时 Wait 不该放行")
	}
}
