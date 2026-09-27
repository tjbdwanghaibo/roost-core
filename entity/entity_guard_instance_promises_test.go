package entity

import (
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/lock"
)

// RR-20260926-67：Guard 以实例（实际持有的锁）而非仅 ID 判断“已持有”。handler 内 Destroy 后 LockManager 换了新锁，
// 同 ID 的新实例不能因为 eMap 里还记着旧实例就被当作已持有——那样新实例会在未加锁的情况下发布、被修改。

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
