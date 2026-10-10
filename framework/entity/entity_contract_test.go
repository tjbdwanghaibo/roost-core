package entity

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
	"strings"
	"sync"
	"testing"
	"time"
)

func lockableElsewhere(e IThreadSafeEntity) bool {
	done := make(chan bool)
	go func() {
		ok := e.GetMutex().TryLock()
		if ok {
			e.GetMutex().Unlock()
		}
		done <- ok
	}()
	return <-done
}

func guardInstancePair(t *testing.T, shareMutex bool) (old, fresh *testEntity) {
	t.Helper()
	id := mustBuildTestEntityID(t, 6701, testEntityCategoryPlayer, EntityKindNone)
	oldMu := lock.NewReentrantMutex(id)
	freshMu := lock.NewReentrantMutex(id)
	if shareMutex {
		freshMu = oldMu
	}
	old = &testEntity{EntityBase: NewEntityBaseWithMutex(id, testEntityCategoryPlayer, false, oldMu, EntityKindNone)}
	fresh = &testEntity{EntityBase: NewEntityBaseWithMutex(id, testEntityCategoryPlayer, false, freshMu, EntityKindNone)}
	return old, fresh
}

func TestGuardLocksNewInstanceWithDifferentMutex(t *testing.T) {
	for _, via := range []string{"RequireEntity", "TryRequireEntity"} {
		t.Run(via, func(t *testing.T) {
			old, fresh := guardInstancePair(t, false)
			guard := newEntityGuard()
			if !guard.RequireEntity(old) {
				t.Fatal("setup: lock old")
			}
			ok := guard.RequireEntity(fresh)
			if via == "TryRequireEntity" {
				ok = guard.TryRequireEntity(fresh)
			}
			if !ok {
				t.Fatalf("%s(fresh) = false", via)
			}
			if lockableElsewhere(fresh) {
				t.Fatalf("%s reported the fresh instance as held, but its own mutex is free: the guard judged by id only", via)
			}
			if !guard.GuardedEntity(fresh) {
				t.Fatal("GuardedEntity(fresh) = false after locking it")
			}
			guard.ReleaseAll()
			if !lockableElsewhere(old) || !lockableElsewhere(fresh) {
				t.Fatalf("locks leaked after ReleaseAll: old free=%v fresh free=%v", lockableElsewhere(old), lockableElsewhere(fresh))
			}
		})
	}
}

// 同 ID、同一把锁的另一个实例（Destroy 之前重复构建）仍是“已持有”：不重复加锁，ReleaseAll 后锁可被他人取得。
func TestGuardTreatsInstanceSharingTheHeldMutexAsHeld(t *testing.T) {
	old, dup := guardInstancePair(t, true)
	guard := newEntityGuard()
	if !guard.RequireEntity(old) || !guard.RequireEntity(dup) || !guard.TryRequireEntity(dup) {
		t.Fatal("RequireEntity / TryRequireEntity on a held mutex must succeed")
	}
	if !guard.GuardedEntity(dup) {
		t.Fatal("GuardedEntity(dup sharing the held mutex) = false")
	}
	guard.ReleaseAll()
	if !lockableElsewhere(old) {
		t.Fatal("the shared mutex is still locked after ReleaseAll: it was locked twice")
	}
}

// 锁序判断同样按实例：同组的新实例不是“已持有”，Cast 前置检查必须拒绝（不能等待，也不能跳过加锁）。
func TestGuardLockOrderCheckUsesInstance(t *testing.T) {
	old, fresh := guardInstancePair(t, false)
	guard := newEntityGuard()
	if !guard.RequireEntity(old) {
		t.Fatal("setup: lock old")
	}
	defer guard.ReleaseAll()
	if guard.CheckContainAllLock([]IThreadSafeEntity{fresh}) {
		t.Fatal("CheckContainAllLock accepted a new same-group instance whose lock is not held")
	}
	if !guard.CheckContainAllLock([]IThreadSafeEntity{old}) {
		t.Fatal("CheckContainAllLock rejected the held instance")
	}
}

