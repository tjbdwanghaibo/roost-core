package app

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
)

// startupStep 是一个被打断（startup.timeout 到期或收到停机信号）时仍在跑的启动阶段：Mod 的 Init / Provide / Start 与
// Service.Init 都不收 ctx，App 杀不掉它，只能等它自己返回（result 投递一次）。
type startupStep struct {
	name     string
	result   <-chan error
	bySignal bool
}

// runStartupStep 在启动期限内等待一个阶段。finished=false 时阶段仍在跑，pending 是它：调用方按有序停机收尾
// （App.stopInterruptedStartup：先关已启动部分的输入，收到信号时再等这一步返回），不能直接释放它可能还在用的依赖。
// 同一时刻只有一个启动阶段读取 signals，进入 Serve 后由正常停机流程接管。
func runStartupStep(ctx context.Context, signals <-chan os.Signal, name string, fn func(context.Context) error) (finished bool, err error, pending *startupStep) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("app: startup %s: %w", name, err), nil
	}
	// 返回时取消阶段的 ctx（收到信号时也取消）：配合 ctx 的阶段（启动阶段的 lifecycle hook）因此尽快返回。
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("app: startup %s panic: %v\n%s", name, p, debug.Stack())
			}
			result <- err
		}()
		err = fn(stepCtx)
	}()
	select {
	case err := <-result:
		return true, err, nil
	case <-ctx.Done():
		return false, fmt.Errorf("app: startup %s did not finish within startup.timeout: %w", name, ctx.Err()),
			&startupStep{name: name, result: result}
	case sig := <-signals:
		return false, fmt.Errorf("app: startup %s interrupted by %v: %w", name, sig, context.Canceled),
			&startupStep{name: name, result: result, bySignal: true}
	}
}
