package entity

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 常驻实体（lifetime=resident，维护者 2026-11-13）。
//
// 常驻实体起服时由本服的预加载载入（生成器把 lifetime=resident 当作 preload=startup，见 framework/dataengine/engine
// 的 PreloadLocalEntities），之后框架不把它移出内存。把实例移出内存的唯一入口是 EntityManager.Destroy（ManagerAccess.Unload、
// 运维 entity.unload、业务的空闲卸载都经它；事务内新建被撤销的实体走 revokeCreated，那时它从未提交过），所以规则只在
// 那里强制一次：只从内存卸载（deleteFromDB=false）的调用对常驻实例返回 ErrEntityResident，实体不受影响。
//
// 框架自己有意让它离开内存的三处带 WithResidentRelease 许可：
//
//   - 换服交出（DataEngine HandOffEntity / Release）：写权交给另一个服，实体跟着走，在目标服加载后常驻；
//   - 删档的收尾（事务内 Destroy(deleteFromDB=true) 在准入后移出内存）：实体本身没有了；
//   - 运维强制刷新（entity.unload force=true，DataEngine RefreshEntity）：卸载后立刻从库重新载入，不离开内存。
//
// 删档本身（deleteFromDB=true）不受常驻约束。许可只给框架这几处用；业务代码不要用它绕过常驻——要让实体不再常驻，
// 改它的 lifetime。
var (
	// ErrEntityResident：实体是 lifetime=resident，只从内存卸载它的调用被拒绝，实体仍在内存里。
	ErrEntityResident = errors.New("entity: the entity is lifetime=resident and stays in memory; it is not unloaded (delete it, or refresh it with the ops entity.unload force=true)")
	// ErrResidentDaoScope：lifetime=resident 的实体有不是 dbscope=sid 的 DAO（ValidateEntityRegistry 拒绝）。
	ErrResidentDaoScope = errors.New("entity: every DAO of a lifetime=resident entity must use dbscope=sid")
)

// residentReleaseKey 是 WithResidentRelease 的 ctx 键。
type residentReleaseKey struct{}

// WithResidentRelease 返回允许 Destroy(deleteFromDB=false) 把 lifetime=resident 实例移出内存的 ctx。只给框架有意
// 移出常驻实体的路径用（见本文件开头）；业务代码不要用。
func WithResidentRelease(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, residentReleaseKey{}, true)
}

func residentReleaseAllowed(ctx context.Context) bool {
	allowed, _ := ctx.Value(residentReleaseKey{}).(bool)
	return allowed
}

// IsEntityKindResident 报告 kind 注册的 builder 声明了 lifetime=resident。
func IsEntityKindResident(kind EntityKind) bool {
	builder := GetEntityBuilderParam(kind)
	return builder != nil && builder.Lifetime == EntityLifetimeResident
}

// refuseResidentUnload 是 Destroy 的常驻检查：只从内存卸载、实例是 lifetime=resident、ctx 没有许可时返回错误并计数。
func refuseResidentUnload(ctx context.Context, e IThreadSafeEntity, reason EntityDestroyReason, deleteFromDB bool) error {
	if deleteFromDB || e.Base().Lifetime() != EntityLifetimeResident || residentReleaseAllowed(ctx) {
		return nil
	}
	kind := e.GetEntityKind()
	metrics.IncCounter("entity.resident_unload_refused_total", metrics.Labels{"kind": strconv.Itoa(int(kind))}, 1)
	return fmt.Errorf("%w: entity %d (kind %d, destroy reason %d)", ErrEntityResident, e.ID(), kind, reason)
}

// residentDaoScopeProblems 返回 lifetime=resident 的 kind 里不是 dbscope=sid 的 DAO。
func residentDaoScopeProblems(builder *EntityBuilderParam) []error {
	if builder == nil || builder.Lifetime != EntityLifetimeResident {
		return nil
	}
	var problems []error
	for i, build := range builder.DaoBuilders {
		if build == nil {
			continue
		}
		dao := build()
		if dao == nil {
			continue
		}
		if scoped, ok := dao.(DatabaseScopedDao); ok && scoped.DbScope() == DatabaseServer {
			continue
		}
		problems = append(problems, fmt.Errorf("%w: kind=%d dao[%d] %T collection=%q is not dbscope=sid; a resident entity is preloaded from this server's database (//roost:dao ... dbscope=sid)",
			ErrResidentDaoScope, builder.Kind, i, dao, dao.CollName()))
	}
	return problems
}