// ReleaseEntity 释放当前实例后，同 ID 仍由本 Guard 持有的旧锁回到记账里，ReleaseAll 时一并释放。
func TestGuardReleaseEntityKeepsSupersededLockAccounted(t *testing.T) {
	old, fresh := guardInstancePair(t, false)
	guard := newEntityGuard()
	if !guard.RequireEntity(old) || !guard.RequireEntity(fresh) {
		t.Fatal("setup")
	}
	guard.ReleaseEntity(fresh.GUId())
	if !lockableElsewhere(fresh) {
		t.Fatal("ReleaseEntity did not release the current instance")
	}
	if lockableElsewhere(old) || !guard.GuardedEntity(old) {
		t.Fatal("the superseded instance's lock must stay held and accounted until the guard releases")
	}
	guard.ReleaseAll()
	if !lockableElsewhere(old) {
		t.Fatal("superseded lock leaked after ReleaseAll")
	}
}

// RR-20260927-25：自定义 lock.Mutex 可以是不可比较的值类型（含 func 字段、值接收者）。同 ID 两个实例各持一份时，
// holding 直接用 == 比较两个接口值会 panic（comparing uncomparable type）；修后不可比较的锁按“不是同一把锁”处理，
// 新实例照常加锁、Guard 释放时两把锁都归还。
type funcFieldMutex struct {
	id     int64
	mu     *sync.Mutex
	onLock func()
}

func (m funcFieldMutex) TryLock() bool { return m.mu.TryLock() }
func (m funcFieldMutex) Lock()         { m.mu.Lock() }
func (m funcFieldMutex) LockWithTimeout(time.Duration) bool {
	m.mu.Lock()
	return true
}
func (m funcFieldMutex) Unlock()       { m.mu.Unlock() }
func (m funcFieldMutex) LockId() int64 { return m.id }

func TestGuardUncomparableCustomMutexValueDoesNotPanic(t *testing.T) {
	for _, via := range []string{"RequireEntity", "TryRequireEntity", "GuardedEntity"} {
		t.Run(via, func(t *testing.T) {
			id := mustBuildTestEntityID(t, 6801, testEntityCategoryPlayer, EntityKindNone)
			oldMu := funcFieldMutex{id: id, mu: &sync.Mutex{}, onLock: func() {}}
			freshMu := funcFieldMutex{id: id, mu: &sync.Mutex{}, onLock: func() {}}
			old := &testEntity{EntityBase: NewEntityBaseWithMutex(id, testEntityCategoryPlayer, false, oldMu, EntityKindNone)}
			fresh := &testEntity{EntityBase: NewEntityBaseWithMutex(id, testEntityCategoryPlayer, false, freshMu, EntityKindNone)}
			guard := newEntityGuard()
			if !guard.RequireEntity(old) {
				t.Fatal("setup: lock old")
			}
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s(fresh) panicked: %v", via, r)
				}
			}()
			switch via {
			case "RequireEntity":
				if !guard.RequireEntity(fresh) {
					t.Fatal("RequireEntity(fresh) = false")
				}
			case "TryRequireEntity":
				if !guard.TryRequireEntity(fresh) {
					t.Fatal("TryRequireEntity(fresh) = false")
				}
			case "GuardedEntity":
				if guard.GuardedEntity(fresh) {
					t.Fatal("GuardedEntity(fresh) = true before locking it: an uncomparable mutex was taken as the held one")
				}
				guard.ReleaseAll()
				return
			}
			if lockableElsewhere(fresh) || !guard.GuardedEntity(fresh) {
				t.Fatal("the fresh instance's own mutex is not held after locking it")
			}
			guard.ReleaseAll()
			if !lockableElsewhere(old) || !lockableElsewhere(fresh) {
				t.Fatalf("locks leaked after ReleaseAll: old free=%v fresh free=%v", lockableElsewhere(old), lockableElsewhere(fresh))
			}
		})
	}
}

