package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 运维卸载单个实体（admin 命令 entity.unload，D16，维护者 2026-10-20 决定 C）。
//
// 把一个常驻实体从本进程内存卸载、不删持久数据，下一次访问按正常冷加载从 Mongo 读回。卸载本身与 DataEngine 驱逐
// 被跳过的原生步骤、Remote 持久拒绝后的卸载是同一入口（ManagerAccess.Unload，DestroyReasonMemoryUnload，同一条
// Sync 规则）；不同的是那两条路径卸载的是“内存已不可信”的实例，这里卸载的是正常实例，所以先保证内存里的每笔修改
// 都已落库，否则卸载会丢掉已提交、未投影的修改：
//
//  1. 屏障（业务池，持实体锁）：取得实体锁，即等正在执行、持有该实体的事务结束——它的 WAL 准入在锁内，结束时
//     要么已登记为在途投影，要么已回滚；然后在 Projector 的准入临界区（heldMu）登记卸载屏障。之后写到该实体的
//     事务在 WAL 准入处以可重试的 coredata.ErrEntityUnloading 拒绝，Nest 在 Guard 内整体回滚，内存不会再出现未落库
//     的修改。锁序与锁内准入相同：实体锁 → heldMu。
//  2. 排空（命令 goroutine，可以等待）：等屏障之前已准入、写到该实体的全部记录投影完成（Projector.WaitEntityProjection，
//     冷加载用的同一个屏障）。
//  3. 卸载（业务池）：ManagerAccess.Unload（自己取实体锁）。屏障下不会有新记录准入，第 2 步之后“内存 == 库”一直成立；
//     这段时间执行的写事务都在准入处被拒绝并回滚。
//  4. 撤销屏障（defer，任何结果都执行）。屏障只在内存里，进程重启即消失；启动恢复本来就先排空 WAL。
//
// 失败时不强行卸载：实体保持常驻、屏障撤销，内存与正常运行时一样（被拒绝的写入已回滚），见 ErrEntityUnloadBusy /
// ErrEntityUnloadTimeout。没有常驻的实体什么也不做（下一次访问本来就冷加载）。不是自动持久化的实体拒绝卸载（库里
// 没有它，卸载等于丢弃）。Remote 托管实体走同一流程（Remote 写入同样经 WAL 准入与投影）；所有权状态处于 fenced /
// recovering / quarantined 或 sharing / draining 迁移中的，由 Remote 运行时负责收尾，按忙拒绝。

var (
	// ErrEntityUnloadBusy 表示实体正忙、这次没有卸载：持有它的事务在 timeout 内没有结束（第 1、3 步取实体锁超时）、
	// 已有另一次 entity.unload 在卸载它，或 Remote 所有权不在稳定状态。实体不受影响，稍后重试。
	ErrEntityUnloadBusy = errors.New("dataengine unload: entity is busy; nothing was unloaded")
	// ErrEntityUnloadTimeout 表示屏障之前已准入的记录在 timeout 内没有投影完（Mongo 慢或中断、投影积压）。
	// 实体不受影响、屏障已撤销；记录仍在 WAL 里照常投影，投影恢复后重试。
	ErrEntityUnloadTimeout = errors.New("dataengine unload: the entity's admitted transactions were not projected in time; nothing was unloaded")
	// ErrEntityUnloadNotPersistent 表示实体不是自动持久化的（AutoPersist=false）：库里没有它，卸载就是丢弃。
	ErrEntityUnloadNotPersistent = errors.New("dataengine unload: entity is not persisted; unloading it would discard its state")
)

// EntityUnloadResult 是 UnloadEntity 的结果。
type EntityUnloadResult struct {
	EntityID int64
	// Resident：命令开始时实体常驻本进程内存。false 时什么也没做。
	Resident bool
	// Unloaded：命令看到的常驻实例已离开内存（本命令卸载了它，或期间被框架的其他路径卸载）。之后实体可能已被下一次
	// 访问（例如仍有订阅者时的 unload_resync）重新加载，那是从库读回的新实例。Resident 而 Unloaded=false 只在出错时出现。
	Unloaded bool
	// Drained：屏障登记时该实体在途、本命令等到投影完成的事务数。
	Drained int
}

