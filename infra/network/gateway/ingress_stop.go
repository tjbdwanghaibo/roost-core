package gateway

import (
	"context"
	"sync"
)

// drainGroup 保留一个停止阶段的完成信号。只能在该阶段全部准入关闭后等待，
// 不得排空后重新 Add；调用方取消或重试不会再创建等待同一批工作的协程。
type drainGroup struct {
	sync.WaitGroup
	once sync.Once
	done chan struct{}
}

func waitRuntime(ctx context.Context, group *drainGroup) error {
	group.once.Do(func() {
		group.done = make(chan struct{})
		go func() { group.Wait(); close(group.done) }()
	})
	select {
	case <-group.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop 先关新准入，保留依赖等待已接纳 dispatcher 与出站。超时不销毁实例，
// 下次用新 context 继续排空；取消等待不等于事务或回调已经结束。
func (game *GameIngress) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := game.stopSerial.Lock(ctx); err != nil {
		return err
	}
	defer game.stopSerial.Unlock()
	game.mu.Lock()
	game.closing = true
	started := game.started
	game.mu.Unlock()
	game.bindings.beginDrain()
	for _, sub := range game.subscriptions {
		if err := sub.DrainContext(ctx); err != nil {
			return err
		}
	}
	if err := waitRuntime(ctx, &game.controlWait); err != nil {
		return err
	}
	if !game.controlsClosed {
		close(game.controls)
		game.controlsClosed = true
	}
	if err := waitRuntime(ctx, &game.controlWorkers); err != nil {
		return err
	}
	if err := waitRuntime(ctx, &game.requestWait); err != nil {
		return err
	}
	if !game.workClosed {
		close(game.work)
		game.workClosed = true
	}
	if err := waitRuntime(ctx, &game.workerWait); err != nil {
		return err
	}
	if err := game.queue.Drain(ctx); err != nil {
		return err
	}
	game.bindings.expire(true)
	game.mu.Lock()
	remaining := make([]*bindingRecord, 0, len(game.resources))
	for _, resource := range game.resources {
		remaining = append(remaining, resource.record)
	}
	game.mu.Unlock()
	for _, record := range remaining {
		game.retire(record)
	}
	if err := waitRuntime(ctx, &game.retireWait); err != nil {
		return err
	}
	if !game.notificationsClosed {
		game.mu.Lock()
		close(game.notifications)
		game.notificationsClosed = true
		game.mu.Unlock()
	}
	if started {
		select {
		case <-game.notificationDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	game.cancel()
	if started {
		select {
		case <-game.sweepDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return game.StopNotifications(ctx)
}
