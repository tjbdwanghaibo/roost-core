package remoteentity

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

var _ entity.RemoteOwnershipManager = (*Manager)(nil)

// GetRemoteOwnership 读取实体在权威中的所有权记录。
func (m *Manager) GetRemoteOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error) {
	if m == nil || m.ownershipStore == nil {
		return entity.RemoteEntityMarkerLease{}, false, entity.ErrRemoteWriteCapabilityDisabled
	}
	meta := entity.ResolveEntityID(id)
	if meta.FullID == 0 || !entity.IsEntityKindRemoteManaged(meta.Kind) {
		return entity.RemoteEntityMarkerLease{}, false, fmt.Errorf("%w: invalid entity %d", entity.ErrRemoteRejected, id)
	}
	ctx, cancel := m.ownershipContext(ctx)
	defer cancel()
	return m.ownershipStore.GetOwnership(ctx, meta.FullID)
}

// ClaimRemoteOwnership 原子地创建第一代本地所有权。对本服务幂等；别的服务赢得认领时按 fence 失败。
func (m *Manager) ClaimRemoteOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, error) {
	wrapper, ctx, release, err := m.beginOwnershipTransition(ctx, id, false)
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	defer release()

	expected, found := wrapper.cachedOwnership()
	if found {
		if expected.OwnerSid == m.localSid {
			return expected, nil
		}
		return entity.RemoteEntityMarkerLease{}, ownershipFenceError(wrapper.id, expected)
	}
	next, err := m.ownershipStore.ClaimOwnership(ctx, wrapper.id, m.localSid)
	if err != nil {
		_ = wrapper.refreshMarked(ctx)
		current, _ := wrapper.cachedOwnership()
		return entity.RemoteEntityMarkerLease{}, errors.Join(ownershipFenceError(wrapper.id, current), err)
	}
	if !validOwnershipLease(next) || next.OwnerSid != m.localSid {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("%w: invalid claimed lease for entity=%d", entity.ErrRemoteFenced, wrapper.id)
	}
	wrapper.applyOwnership(next)
	if err := m.applyLiveOwnership(ctx, wrapper, next, entity.RemoteOwnershipLocalOwned, 0); err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	return next, nil
}

// EnterRemoteSharedMode 与本地写准入串行化后，把实体从本服务独占切换到共享模式；CAS 结果未知时按 settleIndeterminateOwnership 收敛。
func (m *Manager) EnterRemoteSharedMode(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, error) {
	wrapper, ctx, release, err := m.beginOwnershipTransition(ctx, id, false)
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	defer release()
	expected, found := wrapper.cachedOwnership()
	if !found || expected.OwnerSid != m.localSid || expected.Shared {
		return entity.RemoteEntityMarkerLease{}, ownershipFenceError(wrapper.id, expected)
	}
	if err := m.transitionLiveOwnership(ctx, wrapper, expected, entity.RemoteOwnershipSharing); err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	next, err := m.ownershipStore.EnterSharedExpected(ctx, wrapper.id, expected)
	if err != nil {
		// 与 Transfer 相同：脚本可能已把权威改成共享，只是回复丢失了。
		return m.settleIndeterminateOwnership(wrapper, expected, err, "enter shared",
			func(current entity.RemoteEntityMarkerLease) bool {
				return current.OwnerSid == m.localSid && current.Shared
			},
			entity.RemoteOwnershipShared, 0)
	}
	wrapper.applyOwnership(next)
	if err := m.applyLiveOwnership(ctx, wrapper, next, entity.RemoteOwnershipShared, 0); err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	return next, nil
}

// LeaveRemoteSharedMode 在分布式锁下把实体从共享模式切回本服务独占；CAS 结果未知时按 settleIndeterminateOwnership 收敛。
func (m *Manager) LeaveRemoteSharedMode(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, error) {
	wrapper, ctx, release, err := m.beginOwnershipTransition(ctx, id, true)
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	defer release()
	expected, found := wrapper.cachedOwnership()
	if !found || expected.OwnerSid != m.localSid || !expected.Shared {
		return entity.RemoteEntityMarkerLease{}, ownershipFenceError(wrapper.id, expected)
	}
	if err := m.transitionLiveOwnership(ctx, wrapper, expected, entity.RemoteOwnershipDraining); err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	next, err := m.ownershipStore.LeaveSharedExpected(ctx, wrapper.id, expected)
	if err != nil {
		return m.settleIndeterminateOwnership(wrapper, expected, err, "leave shared",
			func(current entity.RemoteEntityMarkerLease) bool {
				return current.OwnerSid == m.localSid && !current.Shared
			},
			entity.RemoteOwnershipLocalOwned, 0)
	}
	wrapper.applyOwnership(next)
	if err := m.applyLiveOwnership(ctx, wrapper, next, entity.RemoteOwnershipLocalOwned, 0); err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	return next, nil
}