// RR-20260927-30：RR-25 用 reflect.Type.Comparable 判定能否比较。含接口字段的结构体在类型层面可比较，但接口里装着 func 时
// == 仍在运行期 panic（comparing uncomparable type func()）。修后按值判定：装着不可比较动态值的锁按“不是同一把锁”处理
// （与 RR-25 相同，各用各的底层锁）；接口字段装的是可比较值时仍能认出同一把锁，不退化成各自加锁。
// hook 放在最前：结构体 == 逐字段比较、遇到不等即停，排在不同的 mu 之后就比较不到 hook。
type ifaceFieldMutex struct {
	hook any
	id   int64
	mu   *sync.Mutex
}

func (m ifaceFieldMutex) TryLock() bool { return m.mu.TryLock() }
func (m ifaceFieldMutex) Lock()         { m.mu.Lock() }
func (m ifaceFieldMutex) LockWithTimeout(time.Duration) bool {
	m.mu.Lock()
	return true
}
func (m ifaceFieldMutex) Unlock()       { m.mu.Unlock() }
func (m ifaceFieldMutex) LockId() int64 { return m.id }

func TestGuardCustomMutexWithFuncInInterfaceFieldDoesNotPanic(t *testing.T) {
	pair := func(t *testing.T, hook func() any, shared bool) (old, fresh *testEntity) {
		id := mustBuildTestEntityID(t, 6891, testEntityCategoryPlayer, EntityKindNone)
		oldLock := &sync.Mutex{}
		freshLock := &sync.Mutex{}
		if shared {
			freshLock = oldLock
		}
		old = &testEntity{EntityBase: NewEntityBaseWithMutex(id, testEntityCategoryPlayer, false, ifaceFieldMutex{id: id, mu: oldLock, hook: hook()}, EntityKindNone)}
		fresh = &testEntity{EntityBase: NewEntityBaseWithMutex(id, testEntityCategoryPlayer, false, ifaceFieldMutex{id: id, mu: freshLock, hook: hook()}, EntityKindNone)}
		return old, fresh
	}
	funcHook := func() any { return func() {} }
	noPanic := func(t *testing.T, via string) {
		t.Helper()
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", via, r)
		}
	}
	for _, via := range []string{"RequireEntity", "TryRequireEntity", "GuardedEntity", "ReleaseEntityInstance"} {
		t.Run(via, func(t *testing.T) {
			old, fresh := pair(t, funcHook, false)
			guard := newEntityGuard()
			if !guard.RequireEntity(old) {
				t.Fatal("setup: lock old")
			}
			defer noPanic(t, via)
			switch via {
			case "RequireEntity":
				if !guard.RequireEntity(fresh) {
					t.Fatal("RequireEntity(fresh) = false")
				}
			case "TryRequireEntity":
				if !guard.TryRequireEntity(fresh) {
					t.Fatal("TryRequireEntity(fresh) = false")
				}
			case "GuardedEntity":
				if guard.GuardedEntity(fresh) {
					t.Fatal("GuardedEntity(fresh) = true before locking it: a mutex holding a func was taken as the held one")
				}
				guard.ReleaseAll()
				return
			case "ReleaseEntityInstance":
				// old 被 fresh 取代、锁转入 superseded：释放 old 要比较 fresh 与 old 的锁（holding）和 superseded 里的锁。
				if !guard.RequireEntity(fresh) {
					t.Fatal("setup: lock fresh")
				}
				guard.ReleaseEntityInstance(old)
				if !lockableElsewhere(old) || lockableElsewhere(fresh) {
					t.Fatalf("ReleaseEntityInstance(old): old free=%v (want true) fresh free=%v (want false)", lockableElsewhere(old), lockableElsewhere(fresh))
				}
				guard.ReleaseAll()
				if !lockableElsewhere(fresh) {
					t.Fatal("fresh lock leaked after ReleaseAll")
				}
				return
			}
			if lockableElsewhere(fresh) || !guard.GuardedEntity(fresh) {
				t.Fatal("the fresh instance's own mutex is not held after locking it")
			}
			guard.ReleaseAll()
			if !lockableElsewhere(old) || !lockableElsewhere(fresh) {
				t.Fatalf("locks leaked after ReleaseAll: old free=%v fresh free=%v", lockableElsewhere(old), lockableElsewhere(fresh))
			}
		})
	}
	t.Run("comparable_value_in_interface_still_same_lock", func(t *testing.T) {
		// 接口字段装的是可比较值、两份包装共用同一把不可重入的底层锁：必须认出是同一把锁，否则第二次加锁会自锁。
		old, fresh := pair(t, func() any { return "observer" }, true)
		guard := newEntityGuard()
		if !guard.RequireEntity(old) {
			t.Fatal("setup: lock old")
		}
		defer noPanic(t, "RequireEntity(shared)")
		if !guard.GuardedEntity(fresh) || !guard.TryRequireEntity(fresh) {
			t.Fatal("a wrapper with a comparable interface field sharing the held lock must count as held")
		}
		guard.ReleaseAll()
		if !lockableElsewhere(old) {
			t.Fatal("shared lock leaked after ReleaseAll")
		}
	})
}

