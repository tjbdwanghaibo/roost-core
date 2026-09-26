package entity

import (
	"testing"

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