// TransferRemoteOwnership 排空已准入的本地写，把所有权转给 newOwnerSID 并推进 marker 与路由代际；共享模式下全程
// 持有实体的分布式锁。
func (m *Manager) TransferRemoteOwnership(ctx context.Context, id int64, newOwnerSID int32) (entity.RemoteEntityMarkerLease, error) {
	if newOwnerSID == 0 || m == nil || newOwnerSID == m.localSid {
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("%w: invalid ownership transfer", entity.ErrRemoteOwnerTransition)
	}
	wrapper, ctx, release, err := m.beginOwnershipTransition(ctx, id, true)
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	defer release()
	expected, found := wrapper.cachedOwnership()
	if !found || expected.OwnerSid != m.localSid {
		return entity.RemoteEntityMarkerLease{}, ownershipFenceError(wrapper.id, expected)
	}
	if err := m.transitionLiveOwnership(ctx, wrapper, expected, entity.RemoteOwnershipDraining); err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	next, err := m.ownershipStore.TransferExpected(ctx, wrapper.id, expected, newOwnerSID)
	if err != nil {
		return m.settleIndeterminateOwnership(wrapper, expected, err, "transfer",
			func(current entity.RemoteEntityMarkerLease) bool { return current.OwnerSid == newOwnerSID },
			entity.RemoteOwnershipFenced, newOwnerSID)
	}
	wrapper.applyOwnership(next)
	if err := m.applyLiveOwnership(ctx, wrapper, next, entity.RemoteOwnershipFenced, newOwnerSID); err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	return next, nil
}

// settleIndeterminateOwnership 判定一次失败的所有权 CAS 意味着什么，Transfer、EnterShared 与 LeaveShared 共用它。
// 存储返回错误不等于“CAS 没执行”：脚本可能已改了权威，只是回复丢失。若当作空操作恢复原来的本地模式，而热 marker
// （在 MarkerCacheTTL 内被信任）仍描述旧模式，下一次 PrepareRemoteWriteBatch 就会按权威已改变的模式准入写者——
// 对 EnterShared 来说，就是权威已是共享、其他节点经分布式锁协调时，本节点仍走独占快路径写入。
//
// 所以：丢弃缓存的 marker，在独立的有界 ctx（调用方的 ctx 可能正是失败原因）上重新询问权威，由答案决定——
//
//   - 权威未变：证明是空操作，恢复原来的本地模式；
//   - applied(current)：CAS 已发生，按成功收尾（实例转到 appliedState，返回当前租约与 nil 错误）；
//   - 权威仍指向本节点但是别的租约：把实例同步到权威的模式并报告失败；
//   - 权威指向别人或没有记录：fence，报告 fence；
//   - 权威不可达：把实例冻结在 Recovering、marker 置为未知。直到之后一次权威读取成功前不准入任何写
//     （批次准入只在这样的读取之后才解冻 Recovering）。
func (m *Manager) settleIndeterminateOwnership(wrapper *remoteEntityWrapper, expected entity.RemoteEntityMarkerLease, cause error, op string,
	applied func(entity.RemoteEntityMarkerLease) bool, appliedState entity.RemoteOwnershipState, appliedExcludeSID int32) (entity.RemoteEntityMarkerLease, error) {
	opErr := fmt.Errorf("remote_entity: %s %d: %w", op, wrapper.id, cause)
	wrapper.invalidateMarker()
	recheckCtx, cancel := context.WithTimeout(context.Background(), m.cfg.OpTimeout)
	defer cancel()
	if err := wrapper.refreshMarked(recheckCtx); err != nil {
		wrapper.invalidateMarker()
		freezeErr := freezeAttachedLive(wrapper.attachedEntity())
		return entity.RemoteEntityMarkerLease{}, errors.Join(opErr,
			fmt.Errorf("%w: ownership of %d is unknown until the authority answers: %v", entity.ErrRemoteOwnerTransition, wrapper.id, err),
			freezeErr)
	}
	current, found := wrapper.cachedOwnership()
	switch {
	case found && current == expected:
		restoreErr := m.restoreOwnershipAfterTransitionFailure(wrapper.attachedEntity(), expected)
		return entity.RemoteEntityMarkerLease{}, errors.Join(opErr, restoreErr)
	case found && applied(current):
		if err := m.applyLiveOwnership(recheckCtx, wrapper, current, appliedState, appliedExcludeSID); err != nil {
			return entity.RemoteEntityMarkerLease{}, errors.Join(opErr, err)
		}
		return current, nil
	case found && current.OwnerSid == m.localSid:
		mode := entity.RemoteOwnershipLocalOwned
		if current.Shared {
			mode = entity.RemoteOwnershipShared
		}
		syncErr := m.applyLiveOwnership(recheckCtx, wrapper, current, mode, 0)
		return entity.RemoteEntityMarkerLease{}, errors.Join(opErr, syncErr)
	default:
		fenceErr := transitionAttachedLive(wrapper.attachedEntity(), entity.RemoteOwnershipFenced)
		if found {
			fenceErr = m.applyLiveOwnership(recheckCtx, wrapper, current, entity.RemoteOwnershipFenced, current.OwnerSid)
		}
		return entity.RemoteEntityMarkerLease{}, errors.Join(opErr, ownershipFenceError(wrapper.id, current), fenceErr)
	}
}