// RR-20260927-26：ReleaseEntityInstance 只释放传入实例自己的锁。旧实例被同 ID 新实例取代（锁在 superseded）时释放旧锁、
// 新实例仍被持有；传入当前实例或与它共用同一把锁的实例时同 ReleaseEntity；本 Guard 没为它登记锁时不动别人的锁。
func TestGuardReleaseEntityInstanceReleasesOnlyThatInstance(t *testing.T) {
	t.Run("superseded_old", func(t *testing.T) {
		old, fresh := guardInstancePair(t, false)
		guard := newEntityGuard()
		if !guard.RequireEntity(old) || !guard.RequireEntity(fresh) {
			t.Fatal("setup")
		}
		guard.ReleaseEntityInstance(old)
		if lockableElsewhere(fresh) || !guard.GuardedEntity(fresh) {
			t.Fatal("ReleaseEntityInstance(old) released the re-created instance's lock")
		}
		if !lockableElsewhere(old) || guard.GuardedEntity(old) {
			t.Fatal("ReleaseEntityInstance(old) did not release the superseded instance's own lock")
		}
		guard.ReleaseAll()
		if !lockableElsewhere(fresh) {
			t.Fatal("fresh lock leaked after ReleaseAll")
		}
	})
	t.Run("current_restores_superseded", func(t *testing.T) {
		old, fresh := guardInstancePair(t, false)
		guard := newEntityGuard()
		if !guard.RequireEntity(old) || !guard.RequireEntity(fresh) {
			t.Fatal("setup")
		}
		guard.ReleaseEntityInstance(fresh)
		if !lockableElsewhere(fresh) {
			t.Fatal("ReleaseEntityInstance(current) did not release it")
		}
		if lockableElsewhere(old) || !guard.GuardedEntity(old) {
			t.Fatal("the superseded instance's lock must stay held and accounted until the guard releases")
		}
		guard.ReleaseAll()
		if !lockableElsewhere(old) {
			t.Fatal("superseded lock leaked after ReleaseAll")
		}
	})
	t.Run("shared_mutex_duplicate", func(t *testing.T) {
		old, dup := guardInstancePair(t, true)
		guard := newEntityGuard()
		if !guard.RequireEntity(old) {
			t.Fatal("setup")
		}
		guard.ReleaseEntityInstance(dup)
		if !lockableElsewhere(old) || guard.GuardedCount() != 0 {
			t.Fatal("ReleaseEntityInstance(duplicate sharing the held mutex) must release that mutex once")
		}
		guard.ReleaseAll()
	})
	t.Run("not_held", func(t *testing.T) {
		old, fresh := guardInstancePair(t, false)
		guard := newEntityGuard()
		if !guard.RequireEntity(fresh) {
			t.Fatal("setup")
		}
		guard.ReleaseEntityInstance(old)
		if lockableElsewhere(fresh) || !guard.GuardedEntity(fresh) {
			t.Fatal("ReleaseEntityInstance of an instance this guard never locked released another instance's lock")
		}
		guard.ReleaseAll()
	})
}

