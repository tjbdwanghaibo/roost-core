package entity

import (
	"fmt"

	flog "github.com/tjbdwanghaibo/roost-core/log"
)

// Create builds and publishes an entity while holding its mutex until the
// short-lived guard scope is released. Later mutations must enter through Nest.
//
// 在 Nest handler 内新建（param.IsCreate 且当前 Guard 作用域绑定了 CreatedEntityCapturer）时，
// Create 就是 handler 内创建：沿用当前作用域走 CreateInScope，锁随 handler 的 Guard 释放。
// 事务 handler 与 Cast 一样进入同一提交边界，回滚 / 拒绝时撤销发布（RR-20260926-35 复核：生成
// Lifecycle 的 Create / GetOrCreate 都经过这里）；memory 快路径 handler 同样持锁到 handler 结束并进入
// 本次 SyncMutation（RR-20260926-48）。取锁按 Cast 的锁序，冲突时返回可重试错误，见 CreateInScope。
// 加载已持久的实体（IsCreate=false，Repository 聚合加载）和 Nest 之外的调用保持
// 短作用域立即发布的原语义。
func (m *EntityManager) Create(param *EntityCreateParam) (IThreadSafeEntity, error) {
	if m == nil {
		return nil, ErrEntityNotManaged
	}
	if param != nil && param.IsCreate {
		if scope := CurrentGuardScope(); scope != nil && scope.guard != nil && scope.guard.createdCapturer != nil {
			return m.CreateInScope(scope, param)
		}
	}
	var result IThreadSafeEntity
	err := WithGuardScope("entity_create", func(scope *GuardScope) error {
		value, err := m.CreateInScope(scope, param)
		if err != nil {
			return err
		}
		result = value
		return nil
	})
	return result, err
}

// CreatedEntityCapturer 接收 Nest handler 内由 CreateInScope 新建的实体（RR-20260926-35）。
// 持锁执行器（Nest）在业务调用期间把它绑定到 Guard 上：事务 handler 让新实体与动态 Cast 一样进入
// 回滚与持久化边界，memory 快路径 handler 只借它表明“新实体随本 handler 的 Guard 持锁”。
//
// CaptureCreatedEntity：revoke 撤销这次发布，事务在回滚 / 明确拒绝时调用；重复调用无副作用。
// 返回错误时 CreateInScope 立即撤销发布并把错误交给业务。
//
// CreatedEntityLockBusy：按锁序不能等待的新实体锁被其他持有者占用（RR-20260926-48）。返回交给业务的错误：
// 可回滚的 Nest 事务给可重试的锁超时类错误，并据此在 handler 结束时整条回滚并重新准入；不能回滚的 handler
// （memory 快路径）给不可自动重排的冲突错误，消息不重排（RR-20260926-64）。
type CreatedEntityCapturer interface {
	CaptureCreatedEntity(created IThreadSafeEntity, revoke func()) error
	CreatedEntityLockBusy(id int64) error
}

// CreateInScope builds an entity, acquires its mutex in the existing
// deterministic guard scope, and only then publishes it. This ordering keeps a
// newly visible entity from being observed before its creator owns the lock.
//
// scope 属于正在执行的 Nest handler 时（Guard 上绑定了 CreatedEntityCapturer），新实体随事务
// 提交：先交给事务捕获回滚与持久化参与者，再加入当前 SyncMutation，Sync 在提交确认前不外发；
// 事务回滚或被拒绝时撤销发布（见 revokeCreated）。取锁遵循与 Cast 相同的锁序（见 lockCreated）：
// 不能有序等待时只 try-lock，被占用返回 capturer 给出的可重试错误，不发布、不持锁。
// 其余作用域（Create 自建的短作用域、Nest 之外的 WithGuardScope）保持等待取锁、立即发布的原语义。
//
// 发布失败（TryAdd 报 ErrEntityExists / ErrEntityRemoved）时归还本次取得的锁：同一作用域随后按同 ID
// 重试（生成 GetOrCreate）会新建实例，若锁仍以旧实例记在 Guard 上，新实例会在未加锁的情况下发布。
func (m *EntityManager) CreateInScope(scope *GuardScope, param *EntityCreateParam) (IThreadSafeEntity, error) {
	if m == nil {
		return nil, ErrEntityNotManaged
	}
	if scope == nil || scope.guard == nil {
		return nil, fmt.Errorf("entity guard scope is required")
	}
	value, err := m.build(param)
	if err != nil {
		return nil, err
	}
	guard := scope.guard
	capturer := guard.createdCapturer
	lockedNow := !guard.Guarded(value.GUId())
	if err := guard.lockCreated(value, capturer); err != nil {
		return nil, err
	}
	if err := m.TryAdd(value); err != nil {
		if lockedNow {
			guard.ReleaseEntity(value.GUId())
		}
		return nil, err
	}
	if capturer != nil {
		revoke := m.createdRevoker(guard, value)
		if err := capturer.CaptureCreatedEntity(value, revoke); err != nil {
			revoke()
			return nil, fmt.Errorf("entity: capture created entity %d in transaction: %w", value.ID(), err)
		}
	}
	if mutation := guard.syncMutation; mutation != nil {
		mutation.Include([]IThreadSafeEntity{value})
	}
	lifetime := EntityLifetimeDefault
	if base := value.Base(); base != nil {
		lifetime = base.Lifetime()
	}
	flog.Debug("entity: created", "id", value.ID(), "category", value.GetEntityCategory(), "kind", value.GetEntityKind(), "lifetime", lifetime)
	return value, nil
}

