// EntityManager：增删查与按类别遍历、删除的持久准入与收尾、销毁可取消、遍历回调契约。

package entity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
	"github.com/tjbdwanghaibo/roost-core/internal/rangecontract"
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
	unregister, err := mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity, EntityDestroyReason) (DeleteAdmission, error) {
		admissionCalls++
		return DeleteAdmissionImmediate, admissionErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity, EntityDestroyReason) (DeleteAdmission, error) {
		return DeleteAdmissionImmediate, nil
	}); !errors.Is(err, ErrDeleteAdmitterExists) {
		t.Fatalf("duplicate admitter error = %v", err)
	}
	if err := mgr.Destroy(context.Background(), value, testDestroyCommon, true); !errors.Is(err, admissionErr) {
		t.Fatalf("destroy admission error = %v", err)
	}
	if mgr.Get(value.ID()) != value || value.IsRemoved() {
		t.Fatal("definitive admission failure removed the entity")
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
	_, err = mgr.RegisterDeleteAdmitter(func(_ context.Context, got IThreadSafeEntity, reason EntityDestroyReason) (DeleteAdmission, error) {
		if got != value {
			return DeleteAdmissionImmediate, errors.New("delete admission received another entity")
		}
		if reason != testDestroyCommon {
			return DeleteAdmissionImmediate, errors.New("delete admission received another reason")
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
			return DeleteAdmissionImmediate, errors.New("delete admission did not run under entity mutex")
		}
		admitted = true
		return DeleteAdmissionImmediate, nil
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

func TestEntityManagerDestroyDeferredAdmissionLeavesFinalizationToTransaction(t *testing.T) {
	mgr := NewEntityManager()
	value := newMgrTestEntity(1006, testEntityCategoryPlayer)
	mgr.Add(value)
	_, err := mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity, EntityDestroyReason) (DeleteAdmission, error) {
		return DeleteAdmissionDeferred, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Destroy(context.Background(), value, testDestroyCommon, true); err != nil {
		t.Fatal(err)
	}
	if mgr.Get(value.ID()) != value || value.IsRemoved() {
		t.Fatal("deferred admission finalized before its transaction committed")
	}
}

func TestEntityManagerDestroyIndeterminateAdmissionStopsServingEntity(t *testing.T) {
	mgr := NewEntityManager()
	value := newMgrTestEntity(1007, testEntityCategoryPlayer)
	mgr.Add(value)
	wantErr := errors.New("wal admission outcome unknown")
	_, err := mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity, EntityDestroyReason) (DeleteAdmission, error) {
		return DeleteAdmissionIndeterminate, wantErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Destroy(context.Background(), value, testDestroyCommon, true); !errors.Is(err, wantErr) {
		t.Fatalf("Destroy error=%v, want %v", err, wantErr)
	}
	if mgr.Get(1007) != nil {
		t.Fatal("indeterminate admission left possibly deleted state reachable")
	}
}

func TestEntityManagerDestroyIndeterminateAdmissionCannotReportSuccess(t *testing.T) {
	mgr := NewEntityManager()
	value := newMgrTestEntity(1008, testEntityCategoryPlayer)
	mgr.Add(value)
	_, err := mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity, EntityDestroyReason) (DeleteAdmission, error) {
		return DeleteAdmissionIndeterminate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Destroy(context.Background(), value, testDestroyCommon, true); !errors.Is(err, ErrDeleteIndeterminate) {
		t.Fatalf("Destroy error=%v, want ErrDeleteIndeterminate", err)
	}
	if mgr.Get(1008) != nil {
		t.Fatal("indeterminate admission without adapter error left entity reachable")
	}
}

func TestEntityManagerDeleteAdmitterCanUnregisterOutsideRegistryLock(t *testing.T) {
	mgr := NewEntityManager()
	value := newMgrTestEntity(1004, testEntityCategoryPlayer)
	mgr.Add(value)
	wantErr := errors.New("admission stopped")
	var unregister func()
	var err error
	unregister, err = mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity, EntityDestroyReason) (DeleteAdmission, error) {
		unregister()
		return DeleteAdmissionImmediate, wantErr
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

// destroyLockProbe 在初次 ctx 检查后报告取锁入口，避免把“尚未开始就取消”
// 误当作“锁已被别的业务持有时仍可取消”。只在 Add 完成后启用屏障。
type destroyLockProbe struct {
	*mgrTestEntity
	mutexReached chan struct{}
	once         sync.Once
}

func (e *destroyLockProbe) GetMutex() lock.Mutex {
	if e.mutexReached != nil {
		e.once.Do(func() { close(e.mutexReached) })
	}
	return e.EntityBase.GetMutex()
}

func TestEntityManagerDestroyCancelsWhileEntityLockIsHeld(t *testing.T) {
	for _, durable := range []bool{false, true} {
		name := "memory"
		if durable {
			name = "durable"
		}
		t.Run(name, func(t *testing.T) {
			mgr := NewEntityManager()
			value := &destroyLockProbe{mgrTestEntity: newMgrTestEntity(1021, testEntityCategoryPlayer)}
			mgr.Add(value)
			value.mutexReached = make(chan struct{})
			var admissions atomic.Int32
			_, err := mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity, EntityDestroyReason) (DeleteAdmission, error) {
				admissions.Add(1)
				return DeleteAdmissionImmediate, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			mu := value.EntityBase.GetMutex()
			mu.Lock()
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			finished := make(chan struct{})
			// 旧实现失败时也由原持锁goroutine解锁并收走等待者，不留下挂起测试。
			t.Cleanup(func() {
				cancel()
				mu.Unlock()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Error("Destroy did not finish after test released the entity lock")
				}
			})
			go func() {
				defer close(finished)
				result <- mgr.Destroy(ctx, value, testDestroyCommon, durable)
			}()
			select {
			case <-value.mutexReached:
			case <-time.After(time.Second):
				t.Fatal("Destroy did not reach the entity mutex")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Destroy error = %v, want cancellation before lock release", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Destroy ignored cancellation while the entity mutex remained held")
			}
			if admissions.Load() != 0 || mgr.Get(value.ID()) != value || value.IsRemoved() || value.IsClear() {
				t.Fatal("canceled lock wait admitted a deletion or removed the live entity")
			}
		})
	}
}

func TestEntityManagerDestroyCancellationAfterAdmissionStillFinalizes(t *testing.T) {
	for _, admission := range []DeleteAdmission{DeleteAdmissionImmediate, DeleteAdmissionIndeterminate} {
		mgr := NewEntityManager()
		value := newMgrTestEntity(1022, testEntityCategoryPlayer)
		mgr.Add(value)
		ctx, cancel := context.WithCancel(context.Background())
		_, err := mgr.RegisterDeleteAdmitter(func(context.Context, IThreadSafeEntity, EntityDestroyReason) (DeleteAdmission, error) {
			cancel()
			return admission, nil
		})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		err = mgr.Destroy(ctx, value, testDestroyCommon, true)
		cancel()
		if admission == DeleteAdmissionImmediate && err != nil {
			t.Fatalf("accepted delete returned %v", err)
		}
		if admission == DeleteAdmissionIndeterminate && !errors.Is(err, ErrDeleteIndeterminate) {
			t.Fatalf("indeterminate delete returned %v", err)
		}
		if mgr.Get(1022) != nil || !value.IsRemoved() || !value.IsClear() {
			t.Fatalf("admission %v: caller cancellation abandoned removal", admission)
		}
	}
}

func TestEntityManagerDestroyPreservesReentrantLockOwnership(t *testing.T) {
	mgr := NewEntityManager()
	value := newMgrTestEntity(1023, testEntityCategoryPlayer)
	mgr.Add(value)
	mu := value.GetMutex()
	mu.Lock()
	defer mu.Unlock()
	if err := mgr.Destroy(context.Background(), value, testDestroyCommon, false); err != nil {
		t.Fatal(err)
	}
	otherAcquired := make(chan bool, 1)
	go func() {
		acquired := mu.TryLock()
		if acquired {
			mu.Unlock()
		}
		otherAcquired <- acquired
	}()
	select {
	case acquired := <-otherAcquired:
		if acquired {
			t.Fatal("Destroy released its caller's outer lock ownership")
		}
	case <-time.After(time.Second):
		t.Fatal("outer lock ownership probe did not finish")
	}
}

// C7（维护者 2026-10-05）：遍历回调契约——回调里可以读写同一个容器，返回 false 立即停止。
// 实体管理器的“读写”是 Get / Add / Destroy；键是实体 ID，值由 ID 决定（KeysOnly）。交出的实体
// ID 为 0（已被 doClear 清零）会被辅助判为“交出不存在的键”——这正是 NC-180 复审补修的承诺。
// 没有 Clear，清空用例跳过。

const contractGroupID = 7001

func entityManagerOps(grouped bool, rangeOf func(mgr *EntityManager, access *ManagerAccess, fn func(IThreadSafeEntity) bool)) func(testing.TB) rangecontract.Ops {
	return func(t testing.TB) rangecontract.Ops {
		mgr := NewEntityManagerWithBuckets(4)
		access := NewManagerAccess(mgr)
		return rangecontract.Ops{
			Set: func(id, _ int64) {
				if mgr.Exists(id) {
					return
				}
				e := newMgrTestEntity(id, testEntityCategoryPlayer)
				if grouped {
					e.Base().SetGroupLockIDForTest(contractGroupID)
				}
				mgr.Add(e)
			},
			Get: func(id int64) (int64, bool) {
				if e := mgr.Get(id); e != nil {
					return e.ID(), true
				}
				return 0, false
			},
			Delete: func(id int64) {
				if e := mgr.Get(id); e != nil {
					if err := mgr.Destroy(context.Background(), e, DestroyReasonMemoryUnload, false); err != nil {
						t.Errorf("destroy %d: %v", id, err)
					}
				}
			},
			Range: func(f func(int64, int64) bool) {
				rangeOf(mgr, access, func(e IThreadSafeEntity) bool { return f(e.ID(), e.ID()) })
			},
		}
	}
}

func TestEntityRangeContract(t *testing.T) {
	subjects := map[string]rangecontract.Subject{
		"EntityManager.Range": {KeysOnly: true, New: entityManagerOps(false, func(mgr *EntityManager, _ *ManagerAccess, fn func(IThreadSafeEntity) bool) {
			mgr.Range(fn)
		})},
		"EntityManager.RangeByCategory": {KeysOnly: true, New: entityManagerOps(false, func(mgr *EntityManager, _ *ManagerAccess, fn func(IThreadSafeEntity) bool) {
			mgr.RangeByCategory(testEntityCategoryPlayer, fn)
		})},
		"EntityManager.RangeGroupEntities": {KeysOnly: true, New: entityManagerOps(true, func(mgr *EntityManager, _ *ManagerAccess, fn func(IThreadSafeEntity) bool) {
			mgr.RangeGroupEntities(contractGroupID, fn)
		})},
		"ManagerAccess.Range": {KeysOnly: true, New: entityManagerOps(false, func(_ *EntityManager, access *ManagerAccess, fn func(IThreadSafeEntity) bool) {
			access.Range(fn)
		})},
	}
	for name, subject := range subjects {
		t.Run(name, func(t *testing.T) { rangecontract.Check(t, subject) })
	}
}