func TestManagerRangeCallbackMayDestroyAndStopsAtFalse(t *testing.T) {
	t.Run("destroy inside Range", func(t *testing.T) {
		mgr := NewEntityManagerWithBuckets(4)
		for i := int64(1); i <= 32; i++ {
			mgr.Add(newMgrTestEntity(i, testEntityCategoryPlayer))
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			mgr.Range(func(e IThreadSafeEntity) bool {
				if err := mgr.Destroy(context.Background(), e, DestroyReasonMemoryUnload, false); err != nil {
					t.Errorf("destroy %d: %v", e.ID(), err)
				}
				return true
			})
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Range did not return: Destroy inside the callback waits for the bucket lock Range holds")
		}
		if n := mgr.Len(); n != 0 {
			t.Fatalf("Len = %d after destroying every visited entity, want 0", n)
		}
	})

	t.Run("false stops Range and RangeByCategory", func(t *testing.T) {
		mgr := NewEntityManagerWithBuckets(4)
		for i := int64(1); i <= 32; i++ {
			mgr.Add(newMgrTestEntity(i, testEntityCategoryPlayer))
		}
		calls := 0
		mgr.Range(func(IThreadSafeEntity) bool { calls++; return false })
		if calls != 1 {
			t.Fatalf("Range called fn %d times after it returned false, want 1", calls)
		}
		calls = 0
		mgr.RangeByCategory(testEntityCategoryPlayer, func(IThreadSafeEntity) bool { calls++; return false })
		if calls != 1 {
			t.Fatalf("RangeByCategory called fn %d times after it returned false, want 1", calls)
		}
	})
}