// createdRevoker 返回幂等的撤销函数。它只在持有该 Guard 的业务 goroutine 上调用
// （事务回滚 / 拒绝或捕获失败），所以用普通布尔值去重。
func (m *EntityManager) createdRevoker(guard *EntityGuard, e IThreadSafeEntity) func() {
	revoked := false
	return func() {
		if revoked {
			return
		}
		revoked = true
		m.revokeCreated(guard, e)
	}
}

// revokeCreated 撤销一次事务内发布，调用时 guard 仍持有新实体的锁：
//  1. 在锁内摘除索引、标记 removed、解除 owner——已经拿到指针的并发访问者在取锁后重新校验，
//     看到 removed 即退出（与 Destroy 相同的 GetLock 契约），Guard 释放时也不再运行 release hook；
//  2. 撤回同步：Nest 的同步调度器实现 SyncSubjectRetractor 时注销该 subject（已持有对象的会话收到
//     ObjectRemove，未持有的直接移除），然后关闭实体自己的同步状态，不再有内容可捕获；
//  3. Guard 释放全部锁之后，与 Destroy 同序执行销毁回调、清理实体资源、回收 LockManager 条目和
//     removing 标记，之后同一 ID 可以重新创建。
//
// 实体从未进入已提交的持久化记录：事务回滚时它的变更随 RollbackTx 一起丢弃，这里不做删除准入。
func (m *EntityManager) revokeCreated(guard *EntityGuard, e IThreadSafeEntity) {
	id := e.ID()
	m.addMu.Lock()
	if m.entities.Get(id) != e {
		// 已被其他路径摘除（例如业务在同一事务里 Destroy 了它），不重复收尾。
		m.addMu.Unlock()
		return
	}
	m.removing[id] = struct{}{}
	e.SetRemoved()
	m.entities.Del(id)
	m.removeGroupIndex(e)
	e.Base().setOwner(nil)
	m.addMu.Unlock()
	flog.Debug("entity: creation revoked", "id", id, "category", e.GetEntityCategory(), "kind", e.GetEntityKind())

	if state := e.Base().Sync(); state != nil {
		if mutation := guard.syncMutation; mutation != nil {
			if retractor, ok := mutation.observer.(SyncSubjectRetractor); ok {
				retractor.RetractSyncSubject(state)
			}
		}
		state.Close()
	}
	guard.AppendPostRelease(func() {
		defer func() {
			if m.locks != nil {
				m.locks.ReleaseLock(id)
			}
			m.addMu.Lock()
			delete(m.removing, id)
			m.addMu.Unlock()
		}()
		defer e.ClearBase()
		e.Base().DestroyAll(DestroyReasonCommon)
		e.OnDestroy(DestroyReasonCommon)
	})
}

func (m *EntityManager) build(param *EntityCreateParam) (IThreadSafeEntity, error) {
	if m == nil {
		return nil, ErrEntityNotManaged
	}
	if param == nil {
		return nil, fmt.Errorf("entity create param is nil")
	}
	m.configMu.RLock()
	defer m.configMu.RUnlock()
	if param.IsCreate && param.Id == 0 && param.UniqueID == 0 {
		if m.idGen == nil {
			return nil, ErrIDGeneratorRequired
		}
		rawID, err := m.idGen()
		if err != nil {
			return nil, fmt.Errorf("generate entity id: %w", err)
		}
		if err := param.setRawID(int64(rawID), param.Kind); err != nil {
			return nil, err
		}
	}
	if param.Mutex == nil {
		param.Mutex = m.locks.GetLock(param.Id)
	}
	if param.Mutex.LockId() != param.Id {
		return nil, fmt.Errorf("entity manager: mutex id=%d does not match entity id=%d", param.Mutex.LockId(), param.Id)
	}
	value, err := BuildEntity(param)
	if err != nil {
		return nil, err
	}
	return value, nil
}
