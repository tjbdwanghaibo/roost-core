package entity

import (
	"context"
	"errors"

	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

type localExecutorKey struct{}

// WithLocalExecutor 指定同步执行 Entity 本地操作的位置。Nest I/O 准备阶段注入业务池
// 延续；独立使用 Entity/Remote 时未注入则就地执行。fn 返回前不能交还所有权，
// 不能用任意异步 goroutine 代替；fn 内也不能再次调用该执行器。
func WithLocalExecutor(ctx context.Context, run func(func()) error) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, localExecutorKey{}, run)
}

// RunLocal 将需要 Entity 本地锁或业务回调的步骤交回本地执行池。
// 连接、缓存和租约元数据的内部互斥不属于这个边界。
func RunLocal(ctx context.Context, fn func()) error {
	// 已在长短业务池时就地执行；即使调用方保存了 I/O 阶段的 ctx，也不能再次投递并自等。
	if fctx.InBusinessWorker() {
		fn()
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if run, ok := ctx.Value(localExecutorKey{}).(func(func()) error); ok && run != nil {
		return run(fn)
	}
	fn()
	return nil
}

type remoteWriteTargetHeldKey struct{}

// WithRemoteWriteTargetHeld 标记调用方已持有 Remote 写目标实例的实体锁，例如事务外独立删档在 EntityManager.Destroy
// 的锁内准备 Remote 写批次。写准入发现这份实例落后于权威时不能把它卸载重载（卸载要取同一把锁，就地执行会在删档
// 中途把实例换掉），按版本冲突拒绝；落后的实例留到下一次不持锁的写准入再换。
func WithRemoteWriteTargetHeld(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, remoteWriteTargetHeldKey{}, true)
}

// RemoteWriteTargetHeld 报告 ctx 是否带 WithRemoteWriteTargetHeld 标记。
func RemoteWriteTargetHeld(ctx context.Context) bool {
	return ctx != nil && ctx.Value(remoteWriteTargetHeldKey{}) == true
}

// ErrColdLoadInLogic 表示业务池阶段缺少预加载目标。应在消息上声明全部冷目标（或用 SendOptionIO 强制 I/O 准备），
// 不能在持有 Guard 时把半个 handler 转移到 io 池。
var ErrColdLoadInLogic = errors.New("entity: cold load forbidden in logic; declare targets and dispatch with SendOptionIO preparation")

type loadedEntitiesOnlyKey struct{}

// WithLoadedEntitiesOnly 禁止 Getter 进行冷加载和等待在途加载；内存中的目标仍可访问。
// 自定义 Getter 必须遵守 LoadedEntitiesOnly，不能丢弃传入的 context。
func WithLoadedEntitiesOnly(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Value(loadedEntitiesOnlyKey{}) == true {
		return ctx
	}
	return context.WithValue(ctx, loadedEntitiesOnlyKey{}, true)
}
func LoadedEntitiesOnly(ctx context.Context) bool {
	return fctx.InBusinessWorker() || (ctx != nil && ctx.Value(loadedEntitiesOnlyKey{}) == true)
}
