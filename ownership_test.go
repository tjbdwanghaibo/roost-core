package remoteentity

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/cube-core/entity"
)

func TestConcurrentOwnershipClaimElectsExactlyOneOwner(t *testing.T) {
	const kind entity.EntityKind = 132
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id := testRemoteFullIDWithKind(1412, 1, kind)
	store := newMockMarkerStore()
	first := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	second := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 2000)
	first.SetOwnershipStore(store)
	second.SetOwnershipStore(store)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, mgr := range []*remoteEntityManager{first, second} {
		wg.Add(1)
		go func(m *remoteEntityManager) {
			defer wg.Done()
			_, err := m.ClaimRemoteOwnership(context.Background(), id)
			errs <- err
		}(mgr)
	}
	wg.Wait()
	close(errs)
	succeeded, fenced := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, entity.ErrRemoteFenced):
			fenced++
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if succeeded != 1 || fenced != 1 {
		t.Fatalf("claim results: succeeded=%d fenced=%d", succeeded, fenced)
	}
	lease, found, err := first.GetRemoteOwnership(context.Background(), id)
	if err != nil || !found || !validOwnershipLease(lease) || (lease.OwnerSid != 1000 && lease.OwnerSid != 2000) {
		t.Fatalf("ownership lease=%+v found=%v err=%v", lease, found, err)
	}
}

func TestOwnershipModeTransitionsAdvanceMarkerEpoch(t *testing.T) {
	const kind entity.EntityKind = 133
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id := testRemoteFullIDWithKind(1413, 1, kind)
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	mgr.SetOwnershipStore(newMockMarkerStore())

	claimed, err := mgr.ClaimRemoteOwnership(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := mgr.EnterRemoteSharedMode(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	local, err := mgr.LeaveRemoteSharedMode(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Shared || !shared.Shared || local.Shared || claimed.MarkerEpoch+1 != shared.MarkerEpoch || shared.MarkerEpoch+1 != local.MarkerEpoch {
		t.Fatalf("claimed=%+v shared=%+v local=%+v", claimed, shared, local)
	}
}
