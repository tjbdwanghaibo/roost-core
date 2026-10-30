package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// entityCreateHandler 是事务外新建持久实体时框架起的那笔事务在 WAL 中的 handler 名。
const entityCreateHandler = "__dataengine_entity_create"

// entityCreateAdmitter 返回 EntityManager 的持久新建准入门（entity.CreateAdmitter），由 Runtime 启动后注册。
//
// 新建的持久实体本身就要落库（维护者 2026-10-30）。Nest handler 内新建由 handler 的事务捕获；没有事务捕获时（Nest 之外的
// 生成 Lifecycle.Create / GetOrCreate，例如登录时建号、服务 Init 里建 World；handler 之外的 RunDetachedTransaction 里
// 新建）由这里补一笔 strict 隔离事务：开一个 Guard 作用域，RunIsolatedTransaction 把事务绑定为它的捕获者，create 经
// CreateInScope 新建并被捕获（每个 DAO 整文档登记），提交被接受后才返回并释放新实体的锁。create 失败或提交被拒绝时
// 事务回滚、撤销发布；结果未知（ErrCommitIndeterminate）时 fail 让进程 fail-stop，与删除准入同一处理。
//
// 代价：调用方等一次 WAL 准入（strict），所以业务 worker 上调用直接拒绝（fctx.ErrBlockingInBusinessWorker），I/O worker
// 上开 Guard 作用域照旧 panic（I/O 池不得取实体锁）。外层有 handler 之外的 RunDetachedTransaction 时，新建这笔事务独立
// 提交：外层随后回滚不撤销已落库的新实体（外层没有可以持有新实体锁直到它提交的 Guard 作用域）。
func entityCreateAdmitter(committer corenest.TransactionCommitter, fail func(error)) entity.CreateAdmitter {
	return func(ctx context.Context, create func(*entity.GuardScope) (entity.IThreadSafeEntity, error)) (entity.IThreadSafeEntity, error) {
		if err := fctx.BlockingError("dataengine.admitEntityCreate"); err != nil {
			return nil, fmt.Errorf("dataengine create: a persistent entity created outside a transaction waits for its commit: %w", err)
		}
		var created entity.IThreadSafeEntity
		err := entity.WithGuardScope(entityCreateHandler, func(scope *entity.GuardScope) error {
			_, err := corenest.RunIsolatedTransaction(ctx, committer, entityCreateHandler, func() (any, error) {
				value, err := create(scope)
				created = value
				return nil, err
			})
			return err
		})
		if err != nil {
			if errors.Is(err, corenest.ErrCommitIndeterminate) && fail != nil {
				fail(err)
			}
			return nil, err
		}
		return created, nil
	}
}