// UnloadEntity 排空实体 id（完整 Entity ID）的在途投影后把它从本进程内存卸载，期间拒绝写到它的事务（见本文件开头）。
// 会等待，只能在业务池之外调用（admin 命令的 goroutine、io 池）；在业务 worker 上调用直接返回
// fctx.ErrBlockingInBusinessWorker，不做任何事。ctx 的截止时间约束取实体锁与等待投影，到期返回
// ErrEntityUnloadBusy / ErrEntityUnloadTimeout。两者都是确定的“没有卸载”，不包裹 ctx 的错误：admin 审计按
// context.DeadlineExceeded 把结果记成 unknown，而这里结果是已知的。
func (runtime *Runtime) UnloadEntity(ctx context.Context, id int64) (result EntityUnloadResult, err error) {
	result.EntityID = id
	if err := fctx.BlockingError("dataengine.UnloadEntity"); err != nil {
		return result, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if runtime == nil || runtime.Projector == nil || runtime.access == nil || runtime.access.Manager() == nil {
		return result, errors.New("dataengine unload: runtime is not configured")
	}
	if !runtime.Ready() {
		return result, coredata.ErrRecoveryIncomplete
	}
	defer func() {
		metrics.IncCounter("dataengine.entity_unload.total", metrics.Labels{"result": unloadOutcome(result, err)}, 1)
	}()
	resident := runtime.access.Manager().Get(id)
	if resident == nil {
		return result, nil
	}
	result.Resident = true
	// 第 1、3 步需要实体锁，交给 Nest 的业务池（Projector 绑定的 RunLocal，与驱逐同一入口）；未装配 Nest 时就地执行。
	local := entity.WithLocalExecutor(ctx, runtime.Projector.localRun())

	// 1. 屏障。
	var release func()
	var barrierErr error
	if runErr := entity.RunLocal(local, func() { release, barrierErr = runtime.raiseUnloadBarrier(ctx, resident) }); runErr != nil {
		// RunLocal 只在 fn 没执行或 fn 内 panic 时报错；屏障若已登记也要撤销，不能留在准入处挡住该实体。
		if release != nil {
			release()
		}
		return result, fmt.Errorf("dataengine unload: entity %d: run on the business pool: %w", id, runErr)
	}
	if barrierErr != nil {
		return result, barrierErr
	}
	if release == nil {
		// 取锁期间已被别的路径卸载（驱逐、Remote 收尾、业务 Destroy）。
		result.Unloaded = true
		return result, nil
	}
	defer release()

	// 2. 排空。
	result.Drained = runtime.Projector.pendingEntityProjections(id)
	if err := runtime.Projector.WaitEntityProjection(ctx, id); err != nil {
		if ctx.Err() != nil {
			return result, fmt.Errorf("%w: entity %d: %d of %d admitted transaction(s) still unprojected: %v",
				ErrEntityUnloadTimeout, id, runtime.Projector.pendingEntityProjections(id), result.Drained, err)
		}
		return result, fmt.Errorf("dataengine unload: entity %d: wait for its projections: %w", id, err)
	}

	// 3. 卸载：卸载此刻常驻的实例。排空期间被驱逐后重新加载的实例同样来自库（屏障下没有新记录），一并卸载。
	// Unloaded 在业务池里、卸载之后立刻判断：返回之后实体可能已被订阅者的重载读回（新实例），不能再按 Get(id) 判断。
	var unloadErr error
	runErr := entity.RunLocal(local, func() {
		current := runtime.access.Manager().Get(id)
		if current == nil {
			result.Unloaded = true
			return
		}
		unloadErr = unloadOnce(ctx, runtime.access, current)
		result.Unloaded = current.IsRemoved()
	})
	if runErr != nil {
		return result, fmt.Errorf("dataengine unload: entity %d: run on the business pool: %w", id, runErr)
	}
	if unloadErr != nil {
		if !result.Unloaded {
			if errors.Is(unloadErr, context.DeadlineExceeded) || errors.Is(unloadErr, context.Canceled) {
				return result, fmt.Errorf("%w: entity %d is held by a running transaction: %v", ErrEntityUnloadBusy, id, unloadErr)
			}
			return result, fmt.Errorf("dataengine unload: entity %d: %w", id, unloadErr)
		}
		// 实例已离开内存，只是业务 OnDestroy 等生命周期回调失败：卸载已经完成，回调错误只记录。
		slog.Error("dataengine unload: a lifecycle hook failed while unloading the entity", "entity", id, "err", unloadErr)
	}
	return result, nil
}

// raiseUnloadBarrier 是第 1 步，在业务池执行：持实体锁（ManagerAccess.WithLocked，等持有实体的在途事务结束、受 ctx
// 约束）确认它仍是当前托管实例、可以卸载，然后登记屏障。返回 (nil, nil) 表示实例已不在内存。
func (runtime *Runtime) raiseUnloadBarrier(ctx context.Context, value entity.IThreadSafeEntity) (func(), error) {
	id := value.ID()
	if !value.AutoPersist() {
		return nil, fmt.Errorf("%w: entity %d", ErrEntityUnloadNotPersistent, id)
	}
	var release func()
	var barrierErr error
	err := runtime.access.WithLocked(ctx, value, func() {
		if remote, ok := value.(entity.IThreadSafeRemoteEntity); ok && entity.IsEntityKindRemoteManaged(value.GetEntityKind()) {
			switch state := remote.RemoteOwnershipState(); state {
			case entity.RemoteOwnershipLocalOwned, entity.RemoteOwnershipShared:
			default:
				barrierErr = fmt.Errorf("%w: remote entity %d ownership is %s; the Remote runtime owns its recovery", ErrEntityUnloadBusy, id, state)
				return
			}
		}
		release, barrierErr = runtime.Projector.beginEntityUnload(id)
	})
	switch {
	case errors.Is(err, entity.ErrEntityRemoved) || errors.Is(err, entity.ErrEntityNotManaged):
		return nil, nil
	case errors.Is(err, entity.ErrEntityNil):
		return nil, fmt.Errorf("dataengine unload: entity %d: %w", id, err)
	case err != nil:
		return nil, fmt.Errorf("%w: entity %d is held by a running transaction: %v", ErrEntityUnloadBusy, id, err)
	}
	return release, barrierErr
}

// unloadOnce 把卸载里的 panic（业务 OnDestroy 等）转成错误，不打穿业务池续行（与 Remote 收尾的卸载相同）。
func unloadOnce(ctx context.Context, access *entity.ManagerAccess, value entity.IThreadSafeEntity) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("unload panic: %v", recovered)
		}
	}()
	return access.Unload(ctx, value)
}

