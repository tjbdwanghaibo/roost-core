package remoteentity

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
)

// A denied facade is not evidence that a native lock is busy. Exercise the
// owner lifecycle with an otherwise idle entity and verify the actual grants.
func TestRemoteOwnershipWithoutPublicLock(t *testing.T) {
	m, authority, live := modeSetup(t, rr44Kind, 9320)
	ctx, id := t.Context(), live.GUId()
	if !nativeLockFree(live) {
		t.Fatal("premise: native lock should be free")
	}
	if facade := live.GetMutex(); facade.TryLock() {
		facade.Unlock()
		t.Fatal("public facade admitted a native lock operation")
	}
	admit := func(want entity.RemoteOwnershipState, epoch uint64) {
		t.Helper()
		batch, err := m.PrepareRemoteWriteBatch(ctx, []int64{id})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := batch.Close(ctx); err != nil {
				t.Error(err)
			}
		}()
		e := batch.(*remoteWriteBatch).entries[0]
		shared := want == entity.RemoteOwnershipShared
		if e.entity != live || live.RemoteOwnershipState() != want || e.lease.MarkerEpoch != epoch || e.distLock != shared || e.wrapper.rMu.IsAcquired() != shared {
			t.Fatalf("admission: state=%s lease=%+v distributed=%t, want %s epoch=%d", live.RemoteOwnershipState(), e.lease, e.distLock, want, epoch)
		}
	}
	initial, found, err := m.GetRemoteOwnership(ctx, id)
	if err != nil || !found {
		t.Fatalf("initial owner: found=%v err=%v", found, err)
	}
	admit(entity.RemoteOwnershipLocalOwned, initial.MarkerEpoch)
	shared, err := m.EnterRemoteSharedMode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	admit(entity.RemoteOwnershipShared, shared.MarkerEpoch)
	local, err := m.LeaveRemoteSharedMode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	admit(entity.RemoteOwnershipLocalOwned, local.MarkerEpoch)

	// A remote CAS supersedes the cached local marker. Expire only its age;
	// admission itself must reread authority, fence, and release the write gate.
	away, err := authority.TransferExpected(ctx, id, local, 2000)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := m.get(id)
	w.markerAt.Store(0)
	if err := probeAdmission(t, m, live); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("expired owner admission: %v", err)
	}
	back, err := authority.TransferExpected(ctx, id, away, m.localSid)
	if err != nil {
		t.Fatal(err)
	}
	w.markerAt.Store(0)
	admit(entity.RemoteOwnershipLocalOwned, back.MarkerEpoch)
}

// Probe from another goroutine: the native entity mutex is reentrant.
func nativeLockFree(e entity.IThreadSafeEntity) bool {
	free := make(chan bool, 1)
	go func() {
		mu := entity.SystemMutex(entitypermit.System(), e)
		ok := mu.TryLock()
		if ok {
			mu.Unlock()
		}
		free <- ok
	}()
	return <-free
}

type rollbackLockWitness struct {
	*testRemoteEntity
	called, lockFree bool
}

func (e *rollbackLockWitness) RollbackRemoteCommit(c entity.RemoteCommit) {
	e.called, e.lockFree = true, nativeLockFree(e)
	e.testRemoteEntity.RollbackRemoteCommit(c)
}

func TestRemoteAbortLocksExactInstanceDespiteSameIDGuard(t *testing.T) {
	m, loader := newRR44Manager(t)
	old := newTestRemoteEntity(9321, 1, rr44Kind)
	live := &rollbackLockWitness{testRemoteEntity: newTestRemoteEntity(9321, 1, rr44Kind)}
	loader.add(live)
	ctx := t.Context()
	batch, err := m.PrepareRemoteWriteBatch(ctx, []int64{live.ID()})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close(ctx)
	mu := entity.SystemMutex(entitypermit.System(), live)
	mu.Lock()
	live.dirty.set(true)
	err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(remoteTestTxID(183), "same-id rollback", "", true, 0))
	mu.Unlock()
	if err != nil || live.dirty.Dirty() {
		t.Fatalf("finalize: dirty=%t err=%v", live.dirty.Dirty(), err)
	}
	scope, release := entity.NewGuardScope(entitypermit.System(), "different instance with same ID")
	defer release()
	if !scope.Guard().RequireEntity(entitypermit.System(), old) || nativeLockFree(old) || !nativeLockFree(live) {
		t.Fatal("premise: only the old instance must be physically locked")
	}
	if err := batch.Abort(ctx, errors.New("rejected")); err != nil {
		t.Fatal(err)
	}
	if !live.called || live.lockFree || !live.dirty.Dirty() || !nativeLockFree(live) || nativeLockFree(old) {
		t.Fatal("rollback must restore dirty under the new instance's own lock, then release only that lock")
	}
}
