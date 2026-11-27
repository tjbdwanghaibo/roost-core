package nest

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// Physical contention probe for framework tests. The business facade's TryLock
// must always refuse, even if the lock is free, so it is not a contention probe.
func physicalTestLock(e entity.IThreadSafeEntity) lock.Mutex {
	return entity.SystemMutex(entitypermit.System(), e)
}

func TestPeerGroupCoordinatorOwnedByEntityManager(t *testing.T) {
	same := entity.NewEntityManager()
	a := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(same)))
	b := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(same)))
	c := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(entity.NewEntityManager())))
	m := a.groupLockManager()
	entry, ok := m.acquire(91)
	if !ok {
		t.Fatal("first lock unavailable")
	}
	defer m.release(entry)
	other := make(chan [2]bool, 1)
	go func() {
		locks := b.groupLockManager()
		e, sharedFree := locks.acquire(91)
		if sharedFree {
			locks.release(e)
		}
		locks = c.groupLockManager()
		e, separateFree := locks.acquire(91)
		if separateFree {
			locks.release(e)
		}
		other <- [2]bool{sharedFree, separateFree}
	}()
	if got := <-other; got != [2]bool{false, true} {
		t.Fatalf("shared and isolated group availability: %v", got)
	}
}
