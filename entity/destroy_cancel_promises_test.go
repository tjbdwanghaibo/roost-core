package entity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/lock"
)

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
