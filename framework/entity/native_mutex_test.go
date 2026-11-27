package entity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// Native contention probes must exercise the framework mutex. A false TryLock
// on the application facade proves nothing about mutual exclusion.
func testSystemMutex(e IThreadSafeEntity) lock.Mutex { return SystemMutex(entitypermit.System(), e) }

func requireScopePanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != ErrEntityLockScope {
			t.Fatalf("panic = %v; want ErrEntityLockScope", r)
		}
	}()
	fn()
}

func assertNativeLockDenied(t *testing.T, mu lock.Mutex, id int64) {
	t.Helper()
	if mu == nil || mu.LockId() != id {
		t.Fatalf("invalid public lock identity, want %d", id)
	}
	if mu.TryLock() || mu.LockWithTimeout(time.Second) {
		t.Fatal("public facade granted a native lock")
	}
	if err := lock.LockContext(context.Background(), mu); !errors.Is(err, ErrEntityLockScope) {
		t.Fatalf("LockContext = %v; want ErrEntityLockScope", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mu.(interface{ LockContext(context.Context) error }).LockContext(ctx); !errors.Is(err, ErrEntityLockScope) {
		t.Fatalf("direct LockContext with canceled ctx = %v; want ErrEntityLockScope", err)
	}
	requireScopePanic(t, mu.Lock)
	requireScopePanic(t, mu.Unlock)
}

func TestNativeMutexNeverAvailableThroughApplicationEntity(t *testing.T) {
	const id = 2099
	ordinary := newMgrTestEntity(id, testEntityCategoryPlayer)
	remote := &testRemoteEntity{RemoteEntityBase: *NewRemoteEntityBase(id, testEntityCategoryPlayer, false, testRemotePlayerKind)}
	for _, e := range []IThreadSafeEntity{ordinary, remote} {
		mu := e.GetMutex()
		assertNativeLockDenied(t, mu, id)
		requireScopePanic(t, func() { SystemMutex(entitypermit.Token{}, e) })
		mgr := NewEntityManager()
		mgr.Add(e)
		if err := mgr.WithLocked(entitypermit.System(), context.Background(), e, func() {
			assertNativeLockDenied(t, mu, id)
			assertNativeLockDenied(t, e.GetMutex(), id)
			if lockableElsewhere(e) {
				t.Fatal("callback released the manager's native ticket")
			}
		}); err != nil || !lockableElsewhere(e) {
			t.Fatalf("WithLocked failed or kept its native ticket: %v", err)
		}
	}
}

func TestConstructorParamNeverExposesManagerMutex(t *testing.T) {
	isolateEntityRegistry(t)
	const kind = pointerContractKind
	mgr := NewEntityManager()
	var saved lock.Mutex
	RegisterEntityBuilder(&EntityBuilderParam{Kind: kind, Category: testEntityCategoryPlayer, Builder: func(p *EntityCreateParam) (IThreadSafeEntity, error) {
		saved = p.Mutex
		assertNativeLockDenied(t, saved, p.Id)
		return &testEntity{EntityBase: NewEntityBaseWithMutex(p.Id, p.Category, false, p.Mutex, p.Kind)}, nil
	}})
	param := &EntityCreateParam{Kind: kind, UniqueID: 9103}
	e, err := mgr.Create(param)
	if err != nil {
		t.Fatal(err)
	}
	id := e.ID()
	assertNativeLockDenied(t, saved, id)
	if param.Mutex != nil {
		t.Fatal("manager wrote its lock to caller-owned params")
	}
	requireScopePanic(t, func() { NewEntityBaseWithMutex(id, e.GetEntityCategory(), false, saved, kind) })
	old := testSystemMutex(e)
	if old != mgr.locks.GetLock(id) {
		t.Fatal("constructor changed the manager's native mutex")
	}
	if err := mgr.Destroy(context.Background(), e, DestroyReasonCommon, false); err != nil {
		t.Fatal(err)
	}
	assertNativeLockDenied(t, e.GetMutex(), id) // retired instance: identity only
	fresh, err := mgr.Create(param)
	if err != nil || fresh.ID() != id || testSystemMutex(fresh) == old || testSystemMutex(fresh) != mgr.locks.GetLock(id) {
		t.Fatalf("reusing caller parameters did not obtain the current manager lock: %v", err)
	}
	custom := lock.NewDefaultMutex(id)
	separate, err := BuildEntity(&EntityCreateParam{Kind: kind, Id: id, Mutex: custom})
	if err != nil || testSystemMutex(separate) != custom {
		t.Fatalf("explicit custom native lock was lost: %v", err)
	}
	assertNativeLockDenied(t, saved, id)
	if _, err := BuildEntity(&EntityCreateParam{Kind: kind, Id: id, Mutex: saved}); !errors.Is(err, ErrEntityLockScope) {
		t.Fatalf("replayed saved builder argument = %v; want ErrEntityLockScope", err)
	}
}

type replacedMutexEntity struct {
	*testEntity
	exposed lock.Mutex
}

func (e *replacedMutexEntity) GetMutex() lock.Mutex { return e.exposed }

// Even a conditional getter returning a perfectly safe value during admission
// cannot implement the production entity contract if it could later return raw.
func TestApplicationRawGetterCannotImplementEntity(t *testing.T) {
	if _, ok := any((*replacedMutexEntity)(nil)).(IThreadSafeEntity); ok {
		t.Fatal("a method returning arbitrary mutexes still implements the entity contract")
	}
}

type rejectNativeReentry struct{ lock.Mutex }

func (m *rejectNativeReentry) Lock() {
	if !m.TryLock() {
		panic("redundant acquire of non-reentrant native lock")
	}
}

func TestSyncFrameSharingThePhysicalMutexDoesNotReacquire(t *testing.T) {
	const id = 9105
	mu := &rejectNativeReentry{lock.NewDefaultMutex(id)}
	a := &testEntity{EntityBase: NewEntityBaseWithMutex(id, testEntityCategoryPlayer, false, mu)}
	b := &testEntity{EntityBase: NewEntityBaseWithMutex(id, testEntityCategoryPlayer, false, mu)}
	b.EnableSync(EntitySyncCreateParam{Enabled: true})
	scope, done := NewGuardScope(entitypermit.System(), "sync alias")
	defer done()
	if !scope.Guard().RequireEntity(entitypermit.System(), a) {
		t.Fatal("guard failed to acquire original instance")
	}
	called := false
	if err := b.Sync().withEntityLock(func() error { called = true; return nil }); err != nil || !called || lockableElsewhere(b) {
		t.Fatalf("sync alias lost physical lock ownership: %v", err)
	}
}

// In-package regression: even a substituted sealed view from the wrong base is
// rejected at both entry points rather than publishing an incorrect lock identity.
type substitutedViewEntity struct {
	*testEntity
	exposed publicEntityMutex
}

func (e *substitutedViewEntity) GetMutex() publicEntityMutex { return e.exposed }

func TestOverriddenNativeGetterRejectedAtBothEntryPoints(t *testing.T) {
	isolateEntityRegistry(t)
	const kind = pointerContractKind
	id := mustBuildTestEntityID(t, 9104, testEntityCategoryPlayer, kind)
	mu := lock.NewDefaultMutex(id)
	e := &substitutedViewEntity{&testEntity{EntityBase: NewEntityBaseWithMutex(id, testEntityCategoryPlayer, false, mu, kind)}, NewEntityBase(id, testEntityCategoryPlayer, false).GetMutex()}
	mgr := NewEntityManager()
	if err := mgr.TryAdd(e); !errors.Is(err, ErrEntityLockScope) || mgr.Get(id) != nil {
		t.Fatalf("TryAdd published a substitute mutex: %v", err)
	}
	RegisterEntityBuilder(&EntityBuilderParam{Kind: kind, Category: testEntityCategoryPlayer, Builder: func(*EntityCreateParam) (IThreadSafeEntity, error) { return e, nil }})
	if got, err := BuildEntity(&EntityCreateParam{Kind: kind, Id: id}); got != nil || !errors.Is(err, ErrEntityLockScope) {
		t.Fatalf("BuildEntity = %T, %v; want rejected substitute mutex", got, err)
	}
}
