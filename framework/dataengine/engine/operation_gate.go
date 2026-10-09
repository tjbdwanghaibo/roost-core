package engine

import (
	"context"
	"sync"
)

// operationGate 串行执行可能等待外部 I/O 的操作；等待所有权时可以取消。
// 零值可用。取消等待只影响调用者，不中断当前持有者或提前释放其资源。
type operationGate struct {
	once sync.Once
	slot chan struct{}
}

func (g *operationGate) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.once.Do(func() { g.slot = make(chan struct{}, 1) })
	select {
	case g.slot <- struct{}{}:
		// 空槽与取消可能同时就绪，取消的调用不进入受保护操作。
		if err := ctx.Err(); err != nil {
			g.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *operationGate) release() { <-g.slot }