func unloadOutcome(result EntityUnloadResult, err error) string {
	switch {
	case err == nil && !result.Resident:
		return "not_resident"
	case err == nil:
		return "unloaded"
	case errors.Is(err, ErrEntityUnloadBusy):
		return "busy"
	case errors.Is(err, ErrEntityUnloadTimeout):
		return "timeout"
	default:
		return "failed"
	}
}

// beginEntityUnload 在 heldMu 下登记实体 id 的卸载屏障，返回撤销函数。与 reserve 的检查同一把锁：登记之后准入的
// 记录一定看得见屏障。Projector 已 fatal / 关闭时拒绝（不会再投影，排空无从谈起）。
func (p *Projector) beginEntityUnload(id int64) (func(), error) {
	p.heldMu.Lock()
	defer p.heldMu.Unlock()
	if fatal := p.fatal(); fatal != nil {
		return nil, fatal
	}
	if terminal := p.walTerminal(); terminal != nil {
		return nil, terminal
	}
	if p.ctx.Err() != nil {
		return nil, ErrRuntimeStopped
	}
	if _, busy := p.unloadingEntities[id]; busy {
		return nil, fmt.Errorf("%w: another entity.unload of entity %d is in progress", ErrEntityUnloadBusy, id)
	}
	if p.unloadingEntities == nil {
		p.unloadingEntities = make(map[int64]uint64)
	}
	p.unloadToken++
	token := p.unloadToken
	p.unloadingEntities[id] = token
	return func() {
		p.heldMu.Lock()
		if p.unloadingEntities[id] == token {
			delete(p.unloadingEntities, id)
		}
		p.heldMu.Unlock()
	}, nil
}

// checkUnloadingEntitiesLocked 在 reserve 的 heldMu 临界区内拒绝写到卸载中实体的新记录。
func (p *Projector) checkUnloadingEntitiesLocked(record coredata.CommitRecord) error {
	if len(p.unloadingEntities) == 0 {
		return nil
	}
	for _, id := range recordEntityIDs(record) {
		if _, unloading := p.unloadingEntities[id]; unloading {
			p.unloadRejected.Add(1)
			metrics.IncCounter("dataengine.entity_unload.admission_rejected.total", nil, 1)
			return fmt.Errorf("%w: entity %d", coredata.ErrEntityUnloading, id)
		}
	}
	return nil
}

// pendingEntityProjections 是实体 id 当前在途（已准入、未投影完）的事务数。
func (p *Projector) pendingEntityProjections(id int64) int {
	p.heldMu.RLock()
	defer p.heldMu.RUnlock()
	return len(p.pendingEntities[id])
}
