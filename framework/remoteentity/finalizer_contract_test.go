package remoteentity

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"testing"
	"time"
)

func (e *reloadableFullDocEntity) RollbackRemoteCommit(entity.RemoteCommit) {
	e.rollbacks.Add(1)
	if !fctx.InFastWorker() {
		e.rollbacksOffFast.Add(1)
	}
}

func TestFinalizerRejectionRollbackRunsOnNestFastWorker(t *testing.T) {
	for i, tc := range []struct {
		name       string
		durability uint8
	}{
		{"memory_unreachable", 0},
		{"strict_rejected", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, live := newReloadFixture(t, 1961+int64(i))
			engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithRemoteEntityManager(entity.IRemoteEntityManager(f.mgr)), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}))
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
			if f.mgr.localExecutor.Load() == nil {
				t.Fatal("premise: NewEngine did not bind NestMgr.RunLocal to the Remote Manager")
			}
			tx := remoteTestTxID(0xD0 + byte(i))
			f.storage.unreachable.Store(tc.durability == 0)
			batch := f.prepareRejected(t, live, tx, tc.durability)
			switch tc.durability {
			case 0:
				if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
					t.Fatalf("commit err=%v, want indeterminate (finalizer resolves it)", err)
				}
			case 2:
				f.mgr.RejectRemoteTransaction(tx, "lease expired")
				if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteRejected) {
					t.Fatalf("strict commit err=%v, want rejected (finalizer settles it)", err)
				}
			}
			if err := batch.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.assertReloadedFromAuthority(t, live) // finalizer 已收尾：回滚、隔离、释放 gate、卸载
			if n := live.rollbacks.Load(); n != 1 {
				t.Fatalf("RollbackRemoteCommit ran %d time(s), want once", n)
			}
			if off := live.rollbacksOffFast.Load(); off != 0 {
				t.Fatalf("finalizer rejection rollback ran off the Nest fast pool (%d call(s)); with Nest bound it must go through NestMgr.RunLocal", off)
			}
		})
	}
}

func TestDeferredCloseIsRefusedOnceTheFinalizerHasStopped(t *testing.T) {
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.StartFinalizer()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}

	// One attempt could pick the Done branch by chance; many cannot all be
	// chance. Every one of them has to be refused.
	const attempts = 64
	accepted := 0
	for i := range attempts {
		item := deferredRemoteClose{txID: remoteTestTxID(byte(i))}
		if err := manager.deferRemoteClose(item); err == nil {
			accepted++
		}
	}
	if accepted != 0 {
		t.Fatalf("%d of %d deferred closes were handed to a queue nobody drains; their entries and finalize slots leak",
			accepted, attempts)
	}
	if queued := len(manager.remote.finalizeQueue); queued != 0 {
		t.Fatalf("%d items are sitting in the finalize queue after the stop completed", queued)
	}
}

// A handoff that starts before the stop must still be drained: refusing it
// would be just as wrong as accepting one too late, because the caller has
// already given up ownership of the entries.
func TestDeferredCloseAcceptedBeforeTheStopIsStillDrained(t *testing.T) {
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.StartFinalizer()

	if err := manager.deferRemoteClose(deferredRemoteClose{txID: remoteTestTxID(7)}); err != nil {
		t.Fatalf("a handoff before the stop must be accepted: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}
	if queued := len(manager.remote.finalizeQueue); queued != 0 {
		t.Fatalf("%d items were left in the queue after the stop drained it", queued)
	}
}