// freezeAttachedLive 把内存实例停在 Recovering。Draining 可以直接转过去；Sharing（EnterShared 在途）必须先经
// Fenced，这是状态表唯一允许的路径。
func freezeAttachedLive(live entity.IThreadSafeRemoteEntity) error {
	if live == nil || live.GetMutex() == nil {
		return nil
	}
	live.GetMutex().Lock()
	defer live.GetMutex().Unlock()
	if err := live.TransitionRemoteOwnership(entity.RemoteOwnershipRecovering); err == nil {
		return nil
	}
	if err := live.TransitionRemoteOwnership(entity.RemoteOwnershipFenced); err != nil {
		return err
	}
	return live.TransitionRemoteOwnership(entity.RemoteOwnershipRecovering)
}

// transitionAttachedLive 在实例自己的锁下把已加载的实例转到 state；实例为 nil 时什么也不做。
func transitionAttachedLive(live entity.IThreadSafeRemoteEntity, state entity.RemoteOwnershipState) error {
	if live == nil || live.GetMutex() == nil {
		return nil
	}
	live.GetMutex().Lock()
	defer live.GetMutex().Unlock()
	return live.TransitionRemoteOwnership(state)
}

// beginOwnershipTransition 与本地写准入串行化。lockShared 为 true 且观察到的状态是共享时，还取得分布式锁，并在锁下
// 刷新所有权后才返回。
func (m *Manager) beginOwnershipTransition(parent context.Context, id int64, lockShared bool) (*remoteEntityWrapper, context.Context, func(), error) {
	if m == nil || m.ownershipStore == nil || m.localSid == 0 {
		return nil, nil, nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	meta := entity.ResolveEntityID(id)
	if meta.FullID == 0 || !entity.IsEntityKindRemoteManaged(meta.Kind) {
		return nil, nil, nil, fmt.Errorf("%w: invalid entity %d", entity.ErrRemoteRejected, id)
	}
	ctx, cancel := m.ownershipContext(parent)
	wrapper := m.getOrCreate(meta.FullID, meta.Category, meta.Kind)
	if wrapper == nil {
		cancel()
		return nil, nil, nil, entity.ErrRemoteOverloaded
	}
	select {
	case wrapper.writeGate <- struct{}{}:
	case <-ctx.Done():
		wrapper.release()
		cancel()
		return nil, nil, nil, ctx.Err()
	}
	wrapper.ownershipMu.Lock()
	distLocked := false
	release := func() {
		if distLocked {
			_ = wrapper.unlockObserved(context.Background(), wrapper.rMu.Version())
		}
		wrapper.ownershipMu.Unlock()
		<-wrapper.writeGate
		wrapper.release()
		cancel()
	}
	if err := wrapper.refreshMarked(ctx); err != nil {
		release()
		return nil, nil, nil, fmt.Errorf("remote_entity: refresh ownership %d: %w", wrapper.id, err)
	}
	lease, found := wrapper.cachedOwnership()
	if lockShared && found && lease.Shared {
		if err := wrapper.rMu.Lock(ctx); err != nil {
			release()
			return nil, nil, nil, fmt.Errorf("remote_entity: transition lock %d: %w", wrapper.id, err)
		}
		distLocked = true
		if err := wrapper.refreshMarked(ctx); err != nil {
			release()
			return nil, nil, nil, fmt.Errorf("remote_entity: refresh locked ownership %d: %w", wrapper.id, err)
		}
	}
	return wrapper, ctx, release, nil
}

func (m *Manager) ownershipContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	// WithTimeout 自动采用更早的截止时间；调用者的长 deadline 不应放大框架预算。
	return context.WithTimeout(parent, m.cfg.OpTimeout)
}

