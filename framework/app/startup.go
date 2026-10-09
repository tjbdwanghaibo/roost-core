package app

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
)

// runStartupStep 在启动期限内等待一个阶段。Mod 的旧接口没有 ctx，不能强行
// 终止它；finished=false 时调用方必须保留依赖与锁，交由进程退出收回资源。
// 同一时刻只有一个启动阶段读取 signals，进入 Serve 后由正常停机流程接管。
func runStartupStep(ctx context.Context, signals <-chan os.Signal, name string, fn func(context.Context) error) (finished bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("app: startup %s: %w", name, err)
	}
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
		return true, err
	case <-ctx.Done():
		return false, fmt.Errorf("app: startup %s did not finish within startup.timeout: %w", name, ctx.Err())
	case sig := <-signals:
		return false, fmt.Errorf("app: startup %s interrupted by %v: %w", name, sig, context.Canceled)
	}
}
