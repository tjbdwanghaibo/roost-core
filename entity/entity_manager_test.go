package entity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// mgrTestEntity implements IThreadSafeEntity for testing.
type mgrTestEntity struct {
	*EntityBase
	ComponentManager
	DaoManager
}

type blockingDestroyEntity struct {
	*mgrTestEntity
	entered chan struct{}
	release chan struct{}
}

type panickingDestroyEntity struct{ *mgrTestEntity }

func (*panickingDestroyEntity) OnDestroy(EntityDestroyReason) { panic("destroy failed") }

func (e *blockingDestroyEntity) OnDestroy(EntityDestroyReason) {
	close(e.entered)
	<-e.release
}

func (e *mgrTestEntity) Base() *EntityBase { return e.EntityBase }

func newMgrTestEntity(id int64, typo EntityCategory) *mgrTestEntity {
	e := &mgrTestEntity{}
	e.EntityBase = NewEntityBase(id, typo, false)
	e.ComponentManager = NewComponentManager()
	e.DaoManager = NewDaoManager()
	return e
}

func TestEntityManager_AddGet(t *testing.T) {
	mgr := NewEntityManager()

	e1 := newMgrTestEntity(1001, testEntityCategoryPlayer)
	e2 := newMgrTestEntity(1002, testEntityCategoryAlliance)

	mgr.Add(e1)
	mgr.Add(e2)

	if mgr.Len() != 2 {
		t.Fatalf("expected 2 entities, got %d", mgr.Len())
	}

	got := mgr.Get(1001)
	if got != e1 {
		t.Fatal("Get(1001) returned wrong entity")
	}

	got = mgr.Get(9999)
	if got != nil {
		t.Fatal("Get(9999) should return nil")
	}
}

func TestEntityManager_GetWithCategory(t *testing.T) {
	mgr := NewEntityManager()
	e := newMgrTestEntity(1001, testEntityCategoryPlayer)
	mgr.Add(e)

	got := mgr.GetWithCategory(1001, testEntityCategoryPlayer)
	if got != e {
		t.Fatal("GetWithCategory should return entity with matching type")
	}

	got = mgr.GetWithCategory(1001, testEntityCategoryAlliance)
	if got != nil {
		t.Fatal("GetWithCategory should return nil on type mismatch")
	}
}

func TestEntityManager_DuplicatePanics(t *testing.T) {
	mgr := NewEntityManager()
	e1 := newMgrTestEntity(1001, testEntityCategoryPlayer)
	mgr.Add(e1)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate add")
		}
	}()

	e2 := newMgrTestEntity(1001, testEntityCategoryPlayer)
	mgr.Add(e2)
}

func TestEntityManager_Remove(t *testing.T) {
	mgr := NewEntityManager()
	e := newMgrTestEntity(1001, testEntityCategoryPlayer)
	mgr.Add(e)

	if err := mgr.Destroy(context.Background(), e, testDestroyCommon, false); err != nil {
		t.Fatal(err)
	}

	if mgr.Get(1001) != nil {
		t.Fatal("entity should be removed")
	}
	if !e.IsRemoved() {
		t.Fatal("entity should be marked removed")
	}
	if mgr.Len() != 0 {
		t.Fatalf("expected 0, got %d", mgr.Len())
	}
}

func TestEntityManagerDestroyRequiresDurableAdmissionBeforeRemoval(t *testing.T) {
	mgr := NewEntityManager()
	value := newMgrTestEntity(1002, testEntityCategoryPlayer)
	mgr.Add(value)

	if err := mgr.Destroy(context.Background(), value, testDestroyCommon, true); !errors.Is(err, ErrDeleteAdmitterNeeded) {
		t.Fatalf("destroy without admitter error = %v", err)
	}
	if mgr.Get(value.ID()) != value || value.IsRemoved() {
		t.Fatal("failed admission removed the entity")
	}

	admissionErr := errors.New("durable wal unavailable")
	admissionCalls := 0
	unregister, err := mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity) error {
		admissionCalls++
		return admissionErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity) error { return nil }); !errors.Is(err, ErrDeleteAdmitterExists) {
		t.Fatalf("duplicate admitter error = %v", err)
	}
	if err := mgr.Destroy(context.Background(), value, testDestroyCommon, true); !errors.Is(err, admissionErr) {
		t.Fatalf("destroy admission error = %v", err)
	}
	if mgr.Get(value.ID()) != value || value.IsRemoved() {
		t.Fatal("indeterminate admission removed the entity")
	}
	outsider := newMgrTestEntity(1003, testEntityCategoryPlayer)
	if err := mgr.Destroy(context.Background(), outsider, testDestroyCommon, true); !errors.Is(err, ErrEntityNotManaged) {
		t.Fatalf("unmanaged destroy error = %v", err)
	}
	if admissionCalls != 1 {
		t.Fatalf("delete admission ran for an unmanaged entity: calls=%d", admissionCalls)
	}

	unregister()
	admitted := false
	_, err = mgr.RegisterDeleteAdmitter(func(_ context.Context, got IThreadSafeEntity) error {
		if got != value {
			return errors.New("delete admission received another entity")
		}
		locked := make(chan bool, 1)
		go func() {
			acquired := got.GetMutex().TryLock()
			if acquired {
				got.GetMutex().Unlock()
			}
			locked <- acquired
		}()
		if <-locked {
			return errors.New("delete admission did not run under entity mutex")
		}
		admitted = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Destroy(context.Background(), value, testDestroyCommon, true); err != nil {
		t.Fatal(err)
	}
	if !admitted || mgr.Get(value.ID()) != nil || !value.IsRemoved() {
		t.Fatalf("durably admitted entity was not removed: admitted=%t removed=%t", admitted, value.IsRemoved())
	}
}

func TestEntityManagerDeleteAdmitterCanUnregisterOutsideRegistryLock(t *testing.T) {
	mgr := NewEntityManager()
	value := newMgrTestEntity(1004, testEntityCategoryPlayer)
	mgr.Add(value)
	wantErr := errors.New("admission stopped")
	var unregister func()
	var err error
	unregister, err = mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity) error {
		unregister()
		return wantErr
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Destroy(context.Background(), value, testDestroyCommon, true) }()
	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("Destroy error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("delete admitter deadlocked while unregistering")
	}
	if mgr.Get(value.ID()) != value || value.IsRemoved() {
		t.Fatal("failed reentrant admission removed entity")
	}
}