func (w *remoteEntityWrapper) cachedOwnership() (entity.RemoteEntityMarkerLease, bool) {
	w.markerMu.RLock()
	lease := w.markerLease
	w.markerMu.RUnlock()
	marker := w.marker.Load()
	return lease, marker == markerLocal || marker == markerShared
}

func (w *remoteEntityWrapper) applyOwnership(lease entity.RemoteEntityMarkerLease) {
	w.setOwnership(lease, true)
}

func validOwnershipLease(lease entity.RemoteEntityMarkerLease) bool {
	return lease.OwnerSid != 0 && lease.MarkerEpoch != 0 && lease.RouteEpoch != 0
}

func ownershipFenceError(id int64, lease entity.RemoteEntityMarkerLease) error {
	return fmt.Errorf("%w: entity=%d owner=%d marker=%d route=%d shared=%t", entity.ErrRemoteFenced, id, lease.OwnerSid, lease.MarkerEpoch, lease.RouteEpoch, lease.Shared)
}

func (m *Manager) transitionLiveOwnership(ctx context.Context, wrapper *remoteEntityWrapper, lease entity.RemoteEntityMarkerLease, target entity.RemoteOwnershipState) error {
	live := wrapper.lookupLocalEntity()
	if live == nil {
		var err error
		live, err = wrapper.loadEntity(ctx)
		if err != nil {
			return fmt.Errorf("remote_entity: load live entity %d for transition: %w", wrapper.id, err)
		}
	}
	if live == nil || live.GetMutex() == nil {
		return nil
	}
	live.GetMutex().Lock()
	defer live.GetMutex().Unlock()
	remote, ok := live.(entity.IThreadSafeRemoteEntity)
	if !ok {
		return nil
	}
	switch remote.RemoteOwnershipState() {
	case entity.RemoteOwnershipUnknown, entity.RemoteOwnershipRecovering:
		// Unknown：首次接触。Recovering：被结果未知的转换冻结；beginOwnershipTransition 已重读权威、租约仍指向本节点，
		// 先解冻再继续。
		initial := entity.RemoteOwnershipLocalOwned
		if lease.Shared {
			initial = entity.RemoteOwnershipShared
		}
		if err := remote.TransitionRemoteOwnership(initial); err != nil {
			return err
		}
	}
	if err := remote.TransitionRemoteOwnership(target); err != nil {
		return err
	}
	wrapper.attachEntity(remote)
	return nil
}

// applyLiveOwnership 把新的所有权租约应用到内存实例（必要时先加载）：设置 ExcludeSId、marker / 路由代际与所有权状态。
func (m *Manager) applyLiveOwnership(ctx context.Context, wrapper *remoteEntityWrapper, lease entity.RemoteEntityMarkerLease, state entity.RemoteOwnershipState, excludeSID int32) error {
	live := wrapper.lookupLocalEntity()
	if live == nil {
		var err error
		live, err = wrapper.loadEntity(ctx)
		if err != nil {
			return fmt.Errorf("remote_entity: load live entity %d for ownership apply: %w", wrapper.id, err)
		}
	}
	if live == nil || live.GetMutex() == nil {
		return nil
	}
	live.GetMutex().Lock()
	defer live.GetMutex().Unlock()
	live.SetExcludeSId(excludeSID)
	if remote, ok := live.(entity.IThreadSafeRemoteEntity); ok {
		version := remote.RemoteVersionVector()
		version.MarkerEpoch = lease.MarkerEpoch
		version.RouteEpoch = lease.RouteEpoch
		if err := remote.SetRemoteVersionVector(version); err != nil {
			return err
		}
		if err := remote.TransitionRemoteOwnership(state); err != nil {
			return err
		}
		wrapper.attachEntity(remote)
	}
	return nil
}

// restoreOwnershipAfterTransitionFailure 在转换被证明未发生时，把实例恢复到租约对应的本地模式。
func (m *Manager) restoreOwnershipAfterTransitionFailure(live entity.IThreadSafeRemoteEntity, lease entity.RemoteEntityMarkerLease) error {
	if live == nil || live.GetMutex() == nil {
		return nil
	}
	live.GetMutex().Lock()
	defer live.GetMutex().Unlock()
	if remote, ok := live.(entity.IThreadSafeRemoteEntity); ok {
		target := entity.RemoteOwnershipLocalOwned
		if lease.Shared {
			target = entity.RemoteOwnershipShared
		}
		return remote.TransitionRemoteOwnership(target)
	}
	return nil
}