// NC-180 的快照语义在 EntityManager 上的后果：Range 先复制桶、锁外回调之后，
// 回调期间 / 之前被 Destroy 的实体照样交给 fn，而 Destroy 收尾的 doClear 已把
// ID / 分类 / 种类写回零值（use-after-destroy；kit/statslog 因此按分类 0 计数，
// RangeByCategory / CountByCategory 的分类读取与 doClear 数据竞争）。修复前
// 桶读锁挡住了 Del，回调期间实体不可能被清理。
//
// 承诺：Range / RangeByCategory 交给 fn 的实体在 fn 返回前不会被清理（ID 与
// 分类可读）；遍历到达前已被摘除的实体不交出。与 nest 分发同一协议：Touch 失败
// 即跳过，持有引用期间 Destroy 的清理推迟到引用归还。
func TestManagerRangeNeverHandsOutAClearedEntity(t *testing.T) {
	const n = 8
	t.Run("entities destroyed before they are reached", func(t *testing.T) {
		mgr := NewEntityManagerWithBuckets(1) // one bucket: one snapshot holds every entity
		for i := int64(1); i <= n; i++ {
			mgr.Add(newMgrTestEntity(i, testEntityCategoryPlayer))
		}
		var first int64
		visited := 0
		mgr.Range(func(e IThreadSafeEntity) bool {
			visited++
			if e.ID() == 0 || e.GetEntityCategory() != testEntityCategoryPlayer {
				t.Fatalf("Range handed out an entity already destroyed and cleared: id=%d category=%v", e.ID(), e.GetEntityCategory())
			}
			if first == 0 {
				first = e.ID()
				for i := int64(1); i <= n; i++ {
					if i != first {
						if err := mgr.Destroy(context.Background(), mgr.Get(i), DestroyReasonMemoryUnload, false); err != nil {
							t.Fatalf("destroy %d: %v", i, err)
						}
					}
				}
			}
			return true
		})
		if visited != 1 {
			t.Fatalf("Range visited %d entities, want 1: the other %d were destroyed before being reached", visited, n-1)
		}
	})

	t.Run("an entity destroyed concurrently while fn holds it", func(t *testing.T) {
		mgr := NewEntityManagerWithBuckets(1)
		for i := int64(1); i <= n; i++ {
			mgr.Add(newMgrTestEntity(i, testEntityCategoryPlayer))
		}
		done := false
		mgr.RangeByCategory(testEntityCategoryPlayer, func(e IThreadSafeEntity) bool {
			if done {
				return true
			}
			done = true
			id := e.ID()
			destroyed := make(chan error, 1)
			go func() { destroyed <- mgr.Destroy(context.Background(), e, DestroyReasonMemoryUnload, false) }()
			select {
			case err := <-destroyed:
				if err != nil {
					t.Fatalf("destroy %d: %v", id, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Destroy of an entity held by a Range callback did not return")
			}
			if mgr.Exists(id) {
				t.Fatalf("entity %d still managed after Destroy returned", id)
			}
			if e.ID() != id || e.GetEntityCategory() != testEntityCategoryPlayer || e.IsClear() {
				t.Fatalf("entity cleared while fn still holds it: id=%d (was %d) category=%v cleared=%v", e.ID(), id, e.GetEntityCategory(), e.IsClear())
			}
			return true
		})
		if got := mgr.Len(); got != n-1 {
			t.Fatalf("Len = %d, want %d", got, n-1)
		}
	})
}

type valueTypeEntity struct {
	*EntityBase
	onClear func()
}

func (v valueTypeEntity) Base() *EntityBase { return v.EntityBase }

const pointerContractKind EntityKind = 219 // 同包已用 kind 见各 *_test.go；219 未被占用

func TestValueTypeEntityIsRejectedAtRegistrationAndPublish(t *testing.T) {
	isolateEntityRegistry(t)
	id := mustBuildTestEntityID(t, 6801, testEntityCategoryPlayer, pointerContractKind)

	t.Run("manager add", func(t *testing.T) {
		manager := NewEntityManager()
		err := manager.TryAdd(valueTypeEntity{EntityBase: NewEntityBase(id, testEntityCategoryPlayer, false, pointerContractKind)})
		if err == nil {
			t.Fatalf("TryAdd accepted a value-type entity implementation (%T): the guard compares instances and would panic on this type", valueTypeEntity{})
		}
		// 修前红只断言“被拒绝并点名类型”；修后一并钉住哨兵（ErrEntityNotPointer 随修复新增）。
		if !errors.Is(err, ErrEntityNotPointer) || !strings.Contains(err.Error(), "valueTypeEntity") {
			t.Fatalf("rejection must carry ErrEntityNotPointer and name the type: %v", err)
		}
		if manager.Get(id) != nil {
			t.Fatal("the rejected entity was published")
		}
	})

	t.Run("registered builder", func(t *testing.T) {
		RegisterEntityBuilder(&EntityBuilderParam{
			Category: testEntityCategoryPlayer,
			Kind:     pointerContractKind,
			Builder: func(param *EntityCreateParam) (IThreadSafeEntity, error) {
				return valueTypeEntity{EntityBase: NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
			},
		})
		_, err := BuildEntity(&EntityCreateParam{IsCreate: true, Id: id, Category: testEntityCategoryPlayer, Kind: pointerContractKind})
		if err == nil {
			t.Fatalf("BuildEntity accepted a builder that returns a value-type entity (%T)", valueTypeEntity{})
		}
		if !errors.Is(err, ErrEntityNotPointer) || !strings.Contains(err.Error(), "valueTypeEntity") {
			t.Fatalf("rejection must carry ErrEntityNotPointer and name the type: %v", err)
		}
	})

	t.Run("pointer entity still accepted", func(t *testing.T) {
		manager := NewEntityManager()
		if err := manager.TryAdd(newTestEntity(id, testEntityCategoryPlayer)); err != nil {
			t.Fatalf("TryAdd(pointer entity) = %v", err)
		}
	})
}
