package operation

import (
	"context"
	"sync"
)

// Serial 串行化一个停止入口（Close / StopWithContext）：同一时刻只有一个调用在做停止步骤，并发的后到者
// 等它做完再按对象状态判断（已关闭就返回 nil）；等待受后到者自己的 ctx 约束，ctx 先结束返回 ctx 错误，
// 对象状态不变、之后可以再调用。零值可用（RR-20261006-10，驱动与 Mod 的 Close 统一口径）。
//
// 用 sync.Mutex 的话，后到者的等待不受自己的 ctx 约束：第一个调用者用 Background 等一个不结束的排空时，
// 后到者跟着永远等下去；在途回调里再次停止（用可取消的 ctx）也会卡在锁上。
type Serial struct {
	once sync.Once
	slot chan struct{}
}

// Lock 取得停止入口；返回 nil 时调用方必须恰好调用一次 Unlock。nil ctx 按 Background 处理。
func (s *Serial) Lock(ctx context.Context) error {
	s.once.Do(func() { s.slot = make(chan struct{}, 1) })
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case s.slot <- struct{}{}:
		return nil
	default:
	}
	select {
	case s.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock 交还停止入口。
func (s *Serial) Unlock() {
	<-s.slot
}
