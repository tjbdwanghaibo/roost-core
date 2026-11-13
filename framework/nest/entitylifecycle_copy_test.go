package nest

// 本文件是 wiring/entitylifecycle/lifecycle.go 的测试副本（除 FromRegistry 外的全部声明，逐条同形）：
// 生成工程每个实体包的 lifecycle_gen.go 用的就是那份 Lifecycle[T]，handler 内新建的生命周期用例
// （TestLifecycleCreateInHandler* 等）要在 Nest 包里测它，但 wiring/mods 依赖 nest，测试不能 import 它。
// TestEntityLifecycleTestCopyMatchesWiring 逐条比对两边的声明（不含注释），不一致即红：改那边时把这里同步改掉。

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
)

// Lifecycle 管一种实体（T 是实体的指针类型，如 *player.Player）的建、读、删。
type Lifecycle[T entity.IThreadSafeEntity] struct {
	access   *entity.ManagerAccess
	kind     entity.EntityKind
	name     string
	notFound error
}

// New 用一份 ManagerAccess 构造。name 是错误信息里的实体名（小写，如 "player"）；notFound 是 Get 找不到时返回的
// 哨兵错误（生成的 ErrPlayerNotFound），调用方用 errors.Is 判断。
func New[T entity.IThreadSafeEntity](access *entity.ManagerAccess, kind entity.EntityKind, name string, notFound error) (*Lifecycle[T], error) {
	if access == nil || access.Manager() == nil {
		return nil, fmt.Errorf("%s lifecycle: Entity access is required", name)
	}
	if notFound == nil {
		notFound = fmt.Errorf("%s lifecycle: Entity not found", name)
	}
	return &Lifecycle[T]{access: access, kind: kind, name: name, notFound: notFound}, nil
}

// Get 按 uniqueID 读取（内存里没有时从存储加载）。删过档的实体返回带 errcode.ErrEntityDeleted 的错误。
func (lifecycle *Lifecycle[T]) Get(ctx context.Context, uniqueID int64) (T, error) {
	var zero T
	fullID, err := entity.BuildEntityID(uniqueID, lifecycle.kind)
	if err != nil {
		return zero, err
	}
	value, err := lifecycle.access.Get(ctx, fullID, entity.MustEntityCategoryOfKind(lifecycle.kind))
	if err != nil {
		return zero, deletedEntityError(err)
	}
	if value == nil {
		return zero, lifecycle.notFound
	}
	typed, ok := value.(T)
	if !ok {
		return zero, fmt.Errorf("%s lifecycle: Entity %d has type %T", lifecycle.name, fullID, value)
	}
	return typed, nil
}

// Create 新建实体，不读存储：uniqueID 必须是发号器给的新号，不能是删过档的号（墓碑会让第一次写成为投影冲突、
// 把进程隔离掉）。号可能已有记录时用 GetOrCreate。
func (lifecycle *Lifecycle[T]) Create(ctx context.Context, uniqueID int64) (T, error) {
	return lifecycle.CreateWith(ctx, uniqueID, nil)
}

// CreateWith 与 Create 相同，另把 param 作为 EntityCreateParam.Param 交给实体的构造（例如按配置建的场景）。
func (lifecycle *Lifecycle[T]) CreateWith(ctx context.Context, uniqueID int64, param any) (T, error) {
	var zero T
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	value, err := lifecycle.access.Create(&entity.EntityCreateParam{
		IsCreate: true,
		UniqueID: uniqueID,
		Kind:     lifecycle.kind,
		Category: entity.MustEntityCategoryOfKind(lifecycle.kind),
		Param:    param,
	})
	if err != nil {
		return zero, err
	}
	typed, ok := value.(T)
	if !ok {
		return zero, fmt.Errorf("%s lifecycle: created Entity has type %T", lifecycle.name, value)
	}
	return typed, nil
}

// GetOrCreate 返回已有的实体，存储里没有记录时新建（第二个返回值为 true）。删过档的实体（只剩墓碑）不再新建：
// 它的号不能再用，返回带 errcode.ErrEntityDeleted（客户端码 entity_deleted）的错误。
// entity.ErrEntityRemoved 表示同号的上一个实例还在移除途中（事务内撤销的新建或 Destroy），是暂时的，
// 有限次重试后才返回。Nest handler 内部框架把这个窗口报成 nest.ErrCreatedEntityLockConflict，自己决定整条消息回滚重入。
func (lifecycle *Lifecycle[T]) GetOrCreate(ctx context.Context, uniqueID int64) (T, bool, error) {
	return lifecycle.GetOrCreateWith(ctx, uniqueID, nil)
}

// GetOrCreateWith 与 GetOrCreate 相同，新建时把 param 交给实体的构造（CreateWith）；已有的实体不看 param。
func (lifecycle *Lifecycle[T]) GetOrCreateWith(ctx context.Context, uniqueID int64, param any) (T, bool, error) {
	for attempt := 1; ; attempt++ {
		value, err := lifecycle.Get(ctx, uniqueID)
		if err == nil {
			return value, false, nil
		}
		if !errors.Is(err, entity.ErrAuthorityEntityNotFound) && !errors.Is(err, lifecycle.notFound) {
			return value, false, err
		}
		value, err = lifecycle.CreateWith(ctx, uniqueID, param)
		if errors.Is(err, entity.ErrEntityExists) {
			value, err = lifecycle.Get(ctx, uniqueID)
			return value, false, err
		}
		if errors.Is(err, entity.ErrEntityRemoved) && attempt < 3 {
			continue
		}
		return value, err == nil, err
	}
}

// Destroy 销毁实体；deletePersisted 为 true 时同时删除持久记录（留墓碑）。
func (lifecycle *Lifecycle[T]) Destroy(ctx context.Context, value T, reason entity.EntityDestroyReason, deletePersisted bool) error {
	if isZero(value) {
		return lifecycle.notFound
	}
	return lifecycle.access.Destroy(ctx, value, reason, deletePersisted)
}

// isZero 判断 value 是不是 T 的零值（nil 指针）：nil 指针装进接口后不等于 nil，不能直接和 nil 比。
func isZero[T entity.IThreadSafeEntity](value T) bool {
	var zero T
	return any(value) == any(zero)
}

// deletedEntityError 给删过档的实体一个稳定的客户端码（errcode.ErrEntityDeleted）；原因留在错误链上供 errors.Is。
func deletedEntityError(err error) error {
	if errors.Is(err, entity.ErrAuthorityEntityDeleted) {
		var coded *errcode.IntError
		if !errors.As(err, &coded) {
			return errcode.Wrap(errcode.ErrEntityDeleted, err)
		}
	}
	return err
}
