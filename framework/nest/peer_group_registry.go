package nest

import (
	"runtime"
	"sync"
	"weak"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// Group membership is an EntityManager property, not a NestMgr property. Two
// access wrappers around that same manager must agree on the physical lock.
// Neither this cache nor the cleanup closure retains the manager or its entities.
var managerGroupLocks sync.Map // weak.Pointer[entity.EntityManager] -> *entityLockGroupLockManager

func groupLocksForGetter(g entity.Getter) *entityLockGroupLockManager {
	if src, ok := g.(interface{ Manager() *entity.EntityManager }); ok {
		if m := src.Manager(); m != nil {
			key := weak.Make(m)
			locks, exists := managerGroupLocks.LoadOrStore(key, newEntityLockGroupLockManager())
			if !exists {
				runtime.AddCleanup(m, func(k weak.Pointer[entity.EntityManager]) { managerGroupLocks.Delete(k) }, key)
			}
			runtime.KeepAlive(m)
			return locks.(*entityLockGroupLockManager)
		}
	}
	// For custom group stores which don't expose their manager identity, a
	// shared coordinator is conservative: equal IDs serialize, never under-lock.
	if groupStoreOf(g) != nil {
		return fallbackGroupLocks
	}
	return newEntityLockGroupLockManager()
}

var fallbackGroupLocks = newEntityLockGroupLockManager()
