// Package operation 管理可取消关闭等待的在途调用所有权。
//
// Lifetime 是仓内停机对象共用的“准入 + 在途计数 + idle 通道”（维护者决定 A3，2026-10-05）。
// 它只实现 roost-coding “三步停机”里的前两步：
//
//  1. 发起关闭：Stop 关闭准入（幂等），之后 Begin 返回 false；
//  2. 在调用方 ctx 内等待排空：Wait(ctx) 等已准入的调用全部 End，ctx 先结束返回 ctx 错误、
//     准入保持关闭、计数不变，之后用新 ctx 再调用 Wait 会继续等同一批；
//  3. 排空后才释放：由使用方在 Wait 返回 nil 之后自己做（关连接、交还依赖等），Lifetime 不持有资源。
//
// 不配合 ctx 的调用不会被终止：取消只代表调用方不再等，不代表在途调用已经结束，也不能代为 End。
package operation

import (
	"context"
	"sync"
)

// Lifetime 零值可用。Stop 阻止新调用；已准入调用仍需 End，不能由取消代为归还。
//
// 一个 Lifetime 只有一次生命：Stop 之后不能重新打开。需要“停止后可以重新启动”的使用方
// （例如每次 Start 一个订阅）每次启动用一个新的 Lifetime。
type Lifetime struct {
	mu       sync.Mutex
	active   int
	stopping bool
	drained  chan struct{}
}

// Begin 准入一次调用；Stop 之后返回 false。返回 true 时调用方必须恰好调用一次 End。
func (l *Lifetime) Begin() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopping {
		return false
	}
	l.active++
	return true
}

// End 归还一次 Begin 准入的调用；关闭准入后最后一个 End 关闭 idle 通道。
func (l *Lifetime) End() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.stopping && l.active == 0 {
		close(l.drained)
	}
}

// Stop 幂等地关闭准入，返回在最后一个已准入调用 End 时关闭的通道（没有在途调用时已关闭）。
// 每次调用返回同一个通道。
func (l *Lifetime) Stop() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.stopping {
		l.stopping = true
		l.drained = make(chan struct{})
		if l.active == 0 {
			close(l.drained)
		}
	}
	return l.drained
}

// Stopping 报告 Stop 是否已被调用（准入已关闭），不代表在途调用已经排空。
func (l *Lifetime) Stopping() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopping
}

// Wait 关闭准入（同 Stop，幂等），在 ctx 内等已准入的调用全部 End。
//
// 已排空时无论 ctx 是否已结束都返回 nil：重试的调用方拿到的是真实状态，而不是 select 在两个
// 都就绪的分支之间随机选出的 ctx 错误。ctx 先结束时返回 ctx.Err()，Lifetime 保持原状，
// 之后再调用 Wait 继续等同一批调用。nil ctx 按 context.Background 处理（不限时）。
func (l *Lifetime) Wait(ctx context.Context) error {
	drained := l.Stop()
	select {
	case <-drained:
		return nil
	default:
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
