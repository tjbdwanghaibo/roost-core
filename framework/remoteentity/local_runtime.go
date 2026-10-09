package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// BindLocalExecutor 由 Nest 构造时调用（nest.LocalExecutorBinder，与 DataEngine 的驱逐同一接线，
// RR-20260926-30）：run 是 NestMgr.RunLocal，把需要 Entity 锁或业务回调的步骤放进快池续行通道并等待。
// 后台收尾（finalizer）不属于任何 Nest 消息，拿不到消息的快池续行，回滚、仅内存卸载与提交后回调都经它
// 回到快池。未绑定（独立使用 remoteentity、没有 Nest）时 entity.RunLocal 就地执行。
func (m *Manager) BindLocalExecutor(run func(func()) error) {
	if m == nil || run == nil {
		return
	}
	m.localExecutor.Store(&run)
}

// localContext 给后台收尾的 ctx 附上 Nest 的本地执行入口；未绑定时原样返回（RunLocal 就地执行）。
// 只用于不属于任何消息的收尾路径：消息收尾（batch.Close/Commit）的 ctx 已带消息自己的快池续行。
func (m *Manager) localContext(ctx context.Context) context.Context {
	if run := m.localExecutor.Load(); run != nil {
		return entity.WithLocalExecutor(ctx, *run)
	}
	return ctx
}

// runLocalStep 在本地执行入口运行 fn，并报告 fn 是否真的执行过：NestMgr.RunLocal 在 Nest 未启动、
// 停机中或已 fence 时拒绝投递，fn 不会执行（区别于 fn 执行中 panic）。
func runLocalStep(ctx context.Context, fn func()) (ran bool, err error) {
	err = entity.RunLocal(ctx, func() {
		ran = true
		fn()
	})
	return ran, err
}

// unloadRejectedEntities 在持久拒绝收尾、gate 释放之后，把批次内持有被拒绝修改的旧实例从本进程内存
// 卸载（不删持久数据），下一次访问经 loader 从权威重新加载（RR-20260926-39）。与 DataEngine 驱逐被
// 跳过的原生步骤留下的实体（RR-20260926-30）同一机制：正式 loader 的实现就是 ManagerAccess.Unload，
// 同一个本地执行入口、同一个 DestroyReasonMemoryUnload、同一条 Sync 规则——旧实例的 SubjectSyncState
// 随卸载关闭（被拒绝的冻结内容一并丢弃），订阅保持登记、不发 remove，实例重新加载后由 kit 的
// EntityRepository.OnEntityLoaded → entitysync.Manager.Rebind 对原订阅者强制全量。
//
// 旧实例保持 Quarantined，从不解冻（RR-28 复核约束：内存≠权威时不解冻）。已卸载的条目记在
// entry.unloaded，重试只处理剩余条目。loader 不支持卸载时返回 entity.ErrRemoteUnloadUnsupported：
// 实例保持隔离，等业务自行重新加载（旧行为）。本地执行入口拒绝投递（Nest 不在运行）时返回错误，
// 调用方保持隔离并重试。
func (m *Manager) unloadRejectedEntities(ctx context.Context, entries []*remoteWriteEntry) (err error) {
	defer func() {
		if errors.Is(err, entity.ErrRemoteUnloadUnsupported) {
			// 不会由框架重载：撤销“重载中”标记，下一写者回到通用的 ErrRemoteFenced（隔离到业务自行重载）。
			clearRejectedReloadPending(entries)
		}
	}()
	unloader, ok := m.backend.(entity.IRemoteEntityUnloader)
	if !ok {
		return entity.ErrRemoteUnloadUnsupported
	}
	var failed error
	ran, runErr := runLocalStep(ctx, func() {
		for _, entry := range entries {
			if entry == nil || entry.entity == nil || entry.unloaded {
				continue
			}
			stale := entry.entity
			if err := unloadRemoteEntityOnce(ctx, unloader, stale); err != nil {
				if errors.Is(err, entity.ErrRemoteUnloadUnsupported) {
					failed = errors.Join(failed, err)
					return
				}
				if !stale.IsRemoved() {
					failed = errors.Join(failed, fmt.Errorf("remote_entity: unload rejected entity %d: %w", entry.lease.EntityID, err))
					continue
				}
				// 实例已离开 EntityManager（业务 OnDestroy 等生命周期回调在移除之后失败）：内存换代已经完成，
				// 回调错误只记录，不重试卸载。
				slog.Error("remote_entity: lifecycle hook failed while unloading a rejected entity", "entity", entry.lease.EntityID, "err", err)
			}
			if entry.wrapper != nil {
				entry.wrapper.detachEntity(stale)
			}
			entry.unloaded = true
			metrics.IncCounter("remote_entity.rejected_unload_total", nil, 1)
		}
	})
	if !ran && runErr == nil {
		runErr = errors.New("remote_entity: local executor did not run the unload")
	}
	return errors.Join(runErr, failed)
}

// markRejectedReloadPending 在持久拒绝收尾（回滚 + 隔离）之后，把批次内的旧实例登记为“等待框架卸载重载”
// （RR-20260926-62）：gate 释放到卸载完成之间，写准入对它返回可重试的 entity.ErrRemoteEntityReloading（包裹
// ErrRemoteFenced）。loader 不能卸载时不登记——实例只能隔离到业务自行重载，继续是通用的 ErrRemoteFenced。
func (m *Manager) markRejectedReloadPending(entries []*remoteWriteEntry) {
	if _, ok := m.backend.(entity.IRemoteEntityUnloader); !ok {
		return
	}
	for _, entry := range entries {
		if entry != nil && entry.wrapper != nil && entry.entity != nil && !entry.unloaded {
			entry.wrapper.markRejectedReload(entry.entity)
		}
	}
}

func clearRejectedReloadPending(entries []*remoteWriteEntry) {
	for _, entry := range entries {
		if entry != nil && entry.wrapper != nil {
			entry.wrapper.clearRejectedReload(entry.entity)
		}
	}
}

// unloadRemoteEntityOnce 把卸载里的 panic（业务 OnDestroy 等）转成错误，不让它打穿 finalizer 或快池续行。
func unloadRemoteEntityOnce(ctx context.Context, unloader entity.IRemoteEntityUnloader, stale entity.IThreadSafeRemoteEntity) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("unload panic: %v", r)
		}
	}()
	return unloader.UnloadRemoteEntity(ctx, stale)
}
