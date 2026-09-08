package remoteentity

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

var _ entity.RemoteOwnershipManager = (*remoteEntityManager)(nil)

func (m *remoteEntityManager) GetRemoteOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error) {
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
func (m *remoteEntityManager) ClaimRemoteOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, error) {
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

func (m *remoteEntityManager) EnterRemoteSharedMode(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, error) {
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
		restoreErr := m.restoreOwnershipAfterTransitionFailure(wrapper.attachedEntity(), expected)
		return entity.RemoteEntityMarkerLease{}, errors.Join(fmt.Errorf("remote_entity: enter shared %d: %w", wrapper.id, err), restoreErr)
	}
	wrapper.applyOwnership(next)
	if err := m.applyLiveOwnership(ctx, wrapper, next, entity.RemoteOwnershipShared, 0); err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	return next, nil
}

func (m *remoteEntityManager) LeaveRemoteSharedMode(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, error) {
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
		restoreErr := m.restoreOwnershipAfterTransitionFailure(wrapper.attachedEntity(), expected)
		return entity.RemoteEntityMarkerLease{}, errors.Join(fmt.Errorf("remote_entity: leave shared %d: %w", wrapper.id, err), restoreErr)
	}
	wrapper.applyOwnership(next)
	if err := m.applyLiveOwnership(ctx, wrapper, next, entity.RemoteOwnershipLocalOwned, 0); err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	return next, nil
}

// TransferRemoteOwnership drains admitted local writes and, for shared mode,
// holds the distributed entity lock while advancing marker and route epochs.
func (m *remoteEntityManager) TransferRemoteOwnership(ctx context.Context, id int64, newOwnerSID int32) (entity.RemoteEntityMarkerLease, error) {
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
		restoreErr := m.restoreOwnershipAfterTransitionFailure(wrapper.attachedEntity(), expected)
		return entity.RemoteEntityMarkerLease{}, errors.Join(fmt.Errorf("remote_entity: transfer %d: %w", wrapper.id, err), restoreErr)
	}
	wrapper.applyOwnership(next)
	if err := m.applyLiveOwnership(ctx, wrapper, next, entity.RemoteOwnershipFenced, newOwnerSID); err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	return next, nil
}

// beginOwnershipTransition serializes with local write admission. When
// lockShared is true it also acquires the distributed lock if the observed
// state is shared, then refreshes ownership under that lock before returning.
func (m *remoteEntityManager) beginOwnershipTransition(parent context.Context, id int64, lockShared bool) (*remoteEntityWrapper, context.Context, func(), error) {
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

func (m *remoteEntityManager) ownershipContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}
	}
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

func (m *remoteEntityManager) transitionLiveOwnership(ctx context.Context, wrapper *remoteEntityWrapper, lease entity.RemoteEntityMarkerLease, target entity.RemoteOwnershipState) error {
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
	if remote.RemoteOwnershipState() == entity.RemoteOwnershipUnknown {
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

func (m *remoteEntityManager) applyLiveOwnership(ctx context.Context, wrapper *remoteEntityWrapper, lease entity.RemoteEntityMarkerLease, state entity.RemoteOwnershipState, excludeSID int32) error {
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

func (m *remoteEntityManager) restoreOwnershipAfterTransitionFailure(live entity.IThreadSafeRemoteEntity, lease entity.RemoteEntityMarkerLease) error {
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
