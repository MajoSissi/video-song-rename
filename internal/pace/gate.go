// Package pace 是识别请求的全局闸门：它同时管两件事。
//
//  1. **节奏**：任意两次请求之间至少隔多久。并发数只说明「同时有几个 worker」，
//     不说明「每秒发几次」——三个 worker 各自打一次请求，落在服务端眼里就是
//     同一秒的三次突发。真正决定会不会被限流的是间隔，不是并发。
//  2. **熔断**：某一次吃到 429 之后，所有 worker 一起停。限流是按 IP 算的，
//     一个 worker 被挡意味着这条 IP 上所有请求都会被挡；让其余 worker 继续跑，
//     等于拿更多请求去撞同一个封禁窗口，只会把窗口撞得更长。
//
// 这两件事分开做是没有意义的：只做节奏、不做熔断，429 之后每个 worker 仍然会
// 各自按节奏继续撞；只做熔断、不做节奏，冷却一结束又是一次齐射，立刻再被挡。
package pace

import (
	"context"
	"sync"
	"time"
)

// tick 是等待时的最大唤醒间隔。
//
// 等待时间不能一次睡到底：冷却截止时刻随时可能被另一次 429 往后推，
// 睡死了的 worker 醒来就已经晚了。200 毫秒的精度对 1.5 秒量级的节奏绰绰有余，
// 而轮询本身的开销可以忽略。
const tick = 200 * time.Millisecond

// Gate 是所有识别请求共用的闸门。零值不可用，用 NewGate。
type Gate struct {
	mu       sync.Mutex
	interval time.Duration
	base     time.Duration
	max      time.Duration

	// next 是下一次可以发请求的最早时刻，由 interval 往前推。
	next time.Time
	// cooldown 是熔断截止时刻。它和 next 取较晚的那个。
	cooldown time.Time
	// strikes 是连续吃到 429 的次数，用来把冷却时间翻倍。
	strikes int
}

// NewGate 建一个闸门。interval 是两次请求的最小间隔；base 是第一次 429 之后
// 冻结多久，max 是冻结时间的上限（连续被挡时逐次翻倍到这个值为止）。
func NewGate(interval, base, max time.Duration) *Gate {
	if interval < 0 {
		interval = 0
	}
	if base <= 0 {
		base = time.Minute
	}
	if max < base {
		max = base
	}
	return &Gate{interval: interval, base: base, max: max}
}

// Interval 返回配置的请求间隔。
func (g *Gate) Interval() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.interval
}

// Wait 阻塞到可以发下一次请求为止。返回 false 表示 ctx 结束了。
//
// 第一次调用是立即返回的：队列刚点开始就干等一个间隔，纯属白等。
func (g *Gate) Wait(ctx context.Context) bool {
	for {
		g.mu.Lock()
		now := time.Now()
		slot := g.next
		if g.cooldown.After(slot) {
			slot = g.cooldown
		}
		if !now.Before(slot) {
			g.next = now.Add(g.interval)
			g.mu.Unlock()
			return true
		}
		g.mu.Unlock()

		wait := time.Until(slot)
		if wait > tick {
			wait = tick
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
	}
}

// Succeeded 报告一次成功的请求：连续被挡的计数清零。
//
// 清零靠的是「冷却结束之后真的发出去了一个请求并且成功了」，不是靠时间流逝。
// 冷却一过就清零的话，紧接着的 429 会从最短的冷却重新开始，等于永远在
// 60 秒这一档上反复撞。
func (g *Gate) Succeeded() {
	g.mu.Lock()
	g.strikes = 0
	g.mu.Unlock()
}

// Limited 登记一次 429：冻结整条流水线，返回这是连续第几次、要冻多久、冻到什么时候。
//
// retryAfter 是应答里 Retry-After 的值；为 0 表示没有这个头（实测经常没有）。
// 它比我们自己的退避更可信时就用它，但**不超过 max**：服务端给一个「1 小时」
// 的时候，我们不该真的把程序挂在那里。
func (g *Gate) Limited(retryAfter time.Duration) (int, time.Duration, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.strikes++
	wait := g.base
	for i := 1; i < g.strikes && wait < g.max; i++ {
		wait *= 2
	}
	if wait > g.max {
		wait = g.max
	}
	if retryAfter > wait {
		wait = retryAfter
	}
	if wait > g.max {
		wait = g.max
	}

	until := time.Now().Add(wait)
	// 已经在冷却里的时候再吃到一次 429，只能把截止时刻往后推，不能提前。
	if g.cooldown.After(until) {
		until = g.cooldown
	} else {
		g.cooldown = until
	}
	// 冷却结束后重新起算节奏，别让冷却末尾攒下的「欠账」变成一次齐射。
	g.next = g.cooldown

	return g.strikes, time.Until(until), until
}

// CoolingUntil 返回冷却截止时刻，零值表示不在冷却中。
func (g *Gate) CoolingUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cooldown
}