func TestEntityManagerDestroyPanicStillCompletesFrameworkCleanup(t *testing.T) {
	mgr := NewEntityManager()
	value := &panickingDestroyEntity{mgrTestEntity: newMgrTestEntity(1005, testEntityCategoryPlayer)}
	mgr.Add(value)
	id := value.ID()
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("expected lifecycle panic")
			}
		}()
		_ = mgr.Destroy(context.Background(), value, testDestroyCommon, false)
	}()
	if mgr.Get(id) != nil || !value.IsClear() {
		t.Fatalf("panic cleanup left entity reachable: managed=%v clear=%v", mgr.Get(id) != nil, value.IsClear())
	}
	replacement := newMgrTestEntity(id, testEntityCategoryPlayer)
	if err := mgr.TryAdd(replacement); err != nil {
		t.Fatalf("panic cleanup retained same-ID tombstone: %v", err)
	}
}

func TestEntityManager_RemoveFencesSameIDUntilLifecycleCompletes(t *testing.T) {
	mgr := NewEntityManager()
	value := &blockingDestroyEntity{
		mgrTestEntity: newMgrTestEntity(1002, testEntityCategoryPlayer),
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	mgr.Add(value)
	done := make(chan error, 1)
	go func() {
		done <- mgr.Destroy(context.Background(), value, testDestroyCommon, false)
	}()

	select {
	case <-value.entered:
	case <-time.After(time.Second):
		t.Fatal("remove did not enter lifecycle callback")
	}
	if mgr.Get(value.ID()) != nil {
		t.Fatal("removing entity must no longer be discoverable")
	}
	replacement := newMgrTestEntity(value.ID(), testEntityCategoryPlayer)
	if err := mgr.TryAdd(replacement); !errors.Is(err, ErrEntityRemoved) {
		t.Fatalf("TryAdd during removal err=%v, want ErrEntityRemoved", err)
	}

	close(value.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := mgr.TryAdd(replacement); err != nil {
		t.Fatalf("TryAdd after lifecycle completion: %v", err)
	}
}

func TestEntityManager_GetMany(t *testing.T) {
	mgr := NewEntityManager()
	e1 := newMgrTestEntity(1, testEntityCategoryPlayer)
	e2 := newMgrTestEntity(2, testEntityCategoryPlayer)
	mgr.Add(e1)
	mgr.Add(e2)

	got := mgr.GetMany([]int64{1, 2, 999})
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
}

func TestEntityManager_Exists(t *testing.T) {
	mgr := NewEntityManager()
	e := newMgrTestEntity(1001, testEntityCategoryPlayer)
	mgr.Add(e)

	if !mgr.Exists(1001) {
		t.Fatal("should exist")
	}
	if mgr.Exists(9999) {
		t.Fatal("should not exist")
	}
}

func TestEntityManager_RangeByCategory(t *testing.T) {
	mgr := NewEntityManager()
	mgr.Add(newMgrTestEntity(1, testEntityCategoryPlayer))
	mgr.Add(newMgrTestEntity(2, testEntityCategoryPlayer))
	mgr.Add(newMgrTestEntity(3, testEntityCategoryAlliance))

	count := 0
	mgr.RangeByCategory(testEntityCategoryPlayer, func(_ IThreadSafeEntity) bool {
		count++
		return true
	})
	if count != 2 {
		t.Fatalf("expected 2 players, got %d", count)
	}
}

func TestEntityManager_CountByCategory(t *testing.T) {
	mgr := NewEntityManager()
	mgr.Add(newMgrTestEntity(1, testEntityCategoryPlayer))
	mgr.Add(newMgrTestEntity(2, testEntityCategoryPlayer))
	mgr.Add(newMgrTestEntity(3, testEntityCategoryAlliance))

	if mgr.CountByCategory(testEntityCategoryPlayer) != 2 {
		t.Fatal("expected 2 players")
	}
	if mgr.CountByCategory(testEntityCategoryAlliance) != 1 {
		t.Fatal("expected 1 alliance")
	}
}

func TestEntityManager_ConcurrentAccess(t *testing.T) {
	mgr := NewEntityManager()

	var wg sync.WaitGroup
	for i := int64(0); i < 100; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			e := newMgrTestEntity(id, testEntityCategoryPlayer)
			mgr.Add(e)
		}(i)
	}
	wg.Wait()

	if mgr.Len() != 100 {
		t.Fatalf("expected 100, got %d", mgr.Len())
	}

	// Concurrent reads
	wg = sync.WaitGroup{}
	for i := int64(0); i < 100; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			got := mgr.Get(id)
			if got == nil {
				t.Errorf("missing entity %d", id)
			}
		}(i)
	}
	wg.Wait()
}
