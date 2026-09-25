package remoteentity

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

var _ entity.RemoteOwnershipManager = (*Manager)(nil)

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

// ClaimRemoteOwnership atomically creates the first local ownership
// generation. It is idempotent for this server and fails closed when another
// server won the claim.
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
		// Same shape as Transfer (U-0188): the script may have flipped the
		// authority to shared and only the reply was lost (RR-20260913-12).
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

// TransferRemoteOwnership drains admitted local writes and, for shared mode,
// holds the distributed entity lock while advancing marker and route epochs.
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

// settleIndeterminateOwnership decides what a failed ownership CAS means —
// Transfer (U-0188, RR-20260913-09), EnterShared and LeaveShared (U-0189,
// RR-20260913-12) share it. An error from the store is not "the CAS did not
// run": the script may have changed the authority and only the reply was
// lost. Treating it as a no-op restored the previous local mode while the
// hot marker (trusted for MarkerCacheTTL) still described it, and the next
// PrepareRemoteWriteBatch admitted a writer whose mode the authority had
// already changed — for EnterShared that means an exclusive fast-path write
// while the authority says shared and other nodes coordinate through the
// distributed lock.
//
// So: forget the cached marker, ask the authority again on an independent
// bounded context (the caller's may already be the thing that failed), and
// let the answer decide —
//
//   - authority unchanged: proven no-op, restore the previous local mode;
//   - applied(current): the CAS happened, finish it as a success (live goes
//     to appliedState, the current lease is returned with a nil error);
//   - authority still names us but in some other lease: sync live to the
//     authority's mode and report the failure;
//   - authority names someone else / nothing: fence, report the fence;
//   - authority unreachable: freeze the live entity in Recovering with the
//     marker unknown. Nothing is admitted until a later authoritative read
//     succeeds (batch admission thaws Recovering only after such a read).
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

// freezeAttachedLive parks a live entity in Recovering. Draining reaches it
// directly; Sharing (EnterShared in flight) has to go through Fenced first,
// which is the only path the state table allows.
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

// transitionAttachedLive moves an already-loaded live entity to state under
// its own mutex; a nil entity is nothing to move.
func transitionAttachedLive(live entity.IThreadSafeRemoteEntity, state entity.RemoteOwnershipState) error {
	if live == nil || live.GetMutex() == nil {
		return nil
	}
	live.GetMutex().Lock()
	defer live.GetMutex().Unlock()
	return live.TransitionRemoteOwnership(state)
}

// beginOwnershipTransition serializes with local write admission. When
// lockShared is true it also acquires the distributed lock if the observed
// state is shared, then refreshes ownership under that lock before returning.
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
		// Unknown: first contact. Recovering: frozen by an indeterminate
		// transition (U-0188); beginOwnershipTransition re-read the authority
		// and lease names us again, so thaw before moving on.
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
