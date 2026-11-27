package entity

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// ErrEntityLockScope means a caller tried to manipulate a native entity lock
// through the application facade. Even the holder of a Nest frame cannot do so.
var ErrEntityLockScope = errors.New("entity: native mutex is framework-owned")

// The named, sealed result is deliberate: a method returning arbitrary lock.Mutex
// cannot satisfy the entity interface. A one-time getter comparison at publication
// alone would allow a conditional override to switch to a raw lock afterwards.
// Ordinary callers can still use the result as a lock.Mutex or call LockContext.
type publicEntityMutex interface {
	lock.Mutex
	LockContext(context.Context) error
	entityMutexViewOnly()
}

// nativeMutexView exposes immutable identity, never the lock or an unlock ticket.
// Embedded bases may be copied by value during construction; the view contains
// no pointer back to the pre-copy base and is independent of goroutine state.
type nativeMutexView struct{ id int64 }

func (*nativeMutexView) entityMutexViewOnly() {}

func (m *nativeMutexView) LockId() int64                    { return m.id }
func (*nativeMutexView) Lock()                              { panic(ErrEntityLockScope) }
func (*nativeMutexView) TryLock() bool                      { return false }
func (*nativeMutexView) LockWithTimeout(time.Duration) bool { return false }
func (*nativeMutexView) LockContext(context.Context) error  { return ErrEntityLockScope }
func (*nativeMutexView) Unlock()                            { panic(ErrEntityLockScope) }

// constructorMutex is the opaque handoff from BuildEntity to the generated
// NewEntityBaseWithMutex constructor. No builder, hook or saved param receives
// the manager's actual lock. The public GetMutex view is NOT such a handoff.
type constructorMutex struct {
	nativeMutexView
	grant *atomic.Pointer[lock.Mutex]
}

func newConstructorMutex(mu lock.Mutex) *constructorMutex {
	grant := new(atomic.Pointer[lock.Mutex])
	grant.Store(&mu)
	return &constructorMutex{nativeMutexView{id: mu.LockId()}, grant}
}

func (m *constructorMutex) take(id int64) lock.Mutex {
	if id == m.id {
		if mu := m.grant.Swap(nil); mu != nil {
			return *mu
		}
	}
	panic(ErrEntityLockScope)
}

// SystemMutex obtains the framework locking protocol, including the stable
// entrance for companion participants. Never pass its result to application
// callbacks. A zero token does not confer access, even for an unmanaged entity.
func SystemMutex(token entitypermit.Token, e IThreadSafeEntity) lock.Mutex {
	if !token.Valid() {
		panic(ErrEntityLockScope)
	}
	return systemMutex(e)
}

func systemMutex(e IThreadSafeEntity) lock.Mutex {
	if e == nil {
		return nil
	}
	if base := e.Base(); base != nil {
		return base.mu
	}
	return nil
}

// Reject a substituted public mutex at both publication boundaries. Compare
// only the known pointer view, never arbitrary (possibly incomparable) mutexes.
func requireNativeMutexView(e IThreadSafeEntity) error {
	base := e.Base()
	if base == nil || base.mu == nil {
		return ErrEntityNil
	}
	if view, ok := e.GetMutex().(*nativeMutexView); !ok || view != &base.mutexView {
		return fmt.Errorf("%w: %T must expose its EntityBase mutex facade", ErrEntityLockScope, e)
	}
	return nil
}
