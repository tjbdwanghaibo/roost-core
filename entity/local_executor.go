package entity

import "context"

type localExecutorKey struct{}

// WithLocalExecutor 指定同步执行 Entity 本地操作的位置。Nest 慢阶段注入快池
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
	if ctx == nil {
		ctx = context.Background()
	}
	if run, ok := ctx.Value(localExecutorKey{}).(func(func()) error); ok && run != nil {
		return run(fn)
	}
	fn()
	return nil
}
